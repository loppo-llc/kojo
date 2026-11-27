package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
)

// pairedExternal creates + pairs an external agent from node A and adds
// it to the fixture group as the owner.
func pairedExternal(t *testing.T) (*Server, http.Handler, *agent.GroupDM, string, string) {
	t.Helper()
	srv, _, h := newExternalTestServer(t)
	// No real agent turns: notifications to kojo members go nowhere.
	srv.groupdms.SetOneShotForTesting(func(context.Context, string, string, agent.OneShotOpts) (<-chan agent.ChatEvent, error) {
		ch := make(chan agent.ChatEvent)
		close(ch)
		return ch, nil
	})
	// Member-change notifications to the kojo agents run async as real
	// Manager.Chat turns that write into the
	// temp config dir. Drain them the way a restart does before the dir
	// is removed: quiescing refuses notification goroutines that have
	// not reached Chat yet (the check and the preparing counter are one
	// atomic step), and WaitAllChatsIdle covers preparing and busy. The notification turns also leave a real persistent
	// claude process behind when the CLI is installed; close those
	// synchronously too. Registered after the Manager.Close cleanup, so
	// it runs first.
	t.Cleanup(func() {
		srv.agents.SetQuiescing(true)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := srv.agents.WaitAllChatsIdle(ctx); err != nil {
			t.Errorf("drain async notification turns: %v", err)
		}
		srv.agents.CloseAllClaudeSessionsSync()
	})
	created := ownerCall(t, srv.handleCreateExternalAgent, "POST", `{"name":"Bot A","ratePerMin":3}`, nil)
	id := created["id"].(string)
	pc := ownerCall(t, srv.handleExternalPairingCode, "POST", "", map[string]string{"id": id})
	_, out := xcall(t, h, "POST", "/v1/pair", "100.64.0.5:4000", "", `{"code":"`+pc["code"].(string)+`"}`)
	tok := out["token"].(string)
	g := srv.groupdms.List()[0]
	if _, err := srv.groupdms.AddMemberAs(g.ID, id, g.Members[0].AgentID, false); err == nil {
		t.Fatal("non-owner add of external must fail")
	}
	if _, err := srv.groupdms.AddMemberAs(g.ID, id, g.Members[0].AgentID, true); err != nil {
		t.Fatalf("owner add: %v", err)
	}
	return srv, h, g, id, tok
}

func TestExternalGroupAPI(t *testing.T) {
	srv, h, g, id, tok := pairedExternal(t)
	const node = "100.64.0.5:4000"

	rr, out := xcall(t, h, "GET", "/v1/groups", node, tok, "")
	if rr.Code != 200 || len(out["groups"].([]any)) != 1 {
		t.Fatalf("groups = %d %s", rr.Code, rr.Body.String())
	}
	if rr, _ := xcall(t, h, "GET", "/v1/groups/nope", node, tok, ""); rr.Code != 404 {
		t.Fatalf("unknown group = %d", rr.Code)
	}

	um, err := srv.groupdms.PostUserMessage(context.Background(), g.ID, "hi @Bot A", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	rr, out = xcall(t, h, "GET", "/v1/updates?wait=0", node, tok, "")
	if ups := out["updates"].([]any); rr.Code != 200 || len(ups) != 1 {
		t.Fatalf("updates = %d %s", rr.Code, rr.Body.String())
	}
	rr, out = xcall(t, h, "GET", "/v1/groups/"+g.ID+"/messages?after=", node, tok, "")
	if rr.Code != 200 || len(out["messages"].([]any)) != 1 || out["latestMessageId"] != um.ID {
		t.Fatalf("messages = %d %s", rr.Code, rr.Body.String())
	}

	// CAS is mandatory
	if rr, out := xcall(t, h, "POST", "/v1/groups/"+g.ID+"/messages", node, tok, `{"content":"x"}`); rr.Code != 409 || out["error"] != "expected_latest_id_required" {
		t.Fatalf("no cas = %d %v", rr.Code, out)
	}
	rr, out = xcall(t, h, "POST", "/v1/groups/"+g.ID+"/messages", node, tok, `{"content":"hello","expectedLatestId":"`+um.ID+`"}`)
	if rr.Code != 200 || out["agentName"] != "Bot A" {
		t.Fatalf("post = %d %s", rr.Code, rr.Body.String())
	}
	// own post auto-acks → nothing pending
	if _, out := xcall(t, h, "GET", "/v1/updates?wait=0", node, tok, ""); len(out["updates"].([]any)) != 0 {
		t.Fatalf("pending after own post: %v", out)
	}
	// stale cursor
	if rr, out := xcall(t, h, "POST", "/v1/groups/"+g.ID+"/messages", node, tok, `{"content":"again","expectedLatestId":"`+um.ID+`"}`); rr.Code != 409 || out["error"] != "stale_expected_id" {
		t.Fatalf("stale = %d %v", rr.Code, out)
	}
	// rate limit (3/min; 2 used)
	xcall(t, h, "POST", "/v1/groups/"+g.ID+"/messages", node, tok, `{"content":"x","expectedLatestId":"bogus"}`)
	if rr, _ := xcall(t, h, "POST", "/v1/groups/"+g.ID+"/messages", node, tok, `{"content":"x","expectedLatestId":"bogus"}`); rr.Code != 429 || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("rate = %d", rr.Code)
	}

	// owner trust endpoint
	req := httptest.NewRequest("PUT", "/x", strings.NewReader(`{"trust":"peer"}`))
	req.SetPathValue("id", g.ID)
	trr := httptest.NewRecorder()
	srv.handleSetGroupExternalTrust(trr, authedRequest(req, auth.Principal{Role: auth.RoleOwner}))
	if trr.Code != 200 || srv.groupdms.ExternalTrust(g.ID) != agent.ExternalTrustPeer {
		t.Fatalf("trust = %d %s", trr.Code, trr.Body.String())
	}
	req = httptest.NewRequest("PUT", "/x", strings.NewReader(`{"trust":"peer"}`))
	req.SetPathValue("id", g.ID)
	trr = httptest.NewRecorder()
	srv.handleSetGroupExternalTrust(trr, authedRequest(req, auth.Principal{Role: auth.RoleAgent, AgentID: "ag_x"}))
	if trr.Code != 403 {
		t.Fatalf("agent trust = %d", trr.Code)
	}

	// delete removes membership
	extCall(t, srv.handleDeleteExternalAgent, "DELETE", "/x", "", map[string]string{"id": id})
	if srv.groupdms.CheckMembership(g.ID, id) == nil {
		t.Fatal("deleted external still a member")
	}
}

func TestExternalEventStream(t *testing.T) {
	srv, h, g, xid, tok := pairedExternal(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "100.64.0.5:4000"
		h.ServeHTTP(w, r)
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	next := func() map[string]any {
		if !sc.Scan() {
			t.Fatalf("stream ended: %v", sc.Err())
		}
		var m map[string]any
		_ = json.Unmarshal(sc.Bytes(), &m)
		return m
	}
	if ev := next(); ev["type"] != "hello" {
		t.Fatalf("first = %v", ev)
	}
	if _, err := srv.groupdms.PostUserMessage(context.Background(), g.ID, "ping", nil, false); err != nil {
		t.Fatal(err)
	}
	ev := next()
	if ev["type"] != "message" || ev["groupId"] != g.ID {
		t.Fatalf("event = %v", ev)
	}
	if !srv.external.hub.online(xid) {
		t.Fatal("not online")
	}
	ownerCall(t, srv.handleRevokeExternalAgent, "POST", "", map[string]string{"id": xid})
	if ev := next(); ev["type"] != "closed" || ev["reason"] != "revoked" {
		t.Fatalf("after revoke = %v", ev)
	}
}

func TestExternalUpdatesEvents(t *testing.T) {
	srv, h, g, _, tok := pairedExternal(t)
	const node = "100.64.0.5:4000"
	_, out := xcall(t, h, "GET", "/v1/updates?wait=0", node, tok, "")
	evs := out["events"].([]any)
	if len(evs) != 1 || evs[0].(map[string]any)["type"] != "member_added" {
		t.Fatalf("events = %v", out)
	}
	seq := int(out["eventSeq"].(float64))
	done := make(chan map[string]any, 1)
	go func() {
		_, o := xcall(t, h, "GET", "/v1/updates?wait=5&since="+strconv.Itoa(seq), node, tok, "")
		done <- o
	}()
	time.Sleep(100 * time.Millisecond)
	if err := srv.groupdms.Delete(g.ID, false); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-done:
		evs := o["events"].([]any)
		if len(evs) != 1 || evs[0].(map[string]any)["type"] != "group_deleted" {
			t.Fatalf("after delete = %v", o)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long poll did not wake")
	}
	// a cursor from the future (e.g. kojo restarted) asks for resync
	if _, o := xcall(t, h, "GET", "/v1/updates?wait=0&since=999", node, tok, ""); o["resync"] != true {
		t.Fatalf("resync = %v", o)
	}
}

func TestExternalWebSocket(t *testing.T) {
	srv, h, g, xid, tok := pairedExternal(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "100.64.0.5:4000"
		h.ServeHTTP(w, r)
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/v1/ws", nil); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: err=%v resp=%v", err, resp)
	}
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/v1/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + tok}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	next := func() map[string]any {
		_, b, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m
	}
	if ev := next(); ev["type"] != "hello" {
		t.Fatalf("first = %v", ev)
	}
	if _, err := srv.groupdms.PostUserMessage(context.Background(), g.ID, "ping", nil, false); err != nil {
		t.Fatal(err)
	}
	if ev := next(); ev["type"] != "message" || ev["groupId"] != g.ID {
		t.Fatalf("event = %v", ev)
	}
	ownerCall(t, srv.handleRevokeExternalAgent, "POST", "", map[string]string{"id": xid})
	if ev := next(); ev["type"] != "closed" || ev["reason"] != "revoked" {
		t.Fatalf("after revoke = %v", ev)
	}
	if _, _, err := conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("close = %v", err)
	}
}

func TestExternalWebSocketRequests(t *testing.T) {
	_, h, g, _, tok := pairedExternal(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "100.64.0.5:4000"
		h.ServeHTTP(w, r)
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/v1/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + tok}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	type res struct {
		Type   string          `json:"type"`
		ID     string          `json:"id"`
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	}
	call := func(id, method, path, body string) res {
		f := map[string]any{"type": "req", "id": id, "method": method, "path": path}
		if body != "" {
			f["body"] = json.RawMessage(body)
		}
		b, _ := json.Marshal(f)
		if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var r res
			_ = json.Unmarshal(raw, &r)
			if r.Type == "res" && r.ID == id {
				return r
			}
		}
	}
	// An empty room needs no expectedLatestId.
	r := call("1", "POST", "/v1/groups/"+g.ID+"/messages", `{"content":"hi"}`)
	if r.Status != http.StatusOK {
		t.Fatalf("first post = %+v %s", r, r.Body)
	}
	r = call("2", "POST", "/v1/groups/"+g.ID+"/messages", `{"content":"again"}`)
	if r.Status != http.StatusConflict || !strings.Contains(string(r.Body), "expected_latest_id_required") {
		t.Fatalf("missing expected = %+v %s", r, r.Body)
	}
	r = call("3", "GET", "/v1/groups/"+g.ID+"/messages?limit=5", "")
	if r.Status != http.StatusOK || !strings.Contains(string(r.Body), `"hi"`) {
		t.Fatalf("list = %+v %s", r, r.Body)
	}
	if r = call("4", "GET", "/v1/events", ""); r.Status != http.StatusNotFound {
		t.Fatalf("events over ws = %+v", r)
	}
	if r = call("5", "DELETE", "/v1/groups/"+g.ID, ""); r.Status != http.StatusMethodNotAllowed {
		t.Fatalf("delete = %+v", r)
	}
}

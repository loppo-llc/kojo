package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

func newRemoteAgentGetServer(t *testing.T, agentID, holderURL, peerStatus string) *Server {
	t.Helper()
	srv := newQueueTestServer(t)
	ctx := context.Background()
	st := srv.agents.Store()
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: agentID, Name: "hub-mirror-name"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	if _, err := st.AcquireAgentLock(ctx, agentID, "peer-away", time.Now().UnixMilli(), 10*60*1000); err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	seedQueuePeer(t, srv, "peer-away", holderURL, peerStatus)
	return srv
}

func getRemoteAgent(t *testing.T, srv *Server, agentID string, p auth.Principal) map[string]any {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agentID, nil)
	r.SetPathValue("id", agentID)
	r = authedRequest(r, p)
	w := httptest.NewRecorder()
	srv.handleGetAgent(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return got
}

func TestRemoteAgentGetReadsThroughToOnlineHolder(t *testing.T) {
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/agents/ag_get" {
			t.Errorf("unexpected holder request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ag_get","name":"holder-name","cronExpr":"15 */3 * * *","etag":"28-holder","nextCronAt":"2026-09-25T12:15:00+09:00"}`))
	}))
	defer holder.Close()
	srv := newRemoteAgentGetServer(t, "ag_get", holder.URL, store.PeerStatusOnline)

	got := getRemoteAgent(t, srv, "ag_get", auth.Principal{Role: auth.RoleOwner})
	if got["name"] != "holder-name" || got["cronExpr"] != "15 */3 * * *" {
		t.Fatalf("holder fields not served: %v", got)
	}
	if got["etag"] != "28-holder" {
		t.Fatalf("etag = %v, want holder row etag for proxied PATCH If-Match", got["etag"])
	}
	if got["holderPeer"] != "peer-away" || got["holderPeerStatus"] != store.PeerStatusOnline {
		t.Fatalf("hub placement fields not overlaid: %v", got)
	}
}

func TestRemoteAgentGetFallsBackToHubRow(t *testing.T) {
	cases := map[string]struct {
		status  string
		handler http.HandlerFunc
	}{
		"holder offline": {store.PeerStatusOffline, func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("offline holder must not be dialed")
		}},
		"holder error": {store.PeerStatusOnline, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}},
		"holder returns other agent": {store.PeerStatusOnline, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"id":"ag_other","name":"wrong"}`))
		}},
		"holder returns non-object": {store.PeerStatusOnline, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[]`))
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			holder := httptest.NewServer(tc.handler)
			defer holder.Close()
			srv := newRemoteAgentGetServer(t, "ag_fb", holder.URL, tc.status)
			got := getRemoteAgent(t, srv, "ag_fb", auth.Principal{Role: auth.RoleOwner})
			if got["name"] != "hub-mirror-name" {
				t.Fatalf("name = %v, want hub row fallback", got["name"])
			}
			if got["holderPeer"] != "peer-away" {
				t.Fatalf("holderPeer = %v", got["holderPeer"])
			}
		})
	}
}

func TestRemoteAgentGetFallbackMarksSnapshotStale(t *testing.T) {
	srv := newRemoteAgentGetServer(t, "ag_st", "http://127.0.0.1:1", store.PeerStatusOnline)
	got := getRemoteAgent(t, srv, "ag_st", auth.Principal{Role: auth.RoleOwner})
	if got["holderSnapshotStale"] != true {
		t.Fatalf("holderSnapshotStale = %v, want true on hub-row fallback", got["holderSnapshotStale"])
	}
}

func TestRemoteAgentGetHubOwnedFieldsWin(t *testing.T) {
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ag_own","name":"n","privileged":true,"ownerDeputy":true,"isSwitching":true,"futureField":7}`))
	}))
	defer holder.Close()
	srv := newRemoteAgentGetServer(t, "ag_own", holder.URL, store.PeerStatusOnline)
	got := getRemoteAgent(t, srv, "ag_own", auth.Principal{Role: auth.RoleOwner})
	if _, ok := got["privileged"]; ok {
		t.Fatalf("privileged must come from the hub row: %v", got)
	}
	if _, ok := got["ownerDeputy"]; ok {
		t.Fatalf("ownerDeputy must come from the hub row: %v", got)
	}
	if got["isSwitching"] != true {
		t.Fatalf("holder-side isSwitching must survive: %v", got)
	}
	if got["futureField"] != float64(7) {
		t.Fatalf("unknown holder fields must pass through: %v", got)
	}
	if _, ok := got["holderSnapshotStale"]; ok {
		t.Fatalf("live holder read must not be marked stale: %v", got)
	}
}

func TestRemoteAgentGetProjectsOfflineHubLocalEdit(t *testing.T) {
	holderName := "hub-mirror-name" // holder never touched the field
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ag_ov","name":"` + holderName + `"}`))
	}))
	defer holder.Close()
	srv := newRemoteAgentGetServer(t, "ag_ov", holder.URL, store.PeerStatusOffline)
	if w := patchRemoteAgent(srv, "ag_ov", `{"name":"edited-offline"}`,
		auth.Principal{Role: auth.RoleOwner}); w.Code != http.StatusOK {
		t.Fatalf("offline hub-local patch: %d %s", w.Code, w.Body.String())
	}
	seedQueuePeer(t, srv, "peer-away", holder.URL, store.PeerStatusOnline)

	if got := getRemoteAgent(t, srv, "ag_ov", auth.Principal{Role: auth.RoleOwner}); got["name"] != "edited-offline" {
		t.Fatalf("name = %v, want pending hub edit projected", got["name"])
	}
	holderName = "renamed-on-holder" // holder changed it later → holder wins at ingest
	if got := getRemoteAgent(t, srv, "ag_ov", auth.Principal{Role: auth.RoleOwner}); got["name"] != "renamed-on-holder" {
		t.Fatalf("name = %v, want holder's later edit", got["name"])
	}
}

func TestRemoteAgentGetDirectoryViewReadsHolder(t *testing.T) {
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ag_dir","name":"holder-name","cronExpr":"* * * * *"}`))
	}))
	defer holder.Close()
	srv := newRemoteAgentGetServer(t, "ag_dir", holder.URL, store.PeerStatusOnline)
	got := getRemoteAgent(t, srv, "ag_dir", auth.Principal{Role: auth.RoleAgent, AgentID: "ag_someone_else"})
	if got["name"] != "holder-name" {
		t.Fatalf("directory name = %v, want holder's", got["name"])
	}
	if _, ok := got["cronExpr"]; ok {
		t.Fatalf("directory view leaked full fields: %v", got)
	}
}

func TestRemoteAgentListReadsHolderConfig(t *testing.T) {
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/ag_ls":
			_, _ = w.Write([]byte(`{"id":"ag_ls","name":"holder-name","model":"m-holder","effort":"max","cronExpr":"15 */3 * * *"}`))
		case "/api/v1/agents/ag_ls/messages":
			_, _ = w.Write([]byte(`{"messages":[]}`))
		default:
			t.Errorf("unexpected holder request: %s", r.URL.Path)
		}
	}))
	defer holder.Close()
	srv := newRemoteAgentGetServer(t, "ag_ls", holder.URL, store.PeerStatusOnline)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	r = authedRequest(r, auth.Principal{Role: auth.RoleOwner})
	w := httptest.NewRecorder()
	srv.handleListAgents(w, r)
	var body struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, a := range body.Agents {
		if a["id"] != "ag_ls" {
			continue
		}
		if a["model"] != "m-holder" || a["effort"] != "max" || a["cronExpr"] != "15 */3 * * *" {
			t.Fatalf("list row not refreshed from holder: %v", a)
		}
		if a["holderPeer"] != "peer-away" {
			t.Fatalf("holderPeer lost: %v", a)
		}
		return
	}
	t.Fatalf("remote agent missing from list: %s", w.Body.String())
}

func TestRemoteAgentGetSkipsProjectionIngestWouldReject(t *testing.T) {
	holderEnd := "" // holder still at base
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ag_sv","silentStart":"","silentEnd":"` + holderEnd + `"}`))
	}))
	defer holder.Close()
	srv := newRemoteAgentGetServer(t, "ag_sv", holder.URL, store.PeerStatusOffline)
	if w := patchRemoteAgent(srv, "ag_sv", `{"silentStart":"22:00","silentEnd":"23:00"}`,
		auth.Principal{Role: auth.RoleOwner}); w.Code != http.StatusOK {
		t.Fatalf("offline hub-local patch: %d %s", w.Code, w.Body.String())
	}
	seedQueuePeer(t, srv, "peer-away", holder.URL, store.PeerStatusOnline)
	if got := getRemoteAgent(t, srv, "ag_sv", auth.Principal{Role: auth.RoleOwner}); got["silentStart"] != "22:00" || got["silentEnd"] != "23:00" {
		t.Fatalf("silent hours = %v-%v, want projected hub edit", got["silentStart"], got["silentEnd"])
	}
	// Holder moved silentEnd to 22:00: silentEnd drops, silentStart
	// would still merge → 22:00-22:00, which ingest rejects. Show the
	// holder's row instead of an invalid merge.
	holderEnd = "22:00"
	got := getRemoteAgent(t, srv, "ag_sv", auth.Principal{Role: auth.RoleOwner})
	if start, _ := got["silentStart"].(string); start != "" {
		t.Fatalf("silentStart = %q, want invalid merge not projected", start)
	}
	if got["silentEnd"] != "22:00" {
		t.Fatalf("silentEnd = %v, want holder's", got["silentEnd"])
	}
}

func TestRemoteAgentListHonoursHolderArchiveState(t *testing.T) {
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/ag_ar":
			_, _ = w.Write([]byte(`{"id":"ag_ar","name":"holder-name","archived":true}`))
		default:
			_, _ = w.Write([]byte(`{"messages":[]}`))
		}
	}))
	defer holder.Close()
	srv := newRemoteAgentGetServer(t, "ag_ar", holder.URL, store.PeerStatusOnline)
	for _, p := range []auth.Principal{{Role: auth.RoleOwner}, {Role: auth.RoleAgent, AgentID: "ag_ar"}} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
		r = authedRequest(r, p)
		w := httptest.NewRecorder()
		srv.handleListAgents(w, r)
		var body struct {
			Agents []map[string]any `json:"agents"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, a := range body.Agents {
			if a["id"] == "ag_ar" {
				t.Fatalf("role %v: holder-archived agent still listed: %v", p.Role, a)
			}
		}
	}
}

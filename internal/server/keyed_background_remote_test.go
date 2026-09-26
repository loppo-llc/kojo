package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

type recordingSlackSurface struct {
	turns     chan []agent.ChatEvent
	abandoned chan string
}

func newRecordingSlackSurface() *recordingSlackSurface {
	return &recordingSlackSurface{turns: make(chan []agent.ChatEvent, 4), abandoned: make(chan string, 4)}
}

func (h *recordingSlackSurface) HandleKeyedBackgroundTurn(_, _ string, events <-chan agent.ChatEvent, _ func()) {
	var evs []agent.ChatEvent
	for e := range events {
		evs = append(evs, e)
	}
	h.turns <- evs
}

func (h *recordingSlackSurface) KeyedBackgroundTasksAbandoned(_, key string, pending int, reason string) {
	h.abandoned <- key + "|" + reason
}

func mustToken(t *testing.T) string {
	t.Helper()
	tok, err := randomKeyedBgToken()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func postKeyedBgNotify(t *testing.T, srv *Server, p auth.Principal, req keyedBgNotifyRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(req)
	r := authedRequest(httptest.NewRequest(http.MethodPost, keyedBgNotifyPath, bytes.NewReader(body)), p)
	w := httptest.NewRecorder()
	srv.handlePeerKeyedBackgroundNotify(w, r)
	return w
}

func TestExternalChatReadyAdvertisesKeyedBackground(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	req := authedRequest(httptest.NewRequest(http.MethodGet, "/api/v1/agents/ag_x/external-chat/ready", nil),
		auth.Principal{Role: auth.RolePeer, PeerID: "peer-a"})
	req.SetPathValue("id", "ag_x")
	rr := httptest.NewRecorder()
	srv.handleExternalChatReady(rr, req)
	var got externalChatReadyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || !got.KeyedBackgroundV1 {
		t.Fatalf("ready = %s err=%v", rr.Body.String(), err)
	}
}

func TestExternalChatRouterRelaysLingerOnlyToCapableHolder(t *testing.T) {
	for _, capable := range []bool{true, false} {
		seen := make(chan map[string]any, 1)
		holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder", KeyedBackgroundV1: capable})
				return
			}
			var raw map[string]any
			_ = json.NewDecoder(r.Body).Decode(&raw)
			seen <- raw
			writeExternalChatTestStream(t, w, agent.ChatEvent{Type: "done", Message: &agent.Message{Content: "ok"}})
		}))
		srv, router, agentID := prepareRemoteExternalChat(t, holder.URL)
		_ = srv
		events, err := router.ChatOneShot(context.Background(), agentID, "hi", agent.OneShotOpts{
			SessionKey: "groupdm:g1", LingerBackgroundTasks: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		for range events {
		}
		raw := <-seen
		if _, ok := raw["lingerBackgroundTasks"]; ok != capable {
			t.Fatalf("capable=%v: lingerBackgroundTasks relayed=%v body=%v", capable, ok, raw)
		}
		holder.Close()
	}
}

func TestExternalChatRouterAttachRequiresCapableHolder(t *testing.T) {
	posted := false
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		posted = true
	}))
	defer holder.Close()
	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	_, err := router.ChatOneShot(context.Background(), agentID, keyedBgPlaceholderMessage, agent.OneShotOpts{
		SessionKey: agentID + ":slack:C1:1.0", ExpectedHolderPeer: "holder", AttachBackgroundToken: mustToken(t),
	})
	if err == nil || posted {
		t.Fatalf("attach to an old holder: err=%v posted=%v", err, posted)
	}
}

func TestKeyedBackgroundNotifyFencing(t *testing.T) {
	srv, router, agentID := prepareRemoteExternalChat(t, "http://127.0.0.1:1")
	srv.externalChat = router
	h := newRecordingSlackSurface()
	defer srv.agents.RegisterKeyedBackgroundHandler(agentID, h)()
	key := agentID + ":slack:C1:1.0"
	holderP := auth.Principal{Role: auth.RolePeer, PeerID: "holder"}
	base := keyedBgNotifyRequest{HolderID: "holder", AgentID: agentID, SessionKey: key, Kind: keyedBgNotifyKindAbandoned, Pending: 2, Reason: "r"}
	with := func(mut func(*keyedBgNotifyRequest)) keyedBgNotifyRequest {
		req := base
		req.NotifyID = mustToken(t)
		if mut != nil {
			mut(&req)
		}
		return req
	}
	cases := []struct {
		name string
		p    auth.Principal
		req  keyedBgNotifyRequest
		want int
	}{
		{"wrong peer", auth.Principal{Role: auth.RolePeer, PeerID: "other"}, with(nil), http.StatusForbidden},
		{"no peer identity", auth.Principal{Role: auth.RoleOwner}, with(nil), http.StatusForbidden},
		{"foreign key", holderP, with(func(r *keyedBgNotifyRequest) { r.SessionKey = "ag_other:slack:C1:1.0" }), http.StatusBadRequest},
		{"turn without token", holderP, with(func(r *keyedBgNotifyRequest) { r.Kind = keyedBgNotifyKindTurn; r.Pending = 0; r.Reason = "" }), http.StatusBadRequest},
		{"no surface", holderP, with(func(r *keyedBgNotifyRequest) { r.SessionKey = "groupdm:missing" }), http.StatusNotFound},
		{"stale holder", auth.Principal{Role: auth.RolePeer, PeerID: "old"}, with(func(r *keyedBgNotifyRequest) { r.HolderID = "old" }), http.StatusConflict},
	}
	for _, tc := range cases {
		if w := postKeyedBgNotify(t, srv, tc.p, tc.req); w.Code != tc.want {
			t.Fatalf("%s: status=%d body=%s, want %d", tc.name, w.Code, w.Body.String(), tc.want)
		}
	}
	select {
	case got := <-h.abandoned:
		t.Fatalf("rejected notify was delivered: %s", got)
	default:
	}
	ok := with(nil)
	for i := 0; i < 2; i++ { // retried notifyId is idempotent
		if w := postKeyedBgNotify(t, srv, holderP, ok); w.Code != http.StatusAccepted {
			t.Fatalf("valid notify status=%d body=%s", w.Code, w.Body.String())
		}
	}
	select {
	case got := <-h.abandoned:
		if got != key+"|r" {
			t.Fatalf("abandoned = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("abandoned notice not delivered to the Hub surface")
	}
	select {
	case got := <-h.abandoned:
		t.Fatalf("duplicate notify delivered twice: %s", got)
	case <-time.After(100 * time.Millisecond):
	}
}

// Hub side end to end: the notify opens the surface admission and attaches to
// the (fake) holder's buffered turn with the one-time token.
func TestKeyedBackgroundNotifyAttachesRemoteTurn(t *testing.T) {
	token := mustToken(t)
	attached := make(chan externalChatTextRequest, 1)
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder", KeyedBackgroundV1: true})
			return
		}
		var req externalChatTextRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		attached <- req
		writeExternalChatTestStream(t, w,
			agent.ChatEvent{Type: "text", Delta: "bg "},
			agent.ChatEvent{Type: "done", Message: &agent.Message{Content: "bg result"}, BackgroundTasksPending: 1})
	}))
	defer holder.Close()
	srv, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	srv.externalChat = router
	h := newRecordingSlackSurface()
	defer srv.agents.RegisterKeyedBackgroundHandler(agentID, h)()
	key := agentID + ":slack:C1:1.0"
	w := postKeyedBgNotify(t, srv, auth.Principal{Role: auth.RolePeer, PeerID: "holder"}, keyedBgNotifyRequest{
		HolderID: "holder", AgentID: agentID, SessionKey: key, Kind: keyedBgNotifyKindTurn, Token: token, NotifyID: mustToken(t),
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	select {
	case req := <-attached:
		if req.AttachBackgroundToken != token || req.SessionKey != key {
			t.Fatalf("attach request = %#v", req)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Hub did not attach to the holder")
	}
	select {
	case evs := <-h.turns:
		last := evs[len(evs)-1]
		if last.Type != "done" || last.Message == nil || last.Message.Content != "bg result" || last.BackgroundTasksPending != 1 {
			t.Fatalf("events = %#v", evs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("background turn not delivered to the Hub surface")
	}
}

// prepareKeyedHolder is a holder whose agent lock names "hub" as the allowed
// proxy, with the Hub reachable at hubURL.
func prepareKeyedHolder(t *testing.T, hubURL string) (*Server, string) {
	t.Helper()
	srv := newChunkedSyncTestServer(t)
	srv.peerID = &peer.Identity{DeviceID: "holder", Name: "Holder"}
	srv.externalChatRelays = newExternalChatRelayRegistry()
	a, err := srv.agents.Create(agent.AgentConfig{Name: "keyed holder"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := srv.agents.Store().UpsertPeer(ctx, &store.PeerRecord{DeviceID: "hub", Name: "Hub", URL: hubURL, Status: store.PeerStatusOnline}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.agents.Store().AcquireAgentLock(ctx, a.ID, "holder", store.NowMillis(), int64(time.Hour/time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := srv.agents.Store().UpdateAgentLockAllowedProxy(ctx, a.ID, "holder", "hub"); err != nil {
		t.Fatal(err)
	}
	return srv, a.ID
}

func holderAttach(t *testing.T, srv *Server, agentID, key, token, peerID string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(externalChatTextRequest{Message: keyedBgPlaceholderMessage, SessionKey: key, AttachBackgroundToken: token})
	r := authedRequest(httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/external-chat", bytes.NewReader(body)),
		auth.Principal{Role: auth.RolePeer, PeerID: peerID})
	r.SetPathValue("id", agentID)
	w := httptest.NewRecorder()
	srv.handleExternalChatText(w, r)
	return w
}

// Holder side end to end: the surface notifies the (fake) Hub, keeps the Hub
// file relay alive while bound, and streams the buffered turn to the attach.
func TestRemoteKeyedSurfaceNotifiesAndStreamsToAttach(t *testing.T) {
	notified := make(chan keyedBgNotifyRequest, 4)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req keyedBgNotifyRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if r.URL.Path != keyedBgNotifyPath || dec.Decode(&req) != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		notified <- req
		w.WriteHeader(http.StatusAccepted)
	}))
	defer hub.Close()
	srv, agentID := prepareKeyedHolder(t, hub.URL)
	key := "groupdm:g1"
	sf := srv.newRemoteKeyedSurface(agentID, key, "hub", hub.URL)
	sf.Retain() // the session's reference
	sf.Release()
	if !srv.externalChatRelays.allowed(agentID, "hub", false) {
		t.Fatal("bound surface does not keep the Hub file relay active")
	}

	events := make(chan agent.ChatEvent, 4)
	var aborted sync.Once
	abortedCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		sf.HandleKeyedSessionTurn(context.Background(), agentID, key, events, func() { aborted.Do(func() { close(abortedCh) }) })
	}()
	var req keyedBgNotifyRequest
	select {
	case req = <-notified:
	case <-time.After(5 * time.Second):
		t.Fatal("surface did not notify the Hub")
	}
	if req.Kind != keyedBgNotifyKindTurn || req.HolderID != "holder" || req.AgentID != agentID || req.SessionKey != key || !validKeyedBgToken(req.Token) {
		t.Fatalf("notify = %#v", req)
	}
	if w := holderAttach(t, srv, agentID, key, req.Token, "other"); w.Code == http.StatusOK {
		t.Fatal("unrelated peer attached to the Hub's background turn")
	}
	if srv.keyedBg.claim(req.Token, agentID, "groupdm:other", "hub") != nil {
		t.Fatal("claim with a different session key succeeded")
	}
	events <- agent.ChatEvent{Type: "text", Delta: "partial"}
	events <- agent.ChatEvent{Type: "done", Message: &agent.Message{Content: "final"}}
	close(events)
	w := holderAttach(t, srv, agentID, key, req.Token, "hub")
	if w.Code != http.StatusOK {
		t.Fatalf("attach status=%d body=%s", w.Code, w.Body.String())
	}
	var gotDone bool
	sc := bufio.NewScanner(strings.NewReader(w.Body.String()))
	for sc.Scan() {
		var env externalChatTextEnvelope
		if json.Unmarshal(sc.Bytes(), &env) == nil && env.Event != nil && env.Event.Type == "done" && env.Event.Message.Content == "final" {
			gotDone = true
		}
	}
	if !gotDone {
		t.Fatalf("attach stream = %s", w.Body.String())
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("surface still waiting after the attach finished")
	}
	select {
	case <-abortedCh:
		t.Fatal("terminated attach aborted the turn")
	default:
	}
	if w := holderAttach(t, srv, agentID, key, req.Token, "hub"); w.Code != http.StatusNotFound {
		t.Fatalf("token reuse status=%d", w.Code)
	}

	// Abandoned notice goes to the same Hub; releasing the session's last
	// reference revokes the relay.
	sf.KeyedBackgroundTasksAbandoned(agentID, key, 2, "idle")
	if got := <-notified; got.Kind != keyedBgNotifyKindAbandoned || got.Pending != 2 || got.Reason != "idle" {
		t.Fatalf("abandoned notify = %#v", got)
	}
	sf.Release()
	if srv.externalChatRelays.allowed(agentID, "hub", false) {
		t.Fatal("Hub file relay still active after the session released its surface")
	}
}

func TestRemoteKeyedSurfaceDropsTurnWhenHubRejects(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"holder_not_current"}}`, http.StatusConflict)
	}))
	defer hub.Close()
	srv, agentID := prepareKeyedHolder(t, hub.URL)
	sf := srv.newRemoteKeyedSurface(agentID, "groupdm:g1", "hub", hub.URL)
	defer sf.Release()
	aborted := false
	start := time.Now()
	sf.HandleKeyedSessionTurn(context.Background(), agentID, "groupdm:g1", make(chan agent.ChatEvent), func() { aborted = true })
	if time.Since(start) > 3*time.Second || aborted {
		t.Fatalf("permanent rejection: took %s aborted=%v", time.Since(start), aborted)
	}
	srv.keyedBg.mu.Lock()
	n := len(srv.keyedBg.attaches)
	srv.keyedBg.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d attach entries leaked", n)
	}
}

func TestRemoteKeyedSurfaceRetriesThenStopsOnLifecycleCancel(t *testing.T) {
	oldBackoff, oldWait := keyedBgNotifyBackoff, keyedBgAttachWait
	keyedBgNotifyBackoff, keyedBgAttachWait = 5*time.Millisecond, time.Hour
	t.Cleanup(func() { keyedBgNotifyBackoff, keyedBgAttachWait = oldBackoff, oldWait })
	var mu sync.Mutex
	calls := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable) // transient: retried
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer hub.Close()
	srv, agentID := prepareKeyedHolder(t, hub.URL)
	sf := srv.newRemoteKeyedSurface(agentID, "groupdm:g1", "hub", hub.URL)
	defer sf.Release()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sf.HandleKeyedSessionTurn(ctx, agentID, "groupdm:g1", make(chan agent.ChatEvent), func() {})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("notify attempts = %d, want retries until 202", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Waiting for the Hub's claim must end promptly on a lifecycle cancel
	// (reset / shutdown / handoff quiesce).
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("surface kept waiting for the claim after lifecycle cancel")
	}
}

func TestRemoteKeyedSurfaceSurvivesHubOutageLongerThanNotifyBudget(t *testing.T) {
	oldBackoff, oldWait, oldBudget, oldMax := keyedBgNotifyBackoff, keyedBgAttachWait, keyedBgTurnNotifyBudget, keyedBgAttachMax
	keyedBgNotifyBackoff, keyedBgAttachWait, keyedBgTurnNotifyBudget, keyedBgAttachMax = 5*time.Millisecond, 40*time.Millisecond, 30*time.Millisecond, 10*time.Second
	t.Cleanup(func() {
		keyedBgNotifyBackoff, keyedBgAttachWait, keyedBgTurnNotifyBudget, keyedBgAttachMax = oldBackoff, oldWait, oldBudget, oldMax
	})
	var mu sync.Mutex
	outage := true
	var tokens []string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req keyedBgNotifyRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		defer mu.Unlock()
		if outage {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		tokens = append(tokens, req.Token)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer hub.Close()
	srv, agentID := prepareKeyedHolder(t, hub.URL)
	sf := srv.newRemoteKeyedSurface(agentID, "groupdm:g1", "hub", hub.URL)
	defer sf.Release()
	in := make(chan agent.ChatEvent, 1)
	in <- agent.ChatEvent{Type: "done", Message: &agent.Message{Content: "x"}}
	close(in)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sf.HandleKeyedSessionTurn(ctx, agentID, "groupdm:g1", in, func() {})
	}()
	// Several whole notify budgets fail; the turn must stay registered.
	time.Sleep(200 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("transient Hub outage dropped the turn")
	default:
	}
	srv.keyedBg.mu.Lock()
	n := len(srv.keyedBg.attaches)
	srv.keyedBg.mu.Unlock()
	if n != 1 {
		t.Fatalf("attach entries during outage = %d, want 1", n)
	}
	mu.Lock()
	outage = false
	mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := len(tokens)
		mu.Unlock()
		if got > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no notify reached the Hub after it recovered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("surface kept waiting after lifecycle cancel")
	}
}

func TestBufferKeyedBgEventsDecouplesAndCoalesces(t *testing.T) {
	in := make(chan agent.ChatEvent)
	out, discard := bufferKeyedBgEvents(in)
	defer discard()
	// Nobody reads out yet (the Hub has not claimed): the producer must
	// never block, however long the turn is.
	var want strings.Builder
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for i := 0; i < 5000; i++ {
			d := "x"
			if i%100 == 0 {
				d = "y"
			}
			want.WriteString(d)
			in <- agent.ChatEvent{Type: "text", Delta: d}
		}
		for i := 0; i < maxKeyedBgBufferedEvents+500; i++ {
			in <- agent.ChatEvent{Type: "tool_use", ToolName: "Bash"}
		}
		in <- agent.ChatEvent{Type: "attachment", Attachments: []agent.MessageAttachment{{Name: "a.txt"}}}
		in <- agent.ChatEvent{Type: "text", Delta: "tail"}
		in <- agent.ChatEvent{Type: "done", Message: &agent.Message{Content: "final"}}
		close(in)
	}()
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("producer blocked on an unclaimed turn")
	}
	var evs []agent.ChatEvent
	for e := range out {
		evs = append(evs, e)
	}
	if len(evs) == 0 || evs[0].Type != "text" || evs[0].Delta != want.String() {
		t.Fatalf("text not coalesced losslessly: first=%#v", evs[0])
	}
	tools, sawAttach := 0, false
	for _, e := range evs {
		switch e.Type {
		case "tool_use":
			tools++
		case "attachment":
			sawAttach = true
		}
	}
	if tools == 0 || tools > maxKeyedBgBufferedEvents {
		t.Fatalf("tool events = %d, want capped at %d", tools, maxKeyedBgBufferedEvents)
	}
	n := len(evs)
	if !sawAttach || evs[n-2].Delta != "tail" || evs[n-1].Type != "done" || evs[n-1].Message.Content != "final" {
		t.Fatalf("attachment / tail text / terminal lost: %#v", evs[n-3:])
	}
}

func TestBufferKeyedBgEventsDiscardDrains(t *testing.T) {
	in := make(chan agent.ChatEvent)
	out, discard := bufferKeyedBgEvents(in)
	in <- agent.ChatEvent{Type: "text", Delta: "a"}
	discard()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			in <- agent.ChatEvent{Type: "tool_use"}
		}
		close(in)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("discarded buffer stopped draining the turn")
	}
	for e := range out {
		t.Fatalf("discarded buffer replayed %#v", e)
	}
}

func TestRemoteKeyedSurfaceRenotifiesUntilLingerBound(t *testing.T) {
	oldWait, oldMax := keyedBgAttachWait, keyedBgAttachMax
	keyedBgAttachWait, keyedBgAttachMax = 30*time.Millisecond, 400*time.Millisecond
	t.Cleanup(func() { keyedBgAttachWait, keyedBgAttachMax = oldWait, oldMax })
	var mu sync.Mutex
	var reqs []keyedBgNotifyRequest
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req keyedBgNotifyRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		reqs = append(reqs, req)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted) // accepted, never attached
	}))
	defer hub.Close()
	srv, agentID := prepareKeyedHolder(t, hub.URL)
	sf := srv.newRemoteKeyedSurface(agentID, "groupdm:g1", "hub", hub.URL)
	defer sf.Release()
	in := make(chan agent.ChatEvent, 1)
	in <- agent.ChatEvent{Type: "done", Message: &agent.Message{Content: "x"}}
	close(in)
	start := time.Now()
	sf.HandleKeyedSessionTurn(context.Background(), agentID, "groupdm:g1", in, func() {})
	if el := time.Since(start); el < keyedBgAttachMax || el > keyedBgAttachMax+3*time.Second {
		t.Fatalf("claim wait = %s, want bounded by %s", el, keyedBgAttachMax)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reqs) < 3 {
		t.Fatalf("notifies = %d, want periodic re-notify", len(reqs))
	}
	ids := map[string]bool{}
	for _, r := range reqs {
		if r.Token != reqs[0].Token || r.Kind != keyedBgNotifyKindTurn {
			t.Fatalf("re-notify changed the token/kind: %#v", r)
		}
		ids[r.NotifyID] = true
	}
	if len(ids) != len(reqs) {
		t.Fatal("re-notify reused a notifyId (the Hub would dedup it away)")
	}
	srv.keyedBg.mu.Lock()
	left := len(srv.keyedBg.attaches)
	srv.keyedBg.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d attach entries leaked past the bound", left)
	}
	if note := srv.agents.PeekKeyedBackgroundNote(agentID, "groupdm:g1"); !strings.Contains(note, "Hubへ届けられず") {
		t.Fatalf("undelivered note = %q", note)
	}
}

func TestKeyedBackgroundNotifyRenotifyKeepsOneDelivery(t *testing.T) {
	token := mustToken(t)
	release := make(chan struct{})
	var mu sync.Mutex
	attaches := 0
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder", KeyedBackgroundV1: true})
			return
		}
		mu.Lock()
		attaches++
		mu.Unlock()
		<-release
		writeExternalChatTestStream(t, w, agent.ChatEvent{Type: "done", Message: &agent.Message{Content: "bg"}})
	}))
	defer holder.Close()
	srv, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	srv.externalChat = router
	h := newRecordingSlackSurface()
	defer srv.agents.RegisterKeyedBackgroundHandler(agentID, h)()
	key := agentID + ":slack:C1:1.0"
	post := func() {
		w := postKeyedBgNotify(t, srv, auth.Principal{Role: auth.RolePeer, PeerID: "holder"}, keyedBgNotifyRequest{
			HolderID: "holder", AgentID: agentID, SessionKey: key, Kind: keyedBgNotifyKindTurn, Token: token, NotifyID: mustToken(t),
		})
		if w.Code != http.StatusAccepted {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	post()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := attaches
		mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first delivery never attached")
		}
		time.Sleep(5 * time.Millisecond)
	}
	post() // holder re-notify while the delivery is still in flight
	time.Sleep(100 * time.Millisecond)
	close(release)
	select {
	case <-h.turns:
	case <-time.After(5 * time.Second):
		t.Fatal("background turn not delivered")
	}
	mu.Lock()
	n := attaches
	mu.Unlock()
	if n != 1 {
		t.Fatalf("attaches = %d, want 1 (re-notify must not start a second delivery)", n)
	}
	// Once it ended, the token is free again (a later re-notify retries).
	deadline = time.Now().Add(2 * time.Second)
	for {
		if ok, _ := srv.keyedBg.beginDelivery(token); ok {
			srv.keyedBg.endDelivery(token)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("delivery slot not released")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBufferKeyedBgEventsBoundsMessages(t *testing.T) {
	in := make(chan agent.ChatEvent)
	out, discard := bufferKeyedBgEvents(in)
	defer discard()
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for i := 0; i < 3*maxKeyedBgBufferedEvents; i++ {
			in <- agent.ChatEvent{Type: "message", Message: &agent.Message{Content: "m"}}
		}
		in <- agent.ChatEvent{Type: "done", Message: &agent.Message{Content: "final"}}
		close(in)
	}()
	// Nobody reads until the whole (unclaimed) turn is queued.
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("producer blocked on an unclaimed turn")
	}
	msgs, sawDone := 0, false
	for e := range out {
		switch e.Type {
		case "message":
			msgs++
		case "done":
			sawDone = true
		}
	}
	if msgs == 0 || msgs > 2*maxKeyedBgBufferedEvents || !sawDone {
		t.Fatalf("messages=%d (cap %d) done=%v", msgs, 2*maxKeyedBgBufferedEvents, sawDone)
	}
}

// A notify rejected at the in-flight delivery cap must not be remembered as
// seen: the holder's retry of the same NotifyID has to be processed.
func TestKeyedBgRejectedNotifyIsNotDeduped(t *testing.T) {
	var rr keyedBgRegistry
	for i := 0; i < maxKeyedBgInflight; i++ {
		if ok, full := rr.beginDelivery("tok" + strconv.Itoa(i)); !ok || full {
			t.Fatalf("beginDelivery %d = %v,%v", i, ok, full)
		}
	}
	id := strings.Repeat("a", 64)
	if !rr.firstNotify(id) {
		t.Fatal("first notify deduped")
	}
	if _, full := rr.beginDelivery("next"); !full {
		t.Fatal("cap not reached")
	}
	rr.forgetNotify(id)
	rr.endDelivery("tok0")
	if !rr.firstNotify(id) {
		t.Fatal("retry of a rejected notify was deduped")
	}
	if ok, full := rr.beginDelivery("next"); !ok || full {
		t.Fatalf("retry not admitted: %v,%v", ok, full)
	}
}

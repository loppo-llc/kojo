package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// newQueueTestServer reuses the chunked-sync fixture (agents manager
// with an isolated store) and stamps a hub peer identity.
func newQueueTestServer(t *testing.T) *Server {
	t.Helper()
	srv := newChunkedSyncTestServer(t)
	srv.peerID = &peer.Identity{DeviceID: "hub"}
	return srv
}

func seedQueuePeer(t *testing.T, srv *Server, deviceID, url, status string) {
	t.Helper()
	if _, err := srv.agents.Store().UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID: deviceID,
		Name:     "laptop-x",
		URL:      url,
		Status:   status,
	}); err != nil {
		t.Fatalf("seed peer: %v", err)
	}
}

func postQueuedSend(srv *Server, agentID, holder, content string) *httptest.ResponseRecorder {
	body := strings.NewReader(fmt.Sprintf(`{"content":%q}`, content))
	r := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/"+agentID+"/messages", body)
	r.Header.Set("Content-Type", "application/json")
	r = authedRequest(r, auth.Principal{Role: auth.RoleOwner})
	w := httptest.NewRecorder()
	srv.proxyOrQueueAgentMessage(w, r, agentID, holder)
	return w
}

func TestQueuedSendEnqueuesWhenHolderOffline(t *testing.T) {
	srv := newQueueTestServer(t)
	seedQueuePeer(t, srv, "peer-away", "http://unreachable.example:1", store.PeerStatusOffline)

	w := postQueuedSend(srv, "ag_q1", "peer-away", "hello while away")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp["queued"] != true || resp["holderPeer"] != "peer-away" {
		t.Fatalf("resp = %v", resp)
	}
	if name, _ := resp["holderPeerName"].(string); name != "laptop-x" {
		t.Fatalf("holderPeerName = %v", resp["holderPeerName"])
	}
	if msg, _ := resp["message"].(string); !strings.Contains(msg, "laptop-x") {
		t.Fatalf("message = %q", msg)
	}
	msgs, err := srv.agents.Store().ListHandoffQueuedMessages(context.Background(), "ag_q1")
	if err != nil || len(msgs) != 1 || msgs[0].Content != "hello while away" {
		t.Fatalf("queue rows = %+v, err %v", msgs, err)
	}
}

func TestQueuedSendEnqueuesOnDialFailure(t *testing.T) {
	srv := newQueueTestServer(t)
	// Marked online but no listener behind the address → dial fails
	// → fall back to enqueue.
	seedQueuePeer(t, srv, "peer-zombie", "http://127.0.0.1:1", store.PeerStatusOnline)

	w := postQueuedSend(srv, "ag_q2", "peer-zombie", "unreachable holder")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["queued"] != true {
		t.Fatalf("resp = %v", resp)
	}
	msgs, _ := srv.agents.Store().ListHandoffQueuedMessages(context.Background(), "ag_q2")
	if len(msgs) != 1 {
		t.Fatalf("queue rows = %+v", msgs)
	}
}

func TestQueuedSendCapReturns429(t *testing.T) {
	srv := newQueueTestServer(t)
	seedQueuePeer(t, srv, "peer-away", "", store.PeerStatusOffline)
	ctx := context.Background()
	for i := 0; i < store.MaxHandoffQueuedPerAgent; i++ {
		if _, err := srv.agents.Store().EnqueueHandoffQueuedMessage(
			ctx, "ag_full", "peer-away", fmt.Sprintf("m%d", i)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	w := postQueuedSend(srv, "ag_full", "peer-away", "one too many")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "queue_full") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestQueuedSendRejectsEmptyContent(t *testing.T) {
	srv := newQueueTestServer(t)
	seedQueuePeer(t, srv, "peer-away", "", store.PeerStatusOffline)
	w := postQueuedSend(srv, "ag_q3", "peer-away", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
}

func TestQueuedMessagesListAndCancelEndpoints(t *testing.T) {
	srv := newQueueTestServer(t)
	ctx := context.Background()
	rec, err := srv.agents.Store().EnqueueHandoffQueuedMessage(ctx, "ag_c", "peer-away", "cancel me")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Non-owner list refused.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/agents/ag_c/queued-messages", nil)
	r.SetPathValue("id", "ag_c")
	r = authedRequest(r, auth.Principal{Role: auth.RoleAgent, AgentID: "ag_c"})
	w := httptest.NewRecorder()
	srv.handleListQueuedAgentMessages(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("agent list status = %d", w.Code)
	}

	// Owner list.
	r = httptest.NewRequest(http.MethodGet, "/api/v1/agents/ag_c/queued-messages", nil)
	r.SetPathValue("id", "ag_c")
	r = authedRequest(r, auth.Principal{Role: auth.RoleOwner})
	w = httptest.NewRecorder()
	srv.handleListQueuedAgentMessages(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d, body %s", w.Code, w.Body.String())
	}
	var listResp struct {
		Messages []struct {
			ID      string `json:"id"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("list json: %v", err)
	}
	if len(listResp.Messages) != 1 || listResp.Messages[0].ID != rec.ID {
		t.Fatalf("list = %+v", listResp)
	}

	// Owner cancel.
	r = httptest.NewRequest(http.MethodDelete, "/api/v1/agents/ag_c/queued-messages/"+rec.ID, nil)
	r.SetPathValue("id", "ag_c")
	r.SetPathValue("qid", rec.ID)
	r = authedRequest(r, auth.Principal{Role: auth.RoleOwner})
	w = httptest.NewRecorder()
	srv.handleCancelQueuedAgentMessage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body %s", w.Code, w.Body.String())
	}
	msgs, _ := srv.agents.Store().ListHandoffQueuedMessages(ctx, "ag_c")
	if len(msgs) != 0 {
		t.Fatalf("queue not empty after cancel: %+v", msgs)
	}

	// Cancel again → 404.
	r = httptest.NewRequest(http.MethodDelete, "/api/v1/agents/ag_c/queued-messages/"+rec.ID, nil)
	r.SetPathValue("id", "ag_c")
	r.SetPathValue("qid", rec.ID)
	r = authedRequest(r, auth.Principal{Role: auth.RoleOwner})
	w = httptest.NewRecorder()
	srv.handleCancelQueuedAgentMessage(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("second cancel status = %d", w.Code)
	}
}

func TestDrainDeliversToOnlineHolderInOrder(t *testing.T) {
	srv := newQueueTestServer(t)
	ctx := context.Background()
	st := srv.agents.Store()

	var got []string
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/messages") {
			t.Errorf("unexpected holder request: %s %s", r.Method, r.URL.Path)
		}
		var req struct {
			Content string `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		got = append(got, req.Content)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"accepted":true,"queued":false}`))
	}))
	defer holder.Close()

	seedQueuePeer(t, srv, "peer-back", holder.URL, store.PeerStatusOnline)
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_dr", Name: "drainee"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := st.AcquireAgentLock(ctx, "ag_dr", "peer-back", store.NowMillis(), 60_000); err != nil {
		t.Fatalf("lock: %v", err)
	}
	for _, c := range []string{"first", "second", "third"} {
		if _, err := st.EnqueueHandoffQueuedMessage(ctx, "ag_dr", "peer-back", c); err != nil {
			t.Fatalf("enqueue %s: %v", c, err)
		}
	}

	srv.drainHandoffQueueOnce(ctx)

	if len(got) != 3 || got[0] != "first" || got[1] != "second" || got[2] != "third" {
		t.Fatalf("delivered = %v", got)
	}
	msgs, _ := st.ListHandoffQueuedMessages(ctx, "ag_dr")
	if len(msgs) != 0 {
		t.Fatalf("queue not emptied: %+v", msgs)
	}
}

func TestDrainKeepsMessagesQueuedOnHolderFailure(t *testing.T) {
	srv := newQueueTestServer(t)
	ctx := context.Background()
	st := srv.agents.Store()

	calls := 0
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusConflict) // e.g. busy
	}))
	defer holder.Close()

	seedQueuePeer(t, srv, "peer-busy", holder.URL, store.PeerStatusOnline)
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_busy", Name: "busy"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := st.AcquireAgentLock(ctx, "ag_busy", "peer-busy", store.NowMillis(), 60_000); err != nil {
		t.Fatalf("lock: %v", err)
	}
	for _, c := range []string{"a", "b"} {
		if _, err := st.EnqueueHandoffQueuedMessage(ctx, "ag_busy", "peer-busy", c); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	srv.drainHandoffQueueOnce(ctx)

	if calls != 1 {
		t.Fatalf("holder calls = %d, want 1 (stop on first failure to preserve order)", calls)
	}
	msgs, _ := st.ListHandoffQueuedMessages(ctx, "ag_busy")
	if len(msgs) != 2 {
		t.Fatalf("queue rows = %+v, want both retained", msgs)
	}
}

func TestDrainSkipsOfflineHolderAndLocalWithoutRuntime(t *testing.T) {
	srv := newQueueTestServer(t)
	ctx := context.Background()
	st := srv.agents.Store()

	// Case 1: holder still offline — untouched.
	seedQueuePeer(t, srv, "peer-off", "", store.PeerStatusOffline)
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_off", Name: "off"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := st.AcquireAgentLock(ctx, "ag_off", "peer-off", store.NowMillis(), 60_000); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := st.EnqueueHandoffQueuedMessage(ctx, "ag_off", "peer-off", "wait"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Case 2: holdership moved to the local peer ("hub") but the
	// runtime isn't attached in the manager — rows stay queued for
	// the next trigger instead of being dropped.
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_loc", Name: "loc"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := st.AcquireAgentLock(ctx, "ag_loc", "hub", store.NowMillis(), 60_000); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := st.EnqueueHandoffQueuedMessage(ctx, "ag_loc", "peer-off", "local soon"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	srv.drainHandoffQueueOnce(ctx)

	for _, id := range []string{"ag_off", "ag_loc"} {
		msgs, _ := st.ListHandoffQueuedMessages(ctx, id)
		if len(msgs) != 1 {
			t.Fatalf("%s: queue rows = %+v, want 1 retained", id, msgs)
		}
	}
}

func TestKickHandoffQueueDrainCoalesces(t *testing.T) {
	srv := newQueueTestServer(t)
	// No queued rows — the pass is a no-op; this just exercises the
	// scheduler state machine for races/deadlocks.
	for i := 0; i < 10; i++ {
		srv.kickHandoffQueueDrain()
	}
	deadline := time.After(2 * time.Second)
	for {
		srv.handoffDrainMu.Lock()
		running := srv.handoffDrainRunning
		srv.handoffDrainMu.Unlock()
		if !running {
			return
		}
		select {
		case <-deadline:
			t.Fatal("drain scheduler never settled")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestDrainForwardsIdempotencyKey(t *testing.T) {
	srv := newQueueTestServer(t)
	ctx := context.Background()
	st := srv.agents.Store()

	var gotKeys []string
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKeys = append(gotKeys, r.Header.Get("Idempotency-Key"))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer holder.Close()

	seedQueuePeer(t, srv, "peer-idem", holder.URL, store.PeerStatusOnline)
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_idem", Name: "idem"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := st.AcquireAgentLock(ctx, "ag_idem", "peer-idem", store.NowMillis(), 60_000); err != nil {
		t.Fatalf("lock: %v", err)
	}
	rec, err := st.EnqueueHandoffQueuedMessage(ctx, "ag_idem", "peer-idem", "dedup me")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	srv.drainHandoffQueueOnce(ctx)

	want := idempotencyKeyForQueueID(rec.ID)
	if len(gotKeys) != 1 || gotKeys[0] != want {
		t.Fatalf("Idempotency-Key = %v, want [%s]", gotKeys, want)
	}
}

func TestDrainStopsWhenHoldershipMovesMidDrain(t *testing.T) {
	srv := newQueueTestServer(t)
	ctx := context.Background()
	st := srv.agents.Store()

	delivered := 0
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered++
		if delivered == 1 {
			// Holdership moves away (to the hub itself) while the
			// first message is being delivered.
			if _, err := st.ForceReclaimAgentToLocal(ctx, "ag_move", "hub",
				store.NowMillis(), 60_000); err != nil {
				t.Errorf("mid-drain reclaim: %v", err)
			}
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer holder.Close()

	seedQueuePeer(t, srv, "peer-move", holder.URL, store.PeerStatusOnline)
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_move", Name: "move"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := st.AcquireAgentLock(ctx, "ag_move", "peer-move", store.NowMillis(), 60_000); err != nil {
		t.Fatalf("lock: %v", err)
	}
	for _, c := range []string{"one", "two"} {
		if _, err := st.EnqueueHandoffQueuedMessage(ctx, "ag_move", "peer-move", c); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	srv.drainHandoffQueueForAgent(ctx, "ag_move")

	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1 (stop after holder change)", delivered)
	}
	msgs, _ := st.ListHandoffQueuedMessages(ctx, "ag_move")
	if len(msgs) != 1 || msgs[0].Content != "two" {
		t.Fatalf("retained = %+v, want only second message", msgs)
	}
}

func TestDrainSchedulesBackoffRetryWhenRowsRemain(t *testing.T) {
	srv := newQueueTestServer(t)
	ctx := context.Background()
	st := srv.agents.Store()

	seedQueuePeer(t, srv, "peer-gone", "", store.PeerStatusOffline)
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_bo", Name: "bo"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := st.AcquireAgentLock(ctx, "ag_bo", "peer-gone", store.NowMillis(), 60_000); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := st.EnqueueHandoffQueuedMessage(ctx, "ag_bo", "peer-gone", "stuck"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	srv.kickHandoffQueueDrain()

	deadline := time.After(2 * time.Second)
	for {
		srv.handoffDrainMu.Lock()
		running := srv.handoffDrainRunning
		timerSet := srv.handoffDrainTimer != nil
		backoff := srv.handoffDrainBackoff
		srv.handoffDrainMu.Unlock()
		if !running {
			if !timerSet {
				t.Fatal("no retry timer armed with rows remaining")
			}
			if backoff != handoffDrainRetryMin {
				t.Fatalf("backoff = %v, want %v", backoff, handoffDrainRetryMin)
			}
			// External kick cancels the pending timer and resets backoff.
			srv.kickHandoffQueueDrain()
			break
		}
		select {
		case <-deadline:
			t.Fatal("drain never settled")
		case <-time.After(5 * time.Millisecond):
		}
	}

	deadline = time.After(2 * time.Second)
	for {
		srv.handoffDrainMu.Lock()
		running := srv.handoffDrainRunning
		backoff := srv.handoffDrainBackoff
		srv.handoffDrainMu.Unlock()
		if !running {
			// Reset to 0 by the external kick, then re-armed at min.
			if backoff != handoffDrainRetryMin {
				t.Fatalf("post-kick backoff = %v, want %v (reset then re-armed)", backoff, handoffDrainRetryMin)
			}
			srv.handoffDrainMu.Lock()
			if srv.handoffDrainTimer != nil {
				srv.handoffDrainTimer.Stop()
			}
			srv.handoffDrainMu.Unlock()
			return
		}
		select {
		case <-deadline:
			t.Fatal("second drain never settled")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestIdempotencyKeyForQueueIDIsValidUUID(t *testing.T) {
	k1 := idempotencyKeyForQueueID("hq_abc")
	k2 := idempotencyKeyForQueueID("hq_abc")
	k3 := idempotencyKeyForQueueID("hq_def")
	if k1 != k2 {
		t.Fatalf("not deterministic: %s vs %s", k1, k2)
	}
	if k1 == k3 {
		t.Fatalf("collision across ids: %s", k1)
	}
	// Must survive the receiving peer's validateIdempotencyKey
	// (uuid.Parse + canonical-form round-trip).
	if !validateIdempotencyKey(k1) {
		t.Fatalf("derived key %q rejected by validateIdempotencyKey", k1)
	}
}

func TestCancelRefusedWhileDeliveryInFlight(t *testing.T) {
	srv := newQueueTestServer(t)
	ctx := context.Background()
	st := srv.agents.Store()
	rec, err := st.EnqueueHandoffQueuedMessage(ctx, "ag_race", "peer-x", "in flight")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if !srv.claimQueuedForDelivery(ctx, "ag_race", rec.ID) {
		t.Fatal("claim refused for existing row")
	}

	r := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/ag_race/queued-messages/"+rec.ID, nil)
	r.SetPathValue("id", "ag_race")
	r.SetPathValue("qid", rec.ID)
	r = authedRequest(r, auth.Principal{Role: auth.RoleOwner})
	w := httptest.NewRecorder()
	srv.handleCancelQueuedAgentMessage(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("cancel during claim: status = %d, body %s", w.Code, w.Body.String())
	}
	if !srv.queuedDeliveryInFlight(rec.ID) {
		t.Fatal("claim dropped by refused cancel")
	}

	// After the claim is released, cancel succeeds.
	srv.unclaimQueued(rec.ID)
	r = httptest.NewRequest(http.MethodDelete, "/api/v1/agents/ag_race/queued-messages/"+rec.ID, nil)
	r.SetPathValue("id", "ag_race")
	r.SetPathValue("qid", rec.ID)
	r = authedRequest(r, auth.Principal{Role: auth.RoleOwner})
	w = httptest.NewRecorder()
	srv.handleCancelQueuedAgentMessage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel after unclaim: status = %d, body %s", w.Code, w.Body.String())
	}
}

func TestClaimSkipsRowCancelledAfterSnapshot(t *testing.T) {
	srv := newQueueTestServer(t)
	ctx := context.Background()
	st := srv.agents.Store()
	rec, err := st.EnqueueHandoffQueuedMessage(ctx, "ag_snap", "peer-x", "gone soon")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Cancel lands after the drain's list snapshot but before the
	// claim — the claim's row re-check must report "don't deliver".
	if err := st.DeleteHandoffQueuedMessage(ctx, "ag_snap", rec.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if srv.claimQueuedForDelivery(ctx, "ag_snap", rec.ID) {
		t.Fatal("claim succeeded for cancelled row; message would be delivered after cancel")
	}
	if srv.queuedDeliveryInFlight(rec.ID) {
		t.Fatal("failed claim left the id marked in-flight")
	}
}

func TestForwardAndDrainShareIdempotencyKey(t *testing.T) {
	srv := newQueueTestServer(t)
	ctx := context.Background()
	st := srv.agents.Store()

	var mu sync.Mutex
	var keys []string
	drop := true
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		dropNow := drop
		mu.Unlock()
		if dropNow {
			// Simulate "processed but connection dropped before the
			// response": hijack and slam the TCP conn shut.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("recorder not hijackable")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatalf("hijack: %v", err)
			}
			conn.Close()
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer holder.Close()

	seedQueuePeer(t, srv, "peer-drop", holder.URL, store.PeerStatusOnline)
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_key", Name: "key"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := st.AcquireAgentLock(ctx, "ag_key", "peer-drop", store.NowMillis(), 60_000); err != nil {
		t.Fatalf("lock: %v", err)
	}

	// Send: forward reaches the holder, response is lost → enqueue.
	w := postQueuedSend(srv, "ag_key", "peer-drop", "dedup across paths")
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"queued":true`) {
		t.Fatalf("send: status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	// Drain redelivers with the SAME key derived from the queue id.
	// (The enqueue also kicked a background drain; every attempt —
	// background or manual — must carry the same key, so we just
	// wait for the queue to empty and then compare all seen keys.)
	mu.Lock()
	drop = false
	mu.Unlock()
	srv.drainHandoffQueueForAgent(ctx, "ag_key")
	deadline := time.After(3 * time.Second)
	for {
		msgs, _ := st.ListHandoffQueuedMessages(ctx, "ag_key")
		if len(msgs) == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("queue not emptied: %+v", msgs)
		case <-time.After(10 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(keys) < 2 {
		t.Fatalf("holder saw %d requests, want >= 2 (forward + drain)", len(keys))
	}
	want := idempotencyKeyForQueueID(resp.ID)
	for i, k := range keys {
		if k != want {
			t.Fatalf("request %d key %q != derived-from-queue-id %q", i, k, want)
		}
	}
}

// stopHandoffDrainForTest brings the queue-drain scheduler to a full
// stop (mirrors Shutdown's sequence) so fire-and-forget kicks from
// handlers under test can't touch the store after the test's TempDir
// cleanup starts. Registered by newChunkedSyncTestServer.
func stopHandoffDrainForTest(t *testing.T, srv *Server) {
	t.Helper()
	srv.handoffDrainMu.Lock()
	srv.handoffDrainStopping = true
	if srv.handoffDrainTimer != nil {
		srv.handoffDrainTimer.Stop()
		srv.handoffDrainTimer = nil
	}
	if srv.handoffDrainCancel != nil {
		srv.handoffDrainCancel()
	}
	var stopped chan struct{}
	if srv.handoffDrainRunning {
		if srv.handoffDrainStopped == nil {
			srv.handoffDrainStopped = make(chan struct{})
		}
		stopped = srv.handoffDrainStopped
	}
	srv.handoffDrainMu.Unlock()
	if stopped != nil {
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Log("handoff drain did not stop within 5s")
		}
	}
}

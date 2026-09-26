package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

const originTestOp = "019e7cc9-dd5e-7971-b654-7840c683879f"

func TestGoalHandoffOriginRequiresHolderAndHonorsStop(t *testing.T) {
	s := newChunkedSyncTestServer(t)
	s.peerID = &peer.Identity{DeviceID: "hub"}
	ctx := context.Background()
	a, err := s.agents.Create(agent.AgentConfig{Name: "handoff", Tool: agent.ToolClaude})
	if err != nil {
		t.Fatal(err)
	}
	st := s.agents.Store()
	lock, err := st.AcquireAgentLock(ctx, a.ID, "source", store.NowMillis(), 60000)
	if err != nil {
		t.Fatal(err)
	}
	barrier := &goalHandoffTestBarrier{done: make(chan struct{})}
	s.handoffArrivalCaps = map[string]*handoffArrivalCapability{"cap": {AgentID: a.ID, SessionKey: a.ID + ":slack:C:T", Reservation: barrier}}
	q := goalHandoffOriginRequest{Capability: "cap", Action: "register", OpID: originTestOp, AgentID: a.ID, Source: "source", Target: "target", SessionKey: a.ID + ":slack:C:T", UserID: "UOWNER"}
	if err = s.applyGoalHandoffOrigin(ctx, "other", q); err == nil {
		t.Fatal("non-holder registered")
	}
	if err = s.applyGoalHandoffOrigin(ctx, "source", q); err != nil {
		t.Fatal(err)
	}
	if err = s.applyGoalHandoffOrigin(ctx, "source", q); err == nil {
		t.Fatal("duplicate overwrote immutable registration")
	}
	check := goalHandoffOriginRequest{Action: "check", OpID: q.OpID, AgentID: a.ID}
	if err = s.applyGoalHandoffOrigin(ctx, "target", check); err == nil {
		t.Fatal("resume before lock transfer")
	}
	close(barrier.done)
	if err = s.applyGoalHandoffOrigin(ctx, "source", goalHandoffOriginRequest{Action: "settled", OpID: q.OpID, AgentID: a.ID}); err != nil {
		t.Fatal(err)
	}
	// Simulate authoritative lock moving; not a live production operation.
	if err = st.ReleaseAgentLock(ctx, a.ID, "source", lock.FencingToken); err != nil {
		t.Fatal(err)
	}
	if _, err = st.AcquireAgentLock(ctx, a.ID, "target", store.NowMillis(), 60000); err != nil {
		t.Fatal(err)
	}
	if err = s.applyGoalHandoffOrigin(ctx, "other", check); err == nil {
		t.Fatal("wrong destination checked")
	}
	if err = s.applyGoalHandoffOrigin(ctx, "target", check); err != nil {
		t.Fatal(err)
	}
	router := newExternalChatRouter(s)
	if handled, err := router.StopIdleGoal(ctx, a.ID, q.SessionKey, "UOTHER"); handled || err != nil {
		t.Fatalf("unauthorized stop: %v %v", handled, err)
	}
	if err = s.applyGoalHandoffOrigin(ctx, "target", check); err != nil {
		t.Fatal("unauthorized stop left tombstone")
	}
	// Missing peer target causes transport failure, but stop must persist first.
	if handled, _ := router.StopIdleGoal(ctx, a.ID, q.SessionKey, "UOWNER"); !handled {
		t.Fatal("idle stop not handled")
	}
	s.goalStopFences.Clear() // process cache lost: database tombstone still fences.
	if err = s.applyGoalHandoffOrigin(ctx, "target", check); err == nil {
		t.Fatal("restart lost stop fence")
	}
}
func TestGoalHandoffFeatureNegotiationRequiresExplicitVersion(t *testing.T) {
	s := newChunkedSyncTestServer(t)
	s.peerID = &peer.Identity{DeviceID: "source"}
	for _, version := range []string{"", "v0", "v1"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Kojo-Goal-Handoff", version)
			w.Write([]byte("{}"))
		}))
		got := s.targetSupportsGoalHandoff(context.Background(), server.URL, "ag_a")
		server.Close()
		if got != (version == "v1") {
			t.Fatalf("version=%q supported=%v", version, got)
		}
	}
}
func TestGoalFinalizeCannotDowngradeToOrdinaryArrival(t *testing.T) {
	s := newChunkedSyncTestServer(t)
	s.peerID = &peer.Identity{DeviceID: "source"}
	calls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "unknown field continuation", 400)
	}))
	defer target.Close()
	err := s.dispatchPeerAgentSyncFinalize(context.Background(), target.URL, "target", "ag_x", originTestOp, nil, &handoffContinuation{OriginPeerID: "source", GoalHandoffID: originTestOp})
	if err == nil || errors.Is(err, errFinalizeContinuationDowngrade) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
func TestGoalHandoffStatusIsOwnerOnlyAndSurvivesJournalReload(t *testing.T) {
	s := newChunkedSyncTestServer(t)
	op := &goalHandoffOperation{ID: originTestOp, AgentID: "ag_x", Phase: "transferring", CreatedAt: time.Now()}
	if err := s.saveGoalHandoff(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	for _, p := range []auth.Principal{{Role: auth.RoleOwner}, {Role: auth.RoleAgent, AgentID: "ag_x"}} {
		r := authedRequest(httptest.NewRequest("GET", "/", nil), p)
		r.SetPathValue("op", op.ID)
		w := httptest.NewRecorder()
		s.handleGoalHandoffStatus(w, r)
		if p.IsOwner() {
			if w.Code != 200 || !strings.Contains(w.Body.String(), "transferring") {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		} else if w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
}
func TestGoalHandoffContinuationValidation(t *testing.T) {
	s := newChunkedSyncTestServer(t)
	s.peerID = &peer.Identity{DeviceID: "target"}
	for _, op := range []string{"invalid", originTestOp} {
		q := peerAgentSyncFinalizeRequest{SourceDeviceID: "source", AgentID: "ag_x", OpID: originTestOp, Continuation: &handoffContinuation{OriginPeerID: "source", GoalHandoffID: op}}
		b, _ := json.Marshal(q)
		r := authedRequest(httptest.NewRequest("POST", "/", bytes.NewReader(b)), auth.Principal{Role: auth.RoleOwner})
		w := httptest.NewRecorder()
		s.handlePeerAgentSyncFinalize(w, r)
		if op == "invalid" && w.Code != 400 {
			t.Fatal(w.Code)
		}
		if op == originTestOp && w.Code == 400 {
			t.Fatalf("valid main handoff required old capability: %s", w.Body.String())
		}
	}
}

type goalHandoffTestBarrier struct {
	done chan struct{}
	err  error
}

func (b *goalHandoffTestBarrier) Activate(context.Context, string, string) error { return nil }
func (b *goalHandoffTestBarrier) Release()                                       {}
func (b *goalHandoffTestBarrier) WaitSourceComplete(ctx context.Context) error {
	select {
	case <-b.done:
		return b.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestGoalOriginSettlementWaitsForAdapterAndDoesNotSurviveRestart(t *testing.T) {
	s := newChunkedSyncTestServer(t)
	s.peerID = &peer.Identity{DeviceID: "hub"}
	ctx := context.Background()
	a, err := s.agents.Create(agent.AgentConfig{Name: "barrier", Tool: agent.ToolClaude})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.agents.Store().AcquireAgentLock(ctx, a.ID, "source", store.NowMillis(), 60000); err != nil {
		t.Fatal(err)
	}
	b := &goalHandoffTestBarrier{done: make(chan struct{})}
	key := a.ID + ":slack:C:T"
	s.handoffArrivalCaps = map[string]*handoffArrivalCapability{"cap": {AgentID: a.ID, SessionKey: key, Reservation: b}}
	q := goalHandoffOriginRequest{Action: "register", OpID: originTestOp, AgentID: a.ID, Source: "source", Target: "target", SessionKey: key, Capability: "cap"}
	if err = s.applyGoalHandoffOrigin(ctx, "source", q); err != nil {
		t.Fatal(err)
	}
	settle := goalHandoffOriginRequest{Action: "settled", AgentID: a.ID, OpID: q.OpID}
	short, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()
	if err = s.applyGoalHandoffOrigin(short, "source", settle); err == nil {
		t.Fatal("snapshot allowed before final history")
	}
	if _, err = s.agents.Store().GetKV(ctx, goalHandoffOrigins, q.OpID+"/settled"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("premature settlement recorded")
	}
	// Source stream capability deletion does not erase the captured passive barrier.
	s.finishHandoffArrivalTurn("cap")
	close(b.done)
	if err = s.applyGoalHandoffOrigin(ctx, "source", settle); err != nil {
		t.Fatal(err)
	}
	// Never revive a lost process barrier from the persisted registration.
	s.goalHandoffBarriers.Clear()
	if err = s.applyGoalHandoffOrigin(ctx, "source", settle); err == nil {
		t.Fatal("replayed settlement after barrier lost")
	}
}

func TestGoalIdleStopIgnoresStaleLocalRouteHint(t *testing.T) {
	seen := make(chan goalStopRequest, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q goalStopRequest
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
		}
		seen <- q
		writeJSONResponse(w, 200, map[string]bool{"paused": true})
	}))
	defer target.Close()
	s, router, id := prepareRemoteExternalChat(t, target.URL)
	router.rememberRoute(id, "hub") // stale source hint while authoritative lock is remote
	key := id + ":slack:C:T"
	q := goalHandoffOriginRequest{AgentID: id, Source: "hub", Target: "holder", SessionKey: key, UserID: "UOWNER", OpID: originTestOp}
	body, _ := json.Marshal(q)
	for _, row := range []*store.KVRecord{
		{Namespace: goalHandoffOrigins, Key: q.OpID, Value: string(body), Type: store.KVTypeJSON, Scope: store.KVScopeMachine},
		{Namespace: goalHandoffOrigins, Key: id + "/" + key, Value: q.OpID, Type: store.KVTypeString, Scope: store.KVScopeMachine},
	} {
		if _, err := s.agents.Store().PutKV(context.Background(), row, store.KVPutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	handled, err := router.StopIdleGoal(context.Background(), id, key, "UOWNER")
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	select {
	case got := <-seen:
		if got.HandoffID != q.OpID {
			t.Fatal(got)
		}
	default:
		t.Fatal("stop acknowledged stale local source instead of destination")
	}
}
func TestGoalStopEndpointCannotAcknowledgeStaleSource(t *testing.T) {
	s := newChunkedSyncTestServer(t)
	s.peerID = &peer.Identity{DeviceID: "source"}
	a, err := s.agents.Create(agent.AgentConfig{Name: "stale", Tool: agent.ToolClaude})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.agents.Store().AcquireAgentLock(context.Background(), a.ID, "target", store.NowMillis(), 60000); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(goalStopRequest{SessionKey: "", HandoffID: originTestOp})
	r := authedRequest(httptest.NewRequest("POST", "/", bytes.NewReader(body)), auth.Principal{Role: auth.RolePeer, PeerID: "hub"})
	r.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	s.handleExternalGoalStop(w, r)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "wrong_holder") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestGoalResumeAuthorizationUsesLockNotCachedSource(t *testing.T) {
	s, router, id := prepareRemoteExternalChat(t, "http://holder.example:8080")
	s.externalChat = router
	router.rememberRoute(id, "hub")
	q := goalRecoveryRequest{AgentID: id, HolderID: "holder", SessionKey: id + ":slack:C:T", ThreadID: "019e7cc9-dd5e-7971-b654-7840c683879e", Generation: 5, UserID: "UOWNER"}
	body, _ := json.Marshal(q)
	req := authedRequest(httptest.NewRequest("POST", "/", bytes.NewReader(body)), auth.Principal{Role: auth.RolePeer, PeerID: "holder"})
	w := httptest.NewRecorder()
	s.handlePeerGoalResume(w, req)
	// No Slack bot in this fixture, but authoritative ownership must pass.
	if w.Code != 409 || !strings.Contains(w.Body.String(), "recovery_unavailable") || strings.Contains(w.Body.String(), "wrong_holder") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if router.routeHint(id) != "holder" {
		t.Fatal("did not refresh stale hint")
	}
}

func TestGoalResumeRejectsHubOwnerPeerIdentityMismatch(t *testing.T) {
	s, router, id := prepareRemoteExternalChat(t, "http://holder.example:8080")
	s.externalChat = router
	q := goalRecoveryRequest{AgentID: id, HolderID: "holder", SessionKey: id + ":slack:C:T", ThreadID: "019e7cc9-dd5e-7971-b654-7840c683879e", Generation: 5, UserID: "UOWNER"}
	body, _ := json.Marshal(q)
	req := authedRequest(httptest.NewRequest("POST", "/", bytes.NewReader(body)), auth.Principal{Role: auth.RoleOwner, PeerID: "other-peer"})
	w := httptest.NewRecorder()
	s.handlePeerGoalResume(w, req)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "holder identity mismatch") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestReconcileResolvedGoalHandoffRetiresUncertainFinalize(t *testing.T) {
	s := newChunkedSyncTestServer(t)
	s.peerID = &peer.Identity{DeviceID: "target"}
	pending, db := newPendingSyncTestServer(t)
	s.pendingSyncDB, s.pendingSyncKEK = db, pending.pendingSyncKEK
	ctx := context.Background()
	a, err := s.agents.Create(agent.AgentConfig{Name: "goal-reconcile", Tool: agent.ToolCodex})
	if err != nil {
		t.Fatal(err)
	}
	prepareFencedIncomingForTest(t, s, a.ID, originTestOp, "source")
	if _, err = s.agents.Store().AcceptIncomingHandoff(ctx, a.ID, originTestOp, "source", "target", "source", store.NowMillis(), time.Minute.Milliseconds()); err != nil {
		t.Fatal(err)
	}
	if err = s.agents.Store().ActivateIncomingHandoff(ctx, a.ID, originTestOp); err != nil {
		t.Fatal(err)
	}
	entry := pendingSyncEntry{SourceDeviceID: "source", IncomingFenced: true, RawToken: "secret", ArrivalUncertain: true}
	if err = s.recordPendingAgentSync(ctx, a.ID, originTestOp, entry); err != nil {
		t.Fatal(err)
	}
	b := agent.GoalBinding{Handoff: &agent.GoalHandoff{ID: originTestOp, SourcePeerID: "source", TargetPeerID: "target", Phase: "resuming"}}
	if err = s.reconcileResolvedGoalHandoff(a.ID, b); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.consumePendingAgentSync(ctx, a.ID, originTestOp); err != nil || ok {
		t.Fatalf("pending remains: ok=%v err=%v", ok, err)
	}
	receipt, err := s.agents.Store().GetIncomingHandoff(ctx, a.ID, originTestOp)
	if err != nil || string(receipt.Phase) != "done" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
}

func TestReconcileResolvedGoalHandoffRejectsMismatchedSource(t *testing.T) {
	s := newChunkedSyncTestServer(t)
	s.peerID = &peer.Identity{DeviceID: "target"}
	pending, db := newPendingSyncTestServer(t)
	s.pendingSyncDB, s.pendingSyncKEK = db, pending.pendingSyncKEK
	ctx := context.Background()
	const id = "ag_goal_mismatch"
	entry := pendingSyncEntry{SourceDeviceID: "source", IncomingFenced: true, RawToken: "secret", ArrivalUncertain: true}
	if err := s.recordPendingAgentSync(ctx, id, originTestOp, entry); err != nil {
		t.Fatal(err)
	}
	b := agent.GoalBinding{Handoff: &agent.GoalHandoff{ID: originTestOp, SourcePeerID: "other", TargetPeerID: "target", Phase: "resuming"}}
	if err := s.reconcileResolvedGoalHandoff(id, b); err == nil {
		t.Fatal("mismatched source reconciled")
	}
	got, ok, err := s.consumePendingAgentSync(ctx, id, originTestOp)
	if err != nil || !ok || !got.ArrivalUncertain || got.ArrivalHandled {
		t.Fatalf("pending changed: %+v ok=%v err=%v", got, ok, err)
	}
}

func TestReconcileResolvedGoalHandoffRejectsEmptyDurableSource(t *testing.T) {
	s := newChunkedSyncTestServer(t)
	pending, db := newPendingSyncTestServer(t)
	s.pendingSyncDB, s.pendingSyncKEK = db, pending.pendingSyncKEK
	ctx := context.Background()
	const id = "ag_goal_empty_source"
	entry := pendingSyncEntry{IncomingFenced: true, RawToken: "secret", ArrivalUncertain: true}
	if err := s.recordPendingAgentSync(ctx, id, originTestOp, entry); err != nil {
		t.Fatal(err)
	}
	b := agent.GoalBinding{Handoff: &agent.GoalHandoff{ID: originTestOp, SourcePeerID: "source", TargetPeerID: "target", Phase: "resuming"}}
	if err := s.reconcileResolvedGoalHandoff(id, b); err == nil {
		t.Fatal("empty durable source reconciled")
	}
}

func TestReconcileResolvedGoalHandoffCompletesPersistedDecision(t *testing.T) {
	for _, receiptDone := range []bool{false, true} {
		t.Run(map[bool]string{false: "after-arrival-decision", true: "after-receipt-finish"}[receiptDone], func(t *testing.T) {
			s := newChunkedSyncTestServer(t)
			s.peerID = &peer.Identity{DeviceID: "target"}
			pending, db := newPendingSyncTestServer(t)
			s.pendingSyncDB, s.pendingSyncKEK = db, pending.pendingSyncKEK
			ctx := context.Background()
			a, err := s.agents.Create(agent.AgentConfig{Name: "goal-reconcile-retry", Tool: agent.ToolCodex})
			if err != nil {
				t.Fatal(err)
			}
			prepareFencedIncomingForTest(t, s, a.ID, originTestOp, "source")
			if _, err = s.agents.Store().AcceptIncomingHandoff(ctx, a.ID, originTestOp, "source", "target", "source", store.NowMillis(), time.Minute.Milliseconds()); err != nil {
				t.Fatal(err)
			}
			if err = s.agents.Store().ActivateIncomingHandoff(ctx, a.ID, originTestOp); err != nil {
				t.Fatal(err)
			}
			if receiptDone {
				if err = s.agents.Store().FinishIncomingHandoff(ctx, a.ID, originTestOp); err != nil {
					t.Fatal(err)
				}
			}
			entry := pendingSyncEntry{SourceDeviceID: "source", IncomingFenced: true, RawToken: "secret", ArrivalHandled: true}
			if err = s.recordPendingAgentSync(ctx, a.ID, originTestOp, entry); err != nil {
				t.Fatal(err)
			}
			b := agent.GoalBinding{Handoff: &agent.GoalHandoff{ID: originTestOp, SourcePeerID: "source", TargetPeerID: "target", Phase: "resuming"}}
			if err = s.reconcileResolvedGoalHandoff(a.ID, b); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := s.consumePendingAgentSync(ctx, a.ID, originTestOp); err != nil || ok {
				t.Fatalf("pending remains: ok=%v err=%v", ok, err)
			}
			receipt, err := s.agents.Store().GetIncomingHandoff(ctx, a.ID, originTestOp)
			if err != nil || string(receipt.Phase) != "done" {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
		})
	}
}

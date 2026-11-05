package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

func prepareFencedIncomingForTest(t *testing.T, srv *Server, agentID, opID, source string) {
	t.Helper()
	ctx := context.Background()
	st := srv.agents.Store()
	v, err := st.GetAgentLockVersion(ctx, agentID)
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.GetAgent(ctx, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SyncAgentFromPeer(ctx, store.AgentSyncPayload{Agent: a, IncrementalMessages: true, IncrementalMemoryEntries: true, IncomingHandoff: &store.IncomingHandoff{AgentID: agentID, OpID: opID, SourcePeer: source, TargetPeer: srv.peerID.DeviceID, Expected: v}}); err != nil {
		t.Fatal(err)
	}
}

// ignoreReclaimNotice answers the force-reclaim notice that
// reclaimForRouteTest now sends to the old holder, so fake holders that
// count or block on requests only see the traffic the test is about.
func ignoreReclaimNotice(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == reclaimedPeerPath {
			writeJSONResponse(w, http.StatusOK, peerAgentReclaimedResponse{})
			return
		}
		h(w, r)
	})
}

func reclaimForRouteTest(t *testing.T, srv *Server, id string) {
	t.Helper()
	req := authedRequest(httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+id+"/handoff/force-reclaim", nil), auth.Principal{Role: auth.RoleOwner})
	req.SetPathValue("id", id)
	rr := httptest.NewRecorder()
	srv.handleAgentHandoffForceReclaim(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("reclaim: %d %s", rr.Code, rr.Body.String())
	}
}

func TestExternalChatRouteReclaimInvalidatesCachedRemote(t *testing.T) {
	srv, router, id := prepareRemoteExternalChat(t, "http://unused.invalid")
	oldCtx, err := router.withRouteVersion(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	router.rememberRouteFrom(oldCtx, id, "holder")
	reclaimForRouteTest(t, srv, id)
	// Delayed discovery from the old generation cannot poison the new one.
	router.rememberRouteFrom(oldCtx, id, "holder")
	holder, local, err := router.initialRoute(context.Background(), id)
	if err != nil || !local || holder != "hub" {
		t.Fatalf("route after reclaim: %q %v %v", holder, local, err)
	}
	currentCtx, err := router.withRouteVersion(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	router.rememberRouteFrom(currentCtx, id, "hub")
	router.forgetRouteFrom(oldCtx, id, "hub")
	if router.routeHint(id) != "hub" {
		t.Fatal("old request deleted a new generation hint")
	}
}

func TestExternalChatLatePOSTCannotRepopulateReclaimedRoute(t *testing.T) {
	posted, release := make(chan struct{}), make(chan struct{})
	var posts atomic.Int32
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	holder := httptest.NewServer(ignoreReclaimNotice(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSONResponse(w, 200, externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		posts.Add(1)
		close(posted)
		<-release
		writeExternalChatTestStream(t, w, agent.ChatEvent{Type: "done"})
	}))
	t.Cleanup(holder.Close)
	t.Cleanup(unblock)
	srv, router, id := prepareRemoteExternalChat(t, holder.URL)
	done := make(chan error, 1)
	go func() {
		events, err := router.ChatOneShot(context.Background(), id, "once", agent.OneShotOpts{})
		if events != nil {
			for range events {
			}
		}
		done <- err
	}()
	select {
	case <-posted:
	case <-time.After(5 * time.Second):
		t.Fatal("POST not observed")
	}
	reclaimForRouteTest(t, srv, id)
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("POST did not finish")
	}
	h, local, err := router.initialRoute(context.Background(), id)
	if err != nil || h != "hub" || !local || posts.Load() != 1 {
		t.Fatalf("route=%q local=%v posts=%d err=%v", h, local, posts.Load(), err)
	}
}

func TestHandoffLateSourceReleaseDoesNotStopReclaimedRuntime(t *testing.T) {
	srv, _, id := prepareRemoteExternalChat(t, "http://unused.invalid")
	v, err := srv.agents.Store().GetAgentLockVersion(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	srv.SetOnAgentReleasedAsSource(func(context.Context, string) { calls.Add(1) })
	reclaimForRouteTest(t, srv, id)
	srv.releaseHandoffSource(context.Background(), id, v.Holder, v.Token)
	if calls.Load() != 0 {
		t.Fatal("old release reached runtime hook after reclaim")
	}
}

func TestIncomingFinalizeRetriesAfterRestartWithAtomicProxy(t *testing.T) {
	srv, _, id := prepareRemoteExternalChat(t, "http://unused.invalid")
	pending, db := newPendingSyncTestServer(t)
	srv.pendingSyncDB, srv.pendingSyncKEK = db, pending.pendingSyncKEK
	ctx := context.Background()
	const op = "roundtrip-restart"
	prepareFencedIncomingForTest(t, srv, id, op, "holder")
	if err := srv.recordPendingAgentSync(ctx, id, op, pendingSyncEntry{SourceDeviceID: "holder", IncomingFenced: true, ArrivalHandled: true}); err != nil {
		t.Fatal(err)
	}
	guard := peer.NewAgentLockGuard(srv.agents.Store(), srv.peerID, nil)
	var calls int
	var firstToken int64
	srv.SetOnAgentSyncFinalized(func(ctx context.Context, gotID, raw, proxy, gotOp string) (bool, error) {
		calls++
		if _, exists := srv.agents.Get(gotID); !exists {
			t.Fatal("finalize failed to reload evicted runtime")
		}
		lock, err := srv.agents.Store().GetAgentLock(ctx, gotID)
		if err != nil {
			t.Fatal(err)
		}
		if lock.HolderPeer != "hub" || lock.AllowedProxyPeer != "holder" || proxy != "holder" {
			t.Fatalf("proxy not atomically accepted: %+v proxy=%s", lock, proxy)
		}
		if calls == 1 {
			firstToken = lock.FencingToken
			return false, store.ErrFencingMismatch
		}
		if lock.FencingToken != firstToken {
			t.Fatal("retry minted another token without ownership gap")
		}
		guard.AddAgent(ctx, gotID)
		return false, nil
	})
	payload, _ := json.Marshal(peerAgentSyncFinalizeRequest{AgentID: id, OpID: op, SourceDeviceID: "holder", Continuation: &handoffContinuation{OriginPeerID: "holder", SessionKey: "slack:C:T", Capability: "already-admitted"}})
	call := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := authedRequest(httptest.NewRequest(http.MethodPost, "/api/v1/peers/agent-sync/finalize", bytes.NewReader(payload)), auth.Principal{Role: auth.RolePeer, PeerID: "holder"})
		srv.handlePeerAgentSyncFinalize(rr, req)
		return rr
	}
	srv.agents.TeardownAgentRuntime(id)
	if rr := call(); rr.Code != 503 {
		t.Fatalf("first: %d %s", rr.Code, rr.Body.String())
	}
	srv.agents.TeardownAgentRuntime(id)
	srv.pendingAgentSyncs = nil
	if rr := call(); rr.Code != 200 {
		t.Fatalf("retry: %d %s", rr.Code, rr.Body.String())
	}
	h, err := srv.agents.Store().GetIncomingHandoff(ctx, id, op)
	if err != nil || h.Phase != "done" || calls != 2 {
		t.Fatalf("receipt %+v calls=%d err=%v", h, calls, err)
	}
}

func TestIncomingDropWithoutPendingCredentials(t *testing.T) {
	for _, beforePhase1 := range []bool{false, true} {
		t.Run(map[bool]string{true: "before snapshot", false: "after snapshot before credentials"}[beforePhase1], func(t *testing.T) {
			srv, _, id := prepareRemoteExternalChat(t, "http://unused.invalid")
			ctx := context.Background()
			if !beforePhase1 {
				prepareFencedIncomingForTest(t, srv, id, "cancel", "holder")
			}
			body, _ := json.Marshal(peerAgentSyncDropRequest{AgentID: id, OpID: "cancel", SourceDeviceID: "holder"})
			rr := httptest.NewRecorder()
			req := authedRequest(httptest.NewRequest(http.MethodPost, "/api/v1/peers/agent-sync/drop", bytes.NewReader(body)), auth.Principal{Role: auth.RolePeer, PeerID: "holder"})
			srv.handlePeerAgentSyncDrop(rr, req)
			if rr.Code != 200 {
				t.Fatalf("drop: %d %s", rr.Code, rr.Body.String())
			}
			v, err := srv.agents.Store().GetAgentLockVersion(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := srv.agents.Store().ValidateIncomingHandoff(ctx, &store.IncomingHandoff{AgentID: id, OpID: "cancel", SourcePeer: "holder", TargetPeer: "hub", Expected: v}); !errors.Is(err, store.ErrStaleHandoff) {
				t.Fatalf("drop lost: %v", err)
			}
		})
	}
}

func TestHandoffCancelledAbortKeepsOwnershipFence(t *testing.T) {
	srv, _, id := prepareRemoteExternalChat(t, "http://unused.invalid")
	v, err := srv.agents.Store().GetAgentLockVersion(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), sourceHandoffVersionKey{}, v))
	cancel()
	reclaimForRouteTest(t, srv, id)
	resp := &switchDeviceResponse{}
	srv.abortAfterFailure(ctx, id, resp, "late failure")
	if resp.Outcome != "abort_failed" || !strings.Contains(resp.AbortFailureReason, "source ownership changed") {
		t.Fatalf("stale abort allowed: %+v", resp)
	}
}

func TestHandoffLateFinalizeIsNotSentAfterReclaim(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewServer(ignoreReclaimNotice(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	t.Cleanup(target.Close)
	srv, _, id := prepareRemoteExternalChat(t, target.URL)
	v, err := srv.agents.Store().GetAgentLockVersion(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	reclaimForRouteTest(t, srv, id)
	err = srv.dispatchPeerAgentSyncFinalize(context.Background(), target.URL, "holder", id, "old", nil, nil, v.Token)
	if !errors.Is(err, store.ErrStaleHandoff) || calls.Load() != 0 {
		t.Fatalf("stale finalize sent: calls=%d err=%v", calls.Load(), err)
	}
}

func TestIncomingMultiHopReturnPreservesShadowUntilFinalize(t *testing.T) {
	// Target Hub last sent to holder (B); holder has since handed off to C.
	delegated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("delegation probe must be read-only")
		}
		writeJSONResponse(w, 200, externalChatReadyResponse{HolderPeer: "source-C"})
	}))
	t.Cleanup(delegated.Close)
	srv, _, id := prepareRemoteExternalChat(t, delegated.URL)
	ctx := context.Background()
	before, err := srv.agents.Store().GetAgentLockVersion(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(peerAgentSyncStateRequest{AgentID: id, SourceDeviceID: "source-C"})
	rr := httptest.NewRecorder()
	req := authedRequest(httptest.NewRequest(http.MethodPost, "/api/v1/peers/agent-sync/state", bytes.NewReader(body)), auth.Principal{Role: auth.RolePeer, PeerID: "source-C"})
	srv.handlePeerAgentSyncState(rr, req)
	if rr.Code != 200 {
		t.Fatalf("state: %d %s", rr.Code, rr.Body.String())
	}
	var state store.AgentSyncState
	if err := json.Unmarshal(rr.Body.Bytes(), &state); err != nil || state.Known {
		t.Fatalf("must request full sync: %+v %v", state, err)
	}
	after, err := srv.agents.Store().GetAgentLockVersion(ctx, id)
	if err != nil || after != before {
		t.Fatalf("probe mutated shadow: %+v %v", after, err)
	}
	a, err := srv.agents.Store().GetAgent(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	payload := &peerAgentSyncRequest{Agent: a, OpID: "multi-hop", SourceDeviceID: "source-C"}
	body, _ = json.Marshal(payload)
	rr = httptest.NewRecorder()
	req = authedRequest(httptest.NewRequest(http.MethodPost, "/api/v1/peers/agent-sync", bytes.NewReader(body)), auth.Principal{Role: auth.RolePeer, PeerID: "source-C"})
	srv.handlePeerAgentSync(rr, req)
	if rr.Code != 200 {
		t.Fatalf("phase1: %d %s", rr.Code, rr.Body.String())
	}
	h, err := srv.agents.Store().GetIncomingHandoff(ctx, id, "multi-hop")
	if err != nil || h.Expected != before || h.DelegatedBy != "holder" {
		t.Fatalf("delegation not bound to receipt: %+v %v", h, err)
	}
	if _, err := srv.agents.Store().AcceptIncomingHandoff(ctx, id, "multi-hop", "source-C", "hub", "hub", store.NowMillis(), 300000); err != nil {
		t.Fatal(err)
	}
	lock, err := srv.agents.Store().GetAgentLock(ctx, id)
	if err != nil || lock.HolderPeer != "hub" || lock.AllowedProxyPeer != "hub" {
		t.Fatalf("multi-hop claim: %+v %v", lock, err)
	}
}

func TestIncomingDelegationCannotOverrideLocalReclaim(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	delegated := httptest.NewServer(ignoreReclaimNotice(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		writeJSONResponse(w, 200, externalChatReadyResponse{HolderPeer: "source-C"})
	}))
	t.Cleanup(delegated.Close)
	t.Cleanup(unblock)
	srv, _, id := prepareRemoteExternalChat(t, delegated.URL)
	done := make(chan error, 1)
	go func() { _, _, err := srv.authorizeIncomingSource(context.Background(), id, "source-C"); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not start")
	}
	reclaimForRouteTest(t, srv, id)
	unblock()
	select {
	case err := <-done:
		if !errors.Is(err, store.ErrStaleHandoff) {
			t.Fatalf("late delegation accepted: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not finish")
	}
	// Old source's state probe must not purge the newly reclaimed agent.
	body, _ := json.Marshal(peerAgentSyncStateRequest{AgentID: id, SourceDeviceID: "holder"})
	rr := httptest.NewRecorder()
	req := authedRequest(httptest.NewRequest(http.MethodPost, "/api/v1/peers/agent-sync/state", bytes.NewReader(body)), auth.Principal{Role: auth.RolePeer, PeerID: "holder"})
	srv.handlePeerAgentSyncState(rr, req)
	if rr.Code != 409 {
		t.Fatalf("state after reclaim: %d %s", rr.Code, rr.Body.String())
	}
	lock, err := srv.agents.Store().GetAgentLock(context.Background(), id)
	if err != nil || lock.HolderPeer != "hub" || lock.AllowedProxyPeer != "hub" {
		t.Fatalf("reclaim lost: %+v %v", lock, err)
	}
}

func TestSourceIdentityBindsOwnerRoleDevices(t *testing.T) {
	if verifySignerIsSource(auth.Principal{Role: auth.RoleOwner, PeerID: "device-A"}, "device-B") {
		t.Fatal("device with owner role impersonated source")
	}
	if !verifySignerIsSource(auth.Principal{Role: auth.RoleOwner}, "device-B") {
		t.Fatal("local owner cannot administer handoff")
	}
}

func TestIncomingDelegationReadsHubDBForOwnerDevice(t *testing.T) {
	var proxied atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		writeJSONResponse(w, 200, externalChatReadyResponse{HolderPeer: "unexpected-proxy"})
	}))
	t.Cleanup(remote.Close)
	hub, _, id := prepareRemoteExternalChat(t, remote.URL)
	hub.agents.TeardownAgentRuntime(id)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agents/{id}/external-chat/ready", hub.handleExternalChatReady)
	handler := hub.remoteAgentProxyMiddleware(mux)
	req := authedRequest(httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+id+"/external-chat/ready", nil), auth.Principal{Role: auth.RoleOwner, PeerID: "returning-peer"})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	var ready externalChatReadyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &ready); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 200 || ready.HolderPeer != "holder" || proxied.Load() != 0 {
		t.Fatalf("Hub route probe: code=%d body=%s proxied=%d", rr.Code, rr.Body.String(), proxied.Load())
	}
	// The read-only exception must not widen remote chat POST authorization.
	rr = httptest.NewRecorder()
	req = authedRequest(httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+id+"/external-chat", nil), auth.Principal{Role: auth.RoleOwner, PeerID: "returning-peer"})
	if hub.externalChatPeerAllowed(rr, req) || rr.Code != 403 {
		t.Fatal("readiness exception broadened POST authorization")
	}
}

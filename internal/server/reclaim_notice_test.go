package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// newReclaimTestServer returns a server identified as self with one agent
// whose lock row is held by holder.
func newReclaimTestServer(t *testing.T, self, holder string) (*Server, string) {
	t.Helper()
	srv := newChunkedSyncTestServer(t)
	srv.peerID = &peer.Identity{DeviceID: self, Name: self}
	a, err := srv.agents.Create(agent.AgentConfig{Name: "reclaimed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.agents.Store().AcquireAgentLock(context.Background(), a.ID, holder,
		store.NowMillis(), int64((5*time.Minute)/time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	return srv, a.ID
}

func postReclaimed(t *testing.T, srv *Server, p auth.Principal, reclaimer, agentID string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(peerAgentReclaimedRequest{ReclaimerDeviceID: reclaimer, AgentID: agentID, ReclaimedAt: store.NowMillis()})
	rr := httptest.NewRecorder()
	req := authedRequest(httptest.NewRequest(http.MethodPost, reclaimedPeerPath, bytes.NewReader(body)), p)
	srv.handlePeerAgentReclaimed(rr, req)
	return rr
}

func TestPeerAgentReclaimedReleasesLocalHolder(t *testing.T) {
	srv, id := newReclaimTestServer(t, "old", "old")
	var hooked atomic.Value
	srv.SetOnAgentReleasedAsSource(func(_ context.Context, agentID string) { hooked.Store(agentID) })

	rr := postReclaimed(t, srv, auth.Principal{Role: auth.RolePeer, PeerID: "hub"}, "hub", id)
	if rr.Code != http.StatusOK {
		t.Fatalf("reclaimed: %d %s", rr.Code, rr.Body.String())
	}
	var out peerAgentReclaimedResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || !out.Released {
		t.Fatalf("must release: %+v %v", out, err)
	}
	if got, _ := hooked.Load().(string); got != id {
		t.Fatalf("release hook not fired: %q", got)
	}
	ctx := context.Background()
	st := srv.agents.Store()
	if _, err := st.GetAgentLock(ctx, id); err == nil {
		t.Fatal("lock row must be gone")
	}
	if marked, err := st.HasAgentFullSyncMarker(ctx, id); err != nil || !marked {
		t.Fatalf("full-sync marker: %v %v", marked, err)
	}

	// Next switch back here: the probe must ask for a full snapshot.
	body, _ := json.Marshal(peerAgentSyncStateRequest{AgentID: id, SourceDeviceID: "hub"})
	rr = httptest.NewRecorder()
	req := authedRequest(httptest.NewRequest(http.MethodPost, "/api/v1/peers/agent-sync/state", bytes.NewReader(body)), auth.Principal{Role: auth.RolePeer, PeerID: "hub"})
	srv.handlePeerAgentSyncState(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("state: %d %s", rr.Code, rr.Body.String())
	}
	var state store.AgentSyncState
	if err := json.Unmarshal(rr.Body.Bytes(), &state); err != nil || state.Known {
		t.Fatalf("must request full sync: %+v %v", state, err)
	}

	// A stale incremental payload (marker landed after the probe) is refused.
	a, err := st.GetAgent(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(&peerAgentSyncRequest{Agent: a, OpID: "inc", SourceDeviceID: "hub", SinceMessageSeq: 3})
	rr = httptest.NewRecorder()
	req = authedRequest(httptest.NewRequest(http.MethodPost, "/api/v1/peers/agent-sync", bytes.NewReader(body)), auth.Principal{Role: auth.RolePeer, PeerID: "hub"})
	srv.handlePeerAgentSync(rr, req)
	if rr.Code != http.StatusConflict || !bytes.Contains(rr.Body.Bytes(), []byte("full_sync_required")) {
		t.Fatalf("incremental after reclaim: %d %s", rr.Code, rr.Body.String())
	}
	if marked, _ := st.HasAgentFullSyncMarker(ctx, id); !marked {
		t.Fatal("refused incremental must keep the marker")
	}
}

func TestPeerAgentReclaimedIgnoresNonHolder(t *testing.T) {
	srv, id := newReclaimTestServer(t, "old", "elsewhere")
	fired := false
	srv.SetOnAgentReleasedAsSource(func(context.Context, string) { fired = true })
	rr := postReclaimed(t, srv, auth.Principal{Role: auth.RolePeer, PeerID: "hub"}, "hub", id)
	if rr.Code != http.StatusOK {
		t.Fatalf("reclaimed: %d %s", rr.Code, rr.Body.String())
	}
	var out peerAgentReclaimedResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || out.Released {
		t.Fatalf("non-holder must not release: %+v %v", out, err)
	}
	if fired {
		t.Fatal("release hook must not fire")
	}
	ctx := context.Background()
	lock, err := srv.agents.Store().GetAgentLock(ctx, id)
	if err != nil || lock.HolderPeer != "elsewhere" {
		t.Fatalf("lock must be untouched: %+v %v", lock, err)
	}
	if marked, _ := srv.agents.Store().HasAgentFullSyncMarker(ctx, id); marked {
		t.Fatal("non-holder must not get a full-sync marker")
	}
}

func TestPeerAgentReclaimedRejectsForeignSigner(t *testing.T) {
	srv, id := newReclaimTestServer(t, "old", "old")
	rr := postReclaimed(t, srv, auth.Principal{Role: auth.RolePeer, PeerID: "mallory"}, "hub", id)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("foreign signer: %d %s", rr.Code, rr.Body.String())
	}
	rr = postReclaimed(t, srv, auth.Principal{Role: auth.RolePeer, PeerID: "old"}, "old", id)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("self reclaimer: %d %s", rr.Code, rr.Body.String())
	}
	lock, err := srv.agents.Store().GetAgentLock(context.Background(), id)
	if err != nil || lock.HolderPeer != "old" {
		t.Fatalf("lock must be untouched: %+v %v", lock, err)
	}
	// Owner (manual recovery on the old holder) may name any reclaimer.
	rr = postReclaimed(t, srv, auth.Principal{Role: auth.RoleOwner}, "hub", id)
	if rr.Code != http.StatusOK {
		t.Fatalf("owner: %d %s", rr.Code, rr.Body.String())
	}
}

func readReclaimNotice(t *testing.T, srv *Server, id string) *reclaimNotice {
	t.Helper()
	rec, err := srv.agents.Store().GetKV(context.Background(), reclaimNoticeNamespace, id)
	if err != nil {
		return nil
	}
	var n reclaimNotice
	if err := json.Unmarshal([]byte(rec.Value), &n); err != nil {
		t.Fatal(err)
	}
	return &n
}

func TestDeliverReclaimNoticeReleasesOnlineAndKeepsOfflinePending(t *testing.T) {
	var got atomic.Value
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != reclaimedPeerPath {
			http.NotFound(w, r)
			return
		}
		var req peerAgentReclaimedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		got.Store(req)
		writeJSONResponse(w, http.StatusOK, peerAgentReclaimedResponse{AgentID: req.AgentID, Released: true})
	}))
	t.Cleanup(old.Close)
	srv, id := newReclaimTestServer(t, "hub", "hub")
	ctx := context.Background()
	st := srv.agents.Store()
	for _, p := range []*store.PeerRecord{
		{DeviceID: "old", Name: "Old", URL: old.URL, Status: store.PeerStatusOnline},
		{DeviceID: "off", Name: "Off", URL: "http://127.0.0.1:1", Status: store.PeerStatusOffline},
	} {
		if _, err := st.UpsertPeer(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	// A notice whose reclaim never committed (no stamp) is dropped unsent.
	if err := srv.recordReclaimNotice(ctx, id, 5000); err != nil {
		t.Fatal(err)
	}
	if res, err := srv.deliverReclaimNotice(ctx, id, ""); err != nil || len(res.ReleasedOn) != 0 {
		t.Fatalf("uncommitted notice delivered: %+v %v", res, err)
	}
	if n := readReclaimNotice(t, srv, id); n != nil {
		t.Fatalf("uncommitted notice must be dropped: %+v", n)
	}
	// A failed later attempt announces the committed reclaim's time, even
	// from another lock epoch (e.g. after a restart re-acquired the lock).
	at := store.NowMillis()
	setLocalReclaimedAtForTest(t, srv, id, at, -1)
	if err := srv.recordReclaimNotice(ctx, id, at+1000); err != nil {
		t.Fatal(err)
	}
	res, err := srv.deliverReclaimNotice(ctx, id, "")
	if err != nil {
		t.Fatal(err)
	}
	if req, _ := got.Load().(peerAgentReclaimedRequest); req.ReclaimedAt != at {
		t.Fatalf("must announce the committed reclaim time %d: %+v", at, req)
	}
	if len(res.ReleasedOn) != 1 || res.ReleasedOn[0] != "old" || len(res.PendingPeers) != 1 || res.PendingPeers[0] != "off" {
		t.Fatalf("result: %+v", res)
	}
	if req, _ := got.Load().(peerAgentReclaimedRequest); req.ReclaimerDeviceID != "hub" || req.AgentID != id {
		t.Fatalf("wire request: %+v", req)
	}
	n := readReclaimNotice(t, srv, id)
	if n == nil || len(n.Peers) != 1 || n.Peers[0] != "off" {
		t.Fatalf("notice must keep only the offline peer: %+v", n)
	}

	// Once the agent leaves this device the notice is stale and dropped.
	lock, err := st.GetAgentLock(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseAgentLock(ctx, id, "hub", lock.FencingToken); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcquireAgentLock(ctx, id, "new", store.NowMillis(), 60000); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.deliverReclaimNotice(ctx, id, ""); err != nil {
		t.Fatal(err)
	}
	if n := readReclaimNotice(t, srv, id); n != nil {
		t.Fatalf("stale notice must be dropped: %+v", n)
	}
}

func TestDeliverReclaimNoticeStopsOnConcurrentReclaim(t *testing.T) {
	var calls atomic.Int32
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeError(w, http.StatusConflict, "concurrent_reclaim", "later reclaim here")
	}))
	t.Cleanup(peerSrv.Close)
	srv, id := newReclaimTestServer(t, "hub", "hub")
	ctx := context.Background()
	if _, err := srv.agents.Store().UpsertPeer(ctx, &store.PeerRecord{
		DeviceID: "z", Name: "Z", URL: peerSrv.URL, Status: store.PeerStatusOnline,
	}); err != nil {
		t.Fatal(err)
	}
	at := store.NowMillis()
	setLocalReclaimedAtForTest(t, srv, id, at, 0)
	if err := srv.recordReclaimNotice(ctx, id, at); err != nil {
		t.Fatal(err)
	}
	res, err := srv.deliverReclaimNotice(ctx, id, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SupersededOn) != 1 || res.SupersededOn[0] != "z" || len(res.PendingPeers) != 0 {
		t.Fatalf("result: %+v", res)
	}
	if n := readReclaimNotice(t, srv, id); n != nil {
		t.Fatalf("superseded notice must not be retried: %+v", n)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestForceReclaimNotifiesOldHolder(t *testing.T) {
	var calls atomic.Int32
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != reclaimedPeerPath {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		var req peerAgentReclaimedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		writeJSONResponse(w, http.StatusOK, peerAgentReclaimedResponse{AgentID: req.AgentID, Released: true})
	}))
	t.Cleanup(holder.Close)
	srv, _, id := prepareRemoteExternalChat(t, holder.URL)
	reclaimForRouteTest(t, srv, id)
	if calls.Load() != 1 {
		t.Fatalf("old holder told %d times", calls.Load())
	}
	if n := readReclaimNotice(t, srv, id); n != nil {
		t.Fatalf("delivered notice must be cleared: %+v", n)
	}
}

func postReclaimedAt(t *testing.T, srv *Server, reclaimer, agentID string, at int64) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(peerAgentReclaimedRequest{ReclaimerDeviceID: reclaimer, AgentID: agentID, ReclaimedAt: at})
	rr := httptest.NewRecorder()
	req := authedRequest(httptest.NewRequest(http.MethodPost, reclaimedPeerPath, bytes.NewReader(body)), auth.Principal{Role: auth.RolePeer, PeerID: reclaimer})
	srv.handlePeerAgentReclaimed(rr, req)
	return rr
}

// setLocalReclaimedAtForTest stamps a reclaim time bound to the current
// lock epoch (tokenDelta != 0 forges a stamp from another epoch).
func setLocalReclaimedAtForTest(t *testing.T, srv *Server, id string, at, tokenDelta int64) {
	t.Helper()
	v, err := srv.agents.Store().GetAgentLockVersion(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.agents.Store().PutKV(context.Background(), &store.KVRecord{
		Namespace: store.ReclaimedAtNamespace, Key: id,
		Value: strconv.FormatInt(at, 10) + ":" + strconv.FormatInt(v.Token+tokenDelta, 10),
		Type:  store.KVTypeString, Scope: store.KVScopeMachine,
	}, store.KVPutOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestPeerAgentReclaimedConcurrentReclaimOrder(t *testing.T) {
	// A newer local reclaim keeps the agent, however late the older
	// notice arrives (no dependence on our own delivery queue).
	srv, id := newReclaimTestServer(t, "z", "z")
	// A stamp from another ownership epoch (e.g. before a restart or an
	// earlier stay) does not count: the old holder yields.
	setLocalReclaimedAtForTest(t, srv, id, 9000, -1)
	if at, err := srv.localReclaimedAt(context.Background(), id); err != nil || at != 0 {
		t.Fatalf("stale-epoch stamp must be ignored: %d %v", at, err)
	}
	setLocalReclaimedAtForTest(t, srv, id, 2000, 0)
	if rr := postReclaimedAt(t, srv, "a", id, 1000); rr.Code != http.StatusConflict {
		t.Fatalf("newer local reclaim must win: %d %s", rr.Code, rr.Body.String())
	}
	// Equal timestamps: the higher device ID is ordered later.
	if rr := postReclaimedAt(t, srv, "a", id, 2000); rr.Code != http.StatusConflict {
		t.Fatalf("tie must go to higher ID: %d %s", rr.Code, rr.Body.String())
	}
	lock, err := srv.agents.Store().GetAgentLock(context.Background(), id)
	if err != nil || lock.HolderPeer != "z" {
		t.Fatalf("winner lost the agent: %+v %v", lock, err)
	}
	// A later reclaim elsewhere wins.
	rr := postReclaimedAt(t, srv, "a", id, 3000)
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"released":true`)) {
		t.Fatalf("later reclaim must win: %d %s", rr.Code, rr.Body.String())
	}
	if at, err := srv.localReclaimedAt(context.Background(), id); err != nil || at != 0 {
		t.Fatalf("losing the agent must forget the local reclaim time: %d %v", at, err)
	}

	// The mirror side of the tie yields.
	srv2, id2 := newReclaimTestServer(t, "a", "a")
	setLocalReclaimedAtForTest(t, srv2, id2, 2000, 0)
	rr = postReclaimedAt(t, srv2, "z", id2, 2000)
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"released":true`)) {
		t.Fatalf("lower ID must yield on a tie: %d %s", rr.Code, rr.Body.String())
	}
	// Peers must send reclaimed_at.
	if rr := postReclaimedAt(t, srv, "a", id, 0); rr.Code != http.StatusBadRequest {
		t.Fatalf("missing reclaimed_at: %d %s", rr.Code, rr.Body.String())
	}
}

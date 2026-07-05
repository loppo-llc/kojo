package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// mirrorHolderStub is an httptest holder peer that serves
// GET /api/v1/agents/{id}/messages and counts hits so tests can
// assert the refresher did (or did not) dial it.
type mirrorHolderStub struct {
	hits atomic.Int64
	msgs []*agent.Message
}

func (h *mirrorHolderStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agents/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		h.hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": h.msgs, "hasMore": false})
	})
	return mux
}

// newMirrorRefreshTestServer wires the minimum Server the refresher
// needs: agent store + manager, self identity, and peer_registry rows
// for self + the stub holder. The agent's lock is placed on
// holderStatus-controlled dev_holder.
func newMirrorRefreshTestServer(t *testing.T, holderURL, holderStatus string) (*Server, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	mgr, err := agent.NewManager(slog.Default())
	if err != nil {
		t.Fatalf("agent.NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	st := mgr.Store()
	ctx := context.Background()

	const agentID = "ag_mirror_refresh"
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: agentID, Name: "mr"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := st.UpsertPeer(ctx, &store.PeerRecord{
		DeviceID: "dev_self", Name: "self", URL: "http://127.0.0.1:1",
		Status: store.PeerStatusOnline, LastSeen: now,
	}); err != nil {
		t.Fatalf("upsert self peer: %v", err)
	}
	if _, err := st.UpsertPeer(ctx, &store.PeerRecord{
		DeviceID: "dev_holder", Name: "holder", URL: holderURL,
		Status: holderStatus, LastSeen: now,
	}); err != nil {
		t.Fatalf("upsert holder peer: %v", err)
	}
	if _, err := st.AcquireAgentLock(ctx, agentID, "dev_holder", now, 60_000); err != nil {
		t.Fatalf("acquire lock for holder: %v", err)
	}

	srv := &Server{
		agents:            mgr,
		logger:            slog.Default(),
		peerID:            &peer.Identity{DeviceID: "dev_self", Name: "self"},
		mirrorRefreshDone: make(chan struct{}),
	}
	return srv, agentID
}

// TestMirrorRefresherPushRefreshesWithoutClientGET asserts one
// refreshRemoteMirrors sweep populates the mirror from the holder stub
// with no client GET involved.
func TestMirrorRefresherPushRefreshesWithoutClientGET(t *testing.T) {
	stub := &mirrorHolderStub{msgs: []*agent.Message{
		{ID: "m1", Role: "user", Content: "hello", Timestamp: "2026-07-05T00:00:00Z"},
		{ID: "m2", Role: "assistant", Content: "world", Timestamp: "2026-07-05T00:00:01Z"},
	}}
	holder := httptest.NewServer(stub.handler())
	defer holder.Close()

	srv, agentID := newMirrorRefreshTestServer(t, holder.URL, store.PeerStatusOnline)

	srv.refreshRemoteMirrors(context.Background())

	if got := stub.hits.Load(); got != 1 {
		t.Fatalf("holder hits = %d, want 1", got)
	}
	rows, _, err := srv.agents.Store().ListRemoteMirrorMessages(context.Background(), agentID, 10, "")
	if err != nil {
		t.Fatalf("ListRemoteMirrorMessages: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("mirror rows = %d, want 2", len(rows))
	}
	if rows[0].ID != "m1" || rows[1].ID != "m2" {
		t.Fatalf("mirror rows = %q,%q, want m1,m2", rows[0].ID, rows[1].ID)
	}
	if rows[1].Content != "world" || rows[1].Role != "assistant" {
		t.Fatalf("row m2 = role %q content %q", rows[1].Role, rows[1].Content)
	}
}

// TestMirrorRefresherSkipsOfflineHolder asserts the refresher never
// dials a holder whose peer_registry status is offline and leaves the
// mirror untouched.
func TestMirrorRefresherSkipsOfflineHolder(t *testing.T) {
	stub := &mirrorHolderStub{msgs: []*agent.Message{
		{ID: "m1", Role: "user", Content: "hello", Timestamp: "2026-07-05T00:00:00Z"},
	}}
	holder := httptest.NewServer(stub.handler())
	defer holder.Close()

	srv, agentID := newMirrorRefreshTestServer(t, holder.URL, store.PeerStatusOffline)

	srv.refreshRemoteMirrors(context.Background())

	if got := stub.hits.Load(); got != 0 {
		t.Fatalf("holder hits = %d, want 0 (offline holder must not be dialed)", got)
	}
	rows, _, err := srv.agents.Store().ListRemoteMirrorMessages(context.Background(), agentID, 10, "")
	if err != nil {
		t.Fatalf("ListRemoteMirrorMessages: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("mirror rows = %d, want 0", len(rows))
	}
}

// TestMirrorRefresherSkipsLocallyHeldAgent asserts an agent whose lock
// returned to self is dropped from the refresh set entirely.
func TestMirrorRefresherSkipsLocallyHeldAgent(t *testing.T) {
	stub := &mirrorHolderStub{msgs: []*agent.Message{
		{ID: "m1", Role: "user", Content: "hello", Timestamp: "2026-07-05T00:00:00Z"},
	}}
	holder := httptest.NewServer(stub.handler())
	defer holder.Close()

	srv, agentID := newMirrorRefreshTestServer(t, holder.URL, store.PeerStatusOnline)
	ctx := context.Background()
	st := srv.agents.Store()
	// Simulate the agent coming home: release the remote lock and
	// re-acquire as self.
	if _, err := st.ReleaseAgentLockByPeer(ctx, "dev_holder"); err != nil {
		t.Fatalf("release holder lock: %v", err)
	}
	if _, err := st.AcquireAgentLock(ctx, agentID, "dev_self", time.Now().UnixMilli(), 60_000); err != nil {
		t.Fatalf("re-acquire as self: %v", err)
	}

	srv.refreshRemoteMirrors(ctx)

	if got := stub.hits.Load(); got != 0 {
		t.Fatalf("holder hits = %d, want 0 (local agent must not be refreshed)", got)
	}
}

// TestMirrorRefresherPrunesDeletedMessages asserts a refresh replaces
// the mirrored window: a message previously mirrored but deleted on
// the holder is pruned, while rows older than the fetched window are
// preserved.
func TestMirrorRefresherPrunesDeletedMessages(t *testing.T) {
	// Holder now returns only m1 and m3 — m2 was deleted there.
	stub := &mirrorHolderStub{msgs: []*agent.Message{
		{ID: "m1", Role: "user", Content: "hello", Timestamp: "2026-07-05T00:00:00Z"},
		{ID: "m3", Role: "assistant", Content: "bye", Timestamp: "2026-07-05T00:00:02Z"},
	}}
	holder := httptest.NewServer(stub.handler())
	defer holder.Close()

	srv, agentID := newMirrorRefreshTestServer(t, holder.URL, store.PeerStatusOnline)
	ctx := context.Background()
	st := srv.agents.Store()
	// Pre-seed the mirror as an earlier refresh would have left it:
	// m0 is OLDER than the fetched window (must survive), m1..m3 are
	// inside it (m2 must be pruned).
	seed := []store.RemoteMirrorMessage{
		{ID: "m0", Role: "user", Content: "ancient", Timestamp: "2026-07-04T23:00:00Z"},
		{ID: "m1", Role: "user", Content: "hello", Timestamp: "2026-07-05T00:00:00Z"},
		{ID: "m2", Role: "assistant", Content: "deleted-on-holder", Timestamp: "2026-07-05T00:00:01Z"},
		{ID: "m3", Role: "assistant", Content: "bye", Timestamp: "2026-07-05T00:00:02Z"},
	}
	if err := st.UpsertRemoteMirrorMessages(ctx, agentID, "dev_holder", seed); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}

	srv.refreshRemoteMirrors(ctx)

	rows, _, err := st.ListRemoteMirrorMessages(ctx, agentID, 10, "")
	if err != nil {
		t.Fatalf("ListRemoteMirrorMessages: %v", err)
	}
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.ID)
	}
	if len(rows) != 3 || rows[0].ID != "m0" || rows[1].ID != "m1" || rows[2].ID != "m3" {
		t.Fatalf("mirror rows = %v, want [m0 m1 m3] (m2 pruned, m0 preserved)", got)
	}
}

// TestMirrorRefresherClearsMirrorOnEmptyTranscript asserts an empty
// hasMore=false holder response clears the mirror for that agent.
func TestMirrorRefresherClearsMirrorOnEmptyTranscript(t *testing.T) {
	stub := &mirrorHolderStub{msgs: []*agent.Message{}}
	holder := httptest.NewServer(stub.handler())
	defer holder.Close()

	srv, agentID := newMirrorRefreshTestServer(t, holder.URL, store.PeerStatusOnline)
	ctx := context.Background()
	st := srv.agents.Store()
	seed := []store.RemoteMirrorMessage{
		{ID: "m1", Role: "user", Content: "hello", Timestamp: "2026-07-05T00:00:00Z"},
	}
	if err := st.UpsertRemoteMirrorMessages(ctx, agentID, "dev_holder", seed); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}

	srv.refreshRemoteMirrors(ctx)

	rows, _, err := st.ListRemoteMirrorMessages(ctx, agentID, 10, "")
	if err != nil {
		t.Fatalf("ListRemoteMirrorMessages: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("mirror rows = %d, want 0 (empty holder transcript clears mirror)", len(rows))
	}
}

// TestMirrorRefresherStopsOnDoneClose asserts the background loop
// exits (acking via mirrorRefreshStopped) when mirrorRefreshDone is
// closed, mirroring the Shutdown sequence.
func TestMirrorRefresherStopsOnDoneClose(t *testing.T) {
	srv := &Server{
		logger:               slog.Default(),
		mirrorRefreshDone:    make(chan struct{}),
		mirrorRefreshStopped: make(chan struct{}),
	}
	go srv.runMirrorRefresher()
	close(srv.mirrorRefreshDone)
	select {
	case <-srv.mirrorRefreshStopped:
	case <-time.After(5 * time.Second):
		t.Fatal("refresher did not stop after mirrorRefreshDone was closed")
	}
}

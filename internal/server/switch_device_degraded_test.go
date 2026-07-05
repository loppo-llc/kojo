package server

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// switchTestTarget is a stub peer that accepts the agent-sync
// protocol and captures the sync payload so tests can assert the
// wire carried the degraded / skip metadata.
type switchTestTarget struct {
	mu       sync.Mutex
	syncBody []byte
}

func (tt *switchTestTarget) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/peers/agent-sync/state", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(store.AgentSyncState{Known: false})
	})
	mux.HandleFunc("/api/v1/peers/agent-sync", func(w http.ResponseWriter, r *http.Request) {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("target: gzip reader: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		raw, err := io.ReadAll(gz)
		if err != nil {
			t.Errorf("target: read sync body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		tt.mu.Lock()
		tt.syncBody = raw
		tt.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"agent_id": "ok"})
	})
	for _, p := range []string{"/api/v1/peers/agent-sync/finalize", "/api/v1/peers/agent-sync/drop"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{"agent_id": "ok"})
		})
	}
	return mux
}

// newSwitchTestServer wires the minimum Server a
// handleAgentHandoffSwitch call needs to run the full source-side
// orchestration against the stub target: agent store + manager, peer
// identity, blob store, and peer_registry rows for self + target.
func newSwitchTestServer(t *testing.T, targetURL string) (*Server, string) {
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

	const agentID = "ag_switch_degraded"
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: agentID, Name: "sw"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := st.UpsertPeer(ctx, &store.PeerRecord{
		DeviceID: "dev_src", Name: "src", URL: "http://127.0.0.1:1",
		Status: store.PeerStatusOnline, LastSeen: now,
	}); err != nil {
		t.Fatalf("upsert self peer: %v", err)
	}
	if _, err := st.UpsertPeer(ctx, &store.PeerRecord{
		DeviceID: "dev_tgt", Name: "tgt", URL: targetURL,
		Status: store.PeerStatusOnline, LastSeen: now,
	}); err != nil {
		t.Fatalf("upsert target peer: %v", err)
	}

	srv := &Server{
		agents: mgr,
		logger: slog.Default(),
		peerID: &peer.Identity{DeviceID: "dev_src", Name: "src"},
		blob:   blob.New(t.TempDir(), blob.WithRefs(blob.NewStoreRefs(st, "dev_src"))),
	}
	return srv, agentID
}

func postSwitch(t *testing.T, srv *Server, agentID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/"+agentID+"/handoff/switch", strings.NewReader(body))
	req.SetPathValue("id", agentID)
	req = authedRequest(req, auth.Principal{Role: auth.RoleOwner})
	rec := httptest.NewRecorder()
	srv.handleAgentHandoffSwitch(rec, req)
	return rec
}

// failMemoryFlush stubs the pre-sync memory flush with a
// deterministic failure and restores the real function on cleanup.
func failMemoryFlush(t *testing.T) {
	t.Helper()
	orig := syncAgentMemoryFromDiskFn
	syncAgentMemoryFromDiskFn = func(context.Context, *store.Store, string, *slog.Logger) error {
		return errors.New("boom: disk flush exploded")
	}
	t.Cleanup(func() { syncAgentMemoryFromDiskFn = orig })
}

// TestSwitchDevice_MemoryFlushFailure_HardFailsWithoutFlag pins the
// default contract: a failed memory flush aborts the switch with
// memory_flush_failed (and now advertises the degraded retry).
func TestSwitchDevice_MemoryFlushFailure_HardFailsWithoutFlag(t *testing.T) {
	tt := &switchTestTarget{}
	target := httptest.NewServer(tt.handler(t))
	defer target.Close()
	srv, agentID := newSwitchTestServer(t, target.URL)
	failMemoryFlush(t)

	rec := postSwitch(t, srv, agentID, `{"target_peer_id":"tgt"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "memory_flush_failed") {
		t.Errorf("body missing memory_flush_failed: %s", body)
	}
	if !strings.Contains(body, `degraded`) {
		t.Errorf("error should advertise the degraded retry: %s", body)
	}
	tt.mu.Lock()
	synced := tt.syncBody != nil
	tt.mu.Unlock()
	if synced {
		t.Errorf("sync payload must NOT be dispatched on hard-fail")
	}
}

// TestSwitchDevice_DegradedFlag_ProceedsAndRecordsSkip drives a full
// source-side switch with a failing memory flush and the degraded
// flag set: the switch must complete, the response must record the
// skipped flush, and the wire payload to target must carry
// degraded_flushes so the arrival prompt can warn the agent.
func TestSwitchDevice_DegradedFlag_ProceedsAndRecordsSkip(t *testing.T) {
	tt := &switchTestTarget{}
	target := httptest.NewServer(tt.handler(t))
	defer target.Close()
	srv, agentID := newSwitchTestServer(t, target.URL)
	failMemoryFlush(t)

	rec := postSwitch(t, srv, agentID, `{"target_peer_id":"tgt","degraded":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp switchDeviceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.HasPrefix(resp.Outcome, "completed") {
		t.Fatalf("outcome = %q; want completed* (reason: %s)", resp.Outcome, resp.Reason)
	}
	if !resp.Degraded {
		t.Errorf("resp.Degraded = false; want true")
	}
	if len(resp.DegradedFlushes) != 1 || resp.DegradedFlushes[0] != "memory_flush" {
		t.Errorf("degraded_flushes = %v; want [memory_flush]", resp.DegradedFlushes)
	}
	// The token store in this test setup issues a raw token lazily;
	// whether TokenAutoReissue is set depends on the token callback
	// wiring, which is absent here — LookupAgentToken returns
	// (_, false) so the wire ships no token and the response must
	// advertise the auto re-issue.
	if !resp.TokenAutoReissue {
		t.Errorf("token_auto_reissue = false; want true when source has no raw token")
	}

	tt.mu.Lock()
	raw := tt.syncBody
	tt.mu.Unlock()
	if raw == nil {
		t.Fatalf("target never received the agent-sync payload")
	}
	var wire peerAgentSyncRequest
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode wire payload: %v", err)
	}
	if len(wire.DegradedFlushes) != 1 || wire.DegradedFlushes[0] != "memory_flush" {
		t.Errorf("wire degraded_flushes = %v; want [memory_flush]", wire.DegradedFlushes)
	}
}

// TestPendingSyncEntry_CarriesTransferNotes pins the record→consume
// roundtrip for the new loss-visibility fields (in-memory path; the
// kv-persisted restart path is covered by pending_agent_sync_test.go
// and serializes the same pendingSyncEntry JSON shape).
func TestPendingSyncEntry_CarriesTransferNotes(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	ctx := context.Background()
	in := pendingSyncEntry{
		RawToken:        "",
		DegradedFlushes: []string{"memory_flush", "persona_flush"},
		TransferSkips: []agent.SkippedSessionFile{
			{Path: "big.jsonl", Reason: "oversized", SizeBytes: 99 << 20},
		},
	}
	if err := srv.recordPendingAgentSync(ctx, "ag_n", "op_n", in); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, ok, err := srv.consumePendingAgentSync(ctx, "ag_n", "op_n")
	if err != nil || !ok {
		t.Fatalf("consume: ok=%v err=%v", ok, err)
	}
	if len(got.DegradedFlushes) != 2 || got.DegradedFlushes[0] != "memory_flush" {
		t.Errorf("degraded flushes = %v", got.DegradedFlushes)
	}
	if len(got.TransferSkips) != 1 || got.TransferSkips[0].Reason != "oversized" ||
		got.TransferSkips[0].Path != "big.jsonl" || got.TransferSkips[0].SizeBytes != 99<<20 {
		t.Errorf("transfer skips = %+v", got.TransferSkips)
	}
}

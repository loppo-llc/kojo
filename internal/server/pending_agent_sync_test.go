package server

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
	"github.com/loppo-llc/kojo/internal/store/secretcrypto"
)

// newPendingSyncTestServer builds a Server with just the fields the
// pending-agent-sync methods touch: kv handle + KEK + logger. No
// agent.Manager / session.Manager — those would drag in the heavy_test
// gate. Tests round-trip the in-memory cache and the sealed kv row
// directly via the Server methods.
func newPendingSyncTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	kv, err := store.Open(context.Background(), store.Options{ConfigDir: t.TempDir()})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = kv.Close() })

	kek := make([]byte, secretcrypto.KEKSize)
	if _, err := rand.Read(kek); err != nil {
		t.Fatalf("rand kek: %v", err)
	}
	srv := &Server{
		pendingSyncKEK: kek,
		pendingSyncDB:  kv,
		logger:         slog.Default(),
	}
	return srv, kv
}

// TestPendingAgentSync_RoundTrip exercises the happy path: record →
// consume → commit, all hitting the in-memory cache plus the sealed
// kv row.
func TestPendingAgentSync_RoundTrip(t *testing.T) {
	ctx := context.Background()
	srv, _ := newPendingSyncTestServer(t)

	const (
		agentID = "ag_test"
		opID    = "op_test"
		raw     = "sk-test-raw-token"
	)
	if err := srv.recordPendingAgentSync(ctx, agentID, opID, raw); err != nil {
		t.Fatalf("recordPendingAgentSync: %v", err)
	}
	entry, ok, err := srv.consumePendingAgentSync(ctx, agentID, opID)
	if err != nil {
		t.Fatalf("consumePendingAgentSync: %v", err)
	}
	if !ok {
		t.Fatalf("consume returned ok=false on a freshly-recorded entry")
	}
	if entry.RawToken != raw {
		t.Fatalf("RawToken mismatch: got %q want %q", entry.RawToken, raw)
	}
	if err := srv.commitPendingAgentSync(ctx, agentID, opID); err != nil {
		t.Fatalf("commitPendingAgentSync: %v", err)
	}
	_, ok, err = srv.consumePendingAgentSync(ctx, agentID, opID)
	if err != nil {
		t.Fatalf("post-commit consume: %v", err)
	}
	if ok {
		t.Fatalf("post-commit consume returned ok=true; entry should be gone")
	}
}

// TestPendingAgentSync_SurvivesDaemonRestart simulates the daemon-
// restart scenario by wiping the in-memory cache after recording.
// consumePendingAgentSync must rehydrate from the sealed kv row.
func TestPendingAgentSync_SurvivesDaemonRestart(t *testing.T) {
	ctx := context.Background()
	srv, _ := newPendingSyncTestServer(t)

	const (
		agentID = "ag_restart"
		opID    = "op_restart"
		raw     = "sk-restart-raw"
	)
	if err := srv.recordPendingAgentSync(ctx, agentID, opID, raw); err != nil {
		t.Fatalf("recordPendingAgentSync: %v", err)
	}
	// Wipe the in-memory cache to mimic a daemon restart.
	srv.pendingTokensMu.Lock()
	srv.pendingAgentSyncs = nil
	srv.pendingTokensMu.Unlock()

	entry, ok, err := srv.consumePendingAgentSync(ctx, agentID, opID)
	if err != nil {
		t.Fatalf("post-restart consume: %v", err)
	}
	if !ok {
		t.Fatalf("post-restart consume returned ok=false; sealed kv row should rehydrate")
	}
	if entry.RawToken != raw {
		t.Fatalf("post-restart RawToken mismatch: got %q want %q", entry.RawToken, raw)
	}
}

// TestPendingAgentSync_AADBindsKey ensures a row written under (a1,
// o1) cannot be decrypted under (a2, o2) — defends against a row
// swapped onto the wrong key (manual db edit, future bug in key
// derivation, etc.).
func TestPendingAgentSync_AADBindsKey(t *testing.T) {
	ctx := context.Background()
	srv, kv := newPendingSyncTestServer(t)

	if err := srv.recordPendingAgentSync(ctx, "ag_a", "op_a", "tok_a"); err != nil {
		t.Fatalf("recordPendingAgentSync ag_a: %v", err)
	}
	// Read the sealed row directly and rewrite it under a
	// different key. The AAD bound in seal/open should fail
	// decryption when consumed under the wrong (agent_id, op_id)
	// pair.
	rec, err := kv.GetKV(ctx, pendingAgentSyncNamespace, pendingAgentSyncKey("ag_a", "op_a"))
	if err != nil {
		t.Fatalf("get ag_a row: %v", err)
	}
	if _, err := kv.PutKV(ctx, &store.KVRecord{
		Namespace:      pendingAgentSyncNamespace,
		Key:            pendingAgentSyncKey("ag_b", "op_b"),
		ValueEncrypted: rec.ValueEncrypted,
		Type:           store.KVTypeBinary,
		Scope:          store.KVScopeMachine,
		Secret:         true,
	}, store.KVPutOptions{}); err != nil {
		t.Fatalf("put ag_b row: %v", err)
	}
	// Wipe cache so consume hits kv.
	srv.pendingTokensMu.Lock()
	srv.pendingAgentSyncs = nil
	srv.pendingTokensMu.Unlock()

	_, _, err = srv.consumePendingAgentSync(ctx, "ag_b", "op_b")
	if err == nil {
		t.Fatalf("consume of swapped row returned nil error; AAD binding broken")
	}
}

// TestPendingAgentSync_NoKEKFallback verifies the in-memory-only
// fallback when no KEK is configured: record/consume still works,
// but a cache wipe (= daemon restart) loses the entry.
func TestPendingAgentSync_NoKEKFallback(t *testing.T) {
	ctx := context.Background()
	srv := &Server{logger: slog.Default()}

	if err := srv.recordPendingAgentSync(ctx, "ag_x", "op_x", "tok_x"); err != nil {
		t.Fatalf("record (no kek): %v", err)
	}
	entry, ok, err := srv.consumePendingAgentSync(ctx, "ag_x", "op_x")
	if err != nil {
		t.Fatalf("consume (no kek): %v", err)
	}
	if !ok || entry.RawToken != "tok_x" {
		t.Fatalf("in-memory record/consume broken: ok=%v entry=%+v", ok, entry)
	}
	// Wipe cache → restart sim. Without kv backing the entry
	// must vanish (and orchestrator retries the whole switch).
	srv.pendingTokensMu.Lock()
	srv.pendingAgentSyncs = nil
	srv.pendingTokensMu.Unlock()
	_, ok, err = srv.consumePendingAgentSync(ctx, "ag_x", "op_x")
	if err != nil {
		t.Fatalf("post-wipe consume (no kek): %v", err)
	}
	if ok {
		t.Fatalf("no-kek fallback unexpectedly survived a cache wipe")
	}
}

// TestPendingAgentSync_RejectsWrongShape ensures consume refuses a
// row with the wrong Type/Scope rather than silently attempting to
// decrypt. Defends against a future kv-write bug or operator hand-
// edit landing a non-binary/non-machine row under the pending key.
func TestPendingAgentSync_RejectsWrongShape(t *testing.T) {
	ctx := context.Background()
	srv, kv := newPendingSyncTestServer(t)

	const (
		agentID = "ag_shape"
		opID    = "op_shape"
	)
	if err := srv.recordPendingAgentSync(ctx, agentID, opID, "tok_shape"); err != nil {
		t.Fatalf("record: %v", err)
	}
	rec, err := kv.GetKV(ctx, pendingAgentSyncNamespace, pendingAgentSyncKey(agentID, opID))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Rewrite the row with the wrong Type (string instead of
	// binary). Re-uses the sealed ValueEncrypted bytes so the
	// only thing wrong is the shape metadata.
	if _, err := kv.PutKV(ctx, &store.KVRecord{
		Namespace:      pendingAgentSyncNamespace,
		Key:            pendingAgentSyncKey(agentID, opID),
		ValueEncrypted: rec.ValueEncrypted,
		Type:           store.KVTypeString,
		Scope:          store.KVScopeMachine,
		Secret:         true,
	}, store.KVPutOptions{IfMatchETag: rec.ETag}); err != nil {
		t.Fatalf("rewrite shape: %v", err)
	}
	// Wipe cache so consume hits kv.
	srv.pendingTokensMu.Lock()
	srv.pendingAgentSyncs = nil
	srv.pendingTokensMu.Unlock()

	_, _, err = srv.consumePendingAgentSync(ctx, agentID, opID)
	if err == nil {
		t.Fatalf("consume of wrong-shape row returned nil error")
	}
}

// TestPendingAgentSync_DropDeletesKV exercises the drop path:
// orchestrator abort must delete the kv row so a later consume
// can't resurrect the token.
func TestPendingAgentSync_DropDeletesKV(t *testing.T) {
	ctx := context.Background()
	srv, kv := newPendingSyncTestServer(t)

	if err := srv.recordPendingAgentSync(ctx, "ag_drop", "op_drop", "tok_drop"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := srv.dropPendingAgentSync(ctx, "ag_drop", "op_drop"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := kv.GetKV(ctx, pendingAgentSyncNamespace, pendingAgentSyncKey("ag_drop", "op_drop")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("drop did not delete kv row: err=%v", err)
	}
}

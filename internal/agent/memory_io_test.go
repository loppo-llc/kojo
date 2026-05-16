package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
)

// memoryIOFixture builds a fresh Manager + agent row + agent dir
// suitable for exercising PutAgentMemory / GetAgentMemory /
// DeleteAgentMemory. The newTestManager helper from groupdm_manager_test
// already wires HOME / configdir / store; we just add an in-memory agent
// and let setGlobalStore propagate via newStore.
func memoryIOFixture(t *testing.T, agentID string) *Manager {
	t.Helper()
	mgr := newTestManager(t)
	a := &Agent{ID: agentID, Name: "alice", Tool: "claude"}
	mgr.mu.Lock()
	mgr.agents[agentID] = a
	mgr.mu.Unlock()
	if err := mgr.store.Upsert(a); err != nil {
		t.Fatalf("seed agent %s: %v", agentID, err)
	}
	if err := os.MkdirAll(filepath.Join(agentDir(agentID), "memory"), 0o755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}
	return mgr
}

// TestPutAgentMemory_RoundTrip covers the happy path: PUT writes the
// file + DB row, GET reads back, second PUT honours If-Match etag,
// stale If-Match returns ErrETagMismatch.
func TestPutAgentMemory_RoundTrip(t *testing.T) {
	mgr := memoryIOFixture(t, "ag_io")
	ctx := context.Background()

	rec1, err := mgr.PutAgentMemory(ctx, "ag_io", "first body\n", "")
	if err != nil {
		t.Fatalf("first PUT: %v", err)
	}
	if rec1.ETag == "" {
		t.Error("expected non-empty etag after first PUT")
	}

	// File on disk matches.
	body, err := os.ReadFile(filepath.Join(agentDir("ag_io"), "MEMORY.md"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(body) != "first body\n" {
		t.Errorf("file body = %q, want %q", body, "first body\n")
	}

	// GET returns the same.
	got, err := mgr.GetAgentMemory(ctx, "ag_io")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if got.Body != "first body\n" {
		t.Errorf("GET body = %q, want %q", got.Body, "first body\n")
	}
	if got.ETag != rec1.ETag {
		t.Errorf("etag drift across PUT/GET")
	}

	// Conditional PUT with matching etag → succeeds, etag advances.
	rec2, err := mgr.PutAgentMemory(ctx, "ag_io", "second body\n", rec1.ETag)
	if err != nil {
		t.Fatalf("second PUT: %v", err)
	}
	if rec2.ETag == rec1.ETag {
		t.Error("etag did not advance on body change")
	}

	// Conditional PUT with stale etag → ErrETagMismatch.
	_, err = mgr.PutAgentMemory(ctx, "ag_io", "third body\n", rec1.ETag)
	if !errors.Is(err, store.ErrETagMismatch) {
		t.Errorf("stale If-Match: want ErrETagMismatch, got %v", err)
	}
}

// TestPutAgentMemory_NotFound: agent doesn't exist.
func TestPutAgentMemory_NotFound(t *testing.T) {
	mgr := newTestManager(t)
	_, err := mgr.PutAgentMemory(context.Background(), "ag_nope", "x", "")
	if !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("want ErrAgentNotFound, got %v", err)
	}
}

// TestPutAgentMemory_Archived: refusing writes to dormant agents.
func TestPutAgentMemory_Archived(t *testing.T) {
	mgr := memoryIOFixture(t, "ag_arc")
	mgr.mu.Lock()
	mgr.agents["ag_arc"].Archived = true
	mgr.mu.Unlock()

	_, err := mgr.PutAgentMemory(context.Background(), "ag_arc", "x", "")
	if !errors.Is(err, ErrAgentArchived) {
		t.Errorf("want ErrAgentArchived, got %v", err)
	}
}

// TestDeleteAgentMemory_RoundTrip: write a body, then delete with the
// matching etag → file removed, DB row tombstoned (next GET returns
// ErrNotFound).
func TestDeleteAgentMemory_RoundTrip(t *testing.T) {
	mgr := memoryIOFixture(t, "ag_iodel")
	ctx := context.Background()

	rec, err := mgr.PutAgentMemory(ctx, "ag_iodel", "body\n", "")
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}

	// Stale If-Match → ErrETagMismatch (file untouched).
	if err := mgr.DeleteAgentMemory(ctx, "ag_iodel", "v0-stale"); !errors.Is(err, store.ErrETagMismatch) {
		t.Errorf("stale DELETE: want ErrETagMismatch, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentDir("ag_iodel"), "MEMORY.md")); err != nil {
		t.Errorf("file should still exist after failed DELETE: %v", err)
	}

	// Matching DELETE removes file and tombstones row.
	if err := mgr.DeleteAgentMemory(ctx, "ag_iodel", rec.ETag); err != nil {
		t.Fatalf("matching DELETE: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentDir("ag_iodel"), "MEMORY.md")); !os.IsNotExist(err) {
		t.Errorf("expected file removed after DELETE, got %v", err)
	}
	if _, err := mgr.GetAgentMemory(ctx, "ag_iodel"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected DB tombstone after DELETE, got %v", err)
	}

	// Idempotent DELETE on already-gone state returns nil.
	if err := mgr.DeleteAgentMemory(ctx, "ag_iodel", ""); err != nil {
		t.Errorf("idempotent DELETE: %v", err)
	}
}

// TestPutAgentMemory_AtomicWrite verifies writeFileAtomic doesn't leave
// a half-written body — at any point during the write, MEMORY.md is
// either the previous content or the new content, never partial. Tests
// the contract via a pre-existing file: if the new write fails (we
// can't easily inject a write failure here), the rename never happens
// and the original stays put. Simpler check: temp file is cleaned up
// after a successful write.
func TestPutAgentMemory_AtomicWrite(t *testing.T) {
	mgr := memoryIOFixture(t, "ag_atom")
	ctx := context.Background()
	if _, err := mgr.PutAgentMemory(ctx, "ag_atom", "hello\n", ""); err != nil {
		t.Fatalf("PUT: %v", err)
	}

	dir := agentDir("ag_atom")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" || (len(e.Name()) > 0 && e.Name()[0] == '.' && filepath.Ext(e.Name()) != ".md") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
}

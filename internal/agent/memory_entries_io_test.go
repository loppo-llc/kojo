package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
)

// memoryEntriesFixture is a thin wrapper around memoryIOFixture so the
// memory_entries tests share the same Manager-with-store-and-agent-row
// setup as the MEMORY.md tests.
func memoryEntriesFixture(t *testing.T, agentID string) *Manager {
	return memoryIOFixture(t, agentID)
}

func TestCreateAgentMemoryEntry_RoundTrip(t *testing.T) {
	mgr := memoryEntriesFixture(t, "ag_e1")
	ctx := context.Background()

	rec, err := mgr.CreateAgentMemoryEntry(ctx, "ag_e1", "topic", "go-gotchas", "first body\n")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if rec.ID == "" || rec.ETag == "" {
		t.Errorf("expected ID + ETag set, got %+v", rec)
	}

	// File landed at canonical path.
	want := filepath.Join(agentDir("ag_e1"), "memory", "topics", "go-gotchas.md")
	body, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read canonical file: %v", err)
	}
	if string(body) != "first body\n" {
		t.Errorf("file body = %q, want %q", body, "first body\n")
	}

	// GET round-trips.
	got, err := mgr.GetAgentMemoryEntry(ctx, "ag_e1", rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Body != "first body\n" || got.ETag != rec.ETag {
		t.Errorf("get drift: body=%q etag-eq=%v", got.Body, got.ETag == rec.ETag)
	}
}

func TestCreateAgentMemoryEntry_RejectsCollision(t *testing.T) {
	mgr := memoryEntriesFixture(t, "ag_e2")
	ctx := context.Background()

	if _, err := mgr.CreateAgentMemoryEntry(ctx, "ag_e2", "people", "alice", "a"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := mgr.CreateAgentMemoryEntry(ctx, "ag_e2", "people", "alice", "b")
	if !errors.Is(err, ErrMemoryEntryExists) {
		t.Errorf("collision: want ErrMemoryEntryExists, got %v", err)
	}
}

func TestCreateAgentMemoryEntry_RejectsBadName(t *testing.T) {
	mgr := memoryEntriesFixture(t, "ag_e3")
	ctx := context.Background()

	cases := []struct{ kind, name string }{
		{"topic", ""},
		{"topic", "../escape"},
		{"topic", "with/slash"},
		{"topic", ".dotfile"},
		{"topic", "back\\slash"},
		{"topic", "trailing."},
		{"topic", "trailing "},
		{"topic", "with\x00nul"},
		{"topic", "with\x07bell"},
		{"topic", "<bad>"},
		{"daily", "not-a-date"},
		{"daily", "2025-13-40"}, // regex would pass; time.Parse rejects
		{"unknown-kind", "x"},
	}
	for _, c := range cases {
		if _, err := mgr.CreateAgentMemoryEntry(ctx, "ag_e3", c.kind, c.name, "x"); err == nil {
			t.Errorf("kind=%q name=%q: expected validation error", c.kind, c.name)
		} else if c.kind != "unknown-kind" && !errors.Is(err, ErrInvalidMemoryEntry) {
			t.Errorf("kind=%q name=%q: want ErrInvalidMemoryEntry, got %v", c.kind, c.name, err)
		}
	}
}

// TestDeleteAgentMemoryEntry_IdempotentMissing: empty If-Match → 204
// even on a missing/already-tombstoned row, so the Web UI's retry
// loop doesn't see spurious 404 / 412.
func TestDeleteAgentMemoryEntry_IdempotentMissing(t *testing.T) {
	mgr := memoryEntriesFixture(t, "ag_eD")
	ctx := context.Background()

	if err := mgr.DeleteAgentMemoryEntry(ctx, "ag_eD", "me_nonexistent", ""); err != nil {
		t.Errorf("missing row + empty If-Match: want nil, got %v", err)
	}
}

func TestCreateAgentMemoryEntry_BodyCap(t *testing.T) {
	mgr := memoryEntriesFixture(t, "ag_e4")
	ctx := context.Background()

	huge := strings.Repeat("x", memoryEntryBodyCap+1)
	_, err := mgr.CreateAgentMemoryEntry(ctx, "ag_e4", "topic", "big", huge)
	if err == nil {
		t.Fatal("expected oversize body to be rejected")
	}
}

func TestUpdateAgentMemoryEntry_BodyOnly(t *testing.T) {
	mgr := memoryEntriesFixture(t, "ag_e5")
	ctx := context.Background()

	rec, err := mgr.CreateAgentMemoryEntry(ctx, "ag_e5", "topic", "n", "v1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	body := "v2"
	upd, err := mgr.UpdateAgentMemoryEntry(ctx, "ag_e5", rec.ID, rec.ETag, MemoryEntryPatch{Body: &body})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Body != "v2" {
		t.Errorf("body not updated: %q", upd.Body)
	}
	if upd.ETag == rec.ETag {
		t.Errorf("etag did not advance")
	}

	// File on disk reflects update.
	got, err := os.ReadFile(filepath.Join(agentDir("ag_e5"), "memory", "topics", "n.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "v2" {
		t.Errorf("file body = %q", got)
	}

	// Stale If-Match → 412.
	_, err = mgr.UpdateAgentMemoryEntry(ctx, "ag_e5", rec.ID, rec.ETag, MemoryEntryPatch{Body: &body})
	if !errors.Is(err, store.ErrETagMismatch) {
		t.Errorf("stale If-Match: want ErrETagMismatch, got %v", err)
	}
}

// TestUpdateAgentMemoryEntry_RenameRefused: rename via PATCH is
// explicitly unsupported in this slice (crash-safe rename without an
// intent file is genuinely hard). PATCH with Kind or Name set must
// surface ErrMemoryEntryRenameUnsupported so the handler can map to
// 400 — callers that want rename use DELETE + CREATE.
func TestUpdateAgentMemoryEntry_RenameRefused(t *testing.T) {
	mgr := memoryEntriesFixture(t, "ag_e6")
	ctx := context.Background()

	rec, err := mgr.CreateAgentMemoryEntry(ctx, "ag_e6", "topic", "old", "v")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	newName := "new"
	_, err = mgr.UpdateAgentMemoryEntry(ctx, "ag_e6", rec.ID, rec.ETag, MemoryEntryPatch{Name: &newName})
	if !errors.Is(err, ErrMemoryEntryRenameUnsupported) {
		t.Errorf("rename name: want ErrMemoryEntryRenameUnsupported, got %v", err)
	}

	newKind := "people"
	_, err = mgr.UpdateAgentMemoryEntry(ctx, "ag_e6", rec.ID, rec.ETag, MemoryEntryPatch{Kind: &newKind})
	if !errors.Is(err, ErrMemoryEntryRenameUnsupported) {
		t.Errorf("rename kind: want ErrMemoryEntryRenameUnsupported, got %v", err)
	}
}

func TestDeleteAgentMemoryEntry_RoundTrip(t *testing.T) {
	mgr := memoryEntriesFixture(t, "ag_e8")
	ctx := context.Background()

	rec, err := mgr.CreateAgentMemoryEntry(ctx, "ag_e8", "topic", "z", "x")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := mgr.DeleteAgentMemoryEntry(ctx, "ag_e8", rec.ID, rec.ETag); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// File gone.
	path := filepath.Join(agentDir("ag_e8"), "memory", "topics", "z.md")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file should be gone: %v", err)
	}

	// GET 404s.
	if _, err := mgr.GetAgentMemoryEntry(ctx, "ag_e8", rec.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("post-delete GET: want ErrNotFound, got %v", err)
	}
}

func TestDeleteAgentMemoryEntry_StaleIfMatch(t *testing.T) {
	mgr := memoryEntriesFixture(t, "ag_e9")
	ctx := context.Background()

	rec, err := mgr.CreateAgentMemoryEntry(ctx, "ag_e9", "topic", "z", "x")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	body := "y"
	if _, err := mgr.UpdateAgentMemoryEntry(ctx, "ag_e9", rec.ID, rec.ETag, MemoryEntryPatch{Body: &body}); err != nil {
		t.Fatalf("update to bump etag: %v", err)
	}

	// Old etag is now stale → 412.
	if err := mgr.DeleteAgentMemoryEntry(ctx, "ag_e9", rec.ID, rec.ETag); !errors.Is(err, store.ErrETagMismatch) {
		t.Errorf("stale If-Match: want ErrETagMismatch, got %v", err)
	}
}

func TestListAgentMemoryEntries_Pagination(t *testing.T) {
	mgr := memoryEntriesFixture(t, "ag_eL")
	ctx := context.Background()

	for _, n := range []string{"a", "b", "c", "d", "e"} {
		if _, err := mgr.CreateAgentMemoryEntry(ctx, "ag_eL", "topic", n, n); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}

	rows, err := mgr.ListAgentMemoryEntries(ctx, "ag_eL", MemoryEntryListOptions{Limit: 2})
	if err != nil {
		t.Fatalf("list page1: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(rows))
	}
	cursor := rows[len(rows)-1].Seq

	rows2, err := mgr.ListAgentMemoryEntries(ctx, "ag_eL", MemoryEntryListOptions{Limit: 10, Cursor: cursor})
	if err != nil {
		t.Fatalf("list page2: %v", err)
	}
	if len(rows2) != 3 {
		t.Errorf("page2 len = %d, want 3", len(rows2))
	}
}

func TestListAgentMemoryEntries_KindFilter(t *testing.T) {
	mgr := memoryEntriesFixture(t, "ag_eK")
	ctx := context.Background()

	if _, err := mgr.CreateAgentMemoryEntry(ctx, "ag_eK", "topic", "t1", "x"); err != nil {
		t.Fatalf("seed t1: %v", err)
	}
	if _, err := mgr.CreateAgentMemoryEntry(ctx, "ag_eK", "people", "p1", "x"); err != nil {
		t.Fatalf("seed p1: %v", err)
	}

	rows, err := mgr.ListAgentMemoryEntries(ctx, "ag_eK", MemoryEntryListOptions{Kind: "people"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Kind != "people" {
		t.Errorf("filter failed: %+v", rows)
	}
}

func TestGetAgentMemoryEntry_CrossAgentLeakBlocked(t *testing.T) {
	mgr := memoryEntriesFixture(t, "ag_eA")
	// Add a second live agent on the same Manager so the GET-by-other-id
	// gets past the agent-existence guard and reaches the cross-agent
	// check inside GetAgentMemoryEntry.
	bAgent := &Agent{ID: "ag_eB", Name: "bob", Tool: "claude"}
	mgr.mu.Lock()
	mgr.agents["ag_eB"] = bAgent
	mgr.mu.Unlock()
	if err := mgr.store.Upsert(bAgent); err != nil {
		t.Fatalf("seed agent ag_eB: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(agentDir("ag_eB"), "memory"), 0o755); err != nil {
		t.Fatalf("mkdir ag_eB: %v", err)
	}
	ctx := context.Background()

	rec, err := mgr.CreateAgentMemoryEntry(ctx, "ag_eA", "topic", "secret", "shh")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Asking via the other agent's id must not leak the entry.
	if _, err := mgr.GetAgentMemoryEntry(ctx, "ag_eB", rec.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("cross-agent GET: want ErrNotFound, got %v", err)
	}
}

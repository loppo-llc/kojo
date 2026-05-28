package store

import (
	"context"
	"errors"
	"testing"
)

func TestMemoryEntryRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	rec, err := s.InsertMemoryEntry(ctx, &MemoryEntryRecord{
		ID: "me1", AgentID: "ag", Kind: "topic", Name: "go", Body: "first body",
	}, MemoryEntryInsertOptions{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if rec.Seq != 1 || rec.Version != 1 || rec.ETag == "" || rec.BodySHA256 == "" {
		t.Errorf("post-insert defaults: %+v", rec)
	}

	got, err := s.GetMemoryEntry(ctx, "me1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Body != "first body" {
		t.Errorf("body = %q, want first body", got.Body)
	}

	byName, err := s.FindMemoryEntryByName(ctx, "ag", "topic", "go")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if byName.ID != "me1" {
		t.Errorf("find id = %q, want me1", byName.ID)
	}

	// Update.
	body := "second"
	updated, err := s.UpdateMemoryEntry(ctx, "me1", rec.ETag, MemoryEntryPatch{Body: &body})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.BodySHA256 == rec.BodySHA256 {
		t.Errorf("sha256 should change on body update")
	}
	if updated.Version != 2 {
		t.Errorf("version = %d, want 2", updated.Version)
	}

	// All-nil patch is no-op.
	noop, err := s.UpdateMemoryEntry(ctx, "me1", updated.ETag, MemoryEntryPatch{})
	if err != nil {
		t.Fatalf("noop: %v", err)
	}
	if noop.ETag != updated.ETag {
		t.Errorf("all-nil patch should not change etag")
	}

	// Soft delete then re-insert with same kind/name should succeed (partial unique index excludes tombstones).
	if err := s.SoftDeleteMemoryEntry(ctx, "me1", ""); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := s.GetMemoryEntry(ctx, "me1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound after soft delete, got %v", err)
	}
	if _, err := s.InsertMemoryEntry(ctx, &MemoryEntryRecord{
		ID: "me2", AgentID: "ag", Kind: "topic", Name: "go", Body: "fresh",
	}, MemoryEntryInsertOptions{}); err != nil {
		t.Fatalf("re-insert after soft delete: %v", err)
	}
}

func TestUpdateMemoryEntryRejectsEmptyName(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")
	rec, err := s.InsertMemoryEntry(ctx, &MemoryEntryRecord{
		ID: "x", AgentID: "ag", Kind: "topic", Name: "n", Body: "b",
	}, MemoryEntryInsertOptions{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	empty := ""
	_, err = s.UpdateMemoryEntry(ctx, "x", rec.ETag, MemoryEntryPatch{Name: &empty})
	if err == nil {
		t.Fatal("expected empty-name rejection")
	}
}

func TestMemoryEntryRejectsInvalidKind(t *testing.T) {
	s := openTestStore(t)
	seedAgent(t, s, "ag")
	_, err := s.InsertMemoryEntry(context.Background(), &MemoryEntryRecord{
		ID: "x", AgentID: "ag", Kind: "bogus", Name: "n", Body: "b",
	}, MemoryEntryInsertOptions{})
	if err == nil {
		t.Fatal("expected invalid-kind rejection")
	}
}

func TestMemoryEntryUniqueLiveNaturalKey(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	if _, err := s.InsertMemoryEntry(ctx, &MemoryEntryRecord{
		ID: "a", AgentID: "ag", Kind: "daily", Name: "n", Body: "x",
	}, MemoryEntryInsertOptions{}); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	// Duplicate (agent_id, kind, name) on a live row → schema's partial
	// unique index must reject.
	_, err := s.InsertMemoryEntry(ctx, &MemoryEntryRecord{
		ID: "b", AgentID: "ag", Kind: "daily", Name: "n", Body: "y",
	}, MemoryEntryInsertOptions{})
	if err == nil {
		t.Fatal("expected unique-key violation")
	}
}

func TestSoftDeletedAgentHidesMemoryEntries(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")
	if _, err := s.InsertMemoryEntry(ctx, &MemoryEntryRecord{
		ID: "x", AgentID: "ag", Kind: "topic", Name: "n", Body: "b",
	}, MemoryEntryInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.SoftDeleteAgent(ctx, "ag"); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	if _, err := s.GetMemoryEntry(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected hidden, got %v", err)
	}
	list, _ := s.ListMemoryEntries(ctx, "ag", MemoryEntryListOptions{})
	if len(list) != 0 {
		t.Errorf("list len = %d, want 0", len(list))
	}
}

func TestAgentMemoryUpsert(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	r1, err := s.UpsertAgentMemory(ctx, "ag", "first", "", AgentMemoryInsertOptions{})
	if err != nil {
		t.Fatalf("upsert v1: %v", err)
	}
	if r1.Version != 1 {
		t.Errorf("v1 version = %d, want 1", r1.Version)
	}

	r2, err := s.UpsertAgentMemory(ctx, "ag", "second", r1.ETag, AgentMemoryInsertOptions{})
	if err != nil {
		t.Fatalf("upsert v2: %v", err)
	}
	if r2.Version != 2 {
		t.Errorf("v2 version = %d, want 2", r2.Version)
	}
	if r2.CreatedAt != r1.CreatedAt {
		t.Errorf("created_at must not change on update")
	}

	// Stale etag.
	_, err = s.UpsertAgentMemory(ctx, "ag", "third", r1.ETag, AgentMemoryInsertOptions{})
	if !errors.Is(err, ErrETagMismatch) {
		t.Fatalf("expected ErrETagMismatch, got %v", err)
	}

	// Blind overwrite refused.
	_, err = s.UpsertAgentMemory(ctx, "ag", "third", "", AgentMemoryInsertOptions{})
	if !errors.Is(err, ErrETagMismatch) {
		t.Fatalf("expected ErrETagMismatch on blind overwrite, got %v", err)
	}

	// AllowOverwrite (importer path).
	r3, err := s.UpsertAgentMemory(ctx, "ag", "third", "", AgentMemoryInsertOptions{AllowOverwrite: true})
	if err != nil {
		t.Fatalf("AllowOverwrite: %v", err)
	}
	if r3.Body != "third" {
		t.Errorf("body = %q, want third", r3.Body)
	}

	got, err := s.GetAgentMemory(ctx, "ag")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Body != "third" {
		t.Errorf("read-back body = %q, want third", got.Body)
	}
}

func TestAgentMemoryRejectsTombstonedAgent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")
	if err := s.SoftDeleteAgent(ctx, "ag"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err := s.UpsertAgentMemory(ctx, "ag", "x", "", AgentMemoryInsertOptions{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

package store

import (
	"context"
	"errors"
	"testing"
)

func TestAgentTaskRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	rec, err := s.CreateAgentTask(ctx, &AgentTaskRecord{
		ID: "t1", AgentID: "ag", Title: "first", Status: "pending",
	}, AgentTaskInsertOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if rec.Seq != 1 || rec.Version != 1 || rec.ETag == "" {
		t.Errorf("post-create defaults: %+v", rec)
	}

	// Second task gets seq=2.
	rec2, err := s.CreateAgentTask(ctx, &AgentTaskRecord{
		ID: "t2", AgentID: "ag", Title: "second", Status: "pending",
	}, AgentTaskInsertOptions{})
	if err != nil {
		t.Fatalf("create #2: %v", err)
	}
	if rec2.Seq != 2 {
		t.Errorf("seq = %d, want 2", rec2.Seq)
	}

	got, err := s.GetAgentTask(ctx, "t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "first" {
		t.Errorf("title = %q, want first", got.Title)
	}

	// Update status pending → done.
	done := "done"
	updated, err := s.UpdateAgentTask(ctx, "t1", rec.ETag, AgentTaskPatch{Status: &done})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Status != "done" {
		t.Errorf("status = %q, want done", updated.Status)
	}
	if updated.Version != 2 {
		t.Errorf("version = %d, want 2", updated.Version)
	}
	if updated.ETag == rec.ETag {
		t.Errorf("etag should change on update")
	}

	// All-nil patch is no-op.
	noop, err := s.UpdateAgentTask(ctx, "t1", updated.ETag, AgentTaskPatch{})
	if err != nil {
		t.Fatalf("noop: %v", err)
	}
	if noop.ETag != updated.ETag {
		t.Errorf("noop should not change etag")
	}

	// Title with empty string rejected.
	empty := ""
	if _, err := s.UpdateAgentTask(ctx, "t1", updated.ETag, AgentTaskPatch{Title: &empty}); err == nil {
		t.Fatal("expected empty-title rejection")
	}

	// Stale etag → mismatch.
	if _, err := s.UpdateAgentTask(ctx, "t1", rec.ETag, AgentTaskPatch{Status: &done}); !errors.Is(err, ErrETagMismatch) {
		t.Errorf("stale etag: got %v, want ErrETagMismatch", err)
	}

	// List active.
	list, err := s.ListAgentTasks(ctx, "ag", AgentTaskListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("list len = %d, want 2", len(list))
	}
	if list[0].Seq > list[1].Seq {
		t.Errorf("list not seq-ordered: %d, %d", list[0].Seq, list[1].Seq)
	}

	// Status filter.
	pending := "pending"
	listPending, err := s.ListAgentTasks(ctx, "ag", AgentTaskListOptions{Status: pending})
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(listPending) != 1 || listPending[0].ID != "t2" {
		t.Errorf("status-filtered list: %+v", listPending)
	}

	// Soft delete.
	if err := s.SoftDeleteAgentTask(ctx, "t1", ""); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := s.GetAgentTask(ctx, "t1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound after soft delete, got %v", err)
	}

	// Soft delete on missing row is idempotent unconditionally.
	if err := s.SoftDeleteAgentTask(ctx, "missing", ""); err != nil {
		t.Errorf("idempotent delete: %v", err)
	}
	// But conditional delete on missing row returns mismatch.
	if err := s.SoftDeleteAgentTask(ctx, "missing", "etag-x"); !errors.Is(err, ErrETagMismatch) {
		t.Errorf("conditional missing: got %v, want ErrETagMismatch", err)
	}
}

func TestAgentTaskCreateRejectsInvalidStatus(t *testing.T) {
	s := openTestStore(t)
	seedAgent(t, s, "ag")
	if _, err := s.CreateAgentTask(context.Background(), &AgentTaskRecord{
		ID: "x", AgentID: "ag", Title: "t", Status: "open",
	}, AgentTaskInsertOptions{}); err == nil {
		t.Fatal("expected invalid-status rejection (open is v0 vocab; store accepts only v1)")
	}
}

func TestAgentTaskCreateRejectsTombstonedAgent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")
	if err := s.SoftDeleteAgent(ctx, "ag"); err != nil {
		t.Fatalf("soft delete agent: %v", err)
	}
	if _, err := s.CreateAgentTask(ctx, &AgentTaskRecord{
		ID: "t", AgentID: "ag", Title: "x", Status: "pending",
	}, AgentTaskInsertOptions{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound on tombstoned parent, got %v", err)
	}
}

func TestAgentTaskBulkInsert(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	// Caller-supplied Seq is ignored; the store allocates from MAX+1
	// inside the tx. The test passes deliberately-wrong seq values to
	// confirm they don't survive into the persisted row.
	recs := []*AgentTaskRecord{
		{ID: "b1", AgentID: "ag", Seq: 999, Title: "one", Status: "pending"},
		{ID: "b2", AgentID: "ag", Seq: 999, Title: "two", Status: "done"},
		{ID: "b3", AgentID: "ag", Seq: 999, Title: "three", Status: "pending"},
	}
	n, err := s.BulkInsertAgentTasks(ctx, "ag", recs, AgentTaskInsertOptions{})
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if n != 3 {
		t.Errorf("inserted = %d, want 3", n)
	}
	for i, r := range recs {
		if r.ETag == "" {
			t.Errorf("bulk did not stamp etag on %s", r.ID)
		}
		if r.Seq != int64(i+1) {
			t.Errorf("seq[%s] = %d, want %d (allocated 1..N)", r.ID, r.Seq, i+1)
		}
	}

	// Re-running with same ids skips (preload-hit silent skip).
	n2, err := s.BulkInsertAgentTasks(ctx, "ag", recs, AgentTaskInsertOptions{})
	if err != nil {
		t.Fatalf("bulk re-run: %v", err)
	}
	if n2 != 0 {
		t.Errorf("idempotent re-run inserted %d, want 0", n2)
	}

	// Mixed: some new, some duplicate. New row gets seq=4 (MAX+1).
	mix := []*AgentTaskRecord{
		{ID: "b1", AgentID: "ag", Title: "one", Status: "pending"},
		{ID: "b4", AgentID: "ag", Title: "four", Status: "pending"},
	}
	n3, err := s.BulkInsertAgentTasks(ctx, "ag", mix, AgentTaskInsertOptions{})
	if err != nil {
		t.Fatalf("bulk mixed: %v", err)
	}
	if n3 != 1 {
		t.Errorf("mixed inserted = %d, want 1", n3)
	}
	if mix[1].Seq != 4 {
		t.Errorf("new row seq = %d, want 4 (continues from MAX)", mix[1].Seq)
	}
	// Skipped record (mix[0]) is left untouched (no canonical fields written).
	if mix[0].Seq != 0 || mix[0].ETag != "" {
		t.Errorf("skipped record was mutated: %+v", mix[0])
	}

	// Cross-agent agent_id mismatch surfaces (validation fail).
	if _, err := s.BulkInsertAgentTasks(ctx, "ag", []*AgentTaskRecord{
		{ID: "x", AgentID: "other", Title: "x", Status: "pending"},
	}, AgentTaskInsertOptions{}); err == nil {
		t.Fatal("expected agent_id mismatch rejection")
	}
}

func TestAgentTaskBulkInsertInBatchDuplicate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	// Same id twice in one batch. First wins; second is a silent
	// ON CONFLICT DO NOTHING and the staged-mutation pass leaves the
	// caller's second record untouched.
	recs := []*AgentTaskRecord{
		{ID: "dup", AgentID: "ag", Title: "first", Status: "pending"},
		{ID: "dup", AgentID: "ag", Title: "second", Status: "done"},
	}
	n, err := s.BulkInsertAgentTasks(ctx, "ag", recs, AgentTaskInsertOptions{})
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if n != 1 {
		t.Errorf("inserted = %d, want 1", n)
	}
	got, err := s.GetAgentTask(ctx, "dup")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "first" {
		t.Errorf("title = %q, want first (first-write-wins on in-batch duplicate)", got.Title)
	}
	if recs[0].ETag == "" {
		t.Errorf("first record should be stamped with etag")
	}
	if recs[1].ETag != "" {
		t.Errorf("duplicate record should not be mutated")
	}
}

func TestAgentTaskBulkInsertCrossAgentCollision(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag1")
	seedAgent(t, s, "ag2")

	if _, err := s.CreateAgentTask(ctx, &AgentTaskRecord{
		ID: "shared", AgentID: "ag1", Title: "owned by ag1", Status: "pending",
	}, AgentTaskInsertOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// ag2 attempts to bulk-import a row with the same id. Must be a hard
	// error (not silent skip) — a different-agent collision is data
	// integrity violation, and silently dropping the row would lose the
	// task.
	_, err := s.BulkInsertAgentTasks(ctx, "ag2", []*AgentTaskRecord{
		{ID: "shared", AgentID: "ag2", Title: "owned by ag2", Status: "pending"},
	}, AgentTaskInsertOptions{})
	if err == nil {
		t.Fatal("expected cross-agent collision error, got nil")
	}
}

func TestAgentTaskDeleteAll(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")
	for i, st := range []string{"pending", "done", "pending"} {
		if _, err := s.CreateAgentTask(ctx, &AgentTaskRecord{
			ID:      string(rune('a' + i)),
			AgentID: "ag",
			Title:   "t",
			Status:  st,
		}, AgentTaskInsertOptions{}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	n, err := s.DeleteAllAgentTasks(ctx, "ag")
	if err != nil {
		t.Fatalf("delete all: %v", err)
	}
	if n != 3 {
		t.Errorf("deleted = %d, want 3", n)
	}
	list, err := s.ListAgentTasks(ctx, "ag", AgentTaskListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("post-delete list len = %d, want 0", len(list))
	}
}

func TestAgentTaskETagConditionalUpdate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")
	rec, err := s.CreateAgentTask(ctx, &AgentTaskRecord{
		ID: "t", AgentID: "ag", Title: "x", Status: "pending",
	}, AgentTaskInsertOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Conditional delete with the live etag succeeds.
	if err := s.SoftDeleteAgentTask(ctx, "t", rec.ETag); err != nil {
		t.Fatalf("conditional delete: %v", err)
	}
}

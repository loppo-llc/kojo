package store

import (
	"context"
	"errors"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), Options{ConfigDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestInsertAndGetAgent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec := &AgentRecord{
		ID:   "ag_test",
		Name: "Hana",
		Settings: map[string]any{
			"tool":  "claude",
			"model": "sonnet",
		},
	}
	out, err := s.InsertAgent(ctx, rec, AgentInsertOptions{})
	if err != nil {
		t.Fatalf("InsertAgent: %v", err)
	}
	if out.Seq <= 0 || out.ETag == "" || out.CreatedAt == 0 {
		t.Fatalf("post-insert defaults missing: %+v", out)
	}
	if out.PersonaRef != "ag_test" || out.WorkspaceID != "ag_test" {
		t.Errorf("expected default persona_ref/workspace_id = id, got %q/%q", out.PersonaRef, out.WorkspaceID)
	}

	got, err := s.GetAgent(ctx, "ag_test")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.Name != "Hana" {
		t.Errorf("name = %q, want Hana", got.Name)
	}
	if got.Settings["tool"] != "claude" {
		t.Errorf("settings.tool = %v, want claude", got.Settings["tool"])
	}
	if got.ETag != out.ETag {
		t.Errorf("etag round-trip mismatch: got %q want %q", got.ETag, out.ETag)
	}
}

func TestGetAgentNotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.GetAgent(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestUpdateAgentETagFlow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	first, err := s.InsertAgent(ctx, &AgentRecord{ID: "a1", Name: "A", Settings: map[string]any{"x": 1}}, AgentInsertOptions{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Wrong if-match → ErrETagMismatch.
	_, err = s.UpdateAgent(ctx, "a1", "bogus-etag", func(r *AgentRecord) error {
		r.Name = "ignored"
		return nil
	})
	if !errors.Is(err, ErrETagMismatch) {
		t.Fatalf("expected ErrETagMismatch, got %v", err)
	}

	// Correct if-match → success, version bumps, etag changes.
	updated, err := s.UpdateAgent(ctx, "a1", first.ETag, func(r *AgentRecord) error {
		r.Name = "renamed"
		r.Settings["x"] = 2
		return nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Version != 2 {
		t.Errorf("version = %d, want 2", updated.Version)
	}
	if updated.ETag == first.ETag {
		t.Errorf("etag should change on update")
	}
	if updated.Name != "renamed" {
		t.Errorf("name = %q, want renamed", updated.Name)
	}

	// Stale if-match (the original) is now invalid.
	_, err = s.UpdateAgent(ctx, "a1", first.ETag, func(r *AgentRecord) error { return nil })
	if !errors.Is(err, ErrETagMismatch) {
		t.Fatalf("expected ErrETagMismatch on stale etag, got %v", err)
	}
}

func TestSoftDeleteHidesAgent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.InsertAgent(ctx, &AgentRecord{ID: "del", Name: "Bye"}, AgentInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.SoftDeleteAgent(ctx, "del"); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := s.GetAgent(ctx, "del"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after soft delete, got %v", err)
	}
	// Idempotent.
	if err := s.SoftDeleteAgent(ctx, "del"); err != nil {
		t.Fatalf("repeat soft delete: %v", err)
	}
	if err := s.SoftDeleteAgent(ctx, "never-existed"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestListAgentsOrdered(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for _, id := range []string{"c", "a", "b"} {
		if _, err := s.InsertAgent(ctx, &AgentRecord{ID: id, Name: id}, AgentInsertOptions{}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	list, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("len = %d, want 3", len(list))
	}
	// Insertion order = c, a, b → seq order should be c < a < b.
	if list[0].ID != "c" || list[1].ID != "a" || list[2].ID != "b" {
		t.Errorf("order = %s,%s,%s; want c,a,b", list[0].ID, list[1].ID, list[2].ID)
	}
}

func TestPersonaUpsertRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Persona requires the agents row to exist (FK).
	if _, err := s.InsertAgent(ctx, &AgentRecord{ID: "p1", Name: "P"}, AgentInsertOptions{}); err != nil {
		t.Fatalf("insert agent: %v", err)
	}

	r1, err := s.UpsertAgentPersona(ctx, "p1", "first body", "", AgentInsertOptions{})
	if err != nil {
		t.Fatalf("upsert v1: %v", err)
	}
	if r1.Version != 1 {
		t.Errorf("v1 version = %d, want 1", r1.Version)
	}

	// If-Match against current etag → succeed.
	r2, err := s.UpsertAgentPersona(ctx, "p1", "second body", r1.ETag, AgentInsertOptions{})
	if err != nil {
		t.Fatalf("upsert v2: %v", err)
	}
	if r2.Version != 2 {
		t.Errorf("v2 version = %d, want 2", r2.Version)
	}
	if r2.CreatedAt != r1.CreatedAt {
		t.Errorf("created_at must not change on upsert: %d -> %d", r1.CreatedAt, r2.CreatedAt)
	}
	if r2.BodySHA256 == r1.BodySHA256 {
		t.Errorf("sha256 should differ for different bodies")
	}

	got, err := s.GetAgentPersona(ctx, "p1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Body != "second body" {
		t.Errorf("body = %q, want second body", got.Body)
	}

	// Stale etag → ErrETagMismatch.
	_, err = s.UpsertAgentPersona(ctx, "p1", "third body", r1.ETag, AgentInsertOptions{})
	if !errors.Is(err, ErrETagMismatch) {
		t.Fatalf("expected ErrETagMismatch on stale etag, got %v", err)
	}

	// If-Match against absent persona of a fresh agent → ErrETagMismatch.
	if _, err := s.InsertAgent(ctx, &AgentRecord{ID: "p2", Name: "P2"}, AgentInsertOptions{}); err != nil {
		t.Fatalf("insert p2: %v", err)
	}
	_, err = s.UpsertAgentPersona(ctx, "p2", "x", "anything", AgentInsertOptions{})
	if !errors.Is(err, ErrETagMismatch) {
		t.Fatalf("expected ErrETagMismatch when asserting etag against absent persona, got %v", err)
	}
}

func TestUpsertPersonaRefusesBlindOverwrite(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.InsertAgent(ctx, &AgentRecord{ID: "p", Name: "P"}, AgentInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.UpsertAgentPersona(ctx, "p", "first", "", AgentInsertOptions{}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// No If-Match, no AllowOverwrite → must fail.
	_, err := s.UpsertAgentPersona(ctx, "p", "second", "", AgentInsertOptions{})
	if !errors.Is(err, ErrETagMismatch) {
		t.Fatalf("expected ErrETagMismatch on blind overwrite, got %v", err)
	}
	// AllowOverwrite=true (importer path) → succeeds.
	r, err := s.UpsertAgentPersona(ctx, "p", "second", "", AgentInsertOptions{AllowOverwrite: true})
	if err != nil {
		t.Fatalf("AllowOverwrite upsert: %v", err)
	}
	if r.Body != "second" {
		t.Errorf("body = %q, want second", r.Body)
	}
}

func TestPersonaResurrectionResetsSeqAndCreatedAt(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.InsertAgent(ctx, &AgentRecord{ID: "r", Name: "R"}, AgentInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	r1, err := s.UpsertAgentPersona(ctx, "r", "first", "", AgentInsertOptions{})
	if err != nil {
		t.Fatalf("upsert v1: %v", err)
	}

	// Tombstone the persona via raw SQL (no public soft-delete helper for
	// persona alone — agent soft delete is the typical route, but we want
	// to isolate the resurrection path here).
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE agent_persona SET deleted_at = ? WHERE agent_id = ?`,
		NowMillis(), "r",
	); err != nil {
		t.Fatalf("manual tombstone: %v", err)
	}

	r2, err := s.UpsertAgentPersona(ctx, "r", "after-tombstone", "", AgentInsertOptions{})
	if err != nil {
		t.Fatalf("resurrection upsert: %v", err)
	}
	if r2.Version != 1 {
		t.Errorf("resurrected version = %d, want 1 (fresh chain)", r2.Version)
	}
	if r2.Seq <= r1.Seq {
		t.Errorf("resurrected seq = %d, want > %d", r2.Seq, r1.Seq)
	}

	// Read back the row and confirm seq/created_at on disk match the returned record.
	var dbSeq, dbCreated int64
	err = s.DB().QueryRowContext(ctx,
		`SELECT seq, created_at FROM agent_persona WHERE agent_id = ?`, "r",
	).Scan(&dbSeq, &dbCreated)
	if err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if dbSeq != r2.Seq || dbCreated != r2.CreatedAt {
		t.Errorf("on-disk vs returned mismatch: db(seq=%d,created=%d) vs ret(seq=%d,created=%d)",
			dbSeq, dbCreated, r2.Seq, r2.CreatedAt)
	}
}

func TestSoftDeletedAgentHidesPersonaAndMessages(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.InsertAgent(ctx, &AgentRecord{ID: "x", Name: "X"}, AgentInsertOptions{}); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	if _, err := s.UpsertAgentPersona(ctx, "x", "body", "", AgentInsertOptions{}); err != nil {
		t.Fatalf("persona: %v", err)
	}
	if _, err := s.AppendMessage(ctx, &MessageRecord{ID: "m1", AgentID: "x", Role: "user", Content: "hi"}, MessageInsertOptions{}); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := s.SoftDeleteAgent(ctx, "x"); err != nil {
		t.Fatalf("soft delete agent: %v", err)
	}

	// Persona must vanish from the read path.
	if _, err := s.GetAgentPersona(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("persona of tombstoned agent should be hidden, got %v", err)
	}

	// Messages too.
	if _, err := s.GetMessage(ctx, "m1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("message of tombstoned agent should be hidden, got %v", err)
	}
	list, err := s.ListMessages(ctx, "x", MessageListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("ListMessages on tombstoned agent should be empty, got %d", len(list))
	}
	count, _ := s.CountMessages(ctx, "x")
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
	if _, err := s.LatestMessage(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("latest on tombstoned agent should be ErrNotFound, got %v", err)
	}
}

func TestUpsertPersonaRejectsTombstonedAgent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.InsertAgent(ctx, &AgentRecord{ID: "gone", Name: "G"}, AgentInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.SoftDeleteAgent(ctx, "gone"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err := s.UpsertAgentPersona(ctx, "gone", "body", "", AgentInsertOptions{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on tombstoned agent, got %v", err)
	}
}

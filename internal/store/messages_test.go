package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func seedAgent(t *testing.T, s *Store, id string) {
	t.Helper()
	_, err := s.InsertAgent(context.Background(), &AgentRecord{ID: id, Name: id}, AgentInsertOptions{})
	if err != nil {
		t.Fatalf("seed agent %s: %v", id, err)
	}
}

func TestAppendAndListMessages(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	for i, role := range []string{"user", "assistant", "user"} {
		_, err := s.AppendMessage(ctx, &MessageRecord{
			ID:      "m" + string(rune('a'+i)),
			AgentID: "ag",
			Role:    role,
			Content: "msg-" + string(rune('a'+i)),
		}, MessageInsertOptions{})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	list, err := s.ListMessages(ctx, "ag", MessageListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("len = %d, want 3", len(list))
	}
	for i, m := range list {
		if m.Seq != int64(i+1) {
			t.Errorf("msg[%d].seq = %d, want %d", i, m.Seq, i+1)
		}
	}

	// Pagination: BeforeSeq=2 should return only seq=1.
	page, err := s.ListMessages(ctx, "ag", MessageListOptions{BeforeSeq: 2})
	if err != nil {
		t.Fatalf("list before: %v", err)
	}
	if len(page) != 1 || page[0].Seq != 1 {
		t.Fatalf("BeforeSeq=2: got %v", page)
	}

	// SinceSeq=1 should return seq 2,3.
	page, err = s.ListMessages(ctx, "ag", MessageListOptions{SinceSeq: 1})
	if err != nil {
		t.Fatalf("list since: %v", err)
	}
	if len(page) != 2 || page[0].Seq != 2 {
		t.Fatalf("SinceSeq=1: got %v", page)
	}

	count, err := s.CountMessages(ctx, "ag")
	if err != nil || count != 3 {
		t.Fatalf("count = %d (err=%v), want 3", count, err)
	}

	latest, err := s.LatestMessage(ctx, "ag")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest.Seq != 3 {
		t.Errorf("latest.seq = %d, want 3", latest.Seq)
	}
}

func TestAppendMessageRejectsInvalidRole(t *testing.T) {
	s := openTestStore(t)
	seedAgent(t, s, "ag")
	_, err := s.AppendMessage(context.Background(), &MessageRecord{
		ID: "m1", AgentID: "ag", Role: "bogus", Content: "x",
	}, MessageInsertOptions{})
	if err == nil {
		t.Fatal("expected role validation error")
	}
}

func TestAppendMessageRejectsMissingAgent(t *testing.T) {
	s := openTestStore(t)
	_, err := s.AppendMessage(context.Background(), &MessageRecord{
		ID: "m1", AgentID: "ghost", Role: "user", Content: "x",
	}, MessageInsertOptions{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing agent, got %v", err)
	}
}

func TestUpdateMessagePatchesAndBumpsETag(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	first, err := s.AppendMessage(ctx, &MessageRecord{
		ID: "m1", AgentID: "ag", Role: "user", Content: "orig",
	}, MessageInsertOptions{})
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	body := "rewrote"
	updated, err := s.UpdateMessage(ctx, "m1", first.ETag, MessagePatch{Content: &body})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Content != body {
		t.Errorf("content = %q, want %q", updated.Content, body)
	}
	if updated.Version != 2 {
		t.Errorf("version = %d, want 2", updated.Version)
	}
	if updated.ETag == first.ETag {
		t.Errorf("etag must change on update")
	}

	// Stale etag → ErrETagMismatch.
	_, err = s.UpdateMessage(ctx, "m1", first.ETag, MessagePatch{Content: &body})
	if !errors.Is(err, ErrETagMismatch) {
		t.Fatalf("expected ErrETagMismatch, got %v", err)
	}
}

func TestSoftDeleteAndTruncate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	for i := 0; i < 5; i++ {
		_, err := s.AppendMessage(ctx, &MessageRecord{
			ID: string(rune('A' + i)), AgentID: "ag", Role: "user", Content: "x",
		}, MessageInsertOptions{})
		if err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	if err := s.SoftDeleteMessage(ctx, "C", ""); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	count, _ := s.CountMessages(ctx, "ag")
	if count != 4 {
		t.Errorf("count after soft delete = %d, want 4", count)
	}

	n, err := s.TruncateMessagesAfterSeq(ctx, "ag", 2, "", "")
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	// seq 3 was already deleted; only seq 4 and 5 should be tombstoned.
	if n != 2 {
		t.Errorf("truncate affected = %d, want 2", n)
	}
	count, _ = s.CountMessages(ctx, "ag")
	if count != 2 {
		t.Errorf("count after truncate = %d, want 2", count)
	}

	// Including deleted should restore the full picture.
	all, err := s.ListMessages(ctx, "ag", MessageListOptions{IncludeDeleted: true})
	if err != nil || len(all) != 5 {
		t.Fatalf("include-deleted list len = %d (err=%v), want 5", len(all), err)
	}
}

func TestSoftDeleteMessageRecomputesETag(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	first, err := s.AppendMessage(ctx, &MessageRecord{
		ID: "m1", AgentID: "ag", Role: "user", Content: "x",
	}, MessageInsertOptions{})
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := s.SoftDeleteMessage(ctx, "m1", ""); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	// Read raw row including tombstone — confirm etag changed and version bumped.
	var (
		etag    string
		version int
		del     int64
	)
	err = s.DB().QueryRowContext(ctx,
		`SELECT etag, version, deleted_at FROM agent_messages WHERE id = ?`, "m1",
	).Scan(&etag, &version, &del)
	if err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if etag == first.ETag {
		t.Errorf("etag unchanged after tombstone: %s", etag)
	}
	if version != 2 {
		t.Errorf("version = %d, want 2", version)
	}
	if del == 0 {
		t.Errorf("deleted_at not set")
	}
}

func TestSoftDeleteMessageIfMatch(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	first, err := s.AppendMessage(ctx, &MessageRecord{
		ID: "m1", AgentID: "ag", Role: "user", Content: "x",
	}, MessageInsertOptions{})
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	// Stale etag must reject — the row is still alive, just the caller's
	// view is out of date.
	if err := s.SoftDeleteMessage(ctx, "m1", "v0-stale"); !errors.Is(err, ErrETagMismatch) {
		t.Fatalf("stale ifMatch: want ErrETagMismatch, got %v", err)
	}

	// Matching etag must succeed.
	if err := s.SoftDeleteMessage(ctx, "m1", first.ETag); err != nil {
		t.Fatalf("matching ifMatch: %v", err)
	}

	// After tombstone, conditional delete on a vanished row maps to
	// ErrNotFound — distinguishes "already gone" from "your view is stale".
	if err := s.SoftDeleteMessage(ctx, "m1", first.ETag); !errors.Is(err, ErrNotFound) {
		t.Fatalf("post-tombstone conditional: want ErrNotFound, got %v", err)
	}

	// Empty ifMatchETag preserves legacy idempotent behaviour.
	if err := s.SoftDeleteMessage(ctx, "m1", ""); err != nil {
		t.Fatalf("idempotent re-delete: %v", err)
	}
	if err := s.SoftDeleteMessage(ctx, "m_nonexistent", ""); err != nil {
		t.Fatalf("missing row + empty ifMatch: %v", err)
	}
}

func TestUpdateMessageAllNilNoop(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	first, err := s.AppendMessage(ctx, &MessageRecord{
		ID: "m1", AgentID: "ag", Role: "user", Content: "x",
	}, MessageInsertOptions{})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := s.UpdateMessage(ctx, "m1", first.ETag, MessagePatch{})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.ETag != first.ETag || got.Version != first.Version {
		t.Errorf("all-nil patch should not bump version/etag: got v=%d e=%s", got.Version, got.ETag)
	}
}

func TestNullJSONRejectsInvalidPayload(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")
	_, err := s.AppendMessage(ctx, &MessageRecord{
		ID: "m1", AgentID: "ag", Role: "user", Content: "x",
		ToolUses: json.RawMessage(`{not json`),
	}, MessageInsertOptions{})
	if err == nil {
		t.Fatal("expected invalid-JSON rejection")
	}
}

func TestTruncateRecomputesETag(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	var etagsBefore []string
	for i := 0; i < 4; i++ {
		rec, err := s.AppendMessage(ctx, &MessageRecord{
			ID: string(rune('A' + i)), AgentID: "ag", Role: "user", Content: "x",
		}, MessageInsertOptions{})
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		etagsBefore = append(etagsBefore, rec.ETag)
	}

	n, err := s.TruncateMessagesAfterSeq(ctx, "ag", 1, "", "")
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if n != 3 {
		t.Errorf("affected = %d, want 3", n)
	}

	// seq=1 still alive with original etag.
	rows, err := s.DB().QueryContext(ctx,
		`SELECT id, etag, deleted_at FROM agent_messages WHERE agent_id = ? ORDER BY seq`, "ag")
	if err != nil {
		t.Fatalf("raw query: %v", err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var id, etag string
		var del any
		if err := rows.Scan(&id, &etag, &del); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if i == 0 && etag != etagsBefore[0] {
			t.Errorf("seq=1 etag should be unchanged: got %s want %s", etag, etagsBefore[0])
		}
		if i > 0 && etag == etagsBefore[i] {
			t.Errorf("seq=%d etag should change after truncate: %s", i+1, etag)
		}
		i++
	}
}

func TestBulkAppendMessages(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	// Seed one row through the per-row path so the bulk allocator must
	// pick up after seq=1, not from zero.
	if _, err := s.AppendMessage(ctx, &MessageRecord{
		ID: "m0", AgentID: "ag", Role: "user", Content: "first",
	}, MessageInsertOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const n = 200
	recs := make([]*MessageRecord, 0, n)
	for i := 0; i < n; i++ {
		// Per-row CreatedAt to confirm BulkAppendMessages honors it.
		ts := int64(1_700_000_000_000 + int64(i))
		recs = append(recs, &MessageRecord{
			ID:        fmt.Sprintf("b%03d", i),
			AgentID:   "ag",
			Role:      "user",
			Content:   fmt.Sprintf("msg-%d", i),
			CreatedAt: ts,
			UpdatedAt: ts,
		})
	}
	got, err := s.BulkAppendMessages(ctx, "ag", recs, MessageInsertOptions{})
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if got != n {
		t.Errorf("inserted = %d, want %d", got, n)
	}

	// Each record must have been mutated in place: seq/version/etag set,
	// timestamps preserved.
	seen := map[int64]bool{}
	for i, rec := range recs {
		if rec.Seq == 0 {
			t.Errorf("rec[%d].seq not assigned", i)
		}
		if seen[rec.Seq] {
			t.Errorf("rec[%d].seq=%d duplicated", i, rec.Seq)
		}
		seen[rec.Seq] = true
		if rec.Version != 1 {
			t.Errorf("rec[%d].version = %d, want 1", i, rec.Version)
		}
		if rec.ETag == "" {
			t.Errorf("rec[%d].etag empty", i)
		}
		if rec.CreatedAt != int64(1_700_000_000_000+int64(i)) {
			t.Errorf("rec[%d].created_at = %d, lost per-row timestamp", i, rec.CreatedAt)
		}
	}

	// Seqs continue from the seeded row: should be 2..201, contiguous.
	list, err := s.ListMessages(ctx, "ag", MessageListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != n+1 {
		t.Fatalf("len = %d, want %d", len(list), n+1)
	}
	for i, m := range list {
		if m.Seq != int64(i+1) {
			t.Errorf("list[%d].seq = %d, want %d", i, m.Seq, i+1)
		}
	}
}

func TestBulkAppendMessagesValidation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	// Mismatched agent_id in one record fails the whole batch.
	_, err := s.BulkAppendMessages(ctx, "ag", []*MessageRecord{
		{ID: "x1", AgentID: "ag", Role: "user", Content: "ok"},
		{ID: "x2", AgentID: "other", Role: "user", Content: "wrong"},
	}, MessageInsertOptions{})
	if err == nil {
		t.Fatal("expected agent_id mismatch rejection")
	}
	count, _ := s.CountMessages(ctx, "ag")
	if count != 0 {
		t.Errorf("partial commit on validation failure: count=%d", count)
	}

	// Invalid role.
	_, err = s.BulkAppendMessages(ctx, "ag", []*MessageRecord{
		{ID: "x1", AgentID: "ag", Role: "bogus", Content: "x"},
	}, MessageInsertOptions{})
	if err == nil {
		t.Fatal("expected invalid role rejection")
	}

	// Empty id.
	_, err = s.BulkAppendMessages(ctx, "ag", []*MessageRecord{
		{ID: "", AgentID: "ag", Role: "user", Content: "x"},
	}, MessageInsertOptions{})
	if err == nil {
		t.Fatal("expected empty id rejection")
	}

	// Soft-deleted parent → ErrNotFound.
	if err := s.SoftDeleteAgent(ctx, "ag"); err != nil {
		t.Fatalf("soft delete agent: %v", err)
	}
	_, err = s.BulkAppendMessages(ctx, "ag", []*MessageRecord{
		{ID: "x1", AgentID: "ag", Role: "user", Content: "x"},
	}, MessageInsertOptions{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for tombstoned parent, got %v", err)
	}

	// Empty batch is a no-op (not an error).
	got, err := s.BulkAppendMessages(ctx, "ag", nil, MessageInsertOptions{})
	if err != nil || got != 0 {
		t.Errorf("empty batch: got (%d, %v), want (0, nil)", got, err)
	}
}

func TestBulkAppendMessagesRollsBackOnConflict(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	// Pre-seed an id so the second record in the batch collides on PK.
	if _, err := s.AppendMessage(ctx, &MessageRecord{
		ID: "dup", AgentID: "ag", Role: "user", Content: "first",
	}, MessageInsertOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Caller's records — must remain pristine after a failed bulk so
	// retries can read the unchanged input slice.
	freshRec := &MessageRecord{ID: "fresh", AgentID: "ag", Role: "user", Content: "would-commit"}
	dupRec := &MessageRecord{ID: "dup", AgentID: "ag", Role: "user", Content: "collides"}

	_, err := s.BulkAppendMessages(ctx, "ag", []*MessageRecord{freshRec, dupRec}, MessageInsertOptions{})
	if err == nil {
		t.Fatal("expected PK conflict")
	}

	// "fresh" must NOT be visible — bulk is all-or-nothing.
	if _, err := s.GetMessage(ctx, "fresh"); !errors.Is(err, ErrNotFound) {
		t.Errorf("partial commit leaked: GetMessage(fresh) err=%v", err)
	}
	count, _ := s.CountMessages(ctx, "ag")
	if count != 1 {
		t.Errorf("count = %d, want 1 (only the seeded row)", count)
	}

	// Caller's records must NOT be mutated when the transaction rolled
	// back — Seq/Version/ETag must be zero, and the caller's CreatedAt
	// must remain the initial zero so a retry isn't skewed.
	for _, rec := range []*MessageRecord{freshRec, dupRec} {
		if rec.Seq != 0 || rec.Version != 0 || rec.ETag != "" {
			t.Errorf("rec %q mutated despite rollback: seq=%d version=%d etag=%q",
				rec.ID, rec.Seq, rec.Version, rec.ETag)
		}
	}
}

func TestBulkAppendMessagesRejectsOptsSeq(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	// opts.Seq is per-row in the bulk path. A non-zero opts.Seq should
	// return an error so callers don't silently get AppendMessage-vs-
	// BulkAppendMessages divergence.
	_, err := s.BulkAppendMessages(ctx, "ag", []*MessageRecord{
		{ID: "x1", AgentID: "ag", Role: "user", Content: "x"},
	}, MessageInsertOptions{Seq: 42})
	if err == nil {
		t.Fatal("expected opts.Seq rejection")
	}
}

func TestMessageRoundtripsJSONFields(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	tools := json.RawMessage(`[{"name":"Read","input":"{\"path\":\"/x\"}"}]`)
	usage := json.RawMessage(`{"inputTokens":10,"outputTokens":5}`)

	first, err := s.AppendMessage(ctx, &MessageRecord{
		ID: "m1", AgentID: "ag", Role: "assistant",
		Content: "hi", ToolUses: tools, Usage: usage,
	}, MessageInsertOptions{})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := s.GetMessage(ctx, "m1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got.ToolUses) != string(tools) {
		t.Errorf("tool_uses round-trip: got %s want %s", got.ToolUses, tools)
	}
	if string(got.Usage) != string(usage) {
		t.Errorf("usage round-trip: got %s want %s", got.Usage, usage)
	}
	if got.ETag != first.ETag {
		t.Errorf("etag drift: %s vs %s", got.ETag, first.ETag)
	}
}

// TestTruncateMessagesAfterSeqPivot covers the optimistic-locking
// guard: the pivot row's etag is checked inside the same transaction
// as the suffix UPDATEs, so a cross-device edit that lands between
// the regenerate handler's pre-flight check and this call cannot
// silently truncate against a stale view.
func TestTruncateMessagesAfterSeqPivot(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	var pivotETag string
	for i := 0; i < 4; i++ {
		rec, err := s.AppendMessage(ctx, &MessageRecord{
			ID: string(rune('A' + i)), AgentID: "ag", Role: "user", Content: "x",
		}, MessageInsertOptions{})
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if rec.ID == "B" {
			pivotETag = rec.ETag
		}
	}
	if pivotETag == "" {
		t.Fatal("pivot etag not captured")
	}

	t.Run("stale pivot etag aborts before any tombstone", func(t *testing.T) {
		// All 4 rows alive before the call.
		n, err := s.TruncateMessagesAfterSeq(ctx, "ag", 2, "B", "v0-stale")
		if !errors.Is(err, ErrETagMismatch) {
			t.Fatalf("want ErrETagMismatch, got %v (n=%d)", err, n)
		}
		count, _ := s.CountMessages(ctx, "ag")
		if count != 4 {
			t.Errorf("transaction rollback failed: alive=%d, want 4", count)
		}
	})

	t.Run("matching pivot etag truncates", func(t *testing.T) {
		n, err := s.TruncateMessagesAfterSeq(ctx, "ag", 2, "B", pivotETag)
		if err != nil {
			t.Fatalf("matching pivot: %v", err)
		}
		if n != 2 {
			t.Errorf("affected = %d, want 2", n)
		}
		count, _ := s.CountMessages(ctx, "ag")
		if count != 2 {
			t.Errorf("count after truncate = %d, want 2", count)
		}
	})

	t.Run("vanished pivot returns ErrNotFound", func(t *testing.T) {
		// Re-seed so we have a known live row, then tombstone it.
		_, err := s.AppendMessage(ctx, &MessageRecord{
			ID: "Z", AgentID: "ag", Role: "user", Content: "x",
		}, MessageInsertOptions{})
		if err != nil {
			t.Fatalf("append Z: %v", err)
		}
		if err := s.SoftDeleteMessage(ctx, "Z", ""); err != nil {
			t.Fatalf("tombstone Z: %v", err)
		}
		_, err = s.TruncateMessagesAfterSeq(ctx, "ag", 0, "Z", "anyetag")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("vanished pivot: want ErrNotFound, got %v", err)
		}
	})

	t.Run("empty pivotID skips check", func(t *testing.T) {
		// Back-compat path — daemon-internal callers (reset, lifecycle)
		// pass empty strings to opt out of the precondition.
		_, err := s.TruncateMessagesAfterSeq(ctx, "ag", 0, "", "")
		if err != nil {
			t.Fatalf("empty pivot: %v", err)
		}
	})
}

// TestTruncateForRegenerate covers the regenerate-flow's pivot-relative
// truncate: afterSeq is derived from the pivot's *immutable* seq inside
// the same transaction that validates the pivot etag, so a cross-device
// prefix delete between the click and the truncate cannot shift the
// boundary.
func TestTruncateForRegenerate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	type seeded struct {
		id   string
		etag string
		seq  int64
	}
	var rows []seeded
	for i, role := range []string{"user", "assistant", "user", "assistant", "user"} {
		rec, err := s.AppendMessage(ctx, &MessageRecord{
			ID: string(rune('A' + i)), AgentID: "ag", Role: role, Content: "x",
		}, MessageInsertOptions{})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		rows = append(rows, seeded{rec.ID, rec.ETag, rec.Seq})
	}

	t.Run("user-mode keeps pivot, kills suffix", func(t *testing.T) {
		// Click on row 2 ("C", user). KillPivot=false → keep A,B,C; kill D,E.
		err := s.TruncateForRegenerate(ctx, "ag", "C", rows[2].etag, "", "", false)
		if err != nil {
			t.Fatalf("user mode: %v", err)
		}
		count, _ := s.CountMessages(ctx, "ag")
		if count != 3 {
			t.Errorf("alive count = %d, want 3", count)
		}
		// Verify pivot still alive.
		got, err := s.GetMessage(ctx, "C")
		if err != nil || got == nil || got.DeletedAt != nil {
			t.Errorf("pivot should still be alive: %v / %+v", err, got)
		}
	})

	t.Run("assistant-mode kills pivot too", func(t *testing.T) {
		// Re-seed so we have a clean transcript for this case.
		seedAgent(t, s, "ag2")
		var pivotETag string
		for i, role := range []string{"user", "assistant", "user", "assistant"} {
			rec, _ := s.AppendMessage(ctx, &MessageRecord{
				ID: "Z" + string(rune('A'+i)), AgentID: "ag2", Role: role, Content: "x",
			}, MessageInsertOptions{})
			if rec.ID == "ZD" {
				pivotETag = rec.ETag
			}
		}
		// Click on assistant ZD. KillPivot=true → kill ZD.
		err := s.TruncateForRegenerate(ctx, "ag2", "ZD", pivotETag, "", "", true)
		if err != nil {
			t.Fatalf("assistant mode: %v", err)
		}
		count, _ := s.CountMessages(ctx, "ag2")
		if count != 3 {
			t.Errorf("alive count = %d, want 3", count)
		}
		got, _ := s.GetMessage(ctx, "ZD")
		if got != nil && got.DeletedAt == nil {
			t.Errorf("pivot ZD should be tombstoned")
		}
	})

	t.Run("stale pivot etag aborts entire TX", func(t *testing.T) {
		seedAgent(t, s, "ag3")
		var pivotID string
		for i, role := range []string{"user", "assistant", "user"} {
			rec, _ := s.AppendMessage(ctx, &MessageRecord{
				ID: "ag3_msg_" + string(rune('0'+i)), AgentID: "ag3", Role: role, Content: "x",
			}, MessageInsertOptions{})
			if rec != nil && pivotID == "" {
				pivotID = rec.ID
			}
		}
		err := s.TruncateForRegenerate(ctx, "ag3", pivotID, "v0-stale", "", "", false)
		if !errors.Is(err, ErrETagMismatch) {
			t.Fatalf("want ErrETagMismatch, got %v", err)
		}
		count, _ := s.CountMessages(ctx, "ag3")
		if count != 3 {
			t.Errorf("transaction rollback failed: alive=%d, want 3", count)
		}
	})

	t.Run("vanished pivot returns ErrNotFound", func(t *testing.T) {
		seedAgent(t, s, "ag4")
		rec, _ := s.AppendMessage(ctx, &MessageRecord{
			ID: "doomed", AgentID: "ag4", Role: "user", Content: "x",
		}, MessageInsertOptions{})
		_ = s.SoftDeleteMessage(ctx, "doomed", "")
		err := s.TruncateForRegenerate(ctx, "ag4", "doomed", rec.ETag, "", "", false)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("vanished: want ErrNotFound, got %v", err)
		}
	})

	t.Run("cross-agent pivot returns ErrNotFound (no etag oracle)", func(t *testing.T) {
		seedAgent(t, s, "ag5_a")
		seedAgent(t, s, "ag5_b")
		rec, _ := s.AppendMessage(ctx, &MessageRecord{
			ID: "ag5_pivot", AgentID: "ag5_a", Role: "user", Content: "x",
		}, MessageInsertOptions{})
		// Try to truncate ag5_b using ag5_a's pivot — must surface as
		// not-found, not as etag mismatch (a 412 oracle on the wrong
		// agent's etag space would let callers probe sibling agents).
		err := s.TruncateForRegenerate(ctx, "ag5_b", "ag5_pivot", rec.ETag, "", "", false)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("cross-agent: want ErrNotFound, got %v", err)
		}
	})

	t.Run("empty pivotETag still derives boundary", func(t *testing.T) {
		// Callers without optimistic locking still benefit from the
		// pivot-relative boundary (no cross-device shift).
		seedAgent(t, s, "ag6")
		for i, role := range []string{"user", "assistant", "user"} {
			s.AppendMessage(ctx, &MessageRecord{
				ID: "ag6_" + string(rune('0'+i)), AgentID: "ag6", Role: role, Content: "x",
			}, MessageInsertOptions{})
		}
		err := s.TruncateForRegenerate(ctx, "ag6", "ag6_0", "", "", "", false)
		if err != nil {
			t.Fatalf("empty pivot etag: %v", err)
		}
	})

	t.Run("empty pivotID is rejected", func(t *testing.T) {
		err := s.TruncateForRegenerate(ctx, "ag", "", "", "", "", false)
		if err == nil {
			t.Errorf("empty pivotID must be rejected")
		}
	})

	t.Run("stale source etag aborts entire TX", func(t *testing.T) {
		// Assistant-mode regen reads the source row outside the truncate
		// transaction. If the source is edited / tombstoned between that
		// snapshot and the truncate, sourceETag mismatches and the whole
		// TX rolls back — the suffix stays alive and the chat is never
		// re-run against stale content.
		seedAgent(t, s, "ag7")
		var srcID, pivotID, pivotETag string
		for i, role := range []string{"user", "assistant", "user"} {
			rec, _ := s.AppendMessage(ctx, &MessageRecord{
				ID: "ag7_" + string(rune('0'+i)), AgentID: "ag7", Role: role, Content: "x",
			}, MessageInsertOptions{})
			if rec == nil {
				continue
			}
			if i == 0 {
				srcID = rec.ID
			}
			if i == 1 {
				pivotID = rec.ID
				pivotETag = rec.ETag
			}
		}
		// Pass a deliberately-wrong source etag.
		err := s.TruncateForRegenerate(ctx, "ag7", pivotID, pivotETag, srcID, "v0-stale", true)
		if !errors.Is(err, ErrETagMismatch) {
			t.Fatalf("stale source etag: want ErrETagMismatch, got %v", err)
		}
		// And the suffix is still alive — the TX rolled back.
		count, _ := s.CountMessages(ctx, "ag7")
		if count != 3 {
			t.Errorf("rollback failed: alive=%d, want 3", count)
		}
	})

	t.Run("vanished source returns ErrNotFound", func(t *testing.T) {
		seedAgent(t, s, "ag8")
		var pivotID, pivotETag string
		srcRec, _ := s.AppendMessage(ctx, &MessageRecord{
			ID: "ag8_src", AgentID: "ag8", Role: "user", Content: "x",
		}, MessageInsertOptions{})
		pivot, _ := s.AppendMessage(ctx, &MessageRecord{
			ID: "ag8_piv", AgentID: "ag8", Role: "assistant", Content: "x",
		}, MessageInsertOptions{})
		pivotID = pivot.ID
		pivotETag = pivot.ETag
		// Tombstone the source.
		_ = s.SoftDeleteMessage(ctx, srcRec.ID, "")
		err := s.TruncateForRegenerate(ctx, "ag8", pivotID, pivotETag, srcRec.ID, srcRec.ETag, true)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("vanished source: want ErrNotFound, got %v", err)
		}
	})

	t.Run("cross-agent source returns ErrNotFound (no etag oracle)", func(t *testing.T) {
		seedAgent(t, s, "ag9_a")
		seedAgent(t, s, "ag9_b")
		// Source under ag9_a, pivot under ag9_b. Caller asserts agentID=ag9_b.
		srcRec, _ := s.AppendMessage(ctx, &MessageRecord{
			ID: "ag9_src", AgentID: "ag9_a", Role: "user", Content: "x",
		}, MessageInsertOptions{})
		pivot, _ := s.AppendMessage(ctx, &MessageRecord{
			ID: "ag9_piv", AgentID: "ag9_b", Role: "assistant", Content: "x",
		}, MessageInsertOptions{})
		err := s.TruncateForRegenerate(ctx, "ag9_b", pivot.ID, pivot.ETag, srcRec.ID, srcRec.ETag, true)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("cross-agent source: want ErrNotFound, got %v", err)
		}
	})

	t.Run("sourceID == pivotID with empty pivotETag still honours sourceETag", func(t *testing.T) {
		// CLI / split-precondition callers might pass only sourceETag
		// (capturing source-side staleness without pivot-side). With
		// sourceID == pivotID, naively skipping the source-distinct
		// branch would silently drop the precondition. Re-validating
		// against the pivot row's curETag closes that hole.
		seedAgent(t, s, "ag10b")
		var pivotID, pivotETag string
		for i, role := range []string{"user", "assistant"} {
			rec, _ := s.AppendMessage(ctx, &MessageRecord{
				ID: "ag10b_" + string(rune('0'+i)), AgentID: "ag10b", Role: role, Content: "x",
			}, MessageInsertOptions{})
			if i == 0 && rec != nil {
				pivotID = rec.ID
				pivotETag = rec.ETag
			}
		}
		_ = pivotETag
		// Fresh sourceETag matches → ok.
		if err := s.TruncateForRegenerate(ctx, "ag10b", pivotID, "", pivotID, pivotETag, false); err != nil {
			t.Fatalf("matching sourceETag: %v", err)
		}
	})

	t.Run("sourceID == pivotID with stale sourceETag returns ErrETagMismatch", func(t *testing.T) {
		seedAgent(t, s, "ag10c")
		var pivotID string
		for i, role := range []string{"user", "assistant"} {
			rec, _ := s.AppendMessage(ctx, &MessageRecord{
				ID: "ag10c_" + string(rune('0'+i)), AgentID: "ag10c", Role: role, Content: "x",
			}, MessageInsertOptions{})
			if i == 0 && rec != nil {
				pivotID = rec.ID
			}
		}
		err := s.TruncateForRegenerate(ctx, "ag10c", pivotID, "", pivotID, "v0-stale", false)
		if !errors.Is(err, ErrETagMismatch) {
			t.Errorf("stale sourceETag w/ empty pivotETag: want ErrETagMismatch, got %v", err)
		}
	})

	t.Run("sourceID == pivotID skips redundant source check", func(t *testing.T) {
		// User-mode regenerate passes sourceID == pivotID. The source
		// check is intentionally skipped — the pivot validation already
		// covers it. Verify no spurious failure when the caller still
		// passes both (idempotent against the no-op branch).
		seedAgent(t, s, "ag10")
		var pivotID, pivotETag string
		for i, role := range []string{"user", "assistant", "user"} {
			rec, _ := s.AppendMessage(ctx, &MessageRecord{
				ID: "ag10_" + string(rune('0'+i)), AgentID: "ag10", Role: role, Content: "x",
			}, MessageInsertOptions{})
			if i == 0 && rec != nil {
				pivotID = rec.ID
				pivotETag = rec.ETag
			}
		}
		err := s.TruncateForRegenerate(ctx, "ag10", pivotID, pivotETag, pivotID, pivotETag, false)
		if err != nil {
			t.Fatalf("user-mode source==pivot: %v", err)
		}
	})
}

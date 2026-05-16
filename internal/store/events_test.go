package store

import (
	"context"
	"testing"
)

func TestRecordEventAndListSince(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(ctx, Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	mustRecord := func(table, id, etag string, op EventOp) int64 {
		t.Helper()
		tx, err := s.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		seq, err := RecordEvent(ctx, tx, table, id, etag, op, 0)
		if err != nil {
			tx.Rollback()
			t.Fatalf("RecordEvent: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		return seq
	}

	s1 := mustRecord("agents", "ag_1", "1-aaa", EventOpInsert)
	s2 := mustRecord("agents", "ag_1", "2-bbb", EventOpUpdate)
	s3 := mustRecord("agent_messages", "msg_1", "1-ccc", EventOpInsert)
	s4 := mustRecord("agents", "ag_1", "", EventOpDelete)
	if !(s1 < s2 && s2 < s3 && s3 < s4) {
		t.Errorf("seq not monotonic: %d %d %d %d", s1, s2, s3, s4)
	}

	// All-tables, since=0 returns all 4.
	res, err := s.ListEventsSince(ctx, 0, ListEventsSinceOptions{})
	if err != nil {
		t.Fatalf("ListEventsSince: %v", err)
	}
	if len(res.Events) != 4 {
		t.Fatalf("Events count = %d want 4", len(res.Events))
	}
	if res.NextSince != s4 {
		t.Errorf("NextSince = %d want %d", res.NextSince, s4)
	}
	if res.Watermark != s1 {
		t.Errorf("Watermark = %d want %d", res.Watermark, s1)
	}
	// Delete row preserves empty etag.
	if res.Events[3].Op != EventOpDelete {
		t.Errorf("Events[3].Op = %q want delete", res.Events[3].Op)
	}
	if res.Events[3].ETag != "" {
		t.Errorf("delete event ETag = %q want empty", res.Events[3].ETag)
	}

	// Table filter narrows to agent_messages → 1 row.
	res, err = s.ListEventsSince(ctx, 0, ListEventsSinceOptions{Table: "agent_messages"})
	if err != nil {
		t.Fatalf("ListEventsSince(table): %v", err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("Events count = %d want 1", len(res.Events))
	}
	if res.Events[0].Table != "agent_messages" {
		t.Errorf("Events[0].Table = %q want agent_messages", res.Events[0].Table)
	}

	// since past last seq returns empty but holds the cursor steady.
	res, err = s.ListEventsSince(ctx, s4, ListEventsSinceOptions{})
	if err != nil {
		t.Fatalf("ListEventsSince(after): %v", err)
	}
	if len(res.Events) != 0 {
		t.Errorf("Events count = %d want 0", len(res.Events))
	}
	if res.NextSince != s4 {
		t.Errorf("NextSince = %d want %d (must not rewind)", res.NextSince, s4)
	}

	// Limit honored.
	res, err = s.ListEventsSince(ctx, 0, ListEventsSinceOptions{Limit: 2})
	if err != nil {
		t.Fatalf("ListEventsSince(limit): %v", err)
	}
	if len(res.Events) != 2 {
		t.Fatalf("limit batch size = %d want 2", len(res.Events))
	}
	if res.NextSince != s2 {
		t.Errorf("NextSince after limited batch = %d want %d", res.NextSince, s2)
	}
}

func TestRecordEventValidatesOp(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(ctx, Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	if _, err := RecordEvent(ctx, tx, "agents", "ag_1", "1-x", "bogus", 0); err == nil {
		t.Error("invalid op accepted")
	}
	if _, err := RecordEvent(ctx, tx, "", "ag_1", "1-x", EventOpInsert, 0); err == nil {
		t.Error("empty table accepted")
	}
	if _, err := RecordEvent(ctx, tx, "agents", "", "1-x", EventOpInsert, 0); err == nil {
		t.Error("empty id accepted")
	}
	if _, err := RecordEvent(context.Background(), nil, "agents", "ag_1", "1-x", EventOpInsert, 0); err == nil {
		t.Error("nil tx accepted")
	}
}

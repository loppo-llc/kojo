package store

import (
	"context"
	"errors"
	"testing"
)

func TestBulkInsertSessions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	agentID := "ag"
	pid := int64(12345)
	exit := int64(0)
	now := NowMillis()
	recs := []*SessionRecord{
		{ID: "s1", AgentID: &agentID, Status: "archived", PID: &pid, Cmd: "claude", WorkDir: "/home/u/p1", StartedAt: &now, StoppedAt: &now, ExitCode: &exit, CreatedAt: now},
		{ID: "s2", AgentID: nil, Status: "archived", Cmd: "codex", WorkDir: "/home/u/p2", CreatedAt: now},
	}
	n, err := s.BulkInsertSessions(ctx, recs, SessionInsertOptions{PeerID: "peer-1"})
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if n != 2 {
		t.Errorf("inserted = %d, want 2", n)
	}
	// seq comes from NextGlobalSeq() (a wallclock-seeded monotonic
	// counter) so we can't assert specific values, only ordering.
	if recs[0].Seq <= 0 || recs[1].Seq <= recs[0].Seq {
		t.Errorf("seq not strictly monotonic: (%d, %d)", recs[0].Seq, recs[1].Seq)
	}
	for _, r := range recs {
		if r.ETag == "" {
			t.Errorf("etag not stamped: %+v", r)
		}
		if r.PeerID != "peer-1" {
			t.Errorf("peer_id = %q, want peer-1", r.PeerID)
		}
	}

	// GetSession round-trip.
	got, err := s.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AgentID == nil || *got.AgentID != "ag" {
		t.Errorf("agent_id = %v, want &ag", got.AgentID)
	}
	if got.Status != "archived" {
		t.Errorf("status = %q, want archived", got.Status)
	}
	if got.PID == nil || *got.PID != 12345 {
		t.Errorf("pid = %v, want &12345", got.PID)
	}
	if got.Cmd != "claude" {
		t.Errorf("cmd = %q", got.Cmd)
	}

	// GetSession on detached row.
	got2, err := s.GetSession(ctx, "s2")
	if err != nil {
		t.Fatalf("get s2: %v", err)
	}
	if got2.AgentID != nil {
		t.Errorf("agent_id should be nil for detached, got %v", got2.AgentID)
	}

	// ListSessionsByAgent
	list, err := s.ListSessionsByAgent(ctx, "ag")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != "s1" {
		t.Errorf("list ag: %+v", list)
	}

	// Detached listing (agent_id IS NULL)
	detached, err := s.ListSessionsByAgent(ctx, "")
	if err != nil {
		t.Fatalf("list detached: %v", err)
	}
	if len(detached) != 1 || detached[0].ID != "s2" {
		t.Errorf("list detached: %+v", detached)
	}

	// Re-running silently skips (preload-hit).
	n2, err := s.BulkInsertSessions(ctx, recs, SessionInsertOptions{PeerID: "peer-1"})
	if err != nil {
		t.Fatalf("bulk re-run: %v", err)
	}
	if n2 != 0 {
		t.Errorf("idempotent re-run inserted = %d, want 0", n2)
	}

	// Mixed: some new, some duplicate.
	mix := []*SessionRecord{
		{ID: "s1", AgentID: &agentID, Status: "archived", Cmd: "claude", WorkDir: "/x"},
		{ID: "s3", AgentID: &agentID, Status: "archived", Cmd: "gemini", WorkDir: "/y"},
	}
	n3, err := s.BulkInsertSessions(ctx, mix, SessionInsertOptions{PeerID: "peer-1"})
	if err != nil {
		t.Fatalf("bulk mixed: %v", err)
	}
	if n3 != 1 {
		t.Errorf("mixed inserted = %d, want 1", n3)
	}
	// mix[1] is the new row; its seq should be strictly greater than
	// the prior batch's last seq (global allocator never goes backward).
	if mix[1].Seq <= recs[1].Seq {
		t.Errorf("new row seq = %d, want > %d (global allocator monotonic)", mix[1].Seq, recs[1].Seq)
	}
	// Skipped record (mix[0]) should stay untouched.
	if mix[0].ETag != "" {
		t.Errorf("skipped record was mutated: %+v", mix[0])
	}
}

func TestBulkInsertSessionsRejectsInvalidStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.BulkInsertSessions(ctx, []*SessionRecord{
		{ID: "x", Status: "running-but-typoed", Cmd: "x", WorkDir: "/x"},
	}, SessionInsertOptions{}); err == nil {
		t.Fatal("expected invalid-status rejection")
	}
}

func TestGetSessionNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetSession(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestBulkInsertSessionsInBatchDuplicate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	recs := []*SessionRecord{
		{ID: "dup", Status: "archived", Cmd: "first", WorkDir: "/a"},
		{ID: "dup", Status: "archived", Cmd: "second", WorkDir: "/b"},
	}
	n, err := s.BulkInsertSessions(ctx, recs, SessionInsertOptions{})
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if n != 1 {
		t.Errorf("inserted = %d, want 1 (first-write-wins on dup)", n)
	}
	got, err := s.GetSession(ctx, "dup")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Cmd != "first" {
		t.Errorf("cmd = %q, want first (first-write-wins)", got.Cmd)
	}
}

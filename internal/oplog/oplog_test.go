package oplog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func mkEntry(t *testing.T, opID string, ts int64, token int64) *Entry {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"k": "v", "op": opID})
	return &Entry{
		OpID:         opID,
		AgentID:      "ag_1",
		FencingToken: token,
		Seq:          ts,
		Table:        "agent_messages",
		Op:           "insert",
		Body:         body,
		ClientTS:     ts,
	}
}

func openLog(t *testing.T, lim Limits) *Log {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ag_1")
	l, err := Open(dir, lim)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestAppendValidationRejectsBadEntries(t *testing.T) {
	l := openLog(t, Limits{})
	cases := []struct {
		name string
		ent  *Entry
	}{
		{"nil", nil},
		{"missing op_id", &Entry{AgentID: "a", FencingToken: 1, ClientTS: 1}},
		{"missing agent_id", &Entry{OpID: "o", FencingToken: 1, ClientTS: 1}},
		{"zero token", &Entry{OpID: "o", AgentID: "a", FencingToken: 0, ClientTS: 1}},
		{"zero ts", &Entry{OpID: "o", AgentID: "a", FencingToken: 1, ClientTS: 0}},
	}
	for _, c := range cases {
		if _, err := l.Append(context.Background(), c.ent); err == nil {
			t.Errorf("%s: expected error", c.name)
		}
	}
}

func TestAppendAndDrainRoundTrip(t *testing.T) {
	l := openLog(t, Limits{})
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if _, err := l.Append(ctx, mkEntry(t, "op-"+string(rune('a'+i-1)), int64(1000+i), 1)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	pend, err := l.Pending()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if pend.Entries != 3 {
		t.Errorf("entries = %d, want 3", pend.Entries)
	}
	if pend.OldestTS != 1001 {
		t.Errorf("oldest_ts = %d, want 1001", pend.OldestTS)
	}

	var seen []string
	if err := l.Drain(ctx, func(_ context.Context, e *Entry) error {
		seen = append(seen, e.OpID)
		return nil
	}); err != nil {
		t.Fatalf("drain: %v", err)
	}
	want := []string{"op-a", "op-b", "op-c"}
	if len(seen) != 3 {
		t.Fatalf("seen = %v", seen)
	}
	for i, w := range want {
		if seen[i] != w {
			t.Errorf("seen[%d] = %q, want %q", i, seen[i], w)
		}
	}

	// After drain the dir should be empty (current.jsonl removed).
	pend, _ = l.Pending()
	if pend.Entries != 0 {
		t.Errorf("post-drain entries = %d, want 0", pend.Entries)
	}
}

func TestAppendRotatesOnEntryThreshold(t *testing.T) {
	l := openLog(t, Limits{MaxEntries: 2, MaxBytes: 1 << 30, MaxAgeMillis: 1 << 30, MaxQueuedRotated: 100})
	ctx := context.Background()
	if _, err := l.Append(ctx, mkEntry(t, "op-a", 1000, 1)); err != nil {
		t.Fatal(err)
	}
	// Second append fills cap → triggers rotate after the write so
	// the entry that crossed the threshold sits in the rotated file.
	rotPath, err := l.Append(ctx, mkEntry(t, "op-b", 1001, 1))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(rotPath) != "rotated.0.jsonl" {
		t.Errorf("rotated path = %s", rotPath)
	}
	if _, err := os.Stat(rotPath); err != nil {
		t.Errorf("rotated file missing: %v", err)
	}
	// current.jsonl should be gone (rename completed).
	if _, err := os.Stat(filepath.Join(l.dir, "current.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("current still present after rotate: %v", err)
	}
	// A third append re-creates current.jsonl alongside the rotated.
	if _, err := l.Append(ctx, mkEntry(t, "op-c", 1002, 1)); err != nil {
		t.Fatal(err)
	}
	pend, _ := l.Pending()
	if pend.Entries != 3 {
		t.Errorf("post-rotate entries = %d, want 3", pend.Entries)
	}
}

func TestDrainOrderRotatedThenCurrent(t *testing.T) {
	l := openLog(t, Limits{MaxEntries: 2, MaxQueuedRotated: 100})
	ctx := context.Background()
	// Two entries hit the rotate threshold (rotated.0).
	for i, op := range []string{"op-a", "op-b"} {
		if _, err := l.Append(ctx, mkEntry(t, op, int64(1000+i), 1)); err != nil {
			t.Fatal(err)
		}
	}
	// One more entry stays in current.
	if _, err := l.Append(ctx, mkEntry(t, "op-c", 1100, 1)); err != nil {
		t.Fatal(err)
	}
	var order []string
	if err := l.Drain(ctx, func(_ context.Context, e *Entry) error {
		order = append(order, e.OpID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"op-a", "op-b", "op-c"}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("drain order[%d] = %q, want %q", i, order[i], want[i])
		}
	}
}

func TestDrainStopsOnVisitorErrorAndPreservesFile(t *testing.T) {
	l := openLog(t, Limits{MaxEntries: 2, MaxQueuedRotated: 100})
	ctx := context.Background()
	for i, op := range []string{"op-a", "op-b", "op-c", "op-d"} {
		if _, err := l.Append(ctx, mkEntry(t, op, int64(1000+i), 1)); err != nil {
			t.Fatal(err)
		}
	}
	// Visit until op-c, then fail. rotated.0 (op-a/op-b) drains
	// successfully and is removed; rotated.1 (op-c/op-d) keeps the
	// failed entry plus its trailer.
	stopAt := "op-c"
	stopErr := errors.New("hub-down")
	calls := 0
	err := l.Drain(ctx, func(_ context.Context, e *Entry) error {
		calls++
		if e.OpID == stopAt {
			return stopErr
		}
		return nil
	})
	if !errors.Is(err, stopErr) {
		t.Fatalf("drain err: got %v want %v", err, stopErr)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (a, b, c)", calls)
	}
	// rotated.0 should be gone, rotated.1 should remain.
	if _, e := os.Stat(filepath.Join(l.dir, "rotated.0.jsonl")); !errors.Is(e, os.ErrNotExist) {
		t.Errorf("rotated.0 still present: %v", e)
	}
	if _, e := os.Stat(filepath.Join(l.dir, "rotated.1.jsonl")); e != nil {
		t.Errorf("rotated.1 missing: %v", e)
	}
	// Pending reflects the un-flushed remainder.
	pend, _ := l.Pending()
	if pend.Entries != 2 {
		t.Errorf("post-fail pending = %d, want 2", pend.Entries)
	}
}

func TestTruncateRemovesEverything(t *testing.T) {
	l := openLog(t, Limits{MaxEntries: 2, MaxQueuedRotated: 100})
	ctx := context.Background()
	for i, op := range []string{"op-a", "op-b", "op-c"} {
		if _, err := l.Append(ctx, mkEntry(t, op, int64(1000+i), 1)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := l.Truncate()
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if n != 2 {
		t.Errorf("removed = %d, want 2 (rotated.0 + current)", n)
	}
	pend, _ := l.Pending()
	if pend.Entries != 0 {
		t.Errorf("post-truncate entries = %d, want 0", pend.Entries)
	}
}

func TestOpenResumesRotateSeqFromExisting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ag_1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Lay down rotated.5 manually so Open's seed picks it up.
	if err := os.WriteFile(filepath.Join(dir, "rotated.5.jsonl"), []byte(`{"op_id":"x","agent_id":"a","fencing_token":1,"client_ts":1}` + "\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Open(dir, Limits{MaxEntries: 1, MaxQueuedRotated: 100})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	// First rotate after one append should create rotated.6.jsonl,
	// not rotated.0.
	rotPath, err := l.Append(context.Background(), mkEntry(t, "op-a", 1000, 1))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(rotPath) != "rotated.6.jsonl" {
		t.Errorf("rotated path = %s, want rotated.6.jsonl", rotPath)
	}
}

func TestAppendRefusesAfterMaxQueuedRotated(t *testing.T) {
	// MaxEntries=1 → every Append rotates. MaxQueuedRotated=2 →
	// after two rotations the third Append must refuse with
	// ErrLimitExceeded so the agent runtime can stop the session.
	l := openLog(t, Limits{MaxEntries: 1, MaxQueuedRotated: 2})
	ctx := context.Background()
	for i, op := range []string{"op-a", "op-b"} {
		if _, err := l.Append(ctx, mkEntry(t, op, int64(1000+i), 1)); err != nil {
			t.Fatalf("append %s: %v", op, err)
		}
	}
	// Backlog now: rotated.0 + rotated.1 = 2 files. Third Append
	// trips the cap *before* opening current.jsonl.
	_, err := l.Append(ctx, mkEntry(t, "op-c", 1100, 1))
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("want ErrLimitExceeded, got %v", err)
	}
	// And subsequent Append calls keep returning the same error
	// until Drain or Truncate clears the backlog.
	if _, err := l.Append(ctx, mkEntry(t, "op-d", 1101, 1)); !errors.Is(err, ErrLimitExceeded) {
		t.Errorf("post-cap append: %v", err)
	}
	// After Drain (no-op visitor) the cap is cleared and Append works again.
	if err := l.Drain(ctx, func(context.Context, *Entry) error { return nil }); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, err := l.Append(ctx, mkEntry(t, "op-e", 1200, 1)); err != nil {
		t.Errorf("post-drain append: %v", err)
	}
}

func TestDrainRotatesPendingCurrentBeforeReleaseLock(t *testing.T) {
	// Regression for the unlock-then-EOF race. Append a single
	// entry into current.jsonl (no rotate), then Drain — the
	// expectation is current.jsonl is rotated under-lock, drained,
	// and removed; the entry must be visited exactly once.
	l := openLog(t, Limits{MaxEntries: 100})
	ctx := context.Background()
	if _, err := l.Append(ctx, mkEntry(t, "op-a", 1000, 1)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := l.Drain(ctx, func(_ context.Context, e *Entry) error {
		if e.OpID != "op-a" {
			t.Errorf("op_id: %s", e.OpID)
		}
		calls++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	// And current.jsonl is gone.
	if _, err := os.Stat(filepath.Join(l.dir, "current.jsonl")); !errors.Is(err, os.ErrNotExist) {
		// Note: it's also acceptable for current.jsonl to be present
		// but empty (a fresh-empty file from the next Append). In our
		// test there's no follow-up Append, so the rotated-then-
		// drained file should be the only artefact and it's gone.
		t.Errorf("current.jsonl unexpectedly present: %v", err)
	}
}

func TestOpenLoadsCurrentStats(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ag_1")
	l, err := Open(dir, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for i, op := range []string{"op-a", "op-b"} {
		if _, err := l.Append(context.Background(),
			mkEntry(t, op, int64(1000+i), 1)); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	// Reopen and Pending should still see both entries.
	l2, err := Open(dir, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l2.Close() })
	pend, err := l2.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if pend.Entries != 2 {
		t.Errorf("pending after reopen = %d, want 2", pend.Entries)
	}
}

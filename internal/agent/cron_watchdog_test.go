package agent

import (
	"sort"
	"testing"
	"time"
)

func TestReArmStale(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	threshold := 30 * time.Second

	t.Run("entry with future Next is not stale", func(t *testing.T) {
		got := reArmStale([]entrySnapshot{
			{agentID: "ag_a", next: now.Add(5 * time.Minute), expr: "*/5 * * * *"},
		}, now, threshold)
		if len(got) != 0 {
			t.Errorf("expected no re-arm, got %v", got)
		}
	})

	t.Run("entry inside grace window is not stale", func(t *testing.T) {
		got := reArmStale([]entrySnapshot{
			{agentID: "ag_a", next: now.Add(-10 * time.Second), expr: "*/5 * * * *"},
		}, now, threshold)
		if len(got) != 0 {
			t.Errorf("expected no re-arm within grace, got %v", got)
		}
	})

	t.Run("entry past threshold is re-armed", func(t *testing.T) {
		got := reArmStale([]entrySnapshot{
			{agentID: "ag_a", next: now.Add(-2 * time.Hour), expr: "*/5 * * * *"},
		}, now, threshold)
		if len(got) != 1 || got[0] != "ag_a" {
			t.Errorf("expected [ag_a], got %v", got)
		}
	})

	t.Run("zero Next is skipped (entry is brand new)", func(t *testing.T) {
		got := reArmStale([]entrySnapshot{
			{agentID: "ag_a", next: time.Time{}, expr: "*/5 * * * *"},
		}, now, threshold)
		if len(got) != 0 {
			t.Errorf("expected zero-Next entry to be skipped, got %v", got)
		}
	})

	t.Run("multiple entries — only the stale ones come back", func(t *testing.T) {
		got := reArmStale([]entrySnapshot{
			{agentID: "ag_fresh", next: now.Add(time.Minute), expr: "*/5 * * * *"},
			{agentID: "ag_stale1", next: now.Add(-time.Hour), expr: "0 * * * *"},
			{agentID: "ag_stale2", next: now.Add(-25 * time.Hour), expr: "0 9 * * *"},
		}, now, threshold)
		sort.Strings(got)
		want := []string{"ag_stale1", "ag_stale2"}
		if len(got) != len(want) {
			t.Fatalf("expected %v, got %v", want, got)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("entry %d: got %q, want %q", i, got[i], want[i])
			}
		}
	})
}

// TestCronScheduler_ReArmStaleEntries exercises the live re-arm path: spin
// up the cron goroutine so Next gets populated, then feed reArmStaleEntries
// a "now" far enough in the future that the recorded Next is well past the
// stale threshold. Verifies the entry table contains a fresh EntryID
// afterwards — robfig/cron only recomputes Next when Schedule is called.
func TestCronScheduler_ReArmStaleEntries(t *testing.T) {
	cs := newCronScheduler(nil, testLogger())
	cs.c.Start()
	defer func() {
		ctx := cs.c.Stop()
		<-ctx.Done()
	}()

	// Schedule a once-a-year-ish expression so the timer never fires during
	// the test (the cron's job func dereferences cs.mgr which is nil here).
	const expr = "0 0 1 1 *" // Jan 1 00:00 yearly
	if err := cs.Schedule("ag_test", expr); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	cs.mu.Lock()
	originalID := cs.entries["ag_test"].id
	cs.mu.Unlock()

	// robfig/cron computes Next asynchronously when running — wait briefly
	// for the entry's Next to be populated before pretending a sleep skip.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cs.mu.Lock()
		entry := cs.c.Entry(originalID)
		cs.mu.Unlock()
		if !entry.Next.IsZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Pretend wall clock jumped 5 years forward — the recorded Next is now
	// well past the watchdog's stale threshold.
	rearmed := cs.reArmStaleEntries(time.Now().AddDate(5, 0, 0))
	if len(rearmed) != 1 || rearmed[0] != "ag_test" {
		t.Fatalf("expected ag_test to be re-armed, got %v", rearmed)
	}

	cs.mu.Lock()
	newID := cs.entries["ag_test"].id
	newExpr := cs.entries["ag_test"].expr
	cs.mu.Unlock()
	if newID == originalID {
		t.Errorf("expected new EntryID after re-arm, still %v", newID)
	}
	if newExpr != expr {
		t.Errorf("expected expression preserved, got %q", newExpr)
	}
}

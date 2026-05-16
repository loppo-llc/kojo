package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
)

// newPollerWithStore builds a notifyPoller wired to a real store via a
// minimal Manager, suitable for testing the DB cutover paths
// (purgeKeysLocked orphan-diff, RemoveAgent bulk wipe). The Manager
// is not Started — these tests drive the poller's exported and
// unexported entry points directly.
func newPollerWithStore(t *testing.T) (*notifyPoller, *Manager) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("APPDATA", "")

	logger := testLogger()
	st, err := newStore(logger)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := &Manager{
		agents:     make(map[string]*Agent),
		backends:   make(map[string]ChatBackend),
		store:      st,
		logger:     logger,
		busy:       make(map[string]busyEntry),
		resetting:  make(map[string]bool),
		editing:    make(map[string]bool),
		profileGen: make(map[string]bool),
		memIndexes: make(map[string]*MemoryIndex),
		patchMus:   make(map[string]*sync.Mutex),
	}
	p := newNotifyPoller(mgr, logger)
	return p, mgr
}

// TestPurgeKeysLocked_OrphanDiff verifies that purgeKeysLocked's pass-2
// list-and-diff sweeps DB rows that were never loaded into memory —
// the failure mode that motivated round-2 finding #5. Seeds the DB
// with cursors for an agent, populates p.cursors with only ONE of
// them, and calls purgeKeysLocked with a keepKeys set that excludes
// every cursor. Both the in-memory entry's row AND the DB-only row
// must be deleted.
func TestPurgeKeysLocked_OrphanDiff(t *testing.T) {
	p, mgr := newPollerWithStore(t)
	st := mgr.Store()
	ctx := context.Background()
	agentID := "ag"

	// FK requires the agent to exist before its cursors.
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{
		ID: agentID, Name: "Ag",
	}, store.AgentInsertOptions{}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// Seed DB with two cursors. One will be tracked in memory, the other
	// will be DB-only — simulating "reload skipped this row" or "agent
	// archived → cursors never loaded → config now removes the source".
	tracked := &store.NotifyCursorRecord{
		ID: agentID + ":slack:Ctracked", Source: "slack",
		AgentID: &agentID, Cursor: "tracked-cursor",
	}
	dbOnly := &store.NotifyCursorRecord{
		ID: agentID + ":slack:CdbOnly", Source: "slack",
		AgentID: &agentID, Cursor: "db-only-cursor",
	}
	if err := st.UpsertNotifyCursor(ctx, tracked, store.NotifyCursorInsertOptions{}); err != nil {
		t.Fatalf("seed tracked: %v", err)
	}
	if err := st.UpsertNotifyCursor(ctx, dbOnly, store.NotifyCursorInsertOptions{}); err != nil {
		t.Fatalf("seed db-only: %v", err)
	}

	// Populate ONLY the tracked cursor in memory (mirrors what a
	// successful reloadCursorsForAgentLocked would have done for one
	// of two configured sources, with the other being missed).
	p.mu.Lock()
	p.cursors[agentID+":Ctracked"] = "tracked-cursor"
	p.sourceTypes[agentID+":Ctracked"] = "slack"

	// Call purgeKeysLocked with an empty keepKeys — both keys are
	// "no longer active". The tracked entry gets the per-row delete
	// path (pass 1); the DB-only entry gets the list-diff path (pass 2).
	p.purgeKeysLocked(agentID, map[string]bool{})
	p.mu.Unlock()

	// Both DB rows must be gone.
	for _, id := range []string{tracked.ID, dbOnly.ID} {
		if _, err := st.GetNotifyCursor(ctx, id); err == nil {
			t.Errorf("%s: still present in DB after orphan-diff purge", id)
		}
	}
	// In-memory state for the agent must be empty.
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.cursors) != 0 {
		t.Errorf("p.cursors not empty after purge: %+v", p.cursors)
	}
	if len(p.sourceTypes) != 0 {
		t.Errorf("p.sourceTypes not empty after purge: %+v", p.sourceTypes)
	}
}

// TestPurgeKeysLocked_NilKeepKeys_BulkSweep verifies the keepKeys==nil
// branch routes through DeleteNotifyCursorsByAgent — addresses round-2
// finding #1 (full-agent purge previously skipped DB rows that were
// not in memory).
func TestPurgeKeysLocked_NilKeepKeys_BulkSweep(t *testing.T) {
	p, mgr := newPollerWithStore(t)
	st := mgr.Store()
	ctx := context.Background()
	agentID := "ag"

	if _, err := st.InsertAgent(ctx, &store.AgentRecord{
		ID: agentID, Name: "Ag",
	}, store.AgentInsertOptions{}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// Seed two DB rows. None tracked in memory — simulates archived
	// agent's cursors that were never reloaded.
	for _, id := range []string{"CdbOne", "CdbTwo"} {
		rec := &store.NotifyCursorRecord{
			ID: agentID + ":slack:" + id, Source: "slack",
			AgentID: &agentID, Cursor: "cursor-" + id,
		}
		if err := st.UpsertNotifyCursor(ctx, rec, store.NotifyCursorInsertOptions{}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	p.mu.Lock()
	p.purgeKeysLocked(agentID, nil) // nil = full purge
	p.mu.Unlock()

	// Both rows must be gone via the bulk-delete branch.
	for _, id := range []string{agentID + ":slack:CdbOne", agentID + ":slack:CdbTwo"} {
		if _, err := st.GetNotifyCursor(ctx, id); err == nil {
			t.Errorf("%s: still present after nil-keepKeys full purge", id)
		}
	}
}

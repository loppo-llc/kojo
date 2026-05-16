package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGetAgentSyncState_UnknownAgent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	st, err := s.GetAgentSyncState(ctx, "ag_nonexistent")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st.Known {
		t.Fatalf("Known=true for missing agent; want false")
	}
	if st.MaxMessageSeq != 0 || st.MaxMemoryEntrySeq != 0 {
		t.Fatalf("seqs non-zero for missing agent: %+v", st)
	}
}

func TestGetAgentSyncState_KnownEmpty(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")
	st, err := s.GetAgentSyncState(ctx, "ag")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !st.Known {
		t.Fatalf("Known=false; want true after InsertAgent")
	}
	if st.AgentETag == "" {
		t.Fatalf("AgentETag empty for seeded agent")
	}
	if st.MaxMessageSeq != 0 {
		t.Fatalf("MaxMessageSeq = %d; want 0 (no messages yet)", st.MaxMessageSeq)
	}
}

func TestGetAgentSyncState_TracksMaxMessageSeq(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")
	for i, role := range []string{"user", "assistant", "user"} {
		if _, err := s.AppendMessage(ctx, &MessageRecord{
			ID: "m" + string(rune('a'+i)), AgentID: "ag", Role: role,
			Content: "hi",
		}, MessageInsertOptions{}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	st, err := s.GetAgentSyncState(ctx, "ag")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if st.MaxMessageSeq != 3 {
		t.Fatalf("MaxMessageSeq = %d; want 3", st.MaxMessageSeq)
	}
}

func TestSyncAgentFromPeer_IncrementalMessagesUpsertsDelta(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")
	// Pre-existing rows on "target" (this DB) — seq 1, 2.
	for i, role := range []string{"user", "assistant"} {
		if _, err := s.AppendMessage(ctx, &MessageRecord{
			ID: "m" + string(rune('a'+i)), AgentID: "ag", Role: role,
			Content: "old-" + string(rune('a'+i)),
		}, MessageInsertOptions{}); err != nil {
			t.Fatalf("seed msg: %v", err)
		}
	}
	// Incremental sync ships only seq=3.
	delta := []*MessageRecord{{
		ID: "mc", AgentID: "ag", Seq: 3, Role: "user", Content: "new",
		Version: 1, ETag: "etag-mc", CreatedAt: 1000, UpdatedAt: 1000,
	}}
	agent, err := s.GetAgent(ctx, "ag")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if err := s.SyncAgentFromPeer(ctx, AgentSyncPayload{
		Agent:               agent,
		Messages:            delta,
		IncrementalMessages: true,
	}); err != nil {
		t.Fatalf("sync incremental: %v", err)
	}
	list, err := s.ListMessages(ctx, "ag", MessageListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("len = %d; want 3 (pre-existing 2 + delta 1)", len(list))
	}
	if list[0].Content != "old-a" || list[1].Content != "old-b" || list[2].Content != "new" {
		t.Fatalf("rows mutated unexpectedly: %+v", list)
	}
}

func TestSyncAgentFromPeer_IncrementalMemoryEntriesHandlesTombstoneAndRecreation(t *testing.T) {
	// Target-side scenario: target has alive memory entry me1
	// (kind=topic, name=go). Source has soft-deleted me1 and
	// inserted me2 with the same (kind,name). Incremental sync
	// must ship both rows in updated_at ASC order so me1's
	// tombstone clears its alive UNIQUE slot before me2's
	// INSERT lands. After sync target should have me1 deleted
	// and me2 alive.
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")
	me1, err := s.InsertMemoryEntry(ctx, &MemoryEntryRecord{
		ID: "me1", AgentID: "ag", Kind: "topic", Name: "go", Body: "old",
	}, MemoryEntryInsertOptions{})
	if err != nil {
		t.Fatalf("seed me1: %v", err)
	}
	// Simulate source's delta: me1 tombstoned (with deleted_at),
	// then me2 fresh insert reusing the (topic, go) slot. Both
	// stamped with explicit updated_at so ASC order is
	// deterministic in the test.
	t1 := me1.UpdatedAt + 1000
	t2 := t1 + 1000
	tomb := int64(t1)
	delta := []*MemoryEntryRecord{
		{
			ID: "me1", AgentID: "ag", Seq: me1.Seq, Kind: "topic", Name: "go",
			Body: "old", BodySHA256: me1.BodySHA256,
			Version: 2, ETag: "etag-me1-deleted",
			CreatedAt: me1.CreatedAt, UpdatedAt: t1, DeletedAt: &tomb,
		},
		{
			ID: "me2", AgentID: "ag", Seq: me1.Seq + 1, Kind: "topic", Name: "go",
			Body: "new", BodySHA256: "sha-new",
			Version: 1, ETag: "etag-me2",
			CreatedAt: t2, UpdatedAt: t2,
		},
	}
	agent, err := s.GetAgent(ctx, "ag")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if err := s.SyncAgentFromPeer(ctx, AgentSyncPayload{
		Agent:                    agent,
		MemoryEntries:            delta,
		IncrementalMemoryEntries: true,
	}); err != nil {
		t.Fatalf("incremental sync: %v", err)
	}
	// me1 must be tombstoned, me2 alive.
	if _, err := s.GetMemoryEntry(ctx, "me1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("me1 not tombstoned after sync: %v", err)
	}
	me2, err := s.GetMemoryEntry(ctx, "me2")
	if err != nil {
		t.Fatalf("get me2: %v", err)
	}
	if me2.Body != "new" {
		t.Errorf("me2 body = %q, want 'new'", me2.Body)
	}
	// Alive UNIQUE on (kind, name) must allow another fresh
	// row to be inserted later (sanity check that me1's
	// tombstone really freed the slot).
	if _, err := s.InsertMemoryEntry(ctx, &MemoryEntryRecord{
		ID: "me3", AgentID: "ag", Kind: "topic", Name: "elsewhere", Body: "x",
	}, MemoryEntryInsertOptions{}); err != nil {
		t.Fatalf("post-sync insert: %v", err)
	}
}

func TestGetAgentSyncState_MemoryEntryUpdatedAtTracksTombstone(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")
	me, err := s.InsertMemoryEntry(ctx, &MemoryEntryRecord{
		ID: "me", AgentID: "ag", Kind: "topic", Name: "go", Body: "x",
	}, MemoryEntryInsertOptions{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	stateBefore, err := s.GetAgentSyncState(ctx, "ag")
	if err != nil {
		t.Fatalf("state before: %v", err)
	}
	if stateBefore.MaxMemoryEntryUpdatedAt != me.UpdatedAt {
		t.Errorf("MaxMemoryEntryUpdatedAt = %d; want %d", stateBefore.MaxMemoryEntryUpdatedAt, me.UpdatedAt)
	}
	// SoftDelete uses NowMillis(); without a small sleep the
	// test's wall-clock granularity is too coarse to distinguish
	// the soft-delete moment from the insert. 2 ms is generous
	// for production callers but cheap in test runtime.
	time.Sleep(2 * time.Millisecond)
	if err := s.SoftDeleteMemoryEntry(ctx, "me", ""); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	stateAfter, err := s.GetAgentSyncState(ctx, "ag")
	if err != nil {
		t.Fatalf("state after: %v", err)
	}
	if stateAfter.MaxMemoryEntryUpdatedAt <= stateBefore.MaxMemoryEntryUpdatedAt {
		t.Errorf("MaxMemoryEntryUpdatedAt did not bump on soft delete: before=%d after=%d",
			stateBefore.MaxMemoryEntryUpdatedAt, stateAfter.MaxMemoryEntryUpdatedAt)
	}
}

func TestSyncAgentFromPeer_FullModeReplacesAll(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")
	for i, role := range []string{"user", "assistant"} {
		if _, err := s.AppendMessage(ctx, &MessageRecord{
			ID: "m" + string(rune('a'+i)), AgentID: "ag", Role: role,
			Content: "old",
		}, MessageInsertOptions{}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	full := []*MessageRecord{{
		ID: "new1", AgentID: "ag", Seq: 1, Role: "user", Content: "rebuilt",
		Version: 1, ETag: "etag-new1", CreatedAt: 1000, UpdatedAt: 1000,
	}}
	agent, err := s.GetAgent(ctx, "ag")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if err := s.SyncAgentFromPeer(ctx, AgentSyncPayload{
		Agent:               agent,
		Messages:            full,
		IncrementalMessages: false,
	}); err != nil {
		t.Fatalf("sync full: %v", err)
	}
	list, err := s.ListMessages(ctx, "ag", MessageListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != "new1" {
		t.Fatalf("full mode did not replace: %+v", list)
	}
}

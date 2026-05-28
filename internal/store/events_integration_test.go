package store

import (
	"context"
	"sync"
	"testing"
)

func TestSetEventListenerFiresAfterCommit(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(ctx, Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var (
		mu   sync.Mutex
		seen []EventRecord
	)
	s.SetEventListener(func(e EventRecord) {
		mu.Lock()
		seen = append(seen, e)
		mu.Unlock()
	})

	a, err := s.InsertAgent(ctx, &AgentRecord{ID: "ag_l", Name: "Listener"}, AgentInsertOptions{})
	if err != nil {
		t.Fatalf("InsertAgent: %v", err)
	}
	if _, err := s.UpdateAgent(ctx, a.ID, a.ETag, func(r *AgentRecord) error {
		r.Name = "Listener2"
		return nil
	}); err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}
	if err := s.SoftDeleteAgent(ctx, a.ID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("listener calls = %d, want 3 (insert+update+delete)", len(seen))
	}
	wantOps := []EventOp{EventOpInsert, EventOpUpdate, EventOpDelete}
	for i, e := range seen {
		if e.Op != wantOps[i] {
			t.Errorf("seen[%d].Op = %q want %q", i, e.Op, wantOps[i])
		}
		if e.ID != "ag_l" {
			t.Errorf("seen[%d].ID = %q want ag_l", i, e.ID)
		}
		if e.Seq <= 0 {
			t.Errorf("seen[%d].Seq = %d want >0", i, e.Seq)
		}
	}
	// Listener can be replaced / cleared.
	s.SetEventListener(nil)
	if _, err := s.InsertAgent(ctx, &AgentRecord{ID: "ag_l2", Name: "Other"}, AgentInsertOptions{}); err != nil {
		t.Fatalf("InsertAgent post-clear: %v", err)
	}
	if got := len(seen); got != 3 {
		t.Errorf("listener still firing after nil-clear: count=%d", got)
	}
}

// TestInsertAgentEmitsEvent asserts that the InsertAgent / UpdateAgent /
// SoftDeleteAgent paths each append a corresponding row to the events
// table, atomically with the domain mutation. Without this, a peer
// reading /api/v1/changes?since=<seq> would miss writes that happened
// while it was offline.
func TestInsertAgentEmitsEvent(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(ctx, Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	agent, err := s.InsertAgent(ctx, &AgentRecord{
		ID:   "ag_1",
		Name: "Alice",
	}, AgentInsertOptions{})
	if err != nil {
		t.Fatalf("InsertAgent: %v", err)
	}

	res, err := s.ListEventsSince(ctx, 0, ListEventsSinceOptions{Table: "agents"})
	if err != nil {
		t.Fatalf("ListEventsSince: %v", err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("events after Insert = %d, want 1", len(res.Events))
	}
	got := res.Events[0]
	if got.Op != EventOpInsert {
		t.Errorf("Op = %q want insert", got.Op)
	}
	if got.ID != "ag_1" {
		t.Errorf("ID = %q want ag_1", got.ID)
	}
	if got.ETag != agent.ETag {
		t.Errorf("ETag = %q want %q", got.ETag, agent.ETag)
	}

	// Update.
	_, err = s.UpdateAgent(ctx, "ag_1", agent.ETag, func(r *AgentRecord) error {
		r.Name = "Alicia"
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}

	res, err = s.ListEventsSince(ctx, got.Seq, ListEventsSinceOptions{Table: "agents"})
	if err != nil {
		t.Fatalf("ListEventsSince: %v", err)
	}
	if len(res.Events) != 1 || res.Events[0].Op != EventOpUpdate {
		t.Fatalf("events after Update = %+v", res.Events)
	}

	// Soft-delete.
	if err := s.SoftDeleteAgent(ctx, "ag_1"); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}

	res, err = s.ListEventsSince(ctx, 0, ListEventsSinceOptions{Table: "agents"})
	if err != nil {
		t.Fatalf("ListEventsSince(post-delete): %v", err)
	}
	if len(res.Events) != 3 {
		t.Fatalf("total events = %d, want 3 (insert+update+delete)", len(res.Events))
	}
	last := res.Events[2]
	if last.Op != EventOpDelete {
		t.Errorf("last Op = %q want delete", last.Op)
	}
	if last.ETag != "" {
		t.Errorf("delete event ETag = %q want empty", last.ETag)
	}
}

func TestAppendMessageEmitsEvent(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(ctx, Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if _, err := s.InsertAgent(ctx, &AgentRecord{ID: "ag_1", Name: "Alice"}, AgentInsertOptions{}); err != nil {
		t.Fatalf("InsertAgent: %v", err)
	}

	msg, err := s.AppendMessage(ctx, &MessageRecord{
		ID: "msg_1", AgentID: "ag_1", Role: "user", Content: "hello",
	}, MessageInsertOptions{})
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	res, err := s.ListEventsSince(ctx, 0, ListEventsSinceOptions{Table: "agent_messages"})
	if err != nil {
		t.Fatalf("ListEventsSince: %v", err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("events = %d want 1", len(res.Events))
	}
	if res.Events[0].ID != "msg_1" || res.Events[0].ETag != msg.ETag {
		t.Errorf("event mismatch: %+v", res.Events[0])
	}
}

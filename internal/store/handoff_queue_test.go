package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestHandoffQueueEnqueueAndListOrder(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	first, err := s.EnqueueHandoffQueuedMessage(ctx, "ag_q", "peer-a", "hello")
	if err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	second, err := s.EnqueueHandoffQueuedMessage(ctx, "ag_q", "peer-a", "world")
	if err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("ids collide: %s", first.ID)
	}
	if first.Status != "queued" || first.HolderPeer != "peer-a" {
		t.Errorf("first rec: %+v", first)
	}

	msgs, err := s.ListHandoffQueuedMessages(ctx, "ag_q")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Content != "hello" || msgs[1].Content != "world" {
		t.Fatalf("order broken: %+v", msgs)
	}

	ids, err := s.ListHandoffQueuedAgentIDs(ctx)
	if err != nil {
		t.Fatalf("agent ids: %v", err)
	}
	if len(ids) != 1 || ids[0] != "ag_q" {
		t.Fatalf("agent ids = %v", ids)
	}
}

func TestHandoffQueueCapEnforced(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for i := 0; i < MaxHandoffQueuedPerAgent; i++ {
		if _, err := s.EnqueueHandoffQueuedMessage(ctx, "ag_cap", "peer-a",
			fmt.Sprintf("msg %d", i)); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	_, err := s.EnqueueHandoffQueuedMessage(ctx, "ag_cap", "peer-a", "overflow")
	if !errors.Is(err, ErrHandoffQueueFull) {
		t.Fatalf("want ErrHandoffQueueFull, got %v", err)
	}
	// Cap is per-agent: another agent still enqueues fine.
	if _, err := s.EnqueueHandoffQueuedMessage(ctx, "ag_other", "peer-a", "ok"); err != nil {
		t.Fatalf("other agent blocked by cap: %v", err)
	}
	n, err := s.CountHandoffQueuedMessages(ctx, "ag_cap")
	if err != nil || n != MaxHandoffQueuedPerAgent {
		t.Fatalf("count = %d, err %v", n, err)
	}
}

func TestHandoffQueueDelete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	rec, err := s.EnqueueHandoffQueuedMessage(ctx, "ag_d", "peer-a", "bye")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Wrong agent_id must not match.
	if err := s.DeleteHandoffQueuedMessage(ctx, "ag_other", rec.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-agent delete: want ErrNotFound, got %v", err)
	}
	if err := s.DeleteHandoffQueuedMessage(ctx, "ag_d", rec.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteHandoffQueuedMessage(ctx, "ag_d", rec.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: want ErrNotFound, got %v", err)
	}
}

func TestHandoffQueuePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s1, err := Open(ctx, Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	rec, err := s1.EnqueueHandoffQueuedMessage(ctx, "ag_p", "peer-a", "survive me")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(ctx, Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	msgs, err := s2.ListHandoffQueuedMessages(ctx, "ag_p")
	if err != nil {
		t.Fatalf("list after reopen: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != rec.ID || msgs[0].Content != "survive me" {
		t.Fatalf("queue not persisted: %+v", msgs)
	}
}

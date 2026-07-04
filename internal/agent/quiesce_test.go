package agent

import (
	"context"
	"testing"
	"time"
)

// TestQuiescingRefusesNewChats: SetQuiescing(true) must make
// acquirePreparing (the shared Chat / ChatOneShot entry gate) refuse
// with ErrAgentBusy; SetQuiescing(false) restores it.
func TestQuiescingRefusesNewChats(t *testing.T) {
	m := &Manager{}
	m.SetQuiescing(true)
	if err := m.acquirePreparing("ag_x"); err == nil {
		t.Fatal("acquirePreparing succeeded while quiescing")
	}
	m.SetQuiescing(false)
	if err := m.acquirePreparing("ag_x"); err != nil {
		t.Fatalf("acquirePreparing after quiesce lift: %v", err)
	}
	m.releasePreparing("ag_x")
}

// TestWaitAllChatsIdle_DrainsBusyAndSummarizing: the daemon-wide idle
// wait must block while any agent is busy or a post-turn summarizer is
// in flight, and return once both clear.
func TestWaitAllChatsIdle_DrainsBusyAndSummarizing(t *testing.T) {
	m := &Manager{
		busy: map[string]busyEntry{
			"ag_a": {cancel: func() {}},
		},
	}
	m.summarizing = 1

	short, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := m.WaitAllChatsIdle(short); err == nil {
		t.Fatal("WaitAllChatsIdle returned while busy + summarizing")
	}

	done := make(chan error, 1)
	ctx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	go func() { done <- m.WaitAllChatsIdle(ctx) }()

	time.Sleep(150 * time.Millisecond)
	m.clearBusy("ag_a")
	m.busyMu.Lock()
	m.summarizing = 0
	m.busyMu.Unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitAllChatsIdle: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitAllChatsIdle did not return after drain")
	}
}

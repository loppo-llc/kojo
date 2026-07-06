package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// Steer while fully idle (no busy entry, no prepare in flight) must
// return ErrAgentNotBusy immediately.
func TestSteerIdleNotBusy(t *testing.T) {
	m := newTestManager(t)
	m.agents["ag_x"] = &Agent{ID: "ag_x", Tool: "claude"}

	if err := m.Steer(context.Background(), "ag_x", "hi"); !errors.Is(err, ErrAgentNotBusy) {
		t.Fatalf("Steer idle = %v, want ErrAgentNotBusy", err)
	}
}

// Steer during prepareChat (preparing counter set, busy entry absent)
// must wait for the turn's steer handle instead of bouncing not_busy —
// the pre-fix behavior demoted the message to a queued normal send,
// reordering it behind later steers.
func TestSteerWaitsForPreparingTurn(t *testing.T) {
	m := newTestManager(t)
	m.preparing = map[string]int{"ag_x": 1}
	m.agents["ag_x"] = &Agent{ID: "ag_x", Tool: "claude"}

	var steered atomic.Value
	go func() {
		time.Sleep(300 * time.Millisecond)
		m.busyMu.Lock()
		delete(m.preparing, "ag_x")
		m.busy["ag_x"] = busyEntry{
			startedAt: time.Now(),
			steer: func(text string) error {
				steered.Store(text)
				return nil
			},
		}
		m.busyMu.Unlock()
	}()

	if err := m.Steer(context.Background(), "ag_x", "mid-turn"); err != nil {
		t.Fatalf("Steer during prepare = %v, want nil", err)
	}
	if got, _ := steered.Load().(string); got != "mid-turn" {
		t.Fatalf("steer fn got %q, want %q", got, "mid-turn")
	}
}

// Steer while busy but before OnSteerReady fires must wait for the
// handle rather than returning ErrSteerUnsupported.
func TestSteerWaitsForSteerHandle(t *testing.T) {
	m := newTestManager(t)
	m.agents["ag_x"] = &Agent{ID: "ag_x", Tool: "claude"}
	m.busy["ag_x"] = busyEntry{startedAt: time.Now()}

	var steered atomic.Value
	go func() {
		time.Sleep(300 * time.Millisecond)
		m.busyMu.Lock()
		e := m.busy["ag_x"]
		e.steer = func(text string) error {
			steered.Store(text)
			return nil
		}
		m.busy["ag_x"] = e
		m.busyMu.Unlock()
	}()

	if err := m.Steer(context.Background(), "ag_x", "late-handle"); err != nil {
		t.Fatalf("Steer before OnSteerReady = %v, want nil", err)
	}
	if got, _ := steered.Load().(string); got != "late-handle" {
		t.Fatalf("steer fn got %q, want %q", got, "late-handle")
	}
}

// A preparing turn whose backend can never steer must fail fast with
// ErrSteerUnsupported, not block for the full steerHandleWait.
func TestSteerUnsupportedBackendFailsFast(t *testing.T) {
	m := newTestManager(t)
	m.preparing = map[string]int{"ag_x": 1}
	m.agents["ag_x"] = &Agent{ID: "ag_x", Tool: "grok"}

	start := time.Now()
	err := m.Steer(context.Background(), "ag_x", "hi")
	if !errors.Is(err, ErrSteerUnsupported) {
		t.Fatalf("Steer on grok = %v, want ErrSteerUnsupported", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("unsupported backend took %v, want fast fail", time.Since(start))
	}
}

// If the preparing turn dies before ever registering busy (prepare
// error path), the waiter must fall out with ErrAgentNotBusy instead of
// hanging until the deadline returns unsupported.
func TestSteerPrepareAbortedReturnsNotBusy(t *testing.T) {
	m := newTestManager(t)
	m.preparing = map[string]int{"ag_x": 1}
	m.agents["ag_x"] = &Agent{ID: "ag_x", Tool: "claude"}

	go func() {
		time.Sleep(300 * time.Millisecond)
		m.busyMu.Lock()
		delete(m.preparing, "ag_x")
		m.busyMu.Unlock()
	}()

	if err := m.Steer(context.Background(), "ag_x", "hi"); !errors.Is(err, ErrAgentNotBusy) {
		t.Fatalf("Steer after aborted prepare = %v, want ErrAgentNotBusy", err)
	}
}

// A waiter pinned to turn A (busy, handle not yet registered) must NOT
// deliver into a different turn B that replaces A mid-wait — it returns
// ErrAgentNotBusy so the client falls back to a normal send.
func TestSteerDoesNotCrossTurnGenerations(t *testing.T) {
	m := newTestManager(t)
	m.agents["ag_x"] = &Agent{ID: "ag_x", Tool: "claude"}
	outA := make(chan ChatEvent, 1)
	m.busy["ag_x"] = busyEntry{startedAt: time.Now(), outCh: outA}

	var steeredB atomic.Bool
	go func() {
		time.Sleep(300 * time.Millisecond)
		outB := make(chan ChatEvent, 1)
		m.busyMu.Lock()
		m.busy["ag_x"] = busyEntry{
			startedAt: time.Now(),
			outCh:     outB,
			steer: func(text string) error {
				steeredB.Store(true)
				return nil
			},
		}
		m.busyMu.Unlock()
	}()

	if err := m.Steer(context.Background(), "ag_x", "for turn A"); !errors.Is(err, ErrAgentNotBusy) {
		t.Fatalf("Steer across turn generations = %v, want ErrAgentNotBusy", err)
	}
	if steeredB.Load() {
		t.Fatal("steer aimed at turn A was delivered into turn B")
	}
}

// Cancelling the caller's context (client disconnect, shutdown) unblocks
// the wait.
func TestSteerContextCancelUnblocks(t *testing.T) {
	m := newTestManager(t)
	m.preparing = map[string]int{"ag_x": 1}
	m.agents["ag_x"] = &Agent{ID: "ag_x", Tool: "claude"}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if err := m.Steer(ctx, "ag_x", "hi"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Steer with cancelled ctx = %v, want context.Canceled", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("cancel took %v to unblock", time.Since(start))
	}
}

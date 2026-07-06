package agent

import (
	"errors"
	"testing"
	"time"
)

// TestManager_Steer_NotBusy verifies Steer refuses when the agent has no
// running turn.
func TestManager_Steer_NotBusy(t *testing.T) {
	m := newTestManager(t)
	if err := m.Steer("ag_nobody", "hello"); !errors.Is(err, ErrAgentNotBusy) {
		t.Errorf("err = %v, want ErrAgentNotBusy", err)
	}
}

// TestManager_Steer_UnsupportedBackend verifies that a busy turn whose
// backend never registered a steer handle (steer == nil, e.g. codex/grok/
// llamacpp) surfaces ErrSteerUnsupported rather than silently no-oping.
func TestManager_Steer_UnsupportedBackend(t *testing.T) {
	m := newTestManager(t)
	m.busyMu.Lock()
	m.busy["ag_test"] = busyEntry{startedAt: time.Now(), cancel: func() {}}
	m.busyMu.Unlock()

	if err := m.Steer("ag_test", "hello"); !errors.Is(err, ErrSteerUnsupported) {
		t.Errorf("err = %v, want ErrSteerUnsupported", err)
	}
}

// TestManager_Steer_InjectsAndPersists verifies that once a backend has
// registered a steer handle (mirroring ChatOptions.OnSteerReady), Steer
// forwards the text to it and appends it to the transcript as a plain user
// message.
func TestManager_Steer_InjectsAndPersists(t *testing.T) {
	m := newTestManager(t)
	m.agents["ag_test"] = &Agent{ID: "ag_test", Name: "Test", Tool: "claude"}
	if err := m.store.Upsert(m.agents["ag_test"]); err != nil {
		t.Fatal(err)
	}

	var got string
	outCh := make(chan ChatEvent, 4)
	m.busyMu.Lock()
	m.busy["ag_test"] = busyEntry{
		startedAt: time.Now(),
		cancel:    func() {},
		outCh:     outCh,
		steer: func(text string) error {
			got = text
			return nil
		},
	}
	m.busyMu.Unlock()

	if err := m.Steer("ag_test", "steer this turn"); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if got != "steer this turn" {
		t.Errorf("steer handle received %q, want %q", got, "steer this turn")
	}

	msgs, err := m.Messages("ag_test", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, mm := range msgs {
		if mm.Role == "user" && mm.Content == "steer this turn" {
			found = true
		}
	}
	if !found {
		t.Error("steered message not persisted to transcript")
	}

	select {
	case e := <-outCh:
		if e.Type != "message" || e.Message == nil || e.Message.Content != "steer this turn" {
			t.Errorf("unexpected outCh event: %+v", e)
		}
	default:
		t.Error("expected a live event pushed to outCh for the steered message")
	}
}

// TestManager_Steer_SteerFuncError propagates the backend's error (e.g.
// process already exited / stdin closed) rather than swallowing it.
func TestManager_Steer_SteerFuncError(t *testing.T) {
	m := newTestManager(t)
	m.busyMu.Lock()
	m.busy["ag_test"] = busyEntry{
		startedAt: time.Now(),
		cancel:    func() {},
		steer: func(text string) error {
			return errors.New("turn is no longer accepting input")
		},
	}
	m.busyMu.Unlock()

	if err := m.Steer("ag_test", "too late"); err == nil {
		t.Error("expected error from steer func to propagate")
	}
}

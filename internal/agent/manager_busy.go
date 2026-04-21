package agent

import (
	"fmt"
	"time"
)

// IsBusy returns true if the agent has an active chat.
func (m *Manager) IsBusy(agentID string) bool {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	_, ok := m.busy[agentID]
	return ok
}

// BusySince returns the time when the agent started its current chat.
// Returns zero time and false if the agent is not busy.
func (m *Manager) BusySince(agentID string) (time.Time, bool) {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	entry, ok := m.busy[agentID]
	if !ok {
		return time.Time{}, false
	}
	return entry.startedAt, true
}

// Subscribe returns a snapshot of all past events and a live channel for an
// agent's ongoing chat. The caller must call unsub when done to free resources.
// If the agent is not busy, busy is false and all other values are zero.
func (m *Manager) Subscribe(agentID string) (startedAt time.Time, past []ChatEvent, live <-chan ChatEvent, unsub func(), busy bool) {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	entry, ok := m.busy[agentID]
	if !ok {
		return time.Time{}, nil, nil, func() {}, false
	}
	if entry.broadcaster == nil {
		return entry.startedAt, nil, nil, func() {}, true
	}
	past, live, unsub = entry.broadcaster.Subscribe()
	return entry.startedAt, past, live, unsub, true
}

// Abort cancels any running chat for an agent.
func (m *Manager) Abort(agentID string) {
	m.busyMu.Lock()
	if entry, ok := m.busy[agentID]; ok {
		entry.cancel()
	}
	m.busyMu.Unlock()
}

func (m *Manager) clearBusy(agentID string) {
	m.busyMu.Lock()
	delete(m.busy, agentID)
	m.busyMu.Unlock()
}

// waitBusyClear waits up to 5 seconds for the agent's busy entry to be removed.
// Returns ErrAgentBusy if the agent is still busy after the timeout.
func (m *Manager) waitBusyClear(agentID string) error {
	for i := 0; i < 50; i++ {
		m.busyMu.Lock()
		_, busy := m.busy[agentID]
		m.busyMu.Unlock()
		if !busy {
			return nil
		}
		if i == 49 {
			return fmt.Errorf("%w, try again later", ErrAgentBusy)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// waitOneShotClear waits up to 5 seconds for the agent's in-flight one-shot
// chats (Slack, Discord, Group DM) to drain. Call cancelOneShots first so the
// goroutines are actively winding down. Returns ErrAgentBusy if not drained.
func (m *Manager) waitOneShotClear(agentID string) error {
	for i := 0; i < 50; i++ {
		m.oneShotCancelsMu.Lock()
		n := len(m.oneShotCancels[agentID])
		m.oneShotCancelsMu.Unlock()
		if n == 0 {
			return nil
		}
		if i == 49 {
			return fmt.Errorf("%w, try again later", ErrAgentBusy)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// acquireResetGuard marks the agent as resetting, cancels any active chat,
// and returns a cleanup function that removes the resetting flag.
// Returns ErrAgentBusy if the agent is already being reset.
func (m *Manager) acquireResetGuard(agentID string) (func(), error) {
	m.busyMu.Lock()
	if m.resetting[agentID] {
		m.busyMu.Unlock()
		return nil, fmt.Errorf("%w, try again later", ErrAgentBusy)
	}
	if m.editing[agentID] {
		m.busyMu.Unlock()
		return nil, fmt.Errorf("%w, try again later", ErrAgentBusy)
	}
	m.resetting[agentID] = true
	if entry, busy := m.busy[agentID]; busy {
		entry.cancel()
	}
	m.busyMu.Unlock()

	cleanup := func() {
		m.busyMu.Lock()
		delete(m.resetting, agentID)
		m.busyMu.Unlock()
	}
	return cleanup, nil
}

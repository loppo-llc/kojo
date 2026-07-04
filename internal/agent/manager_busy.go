package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// IsBusy returns true if the agent has an active chat (any source).
func (m *Manager) IsBusy(agentID string) bool {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	_, ok := m.busy[agentID]
	return ok
}

// IsBusyForStatus returns true only when the agent is busy with a user
// chat or cron job — automated notifications (group DM replies etc.) are
// excluded so that members don't all appear "busy" when responding to a
// broadcast notification.
func (m *Manager) IsBusyForStatus(agentID string) bool {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	entry, ok := m.busy[agentID]
	if !ok {
		return false
	}
	return entry.source == BusySourceUser || entry.source == BusySourceCron
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

// Abort cancels any running chat for an agent — both the user
// chat (busy entry) AND any in-flight one-shot chats (Slack /
// group-DM responders). The §3.7 switch-device
// orchestrator relies on this dual cancel: WaitChatIdle's drain
// waits on both `busy` and `oneShotCancels`, so a bare Abort
// that only signaled the user chat would let one-shots run past
// the quiesce window and write transcript / JSONL after the
// snapshot.
//
// cancelOneShots leaves the oneShotCancels map entry intact so
// waitOneShotClear / WaitChatIdle can observe completion as each
// goroutine removes itself via untrackOneShot. Idempotent — a
// second Abort on a finished chat is a no-op for both halves.
func (m *Manager) Abort(agentID string) {
	m.busyMu.Lock()
	if entry, ok := m.busy[agentID]; ok {
		entry.cancel()
	}
	m.busyMu.Unlock()
	m.cancelOneShots(agentID)
}

// WaitChatIdle polls busyMu until every concurrent write path
// has drained for the agent OR ctx is cancelled. Returns nil on
// idle, ctx.Err() on timeout. Caller is responsible for issuing
// Abort first (WaitChatIdle just observes flags; without an
// abort it would block until the chat finishes naturally).
//
// Drains:
//   - busy:       in-flight Chat / ChatOneShot
//   - preparing:  a Chat between prepareChat entry and busy
//     entry insert (disk side effects still landing)
//   - editing:    Regenerate / transcript edit holding the
//     acquireTranscriptEdit guard
//   - resetting:  ResetData / Fork / Archive / ResetSession
//     holding the acquireResetGuard
//   - mutating:   non-chat state writers (persona / settings /
//     task / credential / avatar / slack tokens)
//     holding AcquireMutation
//   - oneShotCancels: Slack / group-DM one-shot chats
//     cancelled by switch_device_handler's Abort
//
// Without all six checks the §3.7 quiesce window would race a
// Slack / cron / persona-edit write that landed mid-handoff.
//
// Used by the §3.7 device-switch orchestrator: after Abort the
// chat goroutine still needs a few hundred ms to flush its final
// claude session JSONL append before we snapshot the file. The
// 1.5 s caller default is generous; in practice typical aborts
// drain in well under 100 ms.
func (m *Manager) WaitChatIdle(ctx context.Context, agentID string) error {
	return m.waitChatIdle(ctx, agentID, false)
}

// WaitChatIdleSelfCall is the §3.7 device-switch variant used
// when the HTTP request is the agent's own chat tool — typically
// the kojo-switch-device skill's curl. That curl is driven by
// the busy entry it would otherwise wait on, so the busy check
// would deadlock until the orchestration context timed out.
// Skipping busy lets every OTHER concurrent writer (preparing,
// Slack one-shots, editing, resetting, mutating,
// profileGen) still drain before the snapshot.
//
// preparing is intentionally NOT skipped: prepareChat exits
// before busy is set, so the self chat itself no longer counts;
// a non-zero preparing counter means a DIFFERENT chat is in
// prepareChat and must be drained.
//
// Pair with CancelOneShotsForAgent (not Abort) on entry so we
// don't cancel the busy entry making the call.
func (m *Manager) WaitChatIdleSelfCall(ctx context.Context, agentID string) error {
	return m.waitChatIdle(ctx, agentID, true)
}

func (m *Manager) waitChatIdle(ctx context.Context, agentID string, skipBusy bool) error {
	for {
		m.busyMu.Lock()
		_, busyOK := m.busy[agentID]
		preparing := m.preparing[agentID] > 0
		editingOK := m.editing[agentID]
		resettingOK := m.resetting[agentID]
		mutating := m.mutating[agentID] > 0
		m.busyMu.Unlock()
		m.oneShotCancelsMu.Lock()
		oneShotN := len(m.oneShotCancels[agentID])
		m.oneShotCancelsMu.Unlock()
		// profileGen tracks in-flight regeneratePublicProfile
		// goroutines. The entry-gate refuses new regens during
		// switching, but a regen that started BEFORE SetSwitching
		// can still be mid-LLM-roundtrip and would write
		// PublicProfile after the snapshot if we don't wait.
		// LLM round-trips can exceed the 3s quiesce window; in
		// that case the orchestrator times out → 409 fail-closed.
		m.mu.Lock()
		profileGen := m.profileGen[agentID]
		m.mu.Unlock()
		if skipBusy {
			busyOK = false
		}
		if !busyOK && !preparing && !editingOK && !resettingOK && !mutating && oneShotN == 0 && !profileGen {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// SetQuiescing flips the daemon-wide chat gate. When on,
// acquirePreparing refuses every new Chat / ChatOneShot with
// ErrAgentBusy ("server restart in progress") for ALL agents.
// In-flight turns are unaffected — the restart path waits for them
// via WaitAllChatsIdle. Idempotent; guard-off on a non-quiescing
// manager is a no-op.
func (m *Manager) SetQuiescing(on bool) {
	m.busyMu.Lock()
	m.quiescing = on
	m.busyMu.Unlock()
}

// WaitAllChatsIdle polls until every concurrent write path across
// ALL agents has drained, or ctx is cancelled. The daemon-wide
// analogue of WaitChatIdle — same six checks, plus `switching`
// (a §3.7 device switch must not be cut in half by a re-exec) and
// the post-turn summarize counter (turnSummarizeAsync runs after
// busy clears, and killing it mid-write would corrupt recent.md /
// the cursor file).
//
// Callers should SetQuiescing(true) first; otherwise a fresh turn
// can start between the idle observation and whatever the caller
// does next (the restart path re-execs the process).
func (m *Manager) WaitAllChatsIdle(ctx context.Context) error {
	for {
		m.busyMu.Lock()
		idle := len(m.busy) == 0 && len(m.preparing) == 0 &&
			len(m.editing) == 0 && len(m.resetting) == 0 &&
			len(m.mutating) == 0 && len(m.switching) == 0 &&
			m.summarizing == 0
		m.busyMu.Unlock()
		if idle {
			m.oneShotCancelsMu.Lock()
			for _, set := range m.oneShotCancels {
				if len(set) > 0 {
					idle = false
					break
				}
			}
			m.oneShotCancelsMu.Unlock()
		}
		if idle {
			m.mu.Lock()
			for _, on := range m.profileGen {
				if on {
					idle = false
					break
				}
			}
			m.mu.Unlock()
		}
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// CancelOneShotsForAgent cancels every in-flight one-shot chat
// (Slack / group-DM responder) for the agent
// WITHOUT touching the busy entry. The §3.7 device-switch
// orchestrator calls this on the agent-self-call path where
// Abort()'s busy-cancel would kill the curl that initiated the
// switch. Pairs with WaitChatIdleSelfCall.
func (m *Manager) CancelOneShotsForAgent(agentID string) {
	m.cancelOneShots(agentID)
}

// acquirePreparing marks the agent as inside prepareChat. Returns
// ErrAgentBusy when switching is set so callers refuse the chat
// before any disk side effect. Pairs with releasePreparing in
// defer / chat exit.
func (m *Manager) acquirePreparing(agentID string) error {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	if m.quiescing {
		return fmt.Errorf("%w: server restart in progress", ErrAgentBusy)
	}
	if m.switching != nil && m.switching[agentID] {
		return fmt.Errorf("%w: device switch in progress", ErrAgentBusy)
	}
	// Lazy-init: not all test fixtures use NewManager, so the
	// map may be nil here. Guard so an unrelated test that
	// drives Chat through a hand-rolled Manager doesn't panic.
	if m.preparing == nil {
		m.preparing = make(map[string]int)
	}
	m.preparing[agentID]++
	return nil
}

// releasePreparing decrements the preparing counter for the
// agent. No-op when the counter is already zero (defensive
// against a double-release) or the map was never initialised
// (test fixtures that hand-roll Manager).
func (m *Manager) releasePreparing(agentID string) {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	if m.preparing == nil {
		return
	}
	if m.preparing[agentID] > 0 {
		m.preparing[agentID]--
		if m.preparing[agentID] == 0 {
			delete(m.preparing, agentID)
		}
	}
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

// SetSwitching marks the agent as mid-§3.7-switch (true) or
// clears the marker (false). When set, IsSwitching returns true
// and the chat path refuses new starts with ErrAgentSwitching so
// no transcript / JSONL is written between Step -1's quiesce and
// the post-complete drain. Idempotent: setting true on an
// already-switching agent is a no-op; clearing an agent that
// wasn't switching is also a no-op.
//
// Returns ErrAgentBusy when on=true and a restart drain is
// quiescing the daemon — the quiesce check and the switching set
// share busyMu, so a switch can never start after WaitAllChatsIdle
// observed idle. Clearing (on=false) always succeeds so the
// orchestrator's deferred cleanup can't be refused.
func (m *Manager) SetSwitching(agentID string, on bool) error {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	if on && m.quiescing {
		return fmt.Errorf("%w: server restart in progress", ErrAgentBusy)
	}
	if m.switching == nil {
		m.switching = make(map[string]bool)
	}
	if on {
		m.switching[agentID] = true
	} else {
		delete(m.switching, agentID)
	}
	return nil
}

// IsSwitching returns true when SetSwitching(agentID, true) is in
// effect and a §3.7 device-switch is mid-flight.
func (m *Manager) IsSwitching(agentID string) bool {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	return m.switching[agentID]
}

// AcquireMutation reserves a slot in the per-agent mutation
// counter and returns a release callback. The acquire fails
// when a §3.7 device switch is mid-flight on this peer. While
// the slot is held, WaitChatIdle observes the agent as
// non-idle — so Step -1's snapshot cannot race a mutation
// that started just before SetSwitching landed.
//
// Common entry guard for every agent-state mutation surface
// that does NOT route through prepareChat (persona / settings
// / slackbot / tasks / credentials / avatar / slack tokens).
// The release callback is idempotent and safe to defer.
//
// Threadsafe; nil-safe for hand-rolled test fixtures.
func (m *Manager) AcquireMutation(agentID string) (func(), error) {
	if m == nil {
		return func() {}, nil
	}
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	if m.quiescing {
		return nil, fmt.Errorf("%w: server restart in progress", ErrAgentBusy)
	}
	if m.switching != nil && m.switching[agentID] {
		return nil, fmt.Errorf("%w: device switch in progress", ErrAgentBusy)
	}
	if m.mutating == nil {
		m.mutating = make(map[string]int)
	}
	m.mutating[agentID]++
	released := false
	return func() {
		m.busyMu.Lock()
		defer m.busyMu.Unlock()
		if released {
			return
		}
		released = true
		if m.mutating == nil {
			return
		}
		if m.mutating[agentID] > 0 {
			m.mutating[agentID]--
		}
		if m.mutating[agentID] == 0 {
			delete(m.mutating, agentID)
		}
	}, nil
}

// acquireResetGuard marks the agent as resetting, cancels any active chat,
// and returns a cleanup function that removes the resetting flag.
// Returns ErrAgentBusy if the agent is already being reset.
func (m *Manager) acquireResetGuard(agentID string) (func(), error) {
	m.busyMu.Lock()
	if m.quiescing {
		m.busyMu.Unlock()
		return nil, fmt.Errorf("%w: server restart in progress", ErrAgentBusy)
	}
	if m.resetting[agentID] {
		m.busyMu.Unlock()
		return nil, fmt.Errorf("%w, try again later", ErrAgentBusy)
	}
	if m.editing[agentID] {
		m.busyMu.Unlock()
		return nil, fmt.Errorf("%w, try again later", ErrAgentBusy)
	}
	if m.switching != nil && m.switching[agentID] {
		// §3.7 device switch is mid-flight: reset / fork
		// would re-emit MEMORY.md and session JSONLs after
		// the snapshot, stranding them on source.
		m.busyMu.Unlock()
		return nil, fmt.Errorf("%w: device switch in progress", ErrAgentBusy)
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

// WaitChatDone blocks until the busy entry's chat goroutine emits a
// terminal `done` event for the agent's in-flight turn or ctx is
// cancelled. Returns the captured assistant Message converted to a
// store.MessageRecord (with seq=0; caller stamps the target-side
// allocation, ToolUses cleared — see below) or nil on ctx
// cancellation / missing busy entry / nil accumulator.
//
// ToolUses is intentionally CLEARED on the returned record. The
// done event carries the WHOLE turn — the agent's pre-tool-call
// text, every tool_use (including the kojo-switch-device call
// that triggered this whole flow), and every tool_result. The
// snapshot row the orchestrator shipped during /agent-sync ALREADY
// carries that tool_use; if the tail row carried it too, target's
// userMessageAddressed / planATailContent would mis-classify the
// tail as another snapshot (substring-detect of `kojo-switch-device`
// in ToolUses) and the commitment text would not surface in the
// arrival prompt. Stripping ToolUses leaves the tail as a pure
// "post-tool-result completion text" row, distinct in shape from
// the snapshot.
//
// Used by the device-switch self-call orchestrator's deferred
// finalize: after /handoff/complete moves the lock to target, the
// source's claude turn continues for a few hundred milliseconds to
// emit a post-tool-result "I'll do X on arrival" message. Without
// this wait the orchestrator would race ahead, ship /handoff/finalize
// to target empty-handed, and target's arrival prompt would fire
// against a transcript missing the agent's own commitment text. The
// caller passes a bounded ctx so a wedged source backend can't
// indefinitely defer target activation; on timeout the orchestrator
// ships finalize WITHOUT the tail (current behavior — degraded but
// not stuck).
//
// Snapshots the accumulator pointer under busyMu so a concurrent
// clearBusy(agentID) that removes the entry doesn't strand the
// caller on a dead reference. The accumulator outlives clearBusy
// (closed channel + capture stays live until the wait returns).
func (m *Manager) WaitChatDone(ctx context.Context, agentID string) *store.MessageRecord {
	if m == nil {
		return nil
	}
	m.busyMu.Lock()
	entry, ok := m.busy[agentID]
	m.busyMu.Unlock()
	if !ok || entry.accumulator == nil {
		return nil
	}
	doneMsg := entry.accumulator.WaitDone(ctx)
	if doneMsg == nil {
		return nil
	}
	rec, err := messageToRecord(agentID, doneMsg)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("WaitChatDone: messageToRecord failed", "agent", agentID, "err", err)
		}
		return nil
	}
	// Strip ToolUses so the tail's wire shape stays distinct from
	// the snapshot's (which carries kojo-switch-device in its
	// ToolUses). See doc-comment above.
	rec.ToolUses = nil
	ts := parseAgentRFC3339Millis(doneMsg.Timestamp)
	if ts == 0 {
		ts = store.NowMillis()
	}
	rec.CreatedAt = ts
	rec.UpdatedAt = ts
	return rec
}

// SnapshotAccumulatedMessageRecord reconstructs the in-flight
// assistant message from the busy entry's shared accumulator and
// returns it as a store.MessageRecord ready for inclusion in the
// §3.7 agent-sync payload.
//
// Used by the device-switch self-call path: the assistant turn
// containing the kojo-switch-device tool_use is still mid-flight
// (accumulated in processChatEvents' local variables, not yet
// persisted to the messages table). Without this snapshot the sync
// payload would miss the last assistant turn entirely, and the §3.7
// release guard would prevent the processChatEvents defer from ever
// persisting it.
//
// Reads the shared chatAccumulator (NOT the chat broadcaster's
// log) because outCh's non-blocking send drops non-terminal events
// under back-pressure — so the broadcaster's log silently misses
// streaming text / tool_use rows on long claude turns and the
// snapshot would migrate a partial message. The accumulator is fed
// inline by processChatEvents on every event, regardless of outCh
// pressure, so a Snapshot read here matches the chat goroutine's
// own view of the turn.
//
// Falls back to the broadcaster's log when the busy entry has no
// accumulator (legacy / hand-rolled test fixtures); the legacy path
// retains the original behavior so existing tests don't regress.
//
// Returns nil if the agent is not busy or no streaming data has
// accumulated. The caller appends the returned record to the sync
// payload WITHOUT persisting it to the source's DB — on abort the
// chat continues normally and the done event persists the full
// message; on success the source is released and persistence is moot.
func (m *Manager) SnapshotAccumulatedMessageRecord(agentID string) *store.MessageRecord {
	m.busyMu.Lock()
	entry, ok := m.busy[agentID]
	m.busyMu.Unlock()
	if !ok {
		return nil
	}

	var text, thinking string
	var toolUses []ToolUse
	if entry.accumulator != nil {
		text, thinking, toolUses = entry.accumulator.Snapshot()
	} else if entry.broadcaster != nil {
		// Fallback for legacy busy entries that predate the
		// accumulator. Same shape as the original implementation —
		// reads the broadcaster's past log and folds it into a
		// MessageRecord. Drops events if outCh ever back-pressured;
		// acceptable here because the only callers without an
		// accumulator are test fixtures whose chats run too short
		// to back-pressure outCh.
		past, _, unsub := entry.broadcaster.Subscribe()
		unsub()
		var tBuf, thBuf strings.Builder
		for _, ev := range past {
			switch ev.Type {
			case "text":
				tBuf.WriteString(ev.Delta)
			case "thinking":
				thBuf.WriteString(ev.Delta)
			case "tool_use":
				toolUses = append(toolUses, ToolUse{
					ID:    ev.ToolUseID,
					Name:  ev.ToolName,
					Input: ev.ToolInput,
				})
			case "tool_result":
				matchToolOutput(toolUses, ev.ToolUseID, ev.ToolName, ev.ToolOutput)
			}
		}
		text = tBuf.String()
		thinking = thBuf.String()
	} else {
		return nil
	}

	if text == "" && thinking == "" && len(toolUses) == 0 {
		return nil
	}
	msg := newAssistantMessage()
	msg.Content = text
	msg.Thinking = thinking
	msg.ToolUses = toolUses
	rec, err := messageToRecord(agentID, msg)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("SnapshotAccumulatedMessageRecord: conversion failed", "agent", agentID, "err", err)
		}
		return nil
	}
	ts := parseAgentRFC3339Millis(msg.Timestamp)
	if ts == 0 {
		ts = store.NowMillis()
	}
	rec.CreatedAt = ts
	rec.UpdatedAt = ts
	return rec
}

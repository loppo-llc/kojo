package agent

import "errors"

// subagentBackfillWindow bounds how far back the durable-attach lookup scans
// for the message that owns a background subagent's Task tool_use. A Task and
// its background child are close together in the transcript, so a modest window
// finds the owner while keeping the per-batch DB read cheap.
const subagentBackfillWindow = 200

// handleSubagentActivity is the Manager callback the ClaudeBackend's subagent
// tailer drives. It (a) durably attaches the newly-observed Children onto the
// owning message's Task ToolUse so a reload keeps them (Option C backfill) and
// (b) pushes the live events onto any active turn's broadcaster so a connected
// client sees them nest under the Task chip immediately (Option A live tail).
func (m *Manager) handleSubagentActivity(agentID string, act subagentActivity) {
	if act.ToolUseID == "" {
		return
	}
	// §3.7 fencing: don't write transcript for an agent this peer no longer
	// owns (evicted mid device-switch). Mirrors processChatEvents' guard.
	m.mu.Lock()
	_, here := m.agents[agentID]
	m.mu.Unlock()
	if !here {
		return
	}

	if len(act.Children) > 0 {
		if err := m.attachSubagentChildren(agentID, act.ToolUseID, act.Children); err != nil &&
			!errors.Is(err, ErrMessageNotFound) {
			m.logger.Warn("subagent activity: durable attach failed",
				"agent", agentID, "toolUseId", act.ToolUseID, "err", err)
		}
	}
	if len(act.Events) > 0 {
		m.pushSubagentEventsLive(agentID, act.Events)
	}
}

// attachSubagentChildren finds the persisted message whose Task ToolUse matches
// toolUseID and idempotently merges incoming children onto it. Returns
// ErrMessageNotFound when no message in the backfill window owns the tool_use
// (e.g. it scrolled out, or belongs to a one-shot transcript kojo doesn't
// persist) — callers treat that as a benign miss.
func (m *Manager) attachSubagentChildren(agentID, toolUseID string, incoming []ToolUse) error {
	msgs, err := loadMessages(agentID, subagentBackfillWindow)
	if err != nil {
		return err
	}
	// Newest-first is the likely location of a still-running background
	// subagent's owner, so walk from the tail.
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if !mergeChildrenIntoMessage(msg, toolUseID, incoming) {
			// Either this message doesn't own the tool_use, or it does but the
			// merge was a no-op (already attached). Distinguish so a no-op on
			// the true owner doesn't fall through to ErrMessageNotFound.
			if messageOwnsToolUse(msg, toolUseID) {
				return nil
			}
			continue
		}
		if _, err := updateMessageToolUses(agentID, msg.ID, msg.ToolUses); err != nil {
			return err
		}
		return nil
	}
	return ErrMessageNotFound
}

// pushSubagentEventsLive best-effort delivers subagent events onto the agent's
// currently active turn broadcaster (if any). When idle there is no broadcaster
// to attach to; the durable merge above still makes the activity visible on the
// next reload/reconnect (Option C). Non-blocking so a stalled consumer never
// wedges the tailer.
func (m *Manager) pushSubagentEventsLive(agentID string, events []ChatEvent) {
	// Send while HOLDING busyMu: clearBusy removes the entry under busyMu
	// BEFORE the chat goroutine's deferred close(outCh), so an entry observed
	// under the lock is guaranteed to have an open channel for the duration of
	// the lock. Releasing before the send would race that close → send-on-
	// closed-channel panic. Same discipline as Manager.Steer's live push. The
	// non-blocking select keeps the lock hold short even under backpressure.
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	entry, ok := m.busy[agentID]
	if !ok || entry.outCh == nil {
		return
	}
	for _, ev := range events {
		select {
		case entry.outCh <- ev:
		default:
			// Broadcaster backpressure: drop the live copy. The durable merge
			// already persisted it, so a reconnect resyncs from the transcript.
			return
		}
	}
}

// messageOwnsToolUse reports whether any top-level ToolUse of msg has the given
// id (the Task tool_use a background subagent belongs to).
func messageOwnsToolUse(msg *Message, toolUseID string) bool {
	for i := range msg.ToolUses {
		if msg.ToolUses[i].ID == toolUseID {
			return true
		}
	}
	return false
}

// mergeChildrenIntoMessage folds incoming subagent children onto the Task
// ToolUse of msg identified by toolUseID, in place. Returns whether the message
// changed (false when the tool_use isn't in this message, or the merge added
// nothing new — keeping the durable write idempotent).
func mergeChildrenIntoMessage(msg *Message, toolUseID string, incoming []ToolUse) bool {
	for i := range msg.ToolUses {
		if msg.ToolUses[i].ID != toolUseID {
			continue
		}
		merged, changed := mergeSubagentChildren(msg.ToolUses[i].Children, incoming)
		if changed {
			msg.ToolUses[i].Children = merged
		}
		return changed
	}
	return false
}

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/loppo-llc/kojo/internal/notifysource"
)

// ResetData removes conversation logs and memory but keeps settings, persona, avatar, and credentials.
func (m *Manager) ResetData(id string) error {
	m.mu.Lock()
	a, ok := m.agents[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrAgentNotFound, id)
	}
	name := a.Name
	m.mu.Unlock()

	cleanup, err := m.acquireResetGuard(id)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := m.waitBusyClear(id); err != nil {
		return err
	}

	dir := agentDir(id)

	// Remove conversation log
	if err := os.Remove(filepath.Join(dir, messagesFile)); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("reset: failed to remove messages", "agent", id, "err", err)
	}

	// Remove memory files
	if err := os.RemoveAll(filepath.Join(dir, "memory")); err != nil {
		m.logger.Warn("reset: failed to remove memory dir", "agent", id, "err", err)
	}
	if err := os.Remove(filepath.Join(dir, "MEMORY.md")); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("reset: failed to remove MEMORY.md", "agent", id, "err", err)
	}

	// Close and remove FTS index
	m.closeIndex(id)
	if err := os.RemoveAll(filepath.Join(dir, indexDir)); err != nil {
		m.logger.Warn("reset: failed to remove index dir", "agent", id, "err", err)
	}

	// Remove persona summary cache (will be regenerated)
	if err := os.Remove(filepath.Join(dir, "persona_summary.md")); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("reset: failed to remove persona summary", "agent", id, "err", err)
	}

	// Remove tasks (acquire lock to avoid racing with concurrent task API calls)
	mu := agentTaskLock(id)
	mu.Lock()
	DeleteTasksFile(id)
	mu.Unlock()

	// Remove auto-summary marker
	if err := os.Remove(filepath.Join(dir, autoSummaryMarkerFile)); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("reset: failed to remove autosummary marker", "agent", id, "err", err)
	}

	// Remove rolling short-term memory (memory/recent.md). The append-only
	// daily diaries under memory/YYYY-MM-DD.md are kept as an audit trail
	// — recent.md is just a derived rolling pointer, safe to drop on
	// reset so the next session starts without stale short-term memory.
	if err := os.Remove(filepath.Join(dir, "memory", recentSummaryFile)); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("reset: failed to remove recent summary", "agent", id, "err", err)
	}

	// Remove cron lock file
	if err := os.Remove(filepath.Join(dir, cronLockFile)); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("reset: failed to remove cron lock", "agent", id, "err", err)
	}

	// Remove external platform chat history (Slack, etc.)
	if err := os.RemoveAll(filepath.Join(dir, "chat_history")); err != nil {
		m.logger.Warn("reset: failed to remove chat_history dir", "agent", id, "err", err)
	}

	// Remove CLI local state so next chat starts fresh
	if err := os.RemoveAll(filepath.Join(dir, ".claude")); err != nil {
		m.logger.Warn("reset: failed to remove .claude dir", "agent", id, "err", err)
	}
	if err := os.RemoveAll(filepath.Join(dir, ".gemini")); err != nil {
		m.logger.Warn("reset: failed to remove .gemini dir", "agent", id, "err", err)
	}

	// Clear global CLI session stores
	clearClaudeSession(id)
	clearGeminiSession(id)

	// Recreate empty memory directory and MEMORY.md (required for agent to function)
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o755); err != nil {
		return fmt.Errorf("recreate memory dir: %w", err)
	}
	initial := fmt.Sprintf("# %s's Memory\n\nThis file stores persistent memories. Update it as you learn new things.\n", name)
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(initial), 0o644); err != nil {
		return fmt.Errorf("recreate MEMORY.md: %w", err)
	}

	// Clear last message preview
	m.mu.Lock()
	if a, ok := m.agents[id]; ok {
		a.LastMessage = nil
		a.UpdatedAt = time.Now().Format(time.RFC3339)
	}
	m.mu.Unlock()

	m.save()
	m.logger.Info("agent data reset", "id", id)
	return nil
}

// Archive marks an agent as archived without deleting most of its data.
//
// All runtime activity is stopped: cron is unscheduled, the notify poller
// is detached, in-flight one-shot chats are cancelled, the cached memory
// index is closed. The agent stays in the in-memory map (with Archived=true)
// and on disk — credentials, notify tokens, messages, memory, persona,
// avatar, tasks are all retained. Inbound chats route through prepareChat,
// which refuses archived agents with ErrAgentArchived.
//
// EXCEPTION: group DM memberships are removed (and 2-person groups are
// dissolved). A dormant member silently sitting in a chat room is more
// confusing than useful, so we treat group membership as an "active state"
// that doesn't survive archive. Unarchive does NOT restore memberships —
// the agent must be re-invited.
//
// The handler is responsible for stopping the Slack bot, since the bot lifecycle
// is owned by the server (slackHub), not the agent manager.
//
// Idempotent: calling Archive on an already-archived agent is a no-op.
func (m *Manager) Archive(id string) error {
	m.mu.Lock()
	a, ok := m.agents[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrAgentNotFound, id)
	}
	if a.Archived {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	cleanup, err := m.acquireResetGuard(id)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := m.waitBusyClear(id); err != nil {
		return err
	}

	// Set the Archived flag FIRST so any concurrent Update / UpdateNotifySources
	// path that re-checks under m.mu sees the archived state and skips its
	// schedule / rebuild side-effects. The runtime stoppage below then cleans
	// up anything those paths managed to slip in before we took the lock.
	now := time.Now().Format(time.RFC3339)
	m.mu.Lock()
	if a, ok := m.agents[id]; ok {
		a.Archived = true
		a.ArchivedAt = now
		a.UpdatedAt = now
	}
	m.mu.Unlock()

	m.cron.Remove(id)
	// DetachAgent (not RemoveAgent) keeps cursors / lastPoll so unarchive
	// resumes from the last delivered position instead of replaying history.
	m.notifyPoller.DetachAgent(id)
	m.cancelOneShots(id)
	// Wait for in-flight one-shot chats (Slack, Discord, Group DM) to drain
	// before touching group DM membership. Best-effort: if they don't drain
	// within the timeout we log and continue — the chats are cancelled and
	// will exit on their own; we just don't block the API caller indefinitely.
	// Doing this BEFORE groupdms.RemoveAgent prevents a draining one-shot
	// from posting into a group we're about to delete (which would either
	// fail with ErrGroupNotFound or — worse — write transcript bytes to a
	// directory that's seconds away from being os.RemoveAll'd).
	if err := m.waitOneShotClear(id); err != nil {
		m.logger.Warn("archive: one-shot chats did not drain in time", "agent", id, "err", err)
	}
	// Remove from all group DMs so the agent doesn't keep showing up as a
	// silent member that never replies. 2-person groups are dissolved (same
	// semantics as Delete). Memberships are NOT restored on Unarchive — the
	// agent must be re-invited if needed.
	if m.groupdms != nil {
		m.groupdms.RemoveAgent(id)
	}
	m.closeIndex(id)

	m.save()
	m.logger.Info("agent archived", "id", id)
	return nil
}

// Unarchive restores a previously archived agent: clears the Archived flag,
// re-schedules its cron, and rebuilds notify sources. Slack bot re-arming is
// the handler's responsibility (server owns slackHub lifecycle).
//
// Idempotent: calling Unarchive on a non-archived agent is a no-op.
//
// Serializes against Archive via acquireResetGuard so an Archive/Unarchive
// race can't interleave flag flips with cron/poller operations. The
// idempotency check happens INSIDE the guard so we can't observe a stale
// "not archived" state while a concurrent Archive is mid-flight (the guard
// blocks until that Archive's cleanup() releases it).
func (m *Manager) Unarchive(id string) error {
	// Existence pre-check (cheap, avoids acquiring the guard for unknown IDs).
	m.mu.Lock()
	_, ok := m.agents[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrAgentNotFound, id)
	}

	cleanup, err := m.acquireResetGuard(id)
	if err != nil {
		return err
	}
	defer cleanup()

	m.mu.Lock()
	a, ok := m.agents[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrAgentNotFound, id)
	}
	if !a.Archived {
		// Either Unarchive raced and won, or the agent was never archived —
		// either way nothing more to do.
		m.mu.Unlock()
		return nil
	}
	a.Archived = false
	a.ArchivedAt = ""
	a.UpdatedAt = time.Now().Format(time.RFC3339)
	interval := a.IntervalMinutes
	// Copy notify sources outside the lock window so the poller call below
	// doesn't reach into manager state.
	sources := append([]notifysource.Config(nil), a.NotifySources...)
	m.mu.Unlock()

	if expr := intervalToCron(interval, id); expr != "" {
		if err := m.cron.Schedule(id, expr); err != nil {
			m.logger.Warn("failed to re-schedule cron on unarchive", "agent", id, "err", err)
		}
	}
	if len(sources) > 0 {
		m.notifyPoller.RebuildSources(id, sources)
	}

	m.save()
	m.logger.Info("agent unarchived", "id", id)
	return nil
}

// Delete removes an agent and its data.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	_, ok := m.agents[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrAgentNotFound, id)
	}

	cleanup, err := m.acquireResetGuard(id)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := m.waitBusyClear(id); err != nil {
		return err
	}

	// Remove credentials and notify tokens outside lock (DB I/O)
	if m.creds != nil {
		if err := m.creds.DeleteAllForAgent(id); err != nil {
			return fmt.Errorf("delete credentials: %w", err)
		}
		if err := m.creds.DeleteTokensByAgent(id); err != nil {
			m.logger.Warn("failed to delete notify tokens", "agent", id, "err", err)
		}
	}

	m.cron.Remove(id)
	m.notifyPoller.RemoveAgent(id)
	m.cancelOneShots(id)

	// Remove agent from group DMs
	if m.groupdms != nil {
		m.groupdms.RemoveAgent(id)
	}

	// Close cached memory index before removing directory
	m.closeIndex(id)

	// Drop the per-agent PreCompact lock so the global map doesn't
	// accumulate dead entries over the process lifetime.
	dropAgentPreCompactLock(id)

	// Remove agent data directory (best-effort: credentials/cron/notify already cleaned up)
	dir := agentDir(id)
	if err := os.RemoveAll(dir); err != nil {
		m.logger.Warn("failed to remove agent dir", "agent", id, "err", err)
	}

	// Remove from in-memory map
	m.mu.Lock()
	delete(m.agents, id)
	m.mu.Unlock()

	m.save()
	m.logger.Info("agent deleted", "id", id)
	return nil
}

// ResetSession clears the CLI session (e.g. Claude JSONL) for an agent
// without deleting conversation history or memory. The next chat will start
// a fresh CLI session with the full system prompt re-injected.
func (m *Manager) ResetSession(agentID string) error {
	m.mu.Lock()
	a, ok := m.agents[agentID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrAgentNotFound, agentID)
	}
	tool := a.Tool
	m.mu.Unlock()

	// Block if agent is busy, editing, or being reset
	m.busyMu.Lock()
	if _, busy := m.busy[agentID]; busy {
		m.busyMu.Unlock()
		return ErrAgentBusy
	}
	if m.editing[agentID] {
		m.busyMu.Unlock()
		return fmt.Errorf("%w, try again later", ErrAgentBusy)
	}
	if m.resetting[agentID] {
		m.busyMu.Unlock()
		return fmt.Errorf("%w, try again later", ErrAgentBusy)
	}
	m.resetting[agentID] = true
	m.busyMu.Unlock()

	defer func() {
		m.busyMu.Lock()
		delete(m.resetting, agentID)
		m.busyMu.Unlock()
	}()

	switch tool {
	case "claude":
		clearClaudeSession(agentID)
	case "gemini":
		clearGeminiSession(agentID)
	case "custom":
		clearClaudeSession(agentID)
	}
	// Codex uses ephemeral sessions — no persistent state to clear

	m.logger.Info("CLI session reset", "agent", agentID, "tool", tool)
	return nil
}

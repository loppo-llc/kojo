package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
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
	m.lmStudio.ResetSession(id)

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

	// Block if agent is busy or being reset
	m.busyMu.Lock()
	if _, busy := m.busy[agentID]; busy {
		m.busyMu.Unlock()
		return ErrAgentBusy
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
	case "lm-studio":
		m.lmStudio.ResetSession(agentID)
		if m.lmsProxyPort > 0 {
			clearClaudeSession(agentID)
		}
	}
	// Codex uses ephemeral sessions — no persistent state to clear

	m.logger.Info("CLI session reset", "agent", agentID, "tool", tool)
	return nil
}

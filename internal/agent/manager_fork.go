package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ForkOptions controls what state is copied into the forked agent.
type ForkOptions struct {
	Name             string
	IncludeTranscript bool
}

// Fork creates a new agent by deep-copying the source agent's metadata and
// data files. Memory (MEMORY.md, memory/, persona, avatar) is always copied.
// Transcript (messages.jsonl) and its derived state (index/, autosummary marker,
// tasks.json) are copied only when IncludeTranscript is true.
//
// External integrations are intentionally NOT copied: SlackBot, NotifySources,
// and credentials all require per-agent tokens that cannot be safely shared.
// CLI local state (.claude/, .gemini/) is also skipped so the fork starts a
// fresh session. WorkDir is cleared so the fork does not share external output
// storage with the source.
//
// Known limitations: Manager.Update and the task API can write to persona.md /
// tasks.json without honoring the resetting flag, so the snapshot is not fully
// atomic against concurrent PATCH /agents/{id} or task mutations. The same
// looseness applies to Reset today.
func (m *Manager) Fork(srcID string, opts ForkOptions) (*Agent, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return nil, fmt.Errorf("name is required")
	}

	// Hold the source through a reset guard so concurrent chat/edit cannot
	// mutate its files while we copy. The source itself is not reset.
	cleanup, err := m.acquireResetGuard(srcID)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// acquireResetGuard cancels m.busy chats but not one-shot chats (Slack,
	// Discord, Group DM) which can keep writing to MEMORY.md/persona/index.
	// Cancel them explicitly and wait for the goroutines to finish winding
	// down before copying files.
	m.cancelOneShots(srcID)

	if err := m.waitBusyClear(srcID); err != nil {
		return nil, err
	}
	if err := m.waitOneShotClear(srcID); err != nil {
		return nil, err
	}

	// Snapshot the source under the map lock.
	m.mu.Lock()
	src, ok := m.agents[srcID]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, srcID)
	}
	srcCopy := copyAgent(src)
	m.mu.Unlock()

	// Build the forked agent metadata: keep persona/model/tool/etc., reset
	// identity and anything that binds to external systems.
	now := time.Now().Format(time.RFC3339)
	fork := copyAgent(srcCopy)
	fork.ID = generateID()
	fork.Name = opts.Name
	fork.CreatedAt = now
	fork.UpdatedAt = now
	fork.LastMessage = nil
	fork.HasAvatar = false
	fork.AvatarHash = ""
	fork.SlackBot = nil
	fork.NotifySources = nil
	fork.LegacyCronExpr = ""
	// Clear WorkDir so the fork does not share an external file storage
	// directory with the source (would cross-contaminate generated files).
	fork.WorkDir = ""

	// If we're going to copy the sqlite-backed index, close the source's
	// handle first so the files on disk are consistent. Skip when the
	// transcript is not copied — the index is not read in that case.
	if opts.IncludeTranscript {
		m.closeIndex(srcID)
	}

	srcDir := agentDir(srcID)
	dstDir := agentDir(fork.ID)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return nil, fmt.Errorf("create fork dir: %w", err)
	}

	// Remove the partially-populated fork directory if anything below fails
	// before the agent is fully registered.
	forkRegistered := false
	defer func() {
		if !forkRegistered {
			if err := os.RemoveAll(dstDir); err != nil {
				m.logger.Warn("failed to clean up partial fork dir", "dir", dstDir, "err", err)
			}
		}
	}()

	// Memory & persona — always copied.
	if err := copyFileIfExists(filepath.Join(srcDir, "persona.md"), filepath.Join(dstDir, "persona.md")); err != nil {
		return nil, fmt.Errorf("copy persona.md: %w", err)
	}
	if err := copyFileIfExists(filepath.Join(srcDir, "persona_summary.md"), filepath.Join(dstDir, "persona_summary.md")); err != nil {
		return nil, fmt.Errorf("copy persona_summary.md: %w", err)
	}
	if err := copyFileIfExists(filepath.Join(srcDir, "MEMORY.md"), filepath.Join(dstDir, "MEMORY.md")); err != nil {
		return nil, fmt.Errorf("copy MEMORY.md: %w", err)
	}
	if err := copyDirIfExists(filepath.Join(srcDir, "memory"), filepath.Join(dstDir, "memory")); err != nil {
		return nil, fmt.Errorf("copy memory/: %w", err)
	}

	// MEMORY.md may be absent on very old agents — ensure it exists so the
	// forked agent has somewhere to write.
	memPath := filepath.Join(dstDir, "MEMORY.md")
	if _, err := os.Stat(memPath); os.IsNotExist(err) {
		initial := fmt.Sprintf("# %s's Memory\n\nThis file stores persistent memories. Update it as you learn new things.\n", fork.Name)
		if err := os.WriteFile(memPath, []byte(initial), 0o644); err != nil {
			return nil, fmt.Errorf("init MEMORY.md: %w", err)
		}
	}

	// Ensure memory/ exists even if the source had no notes yet.
	if err := os.MkdirAll(filepath.Join(dstDir, "memory"), 0o755); err != nil {
		return nil, fmt.Errorf("ensure memory dir: %w", err)
	}

	// Avatar — copy the single avatar file (any extension) if present.
	if p := avatarFilePath(srcID); p != "" {
		if err := copyFileIfExists(p, filepath.Join(dstDir, filepath.Base(p))); err != nil {
			return nil, fmt.Errorf("copy avatar: %w", err)
		}
	}

	// Transcript & derived state — opt-in. Active todos travel with the
	// transcript because they describe ongoing work in that conversation.
	if opts.IncludeTranscript {
		if err := copyFileIfExists(filepath.Join(srcDir, messagesFile), filepath.Join(dstDir, messagesFile)); err != nil {
			return nil, fmt.Errorf("copy messages: %w", err)
		}
		if err := copyDirIfExists(filepath.Join(srcDir, indexDir), filepath.Join(dstDir, indexDir)); err != nil {
			return nil, fmt.Errorf("copy index: %w", err)
		}
		if err := copyFileIfExists(filepath.Join(srcDir, autoSummaryMarkerFile), filepath.Join(dstDir, autoSummaryMarkerFile)); err != nil {
			return nil, fmt.Errorf("copy autosummary marker: %w", err)
		}
		if err := copyFileIfExists(filepath.Join(srcDir, tasksFile), filepath.Join(dstDir, tasksFile)); err != nil {
			return nil, fmt.Errorf("copy tasks: %w", err)
		}
	}

	// Refresh avatar metadata on the in-memory fork now that the file is copied.
	has, hash := avatarMeta(fork.ID)
	applyAvatarMeta(fork, has, hash)

	// Seed LastMessage from the copied transcript so the list view reflects it.
	if opts.IncludeTranscript {
		if msgs, err := loadMessages(fork.ID, 1); err == nil && len(msgs) > 0 {
			last := msgs[len(msgs)-1]
			fork.LastMessage = &MessagePreview{
				Content:   truncatePreview(last.Content, 100),
				Role:      last.Role,
				Timestamp: last.Timestamp,
			}
		}
	}

	// Register & persist.
	m.mu.Lock()
	m.agents[fork.ID] = fork
	m.mu.Unlock()
	forkRegistered = true
	m.save()

	if expr := intervalToCron(fork.IntervalMinutes, fork.ID); expr != "" {
		if err := m.cron.Schedule(fork.ID, expr); err != nil {
			m.logger.Warn("failed to schedule cron for fork", "agent", fork.ID, "err", err)
		}
	}

	m.logger.Info("agent forked", "src", srcID, "id", fork.ID, "name", fork.Name, "includeTranscript", opts.IncludeTranscript)
	return copyAgent(fork), nil
}

// copyFileIfExists copies a regular file. Missing source is not an error.
// Symlinks are skipped so a malicious agent cannot exfiltrate data from
// outside its own directory by planting symlinks in its data dir.
func copyFileIfExists(src, dst string) error {
	li, err := os.Lstat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if li.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if !li.Mode().IsRegular() {
		return nil
	}

	in, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// copyDirIfExists recursively copies a directory. Missing source is not an error.
// Symlinks (both the top-level dir and any child entries) are skipped so we
// don't leak paths outside the agent dir.
func copyDirIfExists(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", src)
	}
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())
		// Use Lstat so we can skip symlinks without following them.
		li, err := os.Lstat(srcPath)
		if err != nil {
			return err
		}
		if li.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if e.IsDir() {
			if err := copyDirIfExists(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		if !li.Mode().IsRegular() {
			continue
		}
		if err := copyFileIfExists(srcPath, dstPath); err != nil {
			return err
		}
	}
	return nil
}

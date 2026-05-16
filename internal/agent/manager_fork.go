package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/store"
)

// ForkOptions controls what state is copied into the forked agent.
type ForkOptions struct {
	Name             string
	IncludeTranscript bool
}

// Fork creates a new agent by deep-copying the source agent's metadata and
// data files. Memory (MEMORY.md, memory/, persona, avatar) is always copied.
// The transcript (agent_messages) and its derived state — the on-disk
// memory index, the autosummary marker (kv: autosummary/<agentID>), and
// tasks (cloned via cloneAgentTasks against agent_tasks) — are copied
// only when IncludeTranscript is true. Tasks are cloned with fresh
// per-row ids so the destination's primary keys don't collide with
// the source's.
//
// External integrations are intentionally NOT copied: SlackBot, NotifySources,
// and credentials all require per-agent tokens that cannot be safely shared.
// CLI local state (.claude/, .gemini/) is also skipped so the fork starts a
// fresh session. WorkDir is cleared so the fork does not share external output
// storage with the source.
//
// Known limitations: Manager.Update and the task API can write to persona.md
// or agent_tasks without honoring the resetting flag, so the snapshot is not
// fully atomic against concurrent PATCH /agents/{id} or task mutations. The
// same looseness applies to Reset today.
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
	fork.LegacyIntervalMinutes = 0
	// Forking an archived agent must produce an *active* fork. Otherwise the
	// new agent would be born dormant and silently inherit ArchivedAt from
	// the source.
	fork.Archived = false
	fork.ArchivedAt = ""
	// Privilege is NEVER inherited. Forks start as regular agents; the
	// owner must explicitly grant privilege via the dedicated endpoint.
	fork.Privileged = false
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

	// Pre-register the fork's agents row so copyTranscript can satisfy
	// agent_messages' FK-on-agent_id check during AppendMessage. The
	// trailing m.save() would also write the row but happens after the
	// transcript replay; pre-registering closes the FK gap. The cleanup
	// defer tombstones this row on rollback so a failure between here
	// and final registration leaves no observable agent.
	if err := m.store.Upsert(fork); err != nil {
		return nil, fmt.Errorf("pre-register fork in store: %w", err)
	}

	// Remove the partially-populated fork directory and its auth token
	// if anything below fails before the agent is fully registered. The
	// DB row pre-registered above is tombstoned in the same defer so
	// the failure path leaves no observable agent.
	forkRegistered := false
	defer func() {
		if !forkRegistered {
			if err := m.store.Delete(fork.ID); err != nil {
				m.logger.Warn("failed to tombstone half-forked agent", "agent", fork.ID, "err", err)
			}
			if err := os.RemoveAll(dstDir); err != nil {
				m.logger.Warn("failed to clean up partial fork dir", "dir", dstDir, "err", err)
			}
			if m.tokenStore != nil {
				m.tokenStore.RemoveAgentToken(fork.ID)
			}
			// kv has no FK on agents, so a copyMarker that landed
			// before a later step failed leaves an orphan
			// autosummary row. Wipe it as part of the same
			// rollback that drops the dir + token; deleteMarker
			// is idempotent (silent on ErrNotFound) so it's
			// safe even when copyMarker never ran.
			deleteMarker(fork.ID, m.logger)
			// Same rationale for the avatar blob: blob_refs has
			// no FK on agents, so a SaveAvatar that landed for
			// the new fork before a later step failed would
			// leave an orphan blob keyed by the half-created
			// fork id. DeleteAvatar is idempotent (silent on
			// ErrNotFound across every probed extension) so it's
			// safe to call regardless of whether the avatar copy
			// reached SaveAvatar.
			if err := DeleteAvatar(m.blobStore, fork.ID); err != nil {
				m.logger.Warn("failed to clean up partial fork avatar", "agent", fork.ID, "err", err)
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
	// memory/recent.md mirrors the last conversation's pre-compaction
	// summary and is part of the transcript-derived short-term state,
	// not the agent's long-term memory. When the caller opts out of
	// transcript copy, drop this file so the fork starts with a clean
	// short-term context. The append-only daily diary files are kept —
	// they're the audit trail and not directly injected into prompts.
	if !opts.IncludeTranscript {
		if err := os.Remove(filepath.Join(dstDir, "memory", recentSummaryFile)); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("drop recent.md on fork: %w", err)
		}
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

	// Avatar — copy the single published blob (any extension) if
	// present. Phase 2c-2 slice 13 moved the avatar off the agent
	// dir onto blob.ScopeGlobal/agents/<id>/avatar.<ext>; the fork
	// re-publishes via SaveAvatar so the destination row goes
	// through the same Put path (etag, sha256, blob_refs row) as a
	// fresh upload. A nil blobStore (test fixture) silently skips
	// the avatar copy — matches the v0 behaviour where an absent
	// avatar file produced no error.
	if m.blobStore != nil {
		if ext, _, ok := resolveAvatarBlob(m.blobStore, srcID); ok {
			rc, _, err := m.blobStore.Get(blob.ScopeGlobal, avatarBlobPath(srcID, ext))
			if err != nil && !errors.Is(err, blob.ErrNotFound) {
				return nil, fmt.Errorf("copy avatar: open src: %w", err)
			}
			if err == nil {
				err := SaveAvatar(m.blobStore, fork.ID, rc, ext)
				rc.Close()
				if err != nil {
					return nil, fmt.Errorf("copy avatar: %w", err)
				}
			}
		}
	}

	// Transcript & derived state — opt-in. Active todos travel with the
	// transcript because they describe ongoing work in that conversation.
	//
	// The transcript itself lives in agent_messages: copyTranscript
	// replays every src row into the fork's per-agent seq sequence so
	// the fork starts as a deep copy. The remaining derived state
	// breaks down as: the FTS index dir is still on disk and is
	// rsync-style copied below; the autosummary marker has moved to kv
	// (autosummary/<agentID>) and is cloned via copyMarker; tasks live
	// in agent_tasks and are cloned via cloneAgentTasks.
	if opts.IncludeTranscript {
		if err := copyTranscript(srcID, fork.ID); err != nil {
			return nil, fmt.Errorf("copy messages: %w", err)
		}
		if err := copyDirIfExists(filepath.Join(srcDir, indexDir), filepath.Join(dstDir, indexDir)); err != nil {
			return nil, fmt.Errorf("copy index: %w", err)
		}
		// autosummary marker now lives in kv (Phase 2c-2 slice 9).
		// Read source via readMarker (kv first, legacy fallback so a
		// pre-migration source still copies cleanly) and write the
		// destination via writeMarker so it always lands in kv.
		// Surface destination-write failures up to Fork (matching
		// the v0 file-copy semantic — silently dropping the marker
		// would leave the new agent summarising from scratch and
		// the operator would never know).
		if err := copyMarker(srcID, fork.ID, m.logger); err != nil {
			return nil, fmt.Errorf("copy autosummary marker: %w", err)
		}
		if err := m.cloneAgentTasks(context.Background(), srcID, fork.ID); err != nil {
			return nil, fmt.Errorf("copy tasks: %w", err)
		}
	}

	// Refresh avatar metadata on the in-memory fork now that the
	// blob is copied.
	has, hash := m.avatarMeta(fork.ID)
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

	// Provision the fork's auth token BEFORE registering it. A token
	// failure here aborts the fork and triggers the dstDir cleanup
	// defer above, leaving the source untouched. Once registered, the
	// agent is reachable from cron / chat / Slack — a tokenless agent
	// would silently lose self-API access at that point.
	if m.tokenStore != nil {
		if err := m.tokenStore.EnsureAgentToken(fork.ID); err != nil {
			return nil, fmt.Errorf("provision fork token: %w", err)
		}
	}

	// Register & persist.
	m.mu.Lock()
	m.agents[fork.ID] = fork
	m.mu.Unlock()
	forkRegistered = true
	m.save()

	// Sync the freshly-copied MEMORY.md and memory/ tree into the
	// fork's DB rows so cross-device readers see the forked content
	// immediately. m.save() above persists the agent record, which
	// is what UpsertAgentMemory's parent-existence check needs.
	// Best-effort — a sync failure here is logged but doesn't abort
	// the fork (the file copies on disk are the canonical state
	// regardless; the next sync hook will reconcile).
	if st := getGlobalStore(); st != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := SyncAgentMemoryFromDisk(ctx, st, fork.ID, m.logger); err != nil {
			m.logger.Warn("memory sync after fork failed", "agent", fork.ID, "err", err)
		}
		cancel()
	}

	if expr := fork.CronExpr; expr != "" {
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

// copyTranscript replays every live message from srcID into dstID via
// AppendMessage so the fork's transcript starts as a deep copy. seq is
// re-allocated per-agent on the destination — preserving src seq would
// require InsertOptions.Seq, but per-agent seq is monotonic-from-1 by
// design and the fork's seq space starts fresh.
//
// New message IDs are minted for the fork because v0's m_xxxxxxxx ids
// are random hex with collision probability negligible at the per-agent
// level but global-uniqueness is enforced by the agent_messages PRIMARY
// KEY: reusing src ids would conflict.
func copyTranscript(srcID, dstID string) error {
	db := getGlobalStore()
	if db == nil {
		return errStoreNotReady
	}
	ctx, cancel := transcriptCtx()
	defer cancel()

	recs, err := db.ListMessages(ctx, srcID, store.MessageListOptions{Order: "asc"})
	if err != nil {
		return err
	}
	for _, src := range recs {
		dst := *src
		dst.ID = generateMessageID()
		dst.AgentID = dstID
		dst.Seq = 0       // reallocate
		dst.Version = 0   // reset
		dst.ETag = ""     // recompute
		dst.PeerID = ""   // local fork
		dst.DeletedAt = nil
		if _, err := db.AppendMessage(ctx, &dst, store.MessageInsertOptions{
			CreatedAt: src.CreatedAt,
			UpdatedAt: src.UpdatedAt,
		}); err != nil {
			return fmt.Errorf("append fork message %s: %w", dst.ID, err)
		}
	}
	return nil
}

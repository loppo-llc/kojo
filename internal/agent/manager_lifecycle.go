package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/notifysource"
	"github.com/loppo-llc/kojo/internal/store"
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

	// Truncate the DB transcript. The per-agent kojo.db row is the v1
	// canonical store; the legacy messages.jsonl file stays untouched
	// (it lives in v0 dirs only). truncateMessagesTo(id, 0) tombstones
	// every live row in one TruncateMessagesAfterSeq call.
	if err := truncateMessagesTo(id, 0); err != nil && !errors.Is(err, ErrMessageNotFound) {
		m.logger.Warn("reset: failed to truncate transcript", "agent", id, "err", err)
	}

	// Hold the per-agent memory-sync lock around the entire on-
	// disk wipe + recreate + post-reset sync so concurrent CRUD /
	// sync helpers can't observe a half-reset tree (memory/ wiped
	// but MEMORY.md still present) or a fresh tree whose DB rows
	// haven't been reconciled yet. The release lives at the end
	// of the function (or before the early-return on recreate
	// failure) so the lock spans the full critical region.
	releaseSync := lockMemorySync(id)

	// Remove memory files. The entire memory/ subtree is wiped —
	// daily diaries (memory/YYYY-MM-DD.md), recent.md, project /
	// topic / people / archive notes — along with MEMORY.md
	// itself. ResetData's contract (function doc) is "remove
	// conversation logs and memory but keep settings, persona,
	// avatar, credentials"; memory.* counts as memory.
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

	// Drop persistent tasks (DB-backed). Concurrent task API calls go
	// through the same store with etag/If-Match guards, so we don't need
	// the old per-agent file-mutex — DeleteAllAgentTasks runs in a single
	// SQL DELETE statement and any in-flight CreateAgentTask either
	// commits before us (and is wiped) or after us (and stands as a new
	// post-reset task, which is the same window we already accept for
	// every other reset step).
	m.DeleteAllTasks(context.Background(), id)

	// Remove auto-summary marker (kv row + legacy file). Phase 2c-2
	// slice 9: kv is canonical; deleteMarker also unlinks the legacy
	// path opportunistically so a pre-migration install gets cleaned
	// up on its first reset.
	deleteMarker(id, m.logger)

	// (Note: an explicit os.Remove(memory/recent.md) used to live
	// here — it was redundant because the RemoveAll above wipes the
	// whole memory/ subtree. Removed in Phase 2c-2 slice 16; the
	// stale comment that claimed daily diaries were retained as an
	// audit trail was wrong — the RemoveAll has been wiping them
	// since long before slice 16.)

	// Drop cron throttle row (kv) + best-effort unlink legacy file.
	// Phase 2c-2 slice 12: the throttle is canonically stored at
	// (namespace="scheduler", key="cron_last/<id>", scope=machine).
	// Without this drop, a re-created agent inheriting the same id
	// would see its first cron tick artificially throttled by the
	// prior incarnation's last-fire timestamp.
	if err := deleteCronLockDB(m.Store(), id); err != nil {
		m.logger.Warn("reset: failed to delete cron lock kv row", "agent", id, "err", err)
	}
	removeLegacyCronLock(id)

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

	// Recreate empty memory directory and MEMORY.md (required for agent to function).
	// Capture the error rather than returning so we always release
	// the memory-sync lock acquired above.
	var recreateErr error
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o755); err != nil {
		recreateErr = fmt.Errorf("recreate memory dir: %w", err)
	} else {
		initial := fmt.Sprintf("# %s's Memory\n\nThis file stores persistent memories. Update it as you learn new things.\n", name)
		if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(initial), 0o644); err != nil {
			recreateErr = fmt.Errorf("recreate MEMORY.md: %w", err)
		}
	}
	// Bail BEFORE post-reset sync (and lock release) on recreate
	// failure — the sync would race with the still-broken state.
	if recreateErr != nil {
		releaseSync()
		return recreateErr
	}

	// Clear last message preview
	m.mu.Lock()
	if a, ok := m.agents[id]; ok {
		a.LastMessage = nil
		a.UpdatedAt = time.Now().Format(time.RFC3339)
	}
	m.mu.Unlock()

	m.save()

	// Sync the freshly-recreated MEMORY.md (and the now-empty
	// memory/ tree) into the DB. Without this, a cross-device
	// reader observing agent_memory between Reset and the next
	// chat would still see the pre-reset body — and the
	// memory_entries that ResetData wiped on disk would still
	// appear live in the DB until the next sync hook (next chat,
	// next restart). Best-effort: a sync failure logs but doesn't
	// fail the reset; the file is canonical and the next sync
	// hook reconciles.
	//
	// We hold memorySyncMu (acquired above) THROUGH this sync by
	// calling syncAgentMemoryHeld (the lock-not-held variant)
	// rather than the public SyncAgentMemoryFromDisk (which takes
	// the lock and would deadlock). Holding the lock for the full
	// reset → sync window means a concurrent CRUD path that
	// acquires the same lock waits until both DB rows AND the disk
	// match the post-reset state, eliminating the stale-read window
	// that release-before-sync would expose.
	// Post-reset sync timeout: large memory_entries trees (10k+
	// rows) generate one tombstone UPDATE per orphaned row. 5s is
	// too tight at that scale — a timeout would leave pre-reset
	// rows live in the DB after we release the reset guard, which
	// the read paths would then expose. 60s is generous; the actual
	// ops run a single SQL statement per row plus the disk walk,
	// so wall-clock dominates.
	if st := getGlobalStore(); st != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		if err := syncAgentMemoryHeld(ctx, st, id, m.logger); err != nil {
			m.logger.Warn("memory sync after reset failed", "agent", id, "err", err)
		}
		cancel()
	}
	releaseSync()

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
//
// Takes Manager.LockPatch to serialize against in-flight PATCH-style
// mutations (notably handleOAuth2Callback's source-exists-check +
// token-save + Enable trio). Without it, an Archive that landed mid-
// callback could leave us with persisted OAuth tokens against an
// archived source — callers see a "success" HTML page for an agent
// that's already retired. Lock order is LockPatch → acquireResetGuard
// → m.mu; PATCH handlers already take LockPatch first too.
func (m *Manager) Archive(id string) error {
	release := m.LockPatch(id)
	defer release()

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
	// LockPatch parity with Archive/Delete — keeps the per-agent
	// lifecycle ordering consistent across all three transitions
	// even though Unarchive doesn't itself race with the OAuth
	// callback in the same way (it can't make a deleted source
	// reappear). Cheap to take; avoids future surprises if a new
	// caller starts depending on the lock covering Unarchive too.
	release := m.LockPatch(id)
	defer release()

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
	expr := a.CronExpr
	// Copy notify sources outside the lock window so the poller call below
	// doesn't reach into manager state.
	sources := append([]notifysource.Config(nil), a.NotifySources...)
	m.mu.Unlock()

	if expr != "" {
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
//
// Takes Manager.LockPatch to serialize against in-flight PATCH-style
// mutations (see Archive's comment for the OAuth callback rationale).
// Lock order: LockPatch → acquireResetGuard → m.mu.
func (m *Manager) Delete(id string) error {
	release := m.LockPatch(id)
	defer release()

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

	// Tombstone the DB row first. This is the "point of no return": once
	// the row is gone the agent is invisible to the API, change feed, and
	// peer mirrors. Failing tombstone aborts before any destructive
	// cleanup so the user can retry without a half-deleted agent.
	//
	// Past this point cleanup is warn-only and the in-memory map MUST
	// still be drained: returning early after the tombstone would leave
	// the agent live in m.agents but dead in the DB — split-brain that
	// the next API request would surface as a stale row.
	if err := m.store.Delete(id); err != nil {
		return fmt.Errorf("tombstone agent in store: %w", err)
	}

	// Remove credentials and notify tokens outside lock (DB I/O). After
	// the tombstone these are best-effort orphan cleanup; the eventual
	// operator-driven hard-delete pass (planned `--clean` target; not
	// yet implemented) rescues the leftovers if either call fails here.
	if m.creds != nil {
		if err := m.creds.DeleteAllForAgent(id); err != nil {
			m.logger.Warn("failed to delete credentials post-tombstone", "agent", id, "err", err)
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

	// Wipe the autosummary marker (kv + legacy file). The kv table
	// has no FK to agents, so without an explicit DELETE here the
	// row would survive agent deletion as an orphan and re-collide
	// on a future agent reusing the same id.
	deleteMarker(id, m.logger)

	// Drop the cron throttle row (kv) so a re-created agent
	// reusing this id doesn't inherit a stale 50s window. Same
	// orphan-row reasoning as deleteMarker above (the kv table
	// is FK-less by design).
	if err := deleteCronLockDB(m.Store(), id); err != nil {
		m.logger.Warn("failed to delete cron lock kv row", "agent", id, "err", err)
	}

	// Drop avatar blob(s) so a re-created agent reusing this id
	// doesn't inherit a stale image. Same FK-less rationale: the
	// blob_refs row carries the URI, no schema-level cascade ties
	// it to agents.
	if err := DeleteAvatar(m.blobStore, id); err != nil {
		m.logger.Warn("failed to delete avatar blob", "agent", id, "err", err)
	}
	// avatarMu entry intentionally retained — see the avatarMu
	// comment in avatar.go for the rationale (removing while
	// waiters sit on the old mutex breaks the serialization
	// invariant; the leak is bounded by total ids ever created).

	// Remove agent data directory (best-effort: credentials/cron/notify already cleaned up)
	dir := agentDir(id)
	if err := os.RemoveAll(dir); err != nil {
		m.logger.Warn("failed to remove agent dir", "agent", id, "err", err)
	}

	// Drop auth token before removing from the in-memory map so a
	// concurrent request can't temporarily resolve to a deleted agent.
	if m.tokenStore != nil {
		m.tokenStore.RemoveAgentToken(id)
	}

	// Remove from in-memory map. The DB row was already tombstoned above
	// so a concurrent Save() carrying a stale snapshot won't accidentally
	// resurrect this agent — UpdateAgent fails on a tombstoned row and
	// InsertAgent on an existing one (live or not).
	m.mu.Lock()
	delete(m.agents, id)
	m.mu.Unlock()

	m.logger.Info("agent deleted", "id", id)
	return nil
}

// ReloadAgentFromStore re-reads the agent row from kojo.db and
// installs (or replaces) the in-memory cached *Agent. Used by
// the §3.7 device-switch agent-sync hook so a peer-pushed row
// becomes immediately visible to chat / list / fork without a
// daemon restart.
//
// Caller MUST have already committed the row via
// store.SyncAgentFromPeer (or equivalent) before calling. A
// store.ErrNotFound result here is surfaced verbatim so the
// hook can decide whether to log + skip or refuse the sync.
func (m *Manager) ReloadAgentFromStore(agentID string) error {
	if m == nil {
		return fmt.Errorf("agent.Manager.ReloadAgentFromStore: nil receiver")
	}
	if agentID == "" {
		return fmt.Errorf("agent.Manager.ReloadAgentFromStore: agent_id required")
	}
	a, err := m.store.LoadByID(agentID)
	if err != nil {
		return err
	}
	has, hash := m.avatarMeta(agentID)
	applyAvatarMeta(a, has, hash)
	if msgs, mErr := loadMessages(agentID, 1); mErr == nil && len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		a.LastMessage = &MessagePreview{
			Content:   truncatePreview(last.Content, 100),
			Role:      last.Role,
			Timestamp: last.Timestamp,
		}
	}
	m.mu.Lock()
	m.agents[agentID] = a
	m.mu.Unlock()

	if m.logger != nil {
		m.logger.Info("agent reloaded from store", "agent", agentID)
	}
	return nil
}

// ActivateAgentRuntime wires the in-memory cached *Agent into this peer's
// runtime side channels: cron schedule and notify poller. Called by the
// §3.7 device-switch finalize hook (NOT by phase-1 sync) so an agent that
// landed but hasn't yet been claimed by this peer can't fire schedules
// before AgentLockGuard adopts it.
//
// Idempotent and threadsafe. Skipped (no-op) when StartSchedulers hasn't
// run yet — the boot loop registers everything from the same in-memory
// snapshot, so a redundant call here would just race.
//
// Archived agents always have their cron entry and notify sources torn
// down here, even when they were already detached by Archive(): a Reload
// that flipped an active agent to archived underneath us must still
// surface as "stopped" on the runtime side.
func (m *Manager) ActivateAgentRuntime(agentID string) {
	if m == nil || agentID == "" {
		return
	}
	if !m.schedulersStarted {
		return
	}
	a, ok := m.Get(agentID)
	if !ok {
		return
	}
	if a.Archived {
		if m.cron != nil {
			m.cron.Remove(agentID)
		}
		if m.notifyPoller != nil {
			m.notifyPoller.DetachAgent(agentID)
		}
		return
	}
	if m.cron != nil {
		if a.CronExpr != "" {
			if err := m.cron.Schedule(agentID, a.CronExpr); err != nil && m.logger != nil {
				m.logger.Warn("failed to schedule cron after activate", "agent", agentID, "err", err)
			}
		} else {
			m.cron.Remove(agentID)
		}
	}
	if m.notifyPoller != nil {
		m.notifyPoller.RebuildSources(agentID, a.NotifySources)
	}

	// §3.7 device-switch skill: install the SKILL.md so the newly-
	// arrived agent can initiate another switch from this peer.
	// Without this the skill file only exists at prepareChat time,
	// so a fresh arrival that hasn't chatted yet would not expose
	// the /kojo-switch-device slash command. claude/custom only —
	// other backends never create a .claude/ tree.
	if a.Tool == "claude" || a.Tool == "custom" {
		SyncDeviceSwitchSkill(agentID, a.IsDeviceSwitchEnabled(), m.logger)
	}
}

// arrivalNotified deduplicates NotifyDeviceSwitchArrival across
// finalize retries for ONE specific switch attempt. A finalize hook
// failure (e.g. transient kv commit error) causes the orchestrator
// to retry, which re-fires the hook including
// NotifyDeviceSwitchArrival. Without dedup, the agent would receive
// duplicate "device switch complete" system messages for the same
// op.
//
// Keyed by (agentID, opID) so a SUBSEQUENT switch back to this peer
// (different opID) is NOT deduped — earlier versions keyed by
// agentID alone, never cleared the entry, and silently skipped every
// switch-back-to-this-peer beyond the first one for the daemon's
// lifetime. The chat would fire on switch #1 but the auto-continue
// would be missing on switches #2, #3, … leaving claude with no
// input to resume from. Same-op dedup is what we actually want; the
// agentID-only key was overly broad.
var arrivalNotified sync.Map

type arrivalDedupKey struct {
	agentID string
	opID    string
}

// NotifyDeviceSwitchArrival sends a system message to the agent so
// it can immediately resume work on this peer after a device switch.
// Runs async — the caller doesn't need to wait for the chat to
// finish. If the agent is already busy (e.g. a cron tick landed at
// the same instant) the system message is skipped silently.
//
// Idempotent across finalize retries for the SAME opID; a fresh
// opID (a future switch back to this peer for the same agent) fires
// its own arrival chat. opID may be empty for legacy / test callers,
// in which case the dedup keys on agentID alone — accept that the
// retry-safety degrades to old behavior rather than impose an opID
// requirement on every caller.
func (m *Manager) NotifyDeviceSwitchArrival(agentID, sourcePeerName, opID string) {
	if m == nil || agentID == "" {
		return
	}
	key := arrivalDedupKey{agentID: agentID, opID: opID}
	if _, dup := arrivalNotified.LoadOrStore(key, struct{}{}); dup {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		prompt := "デバイス転移完了。転移元: " + sourcePeerName + "。このデバイスで作業を継続してください。直前の会話の流れを確認し、中断された作業があれば再開してください。"
		events, err := m.Chat(ctx, agentID, prompt, "system", nil, BusySourceNotification)
		if err != nil {
			// Clear dedup so a finalize retry for the SAME op can
			// fire — the chat never landed, retry is benign.
			arrivalNotified.Delete(key)
			if m.logger != nil {
				m.logger.Info("device-switch arrival chat skipped", "agent", agentID, "op_id", opID, "err", err)
			}
			return
		}
		// Drain events — the chat runs to completion in the
		// background. Transcript is persisted via the normal
		// processChatEvents path. The dedup entry stays in place
		// so a manual finalize retry days later doesn't re-fire
		// the arrival chat for the same op; a subsequent switch
		// with a fresh opID is unaffected.
		for range events {
		}
		if m.logger != nil {
			m.logger.Info("device-switch arrival chat completed", "agent", agentID, "op_id", opID)
		}
	}()
}

// kvReleasedNamespace + kvReleasedKey: machine-scoped kv row that
// signals "this peer released agent <id> as the §3.7 source." It
// outlives daemon restarts so EvictNonLocalAgentsAtStartup can
// distinguish a switched-AWAY agent (durable marker → evict) from
// an incoming-TARGET agent whose phase-1 sync landed but whose
// finalize hasn't fired yet (no marker → keep, AgentLockGuard +
// fencing middleware handle the in-flight state). Without the
// marker, eviction would have to rely on agent_locks.holder !=
// self alone, which is ambiguous between the two cases.
//
// Cleared by onAgentSyncFinalized (cmd/kojo wiring) when a future
// switch hands the agent BACK to this peer, so a release-then-
// rehandoff cycle doesn't leave a stale marker that would evict
// the freshly re-arrived agent on the next restart.
const kvReleasedNamespace = "handoff"

func kvReleasedKey(agentID string) string { return "released/" + agentID }

// kvArrivedKey is the durable "this peer hosts agent <id>" marker
// written on a successful agent-sync finalize. Survives graceful
// shutdown (AgentLockGuard.Stop deletes agent_locks rows via
// ReleaseAgentLockByPeer, so locks alone cannot seed the guard on
// the next restart). Cleared on source-release so a future switch-
// away from this peer doesn't leave a stale arrival marker that
// would re-seed a phantom agent into the lock guard.
func kvArrivedKey(agentID string) string { return "arrived/" + agentID }

// MarkAgentReleasedHere persists the §3.7 source-release marker for
// agentID in the machine-scoped kv table. The value is the release
// timestamp (RFC3339); the value isn't consulted by the eviction
// path, only the row's existence is. Idempotent.
//
// Deadline-driven retry: loops on PutKV failure with constant 100ms
// (markReleasedBackoff) sleeps between attempts until the caller's
// ctx expires. A single PutKV failure is almost always transient
// (SQLite BUSY under contention, brief disk pressure). The marker is
// critical: without it on disk, a daemon restart on the source side
// would resurrect schedulers / cron / notify-poller for an agent
// target now owns. The caller (releaseAgentLocallyCore) hands in a
// markReleasedCtxBudget-wide ctx so transient SQLite BUSY gets many
// shots inside that window.
func (m *Manager) MarkAgentReleasedHere(ctx context.Context, agentID string) error {
	if m == nil || agentID == "" {
		return nil
	}
	st := m.Store()
	if st == nil {
		return nil
	}
	rec := &store.KVRecord{
		Namespace: kvReleasedNamespace,
		Key:       kvReleasedKey(agentID),
		Value:     time.Now().UTC().Format(time.RFC3339Nano),
		Type:      store.KVTypeString,
		Scope:     store.KVScopeMachine,
	}
	// Deadline-driven retry: keep trying until the caller's ctx
	// expires. Constant 100ms backoff between attempts so quick-
	// failing transient errors (modernc.org/sqlite SQLITE_BUSY
	// snapshots) get many shots inside the budget instead of the
	// prior 5-attempt cap that gave up after ~400ms even when the
	// caller had seconds of headroom left.
	var (
		lastErr  error
		attempts int
	)
	for {
		attempts++
		_, err := st.PutKV(ctx, rec, store.KVPutOptions{})
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("MarkAgentReleasedHere: %w after %d attempts (last: %v)",
				ctx.Err(), attempts, lastErr)
		case <-time.After(markReleasedBackoff):
		}
	}
}

// markReleasedBackoff is the constant sleep between retry attempts.
// 100ms keeps the loop responsive (many attempts inside a 3s budget)
// while leaving SQLite room to clear a brief writer-lock contention
// before we hammer it again.
const markReleasedBackoff = 100 * time.Millisecond

// markReleasedCtxBudget bounds the deadline-driven retry loop in
// MarkAgentReleasedHere. 3s gives ~30 PutKV attempts at 100ms
// constant backoff — wide enough that a brief SQLite writer-lock
// contention burst clears inside the budget, narrow enough that
// the orchestrator switch latency doesn't balloon on a truly
// stuck DB.
const markReleasedCtxBudget = 3 * time.Second

// MarkAgentArrivedHere persists the §3.7 target-arrival marker for
// agentID. Called by the finalize hook after token adopt + guard
// AddAgent succeed; the marker survives graceful shutdown (unlike
// agent_locks rows, which AgentLockGuard.Stop drops via
// ReleaseAgentLockByPeer) so a daemon restart can re-seed the
// guard's desired set. Idempotent; deadline-driven retry mirrors
// MarkAgentReleasedHere's durability story.
func (m *Manager) MarkAgentArrivedHere(ctx context.Context, agentID string) error {
	if m == nil || agentID == "" {
		return nil
	}
	st := m.Store()
	if st == nil {
		return nil
	}
	rec := &store.KVRecord{
		Namespace: kvReleasedNamespace,
		Key:       kvArrivedKey(agentID),
		Value:     time.Now().UTC().Format(time.RFC3339Nano),
		Type:      store.KVTypeString,
		Scope:     store.KVScopeMachine,
	}
	var (
		lastErr  error
		attempts int
	)
	for {
		attempts++
		_, err := st.PutKV(ctx, rec, store.KVPutOptions{})
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("MarkAgentArrivedHere: %w after %d attempts (last: %v)",
				ctx.Err(), attempts, lastErr)
		case <-time.After(markReleasedBackoff):
		}
	}
}

// ClearAgentArrivedHere removes the §3.7 target-arrival marker.
// Called by the source-release path so a switch-away from this
// peer doesn't leave a stale arrival marker that would re-seed
// AgentLockGuard against an agent we no longer own. Idempotent;
// missing row is not an error.
func (m *Manager) ClearAgentArrivedHere(ctx context.Context, agentID string) error {
	if m == nil || agentID == "" {
		return nil
	}
	st := m.Store()
	if st == nil {
		return nil
	}
	err := st.DeleteKV(ctx, kvReleasedNamespace, kvArrivedKey(agentID), "")
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

// ListArrivedAgents returns every agent_id whose LATEST handoff
// marker (by Value-encoded RFC3339Nano timestamp) is an arrival.
// "Latest wins" — rather than strict "released wins" — so a
// re-handoff back to this peer whose earlier ClearAgentReleasedHere
// transiently failed isn't permanently stranded: the new arrival's
// timestamp dominates the stale release marker even when the
// release marker is still on disk.
//
// Used by --peer startup to seed AgentLockGuard.Start. Errors
// propagate so the caller can log a degraded seed.
func (m *Manager) ListArrivedAgents(ctx context.Context) ([]string, error) {
	if m == nil {
		return nil, nil
	}
	st := m.Store()
	if st == nil {
		return nil, nil
	}
	recs, err := st.ListKV(ctx, kvReleasedNamespace)
	if err != nil {
		return nil, err
	}
	latest := latestHandoffMarkers(recs)
	out := make([]string, 0, len(latest))
	for id, kind := range latest {
		if kind == markerArrived {
			out = append(out, id)
		}
	}
	return out, nil
}

// markerKind enumerates the handoff lifecycle markers that share
// the "handoff" kv namespace.
type markerKind int

const (
	markerArrived markerKind = iota + 1
	markerReleased
)

// latestHandoffMarkers walks the (assumed-unordered) recs and
// returns the LATEST marker per agent_id, comparing the Value
// field as RFC3339Nano timestamps (nanosecond precision). A row
// that fails to parse falls back to UpdatedAt (kv writes it as
// millis); the fallback is widened to nanoseconds for comparison
// so the unit is uniform. Same fallback covers a malformed write
// from a future revision that drops the timestamp. Rows whose
// key prefix doesn't match a known marker are skipped silently —
// the kv namespace may host other handoff/* artefacts later.
//
// Tie-break: when two markers for the same id share an identical
// timestamp (millisecond-aligned UpdatedAt fallback, or a coarse
// clock that hands out the same RFC3339Nano string for both
// writes), arrival wins. Rationale: keeping the agent local is
// the recoverable outcome — a stranded released marker leaves
// the agent unreachable until operator intervention.
func latestHandoffMarkers(recs []*store.KVRecord) map[string]markerKind {
	const (
		arrivedPrefix  = "arrived/"
		releasedPrefix = "released/"
	)
	type entry struct {
		kind markerKind
		t    int64 // wall-clock nanos; larger wins, arrival breaks ties
	}
	best := make(map[string]entry, len(recs))
	for _, r := range recs {
		if r == nil {
			continue
		}
		var id string
		var k markerKind
		switch {
		case strings.HasPrefix(r.Key, arrivedPrefix):
			id = r.Key[len(arrivedPrefix):]
			k = markerArrived
		case strings.HasPrefix(r.Key, releasedPrefix):
			id = r.Key[len(releasedPrefix):]
			k = markerReleased
		default:
			continue
		}
		if id == "" {
			continue
		}
		t := r.UpdatedAt * int64(time.Millisecond)
		if r.Value != "" {
			if parsed, perr := time.Parse(time.RFC3339Nano, r.Value); perr == nil {
				t = parsed.UnixNano()
			}
		}
		cur, ok := best[id]
		switch {
		case !ok:
			best[id] = entry{kind: k, t: t}
		case t > cur.t:
			best[id] = entry{kind: k, t: t}
		case t == cur.t && k == markerArrived && cur.kind == markerReleased:
			// Tie-break: arrival wins on equal timestamps.
			best[id] = entry{kind: k, t: t}
		}
	}
	out := make(map[string]markerKind, len(best))
	for id, e := range best {
		out[id] = e.kind
	}
	return out
}

// ListReleasedAgents returns every agent_id whose LATEST handoff
// marker is a release. Mirrors ListArrivedAgents's latest-wins
// semantic: a stale release marker that an outdated arrival
// preceded is NOT returned. Used by --peer startup to subtract
// "still-released" IDs from the agent_locks seed; an agent whose
// latest marker is arrival passes the filter even when a stale
// released/<id> row is still on disk.
func (m *Manager) ListReleasedAgents(ctx context.Context) (map[string]struct{}, error) {
	if m == nil {
		return nil, nil
	}
	st := m.Store()
	if st == nil {
		return nil, nil
	}
	recs, err := st.ListKV(ctx, kvReleasedNamespace)
	if err != nil {
		return nil, err
	}
	latest := latestHandoffMarkers(recs)
	out := make(map[string]struct{}, len(latest))
	for id, kind := range latest {
		if kind == markerReleased {
			out[id] = struct{}{}
		}
	}
	return out, nil
}

// ClearAgentReleasedHere removes the §3.7 source-release marker.
// Called by the finalize hook on a re-handoff so the agent doesn't
// get evicted on the next restart. Idempotent; missing row is not
// an error.
func (m *Manager) ClearAgentReleasedHere(ctx context.Context, agentID string) error {
	if m == nil || agentID == "" {
		return nil
	}
	st := m.Store()
	if st == nil {
		return nil
	}
	err := st.DeleteKV(ctx, kvReleasedNamespace, kvReleasedKey(agentID), "")
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

// agentReleasedHereState reports whether the agent's LATEST handoff
// marker is a release (latest-wins, by Value-encoded RFC3339Nano
// timestamp). A stale released/<id> with a newer arrived/<id>
// returns (false, nil) so a re-handoff back to this peer isn't
// evicted by a leftover marker. ErrNotFound on the released row
// (no release ever recorded) returns (false, nil). Any OTHER read
// error returns (false, err) — caller logs and fails open.
func (m *Manager) agentReleasedHereState(ctx context.Context, agentID string) (bool, error) {
	if m == nil || agentID == "" {
		return false, nil
	}
	st := m.Store()
	if st == nil {
		return false, nil
	}
	released, rerr := st.GetKV(ctx, kvReleasedNamespace, kvReleasedKey(agentID))
	if errors.Is(rerr, store.ErrNotFound) {
		return false, nil
	}
	if rerr != nil {
		return false, rerr
	}
	// Compare against the arrived marker if any; arrival absent
	// means released is the only signal and wins by default.
	arrived, aerr := st.GetKV(ctx, kvReleasedNamespace, kvArrivedKey(agentID))
	if errors.Is(aerr, store.ErrNotFound) {
		return true, nil
	}
	if aerr != nil {
		return false, aerr
	}
	rTime := markerTimestamp(released)
	aTime := markerTimestamp(arrived)
	// Strict > so equal timestamps (same-millis writes that
	// share an RFC3339Nano string, or a fallback to UpdatedAt
	// granularity) keep the agent local — mirrors the tie-break
	// in latestHandoffMarkers.
	return rTime > aTime, nil
}

// markerTimestamp extracts the RFC3339Nano timestamp from a kv
// marker's Value field, falling back to UpdatedAt for legacy or
// future rows that omit it. Returns wall-clock nanoseconds since
// epoch — UpdatedAt is stored as millis, widened here so the
// comparison unit is uniform with parsed RFC3339Nano.
func markerTimestamp(rec *store.KVRecord) int64 {
	if rec == nil {
		return 0
	}
	if rec.Value != "" {
		if t, err := time.Parse(time.RFC3339Nano, rec.Value); err == nil {
			return t.UnixNano()
		}
	}
	return rec.UpdatedAt * int64(time.Millisecond)
}

// ReleaseAgentLocally tears down every in-memory side channel that
// could fire local writes for the agent on THIS peer: cron schedule,
// notify poller, in-flight one-shots, cached memory index, group-DM
// membership, and the agents map entry itself. The on-disk DB rows
// + persona / blob bodies remain so a future agent-sync back to this
// peer can re-hydrate via ReloadAgentFromStore.
//
// Called by the §3.7 source-release hook after a successful complete
// + finalize: target now holds agent_locks, and source must stop
// firing cron / poller / internal Chat against this agentID. Without
// this teardown, source's cron scheduler keeps the entry and
// runCronJob proceeds through the agents.Get hit; the resulting Chat
// would write transcript / JSONL that target never sees.
//
// Safe to call multiple times — every step is no-op-on-missing.
// Threadsafe.
func (m *Manager) ReleaseAgentLocally(agentID string) {
	// withDBCleanup=true: notify_cursors and group_dm_members rows
	// are deleted from this peer's kojo.db. Semantically correct
	// for a §3.7 source-release — the agent's runtime is moving
	// to target, so the source's local cursors / memberships
	// belong to the prior incarnation and have no surviving
	// consumer here.
	m.releaseAgentLocallyCore(agentID, true /*withDBCleanup*/, true /*markReleased*/)
}

// detachAgentInMemory is the no-DB-cleanup variant used by
// EvictNonLocalAgentsAtStartup. The marker that triggered eviction
// implies a prior ReleaseAgentLocally already ran (with full
// cleanup), so we only need to tear down the in-memory side
// channels that NewManager just rebuilt on this fresh boot. We do
// NOT re-issue notifyPoller.RemoveAgent / groupdms.RemoveAgent
// because their DB-row deletes have already happened, and a stale-
// marker reconciliation case (operator-driven re-handoff) where the
// guard above lets us through would re-do destructive deletes
// against rows that may legitimately belong to the (now re-arrived)
// agent.
func (m *Manager) detachAgentInMemory(agentID string) {
	m.releaseAgentLocallyCore(agentID, false /*withDBCleanup*/, false /*markReleased*/)
}

func (m *Manager) releaseAgentLocallyCore(agentID string, withDBCleanup, markReleased bool) {
	if m == nil || agentID == "" {
		return
	}
	// Persist the marker FIRST when this is a real release. The
	// runtime teardown that follows is "best-effort cleanup" —
	// even if it half-fires, the agent stays evicted via the
	// startup eviction path on the next boot AS LONG AS the
	// marker is on disk. Reverse order (teardown → mark) opens a
	// window where a crash between the two steps leaves the
	// runtime quiet now but resurrects on restart.
	//
	// Retried + bounded inside MarkAgentReleasedHere. On
	// exhaustion we log ERROR (operator-visible) and PROCEED with
	// teardown — refusing to tear down here would leave source
	// continuing to write cron/notify/internal-chat against rows
	// target now owns, which is strictly worse than restart
	// resurrection. The startup eviction path also reads
	// agent_locks.holder for belt-and-suspenders, so a missing
	// marker plus holder!=self still has a chance of being
	// caught manually.
	if markReleased {
		markCtx, markCancel := context.WithTimeout(
			context.Background(), markReleasedCtxBudget)
		err := m.MarkAgentReleasedHere(markCtx, agentID)
		markCancel()
		if err != nil && m.logger != nil {
			m.logger.Error("source-release marker write failed after retries; restart eviction may miss this agent",
				"agent", agentID, "err", err)
		}
		// Clear the symmetric arrival marker so a switch-away
		// doesn't leave a stale "this peer hosts <id>" record
		// that would re-seed AgentLockGuard on the next restart.
		// Best-effort: DeleteKV failure leaves a stale marker
		// that would only cause AgentLockGuard.Start to add an
		// agent we don't actually own — the guard's Acquire would
		// then observe ErrLockHeld (target holds the row now)
		// and stay quiet. Less destructive than skipping the
		// release-marker write.
		clearCtx, clearCancel := context.WithTimeout(
			context.Background(), markReleasedCtxBudget)
		if err := m.ClearAgentArrivedHere(clearCtx, agentID); err != nil && m.logger != nil {
			m.logger.Warn("failed to clear arrival marker on source-release",
				"agent", agentID, "err", err)
		}
		clearCancel()
	}
	if m.cron != nil {
		m.cron.Remove(agentID)
	}
	if m.notifyPoller != nil {
		if withDBCleanup {
			m.notifyPoller.RemoveAgent(agentID)
		} else {
			m.notifyPoller.DetachAgent(agentID)
		}
	}
	m.cancelOneShots(agentID)
	m.closeIndex(agentID)
	if withDBCleanup && m.groupdms != nil {
		m.groupdms.RemoveAgent(agentID)
	}
	m.mu.Lock()
	delete(m.agents, agentID)
	m.mu.Unlock()
	// Clear the switching flag — the agent is now released, not
	// transient. Any subsequent lookup will fail with
	// ErrAgentNotFound, which the chat / cron / mutation paths
	// already handle.
	m.busyMu.Lock()
	if m.switching != nil {
		delete(m.switching, agentID)
	}
	m.busyMu.Unlock()
	// (Marker write moved to the top of this function; see the
	// comment above the cron.Remove call for the durability
	// rationale.)
	if m.logger != nil {
		if withDBCleanup {
			m.logger.Info("agent released locally (handoff source-release)", "agent", agentID)
		} else {
			m.logger.Info("agent detached in-memory (startup eviction)", "agent", agentID)
		}
	}
}

// EvictNonLocalAgentsAtStartup drops in-memory side channels for any
// agent we previously §3.7-released as source. Used at daemon boot
// so a switched-away agent that left this peer doesn't resurrect:
// NewManager loads ALL persisted agent rows regardless of holder,
// and StartSchedulers would otherwise schedule cron / notify pollers
// against them — landing writes on a peer that no longer owns the
// lock.
//
// Eviction policy:
//
//   - Released marker absent (`handoff/released/<id>` kv row): KEEP.
//     A bare `agent_locks.holder_peer != self` is ambiguous — it
//     could mean (a) we used to host this agent and switched it away
//     (evict), or (b) target's phase-1 sync landed and finalize
//     hasn't fired yet (keep, AgentLockGuard + fencing middleware
//     handle the in-flight state). Without a durable signal of (a)
//     we cannot tell them apart.
//   - Released marker present: EVICT via ReleaseAgentLocally. We
//     wrote the marker at source-release time; the agent's runtime
//     belongs to the new holder.
//   - kv read error other than ErrNotFound: KEEP + log. A transient
//     SQLite read failure shouldn't silently evict an agent this
//     peer may still own; AgentLockGuard / fencing middleware will
//     sort it out on subsequent ticks.
//   - Belt-and-suspenders: when the kv check says "evict", we also
//     verify agent_locks.holder is somewhere other than self. If the
//     marker is stale (e.g. operator-driven re-handoff that didn't
//     clear it), refuse to evict and log loudly so the operator can
//     reconcile rather than losing runtime for an agent this peer
//     once again owns.
//
// MUST be called BEFORE AgentLockGuard.Start (so the released agents'
// IDs aren't fed back into the guard's desired set) and BEFORE
// StartSchedulers (so cron / poller never see them). selfPeerID == ""
// is a no-op — the caller is single-peer / pre-§3.7 and there's
// nothing to filter against.
//
// store nil is also a no-op (caller is responsible for wiring; we
// don't fail boot on missing store, m.store is the same handle but
// kept explicit so the caller can pass a different handle in test
// fixtures).
func (m *Manager) EvictNonLocalAgentsAtStartup(ctx context.Context, selfPeerID string) {
	if m == nil || selfPeerID == "" {
		return
	}
	st := m.Store()
	if st == nil {
		return
	}
	// Snapshot the agent IDs under m.mu so the iteration doesn't
	// race with any concurrent ReloadAgentFromStore / Delete that
	// may have landed before the caller wired this in.
	m.mu.Lock()
	ids := make([]string, 0, len(m.agents))
	for id := range m.agents {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		// Marker check first — cheap, durable, the authoritative
		// "we released this" signal. Read errors fail open (keep
		// the agent in memory) but log so the operator sees the
		// kv layer is degraded rather than discovering it via
		// downstream symptoms.
		released, kvErr := m.agentReleasedHereState(ctx, id)
		if kvErr != nil {
			if m.logger != nil {
				m.logger.Warn("startup eviction: released-marker read failed; keeping agent in memory",
					"agent", id, "err", kvErr)
			}
			continue
		}
		if !released {
			continue
		}
		// Reconciliation guard: marker exists but holder is back
		// at self → operator-driven re-handoff that didn't clear
		// the marker. Refuse to evict; loud log so the operator
		// notices the marker is stale and can clear it manually.
		lock, err := st.GetAgentLock(ctx, id)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			if m.logger != nil {
				m.logger.Warn("startup eviction: agent_locks read failed for marked agent; keeping in memory",
					"agent", id, "err", err)
			}
			continue
		}
		if lock != nil && lock.HolderPeer == selfPeerID {
			if m.logger != nil {
				m.logger.Warn("startup eviction: released marker present but agent_locks holder is self; refusing eviction (stale marker?)",
					"agent", id)
			}
			continue
		}
		holderForLog := ""
		if lock != nil {
			holderForLog = lock.HolderPeer
		}
		if m.logger != nil {
			m.logger.Info("startup eviction: released marker present; tearing down local runtime",
				"agent", id, "holder", holderForLog, "self", selfPeerID)
		}
		// In-memory only: the durable side of the release
		// (notify_cursors / group_dm_members deletes) already ran
		// at the original ReleaseAgentLocally call site that wrote
		// the marker; re-running them on every restart would
		// either no-op (rows already gone) or clobber rows that
		// rightfully belong to a re-arrived incarnation if the
		// marker is stale.
		m.detachAgentInMemory(id)
	}
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

	// Block if agent is busy, editing, being reset, or switching
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
	if m.switching != nil && m.switching[agentID] {
		// §3.7 device switch is mid-flight: a reset would
		// erase the source session JSONL that the orchestrator
		// is about to migrate to target.
		m.busyMu.Unlock()
		return fmt.Errorf("%w: device switch in progress", ErrAgentBusy)
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

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

	// Clear the global CLI session stores BEFORE acquiring
	// memorySyncMu. clearGrokSession now takes lockGrokSessionTransfer
	// internally, and the peer-agent-sync handler holds
	// lockGrokSessionTransfer THEN takes memorySyncMu (via
	// agent.LockAgentMemorySync at peer_agent_sync_handler.go:519).
	// Acquiring memorySyncMu first here would invert that order and
	// deadlock against a concurrent device-switch arrival. Neither
	// clear touches memory/* nor MEMORY.md, so moving it outside the
	// memorySyncMu region is safe for the half-reset-visibility
	// invariant the lock protects.
	clearClaudeSession(id)
	clearGrokSession(id)

	// Hold the per-agent memory-sync lock around the entire on-
	// disk wipe + recreate + post-reset sync so concurrent CRUD /
	// sync helpers can't observe a half-reset tree (memory/ wiped
	// but MEMORY.md still present) or a fresh tree whose DB rows
	// haven't been reconciled yet. The release lives at the end
	// of the function (or before the early-return on recreate
	// failure) so the lock spans the full critical region.
	releaseSync := lockMemorySync(id)

	// Tombstone all memory_entries DB rows BEFORE wiping disk.
	// Without this, the post-reset syncMemoryEntriesToDB sees an
	// empty memory/ directory, enters hydrate mode (len(disk)==0),
	// and writes pre-reset DB rows back to disk — effectively
	// undoing the reset. Tombstoning first makes the rows invisible
	// to ListMemoryEntries (WHERE deleted_at IS NULL), so the hydrate
	// phase finds nothing to restore.
	if st := getGlobalStore(); st != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		n, err := st.SoftDeleteAllMemoryEntries(ctx, id)
		cancel()
		if err != nil {
			releaseSync()
			return fmt.Errorf("reset: tombstone memory entries: %w", err)
		}
		if n > 0 {
			m.logger.Info("reset: tombstoned memory entries", "agent", id, "count", n)
		}
	}

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

	// (Global CLI session stores cleared above, BEFORE lockMemorySync,
	// to avoid the lock-order inversion against the peer-agent-sync
	// handler. See the comment at the top of this function for detail.)

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
// All runtime activity is stopped: cron is unscheduled, in-flight one-shot
// chats are cancelled, the cached memory index is closed. The agent stays
// in the in-memory map (with Archived=true) and on disk — credentials,
// slack tokens, messages, memory, persona, avatar, tasks are all retained.
// Inbound chats route through prepareChat, which refuses archived agents
// with ErrAgentArchived.
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
// mutations on the same agent. Lock order is LockPatch → acquireResetGuard
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

	// Set the Archived flag FIRST so any concurrent Update path that
	// re-checks under m.mu sees the archived state and skips its
	// schedule side-effects. The runtime stoppage below then cleans
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

// Unarchive restores a previously archived agent: clears the Archived flag
// and re-schedules its cron. Slack bot re-arming is the handler's
// responsibility (server owns slackHub lifecycle).
//
// Idempotent: calling Unarchive on a non-archived agent is a no-op.
//
// Serializes against Archive via acquireResetGuard so an Archive/Unarchive
// race can't interleave flag flips with cron operations. The idempotency
// check happens INSIDE the guard so we can't observe a stale
// "not archived" state while a concurrent Archive is mid-flight (the guard
// blocks until that Archive's cleanup() releases it).
func (m *Manager) Unarchive(id string) error {
	// LockPatch parity with Archive/Delete — keeps the per-agent
	// lifecycle ordering consistent across all three transitions.
	// Cheap to take; avoids future surprises if a new caller starts
	// depending on the lock covering Unarchive too.
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
	m.mu.Unlock()

	if expr != "" {
		if err := m.cron.Schedule(id, expr); err != nil {
			m.logger.Warn("failed to re-schedule cron on unarchive", "agent", id, "err", err)
		}
	}

	m.save()
	m.logger.Info("agent unarchived", "id", id)
	return nil
}

// Delete removes an agent and its data.
//
// Takes Manager.LockPatch to serialize against in-flight PATCH-style
// mutations. Lock order: LockPatch → acquireResetGuard → m.mu.
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

	// Remove credentials and per-agent encrypted tokens (Slack app/bot
	// tokens live in the notify_tokens table) outside lock (DB I/O).
	// After the tombstone these are best-effort orphan cleanup;
	// failures are warn-only and leave the rows behind until the next
	// boot's GC or a manual sweep picks them up.
	if m.creds != nil {
		if err := m.creds.DeleteAllForAgent(id); err != nil {
			m.logger.Warn("failed to delete credentials post-tombstone", "agent", id, "err", err)
		}
		if err := m.creds.DeleteTokensByAgent(id); err != nil {
			m.logger.Warn("failed to delete agent tokens", "agent", id, "err", err)
		}
	}

	m.cron.Remove(id)
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

	// Remove agent data directory (best-effort: credentials/cron/tokens already cleaned up)
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
// runtime side channels: the cron schedule. Called by the §3.7
// device-switch finalize hook (NOT by phase-1 sync) so an agent that
// landed but hasn't yet been claimed by this peer can't fire schedules
// before AgentLockGuard adopts it.
//
// Idempotent and threadsafe. Skipped (no-op) when StartSchedulers hasn't
// run yet — the boot loop registers everything from the same in-memory
// snapshot, so a redundant call here would just race.
//
// Archived agents always have their cron entry torn down here, even when
// it was already detached by Archive(): a Reload that flipped an active
// agent to archived underneath us must still surface as "stopped" on the
// runtime side.
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
	// 帰還 (holder==self) のタイミング — Hub が remote 期間中に蓄積した
	// remote_message_mirror 行は今この瞬間から stale。canonical な
	// agent_messages が再びこの peer にあるので mirror を破棄する。
	// peer 側 (Hub 以外) で初めて adopt するケースは mirror が元々空なので no-op。
	// ベストエフォート: 失敗しても runtime 起動は継続。
	if err := m.DeleteMirrorForAgent(agentID); err != nil && m.logger != nil {
		m.logger.Warn("failed to clear remote message mirror on activate",
			"agent", agentID, "err", err)
	}
	if a.Archived {
		if m.cron != nil {
			m.cron.Remove(agentID)
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

	// §3.7 device-switch skill: install the SKILL.md so the newly-
	// arrived agent can initiate another switch from this peer.
	// Without this the skill file only exists at prepareChat time,
	// so a fresh arrival that hasn't chatted yet would not expose
	// the kojo-switch-device skill. SyncDeviceSwitchSkillForTool
	// dispatches based on Tool: claude/custom install the
	// Claude-Code-flavored body, grok installs its own body
	// (no `!`exec`` substitution, `grok --resume` wording),
	// codex/llama.cpp are no-op.
	SyncDeviceSwitchSkillForTool(agentID, a.Tool, a.IsDeviceSwitchEnabled(), m.logger)
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

// arrivalCancels tracks the in-flight cancel func of every
// NotifyDeviceSwitchArrival goroutine so a target-side teardown
// (releaseAgentLocallyCore / TeardownAgentRuntime) can drop the
// long-running arrival chat alongside the rest of the agent's
// in-memory state. Without this the goroutine outlives teardown
// (the chat runs under context.Background until WithCancel) and
// keeps writing to the transcript even though the agent has
// already been purged or re-adopted by another switch — surfacing
// as duplicate / "ghost" assistant turns. Keyed by agent_id
// (only the latest arrival chat per agent needs cancelling; a
// duplicate finalize is deduped upstream by arrivalNotified).
var arrivalCancels sync.Map // map[string]context.CancelFunc

type arrivalDedupKey struct {
	agentID string
	opID    string
}

// arrivalPromptBase is the always-included header the arrival chat opens with.
// Keep the prefix stable so the prompt cache survives across switches.
const arrivalPromptBase = "デバイス転移完了。転移元: %s。このデバイスで作業を継続してください。"

// arrivalPromptFallbackTail is appended when no unaddressed user instruction
// is found on this peer (e.g. cron-triggered or first-ever switch).
const arrivalPromptFallbackTail = "直前の会話の流れを確認し、中断された作業があれば再開してください。"

// arrivalPromptUserInstructionPreviewLimit caps how many RUNES of the latest
// user instruction we quote into the arrival prompt. Rune-based (not byte-
// based) so a truncate boundary mid-multibyte never produces invalid UTF-8 —
// the rest of the system is UTF-8 throughout and a torn rune would surface as
// a `�` in the prompt the LLM sees. Long pasted dumps still get
// truncated; the agent can re-read the full body from its transcript.
const arrivalPromptUserInstructionPreviewLimit = 4000

// arrivalUserHistoryScanLimit is the maximum number of recent messages the
// arrival look-up walks before giving up on finding a user-role row. Bumped
// well above the previous 20 so a chatty trailing run of system / assistant
// rows (a busy cron + tool storm right before the switch) can't hide the
// user message; arrival is fired once per device-switch so the extra read
// is negligible.
const arrivalUserHistoryScanLimit = 200

// arrivalContext bundles the per-switch text the arrival prompt
// quotes. UserInstruction is the latest UNADDRESSED user-role
// message (the live "do X" ask). TailCommitment is the Plan A tail
// — the assistant text source's claude generated AFTER the
// kojo-switch-device curl returned, captured + shipped via
// /handoff/finalize's TailMessage field. Both may be empty
// independently; buildArrivalPrompt picks the quote layout from
// which ones are present.
type arrivalContext struct {
	UserInstruction string
	TailCommitment  string
}

// buildArrivalPrompt assembles the system-role text the arrival chat sends to
// the just-arrived agent. The prompt always opens with the
// "デバイス転移完了" header so a stable prefix benefits the prompt cache, then
// — depending on what collectArrivalContext finds — quotes the latest
// UNADDRESSED user instruction AND/OR the Plan A tail commitment so the
// LLM has both anchors:
//
//   - The user instruction is what the human actually asked.
//   - The tail commitment is the agent's own statement of what it
//     intended to do post-arrival; claude --continue's JSONL on
//     target doesn't carry this row (source wrote it after the
//     sync read the JSONL bytes), so without quoting here the
//     commitment is invisible to the LLM.
//
// Falls back to the generic "resume interrupted work" tail when both
// are empty (cron-fired switch with no in-flight ask).
//
// Ctx is the same context that wraps chatWithArrivalRetry; a cancelled parent
// here only means the look-up is skipped — the caller still issues the chat
// with the fallback prompt so the agent at least gets the arrival
// notification.
func buildArrivalPrompt(ctx context.Context, m *Manager, agentID, sourcePeerName string) string {
	head := fmt.Sprintf(arrivalPromptBase, sourcePeerName)
	ac := collectArrivalContext(ctx, m, agentID)
	if ac.UserInstruction == "" && ac.TailCommitment == "" {
		return head + arrivalPromptFallbackTail
	}
	var b strings.Builder
	b.WriteString(head)
	if ac.UserInstruction != "" {
		b.WriteString("\n\n直前のユーザー指示（自動文脈ブロックは除去済み）:\n")
		b.WriteString(ac.UserInstruction)
	}
	if ac.TailCommitment != "" {
		b.WriteString("\n\n転移直前にあなた自身が宣言した作業（claude --continue の履歴には載っていないので注意）:\n")
		b.WriteString(ac.TailCommitment)
	}
	b.WriteString("\n\nまず上記の指示・宣言を確認し、未完了であればこのデバイスで実行してください。すでに完了している場合は通常の待機状態に戻ってください。")
	return b.String()
}

// collectArrivalContext walks the recent transcript and pulls out
// the latest unaddressed user instruction (with the kojo-injected
// `<context>...</context>` block stripped) plus the Plan A tail
// commitment (the assistant message immediately newer than the
// kojo-switch snapshot, if present).
//
// Returns the zero arrivalContext when:
//
//   - the store is unavailable / context cancelled
//   - the transcript has no user-role row in the recent scan window
//   - the candidate user body is empty after stripping the volatile block
//   - the latest user message has already been addressed by
//     post-arrival work (so re-quoting it would shake old, completed
//     work loose on every subsequent switch)
//
// Why this exists: the in-flight kojo-switch-device tool_use ships
// to target as a hanging assistant turn in claude's JSONL — claude
// --continue treats the conversation as ended at the user msg and
// the arrival prompt becomes the next "live" instruction. Without
// explicit quoting in the arrival prompt, claude often reads the
// diary-notes auto-context embedded in the user msg, decides "no
// interrupted work", and never actions the actual instruction
// (e.g. "do a security check"). Observed in ag_f71bf5… on 2026-05-28:
// user asked transfer + security check, target arrived and reported
// "no interrupted work" while the security check was the whole point
// of the switch.
func collectArrivalContext(ctx context.Context, m *Manager, agentID string) arrivalContext {
	if m == nil || agentID == "" {
		return arrivalContext{}
	}
	st := m.Store()
	if st == nil {
		return arrivalContext{}
	}
	msgs, err := st.ListMessages(ctx, agentID, store.MessageListOptions{
		Order: "desc",
		Limit: arrivalUserHistoryScanLimit,
	})
	if err != nil {
		if m.logger != nil {
			m.logger.Debug("arrival prompt: ListMessages failed; falling back to generic prompt",
				"agent", agentID, "err", err)
		}
		return arrivalContext{}
	}
	// In desc order: msgs[0] is the newest. Look for the first user-role
	// row whose body has content after stripping the kojo-injected context
	// block. Anything BEFORE it in the slice (i.e. with a higher seq) is a
	// potential addresser — only a non-empty assistant counts, and the
	// snapshot+tail unit gets carved out (see userMessageAddressed).
	for i, msg := range msgs {
		if msg == nil || msg.Role != "user" {
			continue
		}
		body := stripArrivalContextBlock(msg.Content)
		body = strings.TrimSpace(body)
		if body == "" {
			// Empty-after-strip rows (auto-checkin context with no
			// user text) don't help and don't gate "addressed" —
			// fall through and look at the next older user row.
			continue
		}
		newer := msgs[:i]
		if userMessageAddressed(newer) {
			return arrivalContext{}
		}
		return arrivalContext{
			UserInstruction: truncatePromptByRune(body, arrivalPromptUserInstructionPreviewLimit),
			TailCommitment:  truncatePromptByRune(planATailContent(newer), arrivalPromptUserInstructionPreviewLimit),
		}
	}
	return arrivalContext{}
}

// planATailContent returns the content of the MOST-RECENT Plan A
// tail row in `newer` (immediately newer than the NEWEST kojo-switch
// snapshot). The arrival prompt quotes this so the LLM on target
// sees its own pre-transfer commitment text — which doesn't ride
// claude --continue's JSONL because source wrote it after the sync
// read the JSONL bytes.
//
// "Newest snapshot" is picked deliberately: a multi-switch
// transcript has several snapshot+tail pairs, but only the most
// recent one is relevant to THIS arrival. The older units belong
// to past switches whose tails were already presented to claude
// on those arrivals and (assuming the SKILL.md silence directive
// works) acted on.
//
// Returns the empty string when no snapshot was found, when the
// position past the snapshot is occupied by a non-tail-shaped row
// (e.g. another snapshot, an empty assistant, a system message),
// or when the snapshot is the very newest row (no slot for a tail).
func planATailContent(newer []*store.MessageRecord) string {
	// Scan from newest (index 0) toward oldest; the first snapshot
	// we hit is the most recent.
	for i, m := range newer {
		if !isKojoSwitchSnapshot(m) {
			continue
		}
		if i == 0 {
			// Snapshot is the very newest row in `newer`; no
			// slot newer than it for a tail.
			return ""
		}
		cand := newer[i-1]
		if !isPlanATailShape(cand) {
			return ""
		}
		return strings.TrimSpace(cand.Content)
	}
	return ""
}

// userMessageAddressed reports whether any of the messages in `newer`
// (which the caller passes as the prefix of a desc-ordered slice —
// i.e. items with seq > the user row's seq) constitute a substantive
// assistant reply that has actually completed the work.
//
// The device-switch flow lands TWO rows after the user instruction:
//
//  1. the in-flight snapshot (assistant with empty/short content +
//     `kojo-switch-device` in ToolUses), and
//  2. on Plan A, the tail commitment (assistant with non-empty
//     content like "到着したらXを実施する", no kojo-switch ToolUses).
//
// Neither row counts as a real addresser — the snapshot is the
// curl-in-flight stub; the tail is a commitment, not a completion.
// Rows NEWER than the snapshot+tail unit ARE post-arrival activity
// (target wrote them after claude --continue resumed), so a
// non-empty assistant there means the work IS done and a follow-up
// switch should NOT re-quote the original user ask.
//
// Algorithm (with `newer` in descending seq order — newest at index 0):
//
//  1. Walk `newer` and identify EVERY in-flight switch unit
//     (snapshot row + the immediately-newer slot if it is
//     tail-shaped). Multi-switch transcripts can carry several
//     such units stacked on top of the same user instruction
//     (e.g. cron-fired switch after a self-call switch); peeling
//     just the oldest would miss the newer ones and let a tail
//     count as an addresser.
//  2. Items outside every unit are real assistant rows
//     (post-arrival completion text, cron checkin responses,
//     etc.). A non-empty assistant in that residual set means
//     the user instruction has been addressed; the cron switch
//     follow-up should NOT re-quote it.
//
// Fully empty assistant rows (no content, no thinking) never count —
// they're the bare snapshot produced when
// SnapshotAccumulatedMessageRecord fires before any text streams.
func userMessageAddressed(newer []*store.MessageRecord) bool {
	unit := planAUnitMask(newer)
	for i, m := range newer {
		if unit[i] {
			continue
		}
		if isNonEmptyAssistant(m) {
			return true
		}
	}
	return false
}

// planAUnitMask scans `newer` and returns a per-index boolean: true
// for indices that belong to an in-flight kojo-switch unit
// (the snapshot itself plus the tail-shaped row immediately newer
// than each snapshot). Used by userMessageAddressed AND
// planATailContent so both functions share the exact same
// "what's part of a switch unit" rule.
func planAUnitMask(newer []*store.MessageRecord) []bool {
	mask := make([]bool, len(newer))
	for i, m := range newer {
		if !isKojoSwitchSnapshot(m) {
			continue
		}
		mask[i] = true
		if i > 0 && isPlanATailShape(newer[i-1]) {
			mask[i-1] = true
		}
	}
	return mask
}

// isKojoSwitchSnapshot reports whether m is the in-flight assistant
// snapshot the device-switch handler ships to target. Substring-scans
// the raw ToolUses JSON for the skill dir name — the wire format
// always emits `"name":"kojo-switch-device"` (or `"attributionSkill":"kojo-switch-device"`
// from the Skill wrapper) so a single substring is robust against
// either shape.
func isKojoSwitchSnapshot(m *store.MessageRecord) bool {
	if m == nil || m.Role != "assistant" {
		return false
	}
	if len(m.ToolUses) == 0 {
		return false
	}
	return bytes.Contains(m.ToolUses, []byte(deviceSwitchSkillDirName))
}

// isPlanATailShape reports whether m has the shape of the Plan A
// deferred-finalize tail: a non-empty assistant row WITHOUT a
// kojo-switch-device ToolUses entry. The shape isn't 100% specific
// (any non-empty assistant directly post-snapshot fits) but combined
// with snapshotIdx-1 positioning it's robust enough to skip the
// commitment text without confusing it with post-arrival completion.
func isPlanATailShape(m *store.MessageRecord) bool {
	if m == nil || m.Role != "assistant" {
		return false
	}
	if isKojoSwitchSnapshot(m) {
		return false
	}
	return strings.TrimSpace(m.Content) != "" || strings.TrimSpace(m.Thinking) != ""
}

// isNonEmptyAssistant reports whether m is an assistant row with at
// least one non-empty Content or Thinking field.
func isNonEmptyAssistant(m *store.MessageRecord) bool {
	if m == nil || m.Role != "assistant" {
		return false
	}
	return strings.TrimSpace(m.Content) != "" || strings.TrimSpace(m.Thinking) != ""
}

// stripArrivalContextBlock removes the leading kojo-injected
// `<context>...</context>` block from a user message body. Mirrors the
// sentinel-gated rules of stripVolatileContext: only blocks emitted by
// BuildVolatileContext (which embed volatileContextSentinel) are stripped,
// so a user who legitimately opens their own message with `<context>`
// keeps their content intact. A non-sentinel-bearing leading block — or
// no leading `<context>` at all — passes through unchanged.
func stripArrivalContextBlock(s string) string {
	trimmed := strings.TrimLeft(s, " \t\r\n")
	if !strings.HasPrefix(trimmed, "<context>") {
		return s
	}
	closeIdx := strings.Index(trimmed, "</context>")
	if closeIdx <= 0 {
		return s
	}
	if !strings.Contains(trimmed[:closeIdx], volatileContextSentinel) {
		return s
	}
	return strings.TrimLeft(trimmed[closeIdx+len("</context>"):], "\r\n")
}

// truncatePromptByRune caps body at `runeLimit` runes (NOT bytes), so the
// truncation boundary never lands mid-multibyte. Falls through to the
// original string when body already fits.
//
// Walks runes via `range` instead of allocating `[]rune(body)`: a 10 MiB
// pasted body would otherwise pin ~40 MiB of int32s before the slice can
// be truncated. The range loop tracks the byte index of each rune and
// stops at runeLimit, so the substring slice reuses the original backing
// array.
func truncatePromptByRune(body string, runeLimit int) string {
	if runeLimit <= 0 {
		return body
	}
	count := 0
	cutoff := -1 // byte offset to truncate to; -1 = no truncation needed
	for i := range body {
		if count == runeLimit {
			cutoff = i
			break
		}
		count++
	}
	if cutoff < 0 {
		// Either body has ≤ runeLimit runes (no truncation) or
		// exactly runeLimit (range exits before setting cutoff).
		return body
	}
	return body[:cutoff] + "…（以下省略、完全な内容はトランスクリプト参照）"
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
	// Build the cancel context BEFORE the goroutine spawns and
	// register it synchronously so a teardown that fires between
	// here and the goroutine's first instruction can still find
	// our cancel and stop the chat. Wrap in a uniquely-
	// addressable wrapper so CompareAndDelete on defer compares
	// pointers (CancelFunc itself isn't comparable in Go).
	//
	// Replacement semantics: a SECOND arrival for the same agent
	// (different op_id) supersedes the prior one — fire the old
	// cancel and reuse the key. This keeps the registry tracking
	// the LATEST in-flight arrival, never letting an orphan run
	// to completion if a fresh switch lands while it's still
	// drafting the recap. The displaced goroutine's m.Chat will
	// return ctx.Err() and exit cleanly.
	ctx, cancel := context.WithCancel(context.Background())
	myEntry := &cancel
	if prev, loaded := arrivalCancels.Swap(agentID, myEntry); loaded {
		if pf, ok := prev.(*context.CancelFunc); ok && pf != nil && *pf != nil {
			(*pf)()
		}
	}
	go func() {
		// No artificial timeout: the arrival prompt asks the
		// agent to resume mid-thought after a device switch, and
		// claude regularly takes longer than a few minutes on
		// the recap. The previous 3-minute WithTimeout truncated
		// every long arrival with a "⚠️ 制限時間超過により中断されました"
		// system message in the transcript — visible to the user
		// as "timeouts are firing on every switch". A plain
		// Background context drops the deadline.
		defer arrivalCancels.CompareAndDelete(agentID, myEntry)
		defer cancel()

		prompt := buildArrivalPrompt(ctx, m, agentID, sourcePeerName)
		events, err := chatWithArrivalRetry(ctx, m, agentID, prompt, opID)
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

// arrivalChatRetryAttempts × arrivalChatRetryBackoff bounds the total
// wait the arrival prompt accepts before giving up. ActivateAgentRuntime
// schedules the cron side channel right before the arrival fires; if a
// cron tick grabs the busy slot first, m.Chat returns ErrAgentBusy and
// the arrival would otherwise be lost — visible to the user as
// "auto-continue didn't fire on return."
//
// Codex flagged that a 3s budget can't outlast a real cron-triggered
// chat (which can run for minutes). The compromise: bump the budget to
// roughly the longest plausible competing one-shot — claude turns
// typically wrap within a minute or two — and rely on the budget being
// dominated by the COMPLETION of the competing chat, not by polling
// overhead. If a wedged sibling chat truly blocks longer than that,
// the arrival is permanently lost (see LIMITATION on
// chatWithArrivalRetry — /handoff/finalize retry does NOT re-fire
// the hook because pending was already consumed). Recovery in that
// case is manual: the operator drafts a system message via the UI
// to wake the agent, or a future durable arrival queue (kv-backed,
// follow-up work) sweeps it on the next daemon tick.
//
// ReloadAgentFromStore racing with the lookup similarly surfaces as
// ErrAgentNotFound for a brief window; the same retry covers that.
const arrivalChatRetryAttempts = 60

// arrivalChatRetryBackoff is the inter-attempt sleep. 2s × 60 = 120s of
// total wait, far enough to outlast the typical cron-triggered claude
// turn that might transiently hold busy.
const arrivalChatRetryBackoff = 2 * time.Second

// chatWithArrivalRetry wraps m.Chat with a bounded retry loop for the
// arrival prompt. Retries on ErrAgentBusy / ErrAgentNotFound (both
// race-driven and self-clearing). Other errors bail immediately —
// they signal a genuine misconfiguration (no backend, archived
// agent, switching flag pinned by a buggy caller) that a retry
// wouldn't fix.
//
// Returns the same (events, err) shape as m.Chat so the caller's
// drain loop is unchanged.
//
// Cheap busy-wait before each m.Chat attempt: a full m.Chat call
// runs prepareChat (FS sync + memory-context build + system-prompt
// assembly + AppendMessage + backend init), which is expensive to
// repeat 60 times when the only blocker is a competing busy entry.
// Polling IsBusy / Get is just a map lookup under a mutex, so a
// short pre-check skips the wasted prepareChat work on every retry
// while the competing chat finishes.
//
// LIMITATION: this retry only covers ~120s. If a sibling chat truly
// holds busy past arrivalChatRetryAttempts × arrivalChatRetryBackoff,
// the goroutine gives up and arrivalNotified.Delete(key) runs — but
// the finalize HTTP handler has already returned 200 to source by
// then, source's onAgentReleasedAsSource has run, and the pending
// agent-sync entry on target has been consumed. The arrival is
// effectively LOST and the operator must drive a fresh switch (or
// manually trigger a system message on target) to resume claude.
// A durable arrival queue (kv row written before m.Chat, swept at
// daemon startup) is the correct fix; tracked as follow-up.
func chatWithArrivalRetry(ctx context.Context, m *Manager, agentID, prompt, opID string) (<-chan ChatEvent, error) {
	var lastErr error
	for attempt := 0; attempt < arrivalChatRetryAttempts; attempt++ {
		// Cheap pre-check: skip the expensive m.Chat → prepareChat
		// path while the agent is observably busy. The check races
		// with concurrent state changes but at worst we issue one
		// m.Chat call that returns ErrAgentBusy (idempotent, no
		// side effects) — strictly better than burning prepareChat
		// every attempt. ErrAgentNotFound also self-clears via
		// the m.Chat path; we only poll IsBusy here, not Get(),
		// to keep the pre-check single-purpose.
		if attempt > 0 && m.IsBusy(agentID) {
			if m.logger != nil {
				m.logger.Debug("device-switch arrival chat: agent still busy, skipping retry attempt",
					"agent", agentID, "op_id", opID, "attempt", attempt+1)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(arrivalChatRetryBackoff):
			}
			continue
		}
		events, err := m.Chat(ctx, agentID, prompt, "system", nil, BusySourceNotification)
		if err == nil {
			if attempt > 0 && m.logger != nil {
				m.logger.Info("device-switch arrival chat: succeeded after retry",
					"agent", agentID, "op_id", opID, "attempts", attempt+1)
			}
			return events, nil
		}
		lastErr = err
		// Non-retryable: backend init failures, archived agent,
		// fenced-out by switching, etc. Bail immediately so the
		// caller logs the genuine cause instead of masking it
		// with retry timing noise.
		if !errors.Is(err, ErrAgentBusy) && !errors.Is(err, ErrAgentNotFound) {
			return nil, err
		}
		if m.logger != nil {
			m.logger.Debug("device-switch arrival chat: transient reject, retrying",
				"agent", agentID, "op_id", opID, "attempt", attempt+1, "err", err)
		}
		if attempt+1 < arrivalChatRetryAttempts {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(arrivalChatRetryBackoff):
			}
		}
	}
	return nil, lastErr
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

// kvArrivedProxyKey persists the allowed_proxy_peer that the finalize
// handler stamped onto agent_locks for this arrival. AgentLockGuard.Stop
// wipes agent_locks rows via ReleaseAgentLockByPeer on graceful shutdown,
// so the column itself doesn't survive a restart — the next boot's fresh
// AcquireAgentLock re-inserts with allowed_proxy_peer = self, which then
// causes agentHolderAdmitMiddleware to reject the orchestrator's signed
// proxy and the agent becomes unreachable from the UI.
//
// Pairing this kv row with the arrived marker lets the boot path restore
// the original allowed_proxy_peer via UpdateAgentLockAllowedProxy after
// AgentLockGuard.Start re-acquires. Key prefix differs from arrived/ and
// released/ so latestHandoffMarkers naturally ignores it.
//
// Cleared by ClearAgentArrivedHere alongside the arrival marker — a
// switch-away from this peer drops the proxy hint too. Older deployments
// that arrived BEFORE this fix have no row written; restore is best-effort
// and a missing row means "no override, leave fresh acquire default".
func kvArrivedProxyKey(agentID string) string { return "arrived_proxy/" + agentID }

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
// would resurrect schedulers / cron for an agent
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
//
// allowedProxyPeer carries the source device_id that finalize stamped
// onto agent_locks.allowed_proxy_peer. Stored in a sibling kv row so
// the boot path can re-stamp the column after the post-restart fresh
// AcquireAgentLock resets it to self. Pass empty when no proxy override
// is desired (test fixtures / legacy callers); the boot path treats
// missing rows as "leave fresh-acquire default."
func (m *Manager) MarkAgentArrivedHere(ctx context.Context, agentID, allowedProxyPeer string) error {
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
	if err := putKVWithRetry(ctx, st, rec); err != nil {
		return fmt.Errorf("MarkAgentArrivedHere arrival: %w", err)
	}
	// Sibling row: allowed_proxy_peer override for restart-time restore.
	// Order matters — the arrival marker is the authoritative "this peer
	// hosts <id>" signal; if the proxy write fails after the arrival
	// landed, the agent is still reachable for trusted peers and the
	// next finalize retry / operator action can re-stamp the row. The
	// reverse order would risk an arrived_proxy/<id> row with no
	// corresponding arrived/<id> sibling, which the boot path would
	// silently ignore but leaves clutter.
	if allowedProxyPeer == "" {
		return nil
	}
	proxyRec := &store.KVRecord{
		Namespace: kvReleasedNamespace,
		Key:       kvArrivedProxyKey(agentID),
		Value:     allowedProxyPeer,
		Type:      store.KVTypeString,
		Scope:     store.KVScopeMachine,
	}
	if err := putKVWithRetry(ctx, st, proxyRec); err != nil {
		return fmt.Errorf("MarkAgentArrivedHere proxy: %w", err)
	}
	return nil
}

// putKVWithRetry centralises the deadline-driven retry loop that
// MarkAgentReleasedHere / MarkAgentArrivedHere share. Same constant
// 100ms backoff, same "give up only when ctx fires" semantic.
func putKVWithRetry(ctx context.Context, st *store.Store, rec *store.KVRecord) error {
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
			return fmt.Errorf("%w after %d attempts (last: %v)",
				ctx.Err(), attempts, lastErr)
		case <-time.After(markReleasedBackoff):
		}
	}
}

// ClearAgentArrivedHere removes the §3.7 target-arrival marker AND
// the sibling allowed_proxy_peer row. Called by the source-release
// path so a switch-away from this peer doesn't leave a stale arrival
// marker that would re-seed AgentLockGuard against an agent we no
// longer own. Idempotent; missing row on either key is not an error.
//
// The proxy sibling is dropped best-effort AFTER the arrival marker
// goes — if the proxy delete fails the row becomes orphaned, but the
// boot restore path looks up by agent_id only when an arrival marker
// is present, so an orphaned proxy row is dead data the next
// MarkAgentArrivedHere will overwrite.
func (m *Manager) ClearAgentArrivedHere(ctx context.Context, agentID string) error {
	if m == nil || agentID == "" {
		return nil
	}
	st := m.Store()
	if st == nil {
		return nil
	}
	err := st.DeleteKV(ctx, kvReleasedNamespace, kvArrivedKey(agentID), "")
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if perr := st.DeleteKV(ctx, kvReleasedNamespace, kvArrivedProxyKey(agentID), ""); perr != nil &&
		!errors.Is(perr, store.ErrNotFound) {
		// Sibling delete failure logs and falls through — see
		// orphaned-row note above. Surface as warn rather than
		// error so the caller's release sequence still proceeds.
		if m.logger != nil {
			m.logger.Warn("clear arrived-proxy sibling failed; orphan tolerated",
				"agent", agentID, "err", perr)
		}
	}
	return nil
}

// GetAgentArrivedProxy returns the allowed_proxy_peer override that
// MarkAgentArrivedHere stashed alongside the arrival marker. Used by
// the boot path to restore agent_locks.allowed_proxy_peer after a
// graceful-shutdown wipe + fresh AcquireAgentLock. Returns ("", nil)
// when no row exists — caller treats that as "leave fresh-acquire
// default" rather than erroring out (legacy arrivals predate this
// kv row).
func (m *Manager) GetAgentArrivedProxy(ctx context.Context, agentID string) (string, error) {
	if m == nil || agentID == "" {
		return "", nil
	}
	st := m.Store()
	if st == nil {
		return "", nil
	}
	rec, err := st.GetKV(ctx, kvReleasedNamespace, kvArrivedProxyKey(agentID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	if rec == nil {
		return "", nil
	}
	return rec.Value, nil
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
// in-flight one-shots, cached memory index, group-DM membership, and
// the agents map entry itself. The on-disk DB rows + persona / blob
// bodies remain so a future agent-sync back to this peer can
// re-hydrate via ReloadAgentFromStore.
//
// Called by the §3.7 source-release hook after a successful complete
// + finalize: target now holds agent_locks, and source must stop
// firing cron / internal Chat against this agentID. Without this
// teardown, source's cron scheduler keeps the entry and runCronJob
// proceeds through the agents.Get hit; the resulting Chat would write
// transcript / JSONL that target never sees.
//
// Safe to call multiple times — every step is no-op-on-missing.
// Threadsafe.
func (m *Manager) ReleaseAgentLocally(agentID string) {
	// withDBCleanup=true: group_dm_members rows are deleted from
	// this peer's kojo.db. Semantically correct for a §3.7
	// source-release — the agent's runtime is moving to target, so
	// the source's local memberships belong to the prior incarnation
	// and have no surviving consumer here.
	m.releaseAgentLocallyCore(agentID, true /*withDBCleanup*/, true /*markReleased*/)
}

// TeardownAgentRuntime stops every in-memory runtime side channel
// (cron, slack, in-flight one-shots, agents map entry) WITHOUT
// writing the §3.7 source-release marker and WITHOUT touching DB
// rows the orchestrator is about to overwrite (group-DM members).
// Used by the inter-peer state-probe
// self-heal path: the orchestrator is retrying a device-switch
// against this host, so this peer has to stop driving the agent
// before the new agent-sync lands, but the released marker would
// flag the row for startup eviction on the next boot and undo
// the retry.
//
// Idempotent. Threadsafe.
func (m *Manager) TeardownAgentRuntime(agentID string) {
	m.releaseAgentLocallyCore(agentID, false /*withDBCleanup*/, false /*markReleased*/)
}

// detachAgentInMemory is the no-DB-cleanup variant used by
// EvictNonLocalAgentsAtStartup. The marker that triggered eviction
// implies a prior ReleaseAgentLocally already ran (with full
// cleanup), so we only need to tear down the in-memory side
// channels that NewManager just rebuilt on this fresh boot. We do
// NOT re-issue groupdms.RemoveAgent because its DB-row deletes have
// already happened, and a stale-marker reconciliation case
// (operator-driven re-handoff) where the guard above lets us through
// would re-do destructive deletes against rows that may legitimately
// belong to the (now re-arrived) agent.
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
	// continuing to write cron/internal-chat against rows
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
	m.cancelOneShots(agentID)
	// Stop any in-flight arrival chat. It runs under
	// context.Background until WithCancel; without this it
	// outlives every teardown path (source-release, target-
	// purge, EvictAtStartup) and keeps writing to a transcript
	// the agent no longer owns. Best-effort — a finished
	// goroutine cleared the map entry itself; a concurrent
	// newer arrival uses CompareAndDelete to keep its own
	// cancel registered.
	if v, ok := arrivalCancels.LoadAndDelete(agentID); ok {
		if cfp, ok := v.(*context.CancelFunc); ok && cfp != nil && *cfp != nil {
			(*cfp)()
		}
	}
	m.closeIndex(agentID)
	if withDBCleanup && m.groupdms != nil {
		m.groupdms.RemoveAgent(agentID)
	}
	// remote_message_mirror を agent_id 単位に掃除。release / teardown /
	// startup eviction いずれの経路でも、この peer はもう agent の holder
	// ではないか canonical な runtime も無いので mirror の保持義務はない。
	// 削除は冪等で副作用無し。crash recovery: release marker 書込後・
	// mirror delete 前に死んでも、再起動時の EvictNonLocalAgentsAtStartup
	// 経由で detachAgentInMemory → ここに来て掃除されるため stale が
	// 永続化しない。
	if err := m.DeleteMirrorForAgent(agentID); err != nil && m.logger != nil {
		m.logger.Warn("failed to clear remote message mirror on release",
			"agent", agentID, "err", err)
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
// and StartSchedulers would otherwise schedule cron jobs against
// them — landing writes on a peer that no longer owns the lock.
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
// StartSchedulers (so cron never sees them). selfPeerID == ""
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
		// (group_dm_members deletes) already ran
		// at the original ReleaseAgentLocally call site that wrote
		// the marker; re-running them on every restart would
		// either no-op (rows already gone) or clobber rows that
		// rightfully belong to a re-arrived incarnation if the
		// marker is stale.
		m.detachAgentInMemory(id)
	}
}

// PruneToOwnedAgentsForPeer は --peer mode 起動時に、NewManager が DB から
// 全行ロードした m.agents から「この peer が実際に所有している」ID 以外を
// detach する。所有判定は arrived markers ∪ agent_locks.holder == self、
// マイナス released markers。これを呼ばないと StartSchedulers が
// 永続 agent 行全件に cron を載せ、他 peer と二重発火する。
//
// EvictNonLocalAgentsAtStartup は released marker のついた行しか落とさ
// ないため、未finalize / orphan 行は残る。--peer は信頼できる "owned"
// 集合を arrived + lock holder から再構築できるので、Hub よりも厳しく
// pruning できる。
//
// MUST be called AFTER EvictNonLocalAgentsAtStartup and BEFORE
// StartSchedulers。selfPeerID == "" / store nil / Manager nil は no-op で
// true を返す。
//
// 戻り値: ok=true は seed 構築成功 (部分的でも安全側に pruning できた)。
// ok=false は seed 3 read のいずれかが err = m.agents から正当所有を
// 切り分けられない状態。呼出側は ok=false なら fail-closed として
// StartSchedulers の起動を skip する想定。
func (m *Manager) PruneToOwnedAgentsForPeer(ctx context.Context, selfPeerID string) (ok bool) {
	if m == nil || selfPeerID == "" {
		return true
	}
	st := m.Store()
	if st == nil {
		return true
	}
	// Seed の 3 つの read はいずれも destructive detach の前提なので、
	// どれか 1 つでも失敗したら prune を中止し ok=false を返す。
	// 部分集合 seed で進めると正当な arrived agent を落としたり、released
	// agent を残したりする恐れがある。Fencing middleware と
	// AgentLockGuard が後段で誤所属の書込を弾くので、prune skip → 上位で
	// StartSchedulers skip という fail-closed が safe。
	arrived, aerr := m.ListArrivedAgents(ctx)
	if aerr != nil {
		if m.logger != nil {
			m.logger.Warn("PruneToOwnedAgentsForPeer: aborted (arrived markers read failed)", "err", aerr)
		}
		return false
	}
	locks, lerr := st.ListAgentLocksByHolder(ctx, selfPeerID)
	if lerr != nil {
		if m.logger != nil {
			m.logger.Warn("PruneToOwnedAgentsForPeer: aborted (lock holders read failed)", "err", lerr)
		}
		return false
	}
	released, rerr := m.ListReleasedAgents(ctx)
	if rerr != nil {
		if m.logger != nil {
			m.logger.Warn("PruneToOwnedAgentsForPeer: aborted (released markers read failed)", "err", rerr)
		}
		return false
	}

	seed := make(map[string]struct{})
	for _, id := range arrived {
		seed[id] = struct{}{}
	}
	for _, l := range locks {
		seed[l.AgentID] = struct{}{}
	}
	for id := range released {
		delete(seed, id)
	}

	m.mu.Lock()
	ids := make([]string, 0, len(m.agents))
	for id := range m.agents {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		if _, ok := seed[id]; ok {
			continue
		}
		if m.logger != nil {
			m.logger.Info("--peer prune: detaching unowned agent in-memory", "agent", id)
		}
		m.detachAgentInMemory(id)
	}
	return true
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
	case "custom":
		clearClaudeSession(agentID)
	case "grok":
		// Use the counted variant so permission / IO failures
		// surface in the log instead of being silently swallowed.
		// The handler still returns nil on partial failure
		// (matching the claude path and ResetData's stance), but
		// operators get a Warn entry pointing at the cause when
		// the next chat resumes against a leftover session.
		files, sessions, cerr := clearGrokSessionCounted(agentID)
		if cerr != nil {
			m.logger.Warn("CLI session reset: grok clear partial failure",
				"agent", agentID, "err", cerr,
				"filesRemoved", files, "sessionsRemoved", sessions)
		} else {
			m.logger.Debug("CLI session reset: grok cleared",
				"agent", agentID,
				"filesRemoved", files, "sessionsRemoved", sessions)
		}
	}
	// Codex uses ephemeral sessions — no persistent state to clear

	m.logger.Info("CLI session reset", "agent", agentID, "tool", tool)
	return nil
}

package store

import (
	"context"
	"errors"
	"fmt"
)

// PurgeAgentRuntimeStateForRetry drops every per-agent
// device-switch artefact this host carries so a fresh agent-sync
// from a trusted source can land on a clean slate. Unlike
// ForceReclaimAgentToLocal (which leaves the local host as
// holder), this helper does NOT re-insert agent_locks — the
// orchestrator's upcoming agent-sync + AgentLockGuard.AddAgent
// re-creates the row in the right shape. Used by state-probe
// self-heal: detecting a stale row that points at the wrong
// holder means the orchestrator wants to retry the switch, and
// keeping any prior row alive would just trip the next sync's
// existingLock.HolderPeer check with another 409.
//
// Scope (single tx, rolled back on error):
//
//   - agent_locks: DELETE the row outright. The fencing_token
//     counter (agent_fencing_counters) is left in place so the
//     next acquire mints a NEW token — any in-flight write from
//     the previous holder still fails fencing.
//   - blob_refs `kojo://global/agents/<id>/%`: clear
//     handoff_pending so a half-finished switch flag doesn't
//     block the upcoming SetBlobRefHandoffPending in begin.
//     home_peer is intentionally NOT changed here — the row
//     records where the blob body lives, and rewriting it
//     blind would mask a legitimate "blob lives on the source"
//     state. CompleteHandoff drives home_peer to target inside
//     its own tx after the pull succeeds.
//   - kv handoff markers (released/<id>, arrived/<id>,
//     pending/<id>/*): DELETE so the prior switch's audit
//     trail doesn't replay-evict the agent on restart.
//
// Caller is responsible for refusing untrusted invocations —
// this method is the engine, not the gate.
func (s *Store) PurgeAgentRuntimeStateForRetry(ctx context.Context, agentID string) error {
	if agentID == "" {
		return errors.New("store.PurgeAgentRuntimeStateForRetry: agent_id required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.PurgeAgentRuntimeStateForRetry: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM agent_locks WHERE agent_id = ?`, agentID,
	); err != nil {
		return fmt.Errorf("store.PurgeAgentRuntimeStateForRetry: agent_locks: %w", err)
	}

	blobPrefix := "kojo://global/agents/" + agentID + "/"
	blobPrefixEnd := blobPrefix[:len(blobPrefix)-1] + string([]byte{'/' + 1})
	if _, err := tx.ExecContext(ctx,
		`UPDATE blob_refs SET handoff_pending = 0 WHERE uri >= ? AND uri < ? AND handoff_pending = 1`,
		blobPrefix, blobPrefixEnd,
	); err != nil {
		return fmt.Errorf("store.PurgeAgentRuntimeStateForRetry: blob_refs: %w", err)
	}

	releasedKey := "released/" + agentID
	arrivedKey := "arrived/" + agentID
	pendingPrefix := "pending/" + agentID + "/"
	pendingPrefixEnd := pendingPrefix[:len(pendingPrefix)-1] + string([]byte{'/' + 1})
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM kv
		  WHERE namespace = 'handoff'
		    AND (key = ? OR key = ? OR (key >= ? AND key < ?))`,
		releasedKey, arrivedKey, pendingPrefix, pendingPrefixEnd,
	); err != nil {
		return fmt.Errorf("store.PurgeAgentRuntimeStateForRetry: kv handoff: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store.PurgeAgentRuntimeStateForRetry: commit: %w", err)
	}
	return nil
}

// ForceReclaimAgentToLocal restores ONE agent's distributed state
// to a "this host owns the runtime" baseline atomically. Every row
// the §3.7 device-switch ever flipped — agent_locks, blob_refs,
// handoff kv markers — converges to local-self in a single
// transaction so a partial failure either updates every table or
// none. Designed to be the SOLE recovery path that operators (or
// the startup reconciler) drive when a switch left state in an
// unrecoverable shape:
//
//   - agent_locks: row upserted with holder_peer = localPeerID,
//     allowed_proxy_peer = localPeerID, fencing_token bumped via
//     agent_fencing_counters (so any in-flight write from the
//     former holder is invalidated), lease refreshed.
//   - blob_refs `kojo://global/agents/<id>/%`: home_peer rewritten
//     to localPeerID, handoff_pending cleared. Without this the
//     next device-switch attempt would 409 wrong_source because
//     blob_refs still claims the agent lives elsewhere.
//   - kv namespace="handoff":
//     - `released/<id>` marker deleted (otherwise startup eviction
//       throws the just-reclaimed runtime away).
//     - `arrived/<id>` marker deleted (stale arrival from prior
//       half-finished switch would mask the next legitimate one).
//     - `pending/<id>/*` sealed agent-sync tokens deleted (a
//       stranded sync entry would let a retry of the prior
//       orchestrator step double-commit).
//
// Caller MUST refresh the in-memory cache + side channels after
// this call returns; the store has no view into the runtime layer.
//
// `now` is injected for tests; pass NowMillis() in production.
// `leaseDurationMs` follows AgentLockLeaseDuration to match what
// AgentLockGuard's refresh loop expects.
func (s *Store) ForceReclaimAgentToLocal(ctx context.Context, agentID, localPeerID string, now, leaseDurationMs int64) (*AgentLockRecord, error) {
	if agentID == "" {
		return nil, errors.New("store.ForceReclaimAgentToLocal: agent_id required")
	}
	if localPeerID == "" {
		return nil, errors.New("store.ForceReclaimAgentToLocal: local_peer required")
	}
	if leaseDurationMs <= 0 {
		return nil, errors.New("store.ForceReclaimAgentToLocal: lease duration must be > 0")
	}
	if now == 0 {
		now = NowMillis()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store.ForceReclaimAgentToLocal: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// --- agent_locks: upsert (holder, allowed_proxy_peer) = local
	token, err := nextFencingToken(ctx, tx, agentID)
	if err != nil {
		return nil, fmt.Errorf("store.ForceReclaimAgentToLocal: %w", err)
	}
	expires := now + leaseDurationMs
	const lockUpsert = `
INSERT INTO agent_locks (agent_id, holder_peer, fencing_token, lease_expires_at, acquired_at, allowed_proxy_peer)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(agent_id) DO UPDATE SET
  holder_peer        = excluded.holder_peer,
  fencing_token      = excluded.fencing_token,
  lease_expires_at   = excluded.lease_expires_at,
  acquired_at        = excluded.acquired_at,
  allowed_proxy_peer = excluded.allowed_proxy_peer`
	if _, err := tx.ExecContext(ctx, lockUpsert,
		agentID, localPeerID, token, expires, now, localPeerID,
	); err != nil {
		return nil, fmt.Errorf("store.ForceReclaimAgentToLocal: agent_locks: %w", err)
	}

	// --- blob_refs: every per-agent blob points at local again
	blobPrefix := "kojo://global/agents/" + agentID + "/"
	const blobUpd = `
UPDATE blob_refs
   SET home_peer = ?, handoff_pending = 0, updated_at = ?
 WHERE uri >= ? AND uri < ?`
	// Range scan against (uri >= prefix AND uri < prefix+\x7f)
	// hits the primary key index instead of forcing LIKE's
	// case-folding probe. The high bound below replaces the
	// trailing slash with the next ASCII codepoint, which is
	// past any legitimate kojo URI tail.
	blobPrefixEnd := blobPrefix[:len(blobPrefix)-1] + string([]byte{'/' + 1})
	if _, err := tx.ExecContext(ctx, blobUpd,
		localPeerID, now, blobPrefix, blobPrefixEnd,
	); err != nil {
		return nil, fmt.Errorf("store.ForceReclaimAgentToLocal: blob_refs: %w", err)
	}

	// --- kv handoff markers: drop released/arrived/pending for this agent.
	const kvDel = `
DELETE FROM kv
 WHERE namespace = 'handoff'
   AND (
        key = ? OR
        key = ? OR
        (key >= ? AND key < ?)
   )`
	releasedKey := "released/" + agentID
	arrivedKey := "arrived/" + agentID
	pendingPrefix := "pending/" + agentID + "/"
	pendingPrefixEnd := pendingPrefix[:len(pendingPrefix)-1] + string([]byte{'/' + 1})
	if _, err := tx.ExecContext(ctx, kvDel,
		releasedKey, arrivedKey, pendingPrefix, pendingPrefixEnd,
	); err != nil {
		return nil, fmt.Errorf("store.ForceReclaimAgentToLocal: kv handoff: %w", err)
	}

	rec, err := scanAgentLockTx(ctx, tx, agentID)
	if err != nil {
		return nil, fmt.Errorf("store.ForceReclaimAgentToLocal: post-read: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store.ForceReclaimAgentToLocal: commit: %w", err)
	}
	return rec, nil
}

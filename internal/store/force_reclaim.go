package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
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
	blobPrefixEnd, _ := nextPrefix(blobPrefix)
	if _, err := tx.ExecContext(ctx,
		`UPDATE blob_refs SET handoff_pending = 0 WHERE uri >= ? AND uri < ? AND handoff_pending = 1`,
		blobPrefix, blobPrefixEnd,
	); err != nil {
		return fmt.Errorf("store.PurgeAgentRuntimeStateForRetry: blob_refs: %w", err)
	}

	releasedKey := "released/" + agentID
	arrivedKey := "arrived/" + agentID
	pendingPrefix := "pending/" + agentID + "/"
	pendingPrefixEnd, _ := nextPrefix(pendingPrefix)
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
//   - `released/<id>` marker deleted (otherwise startup eviction
//     throws the just-reclaimed runtime away).
//   - `arrived/<id>` marker deleted (stale arrival from prior
//     half-finished switch would mask the next legitimate one).
//   - `pending/<id>/*` sealed agent-sync tokens deleted (a
//     stranded sync entry would let a retry of the prior
//     orchestrator step double-commit).
//
// Caller MUST refresh the in-memory cache + side channels after
// this call returns; the store has no view into the runtime layer.
//
// `now` is injected for tests; pass NowMillis() in production.
// `leaseDurationMs` follows AgentLockLeaseDuration to match what
// AgentLockGuard's refresh loop expects.
func (s *Store) ForceReclaimAgentToLocal(ctx context.Context, agentID, localPeerID string, now, leaseDurationMs int64) (*AgentLockRecord, error) {
	return s.ForceReclaimAgentToLocalStamped(ctx, agentID, localPeerID, now, leaseDurationMs, 0)
}

// ReclaimedAtNamespace holds, per agent, "<reclaimedAt>:<fencingToken>"
// for this device's latest force-reclaim. Binding it to the token scopes
// it to one ownership epoch: once the lock row is released, re-acquired
// or taken by another peer, the stamp no longer matches and is ignored.
const ReclaimedAtNamespace = "handoff_reclaimed_at"

// ForceReclaimAgentToLocalStamped is ForceReclaimAgentToLocal that also
// records reclaimedAt (when > 0) under ReclaimedAtNamespace in the same
// transaction, bound to the new fencing token.
func (s *Store) ForceReclaimAgentToLocalStamped(ctx context.Context, agentID, localPeerID string, now, leaseDurationMs, reclaimedAt int64) (*AgentLockRecord, error) {
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
	if reclaimedAt > 0 {
		if err := upsertMachineKVTx(ctx, tx, ReclaimedAtNamespace, agentID,
			strconv.FormatInt(reclaimedAt, 10)+":"+strconv.FormatInt(token, 10), now); err != nil {
			return nil, fmt.Errorf("store.ForceReclaimAgentToLocal: reclaim stamp: %w", err)
		}
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
	blobPrefixEnd, _ := nextPrefix(blobPrefix)
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
	pendingPrefixEnd, _ := nextPrefix(pendingPrefix)
	if _, err := tx.ExecContext(ctx, kvDel,
		releasedKey, arrivedKey, pendingPrefix, pendingPrefixEnd,
	); err != nil {
		return nil, fmt.Errorf("store.ForceReclaimAgentToLocal: kv handoff: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE incoming_handoffs SET phase='aborted' WHERE agent_id=? AND phase!='done'`, agentID); err != nil {
		return nil, err
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

// AgentFullSyncMarkerKey is the machine-local kv key (namespace
// "handoff") that forces the next incoming device-switch for agentID to
// ship a full snapshot instead of an incremental delta. It is written
// when this peer is told that another device force-reclaimed an agent it
// still believed it owned: rows written here after the reclaim diverge
// from the reclaimer's history (same seq / same memory natural key with
// different ids), so an incremental merge would either drop rows or
// collide on idx_memory_entries_alive_natkey. Cleared by
// SyncAgentFromPeer once a full (non-incremental) snapshot lands.
func AgentFullSyncMarkerKey(agentID string) string { return "full_sync/" + agentID }

// HasAgentFullSyncMarker reports whether AgentFullSyncMarkerKey is set.
func (s *Store) HasAgentFullSyncMarker(ctx context.Context, agentID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM kv WHERE namespace = 'handoff' AND key = ?`,
		AgentFullSyncMarkerKey(agentID)).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store.HasAgentFullSyncMarker: %w", err)
	}
	return n > 0, nil
}

// ReleaseReclaimedAgentHere is the old-holder half of force-reclaim. The
// reclaiming device rewrote only ITS OWN agent_locks row; this peer may
// still record itself as holder (agent_locks.holder_peer == localPeerID)
// or, after a graceful stop wiped the lock rows, still carry the durable
// arrived/<id> marker that re-seeds AgentLockGuard on the next boot.
// Either form keeps a second runtime alive (split-brain: duplicate cron /
// checkins, diverging transcript) and makes every later switch back here
// fail authorizeIncomingSource with ErrStaleHandoff.
//
// When this peer does claim the agent, in one transaction:
//
//   - agent_locks: DELETE our own row and bump agent_fencing_counters so
//     any in-flight write from the local runtime fails fencing and every
//     pending incoming_handoffs generation is invalidated. A row naming
//     another peer is left alone.
//
//   - incoming_handoffs: non-terminal rows → aborted (mirrors
//     ForceReclaimAgentToLocal).
//
//   - blob_refs: clear handoff_pending; home_peer is left alone (see
//     PurgeAgentRuntimeStateForRetry).
//
//   - kv handoff markers arrived/, arrived_proxy/, pending/ → DELETE so a
//     restart does not resurrect the runtime.
//
//   - kv handoff full_sync/<id> (value reclaimerPeerID) and released/<id>
//     → UPSERT, so the next switch back is a full snapshot and startup
//     eviction drops the agent even if the runtime teardown below never
//     ran.
//
// The caller tears down the in-memory runtime AFTER (cron / notify /
// one-shots, AgentLockGuard).
//
// Returns released=false, without touching anything, when this peer does
// not claim the agent (no own lock row and no arrival marker) — e.g. it already handed the agent on, so the reclaim notice
// is obsolete here.
func (s *Store) ReleaseReclaimedAgentHere(ctx context.Context, agentID, localPeerID, reclaimerPeerID string) (bool, error) {
	if agentID == "" {
		return false, errors.New("store.ReleaseReclaimedAgentHere: agent_id required")
	}
	if localPeerID == "" {
		return false, errors.New("store.ReleaseReclaimedAgentHere: local_peer required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store.ReleaseReclaimedAgentHere: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	claims, err := localPeerClaimsAgentTx(ctx, tx, agentID, localPeerID)
	if err != nil {
		return false, fmt.Errorf("store.ReleaseReclaimedAgentHere: %w", err)
	}
	if !claims {
		return false, nil
	}

	// Only our own lock row goes. A row naming another peer (claim came
	// from a leftover arrival marker) is that peer's shadow and may still
	// back an incoming-handoff authorization, so it and the fencing
	// counter stay untouched.
	res, err := tx.ExecContext(ctx,
		`DELETE FROM agent_locks WHERE agent_id = ? AND holder_peer = ?`, agentID, localPeerID)
	if err != nil {
		return false, fmt.Errorf("store.ReleaseReclaimedAgentHere: agent_locks: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store.ReleaseReclaimedAgentHere: agent_locks: %w", err)
	}
	// The counter row is FK'd to agents; skip the bump for an agent this
	// DB never registered (arrival marker without an agents row).
	var hasAgent int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agents WHERE id = ?`, agentID).Scan(&hasAgent); err != nil {
		return false, fmt.Errorf("store.ReleaseReclaimedAgentHere: agent lookup: %w", err)
	}
	if deleted > 0 && hasAgent > 0 {
		if _, err := nextFencingToken(ctx, tx, agentID); err != nil {
			return false, fmt.Errorf("store.ReleaseReclaimedAgentHere: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE incoming_handoffs SET phase='aborted' WHERE agent_id=? AND phase NOT IN ('done','aborted')`,
		agentID,
	); err != nil {
		return false, fmt.Errorf("store.ReleaseReclaimedAgentHere: incoming_handoffs: %w", err)
	}
	blobPrefix := "kojo://global/agents/" + agentID + "/"
	blobPrefixEnd, _ := nextPrefix(blobPrefix)
	if _, err := tx.ExecContext(ctx,
		`UPDATE blob_refs SET handoff_pending = 0 WHERE uri >= ? AND uri < ? AND handoff_pending = 1`,
		blobPrefix, blobPrefixEnd,
	); err != nil {
		return false, fmt.Errorf("store.ReleaseReclaimedAgentHere: blob_refs: %w", err)
	}
	pendingPrefix := "pending/" + agentID + "/"
	pendingPrefixEnd, _ := nextPrefix(pendingPrefix)
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM kv
		  WHERE namespace = 'handoff'
		    AND (key = ? OR key = ? OR (key >= ? AND key < ?))`,
		"arrived/"+agentID, "arrived_proxy/"+agentID, pendingPrefix, pendingPrefixEnd,
	); err != nil {
		return false, fmt.Errorf("store.ReleaseReclaimedAgentHere: kv handoff: %w", err)
	}
	// Durable in the same tx as the release, so a 200 to the reclaimer
	// (which then drops its pending notice) never leaves a state a
	// restart could resurrect, nor an incremental sync over diverged rows.
	now := NowMillis()
	if err := upsertMachineKVTx(ctx, tx, "handoff", AgentFullSyncMarkerKey(agentID), reclaimerPeerID, now); err != nil {
		return false, fmt.Errorf("store.ReleaseReclaimedAgentHere: full-sync marker: %w", err)
	}
	if err := upsertMachineKVTx(ctx, tx, "handoff", "released/"+agentID,
		time.UnixMilli(now).UTC().Format(time.RFC3339Nano), now); err != nil {
		return false, fmt.Errorf("store.ReleaseReclaimedAgentHere: released marker: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store.ReleaseReclaimedAgentHere: commit: %w", err)
	}
	return true, nil
}

// upsertMachineKVTx is PutKV's unconditional upsert for a plain machine-
// scoped string row, inside the caller's tx.
func upsertMachineKVTx(ctx context.Context, tx *sql.Tx, namespace, key, value string, now int64) error {
	rec := KVRecord{
		Namespace: namespace,
		Key:       key,
		Value:     value,
		Type:      KVTypeString,
		Scope:     KVScopeMachine,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}
	var curVersion int64
	err := tx.QueryRowContext(ctx,
		`SELECT version, created_at FROM kv WHERE namespace = ? AND key = ?`,
		rec.Namespace, rec.Key).Scan(&curVersion, &rec.CreatedAt)
	switch {
	case err == nil:
		rec.Version = curVersion + 1
	case errors.Is(err, sql.ErrNoRows):
		rec.CreatedAt = now
	default:
		return err
	}
	rec.ETag, err = computeKVETag(&rec)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO kv (
  namespace, key, value, value_encrypted, type, secret, scope,
  version, etag, created_at, updated_at
) VALUES (?, ?, ?, NULL, ?, 0, ?, ?, ?, ?, ?)
ON CONFLICT(namespace, key) DO UPDATE SET
  value = excluded.value,
  value_encrypted = NULL,
  type = excluded.type,
  secret = 0,
  scope = excluded.scope,
  version = excluded.version,
  etag = excluded.etag,
  updated_at = excluded.updated_at`,
		rec.Namespace, rec.Key, rec.Value, string(rec.Type), string(rec.Scope),
		rec.Version, rec.ETag, rec.CreatedAt, rec.UpdatedAt)
	return err
}

// LocalPeerClaimsAgent is the read-only precheck for
// ReleaseReclaimedAgentHere (same predicate, outside a transaction).
func (s *Store) LocalPeerClaimsAgent(ctx context.Context, agentID, localPeerID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	return localPeerClaimsAgentTx(ctx, tx, agentID, localPeerID)
}

func localPeerClaimsAgentTx(ctx context.Context, tx *sql.Tx, agentID, localPeerID string) (bool, error) {
	var holder sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT holder_peer FROM agent_locks WHERE agent_id = ?`, agentID).Scan(&holder)
	switch {
	case err == nil:
		if holder.String == localPeerID {
			return true, nil
		}
	case errors.Is(err, sql.ErrNoRows):
	default:
		return false, fmt.Errorf("agent_locks lookup: %w", err)
	}
	// No own row: an arrival marker still re-seeds AgentLockGuard on boot
	// (and the guard takes over a foreign row once its lease lapses here).
	var arrived int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM kv WHERE namespace = 'handoff' AND key = ?`,
		"arrived/"+agentID).Scan(&arrived); err != nil {
		return false, fmt.Errorf("arrival marker lookup: %w", err)
	}
	return arrived > 0, nil
}

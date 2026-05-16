package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// AgentLockRecord mirrors one row of `agent_locks`. Per agent_id at
// most one row exists (PK is agent_id); the row records which peer
// currently holds the lock plus the fencing_token paired with the
// holder. Every agent-runtime write must present this token alongside
// its op so a delayed write from an old holder is rejected after lease
// expiry — see docs §3.7 fencing.
type AgentLockRecord struct {
	AgentID         string
	HolderPeer      string
	FencingToken    int64
	LeaseExpiresAt  int64
	AcquiredAt      int64
}

// ErrLockHeld signals that AcquireAgentLock found a live lock held by
// a different peer. Callers (Hub) handle this as "the agent is busy on
// peer X" and surface it to the UI; the standard remediation is to
// wait for lease expiry then retry.
var ErrLockHeld = errors.New("store: agent lock held by another peer")

// ErrFencingMismatch signals that the supplied fencing_token does not
// match the row currently in agent_locks. Used by every agent-runtime
// write path so a peer that lost its lock cannot keep appending after
// the lease expired and another peer took over (split-brain
// prevention).
var ErrFencingMismatch = errors.New("store: fencing token mismatch")

// nextFencingToken advances the per-agent counter inside `tx` and
// returns the new value. The counter is the "last issued token" for
// agent_id; advancing means UPDATE counter+1 if the row exists, else
// INSERT 1. Both paths run inside the caller's tx so the read-modify-
// write cycle is atomic with respect to the agent_locks update that
// pairs with it.
//
// agent_fencing_counters is FK'd to agents with ON DELETE CASCADE;
// counters never roll backward — even on Release / ReleaseByPeer, the
// counter is left intact so a delayed write from an old holder with
// the prior token gets rejected by the next Acquire's bumped value.
func nextFencingToken(ctx context.Context, tx *sql.Tx, agentID string) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE agent_fencing_counters SET next_token = next_token + 1 WHERE agent_id = ?`,
		agentID,
	)
	if err != nil {
		return 0, fmt.Errorf("store.nextFencingToken: update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store.nextFencingToken: rows affected: %w", err)
	}
	if n == 0 {
		// Counter row missing. Two cases:
		//   1. Brand-new agent — INSERT at 1.
		//   2. Pre-0002 DB state where agent_locks may already hold a
		//      row whose fencing_token > 1 (the migration's backfill
		//      should have caught this, but a partially-applied
		//      migration or a path that skipped the backfill needs
		//      defense in depth — otherwise a steal here would issue
		//      a token <= the old holder's, and the old holder's
		//      delayed write would slip through CheckFencing).
		//
		// We pick the larger of (1, existing fencing_token + 1).
		var existing sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT fencing_token FROM agent_locks WHERE agent_id = ?`,
			agentID,
		).Scan(&existing); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("store.nextFencingToken: backfill check: %w", err)
		}
		initial := int64(1)
		if existing.Valid && existing.Int64 >= initial {
			initial = existing.Int64 + 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO agent_fencing_counters (agent_id, next_token) VALUES (?, ?)`,
			agentID, initial,
		); err != nil {
			return 0, fmt.Errorf("store.nextFencingToken: insert: %w", err)
		}
		return initial, nil
	}
	var token int64
	if err := tx.QueryRowContext(ctx,
		`SELECT next_token FROM agent_fencing_counters WHERE agent_id = ?`,
		agentID,
	).Scan(&token); err != nil {
		return 0, fmt.Errorf("store.nextFencingToken: read back: %w", err)
	}
	return token, nil
}

// AcquireAgentLock takes the lock for (agentID, peer) under a fresh
// fencing_token. The semantics:
//
//   - If no row exists yet, INSERT with a freshly minted token from
//     agent_fencing_counters (1 for the very first acquisition,
//     monotonically advancing for every subsequent post-Release
//     re-acquisition).
//   - If a row exists held by `peer` (re-acquire from same holder),
//     refresh lease_expires_at and KEEP the existing fencing_token —
//     the holder's prior writes stay valid.
//   - If a row exists held by another peer:
//     - lease still alive → return ErrLockHeld with the current row
//       so the caller can show "busy on peer X" without a second
//       SELECT.
//     - lease expired → reassign to `peer` and INCREMENT the counter
//       so any in-flight write from the old holder gets rejected.
//
// All branches run inside one BEGIN IMMEDIATE tx to serialize
// concurrent acquisitions; the writer-lock-up-front recipe is the
// same one AppendMessage / etc. use to dodge SQLITE_BUSY_SNAPSHOT
// under WAL.
//
// `now` is injected so callers (the Hub's lease scheduler) can drive
// deterministic clock for tests; pass NowMillis() in production.
// `leaseDuration` is the milliseconds the lease is granted for; the
// schema is clock-agnostic so the resulting lease_expires_at is
// `now + leaseDuration`.
func (s *Store) AcquireAgentLock(ctx context.Context, agentID, peer string, now, leaseDuration int64) (*AgentLockRecord, error) {
	if agentID == "" {
		return nil, errors.New("store.AcquireAgentLock: agent_id required")
	}
	if peer == "" {
		return nil, errors.New("store.AcquireAgentLock: peer required")
	}
	if leaseDuration <= 0 {
		return nil, errors.New("store.AcquireAgentLock: lease duration must be positive")
	}
	if now == 0 {
		now = NowMillis()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store.AcquireAgentLock: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const sel = `
SELECT agent_id, holder_peer, fencing_token, lease_expires_at, acquired_at
  FROM agent_locks WHERE agent_id = ?`
	var cur AgentLockRecord
	switch err := tx.QueryRowContext(ctx, sel, agentID).Scan(
		&cur.AgentID, &cur.HolderPeer, &cur.FencingToken,
		&cur.LeaseExpiresAt, &cur.AcquiredAt,
	); {
	case errors.Is(err, sql.ErrNoRows):
		// First acquisition for this agent (or first since the prior
		// holder Released). Pull a fresh token from the per-agent
		// counter — never reuse 1 across a release-reacquire cycle.
		token, err := nextFencingToken(ctx, tx, agentID)
		if err != nil {
			return nil, err
		}
		const ins = `
INSERT INTO agent_locks (agent_id, holder_peer, fencing_token, lease_expires_at, acquired_at)
VALUES (?, ?, ?, ?, ?)`
		if _, err := tx.ExecContext(ctx, ins, agentID, peer, token, now+leaseDuration, now); err != nil {
			return nil, fmt.Errorf("store.AcquireAgentLock: insert: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("store.AcquireAgentLock: commit: %w", err)
		}
		return &AgentLockRecord{
			AgentID:        agentID,
			HolderPeer:     peer,
			FencingToken:   token,
			LeaseExpiresAt: now + leaseDuration,
			AcquiredAt:     now,
		}, nil

	case err != nil:
		return nil, fmt.Errorf("store.AcquireAgentLock: select: %w", err)
	}

	// Row exists. Three cases below.
	switch {
	case cur.HolderPeer == peer:
		// Same-peer re-acquire. Refresh the lease, keep fencing_token
		// + acquired_at — any in-flight writes from this peer remain
		// valid because their token continues to match. Counter is
		// NOT advanced.
		const upd = `UPDATE agent_locks SET lease_expires_at = ? WHERE agent_id = ?`
		if _, err := tx.ExecContext(ctx, upd, now+leaseDuration, agentID); err != nil {
			return nil, fmt.Errorf("store.AcquireAgentLock: refresh: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("store.AcquireAgentLock: commit: %w", err)
		}
		cur.LeaseExpiresAt = now + leaseDuration
		return &cur, nil

	case cur.LeaseExpiresAt > now:
		// Live lease held by someone else — reject. We still commit
		// the (no-op) tx to release the writer lock promptly; defer
		// Rollback above is a safety net.
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("store.AcquireAgentLock: commit: %w", err)
		}
		return &cur, ErrLockHeld

	default:
		// Lease expired — steal. Bump the counter so the prior
		// holder's delayed writes get rejected with
		// ErrFencingMismatch.
		token, err := nextFencingToken(ctx, tx, agentID)
		if err != nil {
			return nil, err
		}
		const upd = `
UPDATE agent_locks
   SET holder_peer      = ?,
       fencing_token    = ?,
       lease_expires_at = ?,
       acquired_at      = ?
 WHERE agent_id = ?`
		if _, err := tx.ExecContext(ctx, upd, peer, token, now+leaseDuration, now, agentID); err != nil {
			return nil, fmt.Errorf("store.AcquireAgentLock: steal: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("store.AcquireAgentLock: commit: %w", err)
		}
		return &AgentLockRecord{
			AgentID:        agentID,
			HolderPeer:     peer,
			FencingToken:   token,
			LeaseExpiresAt: now + leaseDuration,
			AcquiredAt:     now,
		}, nil
	}
}

// RefreshAgentLock extends the lease on an already-held lock. Returns
// ErrFencingMismatch if (peer, fencingToken) doesn't match the row;
// ErrNotFound if no row exists for agent_id. Both diagnoses are run
// inside the same tx as the conditional UPDATE so a concurrent steal
// can't cause the diagnose-SELECT to disagree with the UPDATE about
// which case actually fired (BEGIN IMMEDIATE serializes writers).
func (s *Store) RefreshAgentLock(ctx context.Context, agentID, peer string, fencingToken, now, leaseDuration int64) (*AgentLockRecord, error) {
	if agentID == "" {
		return nil, errors.New("store.RefreshAgentLock: agent_id required")
	}
	if peer == "" {
		return nil, errors.New("store.RefreshAgentLock: peer required")
	}
	if fencingToken <= 0 {
		return nil, errors.New("store.RefreshAgentLock: fencing_token must be > 0")
	}
	if leaseDuration <= 0 {
		return nil, errors.New("store.RefreshAgentLock: lease duration must be positive")
	}
	if now == 0 {
		now = NowMillis()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store.RefreshAgentLock: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const upd = `
UPDATE agent_locks
   SET lease_expires_at = ?
 WHERE agent_id = ? AND holder_peer = ? AND fencing_token = ?`
	res, err := tx.ExecContext(ctx, upd, now+leaseDuration, agentID, peer, fencingToken)
	if err != nil {
		return nil, fmt.Errorf("store.RefreshAgentLock: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("store.RefreshAgentLock: rows affected: %w", err)
	}
	if n == 0 {
		// Row exists vs row missing — same-tx diagnostic so a
		// concurrent steal/release can't switch the answer between
		// the failed UPDATE and the SELECT below.
		var dummy int
		switch err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM agent_locks WHERE agent_id = ?`, agentID,
		).Scan(&dummy); {
		case errors.Is(err, sql.ErrNoRows):
			return nil, ErrNotFound
		case err != nil:
			return nil, fmt.Errorf("store.RefreshAgentLock: diag: %w", err)
		default:
			// Row exists but predicate didn't match — fence mismatch.
			rec, err := scanAgentLockTx(ctx, tx, agentID)
			if err != nil {
				return nil, err
			}
			return rec, ErrFencingMismatch
		}
	}
	rec, err := scanAgentLockTx(ctx, tx, agentID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store.RefreshAgentLock: commit: %w", err)
	}
	return rec, nil
}

// TransferAgentLock atomically moves the lock from currentPeer to
// newPeer, bumping the fencing_token via the per-agent counter so
// a delayed write from currentPeer with the prior token fails
// CheckFencing on the new value.
//
// docs §3.7 step 6 — the device-switch handoff moves the lock to
// the target peer AFTER its blob pull succeeded (step 5). Without
// the token bump, a write from the source peer that was queued in
// flight could land on the target's row.
//
// Returns ErrNotFound when no row matches (agent_id has no lock
// or current holder differs); ErrFencingMismatch when the lock
// row exists but the (currentPeer, currentToken) tuple doesn't
// match the row.
func (s *Store) TransferAgentLock(ctx context.Context, agentID, currentPeer string, currentToken int64, newPeer string, leaseDurationMs, now int64) (*AgentLockRecord, error) {
	if agentID == "" {
		return nil, errors.New("store.TransferAgentLock: agent_id required")
	}
	if currentPeer == "" || newPeer == "" {
		return nil, errors.New("store.TransferAgentLock: current and new peer required")
	}
	if currentToken <= 0 {
		return nil, errors.New("store.TransferAgentLock: current_token must be > 0")
	}
	if leaseDurationMs <= 0 {
		return nil, errors.New("store.TransferAgentLock: lease duration must be > 0")
	}
	if now == 0 {
		now = NowMillis()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store.TransferAgentLock: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Fencing precondition under the writer lock.
	if err := s.CheckFencingTx(ctx, tx, agentID, currentPeer, currentToken); err != nil {
		return nil, err
	}
	newToken, err := nextFencingToken(ctx, tx, agentID)
	if err != nil {
		return nil, fmt.Errorf("store.TransferAgentLock: %w", err)
	}
	expires := now + leaseDurationMs
	const upd = `
UPDATE agent_locks
   SET holder_peer = ?, fencing_token = ?, lease_expires_at = ?, acquired_at = ?
 WHERE agent_id = ?`
	res, err := tx.ExecContext(ctx, upd, newPeer, newToken, expires, now, agentID)
	if err != nil {
		return nil, fmt.Errorf("store.TransferAgentLock: update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("store.TransferAgentLock: rows affected: %w", err)
	}
	if n == 0 {
		return nil, ErrNotFound
	}
	rec, err := scanAgentLockTx(ctx, tx, agentID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store.TransferAgentLock: commit: %w", err)
	}
	return rec, nil
}

// CompleteHandoffResult bundles the outcome of one atomic device-
// switch complete. agent_locks transfer + every blob_refs.home_peer
// flip run in ONE tx; SwitchedURIs / AlreadyAtTargetURIs let the
// orchestrator render the per-row report without a follow-up read.
type CompleteHandoffResult struct {
	// Lock is the agent_locks row state after the tx. nil when no
	// lock row exists for this agent (transient pre-acquire state).
	Lock *AgentLockRecord
	// LockTransferred reports whether holder_peer + fencing_token
	// moved as part of this call. False on the idempotent re-call
	// (holder already at target) — the row state in Lock still
	// reflects the converged target.
	LockTransferred bool
	// SwitchedURIs is the set of blob_refs URIs whose home_peer
	// just flipped to target (and whose handoff_pending cleared).
	SwitchedURIs []string
	// AlreadyAtTargetURIs is the set of URIs in the agent's prefix
	// range that were already at target before this call —
	// idempotent re-call detection.
	AlreadyAtTargetURIs []string
	// LeftoverURIs is the set of URIs in the agent's prefix range
	// whose state is neither "switched by us" nor "already at
	// target" (e.g. handoff_pending=0 but home_peer is still the
	// source). The orchestrator surfaces these so an operator
	// knows complete did not converge every row.
	LeftoverURIs []string
}

// CompleteHandoff atomically transfers the agent_lock from its
// current holder to targetPeer AND switches every blob_refs row in
// the agent's prefix (kojo://global/agents/<id>/) to home_peer=
// target AND clears handoff_pending. All three mutations run in
// ONE transaction so a crash between them rolls back to the pre-
// call state — no half-completed handoff can survive a daemon
// restart.
//
// Idempotency: when the lock is ALREADY at targetPeer the
// fencing_token is NOT re-bumped (that would invalidate the
// target's current writes); the handler echoes the existing row
// and only the blob_refs loop runs.
//
// Returns ErrFencingMismatch when the lock exists but its
// (holder, token) tuple doesn't match a freshly-read current state
// — a concurrent abort / steal raced us. Returns ErrNotFound when
// the agent has no agent_locks row at all AND no blob_refs rows in
// the prefix; callers treat that as "no state to migrate" and
// surface it as a 404. A no-lock-but-blobs case proceeds normally
// (blobs switch, LockTransferred=false).
func (s *Store) CompleteHandoff(ctx context.Context, agentID, targetPeer, blobURIPrefix string, leaseDurationMs int64) (*CompleteHandoffResult, error) {
	if agentID == "" {
		return nil, errors.New("store.CompleteHandoff: agent_id required")
	}
	if targetPeer == "" {
		return nil, errors.New("store.CompleteHandoff: target peer required")
	}
	if blobURIPrefix == "" {
		return nil, errors.New("store.CompleteHandoff: blob uri prefix required")
	}
	if leaseDurationMs <= 0 {
		return nil, errors.New("store.CompleteHandoff: lease duration must be > 0")
	}
	upper, hasUpper := nextPrefix(blobURIPrefix)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store.CompleteHandoff: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := NowMillis()
	result := &CompleteHandoffResult{}

	// --- Step 1: lock transfer (idempotent) -------------------
	cur, lerr := scanAgentLockTx(ctx, tx, agentID)
	switch {
	case lerr == nil && cur.HolderPeer == targetPeer:
		// Already transferred. No token bump.
		result.Lock = cur
		result.LockTransferred = false
	case lerr == nil:
		// Fencing precondition: predicate the UPDATE on the
		// freshly-read (holder, token). SQLite's BEGIN
		// transaction is DEFERRED — the read above doesn't
		// take a write lock, so a concurrent TransferAgentLock
		// could land between our scan and update. Without this
		// predicate the UPDATE would clobber their state; with
		// it, RowsAffected==0 surfaces as ErrFencingMismatch.
		newToken, terr := nextFencingToken(ctx, tx, agentID)
		if terr != nil {
			return nil, fmt.Errorf("store.CompleteHandoff: fencing: %w", terr)
		}
		expires := now + leaseDurationMs
		res, terr := tx.ExecContext(ctx,
			`UPDATE agent_locks
			    SET holder_peer = ?, fencing_token = ?, lease_expires_at = ?, acquired_at = ?
			  WHERE agent_id = ? AND holder_peer = ? AND fencing_token = ?`,
			targetPeer, newToken, expires, now, agentID, cur.HolderPeer, cur.FencingToken)
		if terr != nil {
			return nil, fmt.Errorf("store.CompleteHandoff: lock update: %w", terr)
		}
		if n, raErr := res.RowsAffected(); raErr == nil && n == 0 {
			return nil, ErrFencingMismatch
		}
		// Re-scan so the returned record carries the new token.
		updated, serr := scanAgentLockTx(ctx, tx, agentID)
		if serr != nil {
			return nil, fmt.Errorf("store.CompleteHandoff: rescan lock: %w", serr)
		}
		result.Lock = updated
		result.LockTransferred = true
	case errors.Is(lerr, ErrNotFound):
		// No lock — proceed with the blob-only path.
		result.Lock = nil
		result.LockTransferred = false
	default:
		return nil, fmt.Errorf("store.CompleteHandoff: read lock: %w", lerr)
	}

	// --- Step 2: switch blob_refs rows still handoff_pending --
	switchQ := `
UPDATE blob_refs
   SET home_peer = ?, handoff_pending = 0, updated_at = ?
 WHERE uri >= ? AND handoff_pending = 1`
	switchArgs := []any{targetPeer, now, blobURIPrefix}
	if hasUpper {
		switchQ += ` AND uri < ?`
		switchArgs = append(switchArgs, upper)
	}
	switchQ += ` RETURNING uri`
	rows, err := tx.QueryContext(ctx, switchQ, switchArgs...)
	if err != nil {
		return nil, fmt.Errorf("store.CompleteHandoff: switch blobs: %w", err)
	}
	for rows.Next() {
		var uri string
		if err := rows.Scan(&uri); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("store.CompleteHandoff: scan switched uri: %w", err)
		}
		result.SwitchedURIs = append(result.SwitchedURIs, uri)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("store.CompleteHandoff: switched rows: %w", err)
	}
	rows.Close()

	// --- Step 3: classify untouched rows for the report ------
	//
	// Rows in the prefix that we DIDN'T just switch fall into two
	// buckets: already at target (idempotent re-call) or stuck at
	// source-with-handoff_pending=0 (leftover from a partial state).
	// The orchestrator surfaces leftovers so the operator knows
	// complete didn't fully converge.
	classifyQ := `
SELECT uri, home_peer, handoff_pending
  FROM blob_refs
 WHERE uri >= ? AND handoff_pending = 0`
	classifyArgs := []any{blobURIPrefix}
	if hasUpper {
		classifyQ += ` AND uri < ?`
		classifyArgs = append(classifyArgs, upper)
	}
	rows2, err := tx.QueryContext(ctx, classifyQ, classifyArgs...)
	if err != nil {
		return nil, fmt.Errorf("store.CompleteHandoff: classify: %w", err)
	}
	switched := make(map[string]struct{}, len(result.SwitchedURIs))
	for _, u := range result.SwitchedURIs {
		switched[u] = struct{}{}
	}
	for rows2.Next() {
		var uri, home string
		var pending int
		if err := rows2.Scan(&uri, &home, &pending); err != nil {
			_ = rows2.Close()
			return nil, fmt.Errorf("store.CompleteHandoff: scan classify row: %w", err)
		}
		if _, ok := switched[uri]; ok {
			// We just flipped this one to home=target,
			// pending=0; it'll re-appear in the classify pass
			// because handoff_pending=0 matches. Skip it from
			// the "already" / "leftover" buckets.
			continue
		}
		if home == targetPeer {
			result.AlreadyAtTargetURIs = append(result.AlreadyAtTargetURIs, uri)
		} else {
			result.LeftoverURIs = append(result.LeftoverURIs, uri)
		}
	}
	if err := rows2.Err(); err != nil {
		_ = rows2.Close()
		return nil, fmt.Errorf("store.CompleteHandoff: classify rows: %w", err)
	}
	rows2.Close()

	// --- Step 4: commit everything atomically -----------------
	if result.Lock == nil &&
		len(result.SwitchedURIs) == 0 &&
		len(result.AlreadyAtTargetURIs) == 0 &&
		len(result.LeftoverURIs) == 0 {
		// Nothing to do — neither a lock nor blob rows for this
		// agent. Surface ErrNotFound so the caller knows complete
		// found no state to migrate; the tx rolls back cleanly.
		return nil, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store.CompleteHandoff: commit: %w", err)
	}
	return result, nil
}

// ReleaseAgentLock removes the row iff (peer, fencingToken) matches.
// The fencing-token guard prevents a peer that lost the lock from
// "releasing" it on behalf of the new holder. Idempotent in the sense
// that calling Release again after success returns ErrNotFound, not
// ErrFencingMismatch.
//
// The agent_fencing_counters row is intentionally NOT cleared so a
// subsequent Acquire on the same agent_id keeps advancing past every
// prior token; a delayed write from this caller after Release will
// fail CheckFencing on the new token.
//
// As with Refresh, the conditional DELETE and the diagnostic SELECT
// run in the same tx so the answer cannot flip under concurrent
// writers.
func (s *Store) ReleaseAgentLock(ctx context.Context, agentID, peer string, fencingToken int64) error {
	if agentID == "" {
		return errors.New("store.ReleaseAgentLock: agent_id required")
	}
	if peer == "" {
		return errors.New("store.ReleaseAgentLock: peer required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.ReleaseAgentLock: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const q = `
DELETE FROM agent_locks
 WHERE agent_id = ? AND holder_peer = ? AND fencing_token = ?`
	res, err := tx.ExecContext(ctx, q, agentID, peer, fencingToken)
	if err != nil {
		return fmt.Errorf("store.ReleaseAgentLock: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.ReleaseAgentLock: rows affected: %w", err)
	}
	if n == 0 {
		var dummy int
		switch err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM agent_locks WHERE agent_id = ?`, agentID,
		).Scan(&dummy); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("store.ReleaseAgentLock: diag: %w", err)
		default:
			return ErrFencingMismatch
		}
	}
	return tx.Commit()
}

// ReleaseAgentLockByPeer drops every lock row currently held by peer.
// Used during peer decommission and on Hub-driven failover (an admin
// has marked the peer offline; its locks must clear so other peers can
// pick them up). Returns the number of rows actually removed.
//
// The agent_fencing_counters rows are intentionally retained so the
// next Acquire on each affected agent advances past the prior token —
// without that, a delayed write from the just-released peer would be
// silently accepted by a new holder that picked up token=1.
func (s *Store) ReleaseAgentLockByPeer(ctx context.Context, peer string) (int64, error) {
	if peer == "" {
		return 0, errors.New("store.ReleaseAgentLockByPeer: peer required")
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM agent_locks WHERE holder_peer = ?`, peer,
	)
	if err != nil {
		return 0, fmt.Errorf("store.ReleaseAgentLockByPeer: %w", err)
	}
	return res.RowsAffected()
}

// ListAgentLocksByHolder returns every agent_locks row whose
// holder_peer matches the given peer. Returns an empty slice when
// no rows match — never ErrNotFound, because the absence of rows
// is itself the intended answer (this peer doesn't hold anything).
//
// Used by --peer startup to seed AgentLockGuard's desired set from
// durable state: in peer mode the in-memory list is empty until a
// live finalize fires AddAgent, so a daemon restart between handoff
// and any subsequent activity would otherwise let the lease expire
// and ownership become recoverable by another peer. Lease expiry
// is NOT consulted here — even an expired row is meaningful (the
// guard's refresh loop will re-Acquire on the next tick).
func (s *Store) ListAgentLocksByHolder(ctx context.Context, peer string) ([]AgentLockRecord, error) {
	if peer == "" {
		return nil, errors.New("store.ListAgentLocksByHolder: peer required")
	}
	const q = `
SELECT agent_id, holder_peer, fencing_token, lease_expires_at, acquired_at
  FROM agent_locks WHERE holder_peer = ?`
	rows, err := s.db.QueryContext(ctx, q, peer)
	if err != nil {
		return nil, fmt.Errorf("store.ListAgentLocksByHolder: %w", err)
	}
	defer rows.Close()
	out := make([]AgentLockRecord, 0)
	for rows.Next() {
		rec, err := scanAgentLockRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store.ListAgentLocksByHolder: scan: %w", err)
		}
		out = append(out, *rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListAgentLocksByHolder: rows: %w", err)
	}
	return out, nil
}

// GetAgentLock returns the row for agent_id or ErrNotFound.
func (s *Store) GetAgentLock(ctx context.Context, agentID string) (*AgentLockRecord, error) {
	const q = `
SELECT agent_id, holder_peer, fencing_token, lease_expires_at, acquired_at
  FROM agent_locks WHERE agent_id = ?`
	rec, err := scanAgentLockRow(s.db.QueryRowContext(ctx, q, agentID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// CheckFencing returns nil iff agent_locks contains a row for agent_id
// with the given (peer, fencing_token). Convenience wrapper around
// CheckFencingTx using a short-lived read tx.
//
// IMPORTANT: this performs a single SELECT, so the result is only
// valid until something else writes — there is a TOCTOU window between
// CheckFencing and the caller's subsequent write where another peer
// could steal the lock and bump the token. Production write paths
// MUST use CheckFencingTx inside the same BEGIN IMMEDIATE transaction
// that performs the write. CheckFencing exists for read-only / non-
// critical callers (audit endpoints, tests) where the window is
// tolerable.
func (s *Store) CheckFencing(ctx context.Context, agentID, peer string, fencingToken int64) error {
	cur, err := s.GetAgentLock(ctx, agentID)
	if err != nil {
		return err
	}
	if cur.HolderPeer != peer || cur.FencingToken != fencingToken {
		return ErrFencingMismatch
	}
	return nil
}

// CheckFencingTx is the tx-scoped variant write paths must use. The
// caller opens BEGIN IMMEDIATE (or s.db.BeginTx(ctx, nil) — kojo's DSN
// pins txlock=immediate so the writer lock is held up front), calls
// CheckFencingTx as the FIRST statement, then issues its INSERT /
// UPDATE / DELETE inside the same tx. Because BEGIN IMMEDIATE has
// already taken the WAL writer lock, no other writer can steal the
// agent_locks row between the check and the gated write.
//
// Returns ErrNotFound if no row exists, ErrFencingMismatch if the row
// exists but the predicate doesn't match.
func (s *Store) CheckFencingTx(ctx context.Context, tx *sql.Tx, agentID, peer string, fencingToken int64) error {
	if agentID == "" {
		return errors.New("store.CheckFencingTx: agent_id required")
	}
	if peer == "" {
		return errors.New("store.CheckFencingTx: peer required")
	}
	if fencingToken <= 0 {
		return errors.New("store.CheckFencingTx: fencing_token must be > 0")
	}
	const q = `
SELECT 1 FROM agent_locks
 WHERE agent_id = ? AND holder_peer = ? AND fencing_token = ?`
	var dummy int
	switch err := tx.QueryRowContext(ctx, q, agentID, peer, fencingToken).Scan(&dummy); {
	case errors.Is(err, sql.ErrNoRows):
		// Distinguish "no row" from "row exists with different
		// predicate" so callers can branch correctly. A second
		// SELECT inside the same tx is safe: BEGIN IMMEDIATE has
		// the writer lock, so concurrent writers are blocked, and
		// the read sees the same snapshot.
		var d2 int
		switch err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM agent_locks WHERE agent_id = ?`, agentID,
		).Scan(&d2); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("store.CheckFencingTx: diag: %w", err)
		default:
			return ErrFencingMismatch
		}
	case err != nil:
		return fmt.Errorf("store.CheckFencingTx: %w", err)
	}
	return nil
}

func scanAgentLockRow(r rowScanner) (*AgentLockRecord, error) {
	var rec AgentLockRecord
	if err := r.Scan(
		&rec.AgentID, &rec.HolderPeer, &rec.FencingToken,
		&rec.LeaseExpiresAt, &rec.AcquiredAt,
	); err != nil {
		return nil, err
	}
	return &rec, nil
}

// scanAgentLockTx fetches the lock row inside an open tx. Used by
// Refresh's success-path return value so the caller sees the row's
// post-update state without a second non-tx round-trip.
func scanAgentLockTx(ctx context.Context, tx *sql.Tx, agentID string) (*AgentLockRecord, error) {
	const q = `
SELECT agent_id, holder_peer, fencing_token, lease_expires_at, acquired_at
  FROM agent_locks WHERE agent_id = ?`
	rec, err := scanAgentLockRow(tx.QueryRowContext(ctx, q, agentID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

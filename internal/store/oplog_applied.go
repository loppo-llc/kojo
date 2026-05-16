package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// OplogAppliedRecord mirrors one row of the oplog_applied ledger.
// docs/multi-device-storage.md §3.13.1 — recorded inside the
// dispatch tx so a peer's retry returns the saved etag without
// re-running the write.
type OplogAppliedRecord struct {
	OpID        string
	AgentID     string
	Fingerprint string
	ResultETag  string
	AppliedAt   int64
}

// IdempotencyTag is the op-log applied-ledger key + fingerprint
// the store records alongside a write. When non-nil on a write
// helper's Idempotency field, the function:
//
//  1. Probes oplog_applied(OpID) inside its tx.
//     - hit + fingerprint match: returns the saved etag without
//       re-running the write.
//     - hit + fingerprint MISMATCH: returns ErrOplogOpIDReused
//       (a peer-side bug — the same op_id was used for two
//       different (table, op, body) tuples).
//     - hit + agent_id mismatch: returns ErrOplogOpIDReused for
//       the same reason — the ledger is keyed on op_id, and the
//       claimed agent must match the ledger row.
//  2. Runs the write.
//  3. Records oplog_applied(OpID, agent_id, fingerprint, etag) in
//     the same tx so a crash between dispatch commit and ledger
//     write is impossible.
//
// Fingerprint is opaque to the store — the caller (oplog_handler)
// computes sha256 over (table || sep || op || sep || body) so a
// replay with the same op_id but a different body is detected.
type IdempotencyTag struct {
	OpID        string
	Fingerprint string
}

// ErrOplogOpIDReused is returned when an idempotency probe finds a
// ledger row whose fingerprint or agent_id doesn't match the
// caller's claim. Indicates a peer-side bug; surfaces as a
// per-entry error in the receive handler.
var ErrOplogOpIDReused = errors.New("store: op_id reused with different fingerprint or agent")

// GetOplogApplied returns the ledger row for opID. Returns
// ErrNotFound when no row exists.
func (s *Store) GetOplogApplied(ctx context.Context, opID string) (*OplogAppliedRecord, error) {
	if opID == "" {
		return nil, errors.New("store.GetOplogApplied: op_id required")
	}
	const q = `SELECT op_id, agent_id, fingerprint, result_etag, applied_at
	             FROM oplog_applied WHERE op_id = ?`
	rec, err := scanOplogAppliedRow(s.db.QueryRowContext(ctx, q, opID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// getOplogAppliedTx is the tx-scoped variant. Used by the write
// helpers to probe the ledger inside the same tx that does the
// write — closes the crash window between commit and ledger
// record that the standalone Get/Record pair couldn't.
func getOplogAppliedTx(ctx context.Context, tx *sql.Tx, opID string) (*OplogAppliedRecord, error) {
	const q = `SELECT op_id, agent_id, fingerprint, result_etag, applied_at
	             FROM oplog_applied WHERE op_id = ?`
	rec, err := scanOplogAppliedRow(tx.QueryRowContext(ctx, q, opID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// recordOplogAppliedTx writes the ledger row inside an open tx.
// INSERT OR FAIL surfaces concurrent op_id reuse as a clean error
// the caller can wrap with ErrOplogOpIDReused; INSERT OR IGNORE
// would silently swallow a genuine inconsistency. The caller is
// expected to have already verified there is no prior row (the
// helper that calls this also calls getOplogAppliedTx first).
func recordOplogAppliedTx(ctx context.Context, tx *sql.Tx, rec *OplogAppliedRecord) error {
	if rec == nil || rec.OpID == "" || rec.AgentID == "" ||
		rec.Fingerprint == "" || rec.ResultETag == "" {
		return errors.New("store.recordOplogAppliedTx: required field missing")
	}
	const q = `INSERT INTO oplog_applied
	             (op_id, agent_id, fingerprint, result_etag, applied_at)
	           VALUES (?, ?, ?, ?, ?)`
	if _, err := tx.ExecContext(ctx, q,
		rec.OpID, rec.AgentID, rec.Fingerprint, rec.ResultETag, rec.AppliedAt,
	); err != nil {
		return fmt.Errorf("store.recordOplogAppliedTx: %w", err)
	}
	return nil
}

// checkOplogIdempotency runs the ledger probe + reuse-conflict
// check for a write helper. Returns:
//   - (savedRec, nil) when a matching row exists; caller short-
//     circuits and returns savedRec.ResultETag without re-writing.
//   - (nil, nil) when no prior row exists; caller proceeds with
//     the write + recordOplogAppliedTx at the end.
//   - (nil, ErrOplogOpIDReused) when a row exists but with a
//     different (agent_id, fingerprint) — peer-side bug.
//   - (nil, err) for any other DB error.
func checkOplogIdempotency(ctx context.Context, tx *sql.Tx, tag *IdempotencyTag, expectedAgentID string) (*OplogAppliedRecord, error) {
	if tag == nil {
		return nil, nil
	}
	if tag.OpID == "" || tag.Fingerprint == "" {
		return nil, errors.New("store: idempotency tag missing op_id or fingerprint")
	}
	prior, err := getOplogAppliedTx(ctx, tx, tag.OpID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if prior.AgentID != expectedAgentID || prior.Fingerprint != tag.Fingerprint {
		return nil, ErrOplogOpIDReused
	}
	return prior, nil
}

// scanOplogAppliedRow is the shared row scanner. Splitting out so
// the standalone Get / tx variant produce identical record shapes.
func scanOplogAppliedRow(row interface {
	Scan(dest ...any) error
}) (*OplogAppliedRecord, error) {
	var rec OplogAppliedRecord
	if err := row.Scan(&rec.OpID, &rec.AgentID, &rec.Fingerprint, &rec.ResultETag, &rec.AppliedAt); err != nil {
		return nil, err
	}
	return &rec, nil
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// -------- agent_workspace_files (singleton per-kind workspace md per agent) --

// WorkspaceFileKind is the typed alias for the `kind` column on
// agent_workspace_files. The CHECK constraint at the SQL layer also
// enforces this; mirroring the allowed values here turns a buggy caller
// into a typed error rather than a sqlite "CHECK constraint failed"
// surfacing through three layers of UI. Keep in sync with
// 0013_agent_workspace_files.sql.
type WorkspaceFileKind string

const (
	// WorkspaceFileKindUser is the user-authored workspace markdown
	// (user.md). Free-form prose the user maintains for the agent; the
	// CLI process consumes it via the on-disk mirror.
	WorkspaceFileKindUser WorkspaceFileKind = "user"

	// WorkspaceFileKindCheckin is the check-in markdown (checkin.md)
	// used as the prompt body for cron-scheduled check-ins. Previously
	// stored as settings_json.cronMessage; see the migration header
	// for the data-migration story.
	WorkspaceFileKindCheckin WorkspaceFileKind = "checkin"

	// WorkspaceFileKindStatus is the agent's self-maintained state file
	// (status.json on disk — JSON, not markdown): freeform key-value
	// pairs (mood, energy, sleepiness, affection, ...) injected into the
	// system prompt tail and edited by the agent itself as its state
	// drifts. Added by migration 0021.
	WorkspaceFileKindStatus WorkspaceFileKind = "status"
)

// validWorkspaceFileKinds mirrors the SQL CHECK constraint. Map shape
// matches validMemoryKinds in memory.go for consistency.
var validWorkspaceFileKinds = map[WorkspaceFileKind]bool{
	WorkspaceFileKindUser:    true,
	WorkspaceFileKindCheckin: true,
	WorkspaceFileKindStatus:  true,
}

// IsValidWorkspaceFileKind reports whether kind is one of the
// workspace-file kinds the table accepts. Mirrors validMemoryKinds in
// memory.go — adding a new kind requires both a schema migration and
// an update here so the contract stays visible in code review.
func IsValidWorkspaceFileKind(kind WorkspaceFileKind) bool {
	return validWorkspaceFileKinds[kind]
}

// AgentWorkspaceFileRecord mirrors the `agent_workspace_files` table. The
// body is the denormalized copy of the on-disk file under
// `<v1>/global/agents/<id>/<kind>.md`; the daemon syncs the file into
// this row (and back out again) using the workspace-sync reconcile path.
//
// Unlike AgentMemoryRecord this struct intentionally has no LastTxID
// column: workspace files are user-driven content (the user edits user.md
// or checkin.md directly), not CLI-tx outputs that need to be tied back
// to a specific oplog write transaction. The §2.5 intent-file protocol
// does not apply.
type AgentWorkspaceFileRecord struct {
	AgentID    string
	Kind       WorkspaceFileKind
	Body       string
	BodySHA256 string

	Seq       int64
	Version   int
	ETag      string
	CreatedAt int64
	UpdatedAt int64
	DeletedAt *int64
	PeerID    string
}

// agentWorkspaceFileETagInput is the canonical record hashed into the
// etag. Includes AgentID + Kind so two different kinds for the same
// agent never collide, plus body_sha256 / updated_at / deleted_at so
// the etag shifts whenever any user-observable column changes. Same
// shape as agentMemoryETagInput minus LastTxID.
type agentWorkspaceFileETagInput struct {
	AgentID    string `json:"agent_id"`
	Kind       string `json:"kind"`
	BodySHA256 string `json:"body_sha256"`
	UpdatedAt  int64  `json:"updated_at"`
	DeletedAt  *int64 `json:"deleted_at"`
}

func computeAgentWorkspaceFileETag(r *AgentWorkspaceFileRecord) (string, error) {
	return CanonicalETag(r.Version, agentWorkspaceFileETagInput{
		AgentID:    r.AgentID,
		Kind:       string(r.Kind),
		BodySHA256: r.BodySHA256,
		UpdatedAt:  r.UpdatedAt,
		DeletedAt:  r.DeletedAt,
	})
}

// AgentWorkspaceFileInsertOptions mirrors AgentMemoryInsertOptions minus
// LastTxID. Fencing makes the upsert atomic with an agent_locks holder
// check; Idempotency makes a replayed op_id short-circuit to the prior
// etag.
type AgentWorkspaceFileInsertOptions struct {
	Now            int64
	Seq            int64
	CreatedAt      int64
	UpdatedAt      int64
	PeerID         string
	AllowOverwrite bool
	Fencing        *FencingPredicate
	Idempotency    *IdempotencyTag
}

// UpsertAgentWorkspaceFile writes (or replaces) the singleton workspace
// file row for (agentID, kind). Mirrors UpsertAgentMemory exactly — see
// that function for the rationale on AllowOverwrite, ifMatchETag, and
// the live-vs-tombstone branching.
//
// Three branches: (1) live row → version-bumping UPDATE in place; (2)
// tombstoned row → DELETE + fresh INSERT to revive (preserves no
// history — the previous tombstone is gone); (3) no row → plain INSERT.
// Branch (2) keeps the primary key intact: SQLite's ON CONFLICT clauses
// would also work but require schema-level UPSERT support that varies
// between dialects, and the explicit branching makes the resurrection
// path easier to reason about in review.
func (s *Store) UpsertAgentWorkspaceFile(ctx context.Context, agentID string, kind WorkspaceFileKind, body, ifMatchETag string, opts AgentWorkspaceFileInsertOptions) (*AgentWorkspaceFileRecord, error) {
	if agentID == "" {
		return nil, errors.New("store.UpsertAgentWorkspaceFile: agent_id required")
	}
	if !IsValidWorkspaceFileKind(kind) {
		return nil, fmt.Errorf("store.UpsertAgentWorkspaceFile: invalid kind %q", string(kind))
	}
	now := opts.Now
	if now == 0 {
		now = NowMillis()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Idempotency probe runs BEFORE fencing — exact replay of a
	// committed write succeeds even after the lock has rotated.
	if prior, err := checkOplogIdempotency(ctx, tx, opts.Idempotency, agentID); err != nil {
		return nil, fmt.Errorf("store.UpsertAgentWorkspaceFile: %w", err)
	} else if prior != nil {
		existing, gerr := s.getAgentWorkspaceFileTx(ctx, tx, agentID, kind)
		if gerr != nil {
			return nil, fmt.Errorf("store.UpsertAgentWorkspaceFile: idempotent re-read: %w", gerr)
		}
		return existing, nil
	}
	// Fencing gate runs SECOND.
	if err := checkFencingPredicate(ctx, s, tx, opts.Fencing, agentID); err != nil {
		return nil, fmt.Errorf("store.UpsertAgentWorkspaceFile: %w", err)
	}

	var alive int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM agents WHERE id = ? AND deleted_at IS NULL`, agentID,
	).Scan(&alive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("store.UpsertAgentWorkspaceFile: agent %q: %w", agentID, ErrNotFound)
		}
		return nil, err
	}

	const sel = `
SELECT body, body_sha256, seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
  FROM agent_workspace_files WHERE agent_id = ? AND kind = ?`
	var (
		prev       AgentWorkspaceFileRecord
		deletedAt  sql.NullInt64
		liveExists bool
		anyRow     bool
	)
	prev.AgentID = agentID
	prev.Kind = kind
	err = tx.QueryRowContext(ctx, sel, agentID, string(kind)).Scan(
		&prev.Body, &prev.BodySHA256, &prev.Seq, &prev.Version,
		&prev.ETag, &prev.CreatedAt, &prev.UpdatedAt, &deletedAt, &prev.PeerID,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		anyRow = true
		if deletedAt.Valid {
			v := deletedAt.Int64
			prev.DeletedAt = &v
		} else {
			liveExists = true
		}
	}

	if err := checkSingletonUpsertPrecondition(liveExists, prev.ETag, ifMatchETag, opts.AllowOverwrite); err != nil {
		return nil, err
	}

	rec := AgentWorkspaceFileRecord{
		AgentID:    agentID,
		Kind:       kind,
		Body:       body,
		BodySHA256: SHA256Hex([]byte(body)),
		PeerID:     opts.PeerID,
	}
	if liveExists {
		rec.Seq = prev.Seq
		rec.Version = prev.Version + 1
		rec.CreatedAt = prev.CreatedAt
	} else {
		if opts.Seq != 0 {
			rec.Seq = opts.Seq
		} else {
			rec.Seq = NextGlobalSeq()
		}
		rec.Version = 1
		if opts.CreatedAt != 0 {
			rec.CreatedAt = opts.CreatedAt
		} else {
			rec.CreatedAt = now
		}
	}
	if opts.UpdatedAt != 0 {
		rec.UpdatedAt = opts.UpdatedAt
	} else {
		rec.UpdatedAt = now
	}
	rec.ETag, err = computeAgentWorkspaceFileETag(&rec)
	if err != nil {
		return nil, err
	}

	if liveExists {
		const upd = `
UPDATE agent_workspace_files SET
  body = ?, body_sha256 = ?,
  version = ?, etag = ?, updated_at = ?, peer_id = ?
WHERE agent_id = ? AND kind = ? AND deleted_at IS NULL`
		if _, err := tx.ExecContext(ctx, upd,
			rec.Body, rec.BodySHA256,
			rec.Version, rec.ETag, rec.UpdatedAt, nullableText(rec.PeerID),
			agentID, string(kind),
		); err != nil {
			return nil, err
		}
	} else {
		if anyRow {
			// Tombstone exists at the same PK — the partial unique
			// index on seq plus the PK collision both block a plain
			// INSERT, so wipe the dead row first. We lose the prior
			// tombstone's metadata, which is fine: tombstones are not
			// historical records, they're a soft-delete sentinel.
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM agent_workspace_files WHERE agent_id = ? AND kind = ?`,
				agentID, string(kind),
			); err != nil {
				return nil, err
			}
		}
		const ins = `
INSERT INTO agent_workspace_files (
  agent_id, kind, body, body_sha256,
  seq, version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`
		if _, err := tx.ExecContext(ctx, ins,
			rec.AgentID, string(rec.Kind), rec.Body, rec.BodySHA256,
			rec.Seq, rec.Version, rec.ETag, rec.CreatedAt, rec.UpdatedAt, nullableText(rec.PeerID),
		); err != nil {
			return nil, err
		}
	}
	if opts.Idempotency != nil {
		if err := recordOplogAppliedTx(ctx, tx, &OplogAppliedRecord{
			OpID:        opts.Idempotency.OpID,
			AgentID:     rec.AgentID,
			Fingerprint: opts.Idempotency.Fingerprint,
			ResultETag:  rec.ETag,
			AppliedAt:   rec.UpdatedAt,
		}); err != nil {
			return nil, fmt.Errorf("store.UpsertAgentWorkspaceFile: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &rec, nil
}

// GetAgentWorkspaceFile returns the workspace file row for (agentID, kind),
// or ErrNotFound when the row is missing OR tombstoned. Filters out
// soft-deleted parents the same way persona/memory reads do — a tombstoned
// agent has no readable workspace files.
func (s *Store) GetAgentWorkspaceFile(ctx context.Context, agentID string, kind WorkspaceFileKind) (*AgentWorkspaceFileRecord, error) {
	if !IsValidWorkspaceFileKind(kind) {
		return nil, fmt.Errorf("store.GetAgentWorkspaceFile: invalid kind %q", string(kind))
	}
	const q = `
SELECT w.body, w.body_sha256, w.seq, w.version, w.etag,
       w.created_at, w.updated_at, w.deleted_at, COALESCE(w.peer_id,'')
  FROM agent_workspace_files w
  JOIN agents              a ON a.id = w.agent_id
 WHERE w.agent_id = ? AND w.kind = ? AND w.deleted_at IS NULL AND a.deleted_at IS NULL`
	var (
		rec       AgentWorkspaceFileRecord
		deletedAt sql.NullInt64
	)
	rec.AgentID = agentID
	rec.Kind = kind
	err := s.db.QueryRowContext(ctx, q, agentID, string(kind)).Scan(
		&rec.Body, &rec.BodySHA256, &rec.Seq, &rec.Version,
		&rec.ETag, &rec.CreatedAt, &rec.UpdatedAt, &deletedAt, &rec.PeerID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if deletedAt.Valid {
		v := deletedAt.Int64
		rec.DeletedAt = &v
	}
	return &rec, nil
}

// getAgentWorkspaceFileTx is the tx-scoped variant used by the
// idempotency short-circuit path in UpsertAgentWorkspaceFile. Skips the
// agents-table join on purpose: a replay probe fires BEFORE the
// alive-agent SELECT in the upsert path, so an agent that was
// tombstoned after the original write must still be able to surface
// the prior result (same as agent_memory). The idempotent re-read is
// about reproducing the exact prior outcome, not re-validating the
// parent's current liveness.
func (s *Store) getAgentWorkspaceFileTx(ctx context.Context, tx *sql.Tx, agentID string, kind WorkspaceFileKind) (*AgentWorkspaceFileRecord, error) {
	const q = `
SELECT w.body, w.body_sha256, w.seq, w.version, w.etag,
       w.created_at, w.updated_at, w.deleted_at, COALESCE(w.peer_id,'')
  FROM agent_workspace_files w
 WHERE w.agent_id = ? AND w.kind = ? AND w.deleted_at IS NULL`
	var (
		rec       AgentWorkspaceFileRecord
		deletedAt sql.NullInt64
	)
	rec.AgentID = agentID
	rec.Kind = kind
	err := tx.QueryRowContext(ctx, q, agentID, string(kind)).Scan(
		&rec.Body, &rec.BodySHA256, &rec.Seq, &rec.Version,
		&rec.ETag, &rec.CreatedAt, &rec.UpdatedAt, &deletedAt, &rec.PeerID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if deletedAt.Valid {
		v := deletedAt.Int64
		rec.DeletedAt = &v
	}
	return &rec, nil
}

// WorkspaceFileListOptions filters ListAgentWorkspaceFiles. All zero-
// value fields are no-ops; the default list is "live rows for the
// agent, ordered by seq ASC".
//
//   - Kind: when non-empty, restricts to one workspace-file kind.
//     Unknown kinds return an error rather than an empty list so a
//     buggy caller surfaces immediately.
//   - IncludeDeleted: when true, tombstoned rows are returned. The
//     §3.7 device-switch delta path MUST set this — otherwise a
//     workspace file the user deleted on device A will silently
//     stick around on device B because the tombstone never crosses
//     the wire.
//   - UpdatedAtSince: when > 0, returns rows whose updated_at >=
//     UpdatedAtSince (inclusive — same-millisecond writes are NOT
//     skipped). Ordering switches to updated_at ASC, kind ASC so
//     ties at the same wall-clock millisecond are stable, and the
//     caller resumes by replaying the last-seen updated_at and
//     deduping on (agent_id, kind) — never by adding +1ms, which
//     would lose any concurrent same-ms write.
type WorkspaceFileListOptions struct {
	Kind           WorkspaceFileKind
	IncludeDeleted bool
	UpdatedAtSince int64
}

// ListAgentWorkspaceFiles returns workspace-file rows for agentID,
// honoring opts. Drives the peer-sync delta and the workspace-sync
// reconcile loop's "what does the DB say is here?" query.
//
// Always joins agents to filter out tombstoned parents — even with
// IncludeDeleted, a workspace file under a dead agent is uninteresting:
// the cascade delete will fire eventually and the row is on its way
// out. The DB-side ON DELETE CASCADE handles the actual cleanup; this
// filter keeps live readers from observing the doomed rows in the
// interim.
func (s *Store) ListAgentWorkspaceFiles(ctx context.Context, agentID string, opts WorkspaceFileListOptions) ([]*AgentWorkspaceFileRecord, error) {
	if agentID == "" {
		return nil, errors.New("store.ListAgentWorkspaceFiles: agent_id required")
	}
	if opts.Kind != "" && !IsValidWorkspaceFileKind(opts.Kind) {
		return nil, fmt.Errorf("store.ListAgentWorkspaceFiles: invalid kind %q", string(opts.Kind))
	}

	q := `
SELECT w.agent_id, w.kind, w.body, w.body_sha256, w.seq, w.version, w.etag,
       w.created_at, w.updated_at, w.deleted_at, COALESCE(w.peer_id,'')
  FROM agent_workspace_files w
  JOIN agents              a ON a.id = w.agent_id
 WHERE w.agent_id = ? AND a.deleted_at IS NULL`
	args := []any{agentID}
	if !opts.IncludeDeleted {
		q += ` AND w.deleted_at IS NULL`
	}
	if opts.Kind != "" {
		q += ` AND w.kind = ?`
		args = append(args, string(opts.Kind))
	}
	if opts.UpdatedAtSince > 0 {
		q += ` AND w.updated_at >= ?`
		args = append(args, opts.UpdatedAtSince)
		q += ` ORDER BY w.updated_at ASC, w.kind ASC`
	} else {
		q += ` ORDER BY w.seq ASC`
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*AgentWorkspaceFileRecord, 0, 2)
	for rows.Next() {
		rec, err := scanAgentWorkspaceFileRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// scanAgentWorkspaceFileRow is the shared row-scan helper for List.
// Single-row reads (Get / getTx) inline their own scan because the
// projection differs (no agent_id/kind in the SELECT — they're in
// the WHERE clause).
func scanAgentWorkspaceFileRow(r rowScanner) (*AgentWorkspaceFileRecord, error) {
	var (
		rec       AgentWorkspaceFileRecord
		kind      string
		deletedAt sql.NullInt64
	)
	if err := r.Scan(
		&rec.AgentID, &kind, &rec.Body, &rec.BodySHA256, &rec.Seq, &rec.Version,
		&rec.ETag, &rec.CreatedAt, &rec.UpdatedAt, &deletedAt, &rec.PeerID,
	); err != nil {
		return nil, err
	}
	rec.Kind = WorkspaceFileKind(kind)
	if deletedAt.Valid {
		v := deletedAt.Int64
		rec.DeletedAt = &v
	}
	return &rec, nil
}

// SoftDeleteAgentWorkspaceFile tombstones the agent_workspace_files row
// for (agentID, kind). Idempotent (already-tombstoned, missing-row, or
// missing-agent all return nil) so the daemon-side file→DB sync can
// call it on every "file gone, row exists" detection without
// conditionals on the caller side. Mirrors SoftDeleteAgentMemory.
//
// ifMatchETag is an optional optimistic-concurrency precondition
// checked inside the TX. Empty means "tombstone unconditionally" —
// matches the daemon-sync path. Non-empty + mismatch returns
// ErrETagMismatch (NOT idempotent: a stale precondition refuses
// rather than silently no-oping). Non-empty + missing-row also
// surfaces ErrETagMismatch — a caller asserting a specific etag
// against a vanished row needs to refetch, not silently succeed.
//
// Recomputes etag on the way out so cross-device readers observe a
// fresh strong-etag and can distinguish "tombstoned" from the prior
// live state via 304 vs new ETag — the body-sha256 stays put because
// the canonical etag input includes deleted_at, so the tombstone
// itself is what shifts the hash.
func (s *Store) SoftDeleteAgentWorkspaceFile(ctx context.Context, agentID string, kind WorkspaceFileKind, ifMatchETag string) error {
	if agentID == "" {
		return errors.New("store.SoftDeleteAgentWorkspaceFile: agent_id required")
	}
	if !IsValidWorkspaceFileKind(kind) {
		return fmt.Errorf("store.SoftDeleteAgentWorkspaceFile: invalid kind %q", string(kind))
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	const sel = `
SELECT body, body_sha256, seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
  FROM agent_workspace_files WHERE agent_id = ? AND kind = ?`
	var (
		rec       AgentWorkspaceFileRecord
		deletedAt sql.NullInt64
	)
	rec.AgentID = agentID
	rec.Kind = kind
	err = tx.QueryRowContext(ctx, sel, agentID, string(kind)).Scan(
		&rec.Body, &rec.BodySHA256, &rec.Seq, &rec.Version,
		&rec.ETag, &rec.CreatedAt, &rec.UpdatedAt, &deletedAt, &rec.PeerID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		// No row → idempotent for the unconditional caller; for the
		// conditional caller, surface as etag mismatch (something
		// committed before us, refetch).
		if ifMatchETag != "" {
			return ErrETagMismatch
		}
		return nil
	}
	if err != nil {
		return err
	}
	if deletedAt.Valid {
		// Already tombstoned. Idempotent for unconditional callers;
		// surface as etag mismatch when the caller asserted a live
		// etag (the row is now in a state they can't reconcile from
		// without a fresh GET).
		if ifMatchETag != "" {
			return ErrETagMismatch
		}
		return nil
	}
	if ifMatchETag != "" && rec.ETag != ifMatchETag {
		return ErrETagMismatch
	}

	now := NowMillis()
	rec.Version++
	rec.UpdatedAt = now
	rec.DeletedAt = &now
	newETag, err := computeAgentWorkspaceFileETag(&rec)
	if err != nil {
		return err
	}

	const upd = `
UPDATE agent_workspace_files SET
  deleted_at = ?, updated_at = ?, version = ?, etag = ?
WHERE agent_id = ? AND kind = ? AND deleted_at IS NULL`
	if _, err := tx.ExecContext(ctx, upd, now, now, rec.Version, newETag, agentID, string(kind)); err != nil {
		return err
	}
	return tx.Commit()
}

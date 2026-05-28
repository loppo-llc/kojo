package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// MemoryEntryKind enumerates the values memory_entries.kind accepts. Mirroring
// the CHECK constraint here turns a buggy caller into a typed error rather
// than a sqlite "CHECK constraint failed: memory_entries" surfacing through
// three layers of UI. Keep in sync with 0001_initial.sql.
var validMemoryKinds = map[string]bool{
	"daily":   true,
	"project": true,
	"topic":   true,
	"people":  true,
	"archive": true,
}

// MemoryEntryRecord mirrors the `memory_entries` table. Body lives both here
// (canonical) and on the filesystem under `<v1>/global/agents/<id>/memory/<kind>/<name>.md`
// — the blob mirror is wired up in Phase 3.
type MemoryEntryRecord struct {
	ID         string
	AgentID    string
	Seq        int64 // per-agent
	Kind       string
	Name       string
	Body       string
	BodySHA256 string

	Version   int
	ETag      string
	CreatedAt int64
	UpdatedAt int64
	DeletedAt *int64
	PeerID    string
}

type memoryEntryETagInput struct {
	ID         string `json:"id"`
	AgentID    string `json:"agent_id"`
	Seq        int64  `json:"seq"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	BodySHA256 string `json:"body_sha256"`
	UpdatedAt  int64  `json:"updated_at"`
	DeletedAt  *int64 `json:"deleted_at"`
}

func computeMemoryEntryETag(r *MemoryEntryRecord) (string, error) {
	return CanonicalETag(r.Version, memoryEntryETagInput{
		ID:         r.ID,
		AgentID:    r.AgentID,
		Seq:        r.Seq,
		Kind:       r.Kind,
		Name:       r.Name,
		BodySHA256: r.BodySHA256,
		UpdatedAt:  r.UpdatedAt,
		DeletedAt:  r.DeletedAt,
	})
}

// MemoryEntryInsertOptions lets the importer override id/seq/
// timestamps. Fencing, when non-nil, makes the insert atomic with an
// agent_locks holder check (see FencingPredicate).
type MemoryEntryInsertOptions struct {
	Now         int64
	CreatedAt   int64
	UpdatedAt   int64
	Seq         int64 // 0 = allocate next per-agent
	PeerID      string
	Fencing     *FencingPredicate
	Idempotency *IdempotencyTag
}

// InsertMemoryEntry creates a new memory entry under (agent_id, kind, name).
// The schema's partial unique index (idx_memory_entries_alive_natkey) blocks
// duplicates among live rows; resurrecting a previously soft-deleted entry
// with the same natural key requires the caller to hard-delete the tombstone
// first or to use UpsertMemoryEntry which handles the resurrection path.
func (s *Store) InsertMemoryEntry(ctx context.Context, rec *MemoryEntryRecord, opts MemoryEntryInsertOptions) (*MemoryEntryRecord, error) {
	if rec == nil {
		return nil, errors.New("store.InsertMemoryEntry: nil record")
	}
	if rec.ID == "" || rec.AgentID == "" || rec.Name == "" {
		return nil, errors.New("store.InsertMemoryEntry: id/agent_id/name required")
	}
	if !validMemoryKinds[rec.Kind] {
		return nil, fmt.Errorf("store.InsertMemoryEntry: invalid kind %q", rec.Kind)
	}

	now := opts.Now
	if now == 0 {
		now = NowMillis()
	}
	created := opts.CreatedAt
	if created == 0 {
		created = now
	}
	updated := opts.UpdatedAt
	if updated == 0 {
		updated = now
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Idempotency probe runs BEFORE fencing — exact replay of a
	// committed write succeeds even after the lock has rotated.
	if prior, err := checkOplogIdempotency(ctx, tx, opts.Idempotency, rec.AgentID); err != nil {
		return nil, fmt.Errorf("store.InsertMemoryEntry: %w", err)
	} else if prior != nil {
		existing, gerr := s.getMemoryEntryTx(ctx, tx, rec.ID)
		if gerr != nil {
			return nil, fmt.Errorf("store.InsertMemoryEntry: idempotent re-read: %w", gerr)
		}
		return existing, nil
	}
	// Fencing gate runs SECOND.
	if err := checkFencingPredicate(ctx, s, tx, opts.Fencing, rec.AgentID); err != nil {
		return nil, fmt.Errorf("store.InsertMemoryEntry: %w", err)
	}

	// Parent agent must be alive — same invariant as messages/persona.
	var alive int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM agents WHERE id = ? AND deleted_at IS NULL`, rec.AgentID,
	).Scan(&alive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("store.InsertMemoryEntry: agent %q: %w", rec.AgentID, ErrNotFound)
		}
		return nil, err
	}

	seq := opts.Seq
	if seq == 0 {
		var maxSeq sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT MAX(seq) FROM memory_entries WHERE agent_id = ?`, rec.AgentID,
		).Scan(&maxSeq); err != nil {
			return nil, err
		}
		seq = 1
		if maxSeq.Valid {
			seq = maxSeq.Int64 + 1
		}
	}

	out := *rec
	out.Seq = seq
	out.Version = 1
	out.CreatedAt = created
	out.UpdatedAt = updated
	out.PeerID = opts.PeerID
	out.DeletedAt = nil
	out.BodySHA256 = SHA256Hex([]byte(out.Body))
	out.ETag, err = computeMemoryEntryETag(&out)
	if err != nil {
		return nil, err
	}

	const q = `
INSERT INTO memory_entries (
  id, agent_id, seq, kind, name, body, body_sha256,
  version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`
	if _, err := tx.ExecContext(ctx, q,
		out.ID, out.AgentID, out.Seq, out.Kind, out.Name, out.Body, out.BodySHA256,
		out.Version, out.ETag, out.CreatedAt, out.UpdatedAt, nullableText(out.PeerID),
	); err != nil {
		return nil, fmt.Errorf("store.InsertMemoryEntry: %w", err)
	}
	if opts.Idempotency != nil {
		if err := recordOplogAppliedTx(ctx, tx, &OplogAppliedRecord{
			OpID:        opts.Idempotency.OpID,
			AgentID:     out.AgentID,
			Fingerprint: opts.Idempotency.Fingerprint,
			ResultETag:  out.ETag,
			AppliedAt:   out.UpdatedAt,
		}); err != nil {
			return nil, fmt.Errorf("store.InsertMemoryEntry: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &out, nil
}

// getMemoryEntryTx is the tx-scoped re-read used by the
// idempotency short-circuit path. Same query as GetMemoryEntry
// but bound to the open writer tx.
func (s *Store) getMemoryEntryTx(ctx context.Context, tx *sql.Tx, id string) (*MemoryEntryRecord, error) {
	const q = `
SELECT m.id, m.agent_id, m.seq, m.kind, m.name, m.body, m.body_sha256,
       m.version, m.etag, m.created_at, m.updated_at, m.deleted_at, COALESCE(m.peer_id,'')
  FROM memory_entries m
 WHERE m.id = ? AND m.deleted_at IS NULL`
	rec, err := scanMemoryEntryRow(tx.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// GetMemoryEntry returns a single live memory entry by id. ErrNotFound on
// miss, on tombstone, or on tombstoned parent agent.
func (s *Store) GetMemoryEntry(ctx context.Context, id string) (*MemoryEntryRecord, error) {
	const q = `
SELECT m.id, m.agent_id, m.seq, m.kind, m.name, m.body, m.body_sha256,
       m.version, m.etag, m.created_at, m.updated_at, m.deleted_at, COALESCE(m.peer_id,'')
  FROM memory_entries m
  JOIN agents         a ON a.id = m.agent_id
 WHERE m.id = ? AND m.deleted_at IS NULL AND a.deleted_at IS NULL`
	rec, err := scanMemoryEntryRow(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// FindMemoryEntryByName returns the live entry for (agent_id, kind, name).
// Used by the v0→v1 importer (which keys entries by filesystem path) and by
// the merge-queue resolver.
func (s *Store) FindMemoryEntryByName(ctx context.Context, agentID, kind, name string) (*MemoryEntryRecord, error) {
	const q = `
SELECT m.id, m.agent_id, m.seq, m.kind, m.name, m.body, m.body_sha256,
       m.version, m.etag, m.created_at, m.updated_at, m.deleted_at, COALESCE(m.peer_id,'')
  FROM memory_entries m
  JOIN agents         a ON a.id = m.agent_id
 WHERE m.agent_id = ? AND m.kind = ? AND m.name = ?
   AND m.deleted_at IS NULL AND a.deleted_at IS NULL`
	rec, err := scanMemoryEntryRow(s.db.QueryRowContext(ctx, q, agentID, kind, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// MemoryEntryListOptions configures ListMemoryEntries.
type MemoryEntryListOptions struct {
	Kind   string // "" = all kinds
	Limit  int    // 0 = unbounded
	Cursor int64  // seq strictly greater (keyset paging); 0 = from start

	// IncludeDeleted: include soft-deleted rows in the result.
	// Default false (callers see only live entries). The §3.7
	// incremental device-switch path sets true so source can
	// ship tombstones to target; target's syncMemoryEntriesTx
	// upserts them and the row becomes deleted on target too.
	IncludeDeleted bool

	// UpdatedAtSince: return only rows with updated_at >= this.
	// 0 = no filter. `>=` is inclusive — defends against
	// same-millisecond mutations colliding on the boundary,
	// trading idempotent resend of every row sharing the cursor
	// timestamp (typically 1, but a burst of mutations could
	// share a single millisecond) for full coverage.
	// Used by the §3.7 device-switch delta (memory_entries
	// cursor) because seq alone misses in-place body updates
	// and soft-deletes. Order flips to updated_at ASC when this
	// is > 0 so tombstones precede recreations on the same
	// (agent_id, kind, name) — the alive UNIQUE index requires
	// the old row to be tombstoned BEFORE the new row's INSERT
	// lands on target.
	UpdatedAtSince int64
}

// ListMemoryEntries returns the live entries for agentID. Ordered by seq ASC
// — this matches the "most recent at end" semantics the v0 list helpers use
// and keeps cursor pagination monotonic.
func (s *Store) ListMemoryEntries(ctx context.Context, agentID string, opts MemoryEntryListOptions) ([]*MemoryEntryRecord, error) {
	if agentID == "" {
		return nil, errors.New("store.ListMemoryEntries: agent_id required")
	}
	args := []any{agentID}
	q := `
SELECT m.id, m.agent_id, m.seq, m.kind, m.name, m.body, m.body_sha256,
       m.version, m.etag, m.created_at, m.updated_at, m.deleted_at, COALESCE(m.peer_id,'')
  FROM memory_entries m
  JOIN agents         a ON a.id = m.agent_id
 WHERE m.agent_id = ? AND a.deleted_at IS NULL`
	if !opts.IncludeDeleted {
		q += ` AND m.deleted_at IS NULL`
	}
	if opts.Kind != "" {
		if !validMemoryKinds[opts.Kind] {
			return nil, fmt.Errorf("store.ListMemoryEntries: invalid kind %q", opts.Kind)
		}
		q += ` AND m.kind = ?`
		args = append(args, opts.Kind)
	}
	if opts.Cursor > 0 {
		q += ` AND m.seq > ?`
		args = append(args, opts.Cursor)
	}
	if opts.UpdatedAtSince > 0 {
		// `>=`, not `>`, to defend against same-millisecond
		// mutations. NowMillis() resolves at 1 ms; a burst of
		// rapid InsertMemoryEntry / SoftDelete calls can share a
		// single updated_at value, and a `>` cursor would skip
		// every row at the boundary that target hasn't observed
		// yet. `>=` plus ON CONFLICT(id) DO UPDATE on the
		// receiver makes this idempotent — every row sharing
		// the cursor timestamp (one or more) is overwritten with
		// the same content (no observable change). Cost: a few
		// redundant rows per switch, dominated by whatever
		// concurrent burst happened to land on the boundary ms.
		q += ` AND m.updated_at >= ?`
		args = append(args, opts.UpdatedAtSince)
	}
	// UpdatedAtSince filter implies the §3.7 delta path —
	// emit updated_at ASC so a tombstone update precedes the
	// recreation insert that reused its (kind,name) slot.
	// Otherwise stay on seq ASC (the doc-facing "most recent
	// at end" order callers expect for UI listing / FTS index).
	if opts.UpdatedAtSince > 0 {
		q += ` ORDER BY m.updated_at ASC, m.seq ASC`
	} else {
		q += ` ORDER BY m.seq ASC`
	}
	if opts.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, opts.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MemoryEntryRecord
	for rows.Next() {
		rec, err := scanMemoryEntryRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// MemoryEntryPatch supports partial updates. Pass nil to leave a field
// unchanged.
type MemoryEntryPatch struct {
	Kind *string
	Name *string
	Body *string
	// Fencing, when non-nil, makes the patch atomic with an
	// agent_locks holder check (see FencingPredicate). The
	// predicate's agent_id must match the entry's agent_id; the
	// store re-reads the row inside the tx and verifies the
	// match before applying the patch.
	Fencing *FencingPredicate
	// Idempotency makes the patch crash-safe across the dispatch
	// commit boundary — a prior matching ledger row short-
	// circuits the update.
	Idempotency *IdempotencyTag
}

// UpdateMemoryEntry applies patch to the entry identified by id with optional
// If-Match. Recomputes BodySHA256 + ETag.
func (s *Store) UpdateMemoryEntry(ctx context.Context, id, ifMatchETag string, patch MemoryEntryPatch) (*MemoryEntryRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	const sel = `
SELECT m.id, m.agent_id, m.seq, m.kind, m.name, m.body, m.body_sha256,
       m.version, m.etag, m.created_at, m.updated_at, m.deleted_at, COALESCE(m.peer_id,'')
  FROM memory_entries m
  JOIN agents         a ON a.id = m.agent_id
 WHERE m.id = ? AND m.deleted_at IS NULL AND a.deleted_at IS NULL`
	cur, err := scanMemoryEntryRow(tx.QueryRowContext(ctx, sel, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// Idempotency probe runs BEFORE fencing — exact replay of a
	// committed write succeeds even after the lock has rotated.
	// The probe is gated on cur.AgentID (the row's true owner),
	// which is exactly the scope check we want: a tag claiming a
	// different agent surfaces as ErrOplogOpIDReused.
	if prior, err := checkOplogIdempotency(ctx, tx, patch.Idempotency, cur.AgentID); err != nil {
		return nil, fmt.Errorf("store.UpdateMemoryEntry: %w", err)
	} else if prior != nil {
		return cur, nil
	}
	// Fencing gate runs AFTER the row read so we can check the
	// predicate's claimed agent_id against the row's actual
	// agent_id — without this scope check, a peer holding the
	// lock for agent A could patch a memory entry that belongs
	// to agent B. CheckFencingTx then verifies (peer, token)
	// against agent_locks for the row's true owner.
	if patch.Fencing != nil {
		if patch.Fencing.AgentID != cur.AgentID {
			return nil, fmt.Errorf("store.UpdateMemoryEntry: %w: fencing agent_id %q does not match entry agent_id %q",
				ErrFencingMismatch, patch.Fencing.AgentID, cur.AgentID)
		}
		if err := checkFencingPredicate(ctx, s, tx, patch.Fencing, cur.AgentID); err != nil {
			return nil, fmt.Errorf("store.UpdateMemoryEntry: %w", err)
		}
	}
	if ifMatchETag != "" && cur.ETag != ifMatchETag {
		return nil, ErrETagMismatch
	}
	if patch.Kind == nil && patch.Name == nil && patch.Body == nil {
		return cur, nil
	}

	next := *cur
	if patch.Kind != nil {
		if !validMemoryKinds[*patch.Kind] {
			return nil, fmt.Errorf("store.UpdateMemoryEntry: invalid kind %q", *patch.Kind)
		}
		next.Kind = *patch.Kind
	}
	if patch.Name != nil {
		if *patch.Name == "" {
			// Insert rejects empty names; let update mirror that.
			return nil, errors.New("store.UpdateMemoryEntry: name must not be empty")
		}
		next.Name = *patch.Name
	}
	if patch.Body != nil {
		next.Body = *patch.Body
		next.BodySHA256 = SHA256Hex([]byte(next.Body))
	}
	next.Version = cur.Version + 1
	next.UpdatedAt = NowMillis()
	next.ETag, err = computeMemoryEntryETag(&next)
	if err != nil {
		return nil, err
	}

	const upd = `
UPDATE memory_entries
   SET kind = ?, name = ?, body = ?, body_sha256 = ?,
       version = ?, etag = ?, updated_at = ?
 WHERE id = ? AND etag = ? AND deleted_at IS NULL`
	res, err := tx.ExecContext(ctx, upd,
		next.Kind, next.Name, next.Body, next.BodySHA256,
		next.Version, next.ETag, next.UpdatedAt,
		id, cur.ETag,
	)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrETagMismatch
	}
	if patch.Idempotency != nil {
		if err := recordOplogAppliedTx(ctx, tx, &OplogAppliedRecord{
			OpID:        patch.Idempotency.OpID,
			AgentID:     next.AgentID,
			Fingerprint: patch.Idempotency.Fingerprint,
			ResultETag:  next.ETag,
			AppliedAt:   next.UpdatedAt,
		}); err != nil {
			return nil, fmt.Errorf("store.UpdateMemoryEntry: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &next, nil
}

// SoftDeleteMemoryEntry tombstones an entry.
//
// Empty ifMatchETag → unconditional, idempotent: a missing or already-
// tombstoned row returns nil so daemon-side reconciliation (sync) can
// blindly call this without racing with a parallel CLI delete.
//
// Non-empty ifMatchETag → conditional: missing row, tombstoned row, or
// etag mismatch all surface as ErrETagMismatch. The HTTP handler maps
// that to 412 so a Web client asserting a specific live etag never
// silently succeeds against a row that drifted out from under it.
func (s *Store) SoftDeleteMemoryEntry(ctx context.Context, id, ifMatchETag string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Live row + alive parent agent. The agent join is here (rather
	// than a separate check) so a tombstoned-parent / dangling-FK case
	// surfaces as "not found" identically to a tombstoned row.
	const sel = `
SELECT m.id, m.agent_id, m.seq, m.kind, m.name, m.body, m.body_sha256,
       m.version, m.etag, m.created_at, m.updated_at, m.deleted_at, COALESCE(m.peer_id,'')
  FROM memory_entries m
  JOIN agents         a ON a.id = m.agent_id
 WHERE m.id = ? AND m.deleted_at IS NULL AND a.deleted_at IS NULL`
	cur, err := scanMemoryEntryRow(tx.QueryRowContext(ctx, sel, id))
	if errors.Is(err, sql.ErrNoRows) {
		// No live row. For unconditional callers this is a no-op
		// (the row is already in the desired state); for conditional
		// callers the asserted etag can't be reconciled against a
		// vanished row, so surface as etag mismatch and force a
		// refetch.
		if ifMatchETag != "" {
			return ErrETagMismatch
		}
		return nil
	}
	if err != nil {
		return err
	}
	if ifMatchETag != "" && cur.ETag != ifMatchETag {
		return ErrETagMismatch
	}

	now := NowMillis()
	cur.Version++
	cur.UpdatedAt = now
	cur.DeletedAt = &now
	newETag, err := computeMemoryEntryETag(cur)
	if err != nil {
		return err
	}

	// Include the asserted etag (the one we just read & verified) in
	// the UPDATE predicate for the conditional path, so a concurrent
	// mutator that landed between our SELECT and this UPDATE surfaces
	// as 0 rows affected — which we then translate to ErrETagMismatch.
	// Without this guard, a Web client racing a CLI write could
	// silently overwrite the CLI's change.
	//
	// The unconditional path keeps the looser "any live row" predicate
	// so daemon-side reconciliation (sync's tombstone phase) stays
	// idempotent — it doesn't need racey concurrent edits to abort
	// the tombstone, just to land it.
	if ifMatchETag != "" {
		const updCond = `
UPDATE memory_entries
   SET deleted_at = ?, updated_at = ?, version = ?, etag = ?
 WHERE id = ? AND etag = ? AND deleted_at IS NULL`
		res, err := tx.ExecContext(ctx, updCond, now, now, cur.Version, newETag, id, cur.ETag)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrETagMismatch
		}
	} else {
		const updUncond = `
UPDATE memory_entries
   SET deleted_at = ?, updated_at = ?, version = ?, etag = ?
 WHERE id = ? AND deleted_at IS NULL`
		if _, err := tx.ExecContext(ctx, updUncond, now, now, cur.Version, newETag, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func scanMemoryEntryRow(r rowScanner) (*MemoryEntryRecord, error) {
	var (
		rec       MemoryEntryRecord
		deletedAt sql.NullInt64
	)
	if err := r.Scan(
		&rec.ID, &rec.AgentID, &rec.Seq, &rec.Kind, &rec.Name, &rec.Body, &rec.BodySHA256,
		&rec.Version, &rec.ETag, &rec.CreatedAt, &rec.UpdatedAt, &deletedAt, &rec.PeerID,
	); err != nil {
		return nil, err
	}
	if deletedAt.Valid {
		v := deletedAt.Int64
		rec.DeletedAt = &v
	}
	return &rec, nil
}

// -------- agent_memory (singleton MEMORY.md per agent) ----------------

// AgentMemoryRecord mirrors the `agent_memory` table. The body is the
// denormalized copy of the global blob `<v1>/global/agents/<id>/MEMORY.md`;
// the daemon syncs the file into this row using the intent-file protocol
// (§2.5). LastTxID identifies the originating write transaction; NULL means
// the row was loaded from a CLI-direct write before the daemon caught up.
type AgentMemoryRecord struct {
	AgentID    string
	Body       string
	BodySHA256 string
	LastTxID   *string

	Seq       int64
	Version   int
	ETag      string
	CreatedAt int64
	UpdatedAt int64
	DeletedAt *int64
	PeerID    string
}

type agentMemoryETagInput struct {
	AgentID    string  `json:"agent_id"`
	BodySHA256 string  `json:"body_sha256"`
	LastTxID   *string `json:"last_tx_id"`
	UpdatedAt  int64   `json:"updated_at"`
	DeletedAt  *int64  `json:"deleted_at"`
}

func computeAgentMemoryETag(r *AgentMemoryRecord) (string, error) {
	return CanonicalETag(r.Version, agentMemoryETagInput{
		AgentID:    r.AgentID,
		BodySHA256: r.BodySHA256,
		LastTxID:   r.LastTxID,
		UpdatedAt:  r.UpdatedAt,
		DeletedAt:  r.DeletedAt,
	})
}

// AgentMemoryInsertOptions follows the same shape as AgentInsertOptions.
// Fencing makes the upsert atomic with an agent_locks holder check;
// Idempotency makes a replayed op_id short-circuit to the prior etag.
type AgentMemoryInsertOptions struct {
	Now            int64
	Seq            int64
	CreatedAt      int64
	UpdatedAt      int64
	PeerID         string
	LastTxID       *string
	AllowOverwrite bool
	Fencing        *FencingPredicate
	Idempotency    *IdempotencyTag
}

// UpsertAgentMemory writes (or replaces) the singleton MEMORY.md row for
// agentID. Mirrors the persona upsert semantics — see UpsertAgentPersona for
// the rationale on AllowOverwrite, ifMatchETag, and the live-vs-tombstone
// branching.
func (s *Store) UpsertAgentMemory(ctx context.Context, agentID, body, ifMatchETag string, opts AgentMemoryInsertOptions) (*AgentMemoryRecord, error) {
	if agentID == "" {
		return nil, errors.New("store.UpsertAgentMemory: agent_id required")
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
		return nil, fmt.Errorf("store.UpsertAgentMemory: %w", err)
	} else if prior != nil {
		existing, gerr := s.getAgentMemoryTx(ctx, tx, agentID)
		if gerr != nil {
			return nil, fmt.Errorf("store.UpsertAgentMemory: idempotent re-read: %w", gerr)
		}
		return existing, nil
	}
	// Fencing gate runs SECOND.
	if err := checkFencingPredicate(ctx, s, tx, opts.Fencing, agentID); err != nil {
		return nil, fmt.Errorf("store.UpsertAgentMemory: %w", err)
	}

	var alive int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM agents WHERE id = ? AND deleted_at IS NULL`, agentID,
	).Scan(&alive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("store.UpsertAgentMemory: agent %q: %w", agentID, ErrNotFound)
		}
		return nil, err
	}

	const sel = `
SELECT body, body_sha256, last_tx_id, seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
  FROM agent_memory WHERE agent_id = ?`
	var (
		prev       AgentMemoryRecord
		lastTxID   sql.NullString
		deletedAt  sql.NullInt64
		liveExists bool
		anyRow     bool
	)
	prev.AgentID = agentID
	err = tx.QueryRowContext(ctx, sel, agentID).Scan(
		&prev.Body, &prev.BodySHA256, &lastTxID, &prev.Seq, &prev.Version,
		&prev.ETag, &prev.CreatedAt, &prev.UpdatedAt, &deletedAt, &prev.PeerID,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		anyRow = true
		if lastTxID.Valid {
			v := lastTxID.String
			prev.LastTxID = &v
		}
		if deletedAt.Valid {
			v := deletedAt.Int64
			prev.DeletedAt = &v
		} else {
			liveExists = true
		}
	}

	switch {
	case ifMatchETag != "":
		if !liveExists || prev.ETag != ifMatchETag {
			return nil, ErrETagMismatch
		}
	case liveExists && !opts.AllowOverwrite:
		return nil, ErrETagMismatch
	}

	// Normalize LastTxID: a non-nil pointer to "" would hash as a present
	// JSON string but persist as NULL, leaving the etag unreconstructible
	// from the row. Treat empty as nil end-to-end.
	var lastTx *string
	if opts.LastTxID != nil && *opts.LastTxID != "" {
		v := *opts.LastTxID
		lastTx = &v
	}
	rec := AgentMemoryRecord{
		AgentID:    agentID,
		Body:       body,
		BodySHA256: SHA256Hex([]byte(body)),
		LastTxID:   lastTx,
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
	rec.ETag, err = computeAgentMemoryETag(&rec)
	if err != nil {
		return nil, err
	}

	if liveExists {
		const upd = `
UPDATE agent_memory SET
  body = ?, body_sha256 = ?, last_tx_id = ?,
  version = ?, etag = ?, updated_at = ?, peer_id = ?
WHERE agent_id = ? AND deleted_at IS NULL`
		if _, err := tx.ExecContext(ctx, upd,
			rec.Body, rec.BodySHA256, nullableTxID(rec.LastTxID),
			rec.Version, rec.ETag, rec.UpdatedAt, nullableText(rec.PeerID), agentID,
		); err != nil {
			return nil, err
		}
	} else {
		if anyRow {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM agent_memory WHERE agent_id = ?`, agentID,
			); err != nil {
				return nil, err
			}
		}
		const ins = `
INSERT INTO agent_memory (
  agent_id, body, body_sha256, last_tx_id,
  seq, version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`
		if _, err := tx.ExecContext(ctx, ins,
			rec.AgentID, rec.Body, rec.BodySHA256, nullableTxID(rec.LastTxID),
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
			return nil, fmt.Errorf("store.UpsertAgentMemory: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &rec, nil
}

// GetAgentMemory returns the MEMORY.md row for agentID, or ErrNotFound.
// Filters out soft-deleted parents the same way persona/messages reads do.
func (s *Store) GetAgentMemory(ctx context.Context, agentID string) (*AgentMemoryRecord, error) {
	const q = `
SELECT m.body, m.body_sha256, m.last_tx_id, m.seq, m.version, m.etag,
       m.created_at, m.updated_at, m.deleted_at, COALESCE(m.peer_id,'')
  FROM agent_memory m
  JOIN agents      a ON a.id = m.agent_id
 WHERE m.agent_id = ? AND m.deleted_at IS NULL AND a.deleted_at IS NULL`
	var (
		rec       AgentMemoryRecord
		lastTxID  sql.NullString
		deletedAt sql.NullInt64
	)
	rec.AgentID = agentID
	err := s.db.QueryRowContext(ctx, q, agentID).Scan(
		&rec.Body, &rec.BodySHA256, &lastTxID, &rec.Seq, &rec.Version,
		&rec.ETag, &rec.CreatedAt, &rec.UpdatedAt, &deletedAt, &rec.PeerID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if lastTxID.Valid {
		v := lastTxID.String
		rec.LastTxID = &v
	}
	if deletedAt.Valid {
		v := deletedAt.Int64
		rec.DeletedAt = &v
	}
	return &rec, nil
}

// getAgentMemoryTx is the tx-scoped variant used by the
// idempotency short-circuit path in UpsertAgentMemory.
func (s *Store) getAgentMemoryTx(ctx context.Context, tx *sql.Tx, agentID string) (*AgentMemoryRecord, error) {
	const q = `
SELECT m.body, m.body_sha256, m.last_tx_id, m.seq, m.version, m.etag,
       m.created_at, m.updated_at, m.deleted_at, COALESCE(m.peer_id,'')
  FROM agent_memory m
 WHERE m.agent_id = ? AND m.deleted_at IS NULL`
	var (
		rec       AgentMemoryRecord
		lastTxID  sql.NullString
		deletedAt sql.NullInt64
	)
	rec.AgentID = agentID
	err := tx.QueryRowContext(ctx, q, agentID).Scan(
		&rec.Body, &rec.BodySHA256, &lastTxID, &rec.Seq, &rec.Version,
		&rec.ETag, &rec.CreatedAt, &rec.UpdatedAt, &deletedAt, &rec.PeerID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if lastTxID.Valid {
		v := lastTxID.String
		rec.LastTxID = &v
	}
	if deletedAt.Valid {
		v := deletedAt.Int64
		rec.DeletedAt = &v
	}
	return &rec, nil
}

func nullableTxID(p *string) any {
	if p == nil || *p == "" {
		return nil
	}
	return *p
}

// SoftDeleteAgentMemory tombstones the agent_memory row for agentID.
// Idempotent (already-tombstoned, missing-row, or missing-agent all
// return nil) so the daemon-side file→DB sync can call it on every
// "file gone, row exists" detection without conditionals on the
// caller side.
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
func (s *Store) SoftDeleteAgentMemory(ctx context.Context, agentID, ifMatchETag string) error {
	if agentID == "" {
		return errors.New("store.SoftDeleteAgentMemory: agent_id required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	const sel = `
SELECT body, body_sha256, last_tx_id, seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
  FROM agent_memory WHERE agent_id = ?`
	var (
		rec       AgentMemoryRecord
		lastTxID  sql.NullString
		deletedAt sql.NullInt64
	)
	rec.AgentID = agentID
	err = tx.QueryRowContext(ctx, sel, agentID).Scan(
		&rec.Body, &rec.BodySHA256, &lastTxID, &rec.Seq, &rec.Version,
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
	if lastTxID.Valid {
		v := lastTxID.String
		rec.LastTxID = &v
	}
	newETag, err := computeAgentMemoryETag(&rec)
	if err != nil {
		return err
	}

	const upd = `
UPDATE agent_memory SET
  deleted_at = ?, updated_at = ?, version = ?, etag = ?
WHERE agent_id = ? AND deleted_at IS NULL`
	if _, err := tx.ExecContext(ctx, upd, now, now, rec.Version, newETag, agentID); err != nil {
		return err
	}
	return tx.Commit()
}

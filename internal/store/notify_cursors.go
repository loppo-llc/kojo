package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// NotifyCursorRecord mirrors the `notify_cursors` table.
//
// id is a composite source identifier — schema example "agent:slack:Cxxx".
// The v0→v1 importer composes it as "<agent_id>:<source>:<source_id>";
// the runtime must use the same shape so a re-imported row matches an in-
// flight live row by primary key.
//
// agent_id is nullable: a cursor scoped to the deployment as a whole
// (e.g. a future system-level integration) wouldn't be tied to a specific
// agent. Today every v0 cursor has an agent in its key, so all imported
// rows have agent_id populated.
//
// notify_cursors has no `seq` column (unlike agents/agent_messages/etc.),
// so this record carries only the version/etag/timestamps common columns.
// Cursor freshness ordering is implicit in updated_at — the WS invalidator
// uses the global event log, not a notify_cursors-local seq.
type NotifyCursorRecord struct {
	ID      string
	Source  string  // 'slack' | 'gmail' | 'discord' | ...
	AgentID *string // nullable; runtime cursors always carry an agent today
	Cursor  string  // opaque to kojo; set by the notify source plugin

	Version   int
	ETag      string
	CreatedAt int64
	UpdatedAt int64
	DeletedAt *int64
	PeerID    string
}

type notifyCursorETagInput struct {
	ID        string  `json:"id"`
	Source    string  `json:"source"`
	AgentID   *string `json:"agent_id"`
	Cursor    string  `json:"cursor"`
	UpdatedAt int64   `json:"updated_at"`
	DeletedAt *int64  `json:"deleted_at"`
}

func computeNotifyCursorETag(r *NotifyCursorRecord) (string, error) {
	return CanonicalETag(r.Version, notifyCursorETagInput{
		ID:        r.ID,
		Source:    r.Source,
		AgentID:   r.AgentID,
		Cursor:    r.Cursor,
		UpdatedAt: r.UpdatedAt,
		DeletedAt: r.DeletedAt,
	})
}

// NotifyCursorInsertOptions lets the v0→v1 importer preserve original
// timestamps and override the clock for tests.
type NotifyCursorInsertOptions struct {
	Now       int64
	CreatedAt int64
	UpdatedAt int64
	PeerID    string
}

// BulkInsertNotifyCursors inserts many notify_cursor rows in a single
// transaction. Used by the v0→v1 importer; live cutover (poller writing
// each cursor as it advances) goes through a single-row Upsert API which
// is intentionally not yet exposed — the poller still writes the JSON
// file in the v1 branch as of slice 8a.
//
// Idempotency contract matches BulkInsertSessions: rows whose id already
// exists are skipped via ON CONFLICT DO NOTHING + a preload-set so a
// re-run leaves the existing row untouched. Caller records are mutated
// in place AFTER commit with assigned etag/timestamps; skipped rows are
// left untouched.
//
// AgentID=&"" is normalized to nil on the staged copy so the canonical
// ETag (computed from the staged record) agrees with the round-tripped
// record read back from SQL (NULL → nil pointer).
func (s *Store) BulkInsertNotifyCursors(ctx context.Context, recs []*NotifyCursorRecord, opts NotifyCursorInsertOptions) (int, error) {
	if len(recs) == 0 {
		return 0, nil
	}

	now := opts.Now
	if now == 0 {
		now = NowMillis()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// Up-front validation. Surface here so a pathological record at index
	// N doesn't leave N-1 partial inserts to roll back.
	for i, r := range recs {
		if r == nil {
			return 0, fmt.Errorf("store.BulkInsertNotifyCursors: nil record at index %d", i)
		}
		if r.ID == "" {
			return 0, fmt.Errorf("store.BulkInsertNotifyCursors: index %d: id required", i)
		}
		if r.Source == "" {
			return 0, fmt.Errorf("store.BulkInsertNotifyCursors: index %d (id=%s): source required", i, r.ID)
		}
		if r.Cursor == "" {
			return 0, fmt.Errorf("store.BulkInsertNotifyCursors: index %d (id=%s): cursor required", i, r.ID)
		}
	}

	existing, err := preloadExistingNotifyCursorIDs(ctx, tx, recs)
	if err != nil {
		return 0, fmt.Errorf("store.BulkInsertNotifyCursors: preload existing: %w", err)
	}

	const q = `
INSERT INTO notify_cursors (
  id, source, agent_id, cursor,
  version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
ON CONFLICT(id) DO NOTHING`
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	type stagedCursor struct {
		idx int
		rec NotifyCursorRecord
	}
	staged := make([]stagedCursor, 0, len(recs))

	inserted := 0
	for i, r := range recs {
		if existing[r.ID] {
			continue
		}

		created := r.CreatedAt
		if created == 0 {
			created = opts.CreatedAt
		}
		if created == 0 {
			created = now
		}
		updated := r.UpdatedAt
		if updated == 0 {
			updated = opts.UpdatedAt
		}
		if updated == 0 {
			updated = created
		}

		out := *r
		// Normalize AgentID=&"" → nil on the staged copy so the canonical
		// ETag agrees with the value read back from SQL. The caller's
		// recs[i] is mutated in-place AFTER commit (see post-commit copy
		// loop below), so this also surfaces back to the caller as a
		// nil AgentID — distinguishing "skipped" (untouched) from
		// "inserted" (rewritten with normalized values).
		if out.AgentID != nil && *out.AgentID == "" {
			out.AgentID = nil
		}
		out.Version = 1
		out.CreatedAt = created
		out.UpdatedAt = updated
		out.PeerID = opts.PeerID
		out.DeletedAt = nil
		etag, err := computeNotifyCursorETag(&out)
		if err != nil {
			return 0, fmt.Errorf("store.BulkInsertNotifyCursors: index %d (id=%s): etag: %w", i, r.ID, err)
		}
		out.ETag = etag

		res, err := stmt.ExecContext(ctx,
			out.ID, out.Source, nullableTextPtr(out.AgentID), out.Cursor,
			out.Version, out.ETag, out.CreatedAt, out.UpdatedAt, nullableText(out.PeerID),
		)
		if err != nil {
			return 0, fmt.Errorf("store.BulkInsertNotifyCursors: index %d (id=%s): %w", i, r.ID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		if n > 0 {
			inserted++
			staged = append(staged, stagedCursor{idx: i, rec: out})
		}
		// n == 0 here means an in-batch duplicate id: first wins via
		// ON CONFLICT DO NOTHING. Concurrent writers can't interleave —
		// SQLite holds the write lock from our first INSERT through commit.
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	for _, s := range staged {
		*recs[s.idx] = s.rec
	}
	return inserted, nil
}

// preloadExistingNotifyCursorIDs returns the set of cursor ids already in
// notify_cursors for the given batch. Chunked to stay under SQLite's
// default 999-variable limit.
func preloadExistingNotifyCursorIDs(ctx context.Context, tx *sql.Tx, recs []*NotifyCursorRecord) (map[string]bool, error) {
	if len(recs) == 0 {
		return nil, nil
	}
	const chunkSize = 500
	out := make(map[string]bool, len(recs))
	for start := 0; start < len(recs); start += chunkSize {
		end := start + chunkSize
		if end > len(recs) {
			end = len(recs)
		}
		ids := make([]any, 0, end-start)
		placeholders := make([]byte, 0, (end-start)*2)
		for i := start; i < end; i++ {
			if i > start {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
			ids = append(ids, recs[i].ID)
		}
		q := `SELECT id FROM notify_cursors WHERE id IN (` + string(placeholders) + `)`
		rows, err := tx.QueryContext(ctx, q, ids...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// GetNotifyCursor returns the live row by id. ErrNotFound on miss or
// tombstone — callers wanting to inspect a tombstone need a dedicated
// helper, which doesn't exist yet because no caller does. The
// `deleted_at IS NULL` guard is defense-in-depth: today no v1 path
// writes a tombstone (DeleteNotifyCursor is a hard delete, Upsert
// reincarnates over tombstones into a fresh live row), but a future
// peer-replication slice may replay tombstones from the op_log and we
// don't want the poller to mistake a dead row for a live cursor.
func (s *Store) GetNotifyCursor(ctx context.Context, id string) (*NotifyCursorRecord, error) {
	const q = `
SELECT id, source, agent_id, cursor,
       version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
  FROM notify_cursors
 WHERE id = ? AND deleted_at IS NULL`
	rec, err := scanNotifyCursorRow(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// ListNotifyCursorsByAgent returns LIVE cursors owned by agentID
// ordered by id. agentID="" returns rows with NULL agent_id
// (deployment-scoped cursors; not used in v0 today but the schema
// allows them). Tombstoned rows are excluded — see GetNotifyCursor for
// the reasoning.
func (s *Store) ListNotifyCursorsByAgent(ctx context.Context, agentID string) ([]*NotifyCursorRecord, error) {
	q := `
SELECT id, source, agent_id, cursor,
       version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
  FROM notify_cursors
 WHERE %s AND deleted_at IS NULL
 ORDER BY id ASC`
	var (
		rows *sql.Rows
		err  error
	)
	if agentID == "" {
		rows, err = s.db.QueryContext(ctx, fmt.Sprintf(q, "agent_id IS NULL"))
	} else {
		rows, err = s.db.QueryContext(ctx, fmt.Sprintf(q, "agent_id = ?"), agentID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*NotifyCursorRecord
	for rows.Next() {
		rec, err := scanNotifyCursorRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// UpsertNotifyCursor inserts a new cursor row or updates the existing one
// in place. Used by the live notify_poller as it advances cursors during
// runtime (slice 8a's BulkInsertNotifyCursors handles the v0→v1 import
// path; this one handles the post-cutover write path).
//
// On INSERT (no row, or only a soft-tombstoned row): version=1,
// created_at=now (or opts.CreatedAt). A pre-existing tombstoned row is
// hard-deleted before insert so the new live row starts at version 1
// rather than carrying forward the dead row's lineage — cursors are pure
// progress markers, the tombstone has no audit value worth keeping. The
// importer's ON CONFLICT DO NOTHING posture is unaffected: import runs
// before live writes, and a row imported in slice 8a's bulk path appears
// here as a live row (deleted_at IS NULL) so we take the UPDATE branch.
//
// On UPDATE: version = prev.version + 1, created_at preserved,
// updated_at = now (or opts.UpdatedAt).
//
// peer_id is stamped from opts.PeerID on every write (overwriting the
// prior value) — multi-peer fencing decides who's allowed to advance the
// cursor at a higher layer; this method just records who actually did.
//
// rec is mutated in place with the assigned version, etag, and
// timestamps so the caller can use the returned record for in-memory
// cache update without re-reading.
//
// The whole upsert runs in a single tx so a concurrent reader either
// sees the prior row or the new one — never an intermediate state with
// version bumped but etag stale.
func (s *Store) UpsertNotifyCursor(ctx context.Context, rec *NotifyCursorRecord, opts NotifyCursorInsertOptions) error {
	if rec == nil {
		return fmt.Errorf("store.UpsertNotifyCursor: nil record")
	}
	if rec.ID == "" {
		return fmt.Errorf("store.UpsertNotifyCursor: id required")
	}
	if rec.Source == "" {
		return fmt.Errorf("store.UpsertNotifyCursor: id=%s: source required", rec.ID)
	}
	if rec.Cursor == "" {
		return fmt.Errorf("store.UpsertNotifyCursor: id=%s: cursor required", rec.ID)
	}

	// Normalize AgentID=&"" → nil up front so the canonical ETag computed
	// here agrees with the round-tripped record (NULL → nil pointer).
	// Mirrors BulkInsertNotifyCursors's normalization.
	if rec.AgentID != nil && *rec.AgentID == "" {
		rec.AgentID = nil
	}

	now := opts.Now
	if now == 0 {
		now = NowMillis()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	const sel = `
SELECT version, created_at, deleted_at
  FROM notify_cursors
 WHERE id = ?`
	var (
		prevVersion   int
		prevCreatedAt int64
		prevDeletedAt sql.NullInt64
		anyRow        bool
		liveExists    bool
	)
	err = tx.QueryRowContext(ctx, sel, rec.ID).Scan(&prevVersion, &prevCreatedAt, &prevDeletedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	default:
		anyRow = true
		if !prevDeletedAt.Valid {
			liveExists = true
		}
	}

	// Compute final field values BEFORE etag.
	if liveExists {
		rec.Version = prevVersion + 1
		rec.CreatedAt = prevCreatedAt
	} else {
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
	rec.PeerID = opts.PeerID
	// A successful upsert always lands on a live (non-tombstoned) row.
	// Clear DeletedAt on the staged record so callers reading rec back
	// don't see a stale tombstone, and so the etag input matches the
	// row that's about to be written (deleted_at = NULL).
	rec.DeletedAt = nil

	etag, err := computeNotifyCursorETag(rec)
	if err != nil {
		return fmt.Errorf("store.UpsertNotifyCursor: id=%s: etag: %w", rec.ID, err)
	}
	rec.ETag = etag

	var op EventOp
	if liveExists {
		op = EventOpUpdate
		const upd = `
UPDATE notify_cursors SET
  source = ?, agent_id = ?, cursor = ?,
  version = ?, etag = ?, updated_at = ?, peer_id = ?
WHERE id = ? AND deleted_at IS NULL`
		if _, err := tx.ExecContext(ctx, upd,
			rec.Source, nullableTextPtr(rec.AgentID), rec.Cursor,
			rec.Version, rec.ETag, rec.UpdatedAt, nullableText(rec.PeerID),
			rec.ID,
		); err != nil {
			return fmt.Errorf("store.UpsertNotifyCursor: id=%s: update: %w", rec.ID, err)
		}
	} else {
		op = EventOpInsert
		if anyRow {
			// Tombstoned row in the way. Drop it so the fresh insert
			// can start at version=1 — cursors are progress markers,
			// the dead row's lineage has no value to preserve.
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM notify_cursors WHERE id = ?`, rec.ID,
			); err != nil {
				return fmt.Errorf("store.UpsertNotifyCursor: id=%s: drop tombstone: %w", rec.ID, err)
			}
		}
		const ins = `
INSERT INTO notify_cursors (
  id, source, agent_id, cursor,
  version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`
		if _, err := tx.ExecContext(ctx, ins,
			rec.ID, rec.Source, nullableTextPtr(rec.AgentID), rec.Cursor,
			rec.Version, rec.ETag, rec.CreatedAt, rec.UpdatedAt, nullableText(rec.PeerID),
		); err != nil {
			return fmt.Errorf("store.UpsertNotifyCursor: id=%s: insert: %w", rec.ID, err)
		}
	}

	// Inside-tx event recording so /api/v1/changes never returns an
	// event for a row that didn't commit (and vice versa). Mirrors the
	// pattern in InsertAgent / UpdateAgent / kv upsert.
	evSeq, err := RecordEvent(ctx, tx, "notify_cursors", rec.ID, rec.ETag, op, rec.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store.UpsertNotifyCursor: id=%s: record event: %w", rec.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.fireEvent(EventRecord{
		Seq: evSeq, Table: "notify_cursors", ID: rec.ID, ETag: rec.ETag,
		Op: op, TS: rec.UpdatedAt,
	})
	return nil
}

// DeleteNotifyCursorsByAgent hard-deletes every notify_cursor row owned
// by agentID. Used when the agent itself is removed (Manager.Delete),
// at which point the in-memory poller state for the agent is also
// purged. Cursors are pure progress markers — a tombstone has no audit
// value worth keeping around once the owning agent is gone.
//
// agentID="" is rejected to prevent an accidental wipe of every cursor
// in the table; deployment-scoped rows (agent_id IS NULL) are only
// removed via the explicit single-id DeleteNotifyCursor.
//
// One delete event per row is recorded inside the same tx so a peer
// reading /api/v1/changes sees the same row-by-row removal it would
// have seen from individual DeleteNotifyCursor calls. Listing live
// rows up front (rather than relying on RowsAffected after the bulk
// DELETE) is what makes per-row event emission possible.
//
// Returns the number of LIVE rows whose delete event was emitted.
// Tombstoned rows are physically wiped by the bulk DELETE but are
// excluded from this count (their original tombstone-write event
// already announced their removal — see the parallel guard in
// DeleteNotifyCursor). A 0 return for an agent that had only
// tombstones, or no rows at all, is the normal idempotent case.
func (s *Store) DeleteNotifyCursorsByAgent(ctx context.Context, agentID string) (int64, error) {
	if agentID == "" {
		return 0, fmt.Errorf("store.DeleteNotifyCursorsByAgent: agent id required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// Tombstoned rows are excluded from the event-emission list — their
	// delete event was already fired when the tombstone was written, so
	// firing again here would mislead a peer into double-applying. The
	// physical DELETE below still wipes them (both live and tombstoned),
	// matching the "no audit value once the agent is gone" contract.
	// ORDER BY id ASC keeps the per-row event sequence deterministic so
	// peers / tests can rely on the emission order.
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM notify_cursors WHERE agent_id = ? AND deleted_at IS NULL ORDER BY id ASC`, agentID)
	if err != nil {
		return 0, fmt.Errorf("store.DeleteNotifyCursorsByAgent: list: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store.DeleteNotifyCursorsByAgent: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	// Always issue the bulk DELETE — even if ids is empty (no live rows),
	// there may still be tombstoned rows for this agent that need
	// physical removal. The tombstones don't produce a delete event
	// (their original tombstone-write event already covered that), but
	// leaving them on disk would defeat the "wipe per-agent state"
	// contract and let a future re-creation of the same agentID pick
	// up stale lineage in UpsertNotifyCursor's "drop tombstone" branch.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM notify_cursors WHERE agent_id = ?`, agentID,
	); err != nil {
		return 0, fmt.Errorf("store.DeleteNotifyCursorsByAgent: delete: %w", err)
	}

	if len(ids) == 0 {
		// No live rows to emit events for; tombstones (if any) were just
		// physically removed without re-firing.
		return 0, tx.Commit()
	}

	now := NowMillis()
	type pendingFire struct {
		id  string
		seq int64
	}
	fires := make([]pendingFire, 0, len(ids))
	for _, id := range ids {
		seq, err := RecordEvent(ctx, tx, "notify_cursors", id, "", EventOpDelete, now)
		if err != nil {
			return 0, fmt.Errorf("store.DeleteNotifyCursorsByAgent: record event id=%s: %w", id, err)
		}
		fires = append(fires, pendingFire{id: id, seq: seq})
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	for _, f := range fires {
		s.fireEvent(EventRecord{
			Seq: f.seq, Table: "notify_cursors", ID: f.id,
			Op: EventOpDelete, TS: now,
		})
	}
	return int64(len(ids)), nil
}

// DeleteNotifyCursor hard-deletes a cursor row by id. Used by the
// notify_poller's purge path when an agent's notify source is removed
// from config (or the agent itself is deleted) — the cursor is no longer
// meaningful and a tombstone has no audit value (cursors are opaque
// progress markers, not user-visible state).
//
// Idempotent: returns nil if no row matches, mirroring the v0 JSON-file
// purge semantics (which simply dropped keys from the in-memory map).
//
// Event semantics (matches DeleteNotifyCursorsByAgent):
//   - Live row deleted → RecordEvent + fireEvent EventOpDelete.
//   - Tombstoned row physically removed → no event (the original
//     tombstone-write event already covered that; firing again would
//     mislead a peer's /api/v1/changes consumer into double-applying).
//   - No matching row → no event (delete-of-nothing would suggest a
//     removal that never happened).
//
// The two DELETE statements separate "live wipe + event" from
// "silent tombstone cleanup" cleanly. SQLite executes them in tx order
// inside a single commit, so a peer reading the row right after the
// commit sees the row gone regardless of which branch ran.
func (s *Store) DeleteNotifyCursor(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("store.DeleteNotifyCursor: id required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Live-row delete: this branch counts toward the event stream.
	resLive, err := tx.ExecContext(ctx,
		`DELETE FROM notify_cursors WHERE id = ? AND deleted_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("store.DeleteNotifyCursor: id=%s: delete (live): %w", id, err)
	}
	nLive, err := resLive.RowsAffected()
	if err != nil {
		return err
	}

	// Tombstone cleanup: silently remove any tombstoned row at the same
	// id. A live row at this id can't co-exist with a tombstone (id is
	// PRIMARY KEY) so this never overlaps with the live-delete above.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM notify_cursors WHERE id = ? AND deleted_at IS NOT NULL`, id,
	); err != nil {
		return fmt.Errorf("store.DeleteNotifyCursor: id=%s: delete (tombstone): %w", id, err)
	}

	if nLive == 0 {
		// Either no row matched (idempotent miss) or only a tombstone
		// was wiped — neither emits an event.
		return tx.Commit()
	}

	now := NowMillis()
	evSeq, err := RecordEvent(ctx, tx, "notify_cursors", id, "", EventOpDelete, now)
	if err != nil {
		return fmt.Errorf("store.DeleteNotifyCursor: id=%s: record event: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.fireEvent(EventRecord{
		Seq: evSeq, Table: "notify_cursors", ID: id,
		Op: EventOpDelete, TS: now,
	})
	return nil
}

func scanNotifyCursorRow(r rowScanner) (*NotifyCursorRecord, error) {
	var (
		rec       NotifyCursorRecord
		agentID   sql.NullString
		deletedAt sql.NullInt64
	)
	if err := r.Scan(
		&rec.ID, &rec.Source, &agentID, &rec.Cursor,
		&rec.Version, &rec.ETag, &rec.CreatedAt, &rec.UpdatedAt, &deletedAt, &rec.PeerID,
	); err != nil {
		return nil, err
	}
	if agentID.Valid {
		v := agentID.String
		rec.AgentID = &v
	}
	if deletedAt.Valid {
		v := deletedAt.Int64
		rec.DeletedAt = &v
	}
	return &rec, nil
}

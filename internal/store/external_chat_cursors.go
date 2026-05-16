package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ExternalChatCursorRecord mirrors the `external_chat_cursors` table.
//
// id is a composite identifier — by convention the v0→v1 importer composes
// it as "<agent_id>:<source>:<channel_id>:<thread_id>" for per-thread
// cursors (the only shape v0 produces today; channel-level rollups are
// not cursor-driven and are not imported). The schema carries
// channel_id as a regular column (no dedicated index — agent_id is the
// only secondary index, see migration 0001); thread_id lives only
// inside the composite id. Runtime callers that resume polling reuse
// the same composition so a re-imported row matches an in-flight live
// row by primary key.
//
// agent_id is nullable to accommodate a future deployment-scoped integration
// (e.g. a system-wide bot account) that wouldn't be tied to one agent. Every
// v0 cursor today is per-agent, so all imported rows have agent_id populated.
//
// channel_id is nullable for the same reason — a future cursor for a non-
// channel-shaped source (DM list, PM stream) may not have a channel id.
// Every v0 cursor today corresponds to a Slack channel and populates this
// column.
//
// cursor is opaque to kojo — the polling plugin sets it (Slack: the latest
// numeric ts; Discord: a snowflake; etc.) and the runtime hands it back
// unchanged on the next poll.
//
// external_chat_cursors has no `seq` column, mirroring notify_cursors:
// cursor freshness ordering is implicit in updated_at and the WS invalidator
// uses the global event log.
type ExternalChatCursorRecord struct {
	ID        string
	Source    string  // 'slack' | 'discord' | ...
	AgentID   *string // nullable
	ChannelID *string // nullable
	Cursor    string  // opaque to kojo

	Version   int
	ETag      string
	CreatedAt int64
	UpdatedAt int64
	DeletedAt *int64
	PeerID    string
}

type externalChatCursorETagInput struct {
	ID        string  `json:"id"`
	Source    string  `json:"source"`
	AgentID   *string `json:"agent_id"`
	ChannelID *string `json:"channel_id"`
	Cursor    string  `json:"cursor"`
	UpdatedAt int64   `json:"updated_at"`
	DeletedAt *int64  `json:"deleted_at"`
}

func computeExternalChatCursorETag(r *ExternalChatCursorRecord) (string, error) {
	return CanonicalETag(r.Version, externalChatCursorETagInput{
		ID:        r.ID,
		Source:    r.Source,
		AgentID:   r.AgentID,
		ChannelID: r.ChannelID,
		Cursor:    r.Cursor,
		UpdatedAt: r.UpdatedAt,
		DeletedAt: r.DeletedAt,
	})
}

// ExternalChatCursorInsertOptions lets the v0→v1 importer preserve original
// timestamps and override the clock for tests. PeerID is recorded on every
// imported row for the same audit reason as notify_cursors: cursors are
// global-scoped (other peers must see the same cursor to avoid re-fetching
// the same external history on device switch) but the row remembers which
// peer last advanced it.
type ExternalChatCursorInsertOptions struct {
	Now       int64
	CreatedAt int64
	UpdatedAt int64
	PeerID    string
}

// BulkInsertExternalChatCursors inserts many external_chat_cursor rows in
// a single transaction. Used by the v0→v1 importer; live cutover (the
// polling plugin writing each cursor as it advances) goes through a single-
// row Upsert API which is intentionally not yet exposed — the runtime still
// reads the JSONL file directly as of slice 11.
//
// Idempotency contract matches BulkInsertNotifyCursors: rows whose id
// already exists are skipped via ON CONFLICT DO NOTHING + a preload-set so
// a re-run leaves the existing row untouched. Caller records are mutated
// in place AFTER commit with assigned etag/timestamps; skipped rows are
// left untouched so callers can distinguish "imported now" from "already
// there".
//
// AgentID=&"" and ChannelID=&"" are normalized to nil on the staged copy
// so the canonical ETag (computed from the staged record) agrees with the
// round-tripped record read back from SQL (NULL → nil pointer).
func (s *Store) BulkInsertExternalChatCursors(ctx context.Context, recs []*ExternalChatCursorRecord, opts ExternalChatCursorInsertOptions) (int, error) {
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

	for i, r := range recs {
		if r == nil {
			return 0, fmt.Errorf("store.BulkInsertExternalChatCursors: nil record at index %d", i)
		}
		if r.ID == "" {
			return 0, fmt.Errorf("store.BulkInsertExternalChatCursors: index %d: id required", i)
		}
		if r.Source == "" {
			return 0, fmt.Errorf("store.BulkInsertExternalChatCursors: index %d (id=%s): source required", i, r.ID)
		}
		if r.Cursor == "" {
			return 0, fmt.Errorf("store.BulkInsertExternalChatCursors: index %d (id=%s): cursor required", i, r.ID)
		}
	}

	existing, err := preloadExistingExternalChatCursorIDs(ctx, tx, recs)
	if err != nil {
		return 0, fmt.Errorf("store.BulkInsertExternalChatCursors: preload existing: %w", err)
	}

	const q = `
INSERT INTO external_chat_cursors (
  id, source, agent_id, channel_id, cursor,
  version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
ON CONFLICT(id) DO NOTHING`
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	type stagedCursor struct {
		idx int
		rec ExternalChatCursorRecord
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
		// Normalize &"" → nil on the staged copy so the canonical ETag
		// agrees with the value read back from SQL.
		if out.AgentID != nil && *out.AgentID == "" {
			out.AgentID = nil
		}
		if out.ChannelID != nil && *out.ChannelID == "" {
			out.ChannelID = nil
		}
		out.Version = 1
		out.CreatedAt = created
		out.UpdatedAt = updated
		out.PeerID = opts.PeerID
		out.DeletedAt = nil
		etag, err := computeExternalChatCursorETag(&out)
		if err != nil {
			return 0, fmt.Errorf("store.BulkInsertExternalChatCursors: index %d (id=%s): etag: %w", i, r.ID, err)
		}
		out.ETag = etag

		res, err := stmt.ExecContext(ctx,
			out.ID, out.Source, nullableTextPtr(out.AgentID), nullableTextPtr(out.ChannelID), out.Cursor,
			out.Version, out.ETag, out.CreatedAt, out.UpdatedAt, nullableText(out.PeerID),
		)
		if err != nil {
			return 0, fmt.Errorf("store.BulkInsertExternalChatCursors: index %d (id=%s): %w", i, r.ID, err)
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

// preloadExistingExternalChatCursorIDs returns the set of cursor ids
// already in external_chat_cursors for the given batch. Delegates to
// preloadExistingKeys.
func preloadExistingExternalChatCursorIDs(ctx context.Context, tx *sql.Tx, recs []*ExternalChatCursorRecord) (map[string]bool, error) {
	return preloadExistingKeys(ctx, tx, "external_chat_cursors", "id", recs, func(r *ExternalChatCursorRecord) string { return r.ID })
}

// GetExternalChatCursor returns the row by id. ErrNotFound on miss.
func (s *Store) GetExternalChatCursor(ctx context.Context, id string) (*ExternalChatCursorRecord, error) {
	const q = `
SELECT id, source, agent_id, channel_id, cursor,
       version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
  FROM external_chat_cursors
 WHERE id = ?`
	rec, err := scanExternalChatCursorRow(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// ListExternalChatCursorsByAgent returns cursors owned by agentID ordered
// by id. agentID="" returns rows with NULL agent_id (deployment-scoped
// cursors; not used in v0 today but the schema allows them).
func (s *Store) ListExternalChatCursorsByAgent(ctx context.Context, agentID string) ([]*ExternalChatCursorRecord, error) {
	q := `
SELECT id, source, agent_id, channel_id, cursor,
       version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
  FROM external_chat_cursors
 WHERE %s
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
	var out []*ExternalChatCursorRecord
	for rows.Next() {
		rec, err := scanExternalChatCursorRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func scanExternalChatCursorRow(r rowScanner) (*ExternalChatCursorRecord, error) {
	var (
		rec       ExternalChatCursorRecord
		agentID   sql.NullString
		channelID sql.NullString
		deletedAt sql.NullInt64
	)
	if err := r.Scan(
		&rec.ID, &rec.Source, &agentID, &channelID, &rec.Cursor,
		&rec.Version, &rec.ETag, &rec.CreatedAt, &rec.UpdatedAt, &deletedAt, &rec.PeerID,
	); err != nil {
		return nil, err
	}
	if agentID.Valid {
		v := agentID.String
		rec.AgentID = &v
	}
	if channelID.Valid {
		v := channelID.String
		rec.ChannelID = &v
	}
	if deletedAt.Valid {
		v := deletedAt.Int64
		rec.DeletedAt = &v
	}
	return &rec, nil
}

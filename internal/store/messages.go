package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// messageSelectColumns is the canonical SELECT column list for
// `agent_messages m` reads. Every read path uses this exact projection
// so scanMessageRow stays the single source of truth for column
// ordering. FROM / JOIN / WHERE clauses are intentionally NOT factored
// out — readers differ on join-vs-no-join (cascade-on-tombstone via
// JOIN agents vs. tx-scoped reads that already validated the parent)
// and soft-delete predicates, and merging those would obscure intent.
const messageSelectColumns = `m.id, m.agent_id, m.seq, m.role,
       COALESCE(m.content,''), COALESCE(m.thinking,''),
       m.tool_uses, m.attachments, m.usage,
       m.version, m.etag, m.created_at, m.updated_at, m.deleted_at, COALESCE(m.peer_id,'')`

// messageTombstoneUpdate is the canonical UPDATE statement used by
// every tombstone-with-recomputed-etag path. The `deleted_at IS NULL`
// guard makes the update idempotent under concurrent deletes.
const messageTombstoneUpdate = `
UPDATE agent_messages
   SET deleted_at = ?, updated_at = ?, version = ?, etag = ?
 WHERE id = ? AND deleted_at IS NULL`

// MessageRecord mirrors the `agent_messages` table. Tool/attachment/usage
// payloads are kept as opaque JSON so message-shape upgrades don't require a
// schema migration; downstream consumers are expected to validate their own
// shape on read.
type MessageRecord struct {
	ID          string
	AgentID     string
	Seq         int64 // per-agent seq, monotonic
	Role        string
	Content     string
	Thinking    string
	ToolUses    json.RawMessage // nil-safe: empty == NULL
	Attachments json.RawMessage
	Usage       json.RawMessage

	Version   int
	ETag      string
	CreatedAt int64
	UpdatedAt int64
	DeletedAt *int64
	PeerID    string
}

// validRoles mirrors the CHECK constraint in 0001_initial.sql. We re-validate
// in Go so callers get a typed error rather than a sqlite "CHECK constraint
// failed" string surfacing through three layers of UI.
var validRoles = map[string]bool{
	"user":      true,
	"assistant": true,
	"system":    true,
	"tool":      true,
}

type messageETagInput struct {
	ID          string          `json:"id"`
	AgentID     string          `json:"agent_id"`
	Seq         int64           `json:"seq"`
	Role        string          `json:"role"`
	Content     string          `json:"content"`
	Thinking    string          `json:"thinking"`
	ToolUses    json.RawMessage `json:"tool_uses,omitempty"`
	Attachments json.RawMessage `json:"attachments,omitempty"`
	Usage       json.RawMessage `json:"usage,omitempty"`
	UpdatedAt   int64           `json:"updated_at"`
	DeletedAt   *int64          `json:"deleted_at"`
}

// computeMessageETag derives the canonical etag. Body fields (content,
// thinking, tool_uses, attachments, usage) are encoded directly into the
// canonical record so distinct payloads can't collide via accidental NUL
// concatenation — JSON encoding handles escaping for us, and canonicalJSON
// sorts keys so the result is stable regardless of struct field order.
func computeMessageETag(r *MessageRecord) (string, error) {
	return CanonicalETag(r.Version, messageETagInput{
		ID:          r.ID,
		AgentID:     r.AgentID,
		Seq:         r.Seq,
		Role:        r.Role,
		Content:     r.Content,
		Thinking:    r.Thinking,
		ToolUses:    r.ToolUses,
		Attachments: r.Attachments,
		Usage:       r.Usage,
		UpdatedAt:   r.UpdatedAt,
		DeletedAt:   r.DeletedAt,
	})
}

// MessageInsertOptions lets the v0→v1 importer preserve original timestamps
// and seq, and lets tests inject fixed clocks.
type MessageInsertOptions struct {
	Now       int64
	CreatedAt int64
	UpdatedAt int64
	Seq       int64 // 0 = allocate next per-agent
	PeerID    string
	// Fencing, when non-nil, makes the insert atomic with an
	// agent_locks holder check. The store runs CheckFencingTx as
	// the first statement of the same BEGIN IMMEDIATE tx the
	// INSERT lives in, so a peer that stole the lock between the
	// caller's check and the write surfaces as ErrFencingMismatch
	// instead of a committed stale-token write. The op-log
	// replay path threads its (peer, token) here for each entry.
	Fencing *FencingPredicate
	// Idempotency, when non-nil, gates the write on the
	// oplog_applied ledger inside the same tx — a matching row
	// short-circuits the write and returns the saved etag, a
	// mismatched row surfaces as ErrOplogOpIDReused, and a
	// missing row leads to a fresh write + ledger insert at the
	// end of the tx. Used by the op-log receive handler so
	// replays are crash-safe across the dispatch commit boundary.
	Idempotency *IdempotencyTag
}

// AppendMessage inserts a new message at the next per-agent seq. The seq
// allocation runs inside the same transaction as the insert so concurrent
// appenders for the same agent serialize on the table's UNIQUE(agent_id,seq)
// constraint rather than racing.
//
// Returns the inserted record with seq/etag/timestamps filled in.
func (s *Store) AppendMessage(ctx context.Context, rec *MessageRecord, opts MessageInsertOptions) (*MessageRecord, error) {
	if rec == nil {
		return nil, errors.New("store.AppendMessage: nil record")
	}
	if rec.ID == "" {
		return nil, errors.New("store.AppendMessage: id required")
	}
	if rec.AgentID == "" {
		return nil, errors.New("store.AppendMessage: agent_id required")
	}
	if !validRoles[rec.Role] {
		return nil, fmt.Errorf("store.AppendMessage: invalid role %q", rec.Role)
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

	// Idempotency probe runs BEFORE fencing so an exact replay of
	// a successfully-committed write succeeds even after the lock
	// has rotated. Without this ordering, a peer retry against a
	// lock that has since moved would fail the fencing gate and
	// the saved ledger row would never be consulted.
	if prior, err := checkOplogIdempotency(ctx, tx, opts.Idempotency, rec.AgentID); err != nil {
		return nil, fmt.Errorf("store.AppendMessage: %w", err)
	} else if prior != nil {
		existing, gerr := s.getMessageTx(ctx, tx, rec.ID)
		if gerr != nil {
			return nil, fmt.Errorf("store.AppendMessage: idempotent re-read: %w", gerr)
		}
		return existing, nil
	}
	// Fencing gate runs SECOND. A stolen lock surfaces as
	// ErrFencingMismatch before any read commits the writer lock
	// to the SQL statement that follows. Opt-in via opts.Fencing.
	if err := checkFencingPredicate(ctx, s, tx, opts.Fencing, rec.AgentID); err != nil {
		return nil, fmt.Errorf("store.AppendMessage: %w", err)
	}

	// Confirm the parent agent is alive — without this an importer or buggy
	// caller can chain inserts onto a soft-deleted agent and surface the
	// rows in lists once a future migration relaxes the deleted_at filter.
	var alive int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM agents WHERE id = ? AND deleted_at IS NULL`, rec.AgentID,
	).Scan(&alive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("store.AppendMessage: agent %q: %w", rec.AgentID, ErrNotFound)
		}
		return nil, err
	}

	seq := opts.Seq
	if seq == 0 {
		var maxSeq sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT MAX(seq) FROM agent_messages WHERE agent_id = ?`, rec.AgentID,
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
	out.ToolUses, err = nullJSON(out.ToolUses)
	if err != nil {
		return nil, fmt.Errorf("store.AppendMessage: tool_uses: %w", err)
	}
	out.Attachments, err = nullJSON(out.Attachments)
	if err != nil {
		return nil, fmt.Errorf("store.AppendMessage: attachments: %w", err)
	}
	out.Usage, err = nullJSON(out.Usage)
	if err != nil {
		return nil, fmt.Errorf("store.AppendMessage: usage: %w", err)
	}
	out.ETag, err = computeMessageETag(&out)
	if err != nil {
		return nil, err
	}

	const q = `
INSERT INTO agent_messages (
  id, agent_id, seq, role, content, thinking, tool_uses, attachments, usage,
  version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`
	if _, err := tx.ExecContext(ctx, q,
		out.ID, out.AgentID, out.Seq, out.Role,
		nullableText(out.Content), nullableText(out.Thinking),
		nullableRaw(out.ToolUses), nullableRaw(out.Attachments), nullableRaw(out.Usage),
		out.Version, out.ETag, out.CreatedAt, out.UpdatedAt, nullableText(out.PeerID),
	); err != nil {
		return nil, fmt.Errorf("store.AppendMessage: %w", err)
	}
	evSeq, err := RecordEvent(ctx, tx, "agent_messages", out.ID, out.ETag, EventOpInsert, out.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("store.AppendMessage: record event: %w", err)
	}
	// Idempotency ledger insert (same tx so a crash here rolls
	// back the dispatch too — no orphan write without ledger row).
	if opts.Idempotency != nil {
		if err := recordOplogAppliedTx(ctx, tx, &OplogAppliedRecord{
			OpID:        opts.Idempotency.OpID,
			AgentID:     out.AgentID,
			Fingerprint: opts.Idempotency.Fingerprint,
			ResultETag:  out.ETag,
			AppliedAt:   out.UpdatedAt,
		}); err != nil {
			return nil, fmt.Errorf("store.AppendMessage: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.fireEvent(EventRecord{
		Seq: evSeq, Table: "agent_messages", ID: out.ID, ETag: out.ETag,
		Op: EventOpInsert, TS: out.UpdatedAt,
	})
	return &out, nil
}

// BulkAppendMessages inserts a batch of messages for one agent inside a
// single transaction. Used by the v0→v1 importer for transcripts large
// enough that the per-row AppendMessage transaction overhead dominates —
// every AppendMessage opens its own BEGIN/COMMIT pair, which on SQLite
// means one fsync per message. A 100k-row transcript is roughly 100k
// fsyncs through AppendMessage; through BulkAppendMessages with a 5k-row
// chunk it is ~20.
//
// Contract:
//   - Every record's AgentID must equal agentID. A mismatch fails the
//     batch before any insert so a misrouted slice can't poison the
//     transcript.
//   - Roles are validated against validRoles up front; an invalid role on
//     any record fails the whole batch.
//   - Seqs are allocated as MAX(seq)+1, +2, ... at the start of the
//     transaction. A non-zero rec.Seq overrides allocation for that row;
//     callers using explicit seqs are responsible for keeping the
//     UNIQUE(agent_id, seq) constraint satisfied.
//   - Per-row CreatedAt/UpdatedAt: when non-zero on the record they are
//     honored verbatim, otherwise opts.CreatedAt/UpdatedAt are used,
//     otherwise opts.Now, otherwise NowMillis(). This lets the importer
//     preserve original v0 timestamps without filling MessageInsertOptions
//     row-by-row.
//   - All-or-nothing: any error mid-batch rolls the transaction back. A
//     partial-success contract would leave the caller without a clean way
//     to recover seqs after a crash.
//   - On success the input records are mutated in place — Seq, Version,
//     ETag, CreatedAt, UpdatedAt, PeerID, and the normalized JSON columns
//     are filled in so callers can read the assigned values without an
//     extra ListMessages round-trip.
//
// Cross-agent id collisions still surface as a SQLite UNIQUE error on
// the PRIMARY KEY — the bulk path does not weaken that guard.
func (s *Store) BulkAppendMessages(ctx context.Context, agentID string, recs []*MessageRecord, opts MessageInsertOptions) (int, error) {
	if agentID == "" {
		return 0, errors.New("store.BulkAppendMessages: agent_id required")
	}
	if len(recs) == 0 {
		return 0, nil
	}
	// opts.Seq is per-record in the bulk path — accepting it here would
	// silently diverge from AppendMessage, where opts.Seq overrides the
	// allocator. Per-record rec.Seq is the documented escape hatch; force
	// callers who want explicit seqs to use it.
	if opts.Seq != 0 {
		return 0, errors.New("store.BulkAppendMessages: opts.Seq not supported; set rec.Seq per record")
	}
	for i, rec := range recs {
		if rec == nil {
			return 0, fmt.Errorf("store.BulkAppendMessages: nil record at index %d", i)
		}
		if rec.ID == "" {
			return 0, fmt.Errorf("store.BulkAppendMessages: id required at index %d", i)
		}
		if rec.AgentID != agentID {
			return 0, fmt.Errorf("store.BulkAppendMessages: agent_id %q at index %d does not match batch %q",
				rec.AgentID, i, agentID)
		}
		if !validRoles[rec.Role] {
			return 0, fmt.Errorf("store.BulkAppendMessages: invalid role %q at index %d", rec.Role, i)
		}
	}

	fallbackNow := opts.Now
	if fallbackNow == 0 {
		fallbackNow = NowMillis()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// Parent-alive check mirrors AppendMessage so a soft-deleted agent
	// can't accumulate ghost rows via the bulk path.
	var alive int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM agents WHERE id = ? AND deleted_at IS NULL`, agentID,
	).Scan(&alive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("store.BulkAppendMessages: agent %q: %w", agentID, ErrNotFound)
		}
		return 0, err
	}

	// MAX(seq) once per batch, not once per row. The transaction holds the
	// writer lock so no concurrent appender can interleave between the
	// allocation here and the inserts below.
	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(seq) FROM agent_messages WHERE agent_id = ?`, agentID,
	).Scan(&maxSeq); err != nil {
		return 0, err
	}
	nextSeq := int64(1)
	if maxSeq.Valid {
		nextSeq = maxSeq.Int64 + 1
	}

	const q = `
INSERT INTO agent_messages (
  id, agent_id, seq, role, content, thinking, tool_uses, attachments, usage,
  version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	// Stage assigned values in a parallel slice so an INSERT/Commit
	// failure does not leave the caller's records half-updated. We copy
	// them onto *recs only after Commit succeeds.
	staged := make([]MessageRecord, len(recs))
	for i, rec := range recs {
		seq := rec.Seq
		if seq == 0 {
			seq = nextSeq
			nextSeq++
		}
		created := rec.CreatedAt
		if created == 0 {
			created = opts.CreatedAt
		}
		if created == 0 {
			created = fallbackNow
		}
		updated := rec.UpdatedAt
		if updated == 0 {
			updated = opts.UpdatedAt
		}
		if updated == 0 {
			updated = fallbackNow
		}

		out := *rec
		out.Seq = seq
		out.Version = 1
		out.CreatedAt = created
		out.UpdatedAt = updated
		out.PeerID = opts.PeerID
		out.DeletedAt = nil
		out.ToolUses, err = nullJSON(out.ToolUses)
		if err != nil {
			return 0, fmt.Errorf("store.BulkAppendMessages: tool_uses at index %d: %w", i, err)
		}
		out.Attachments, err = nullJSON(out.Attachments)
		if err != nil {
			return 0, fmt.Errorf("store.BulkAppendMessages: attachments at index %d: %w", i, err)
		}
		out.Usage, err = nullJSON(out.Usage)
		if err != nil {
			return 0, fmt.Errorf("store.BulkAppendMessages: usage at index %d: %w", i, err)
		}
		out.ETag, err = computeMessageETag(&out)
		if err != nil {
			return 0, fmt.Errorf("store.BulkAppendMessages: etag at index %d: %w", i, err)
		}

		if _, err := stmt.ExecContext(ctx,
			out.ID, out.AgentID, out.Seq, out.Role,
			nullableText(out.Content), nullableText(out.Thinking),
			nullableRaw(out.ToolUses), nullableRaw(out.Attachments), nullableRaw(out.Usage),
			out.Version, out.ETag, out.CreatedAt, out.UpdatedAt, nullableText(out.PeerID),
		); err != nil {
			return 0, fmt.Errorf("store.BulkAppendMessages: insert at index %d (id=%q): %w", i, out.ID, err)
		}
		staged[i] = out
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	// Commit succeeded — publish assigned seq/etag/etc. to caller's recs.
	for i := range recs {
		*recs[i] = staged[i]
	}
	return len(recs), nil
}

// GetMessage returns a single message by id. Returns ErrNotFound on miss
// OR on a soft-deleted parent agent. Cascade-on-tombstone is enforced via
// the JOIN rather than by mass UPDATE on SoftDeleteAgent — a 100k-message
// agent would otherwise pay a heavy delete cost.
func (s *Store) GetMessage(ctx context.Context, id string) (*MessageRecord, error) {
	const q = `SELECT ` + messageSelectColumns + `
  FROM agent_messages m
  JOIN agents        a ON a.id = m.agent_id
 WHERE m.id = ? AND m.deleted_at IS NULL AND a.deleted_at IS NULL`
	rec, err := scanMessageRow(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// getMessageTx is the tx-scoped variant used by AppendMessage's
// idempotent re-read branch. Same query as GetMessage but bound
// to the open writer tx so the read sees the in-progress
// transaction's snapshot (including any preceding INSERT that the
// op-log dispatch path wired up).
func (s *Store) getMessageTx(ctx context.Context, tx *sql.Tx, id string) (*MessageRecord, error) {
	const q = `SELECT ` + messageSelectColumns + `
  FROM agent_messages m
 WHERE m.id = ? AND m.deleted_at IS NULL`
	rec, err := scanMessageRow(tx.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// MessageListOptions configures ListMessages.
type MessageListOptions struct {
	// Limit caps the number of returned rows (after pagination). 0 = no cap.
	Limit int
	// BeforeSeq returns only rows with seq < BeforeSeq. 0 = no upper bound.
	// Used for keyset pagination ("older messages") matching the v0 jsonl
	// "before <id>" semantics — but we use seq, not id, because seq is
	// monotonic and survives ID re-issuance during the importer.
	BeforeSeq int64
	// SinceSeq returns only rows with seq > SinceSeq. 0 = no lower bound.
	// Used by the WebSocket invalidation feed (Phase 4) to backfill clients.
	SinceSeq int64
	// Order: "asc" returns oldest-first, "desc" newest-first. Empty = "asc".
	Order string
	// IncludeDeleted: include soft-deleted rows. Default false.
	IncludeDeleted bool
}

// ListMessages returns the messages for agentID matching opts. Always orders
// by seq (the natural conversation order); ID/timestamp order can diverge if
// the importer rewrites IDs but the design doc treats seq as authoritative.
func (s *Store) ListMessages(ctx context.Context, agentID string, opts MessageListOptions) ([]*MessageRecord, error) {
	if agentID == "" {
		return nil, errors.New("store.ListMessages: agent_id required")
	}

	args := []any{agentID}
	q := `SELECT ` + messageSelectColumns + `
  FROM agent_messages m
  JOIN agents        a ON a.id = m.agent_id
 WHERE m.agent_id = ? AND a.deleted_at IS NULL`
	if !opts.IncludeDeleted {
		q += ` AND m.deleted_at IS NULL`
	}
	if opts.BeforeSeq > 0 {
		q += ` AND m.seq < ?`
		args = append(args, opts.BeforeSeq)
	}
	if opts.SinceSeq > 0 {
		q += ` AND m.seq > ?`
		args = append(args, opts.SinceSeq)
	}
	order := "ASC"
	if opts.Order == "desc" {
		order = "DESC"
	}
	q += ` ORDER BY m.seq ` + order
	if opts.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, opts.Limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MessageRecord
	for rows.Next() {
		rec, err := scanMessageRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// HasNonAppendOnlyMessages returns true when any row in
// agent_messages for the agent shows a sign of in-place
// mutation — either soft-deleted (deleted_at IS NOT NULL) or
// edited (version > 1 / regenerate / transcript edit). The
// §3.7 incremental device-switch orchestrator uses this to
// downgrade to full-replace mode: a seq-cursor delta would
// skip both kinds of mutation (the seq is preserved across
// tombstone and edit) and let target keep showing stale
// transcript that source has updated. The bool is conservative
// — false ONLY when every row is in its original append state.
//
// LIMIT 1 + EXISTS-shape semantics keep this O(1) against the
// agent_id index even for agents with tens of thousands of
// messages.
func (s *Store) HasNonAppendOnlyMessages(ctx context.Context, agentID string) (bool, error) {
	if agentID == "" {
		return false, errors.New("store.HasNonAppendOnlyMessages: agent_id required")
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM agent_messages
		  WHERE agent_id = ?
		    AND (deleted_at IS NOT NULL OR version > 1)
		  LIMIT 1`, agentID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// CountMessages returns the number of live messages for agentID. Returns 0
// for soft-deleted agents (cascade-on-tombstone via the JOIN).
func (s *Store) CountMessages(ctx context.Context, agentID string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*)
		   FROM agent_messages m
		   JOIN agents        a ON a.id = m.agent_id
		  WHERE m.agent_id = ? AND m.deleted_at IS NULL AND a.deleted_at IS NULL`,
		agentID,
	).Scan(&n)
	return n, err
}

// TranscriptRevision returns (count, maxUpdatedAt) for the agent's live
// messages. Callers (FTS index, snapshot diff) use the pair as a cheap
// "did anything change?" cursor — count alone misses edit-in-place,
// max(updated_at) alone misses pure deletions that bring it back to a
// prior value. Together they detect every observable change to the
// transcript without scanning rows.
//
// Returns (0, 0, nil) for a tombstoned agent or empty transcript so the
// caller can treat the zero pair as the canonical "no transcript" state.
func (s *Store) TranscriptRevision(ctx context.Context, agentID string) (count int64, maxUpdatedAt int64, err error) {
	const q = `
SELECT COUNT(*), COALESCE(MAX(m.updated_at), 0)
  FROM agent_messages m
  JOIN agents        a ON a.id = m.agent_id
 WHERE m.agent_id = ? AND m.deleted_at IS NULL AND a.deleted_at IS NULL`
	err = s.db.QueryRowContext(ctx, q, agentID).Scan(&count, &maxUpdatedAt)
	return
}

// LatestMessage returns the highest-seq live message for agentID, or
// ErrNotFound if the transcript is empty / the agent is tombstoned.
func (s *Store) LatestMessage(ctx context.Context, agentID string) (*MessageRecord, error) {
	const q = `SELECT ` + messageSelectColumns + `
  FROM agent_messages m
  JOIN agents        a ON a.id = m.agent_id
 WHERE m.agent_id = ? AND m.deleted_at IS NULL AND a.deleted_at IS NULL
 ORDER BY m.seq DESC LIMIT 1`
	rec, err := scanMessageRow(s.db.QueryRowContext(ctx, q, agentID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// UpdateMessageContent rewrites the content/thinking/tool_uses of a message
// in place, bumping version+etag. Only fields the caller specifies via
// non-nil pointers are touched; passing all nils is a no-op (returns the
// existing record).
type MessagePatch struct {
	Content     *string
	Thinking    *string
	ToolUses    json.RawMessage // nil = unchanged; empty []byte("null") to clear
	Attachments json.RawMessage
	Usage       json.RawMessage
}

// UpdateMessage applies patch to the message identified by id, with optional
// If-Match etag check. Returns the new record.
func (s *Store) UpdateMessage(ctx context.Context, id, ifMatchETag string, patch MessagePatch) (*MessageRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// JOIN agents WHERE alive enforces cascade-on-tombstone for mutations
	// the same way the read helpers do. Without it a write could land on a
	// tombstoned-parent row and resurrect content via the change feed.
	const sel = `SELECT ` + messageSelectColumns + `
  FROM agent_messages m
  JOIN agents        a ON a.id = m.agent_id
 WHERE m.id = ? AND m.deleted_at IS NULL AND a.deleted_at IS NULL`
	cur, err := scanMessageRow(tx.QueryRowContext(ctx, sel, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if ifMatchETag != "" && cur.ETag != ifMatchETag {
		return nil, ErrETagMismatch
	}

	// All-nil patch: documented no-op. Without this we would still
	// recompute etag and bump version — fine semantically, but
	// surprising to callers who treated UpdateMessage as conditional.
	if patch.Content == nil && patch.Thinking == nil &&
		patch.ToolUses == nil && patch.Attachments == nil && patch.Usage == nil {
		return cur, nil
	}

	next := *cur
	if patch.Content != nil {
		next.Content = *patch.Content
	}
	if patch.Thinking != nil {
		next.Thinking = *patch.Thinking
	}
	if patch.ToolUses != nil {
		next.ToolUses, err = nullJSON(patch.ToolUses)
		if err != nil {
			return nil, fmt.Errorf("store.UpdateMessage: tool_uses: %w", err)
		}
	}
	if patch.Attachments != nil {
		next.Attachments, err = nullJSON(patch.Attachments)
		if err != nil {
			return nil, fmt.Errorf("store.UpdateMessage: attachments: %w", err)
		}
	}
	if patch.Usage != nil {
		next.Usage, err = nullJSON(patch.Usage)
		if err != nil {
			return nil, fmt.Errorf("store.UpdateMessage: usage: %w", err)
		}
	}
	next.Version = cur.Version + 1
	next.UpdatedAt = NowMillis()
	next.ETag, err = computeMessageETag(&next)
	if err != nil {
		return nil, err
	}

	// AND deleted_at IS NULL guards a tombstone race between SELECT and
	// UPDATE. Without it a concurrent SoftDeleteMessage could lose its
	// effect when this UPDATE wins the writer lock.
	const upd = `
UPDATE agent_messages
   SET content = ?, thinking = ?, tool_uses = ?, attachments = ?, usage = ?,
       version = ?, etag = ?, updated_at = ?
 WHERE id = ? AND etag = ? AND deleted_at IS NULL`
	res, err := tx.ExecContext(ctx, upd,
		nullableText(next.Content), nullableText(next.Thinking),
		nullableRaw(next.ToolUses), nullableRaw(next.Attachments), nullableRaw(next.Usage),
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
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &next, nil
}

// SoftDeleteMessage tombstones a message. Idempotent on a missing/dead
// row when ifMatchETag is empty. Recomputes etag so the tombstone is
// visible in the change feed (messageETagInput captures DeletedAt — a
// stale "alive" etag would bypass invalidation).
//
// ifMatchETag, when non-empty, enforces optimistic locking against the
// pre-delete row's etag: a missing/dead row returns ErrNotFound (so the
// caller can distinguish "already gone" from "your view is stale"), and
// a live-but-different etag returns ErrETagMismatch. Empty ifMatchETag
// preserves the legacy idempotent behaviour for daemon-internal callers.
func (s *Store) SoftDeleteMessage(ctx context.Context, id, ifMatchETag string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	const sel = `SELECT ` + messageSelectColumns + `
  FROM agent_messages m
  JOIN agents        a ON a.id = m.agent_id
 WHERE m.id = ? AND m.deleted_at IS NULL AND a.deleted_at IS NULL`
	cur, err := scanMessageRow(tx.QueryRowContext(ctx, sel, id))
	if errors.Is(err, sql.ErrNoRows) {
		// Conditional callers must see a precondition signal so the UI can
		// refetch — silently "succeeding" on a row that no longer exists
		// would let a stale client think its delete landed.
		if ifMatchETag != "" {
			return ErrNotFound
		}
		return nil // idempotent (also a no-op for tombstoned-parent messages)
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
	newETag, err := computeMessageETag(cur)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, messageTombstoneUpdate, now, now, cur.Version, newETag, id); err != nil {
		return err
	}
	return tx.Commit()
}

// TruncateMessagesAfterSeq tombstones every live message for agentID whose
// seq > afterSeq. Used by lifecycle paths (Reset). The regenerate flow
// uses TruncateForRegenerate instead, which derives afterSeq from a
// pivot inside the transaction so the boundary cannot drift on a
// cross-device prefix mutation.
//
// pivotID + pivotETag are accepted as a soft precondition for callers
// that want optimistic locking against an arbitrary row (e.g. future
// "rollback to this checkpoint" features); both empty disables the
// check. Returns ErrNotFound if the pivot row is gone/tombstoned and
// ErrETagMismatch on a live-but-different etag.
//
// Iterates row-by-row inside a single transaction so each affected row gets
// a freshly recomputed etag. A bulk UPDATE would leave stale etags and
// silently break If-Match for downstream readers.
func (s *Store) TruncateMessagesAfterSeq(ctx context.Context, agentID string, afterSeq int64, pivotID, pivotETag string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if pivotID != "" && pivotETag != "" {
		var (
			etag string
			del  sql.NullInt64
		)
		err := tx.QueryRowContext(ctx,
			`SELECT m.etag, m.deleted_at
			   FROM agent_messages m
			   JOIN agents        a ON a.id = m.agent_id
			  WHERE m.id = ? AND m.agent_id = ? AND a.deleted_at IS NULL`,
			pivotID, agentID,
		).Scan(&etag, &del)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		if err != nil {
			return 0, err
		}
		if del.Valid {
			return 0, ErrNotFound
		}
		if etag != pivotETag {
			return 0, ErrETagMismatch
		}
	}

	const sel = `SELECT ` + messageSelectColumns + `
  FROM agent_messages m
  JOIN agents        a ON a.id = m.agent_id
 WHERE m.agent_id = ? AND m.seq > ? AND m.deleted_at IS NULL AND a.deleted_at IS NULL
 ORDER BY m.seq ASC`
	rows, err := tx.QueryContext(ctx, sel, agentID, afterSeq)
	if err != nil {
		return 0, err
	}
	var targets []*MessageRecord
	for rows.Next() {
		rec, err := scanMessageRow(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		targets = append(targets, rec)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	now := NowMillis()
	var n int64
	for _, rec := range targets {
		rec.Version++
		rec.UpdatedAt = now
		rec.DeletedAt = &now
		newETag, err := computeMessageETag(rec)
		if err != nil {
			return n, err
		}
		res, err := tx.ExecContext(ctx, messageTombstoneUpdate, now, now, rec.Version, newETag, rec.ID)
		if err != nil {
			return n, err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return n, err
		}
		n += affected
	}
	if err := tx.Commit(); err != nil {
		return n, err
	}
	return n, nil
}

// TruncateMessagesFromCreatedAt tombstones every live message for agentID
// whose created_at >= sinceMillis. Sister method to TruncateMessagesAfterSeq
// for callers that need a wall-clock boundary instead of a seq pivot — the
// memory-truncate flow uses it to drop everything at or after a user-picked
// datetime in one shot.
//
// Same row-by-row tombstone-with-recomputed-etag pattern as
// TruncateMessagesAfterSeq, for the same reason: a bulk UPDATE would
// leave stale etags and silently break If-Match for downstream readers.
func (s *Store) TruncateMessagesFromCreatedAt(ctx context.Context, agentID string, sinceMillis int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	const sel = `SELECT ` + messageSelectColumns + `
  FROM agent_messages m
  JOIN agents        a ON a.id = m.agent_id
 WHERE m.agent_id = ? AND m.created_at >= ? AND m.deleted_at IS NULL AND a.deleted_at IS NULL
 ORDER BY m.seq ASC`
	rows, err := tx.QueryContext(ctx, sel, agentID, sinceMillis)
	if err != nil {
		return 0, err
	}
	var targets []*MessageRecord
	for rows.Next() {
		rec, err := scanMessageRow(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		targets = append(targets, rec)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	now := NowMillis()
	var n int64
	for _, rec := range targets {
		rec.Version++
		rec.UpdatedAt = now
		rec.DeletedAt = &now
		newETag, err := computeMessageETag(rec)
		if err != nil {
			return n, err
		}
		res, err := tx.ExecContext(ctx, messageTombstoneUpdate, now, now, rec.Version, newETag, rec.ID)
		if err != nil {
			return n, err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return n, err
		}
		n += affected
	}
	if err := tx.Commit(); err != nil {
		return n, err
	}
	return n, nil
}

// TruncateForRegenerate atomically validates the pivot row's etag and
// tombstones the suffix derived from the pivot's *immutable* seq —
// all inside a single transaction. This is the regenerate-flow
// counterpart to TruncateMessagesAfterSeq: callers don't compute
// afterSeq themselves (avoiding the TOCTOU window where a cross-device
// prefix delete shifts the boundary), they just describe the click.
//
// killPivot:
//   - true: tombstone seq >= pivot.Seq (assistant-mode regenerate —
//     the user clicked an assistant response, wants it and everything
//     after gone, then re-runs from the preceding user message).
//   - false: tombstone seq > pivot.Seq (user-mode regenerate — the
//     user clicked their own message, wants to keep it but redo
//     everything after).
//
// pivotID must be non-empty (the boundary is pivot-relative — empty
// pivot is rejected rather than silently accepting "kill everything"
// since the legacy kill-all path goes through TruncateMessagesAfterSeq
// instead). pivotETag is optional: when non-empty, mismatch returns
// ErrETagMismatch; when empty, the etag check is skipped but the
// pivot's seq is still used to derive the boundary atomically — so
// callers without optimistic-locking still get correct boundary
// semantics under cross-device prefix mutations.
//
// sourceID + sourceETag close a second TOCTOU window specific to
// assistant-mode regenerate: the row whose content drives the chat
// (the user message preceding the clicked assistant pivot) is read
// outside this transaction by Manager.Regenerate to feed prepareChat.
// If a cross-device edit / tombstone lands between that read and this
// truncate, the chat would re-run against stale content. By passing
// the snapshot's etag here, the same TX that validates the pivot can
// also re-validate the source — mismatch returns ErrETagMismatch so
// the caller can surface 412 instead of committing a regen against
// disappeared/edited input.
//
// When sourceID == "" the source-side precondition is dropped
// entirely (CLI / internal callers without optimistic locking).
// When sourceID == pivotID (user-mode regenerate, where the source
// IS the pivot) the pivot SELECT's curETag is reused to validate
// sourceETag — no extra round trip, but the precondition is still
// honoured even when the caller left pivotETag empty. When
// sourceID != pivotID (assistant-mode), a second SELECT inside this
// TX reads the source row and validates identity + etag (etag check
// only when sourceETag != "").
func (s *Store) TruncateForRegenerate(ctx context.Context, agentID, pivotID, pivotETag, sourceID, sourceETag string, killPivot bool) error {
	if pivotID == "" {
		return errors.New("store: TruncateForRegenerate requires non-empty pivotID")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Locate the pivot row and validate identity + etag inside the TX
	// so subsequent UPDATEs see the same view of the world.
	var (
		pivotSeq  int64
		curETag   string
		curAgent  string
		deletedAt sql.NullInt64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT m.seq, m.etag, m.agent_id, m.deleted_at
		   FROM agent_messages m
		   JOIN agents        a ON a.id = m.agent_id
		  WHERE m.id = ? AND a.deleted_at IS NULL`,
		pivotID,
	).Scan(&pivotSeq, &curETag, &curAgent, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if curAgent != agentID {
		// Cross-agent guard — surface as not-found, never as etag
		// mismatch (a 412 oracle on the wrong agent's etag space
		// would let callers probe sibling agents).
		return ErrNotFound
	}
	if deletedAt.Valid {
		return ErrNotFound
	}
	if pivotETag != "" && curETag != pivotETag {
		return ErrETagMismatch
	}

	// User-mode regenerate (sourceID == pivotID) shares the row with
	// the pivot, so the pivot SELECT above is the only read needed.
	// When the caller passed sourceETag while leaving pivotETag
	// empty (e.g. a CLI caller that captured only the source-side
	// snapshot, or any future client splitting the two preconditions
	// apart), the etag check would otherwise silently drop. The
	// guard below re-uses curETag from the pivot SELECT to honour
	// the source-side precondition without an extra round trip.
	if sourceID != "" && sourceID == pivotID && sourceETag != "" && curETag != sourceETag {
		return ErrETagMismatch
	}

	// Assistant-mode regenerate: source row is the user message
	// preceding the clicked assistant pivot — different row, needs its
	// own SELECT. Same TX as the pivot read so a concurrent edit on
	// the source between Manager.Regenerate's GetMessage and this
	// point surfaces as ErrETagMismatch / ErrNotFound rather than
	// committing a regen against stale content.
	if sourceID != "" && sourceID != pivotID {
		var (
			srcETag    string
			srcAgent   string
			srcDeleted sql.NullInt64
		)
		err = tx.QueryRowContext(ctx,
			`SELECT m.etag, m.agent_id, m.deleted_at
			   FROM agent_messages m
			   JOIN agents        a ON a.id = m.agent_id
			  WHERE m.id = ? AND a.deleted_at IS NULL`,
			sourceID,
		).Scan(&srcETag, &srcAgent, &srcDeleted)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if srcAgent != agentID {
			// Cross-agent — same surface as the pivot guard above.
			return ErrNotFound
		}
		if srcDeleted.Valid {
			return ErrNotFound
		}
		if sourceETag != "" && srcETag != sourceETag {
			return ErrETagMismatch
		}
	}

	afterSeq := pivotSeq
	if killPivot {
		afterSeq = pivotSeq - 1
	}

	const sel = `SELECT ` + messageSelectColumns + `
  FROM agent_messages m
 WHERE m.agent_id = ? AND m.seq > ? AND m.deleted_at IS NULL
 ORDER BY m.seq ASC`
	rows, err := tx.QueryContext(ctx, sel, agentID, afterSeq)
	if err != nil {
		return err
	}
	var targets []*MessageRecord
	for rows.Next() {
		rec, err := scanMessageRow(rows)
		if err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, rec)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	now := NowMillis()
	for _, rec := range targets {
		rec.Version++
		rec.UpdatedAt = now
		rec.DeletedAt = &now
		newETag, err := computeMessageETag(rec)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, messageTombstoneUpdate, now, now, rec.Version, newETag, rec.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func scanMessageRow(r rowScanner) (*MessageRecord, error) {
	var (
		rec         MessageRecord
		toolUses    sql.NullString
		attachments sql.NullString
		usage       sql.NullString
		deletedAt   sql.NullInt64
	)
	if err := r.Scan(
		&rec.ID, &rec.AgentID, &rec.Seq, &rec.Role,
		&rec.Content, &rec.Thinking,
		&toolUses, &attachments, &usage,
		&rec.Version, &rec.ETag, &rec.CreatedAt, &rec.UpdatedAt, &deletedAt, &rec.PeerID,
	); err != nil {
		return nil, err
	}
	if toolUses.Valid {
		rec.ToolUses = json.RawMessage(toolUses.String)
	}
	if attachments.Valid {
		rec.Attachments = json.RawMessage(attachments.String)
	}
	if usage.Valid {
		rec.Usage = json.RawMessage(usage.String)
	}
	if deletedAt.Valid {
		v := deletedAt.Int64
		rec.DeletedAt = &v
	}
	return &rec, nil
}

// nullJSON normalizes empty JSON to nil so callers can treat "no value"
// uniformly, and rejects malformed JSON up front. Storing invalid JSON would
// pass SQLite (the schema has no json_valid CHECK on these columns) but blow
// up later in any reader that decodes the column — far from the offending
// caller.
func nullJSON(b json.RawMessage) (json.RawMessage, error) {
	if len(b) == 0 {
		return nil, nil
	}
	if !json.Valid(b) {
		return nil, errors.New("store: invalid JSON payload")
	}
	return b, nil
}

func nullableRaw(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

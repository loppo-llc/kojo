package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// validTaskStatuses mirrors the agent_tasks.status CHECK constraint in
// 0001_initial.sql. We re-validate in Go so a buggy caller surfaces as a
// typed error rather than a sqlite "CHECK constraint failed" string
// bubbling through the HTTP layer.
//
// Note: this is the v1 vocabulary. v0 used "open"/"done"; the cutover layer
// in internal/agent/task.go and the v0→v1 importer translate "open" →
// "pending" before reaching the store.
var validTaskStatuses = map[string]bool{
	"pending":     true,
	"in_progress": true,
	"done":        true,
	"cancelled":   true,
}

// AgentTaskRecord mirrors the `agent_tasks` table.
type AgentTaskRecord struct {
	ID      string
	AgentID string
	Seq     int64 // per-agent monotonic
	Title   string
	Body    string // optional, may be empty
	Status  string // 'pending'|'in_progress'|'done'|'cancelled'
	DueAt   *int64

	Version   int
	ETag      string
	CreatedAt int64
	UpdatedAt int64
	DeletedAt *int64
	PeerID    string
}

// agentTaskETagInput is the canonical record shape for agent_tasks.etag.
//
// docs/multi-device-storage.md §3.2 lists "id, agent_id, title, body,
// status, due_at" as the canonical fields. We extend that with seq,
// updated_at, deleted_at to match the in-tree convention used by every
// other table (agent_messages, memory_entries, agent_persona, etc.):
//
//   - updated_at: without it a no-op UPDATE keeps the same etag, but a
//     same-value re-write of every other field would not bump the etag,
//     defeating optimistic concurrency for the "touch row to bump
//     mtime" idiom.
//   - deleted_at: tombstoning a row would otherwise keep the live etag,
//     so a stale client could "successfully" assert an etag against a
//     tombstone.
//   - seq: per-agent ordering is part of the row's identity for the UI
//     list view; reordering must invalidate cached etags.
//
// This divergence is intentional and uniform across tables; the doc spec
// is the under-specified one. See agent_messages, memory_entries for
// the same shape.
type agentTaskETagInput struct {
	ID        string `json:"id"`
	AgentID   string `json:"agent_id"`
	Seq       int64  `json:"seq"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Status    string `json:"status"`
	DueAt     *int64 `json:"due_at"`
	UpdatedAt int64  `json:"updated_at"`
	DeletedAt *int64 `json:"deleted_at"`
}

func computeAgentTaskETag(r *AgentTaskRecord) (string, error) {
	return CanonicalETag(r.Version, agentTaskETagInput{
		ID:        r.ID,
		AgentID:   r.AgentID,
		Seq:       r.Seq,
		Title:     r.Title,
		Body:      r.Body,
		Status:    r.Status,
		DueAt:     r.DueAt,
		UpdatedAt: r.UpdatedAt,
		DeletedAt: r.DeletedAt,
	})
}

// AgentTaskInsertOptions lets callers (notably the v0→v1 importer)
// preserve original timestamps and override the clock for tests.
//
// Seq is intentionally not exposed here: both CreateAgentTask and
// BulkInsertAgentTasks allocate seq from MAX(seq)+1 inside the
// transaction. Caller-supplied seq would race against parallel writers
// for the same agent; serialization on the SQLite write lock + the
// unique (agent_id, seq) constraint is the canonical guard. The v0
// → v1 importer doesn't need to preserve original task ordering
// either: tasks have no inherent timeline (unlike messages) and seq
// is a stable-sort tiebreaker, not part of the row's identity.
type AgentTaskInsertOptions struct {
	Now       int64
	CreatedAt int64
	UpdatedAt int64
	PeerID    string
}

// CreateAgentTask inserts a new task at the next per-agent seq. Seq
// allocation runs inside the same transaction as the insert so concurrent
// creators serialize on the table's UNIQUE(agent_id, seq) constraint
// rather than racing.
func (s *Store) CreateAgentTask(ctx context.Context, rec *AgentTaskRecord, opts AgentTaskInsertOptions) (*AgentTaskRecord, error) {
	if rec == nil {
		return nil, errors.New("store.CreateAgentTask: nil record")
	}
	if rec.ID == "" {
		return nil, errors.New("store.CreateAgentTask: id required")
	}
	if rec.AgentID == "" {
		return nil, errors.New("store.CreateAgentTask: agent_id required")
	}
	if rec.Title == "" {
		return nil, errors.New("store.CreateAgentTask: title required")
	}
	if !validTaskStatuses[rec.Status] {
		return nil, fmt.Errorf("store.CreateAgentTask: invalid status %q", rec.Status)
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

	// Parent agent must be alive — same invariant as messages/memory_entries.
	var alive int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM agents WHERE id = ? AND deleted_at IS NULL`, rec.AgentID,
	).Scan(&alive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("store.CreateAgentTask: agent %q: %w", rec.AgentID, ErrNotFound)
		}
		return nil, err
	}

	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(seq) FROM agent_tasks WHERE agent_id = ?`, rec.AgentID,
	).Scan(&maxSeq); err != nil {
		return nil, err
	}
	seq := int64(1)
	if maxSeq.Valid {
		seq = maxSeq.Int64 + 1
	}

	out := *rec
	out.Seq = seq
	out.Version = 1
	out.CreatedAt = created
	out.UpdatedAt = updated
	out.PeerID = opts.PeerID
	out.DeletedAt = nil
	out.ETag, err = computeAgentTaskETag(&out)
	if err != nil {
		return nil, err
	}

	const q = `
INSERT INTO agent_tasks (
  id, agent_id, seq, title, body, status, due_at,
  version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`
	if _, err := tx.ExecContext(ctx, q,
		out.ID, out.AgentID, out.Seq, out.Title, out.Body, out.Status, nullableInt64Ptr(out.DueAt),
		out.Version, out.ETag, out.CreatedAt, out.UpdatedAt, nullableText(out.PeerID),
	); err != nil {
		return nil, fmt.Errorf("store.CreateAgentTask: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &out, nil
}

// BulkInsertAgentTasks inserts many tasks for one agent in a single
// transaction. Used by the v0→v1 importer; live UI flows should use
// CreateAgentTask one at a time so per-task etags / events are emitted.
//
// Idempotency: rows whose id already exists for *this same agent* are
// skipped silently. A row whose id is already present under a *different*
// agent surfaces as a hard error rather than a silent skip — that
// situation is a v0 data-integrity violation (task_<random> ids should
// not collide across agents) and corrupting the v1 store by attaching
// a different agent's row to this batch would be silent data loss.
//
// Seq allocation: the per-row Seq is allocated inside the transaction
// from MAX(seq)+1 and advances only when an INSERT actually lands.
// Skipped duplicates (same-agent ids hit by the preload) do not burn
// seq values, so re-runs over the same input don't widen seq gaps.
// Caller-supplied r.Seq is ignored. This sidesteps the race where a
// parallel CreateAgentTask for the same agent could claim a seq the
// bulk caller wanted.
//
// Returned count is the number of rows actually inserted (new for this
// run). After commit, each successfully-inserted record in `recs` is
// updated in-place with its canonical Seq / Version / ETag / timestamps;
// records that were skipped (duplicate id under same agent) are left
// untouched.
//
// CreatedAt/UpdatedAt fallback: r.CreatedAt → opts.CreatedAt → now,
// then r.UpdatedAt → opts.UpdatedAt → CreatedAt. This matches the
// "explicit per-row > batch-level > clock" precedence the rest of the
// store layer follows.
func (s *Store) BulkInsertAgentTasks(ctx context.Context, agentID string, recs []*AgentTaskRecord, opts AgentTaskInsertOptions) (int, error) {
	if agentID == "" {
		return 0, errors.New("store.BulkInsertAgentTasks: agent_id required")
	}
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

	// Parent agent must be alive.
	var alive int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM agents WHERE id = ? AND deleted_at IS NULL`, agentID,
	).Scan(&alive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("store.BulkInsertAgentTasks: agent %q: %w", agentID, ErrNotFound)
		}
		return 0, err
	}

	// Up-front validation: nil / empty-string / invalid-status surfaces
	// before we touch the DB so a pathological record at index 50 doesn't
	// leave 49 partial inserts on rollback.
	for i, r := range recs {
		if r == nil {
			return 0, fmt.Errorf("store.BulkInsertAgentTasks: nil record at index %d", i)
		}
		if r.ID == "" {
			return 0, fmt.Errorf("store.BulkInsertAgentTasks: index %d: id required", i)
		}
		if r.AgentID != agentID {
			return 0, fmt.Errorf("store.BulkInsertAgentTasks: index %d: agent_id mismatch (%q vs %q)", i, r.AgentID, agentID)
		}
		if r.Title == "" {
			return 0, fmt.Errorf("store.BulkInsertAgentTasks: index %d: title required", i)
		}
		if !validTaskStatuses[r.Status] {
			return 0, fmt.Errorf("store.BulkInsertAgentTasks: index %d: invalid status %q", i, r.Status)
		}
	}

	// Pre-load existing rows for this batch's ids. The agent_id from
	// each match decides whether to skip silently (same agent) or fail
	// loudly (cross-agent collision).
	existingForAgent, err := preloadExistingTaskIDs(ctx, tx, agentID, recs)
	if err != nil {
		return 0, fmt.Errorf("store.BulkInsertAgentTasks: preload existing: %w", err)
	}

	// Seq baseline inside the tx so a parallel CreateAgentTask cannot
	// claim seq values we're about to use. SQLite serializes writers, so
	// MAX(seq) read here is the post-commit value once we hold the write
	// lock for the upcoming INSERTs.
	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(seq) FROM agent_tasks WHERE agent_id = ?`, agentID,
	).Scan(&maxSeq); err != nil {
		return 0, err
	}
	nextSeq := int64(1)
	if maxSeq.Valid {
		nextSeq = maxSeq.Int64 + 1
	}

	const q = `
INSERT INTO agent_tasks (
  id, agent_id, seq, title, body, status, due_at,
  version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
ON CONFLICT(id) DO NOTHING`
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	type stagedTask struct {
		idx int
		rec AgentTaskRecord
	}
	staged := make([]stagedTask, 0, len(recs))

	inserted := 0
	for i, r := range recs {
		if existingForAgent[r.ID] {
			// Already in DB under this same agent — skip silently. Don't
			// burn a seq value: re-runs would otherwise widen seq gaps
			// every time without a corresponding row.
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
		out.Seq = nextSeq
		out.Version = 1
		out.CreatedAt = created
		out.UpdatedAt = updated
		out.PeerID = opts.PeerID
		out.DeletedAt = nil
		etag, err := computeAgentTaskETag(&out)
		if err != nil {
			return 0, fmt.Errorf("store.BulkInsertAgentTasks: index %d: etag: %w", i, err)
		}
		out.ETag = etag

		res, err := stmt.ExecContext(ctx,
			out.ID, out.AgentID, out.Seq, out.Title, out.Body, out.Status, nullableInt64Ptr(out.DueAt),
			out.Version, out.ETag, out.CreatedAt, out.UpdatedAt, nullableText(out.PeerID),
		)
		if err != nil {
			return 0, fmt.Errorf("store.BulkInsertAgentTasks: index %d (id=%s): %w", i, r.ID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		if n > 0 {
			inserted++
			nextSeq++
			staged = append(staged, stagedTask{idx: i, rec: out})
		}
		// n == 0 here means a duplicate id appeared *within this same
		// batch*: the first occurrence inserted, the second hit ON
		// CONFLICT DO NOTHING. Concurrent writers can't interleave —
		// the SQLite write lock is held from our first INSERT through
		// the matching commit, so a second tx can't sneak in. Skip
		// silently to keep the "first-write-wins on duplicate id"
		// guarantee uniform with the preload-hit path.
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}

	// Apply staged mutations to caller records *after* commit. A failure
	// before this point leaves recs untouched so the caller's error
	// handling never sees half-written canonical fields.
	for _, s := range staged {
		*recs[s.idx] = s.rec
	}
	return inserted, nil
}

// preloadExistingTaskIDs returns the set of task ids in `recs` that are
// already present in agent_tasks under agentID. Cross-agent collisions
// (same id under a different agent) surface as an error so the caller
// can abort rather than silently dropping the batch row on conflict.
//
// The query uses an IN-list scoped by id, chunked to chunkSize entries
// per round-trip to stay well under SQLite's 999-variable default
// limit. The total cost is bounded by len(recs)/chunkSize round-trips
// instead of len(recs) per-row probes.
func preloadExistingTaskIDs(ctx context.Context, tx *sql.Tx, agentID string, recs []*AgentTaskRecord) (map[string]bool, error) {
	if len(recs) == 0 {
		return nil, nil
	}
	// SQLite's variable limit is 999 by default; chunk to stay well below.
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
		q := `SELECT id, agent_id FROM agent_tasks WHERE id IN (` + string(placeholders) + `)`
		rows, err := tx.QueryContext(ctx, q, ids...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, owner string
			if err := rows.Scan(&id, &owner); err != nil {
				rows.Close()
				return nil, err
			}
			if owner != agentID {
				rows.Close()
				return nil, fmt.Errorf("task id %q already owned by agent %q (cross-agent collision)", id, owner)
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

// GetAgentTask returns a single live task by id. ErrNotFound on miss,
// tombstone, or tombstoned parent agent.
func (s *Store) GetAgentTask(ctx context.Context, id string) (*AgentTaskRecord, error) {
	const q = `
SELECT t.id, t.agent_id, t.seq, t.title, COALESCE(t.body,'') AS body, t.status, t.due_at,
       t.version, t.etag, t.created_at, t.updated_at, t.deleted_at, COALESCE(t.peer_id,'')
  FROM agent_tasks t
  JOIN agents      a ON a.id = t.agent_id
 WHERE t.id = ? AND t.deleted_at IS NULL AND a.deleted_at IS NULL`
	rec, err := scanAgentTaskRow(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// AgentTaskListOptions configures ListAgentTasks.
type AgentTaskListOptions struct {
	Status string // "" = all statuses
	Limit  int    // 0 = unbounded
	Cursor int64  // seq strictly greater (keyset paging); 0 = from start
}

// ListAgentTasks returns the live tasks for agentID ordered by seq ASC.
func (s *Store) ListAgentTasks(ctx context.Context, agentID string, opts AgentTaskListOptions) ([]*AgentTaskRecord, error) {
	if agentID == "" {
		return nil, errors.New("store.ListAgentTasks: agent_id required")
	}
	args := []any{agentID}
	q := `
SELECT t.id, t.agent_id, t.seq, t.title, COALESCE(t.body,'') AS body, t.status, t.due_at,
       t.version, t.etag, t.created_at, t.updated_at, t.deleted_at, COALESCE(t.peer_id,'')
  FROM agent_tasks t
  JOIN agents      a ON a.id = t.agent_id
 WHERE t.agent_id = ? AND t.deleted_at IS NULL AND a.deleted_at IS NULL`
	if opts.Status != "" {
		if !validTaskStatuses[opts.Status] {
			return nil, fmt.Errorf("store.ListAgentTasks: invalid status %q", opts.Status)
		}
		q += ` AND t.status = ?`
		args = append(args, opts.Status)
	}
	if opts.Cursor > 0 {
		q += ` AND t.seq > ?`
		args = append(args, opts.Cursor)
	}
	q += ` ORDER BY t.seq ASC`
	if opts.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, opts.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AgentTaskRecord
	for rows.Next() {
		rec, err := scanAgentTaskRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// AgentTaskPatch supports partial updates. nil = leave unchanged. To
// clear DueAt, set ClearDueAt=true (a nil DueAt is otherwise read as
// "leave alone" — Go has no tri-state pointer-of-pointer convention here).
type AgentTaskPatch struct {
	Title       *string
	Body        *string
	Status      *string
	DueAt       *int64
	ClearDueAt  bool
}

// UpdateAgentTask applies patch to the task identified by id with optional
// If-Match etag check. Returns the updated record.
func (s *Store) UpdateAgentTask(ctx context.Context, id, ifMatchETag string, patch AgentTaskPatch) (*AgentTaskRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	const sel = `
SELECT t.id, t.agent_id, t.seq, t.title, COALESCE(t.body,'') AS body, t.status, t.due_at,
       t.version, t.etag, t.created_at, t.updated_at, t.deleted_at, COALESCE(t.peer_id,'')
  FROM agent_tasks t
  JOIN agents      a ON a.id = t.agent_id
 WHERE t.id = ? AND t.deleted_at IS NULL AND a.deleted_at IS NULL`
	cur, err := scanAgentTaskRow(tx.QueryRowContext(ctx, sel, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if ifMatchETag != "" && cur.ETag != ifMatchETag {
		return nil, ErrETagMismatch
	}
	if patch.Title == nil && patch.Body == nil && patch.Status == nil && patch.DueAt == nil && !patch.ClearDueAt {
		return cur, nil
	}

	next := *cur
	if patch.Title != nil {
		if *patch.Title == "" {
			return nil, errors.New("store.UpdateAgentTask: title must not be empty")
		}
		next.Title = *patch.Title
	}
	if patch.Body != nil {
		next.Body = *patch.Body
	}
	if patch.Status != nil {
		if !validTaskStatuses[*patch.Status] {
			return nil, fmt.Errorf("store.UpdateAgentTask: invalid status %q", *patch.Status)
		}
		next.Status = *patch.Status
	}
	if patch.ClearDueAt {
		next.DueAt = nil
	} else if patch.DueAt != nil {
		v := *patch.DueAt
		next.DueAt = &v
	}
	next.Version = cur.Version + 1
	next.UpdatedAt = NowMillis()
	next.ETag, err = computeAgentTaskETag(&next)
	if err != nil {
		return nil, err
	}

	const upd = `
UPDATE agent_tasks
   SET title = ?, body = ?, status = ?, due_at = ?,
       version = ?, etag = ?, updated_at = ?
 WHERE id = ? AND etag = ? AND deleted_at IS NULL`
	res, err := tx.ExecContext(ctx, upd,
		next.Title, next.Body, next.Status, nullableInt64Ptr(next.DueAt),
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
		// Either the row vanished (unlikely — we just SELECTed it inside
		// the same tx) or a concurrent mutator landed between SELECT and
		// UPDATE. Surface as etag mismatch so callers refetch and retry.
		return nil, ErrETagMismatch
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &next, nil
}

// SoftDeleteAgentTask tombstones a task.
//
// Empty ifMatchETag → unconditional, idempotent (a missing or already
// tombstoned row returns nil).
// Non-empty ifMatchETag → conditional: missing/tombstoned/mismatched
// surface as ErrETagMismatch.
func (s *Store) SoftDeleteAgentTask(ctx context.Context, id, ifMatchETag string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	const sel = `
SELECT t.id, t.agent_id, t.seq, t.title, COALESCE(t.body,'') AS body, t.status, t.due_at,
       t.version, t.etag, t.created_at, t.updated_at, t.deleted_at, COALESCE(t.peer_id,'')
  FROM agent_tasks t
  JOIN agents      a ON a.id = t.agent_id
 WHERE t.id = ? AND t.deleted_at IS NULL AND a.deleted_at IS NULL`
	cur, err := scanAgentTaskRow(tx.QueryRowContext(ctx, sel, id))
	if errors.Is(err, sql.ErrNoRows) {
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
	newETag, err := computeAgentTaskETag(cur)
	if err != nil {
		return err
	}

	if ifMatchETag != "" {
		const updCond = `
UPDATE agent_tasks
   SET deleted_at = ?, updated_at = ?, version = ?, etag = ?
 WHERE id = ? AND etag = ? AND deleted_at IS NULL`
		res, err := tx.ExecContext(ctx, updCond, now, now, cur.Version, newETag, id, ifMatchETag)
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
UPDATE agent_tasks
   SET deleted_at = ?, updated_at = ?, version = ?, etag = ?
 WHERE id = ? AND deleted_at IS NULL`
		if _, err := tx.ExecContext(ctx, updUncond, now, now, cur.Version, newETag, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteAllAgentTasks hard-deletes every task row for agentID. Used by
// the data-reset flow that nukes an agent's persistent state. Tombstones
// are removed too — the operation is "vacate this agent's task slate"
// regardless of liveness. Bypasses etag because the data-reset flow
// owns the agent for the duration of the call.
func (s *Store) DeleteAllAgentTasks(ctx context.Context, agentID string) (int64, error) {
	if agentID == "" {
		return 0, errors.New("store.DeleteAllAgentTasks: agent_id required")
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM agent_tasks WHERE agent_id = ?`, agentID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanAgentTaskRow(r rowScanner) (*AgentTaskRecord, error) {
	var (
		rec       AgentTaskRecord
		dueAt     sql.NullInt64
		deletedAt sql.NullInt64
	)
	if err := r.Scan(
		&rec.ID, &rec.AgentID, &rec.Seq, &rec.Title, &rec.Body, &rec.Status, &dueAt,
		&rec.Version, &rec.ETag, &rec.CreatedAt, &rec.UpdatedAt, &deletedAt, &rec.PeerID,
	); err != nil {
		return nil, err
	}
	if dueAt.Valid {
		v := dueAt.Int64
		rec.DueAt = &v
	}
	if deletedAt.Valid {
		v := deletedAt.Int64
		rec.DeletedAt = &v
	}
	return &rec, nil
}

// nullableInt64Ptr converts an optional *int64 into the form database/sql
// understands. Returns sql.NullInt64{Valid:false} when the pointer is nil
// so the column is written as SQL NULL rather than 0.
func nullableInt64Ptr(p *int64) any {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}

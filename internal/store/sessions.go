package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// validSessionStatuses mirrors the sessions.status CHECK constraint in
// 0001_initial.sql. Re-validated in Go so a buggy caller surfaces as a
// typed error rather than a sqlite "CHECK constraint failed" string.
var validSessionStatuses = map[string]bool{
	"running":  true,
	"stopped":  true,
	"archived": true,
}

// SessionRecord mirrors the `sessions` table. The session row is local-
// scoped (per-peer PTY state) — peer_id records which physical device
// launched / archived this session so cross-device snapshot inspection
// can attribute the row to its origin. The schema enforces a global
// `id TEXT PRIMARY KEY`, so two peers cannot legally allocate the
// same id; if a snapshot merge surfaces a collision the merger must
// reject one side rather than silently overwrite. peer_id is
// disambiguating metadata, not a tiebreaker.
type SessionRecord struct {
	ID        string
	AgentID   *string // nullable — sessions can be detached / pre-agent
	Status    string  // 'running' | 'stopped' | 'archived'
	PID       *int64
	Cmd       string
	WorkDir   string
	StartedAt *int64
	StoppedAt *int64
	ExitCode  *int64

	Seq       int64
	Version   int
	ETag      string
	CreatedAt int64
	UpdatedAt int64
	DeletedAt *int64
	PeerID    string
}

type sessionETagInput struct {
	ID        string  `json:"id"`
	AgentID   *string `json:"agent_id"`
	Status    string  `json:"status"`
	PID       *int64  `json:"pid"`
	Cmd       string  `json:"cmd"`
	WorkDir   string  `json:"work_dir"`
	StartedAt *int64  `json:"started_at"`
	StoppedAt *int64  `json:"stopped_at"`
	ExitCode  *int64  `json:"exit_code"`
	Seq       int64   `json:"seq"`
	UpdatedAt int64   `json:"updated_at"`
	DeletedAt *int64  `json:"deleted_at"`
}

func computeSessionETag(r *SessionRecord) (string, error) {
	return CanonicalETag(r.Version, sessionETagInput{
		ID:        r.ID,
		AgentID:   r.AgentID,
		Status:    r.Status,
		PID:       r.PID,
		Cmd:       r.Cmd,
		WorkDir:   r.WorkDir,
		StartedAt: r.StartedAt,
		StoppedAt: r.StoppedAt,
		ExitCode:  r.ExitCode,
		Seq:       r.Seq,
		UpdatedAt: r.UpdatedAt,
		DeletedAt: r.DeletedAt,
	})
}

// SessionInsertOptions lets the v0→v1 importer preserve original
// timestamps and override the clock for tests.
//
// Seq is intentionally not exposed: BulkInsertSessions allocates seq
// via NextGlobalSeq(), the same atomic CAS counter that backs every
// global-partition table (agents / agent_persona / agent_memory /
// groupdms / sessions / events). Caller-supplied seq would diverge
// from the global counter and let two writers across different tables
// produce the same seq value.
type SessionInsertOptions struct {
	Now       int64
	CreatedAt int64
	UpdatedAt int64
	PeerID    string
}

// BulkInsertSessions inserts many session rows in a single transaction.
// Used by the v0→v1 importer; live runtime callers (PTY launch,
// status transitions) go through dedicated single-row APIs that
// emit per-row events. Same idempotency contract as
// BulkInsertAgentTasks: rows whose id already exists are skipped via
// ON CONFLICT DO NOTHING + a preload-set. Seq is allocated via
// NextGlobalSeq() (the global CAS counter shared across global-
// partition tables; see SessionInsertOptions.Seq comment).
//
// Caller records are mutated in place AFTER commit with their
// assigned seq/etag/timestamps. Records skipped by the preload are
// left untouched.
func (s *Store) BulkInsertSessions(ctx context.Context, recs []*SessionRecord, opts SessionInsertOptions) (int, error) {
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

	// Up-front validation. Surface here so a pathological record at
	// index N doesn't leave N-1 partial inserts on rollback.
	//
	// AgentID=&"" is normalized to nil on the staged copy below so
	// the canonical ETag (computed from the staged record) agrees
	// with the value read back from SQL (NULL scans into a nil
	// pointer). Doing the rewrite on the staged copy rather than
	// the caller's slice keeps recs[i] untouched on validation /
	// commit failure.
	for i, r := range recs {
		if r == nil {
			return 0, fmt.Errorf("store.BulkInsertSessions: nil record at index %d", i)
		}
		if r.ID == "" {
			return 0, fmt.Errorf("store.BulkInsertSessions: index %d: id required", i)
		}
		if !validSessionStatuses[r.Status] {
			return 0, fmt.Errorf("store.BulkInsertSessions: index %d: invalid status %q", i, r.Status)
		}
	}

	// Preload existing ids so a re-run silently skips already-imported
	// rows (rather than failing on PK conflict). Sessions don't have
	// the cross-agent collision problem agent_tasks does — id is
	// session-scoped and AgentID is nullable — so a duplicate id is
	// always "same row, already imported" and skipping is safe.
	existing, err := preloadExistingSessionIDs(ctx, tx, recs)
	if err != nil {
		return 0, fmt.Errorf("store.BulkInsertSessions: preload existing: %w", err)
	}

	// sessions uses the global seq allocator (see seedGlobalSeq in
	// store.go: agents / agent_persona / agent_memory / groupdms /
	// sessions / events all share NextGlobalSeq). Using table-local
	// MAX(seq)+1 here would race a parallel global allocator and let
	// two writers produce the same seq across tables. NextGlobalSeq()
	// is a CAS counter seeded from the union-MAX at boot, so the
	// values it hands out are guaranteed monotonic.

	const q = `
INSERT INTO sessions (
  id, agent_id, status, pid, cmd, work_dir,
  started_at, stopped_at, exit_code,
  seq, version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
ON CONFLICT(id) DO NOTHING`
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	type stagedSession struct {
		idx int
		rec SessionRecord
	}
	staged := make([]stagedSession, 0, len(recs))

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
		// Normalize AgentID=&"" → nil on the staged copy so the
		// canonical ETag (computed from `out`) and the round-tripped
		// record read back from SQL (NULL → nil pointer) agree. The
		// caller's recs[i] stays untouched.
		if out.AgentID != nil && *out.AgentID == "" {
			out.AgentID = nil
		}
		out.Seq = NextGlobalSeq()
		out.Version = 1
		out.CreatedAt = created
		out.UpdatedAt = updated
		out.PeerID = opts.PeerID
		out.DeletedAt = nil
		etag, err := computeSessionETag(&out)
		if err != nil {
			return 0, fmt.Errorf("store.BulkInsertSessions: index %d: etag: %w", i, err)
		}
		out.ETag = etag

		res, err := stmt.ExecContext(ctx,
			out.ID, nullableTextPtr(out.AgentID), out.Status, nullableInt64Ptr(out.PID),
			out.Cmd, out.WorkDir,
			nullableInt64Ptr(out.StartedAt), nullableInt64Ptr(out.StoppedAt), nullableInt64Ptr(out.ExitCode),
			out.Seq, out.Version, out.ETag, out.CreatedAt, out.UpdatedAt, nullableText(out.PeerID),
		)
		if err != nil {
			return 0, fmt.Errorf("store.BulkInsertSessions: index %d (id=%s): %w", i, r.ID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		if n > 0 {
			inserted++
			staged = append(staged, stagedSession{idx: i, rec: out})
		}
		// n == 0 here means a duplicate id appeared within the same
		// batch — first wins via ON CONFLICT DO NOTHING. Concurrent
		// writers can't interleave: SQLite holds the write lock from
		// our first INSERT through commit.
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	for _, s := range staged {
		*recs[s.idx] = s.rec
	}
	return inserted, nil
}

// preloadExistingSessionIDs returns the set of session ids already in
// the sessions table for the given batch. Delegates to
// preloadExistingKeys.
func preloadExistingSessionIDs(ctx context.Context, tx *sql.Tx, recs []*SessionRecord) (map[string]bool, error) {
	return preloadExistingKeys(ctx, tx, "sessions", "id", recs, func(r *SessionRecord) string { return r.ID })
}

// GetSession returns the row by id. ErrNotFound on miss. Used by tests
// and audit tools; the live PTY runtime keeps its own in-memory state.
func (s *Store) GetSession(ctx context.Context, id string) (*SessionRecord, error) {
	const q = `
SELECT id, agent_id, status, pid, COALESCE(cmd,''), COALESCE(work_dir,''),
       started_at, stopped_at, exit_code,
       seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
  FROM sessions
 WHERE id = ?`
	rec, err := scanSessionRow(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// ListSessionsByAgent returns sessions owned by agentID ordered by seq.
// Includes archived rows so the importer can verify a round-trip.
// agentID="" returns rows with NULL agent_id only — matching the
// "detached / pre-agent" sessions semantic.
func (s *Store) ListSessionsByAgent(ctx context.Context, agentID string) ([]*SessionRecord, error) {
	q := `
SELECT id, agent_id, status, pid, COALESCE(cmd,''), COALESCE(work_dir,''),
       started_at, stopped_at, exit_code,
       seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
  FROM sessions
 WHERE %s
 ORDER BY seq ASC`
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
	var out []*SessionRecord
	for rows.Next() {
		rec, err := scanSessionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func scanSessionRow(r rowScanner) (*SessionRecord, error) {
	var (
		rec       SessionRecord
		agentID   sql.NullString
		pid       sql.NullInt64
		startedAt sql.NullInt64
		stoppedAt sql.NullInt64
		exitCode  sql.NullInt64
		deletedAt sql.NullInt64
	)
	if err := r.Scan(
		&rec.ID, &agentID, &rec.Status, &pid, &rec.Cmd, &rec.WorkDir,
		&startedAt, &stoppedAt, &exitCode,
		&rec.Seq, &rec.Version, &rec.ETag, &rec.CreatedAt, &rec.UpdatedAt, &deletedAt, &rec.PeerID,
	); err != nil {
		return nil, err
	}
	if agentID.Valid {
		v := agentID.String
		rec.AgentID = &v
	}
	if pid.Valid {
		v := pid.Int64
		rec.PID = &v
	}
	if startedAt.Valid {
		v := startedAt.Int64
		rec.StartedAt = &v
	}
	if stoppedAt.Valid {
		v := stoppedAt.Int64
		rec.StoppedAt = &v
	}
	if exitCode.Valid {
		v := exitCode.Int64
		rec.ExitCode = &v
	}
	if deletedAt.Valid {
		v := deletedAt.Int64
		rec.DeletedAt = &v
	}
	return &rec, nil
}

// nullableTextPtr converts *string to a database/sql NULL-aware value.
// Distinct from nullableText (which takes a plain string and emits NULL
// for ""): a session with AgentID=nil legitimately means "detached",
// while a session with AgentID=&"" would be a malformed row that we
// surface as NULL too rather than persisting an empty-string FK target.
func nullableTextPtr(p *string) any {
	if p == nil || *p == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: *p, Valid: true}
}

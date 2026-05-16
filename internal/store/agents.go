package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNotFound is returned by repo Get/Update helpers when no row matches.
var ErrNotFound = errors.New("store: not found")

// ErrETagMismatch is returned by Update helpers when an If-Match check fails.
// Callers should treat this as a 412 Precondition Failed at the API layer.
var ErrETagMismatch = errors.New("store: etag mismatch")

// AgentRecord mirrors the `agents` table. Settings carries every backend/UI
// preference flag that doesn't deserve its own column; the DB stores the raw
// JSON so older binaries can preserve forward-introduced keys via round-trip
// (the "soft" forward-compat the design doc calls for in §3.3).
type AgentRecord struct {
	ID          string
	Name        string
	PersonaRef  string
	Settings    map[string]any
	WorkspaceID string

	// common columns
	Seq       int64
	Version   int
	ETag      string
	CreatedAt int64
	UpdatedAt int64
	DeletedAt *int64
	PeerID    string
}

// AgentPersonaRecord mirrors the `agent_persona` table. Body is the canonical
// persona text; body_sha256 is recomputed on every update so the change feed
// can de-dupe no-op writes upstream.
type AgentPersonaRecord struct {
	AgentID    string
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

// agentETagInput is the canonical record shape for the agents table's etag.
// Field order is irrelevant (CanonicalETag canonicalizes), but the *set* of
// fields is the contract: changing it requires bumping `version`.
type agentETagInput struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	PersonaRef  string         `json:"persona_ref"`
	Settings    map[string]any `json:"settings"`
	WorkspaceID string         `json:"workspace_id"`
	UpdatedAt   int64          `json:"updated_at"`
	DeletedAt   *int64         `json:"deleted_at"`
}

type agentPersonaETagInput struct {
	AgentID    string `json:"agent_id"`
	BodySHA256 string `json:"body_sha256"`
	UpdatedAt  int64  `json:"updated_at"`
	DeletedAt  *int64 `json:"deleted_at"`
}

func computeAgentETag(r *AgentRecord) (string, error) {
	return CanonicalETag(r.Version, agentETagInput{
		ID:          r.ID,
		Name:        r.Name,
		PersonaRef:  r.PersonaRef,
		Settings:    r.Settings,
		WorkspaceID: r.WorkspaceID,
		UpdatedAt:   r.UpdatedAt,
		DeletedAt:   r.DeletedAt,
	})
}

func computeAgentPersonaETag(r *AgentPersonaRecord) (string, error) {
	return CanonicalETag(r.Version, agentPersonaETagInput{
		AgentID:    r.AgentID,
		BodySHA256: r.BodySHA256,
		UpdatedAt:  r.UpdatedAt,
		DeletedAt:  r.DeletedAt,
	})
}

// AgentInsertOptions lets callers override timestamps/seq for tests and the
// v0→v1 importer (which must preserve the original CreatedAt to keep the
// change feed's "happened-before" relation intact).
type AgentInsertOptions struct {
	Now       int64 // 0 = NowMillis()
	Seq       int64 // 0 = NextGlobalSeq()
	CreatedAt int64 // 0 = Now
	UpdatedAt int64 // 0 = Now
	PeerID    string
	// AllowOverwrite lets UpsertAgentPersona blindly replace an existing
	// live persona without an If-Match etag. Reserved for the v0→v1
	// importer and daemon-internal callers that already serialize against
	// per-agent state. API handlers MUST leave this false and supply
	// ifMatchETag instead.
	AllowOverwrite bool
}

// InsertAgent writes a new agents row. Seq, ETag, CreatedAt, UpdatedAt are
// filled in from opts (or defaulted) and reflected back into the returned
// record so callers don't have to re-read.
func (s *Store) InsertAgent(ctx context.Context, rec *AgentRecord, opts AgentInsertOptions) (*AgentRecord, error) {
	if rec == nil {
		return nil, errors.New("store.InsertAgent: nil record")
	}
	if rec.ID == "" {
		return nil, errors.New("store.InsertAgent: id required")
	}
	if rec.Name == "" {
		return nil, errors.New("store.InsertAgent: name required")
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
	seq := opts.Seq
	if seq == 0 {
		seq = NextGlobalSeq()
	}

	settingsJSON, err := marshalSettings(rec.Settings)
	if err != nil {
		return nil, fmt.Errorf("store.InsertAgent: settings: %w", err)
	}

	out := *rec
	out.Seq = seq
	out.Version = 1
	out.CreatedAt = created
	out.UpdatedAt = updated
	out.PeerID = opts.PeerID
	out.DeletedAt = nil
	if out.PersonaRef == "" {
		out.PersonaRef = rec.ID
	}
	if out.WorkspaceID == "" {
		out.WorkspaceID = rec.ID
	}
	out.ETag, err = computeAgentETag(&out)
	if err != nil {
		return nil, err
	}

	const q = `
INSERT INTO agents (
  id, name, persona_ref, settings_json, workspace_id,
  seq, version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`
	// tx wraps both the row insert AND the RecordEvent so a peer
	// reading /api/v1/changes never sees an event for a row that
	// failed to commit (or vice versa).
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store.InsertAgent: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, q,
		out.ID, out.Name, nullableText(out.PersonaRef), settingsJSON, nullableText(out.WorkspaceID),
		out.Seq, out.Version, out.ETag, out.CreatedAt, out.UpdatedAt, nullableText(out.PeerID),
	); err != nil {
		return nil, fmt.Errorf("store.InsertAgent: %w", err)
	}
	evSeq, err := RecordEvent(ctx, tx, "agents", out.ID, out.ETag, EventOpInsert, out.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("store.InsertAgent: record event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store.InsertAgent: commit: %w", err)
	}
	s.fireEvent(EventRecord{
		Seq: evSeq, Table: "agents", ID: out.ID, ETag: out.ETag,
		Op: EventOpInsert, TS: out.UpdatedAt,
	})
	return &out, nil
}

// GetAgent returns the agent by id. Returns ErrNotFound if missing or
// soft-deleted (callers wanting to inspect tombstones must use a dedicated
// helper — there is none yet because no caller needs that).
func (s *Store) GetAgent(ctx context.Context, id string) (*AgentRecord, error) {
	const q = `
SELECT id, name, COALESCE(persona_ref, ''), settings_json, COALESCE(workspace_id, ''),
       seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id, '')
  FROM agents WHERE id = ? AND deleted_at IS NULL`
	row := s.db.QueryRowContext(ctx, q, id)
	rec, err := scanAgentRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// ListAgents returns all live agents, ordered by seq ASC (stable for UI lists
// and deterministic for snapshot diffs in tests).
func (s *Store) ListAgents(ctx context.Context) ([]*AgentRecord, error) {
	const q = `
SELECT id, name, COALESCE(persona_ref, ''), settings_json, COALESCE(workspace_id, ''),
       seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id, '')
  FROM agents
 WHERE deleted_at IS NULL
 ORDER BY seq ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AgentRecord
	for rows.Next() {
		rec, err := scanAgentRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// UpdateAgent applies mutate inside a transaction with optimistic locking on
// etag. ifMatchETag may be empty to skip the check (used by daemon-internal
// callers that already serialize against per-agent state); API handlers MUST
// pass the client's If-Match value.
//
// mutate receives a pointer to a copy of the current record. The function
// must not mutate read-only fields (ID, CreatedAt, Seq); UpdatedAt and ETag
// are recomputed by UpdateAgent regardless of what mutate sets.
func (s *Store) UpdateAgent(ctx context.Context, id string, ifMatchETag string, mutate func(*AgentRecord) error) (*AgentRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	const sel = `
SELECT id, name, COALESCE(persona_ref, ''), settings_json, COALESCE(workspace_id, ''),
       seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id, '')
  FROM agents WHERE id = ? AND deleted_at IS NULL`
	row := tx.QueryRowContext(ctx, sel, id)
	cur, err := scanAgentRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if ifMatchETag != "" && cur.ETag != ifMatchETag {
		return nil, ErrETagMismatch
	}

	next := *cur
	if next.Settings != nil {
		// deep-ish copy: callers can append to slices/maps safely without
		// mutating cur (which is harmless here, but becomes important once
		// we cache reads in front of the DB).
		next.Settings = cloneJSONMap(next.Settings)
	}
	if err := mutate(&next); err != nil {
		return nil, err
	}

	// Reset every column the caller is not allowed to mutate. Without this
	// a careless mutate() could swap ID/Seq/CreatedAt/DeletedAt and the
	// canonical-record etag would diverge from the row that survives the
	// UPDATE, silently breaking If-Match for downstream readers.
	next.ID = cur.ID
	next.Seq = cur.Seq
	next.CreatedAt = cur.CreatedAt
	next.DeletedAt = cur.DeletedAt
	next.PeerID = cur.PeerID

	next.Version = cur.Version + 1
	next.UpdatedAt = NowMillis()
	next.ETag, err = computeAgentETag(&next)
	if err != nil {
		return nil, err
	}

	settingsJSON, err := marshalSettings(next.Settings)
	if err != nil {
		return nil, err
	}

	// AND deleted_at IS NULL guards against a soft-delete that landed
	// between SELECT and UPDATE — without it we'd resurrect a tombstone
	// in-place. The etag check alone isn't enough because SoftDeleteAgent
	// also recomputes etag.
	const upd = `
UPDATE agents SET
  name = ?, persona_ref = ?, settings_json = ?, workspace_id = ?,
  version = ?, etag = ?, updated_at = ?
WHERE id = ? AND etag = ? AND deleted_at IS NULL`
	res, err := tx.ExecContext(ctx, upd,
		next.Name, nullableText(next.PersonaRef), settingsJSON, nullableText(next.WorkspaceID),
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
		// Lost the race — between SELECT and UPDATE, another writer mutated
		// the row. Surfacing as ErrETagMismatch lets callers retry the same
		// way they would for an If-Match failure.
		return nil, ErrETagMismatch
	}
	evSeq, err := RecordEvent(ctx, tx, "agents", id, next.ETag, EventOpUpdate, next.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("store.UpdateAgent: record event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.fireEvent(EventRecord{
		Seq: evSeq, Table: "agents", ID: id, ETag: next.ETag,
		Op: EventOpUpdate, TS: next.UpdatedAt,
	})
	return &next, nil
}

// SoftDeleteAgent tombstones the agent. Idempotent: deleting an already-
// deleted (or absent) row returns nil so callers can use it in at-least-once
// delivery paths without bookkeeping. The etag is recomputed because
// agentETagInput captures DeletedAt — leaving the old etag would surface a
// stale "alive" cache hit for any reader that resolves the row by id.
func (s *Store) SoftDeleteAgent(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	const sel = `
SELECT id, name, COALESCE(persona_ref, ''), settings_json, COALESCE(workspace_id, ''),
       seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id, '')
  FROM agents WHERE id = ? AND deleted_at IS NULL`
	cur, err := scanAgentRow(tx.QueryRowContext(ctx, sel, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil // idempotent
	}
	if err != nil {
		return err
	}

	now := NowMillis()
	deletedAt := now
	cur.Version++
	cur.UpdatedAt = now
	cur.DeletedAt = &deletedAt
	newETag, err := computeAgentETag(cur)
	if err != nil {
		return err
	}

	const upd = `
UPDATE agents
   SET deleted_at = ?, updated_at = ?, version = ?, etag = ?
 WHERE id = ? AND deleted_at IS NULL`
	if _, err := tx.ExecContext(ctx, upd, deletedAt, now, cur.Version, newETag, id); err != nil {
		return err
	}
	// Delete events carry no etag — the row is gone from a peer's
	// point of view (the soft-delete is an internal-only retention
	// detail). Peers receiving the event drop their cache row outright.
	evSeq, err := RecordEvent(ctx, tx, "agents", id, "", EventOpDelete, now)
	if err != nil {
		return fmt.Errorf("store.SoftDeleteAgent: record event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.fireEvent(EventRecord{
		Seq: evSeq, Table: "agents", ID: id, Op: EventOpDelete, TS: now,
	})
	return nil
}

// HardDeleteAgent removes the row outright. Cascades to agent_persona /
// agent_memory / agent_messages / agent_tasks / memory_entries / agent_locks /
// cron_runs / compactions via the schema's ON DELETE CASCADE. Intended for
// the eventual operator-driven hard-delete pass (planned `--clean` target;
// not yet implemented) and tests; production deletes go through
// SoftDeleteAgent + GC.
func (s *Store) HardDeleteAgent(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE id = ?`, id)
	return err
}

// rowScanner is the minimal interface satisfied by *sql.Row and *sql.Rows so
// we can share scan code.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanAgentRow(r rowScanner) (*AgentRecord, error) {
	var (
		rec       AgentRecord
		settings  string
		deletedAt sql.NullInt64
	)
	if err := r.Scan(
		&rec.ID, &rec.Name, &rec.PersonaRef, &settings, &rec.WorkspaceID,
		&rec.Seq, &rec.Version, &rec.ETag, &rec.CreatedAt, &rec.UpdatedAt, &deletedAt, &rec.PeerID,
	); err != nil {
		return nil, err
	}
	if deletedAt.Valid {
		v := deletedAt.Int64
		rec.DeletedAt = &v
	}
	if settings != "" {
		if err := json.Unmarshal([]byte(settings), &rec.Settings); err != nil {
			return nil, fmt.Errorf("store: decode settings_json for %s: %w", rec.ID, err)
		}
	}
	return &rec, nil
}

func marshalSettings(m map[string]any) (string, error) {
	if m == nil {
		return "{}", nil
	}
	buf, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

// cloneJSONMap is a shallow round-trip clone via JSON. Acceptable because
// settings_json is bounded and only contains JSON-roundtrippable values; the
// alternative (reflect-based deep copy) would silently disagree with the
// canonical etag input on edge cases (e.g. *bool pointers).
func cloneJSONMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	buf, err := json.Marshal(m)
	if err != nil {
		return m
	}
	var out map[string]any
	if err := json.Unmarshal(buf, &out); err != nil {
		return m
	}
	return out
}

func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// -- agent_persona ------------------------------------------------------

// UpsertAgentPersona writes (or replaces) the persona row for agentID. The
// persona body lives both here (canonical) and on the filesystem under
// `<v1>/global/agents/<id>/persona.md` (handled by the blob layer in Phase 3).
// This helper only touches the DB row; the blob mirror is wired up later.
//
// ifMatchETag enforces optimistic locking against the prior row's etag —
// pass "" to skip (used by the v0→v1 importer and by daemon-internal callers
// that already serialize against per-agent state). API handlers MUST pass
// the client's If-Match value or "" only when no prior row exists.
func (s *Store) UpsertAgentPersona(ctx context.Context, agentID, body, ifMatchETag string, opts AgentInsertOptions) (*AgentPersonaRecord, error) {
	if agentID == "" {
		return nil, errors.New("store.UpsertAgentPersona: agent_id required")
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

	// Reject persona writes against tombstoned agents — the FK alone would
	// allow it (CASCADE only triggers on DELETE, not on tombstone), and a
	// resurrection here would surface zombie persona rows in the change feed
	// after the agent was GC'd.
	var alive int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM agents WHERE id = ? AND deleted_at IS NULL`, agentID,
	).Scan(&alive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("store.UpsertAgentPersona: agent %q: %w", agentID, ErrNotFound)
		}
		return nil, err
	}

	// Read the prior row (live OR tombstoned). The two cases follow distinct
	// write paths:
	//   - liveExists  → in-place UPDATE keeping seq + created_at
	//   - tombstone   → physical REPLACE; seq + created_at reset to fresh values
	//   - none        → fresh INSERT
	const sel = `
SELECT body, body_sha256, seq, version, etag, created_at, updated_at, deleted_at, COALESCE(peer_id,'')
  FROM agent_persona WHERE agent_id = ?`
	var (
		prev       AgentPersonaRecord
		deletedAt  sql.NullInt64
		liveExists bool
		anyRow     bool
	)
	prev.AgentID = agentID
	err = tx.QueryRowContext(ctx, sel, agentID).Scan(
		&prev.Body, &prev.BodySHA256, &prev.Seq, &prev.Version,
		&prev.ETag, &prev.CreatedAt, &prev.UpdatedAt, &deletedAt, &prev.PeerID,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Nothing at all.
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

	switch {
	case ifMatchETag != "":
		// Caller asserted a prior etag.
		if !liveExists || prev.ETag != ifMatchETag {
			return nil, ErrETagMismatch
		}
	case liveExists && !opts.AllowOverwrite:
		// No If-Match given, but a live row exists — refuse blind overwrite.
		// Importers/internal callers that legitimately need to replace a
		// row without an etag must set opts.AllowOverwrite.
		return nil, ErrETagMismatch
	}

	rec := AgentPersonaRecord{
		AgentID:    agentID,
		Body:       body,
		BodySHA256: SHA256Hex([]byte(body)),
		PeerID:     opts.PeerID,
	}

	if liveExists {
		// In-place update preserves seq exactly. opts.Seq is ignored here
		// because the SQL UPDATE doesn't touch the seq column — honoring
		// opts.Seq would only desync the returned record from the row.
		// Importers that need a specific seq must use a fresh tombstoned
		// row (resurrection path) or write directly via SQL.
		rec.Seq = prev.Seq
		rec.Version = prev.Version + 1
		rec.CreatedAt = prev.CreatedAt
	} else {
		// Fresh persona OR resurrection of a tombstoned row — both reset
		// the version chain.
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
	rec.ETag, err = computeAgentPersonaETag(&rec)
	if err != nil {
		return nil, err
	}

	if liveExists {
		// In-place update preserves seq/created_at exactly.
		const upd = `
UPDATE agent_persona SET
  body = ?, body_sha256 = ?, version = ?, etag = ?, updated_at = ?, peer_id = ?
WHERE agent_id = ? AND deleted_at IS NULL`
		if _, err := tx.ExecContext(ctx, upd,
			rec.Body, rec.BodySHA256, rec.Version, rec.ETag, rec.UpdatedAt,
			nullableText(rec.PeerID), agentID,
		); err != nil {
			return nil, err
		}
	} else {
		// Either no row at all, or only a tombstone. Use explicit DELETE +
		// INSERT instead of INSERT OR REPLACE: REPLACE resolves UNIQUE
		// conflicts by deleting *every* conflicting row, including
		// idx_agent_persona_seq matches against unrelated agents — that
		// would silently wipe an unrelated persona if seq happened to
		// collide. The tx holds the writer lock (BEGIN IMMEDIATE), so the
		// two statements together are atomic from any other writer's
		// point of view.
		if anyRow {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM agent_persona WHERE agent_id = ?`, rec.AgentID,
			); err != nil {
				return nil, err
			}
		}
		const ins = `
INSERT INTO agent_persona (
  agent_id, body, body_sha256, seq, version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`
		if _, err := tx.ExecContext(ctx, ins,
			rec.AgentID, rec.Body, rec.BodySHA256, rec.Seq, rec.Version, rec.ETag,
			rec.CreatedAt, rec.UpdatedAt, nullableText(rec.PeerID),
		); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &rec, nil
}

// GetAgentPersona returns the persona for agentID, or ErrNotFound. The join
// against agents enforces the parent-alive invariant on read so callers can't
// resurrect a soft-deleted agent's persona via a stale id reference.
func (s *Store) GetAgentPersona(ctx context.Context, agentID string) (*AgentPersonaRecord, error) {
	const q = `
SELECT p.body, p.body_sha256, p.seq, p.version, p.etag, p.created_at, p.updated_at, p.deleted_at, COALESCE(p.peer_id,'')
  FROM agent_persona p
  JOIN agents       a ON a.id = p.agent_id
 WHERE p.agent_id = ? AND p.deleted_at IS NULL AND a.deleted_at IS NULL`
	var (
		rec       AgentPersonaRecord
		deletedAt sql.NullInt64
	)
	rec.AgentID = agentID
	err := s.db.QueryRowContext(ctx, q, agentID).Scan(
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

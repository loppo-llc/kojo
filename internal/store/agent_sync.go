package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// nullableJSON converts an empty / nil json.RawMessage to a SQL NULL.
// Mirrors nullableText for raw-JSON columns (tool_uses, attachments,
// usage on agent_messages) so a missing field round-trips as NULL
// instead of the literal string "null" — distinguishing "no row data"
// from "explicit JSON null" is part of the canonical etag contract.
func nullableJSON(v json.RawMessage) any {
	if len(v) == 0 {
		return nil
	}
	return string(v)
}

// AgentSyncPayload is the bundle of rows a §3.7 device-switch
// pushes from source to target so the agent's runtime can continue
// on the new home_peer. The orchestrator builds this on the
// source side from its own kojo.db; the target applies it via
// SyncAgentFromPeer inside one transaction.
//
// Empty slices / nil pointers are honoured: a target that
// receives no Persona / Memory / Messages / MemoryEntries clears
// its corresponding rows (the agent existed on source without
// that surface, so target should mirror).
type AgentSyncPayload struct {
	Agent         *AgentRecord
	Persona       *AgentPersonaRecord  // nil = no persona on source
	Memory        *AgentMemoryRecord   // nil = no MEMORY.md on source
	Messages      []*MessageRecord     // empty = clear (full mode) OR no new rows (incremental mode)
	MemoryEntries []*MemoryEntryRecord // empty = clear (full mode) OR no new rows (incremental mode)
	Tasks         []*AgentTaskRecord   // empty = clear target's tasks

	// IncrementalMessages, when true, switches syncMessagesTx
	// from "delete-then-insert" (source-wins full replace) to
	// "upsert" (UPSERT supplied rows, leave the rest alone).
	// The §3.7 incremental device-switch orchestrator sets this
	// after fetching target's max(agent_messages.seq) and
	// shipping only rows newer than that — the existing rows on
	// target are bit-identical to source's by virtue of seq +
	// etag, so DELETE would just waste I/O. ON CONFLICT(id)
	// keeps the path idempotent across retries.
	IncrementalMessages bool

	// IncrementalMemoryEntries is the analogue for memory_entries.
	IncrementalMemoryEntries bool
}

// SyncAgentFromPeer overwrites the target's local copy of one
// agent's metadata + transcript + memory with the source-supplied
// payload. Atomic across all five tables: a crash mid-call rolls
// back so the target never observes a half-synced state.
//
// Semantics are "source wins": every field in payload is written
// verbatim; existing rows on target are replaced (messages /
// memory_entries are DELETE-then-INSERT, agents / persona / memory
// are UPSERT). Version + etag are taken from the source record
// so reads after sync return the same canonical state the source
// saw — UI cross-peer reads stay consistent.
//
// Caller MUST validate the payload's agent_id is the one it
// expects (the orchestrator does this against the agent it's
// currently switching). This function does not enforce
// authorization; the HTTP layer (handlePeerAgentSync) gates
// RolePeer + caller-source identity matching.
func (s *Store) SyncAgentFromPeer(ctx context.Context, payload AgentSyncPayload) error {
	if payload.Agent == nil {
		return errors.New("store.SyncAgentFromPeer: nil agent record")
	}
	agentID := payload.Agent.ID
	if agentID == "" {
		return errors.New("store.SyncAgentFromPeer: agent id required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.SyncAgentFromPeer: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := syncAgentRowTx(ctx, tx, payload.Agent); err != nil {
		return err
	}
	if err := syncAgentPersonaTx(ctx, tx, agentID, payload.Persona); err != nil {
		return err
	}
	if err := syncAgentMemoryTx(ctx, tx, agentID, payload.Memory); err != nil {
		return err
	}
	if err := syncMessagesTx(ctx, tx, agentID, payload.Messages, payload.IncrementalMessages); err != nil {
		return err
	}
	if err := syncMemoryEntriesTx(ctx, tx, agentID, payload.MemoryEntries, payload.IncrementalMemoryEntries); err != nil {
		return err
	}
	if err := syncAgentTasksTx(ctx, tx, agentID, payload.Tasks); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store.SyncAgentFromPeer: commit: %w", err)
	}
	return nil
}

// syncAgentTasksTx replaces every agent_tasks row for agentID
// with the supplied source set. Same DELETE-then-INSERT pattern
// as syncMessagesTx / syncMemoryEntriesTx — source-wins, no
// per-row merge. Caller is the §3.7 device-switch orchestrator;
// runtime callers (BulkInsertAgentTasks, individual InsertTask)
// stay on the normal API path.
func syncAgentTasksTx(ctx context.Context, tx *sql.Tx, agentID string, recs []*AgentTaskRecord) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM agent_tasks WHERE agent_id = ?`, agentID); err != nil {
		return fmt.Errorf("syncAgentTasksTx: delete: %w", err)
	}
	if len(recs) == 0 {
		return nil
	}
	const q = `
INSERT INTO agent_tasks (
  id, agent_id, seq, title, body, status, due_at,
  version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`
	for i, t := range recs {
		if t == nil {
			return fmt.Errorf("syncAgentTasksTx: nil record at %d", i)
		}
		if t.ID == "" || t.AgentID == "" {
			return fmt.Errorf("syncAgentTasksTx: index %d: id/agent_id required", i)
		}
		if t.AgentID != agentID {
			return fmt.Errorf("syncAgentTasksTx: index %d: agent_id mismatch (%q vs %q)",
				i, t.AgentID, agentID)
		}
		if t.Title == "" {
			return fmt.Errorf("syncAgentTasksTx: index %d: title required", i)
		}
		if !validTaskStatuses[t.Status] {
			return fmt.Errorf("syncAgentTasksTx: index %d: invalid status %q", i, t.Status)
		}
		version := t.Version
		if version <= 0 {
			version = 1
		}
		seq := t.Seq
		if seq <= 0 {
			return fmt.Errorf("syncAgentTasksTx: index %d: seq must be > 0", i)
		}
		now := t.UpdatedAt
		if now <= 0 {
			now = NowMillis()
		}
		created := t.CreatedAt
		if created <= 0 {
			created = now
		}
		etag := t.ETag
		if etag == "" {
			copy := *t
			copy.Version = version
			copy.Seq = seq
			copy.UpdatedAt = now
			computed, cerr := computeAgentTaskETag(&copy)
			if cerr != nil {
				return fmt.Errorf("syncAgentTasksTx: index %d: etag: %w", i, cerr)
			}
			etag = computed
		}
		var deletedAt sql.NullInt64
		if t.DeletedAt != nil {
			deletedAt = sql.NullInt64{Int64: *t.DeletedAt, Valid: true}
		}
		var dueAt sql.NullInt64
		if t.DueAt != nil {
			dueAt = sql.NullInt64{Int64: *t.DueAt, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, q,
			t.ID, t.AgentID, seq, t.Title, nullableText(t.Body), t.Status, dueAt,
			version, etag, created, now, deletedAt, nullableText(t.PeerID),
		); err != nil {
			return fmt.Errorf("syncAgentTasksTx: index %d: %w", i, err)
		}
		if _, err := RecordEvent(ctx, tx, "agent_tasks", t.ID, etag, EventOpInsert, now); err != nil {
			return fmt.Errorf("syncAgentTasksTx: index %d: record event: %w", i, err)
		}
	}
	return nil
}

func syncAgentRowTx(ctx context.Context, tx *sql.Tx, rec *AgentRecord) error {
	settingsJSON, err := marshalSettings(rec.Settings)
	if err != nil {
		return fmt.Errorf("syncAgentRowTx: settings: %w", err)
	}
	personaRef := rec.PersonaRef
	if personaRef == "" {
		personaRef = rec.ID
	}
	workspaceID := rec.WorkspaceID
	if workspaceID == "" {
		workspaceID = rec.ID
	}
	version := rec.Version
	if version <= 0 {
		version = 1
	}
	seq := rec.Seq
	if seq <= 0 {
		seq = NextGlobalSeq()
	}
	created := rec.CreatedAt
	if created <= 0 {
		created = NowMillis()
	}
	updated := rec.UpdatedAt
	if updated <= 0 {
		updated = created
	}
	etag := rec.ETag
	if etag == "" {
		copy := *rec
		copy.Version = version
		copy.PersonaRef = personaRef
		copy.WorkspaceID = workspaceID
		copy.UpdatedAt = updated
		computed, cerr := computeAgentETag(&copy)
		if cerr != nil {
			return fmt.Errorf("syncAgentRowTx: etag: %w", cerr)
		}
		etag = computed
	}
	const q = `
INSERT INTO agents (
  id, name, persona_ref, settings_json, workspace_id,
  seq, version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  name          = excluded.name,
  persona_ref   = excluded.persona_ref,
  settings_json = excluded.settings_json,
  workspace_id  = excluded.workspace_id,
  seq           = excluded.seq,
  version       = excluded.version,
  etag          = excluded.etag,
  updated_at    = excluded.updated_at,
  deleted_at    = excluded.deleted_at,
  peer_id       = excluded.peer_id
`
	var deletedAt sql.NullInt64
	if rec.DeletedAt != nil {
		deletedAt = sql.NullInt64{Int64: *rec.DeletedAt, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, q,
		rec.ID, rec.Name, nullableText(personaRef), settingsJSON, nullableText(workspaceID),
		seq, version, etag, created, updated, deletedAt, nullableText(rec.PeerID),
	); err != nil {
		return fmt.Errorf("syncAgentRowTx: %w", err)
	}
	if _, err := RecordEvent(ctx, tx, "agents", rec.ID, etag, EventOpUpdate, updated); err != nil {
		return fmt.Errorf("syncAgentRowTx: record event: %w", err)
	}
	return nil
}

func syncAgentPersonaTx(ctx context.Context, tx *sql.Tx, agentID string, rec *AgentPersonaRecord) error {
	if rec == nil {
		// Source has no persona row → mirror by clearing.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM agent_persona WHERE agent_id = ?`, agentID); err != nil {
			return fmt.Errorf("syncAgentPersonaTx: delete: %w", err)
		}
		return nil
	}
	version := rec.Version
	if version <= 0 {
		version = 1
	}
	seq := rec.Seq
	if seq <= 0 {
		seq = NextGlobalSeq()
	}
	now := rec.UpdatedAt
	if now <= 0 {
		now = NowMillis()
	}
	created := rec.CreatedAt
	if created <= 0 {
		created = now
	}
	etag := rec.ETag
	if etag == "" {
		copy := *rec
		copy.Version = version
		copy.UpdatedAt = now
		computed, cerr := computeAgentPersonaETag(&copy)
		if cerr != nil {
			return fmt.Errorf("syncAgentPersonaTx: etag: %w", cerr)
		}
		etag = computed
	}
	const q = `
INSERT INTO agent_persona (
  agent_id, body, body_sha256,
  seq, version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(agent_id) DO UPDATE SET
  body         = excluded.body,
  body_sha256  = excluded.body_sha256,
  seq          = excluded.seq,
  version      = excluded.version,
  etag         = excluded.etag,
  updated_at   = excluded.updated_at,
  deleted_at   = excluded.deleted_at,
  peer_id      = excluded.peer_id
`
	var deletedAt sql.NullInt64
	if rec.DeletedAt != nil {
		deletedAt = sql.NullInt64{Int64: *rec.DeletedAt, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, q,
		agentID, rec.Body, rec.BodySHA256,
		seq, version, etag, created, now, deletedAt, nullableText(rec.PeerID),
	); err != nil {
		return fmt.Errorf("syncAgentPersonaTx: %w", err)
	}
	if _, err := RecordEvent(ctx, tx, "agent_persona", agentID, etag, EventOpUpdate, now); err != nil {
		return fmt.Errorf("syncAgentPersonaTx: record event: %w", err)
	}
	return nil
}

func syncAgentMemoryTx(ctx context.Context, tx *sql.Tx, agentID string, rec *AgentMemoryRecord) error {
	if rec == nil {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM agent_memory WHERE agent_id = ?`, agentID); err != nil {
			return fmt.Errorf("syncAgentMemoryTx: delete: %w", err)
		}
		return nil
	}
	version := rec.Version
	if version <= 0 {
		version = 1
	}
	seq := rec.Seq
	if seq <= 0 {
		seq = NextGlobalSeq()
	}
	now := rec.UpdatedAt
	if now <= 0 {
		now = NowMillis()
	}
	created := rec.CreatedAt
	if created <= 0 {
		created = now
	}
	etag := rec.ETag
	if etag == "" {
		copy := *rec
		copy.Version = version
		copy.UpdatedAt = now
		computed, cerr := computeAgentMemoryETag(&copy)
		if cerr != nil {
			return fmt.Errorf("syncAgentMemoryTx: etag: %w", cerr)
		}
		etag = computed
	}
	const q = `
INSERT INTO agent_memory (
  agent_id, body, body_sha256, last_tx_id,
  seq, version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(agent_id) DO UPDATE SET
  body         = excluded.body,
  body_sha256  = excluded.body_sha256,
  last_tx_id   = excluded.last_tx_id,
  seq          = excluded.seq,
  version      = excluded.version,
  etag         = excluded.etag,
  updated_at   = excluded.updated_at,
  deleted_at   = excluded.deleted_at,
  peer_id      = excluded.peer_id
`
	var deletedAt sql.NullInt64
	if rec.DeletedAt != nil {
		deletedAt = sql.NullInt64{Int64: *rec.DeletedAt, Valid: true}
	}
	var lastTx any
	if rec.LastTxID != nil && *rec.LastTxID != "" {
		lastTx = *rec.LastTxID
	} else {
		lastTx = nil
	}
	if _, err := tx.ExecContext(ctx, q,
		agentID, rec.Body, rec.BodySHA256, lastTx,
		seq, version, etag, created, now, deletedAt, nullableText(rec.PeerID),
	); err != nil {
		return fmt.Errorf("syncAgentMemoryTx: %w", err)
	}
	if _, err := RecordEvent(ctx, tx, "agent_memory", agentID, etag, EventOpUpdate, now); err != nil {
		return fmt.Errorf("syncAgentMemoryTx: record event: %w", err)
	}
	return nil
}

func syncMessagesTx(ctx context.Context, tx *sql.Tx, agentID string, recs []*MessageRecord, incremental bool) error {
	if !incremental {
		// Full-replace mode (non-§3.7 callers / first-time
		// device-switch / explicit reset path). DELETE first so
		// rows the source has tombstoned aren't left orphaned
		// on target.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM agent_messages WHERE agent_id = ?`, agentID); err != nil {
			return fmt.Errorf("syncMessagesTx: delete: %w", err)
		}
	}
	if len(recs) == 0 {
		return nil
	}
	// Incremental mode upserts by id — target's existing rows
	// (all with seq ≤ payload's SinceMessageSeq) are left
	// untouched, only the new delta lands. Full mode's INSERT
	// would also succeed against an empty table because we just
	// deleted, but using the same UPSERT shape here keeps the
	// SQL one statement and makes "incremental delete + reinsert
	// of a single torn message" idempotent under retry.
	const q = `
INSERT INTO agent_messages (
  id, agent_id, seq, role, content, thinking, tool_uses, attachments, usage,
  version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  agent_id    = excluded.agent_id,
  seq         = excluded.seq,
  role        = excluded.role,
  content     = excluded.content,
  thinking    = excluded.thinking,
  tool_uses   = excluded.tool_uses,
  attachments = excluded.attachments,
  usage       = excluded.usage,
  version     = excluded.version,
  etag        = excluded.etag,
  updated_at  = excluded.updated_at,
  deleted_at  = excluded.deleted_at,
  peer_id     = excluded.peer_id
`
	for i, m := range recs {
		if m == nil {
			return fmt.Errorf("syncMessagesTx: nil record at %d", i)
		}
		if m.ID == "" || m.AgentID == "" {
			return fmt.Errorf("syncMessagesTx: index %d: id/agent_id required", i)
		}
		if m.AgentID != agentID {
			return fmt.Errorf("syncMessagesTx: index %d: agent_id mismatch (%q vs %q)",
				i, m.AgentID, agentID)
		}
		if !validRoles[m.Role] {
			return fmt.Errorf("syncMessagesTx: index %d: invalid role %q", i, m.Role)
		}
		version := m.Version
		if version <= 0 {
			version = 1
		}
		seq := m.Seq
		if seq <= 0 {
			return fmt.Errorf("syncMessagesTx: index %d: seq must be > 0 (source rows have explicit seq)", i)
		}
		now := m.UpdatedAt
		if now <= 0 {
			now = NowMillis()
		}
		created := m.CreatedAt
		if created <= 0 {
			created = now
		}
		etag := m.ETag
		if etag == "" {
			copy := *m
			copy.Version = version
			copy.Seq = seq
			copy.UpdatedAt = now
			computed, cerr := computeMessageETag(&copy)
			if cerr != nil {
				return fmt.Errorf("syncMessagesTx: index %d: etag: %w", i, cerr)
			}
			etag = computed
		}
		var deletedAt sql.NullInt64
		if m.DeletedAt != nil {
			deletedAt = sql.NullInt64{Int64: *m.DeletedAt, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, q,
			m.ID, m.AgentID, seq, m.Role,
			nullableText(m.Content), nullableText(m.Thinking),
			nullableJSON(m.ToolUses), nullableJSON(m.Attachments), nullableJSON(m.Usage),
			version, etag, created, now, deletedAt, nullableText(m.PeerID),
		); err != nil {
			return fmt.Errorf("syncMessagesTx: index %d: %w", i, err)
		}
		if _, err := RecordEvent(ctx, tx, "agent_messages", m.ID, etag, EventOpInsert, now); err != nil {
			return fmt.Errorf("syncMessagesTx: index %d: record event: %w", i, err)
		}
	}
	return nil
}

func syncMemoryEntriesTx(ctx context.Context, tx *sql.Tx, agentID string, recs []*MemoryEntryRecord, incremental bool) error {
	if !incremental {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM memory_entries WHERE agent_id = ?`, agentID); err != nil {
			return fmt.Errorf("syncMemoryEntriesTx: delete: %w", err)
		}
	}
	if len(recs) == 0 {
		return nil
	}
	const q = `
INSERT INTO memory_entries (
  id, agent_id, seq, kind, name, body, body_sha256,
  version, etag, created_at, updated_at, deleted_at, peer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  agent_id    = excluded.agent_id,
  seq         = excluded.seq,
  kind        = excluded.kind,
  name        = excluded.name,
  body        = excluded.body,
  body_sha256 = excluded.body_sha256,
  version     = excluded.version,
  etag        = excluded.etag,
  updated_at  = excluded.updated_at,
  deleted_at  = excluded.deleted_at,
  peer_id     = excluded.peer_id
`
	for i, m := range recs {
		if m == nil {
			return fmt.Errorf("syncMemoryEntriesTx: nil record at %d", i)
		}
		if m.ID == "" || m.AgentID == "" {
			return fmt.Errorf("syncMemoryEntriesTx: index %d: id/agent_id required", i)
		}
		if m.AgentID != agentID {
			return fmt.Errorf("syncMemoryEntriesTx: index %d: agent_id mismatch (%q vs %q)",
				i, m.AgentID, agentID)
		}
		version := m.Version
		if version <= 0 {
			version = 1
		}
		seq := m.Seq
		if seq <= 0 {
			return fmt.Errorf("syncMemoryEntriesTx: index %d: seq must be > 0", i)
		}
		now := m.UpdatedAt
		if now <= 0 {
			now = NowMillis()
		}
		created := m.CreatedAt
		if created <= 0 {
			created = now
		}
		etag := m.ETag
		if etag == "" {
			copy := *m
			copy.Version = version
			copy.Seq = seq
			copy.UpdatedAt = now
			computed, cerr := computeMemoryEntryETag(&copy)
			if cerr != nil {
				return fmt.Errorf("syncMemoryEntriesTx: index %d: etag: %w", i, cerr)
			}
			etag = computed
		}
		var deletedAt sql.NullInt64
		if m.DeletedAt != nil {
			deletedAt = sql.NullInt64{Int64: *m.DeletedAt, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, q,
			m.ID, m.AgentID, seq, m.Kind, m.Name, m.Body, m.BodySHA256,
			version, etag, created, now, deletedAt, nullableText(m.PeerID),
		); err != nil {
			return fmt.Errorf("syncMemoryEntriesTx: index %d: %w", i, err)
		}
		if _, err := RecordEvent(ctx, tx, "memory_entries", m.ID, etag, EventOpInsert, now); err != nil {
			return fmt.Errorf("syncMemoryEntriesTx: index %d: record event: %w", i, err)
		}
	}
	return nil
}

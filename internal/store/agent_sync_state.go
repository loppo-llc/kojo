package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// AgentSyncState is the snapshot a target peer reports back to a
// device-switch orchestrator so the source can send only the delta
// the target doesn't already have. Returned by GetAgentSyncState
// and surfaced over POST /api/v1/peers/agent-sync/state.
//
// Known == false signals the target has never seen this agent —
// the orchestrator must do a full sync (every field below is 0/"").
//
// Max*Seq are the largest seq on each table for the agent; the
// orchestrator filters source rows with `seq > Max*Seq` and ships
// only those, halving payload size on every switch after the
// first. *ETag fields let the orchestrator skip retransmitting
// agents / agent_persona / agent_memory bodies that already
// match; the source still ships task rows in full because they're
// small and routinely mutated.
type AgentSyncState struct {
	Known bool `json:"known"`
	// MaxMessageSeq tracks max(agent_messages.seq) for the agent
	// on target. Used as the seq cursor for the §3.7 incremental
	// device-switch protocol — source ships rows with seq > this.
	// Append-only transcripts (no edits / no truncates) are the
	// common case and this cursor is sufficient; mixed-mode
	// transcripts trigger a full-replace downgrade on source via
	// HasNonAppendOnlyMessages.
	MaxMessageSeq int64 `json:"max_message_seq"`
	// MaxMemoryEntrySeq is reported for diagnostics but NOT used
	// as a delta cursor — memory_entries allow body updates +
	// soft-deletes + recreations on the same seq, so the
	// orchestrator keys off MaxMemoryEntryUpdatedAt instead.
	MaxMemoryEntrySeq int64 `json:"max_memory_entry_seq"`
	// MaxMemoryEntryUpdatedAt is the cursor the orchestrator
	// uses for memory_entries delta. Every InsertMemoryEntry /
	// UpdateMemoryEntry / SoftDeleteMemoryEntry bumps updated_at,
	// so source's `updated_at >= this` filter catches every
	// mutation target hasn't observed — including tombstones and
	// recreations. (`>=`, not `>`, defends against same-
	// millisecond mutations colliding on this cursor; every row
	// sharing the cursor timestamp — one or more — gets
	// idempotently re-shipped via the receiver's
	// ON CONFLICT(id) DO UPDATE.) The handler runs
	// IncrementalMemoryEntries when the wire
	// SinceMemoryEntryUpdatedAt > 0.
	//
	// Includes tombstoned rows (deleted_at != NULL) — their
	// updated_at IS the soft-delete instant, and the delta MUST
	// see them so target can mirror the deletion. A
	// COALESCE-style "live max only" would silently drop
	// tombstones whose updated_at exceeds the latest live row.
	MaxMemoryEntryUpdatedAt int64  `json:"max_memory_entry_updated_at"`
	AgentETag               string `json:"agent_etag,omitempty"`
	PersonaETag             string `json:"persona_etag,omitempty"`
	MemoryETag              string `json:"memory_etag,omitempty"`
}

// GetAgentSyncState reads the per-agent high-water marks the
// §3.7 incremental device-switch protocol needs.
//
// The function is read-only and safe to call from any peer's
// kojo.db. Returns Known=false (with zero everything else) when
// the agents row is missing or deleted — that's the "first-time
// sync" signal for the orchestrator.
//
// All four queries run in a single DB connection (no transaction
// — they are read-only and SQLite's snapshot isolation under
// journal_mode=WAL gives consistent reads across them without
// holding a write lock).
func (s *Store) GetAgentSyncState(ctx context.Context, agentID string) (*AgentSyncState, error) {
	if agentID == "" {
		return nil, errors.New("store.GetAgentSyncState: agent_id required")
	}
	out := &AgentSyncState{}

	// agents row + etag. Soft-deleted rows count as "not known" so
	// a recreated agent on source overwrites the tombstone cleanly.
	var agentETag sql.NullString
	var agentDeleted sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT etag, deleted_at FROM agents WHERE id = ?`, agentID).
		Scan(&agentETag, &agentDeleted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Known stays false; remaining fields default to zero.
		return out, nil
	case err != nil:
		return nil, fmt.Errorf("store.GetAgentSyncState: agents lookup: %w", err)
	}
	if agentDeleted.Valid {
		return out, nil
	}
	out.Known = true
	if agentETag.Valid {
		out.AgentETag = agentETag.String
	}

	// agent_persona / agent_memory etags. Missing rows leave
	// the corresponding ETag empty (orchestrator interprets as
	// "send if you have one").
	var personaETag sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT etag FROM agent_persona WHERE agent_id = ? AND deleted_at IS NULL`,
		agentID).Scan(&personaETag); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store.GetAgentSyncState: persona lookup: %w", err)
	}
	if personaETag.Valid {
		out.PersonaETag = personaETag.String
	}

	var memoryETag sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT etag FROM agent_memory WHERE agent_id = ? AND deleted_at IS NULL`,
		agentID).Scan(&memoryETag); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store.GetAgentSyncState: memory lookup: %w", err)
	}
	if memoryETag.Valid {
		out.MemoryETag = memoryETag.String
	}

	// Max seqs. COALESCE(MAX(seq),0) returns 0 cleanly when
	// the agent has no messages / memory_entries yet (e.g. a
	// freshly forked agent that synced its row but never spoke).
	// IncludeDeleted is intentional: a tombstoned message still
	// occupies its seq slot, and resurrecting it via an
	// incremental sync would create a seq gap on target.
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq),0) FROM agent_messages WHERE agent_id = ?`,
		agentID).Scan(&out.MaxMessageSeq); err != nil {
		return nil, fmt.Errorf("store.GetAgentSyncState: max message seq: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq),0) FROM memory_entries WHERE agent_id = ?`,
		agentID).Scan(&out.MaxMemoryEntrySeq); err != nil {
		return nil, fmt.Errorf("store.GetAgentSyncState: max memory entry seq: %w", err)
	}
	// max(updated_at) across LIVE AND TOMBSTONED rows so the
	// orchestrator's delta sees every state transition including
	// the soft-delete bump itself.
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(updated_at),0) FROM memory_entries WHERE agent_id = ?`,
		agentID).Scan(&out.MaxMemoryEntryUpdatedAt); err != nil {
		return nil, fmt.Errorf("store.GetAgentSyncState: max memory entry updated_at: %w", err)
	}
	return out, nil
}

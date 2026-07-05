package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// MaxHandoffQueuedPerAgent caps the hub-side queue-and-forward buffer
// per agent. Enforced inside the enqueue transaction so concurrent
// enqueues cannot overshoot. 100 is comfortably above any realistic
// "typed while the laptop was closed" burst while still bounding the
// DB row count and the reconnect drain burst on the holder.
const MaxHandoffQueuedPerAgent = 100

// ErrHandoffQueueFull is returned by EnqueueHandoffQueuedMessage when
// the per-agent cap is reached. The HTTP layer maps it to 429.
var ErrHandoffQueueFull = errors.New("store: handoff queue full for agent")

// HandoffQueuedMessage mirrors one row of handoff_queued_messages
// (migration 0023). HolderPeer is the holder at enqueue time —
// informational only; the drain re-resolves the current holder from
// agent_locks.
type HandoffQueuedMessage struct {
	ID         string
	AgentID    string
	HolderPeer string
	Content    string
	CreatedAt  int64
	Status     string
}

// NewHandoffQueuedMessageID mints a queue row id. Exported so the
// HTTP layer can pre-generate the id BEFORE the synchronous forward
// attempt: the same id (via its derived idempotency key) then covers
// both the initial forward and any later drain redelivery, so a
// "processed but connection dropped before the response" forward
// followed by enqueue cannot double-inject.
func NewHandoffQueuedMessageID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: handoff queue id: %w", err)
	}
	return "hq_" + hex.EncodeToString(b[:]), nil
}

// EnqueueHandoffQueuedMessage appends one message to the agent's
// queue, enforcing MaxHandoffQueuedPerAgent inside a transaction.
// Returns ErrHandoffQueueFull when the cap is reached.
func (s *Store) EnqueueHandoffQueuedMessage(ctx context.Context, agentID, holderPeer, content string) (*HandoffQueuedMessage, error) {
	return s.EnqueueHandoffQueuedMessageWithID(ctx, "", agentID, holderPeer, content)
}

// EnqueueHandoffQueuedMessageWithID is EnqueueHandoffQueuedMessage
// with a caller-supplied id (empty → minted here); see
// NewHandoffQueuedMessageID for why callers pre-generate.
func (s *Store) EnqueueHandoffQueuedMessageWithID(ctx context.Context, id, agentID, holderPeer, content string) (*HandoffQueuedMessage, error) {
	if agentID == "" {
		return nil, errors.New("store.EnqueueHandoffQueuedMessage: agent_id required")
	}
	if content == "" {
		return nil, errors.New("store.EnqueueHandoffQueuedMessage: content required")
	}
	if id == "" {
		var err error
		id, err = NewHandoffQueuedMessageID()
		if err != nil {
			return nil, err
		}
	}
	now := NowMillis()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store.EnqueueHandoffQueuedMessage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM handoff_queued_messages WHERE agent_id = ?`, agentID,
	).Scan(&n); err != nil {
		return nil, fmt.Errorf("store.EnqueueHandoffQueuedMessage: count: %w", err)
	}
	if n >= MaxHandoffQueuedPerAgent {
		return nil, ErrHandoffQueueFull
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO handoff_queued_messages (id, agent_id, holder_peer, content, created_at, status)
VALUES (?, ?, ?, ?, ?, 'queued')`,
		id, agentID, holderPeer, content, now,
	); err != nil {
		return nil, fmt.Errorf("store.EnqueueHandoffQueuedMessage: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store.EnqueueHandoffQueuedMessage: commit: %w", err)
	}
	return &HandoffQueuedMessage{
		ID: id, AgentID: agentID, HolderPeer: holderPeer,
		Content: content, CreatedAt: now, Status: "queued",
	}, nil
}

// ListHandoffQueuedMessages returns the agent's queued messages in
// enqueue order (created_at, then rowid for same-millisecond ties —
// rowid is monotonic per insert so ties keep arrival order).
func (s *Store) ListHandoffQueuedMessages(ctx context.Context, agentID string) ([]*HandoffQueuedMessage, error) {
	if agentID == "" {
		return nil, errors.New("store.ListHandoffQueuedMessages: agent_id required")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, agent_id, holder_peer, content, created_at, status
  FROM handoff_queued_messages
 WHERE agent_id = ?
 ORDER BY created_at ASC, rowid ASC`, agentID)
	if err != nil {
		return nil, fmt.Errorf("store.ListHandoffQueuedMessages: %w", err)
	}
	defer rows.Close()
	var out []*HandoffQueuedMessage
	for rows.Next() {
		var m HandoffQueuedMessage
		if err := rows.Scan(&m.ID, &m.AgentID, &m.HolderPeer, &m.Content, &m.CreatedAt, &m.Status); err != nil {
			return nil, fmt.Errorf("store.ListHandoffQueuedMessages: scan: %w", err)
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// ListHandoffQueuedAgentIDs returns the distinct agent_ids that have
// at least one queued message. The drain iterates this set and
// re-resolves each agent's current holder.
func (s *Store) ListHandoffQueuedAgentIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT agent_id FROM handoff_queued_messages ORDER BY agent_id`)
	if err != nil {
		return nil, fmt.Errorf("store.ListHandoffQueuedAgentIDs: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store.ListHandoffQueuedAgentIDs: scan: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CountHandoffQueuedMessages returns the queue depth for one agent.
func (s *Store) CountHandoffQueuedMessages(ctx context.Context, agentID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM handoff_queued_messages WHERE agent_id = ?`, agentID,
	).Scan(&n)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("store.CountHandoffQueuedMessages: %w", err)
	}
	return n, nil
}

// GetHandoffQueuedMessage returns one queued row or ErrNotFound.
// The drain re-checks row existence right before delivery so an
// owner cancel that landed after the drain's list snapshot wins.
func (s *Store) GetHandoffQueuedMessage(ctx context.Context, agentID, id string) (*HandoffQueuedMessage, error) {
	if agentID == "" || id == "" {
		return nil, errors.New("store.GetHandoffQueuedMessage: agent_id and id required")
	}
	var m HandoffQueuedMessage
	err := s.db.QueryRowContext(ctx, `
SELECT id, agent_id, holder_peer, content, created_at, status
  FROM handoff_queued_messages
 WHERE agent_id = ? AND id = ?`, agentID, id,
	).Scan(&m.ID, &m.AgentID, &m.HolderPeer, &m.Content, &m.CreatedAt, &m.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store.GetHandoffQueuedMessage: %w", err)
	}
	return &m, nil
}

// DeleteHandoffQueuedMessage removes one queued row (delivered or
// operator-cancelled). agentID is part of the key so an id guessed
// across agents cannot cancel another agent's message. Returns
// ErrNotFound when no row matches.
func (s *Store) DeleteHandoffQueuedMessage(ctx context.Context, agentID, id string) error {
	if agentID == "" || id == "" {
		return errors.New("store.DeleteHandoffQueuedMessage: agent_id and id required")
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM handoff_queued_messages WHERE agent_id = ? AND id = ?`, agentID, id)
	if err != nil {
		return fmt.Errorf("store.DeleteHandoffQueuedMessage: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.DeleteHandoffQueuedMessage: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

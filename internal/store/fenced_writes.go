package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// FencingPredicate makes a write atomic with the agent_lock holder
// check (docs/multi-device-storage.md §3.5 — "agent-runtime write
// requires lock holder peer + matching fencing token"). When passed
// through *InsertOptions to a write helper that supports it, the
// helper opens BEGIN IMMEDIATE, runs CheckFencingTx as the first
// statement, and gates the write on it; concurrent peers that
// stole the lock between the caller's read and the write see
// ErrFencingMismatch instead of a silently-committed stale write.
//
// Zero value (or nil pointer) skips the check — single-Hub callers
// that already hold their own lock guarantee can opt out.
type FencingPredicate struct {
	AgentID      string
	Peer         string
	FencingToken int64
}

// checkFencingPredicate runs CheckFencingTx if pred is non-nil. The
// predicate's AgentID must match the row being written (the caller
// is responsible for threading the right agent_id through). Returns
// the wrapped error so the caller can errors.Is against
// ErrFencingMismatch / ErrNotFound.
func checkFencingPredicate(ctx context.Context, s *Store, tx *sql.Tx, pred *FencingPredicate, expectedAgentID string) error {
	if pred == nil {
		return nil
	}
	if pred.AgentID == "" {
		return errors.New("store: fencing predicate agent_id required")
	}
	if pred.AgentID != expectedAgentID {
		return fmt.Errorf("store: fencing predicate agent_id %q does not match write target %q",
			pred.AgentID, expectedAgentID)
	}
	if pred.Peer == "" {
		return errors.New("store: fencing predicate peer required")
	}
	if pred.FencingToken <= 0 {
		return errors.New("store: fencing predicate token must be > 0")
	}
	return s.CheckFencingTx(ctx, tx, pred.AgentID, pred.Peer, pred.FencingToken)
}

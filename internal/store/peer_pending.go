package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PeerPendingRecord mirrors one row of `peer_pending` — a peer that
// has called Hub's POST /api/v1/peers/join-request but is still
// awaiting Owner approval. Approve moves the row into
// `peer_registry` (trusted=true) and drops the pending row. Reject
// drops it only (peer is free to re-request).
//
// first_seen / last_seen are unix millis.
type PeerPendingRecord struct {
	DeviceID  string
	Name      string
	URL       string
	FirstSeen int64
	LastSeen  int64
}

// UpsertPeerPending inserts a fresh pending row or overwrites the
// name/url/last_seen of an existing one (`first_seen` preserved on
// conflict). Single-statement INSERT ... ON CONFLICT so concurrent
// join-requests for the same device_id can't race the
// SELECT→INSERT window into a UNIQUE constraint error.
func (s *Store) UpsertPeerPending(ctx context.Context, rec *PeerPendingRecord) (*PeerPendingRecord, error) {
	if rec == nil {
		return nil, errors.New("store.UpsertPeerPending: nil record")
	}
	if rec.DeviceID == "" {
		return nil, errors.New("store.UpsertPeerPending: device_id required")
	}
	if rec.Name == "" {
		return nil, errors.New("store.UpsertPeerPending: name required")
	}
	if rec.URL == "" {
		return nil, errors.New("store.UpsertPeerPending: url required")
	}
	now := rec.LastSeen
	if now == 0 {
		now = NowMillis()
	}
	const q = `
INSERT INTO peer_pending (device_id, name, url, first_seen, last_seen)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(device_id) DO UPDATE SET
  name      = excluded.name,
  url       = excluded.url,
  last_seen = excluded.last_seen
RETURNING first_seen, last_seen`
	var firstSeen, lastSeen int64
	if err := s.db.QueryRowContext(ctx, q,
		rec.DeviceID, rec.Name, rec.URL, now, now,
	).Scan(&firstSeen, &lastSeen); err != nil {
		return nil, fmt.Errorf("store.UpsertPeerPending: %w", err)
	}
	return &PeerPendingRecord{
		DeviceID:  rec.DeviceID,
		Name:      rec.Name,
		URL:       rec.URL,
		FirstSeen: firstSeen,
		LastSeen:  lastSeen,
	}, nil
}

// GetPeerPending returns the row keyed by device_id or ErrNotFound.
func (s *Store) GetPeerPending(ctx context.Context, deviceID string) (*PeerPendingRecord, error) {
	const q = `SELECT device_id, name, url, first_seen, last_seen
                 FROM peer_pending WHERE device_id = ?`
	rec, err := scanPeerPendingRow(s.db.QueryRowContext(ctx, q, deviceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// ListPeerPending returns every pending row ordered by last_seen DESC
// then device_id ASC.
func (s *Store) ListPeerPending(ctx context.Context) ([]*PeerPendingRecord, error) {
	const q = `SELECT device_id, name, url, first_seen, last_seen
                 FROM peer_pending
                ORDER BY last_seen DESC, device_id ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store.ListPeerPending: %w", err)
	}
	defer rows.Close()
	var out []*PeerPendingRecord
	for rows.Next() {
		rec, err := scanPeerPendingRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// DeletePeerPending removes a pending row by device_id. Idempotent;
// returns nil even when no row matches (Reject must be a no-op when
// a concurrent Approve already promoted the row).
func (s *Store) DeletePeerPending(ctx context.Context, deviceID string) error {
	if deviceID == "" {
		return errors.New("store.DeletePeerPending: device_id required")
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM peer_pending WHERE device_id = ?`, deviceID,
	); err != nil {
		return fmt.Errorf("store.DeletePeerPending: %w", err)
	}
	return nil
}

// ApprovePeerPending promotes the pending row keyed by device_id into
// peer_registry (trusted=true) and removes the pending row in one
// transaction. Returns ErrNotFound when no pending row matches.
func (s *Store) ApprovePeerPending(ctx context.Context, deviceID string) (*PeerRecord, error) {
	if deviceID == "" {
		return nil, errors.New("store.ApprovePeerPending: device_id required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store.ApprovePeerPending: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	pending, err := scanPeerPendingRow(tx.QueryRowContext(ctx,
		`SELECT device_id, name, url, first_seen, last_seen
		   FROM peer_pending WHERE device_id = ?`, deviceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store.ApprovePeerPending: select pending: %w", err)
	}

	// Upsert into peer_registry. Insert when fresh, update name/url
	// on conflict. trusted = 1 always (Approve grants the privileged
	// surface — that's the whole point of the operator action).
	if _, err := tx.ExecContext(ctx, `
INSERT INTO peer_registry (device_id, name, url, last_seen, status, trusted)
VALUES (?, ?, ?, NULL, ?, 1)
ON CONFLICT(device_id) DO UPDATE SET
  name      = excluded.name,
  url       = excluded.url,
  trusted   = 1`,
		pending.DeviceID, pending.Name, pending.URL,
		PeerStatusOffline,
	); err != nil {
		return nil, fmt.Errorf("store.ApprovePeerPending: upsert registry: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM peer_pending WHERE device_id = ?`, deviceID,
	); err != nil {
		return nil, fmt.Errorf("store.ApprovePeerPending: delete pending: %w", err)
	}

	rec, err := scanPeerRow(tx.QueryRowContext(ctx, `
SELECT device_id, name, url,
       COALESCE(last_seen,0), status, trusted
  FROM peer_registry WHERE device_id = ?`, deviceID))
	if err != nil {
		return nil, fmt.Errorf("store.ApprovePeerPending: re-read: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store.ApprovePeerPending: commit: %w", err)
	}
	return rec, nil
}

func scanPeerPendingRow(r rowScanner) (*PeerPendingRecord, error) {
	var rec PeerPendingRecord
	if err := r.Scan(
		&rec.DeviceID, &rec.Name, &rec.URL,
		&rec.FirstSeen, &rec.LastSeen,
	); err != nil {
		return nil, err
	}
	return &rec, nil
}

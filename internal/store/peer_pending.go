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
// `peer_registry` and drops the pending row. Reject drops it only
// (peer is free to re-request).
//
// first_seen / last_seen are unix millis. NodeKey is the Tailscale
// stable NodeKey of the requesting peer, observed by the Hub via
// LocalClient.WhoIs on the inbound HTTP request — the peer never
// sends it. The handler refuses a repeat /join-request whose
// device_id binds to a different NodeKey than the one originally
// recorded (the operator must explicitly delete the stale row to
// rotate identity).
type PeerPendingRecord struct {
	DeviceID  string
	Name      string
	URL       string
	NodeKey   string
	FirstSeen int64
	LastSeen  int64
}

// UpsertPeerPending refreshes name/url/last_seen on an existing pending
// row. node_key is PRESERVED on conflict — only the original NodeKey
// observed at insert time stays authoritative. A repeat /join-request
// whose WhoIs-resolved NodeKey differs from the stored value should
// be rejected at the handler before reaching here.
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
INSERT INTO peer_pending (device_id, name, url, first_seen, last_seen, node_key)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(device_id) DO UPDATE SET
  name      = excluded.name,
  url       = excluded.url,
  last_seen = excluded.last_seen
  -- node_key PRESERVED on conflict: identity binding is set once
  -- at first insert.
RETURNING first_seen, last_seen, node_key`
	var firstSeen, lastSeen int64
	var nk string
	if err := s.db.QueryRowContext(ctx, q,
		rec.DeviceID, rec.Name, rec.URL, now, now, rec.NodeKey,
	).Scan(&firstSeen, &lastSeen, &nk); err != nil {
		return nil, fmt.Errorf("store.UpsertPeerPending: %w", err)
	}
	return &PeerPendingRecord{
		DeviceID:  rec.DeviceID,
		Name:      rec.Name,
		URL:       rec.URL,
		NodeKey:   nk,
		FirstSeen: firstSeen,
		LastSeen:  lastSeen,
	}, nil
}

// InsertPeerPendingIfAbsent tries to insert a fresh pending row.
// Returns (inserted=true, …) when the row landed; (inserted=false,
// …) when a row already exists for that device_id and the existing
// state is returned untouched (no metadata overwrite, no node_key
// overwrite).
//
// The handler is expected to use this on first contact. A concurrent
// race is serialised by SQLite at the row level; one caller wins as
// the inserter, the other receives inserted=false and is told to
// take the repeat path (which the handler gates on NodeKey
// equality before calling UpsertPeerPending).
func (s *Store) InsertPeerPendingIfAbsent(ctx context.Context, rec *PeerPendingRecord) (*PeerPendingRecord, bool, error) {
	if rec == nil {
		return nil, false, errors.New("store.InsertPeerPendingIfAbsent: nil record")
	}
	if rec.DeviceID == "" {
		return nil, false, errors.New("store.InsertPeerPendingIfAbsent: device_id required")
	}
	if rec.Name == "" {
		return nil, false, errors.New("store.InsertPeerPendingIfAbsent: name required")
	}
	if rec.URL == "" {
		return nil, false, errors.New("store.InsertPeerPendingIfAbsent: url required")
	}
	now := rec.LastSeen
	if now == 0 {
		now = NowMillis()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO peer_pending (device_id, name, url, first_seen, last_seen, node_key)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(device_id) DO NOTHING`,
		rec.DeviceID, rec.Name, rec.URL, now, now, rec.NodeKey,
	)
	if err != nil {
		return nil, false, fmt.Errorf("store.InsertPeerPendingIfAbsent: insert: %w", err)
	}
	n, _ := res.RowsAffected()
	out, err := s.GetPeerPending(ctx, rec.DeviceID)
	if err != nil {
		return nil, false, fmt.Errorf("store.InsertPeerPendingIfAbsent: read-back: %w", err)
	}
	return out, n > 0, nil
}

// GetPeerPending returns the row keyed by device_id or ErrNotFound.
func (s *Store) GetPeerPending(ctx context.Context, deviceID string) (*PeerPendingRecord, error) {
	const q = `SELECT device_id, name, url, first_seen, last_seen, node_key
                 FROM peer_pending WHERE device_id = ?`
	rec, err := scanPeerPendingRow(s.db.QueryRowContext(ctx, q, deviceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// GetPeerPendingByNodeKey returns the pending row whose node_key
// column matches `nodeKey`. Used by the join-request handler to detect
// a repeat POST from the same Tailscale node arriving against a
// different device_id (= NodeKey collision; handler rejects with
// 409 so the operator can clean up the old row).
func (s *Store) GetPeerPendingByNodeKey(ctx context.Context, nodeKey string) (*PeerPendingRecord, error) {
	if nodeKey == "" {
		return nil, ErrNotFound
	}
	const q = `SELECT device_id, name, url, first_seen, last_seen, node_key
                 FROM peer_pending WHERE node_key = ? LIMIT 1`
	rec, err := scanPeerPendingRow(s.db.QueryRowContext(ctx, q, nodeKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// ListPeerPending returns every pending row ordered by last_seen DESC
// then device_id ASC.
func (s *Store) ListPeerPending(ctx context.Context) ([]*PeerPendingRecord, error) {
	const q = `SELECT device_id, name, url, first_seen, last_seen, node_key
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
// peer_registry (carrying NodeKey across) and removes the pending row
// in one transaction. Returns ErrNotFound when no pending row matches.
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
		`SELECT device_id, name, url, first_seen, last_seen, node_key
		   FROM peer_pending WHERE device_id = ?`, deviceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store.ApprovePeerPending: select pending: %w", err)
	}

	// Upsert into peer_registry. Insert when fresh, refresh
	// name/url/node_key on conflict. NodeKey carries the
	// identity binding established at pending-insert time.
	if _, err := tx.ExecContext(ctx, `
INSERT INTO peer_registry (device_id, name, url, node_key, last_seen, status)
VALUES (?, ?, ?, ?, NULL, ?)
ON CONFLICT(device_id) DO UPDATE SET
  name     = excluded.name,
  url      = excluded.url,
  node_key = CASE WHEN excluded.node_key IS NULL OR excluded.node_key = ''
                  THEN peer_registry.node_key
                  ELSE excluded.node_key END`,
		pending.DeviceID, pending.Name, pending.URL,
		nullableText(pending.NodeKey),
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
SELECT device_id, name, url, COALESCE(node_key,''),
       COALESCE(last_seen,0), status
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
		&rec.FirstSeen, &rec.LastSeen, &rec.NodeKey,
	); err != nil {
		return nil, err
	}
	return &rec, nil
}

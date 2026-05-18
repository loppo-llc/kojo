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
// first_seen / last_seen are unix millis. JoinSecretHash is the
// sha256 of the per-join secret the Hub returned to the original
// requester on first contact; the raw secret never lands in the
// DB (Codex review hardening).
type PeerPendingRecord struct {
	DeviceID       string
	Name           string
	URL            string
	FirstSeen      int64
	LastSeen       int64
	JoinSecretHash string
}

// UpsertPeerPending is used only by AUTHENTICATED join-request
// repeats — the handler verifies the caller's join_secret BEFORE
// calling here, so refreshing name/url/last_seen is safe. The
// join_secret_hash column is PRESERVED on conflict; this method
// is NOT a path to claim a new device_id (use
// InsertPeerPendingIfAbsent for that).
//
// Codex review: the previous version exposed unauth name/url
// overwrites because both fresh-insert and post-conflict-update
// went through the same SQL. The split removes that footgun.
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
INSERT INTO peer_pending (device_id, name, url, first_seen, last_seen, join_secret_hash)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(device_id) DO UPDATE SET
  name      = excluded.name,
  url       = excluded.url,
  last_seen = excluded.last_seen
  -- join_secret_hash PRESERVED on conflict so the first-writer
  -- retains the binding; later callers can't overwrite it.
RETURNING first_seen, last_seen, join_secret_hash`
	var firstSeen, lastSeen int64
	var hash string
	if err := s.db.QueryRowContext(ctx, q,
		rec.DeviceID, rec.Name, rec.URL, now, now, rec.JoinSecretHash,
	).Scan(&firstSeen, &lastSeen, &hash); err != nil {
		return nil, fmt.Errorf("store.UpsertPeerPending: %w", err)
	}
	return &PeerPendingRecord{
		DeviceID:       rec.DeviceID,
		Name:           rec.Name,
		URL:            rec.URL,
		FirstSeen:      firstSeen,
		LastSeen:       lastSeen,
		JoinSecretHash: hash,
	}, nil
}

// InsertPeerPendingIfAbsent tries to insert a fresh pending row.
// Returns (inserted=true, …) when the row landed; (inserted=false,
// …) when a row already exists for that device_id and the existing
// state is returned untouched (no metadata overwrite, no
// join_secret_hash overwrite).
//
// This is the UNAUTHENTICATED entry point: the FIRST /join-request
// POST for a fresh device_id calls here. A concurrent first-time
// POST race is serialised by SQLite at the row level; one caller
// wins as the inserter, the other receives inserted=false and is
// rejected unless it can prove ownership via Authorization: Bearer
// against the stored join_secret_hash (Codex review hardening).
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
	// INSERT ... ON CONFLICT DO NOTHING returns 0 rows on conflict;
	// the second SELECT then surfaces whichever state landed. Both
	// statements run on the same connection so RETURNING wouldn't
	// help (DO NOTHING + RETURNING combinations are still touchy on
	// older SQLite; the explicit two-statement form is unambiguous).
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO peer_pending (device_id, name, url, first_seen, last_seen, join_secret_hash)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(device_id) DO NOTHING`,
		rec.DeviceID, rec.Name, rec.URL, now, now, rec.JoinSecretHash,
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
	const q = `SELECT device_id, name, url, first_seen, last_seen, join_secret_hash
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
	const q = `SELECT device_id, name, url, first_seen, last_seen, join_secret_hash
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
//
// The second return value is the pending row's join_secret_hash —
// the caller carries it into the pairing stash so the peer's
// join_secret continues to authenticate /join-request polls after
// the pending row is gone (Codex review hardening).
func (s *Store) ApprovePeerPending(ctx context.Context, deviceID string) (*PeerRecord, string, error) {
	if deviceID == "" {
		return nil, "", errors.New("store.ApprovePeerPending: device_id required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", fmt.Errorf("store.ApprovePeerPending: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	pending, err := scanPeerPendingRow(tx.QueryRowContext(ctx,
		`SELECT device_id, name, url, first_seen, last_seen, join_secret_hash
		   FROM peer_pending WHERE device_id = ?`, deviceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("store.ApprovePeerPending: select pending: %w", err)
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
		return nil, "", fmt.Errorf("store.ApprovePeerPending: upsert registry: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM peer_pending WHERE device_id = ?`, deviceID,
	); err != nil {
		return nil, "", fmt.Errorf("store.ApprovePeerPending: delete pending: %w", err)
	}

	rec, err := scanPeerRow(tx.QueryRowContext(ctx, `
SELECT device_id, name, url,
       COALESCE(last_seen,0), status, trusted
  FROM peer_registry WHERE device_id = ?`, deviceID))
	if err != nil {
		return nil, "", fmt.Errorf("store.ApprovePeerPending: re-read: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, "", fmt.Errorf("store.ApprovePeerPending: commit: %w", err)
	}
	return rec, pending.JoinSecretHash, nil
}

func scanPeerPendingRow(r rowScanner) (*PeerPendingRecord, error) {
	var rec PeerPendingRecord
	if err := r.Scan(
		&rec.DeviceID, &rec.Name, &rec.URL,
		&rec.FirstSeen, &rec.LastSeen, &rec.JoinSecretHash,
	); err != nil {
		return nil, err
	}
	return &rec, nil
}

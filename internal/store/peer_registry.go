package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PeerRecord mirrors one row of the `peer_registry` table. device_id is
// a stable GUID minted by the peer the first time it joins the cluster
// — public_key is its long-lived identity for inter-peer auth (separate
// from per-user Bearer tokens).
//
// `Status` is one of the schema's CHECK values: 'online' | 'offline' |
// 'degraded'. The Hub flips it to 'offline' after a heartbeat-miss
// threshold (3.7); 'degraded' is reserved for "reachable but with
// errors" cases (sha256 scrub failures, slow disk, etc.).
type PeerRecord struct {
	DeviceID     string
	Name         string
	PublicKey    string
	Capabilities string // raw JSON, empty = NULL
	LastSeen     int64  // unix millis, 0 = NULL
	Status       string
}

// PeerStatus values accepted by the schema's CHECK constraint.
const (
	PeerStatusOnline   = "online"
	PeerStatusOffline  = "offline"
	PeerStatusDegraded = "degraded"
)

// ErrPeerKeyUnchanged is returned by RotatePeerKey when the supplied
// new public_key matches the existing one. Callers (e.g. the HTTP
// handler) should `errors.Is` against this sentinel rather than
// pattern-matching on the message string.
var ErrPeerKeyUnchanged = errors.New("store: rotate-key: new public_key matches existing")

// validPeerStatus mirrors the CHECK constraint so callers can fail
// fast at the Go layer instead of getting a SQLITE_CONSTRAINT error
// from the driver. Adding a new value here requires updating the
// migration's CHECK clause to match.
func validPeerStatus(s string) bool {
	switch s {
	case PeerStatusOnline, PeerStatusOffline, PeerStatusDegraded:
		return true
	}
	return false
}

// UpsertPeer inserts or updates a peer_registry row keyed by device_id.
// Identity-stable columns (public_key) are preserved on conflict so a
// peer that re-registers cannot silently rotate its key without going
// through an explicit RotatePeerKey path (Phase 4 slice 2+). Mutable
// columns (name, capabilities, last_seen, status) overwrite.
//
// Empty Capabilities is stored as NULL (matches the schema's nullable
// column) so JSON parsers downstream can branch on the difference
// between "no caps reported" and "empty caps object".
func (s *Store) UpsertPeer(ctx context.Context, rec *PeerRecord) (*PeerRecord, error) {
	if rec == nil {
		return nil, errors.New("store.UpsertPeer: nil record")
	}
	if rec.DeviceID == "" {
		return nil, errors.New("store.UpsertPeer: device_id required")
	}
	if rec.Name == "" {
		return nil, errors.New("store.UpsertPeer: name required")
	}
	if rec.PublicKey == "" {
		return nil, errors.New("store.UpsertPeer: public_key required")
	}
	if rec.Status == "" {
		rec.Status = PeerStatusOnline
	}
	if !validPeerStatus(rec.Status) {
		return nil, fmt.Errorf("store.UpsertPeer: invalid status %q", rec.Status)
	}

	// On conflict only the *mutable* columns update — preserving the
	// public_key column blocks a silent identity-key rotation (a
	// hostile or buggy peer can re-register but cannot also swap its
	// long-lived key without a separate RotatePeerKey path that
	// audits the swap). Status / capabilities / last_seen / name are
	// expected to drift over time and overwrite cleanly.
	const q = `
INSERT INTO peer_registry (device_id, name, public_key, capabilities, last_seen, status)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(device_id) DO UPDATE SET
  name         = excluded.name,
  capabilities = excluded.capabilities,
  last_seen    = excluded.last_seen,
  status       = excluded.status
`
	if _, err := s.db.ExecContext(ctx, q,
		rec.DeviceID, rec.Name, rec.PublicKey,
		nullableText(rec.Capabilities),
		nullableInt64(rec.LastSeen),
		rec.Status,
	); err != nil {
		return nil, fmt.Errorf("store.UpsertPeer: %w", err)
	}
	return s.GetPeer(ctx, rec.DeviceID)
}

// RegisterPeerMetadata is the operator-driven peer registration path:
// it inserts a new row (last_seen=0, status='offline') or updates only
// the metadata columns (name, capabilities) of an existing row,
// preserving last_seen / status / public_key.
//
// This is the atomic alternative to GetPeer→UpsertPeer in the HTTP
// handler. The race that read-modify-write opens is concrete: the
// registrar's heartbeat loop ticks every few seconds and fires
// TouchPeer(status=online, last_seen=now); a UI rename that happened
// to land in that window would, with the read-then-upsert path, write
// the now-stale offline/0 row back over the heartbeat update. The
// single-statement INSERT ... ON CONFLICT below closes that window
// because SQLite serializes statements at the row level.
//
// public_key is preserved on conflict for the same reason as UpsertPeer
// — re-registration cannot rotate the long-lived identity key without
// going through an explicit RotatePeerKey path that audits the swap.
//
// Empty Capabilities is stored as NULL.
func (s *Store) RegisterPeerMetadata(ctx context.Context, rec *PeerRecord) (*PeerRecord, error) {
	if rec == nil {
		return nil, errors.New("store.RegisterPeerMetadata: nil record")
	}
	if rec.DeviceID == "" {
		return nil, errors.New("store.RegisterPeerMetadata: device_id required")
	}
	if rec.Name == "" {
		return nil, errors.New("store.RegisterPeerMetadata: name required")
	}
	if rec.PublicKey == "" {
		return nil, errors.New("store.RegisterPeerMetadata: public_key required")
	}
	const q = `
INSERT INTO peer_registry (device_id, name, public_key, capabilities, last_seen, status)
VALUES (?, ?, ?, ?, NULL, ?)
ON CONFLICT(device_id) DO UPDATE SET
  name         = excluded.name,
  capabilities = excluded.capabilities
  -- public_key, last_seen, status intentionally NOT touched on conflict
`
	if _, err := s.db.ExecContext(ctx, q,
		rec.DeviceID, rec.Name, rec.PublicKey,
		nullableText(rec.Capabilities),
		PeerStatusOffline,
	); err != nil {
		return nil, fmt.Errorf("store.RegisterPeerMetadata: %w", err)
	}
	return s.GetPeer(ctx, rec.DeviceID)
}

// RotatePeerKey swaps public_key for the row keyed by device_id and
// returns (oldKey, updatedRecord, nil). ErrNotFound is returned if no
// row matches.
//
// This is the explicit, audited path that the UpsertPeer /
// RegisterPeerMetadata contracts intentionally do NOT take: those
// preserve public_key on conflict so a hostile or buggy peer cannot
// silently rotate its long-lived identity by re-registering. Callers
// MUST be the operator (Owner principal at the HTTP layer) and MUST
// log the old → new fingerprint pair so the swap is reviewable.
//
// The transaction reads the current public_key under the same SQL
// statement that updates it so the returned `oldKey` matches the
// row that was actually overwritten — without that the caller would
// have to GetPeer first and the read-modify-write window could drop
// a concurrent rotation. SQLite serializes writes at the database
// level so the SELECT-then-UPDATE inside one tx is race-free against
// other writers.
//
// `last_seen` and `status` are intentionally NOT touched: an
// operator-driven rotation does not imply the peer is reachable
// right now, and the heartbeat loop is the authoritative writer for
// liveness columns.
func (s *Store) RotatePeerKey(ctx context.Context, deviceID, newPublicKey string) (string, *PeerRecord, error) {
	if deviceID == "" {
		return "", nil, errors.New("store.RotatePeerKey: device_id required")
	}
	if newPublicKey == "" {
		return "", nil, errors.New("store.RotatePeerKey: public_key required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, fmt.Errorf("store.RotatePeerKey: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var oldKey string
	if err := tx.QueryRowContext(ctx,
		`SELECT public_key FROM peer_registry WHERE device_id = ?`,
		deviceID,
	).Scan(&oldKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, ErrNotFound
		}
		return "", nil, fmt.Errorf("store.RotatePeerKey: select: %w", err)
	}
	if oldKey == newPublicKey {
		// No-op rotations are explicitly rejected — the caller
		// almost certainly meant a different key, and silently
		// pretending the swap happened would prevent useful audit
		// review of "I expected my new key to be different". The
		// sentinel error lets HTTP handlers use errors.Is rather
		// than pattern-matching on the message string.
		return "", nil, ErrPeerKeyUnchanged
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE peer_registry SET public_key = ? WHERE device_id = ?`,
		newPublicKey, deviceID,
	); err != nil {
		return "", nil, fmt.Errorf("store.RotatePeerKey: update: %w", err)
	}
	// Re-read inside the same transaction so the returned record
	// reflects the row exactly as we left it. A post-commit GetPeer
	// would expose a window where another rotate (or a registrar
	// heartbeat) could land first, so the response would describe a
	// row the caller never saw. Inside the tx the read is serialized
	// with the UPDATE we just issued, so the returned record is
	// guaranteed to carry our newPublicKey.
	rec, err := scanPeerRow(tx.QueryRowContext(ctx, `
SELECT device_id, name, public_key,
       COALESCE(capabilities,''), COALESCE(last_seen,0), status
  FROM peer_registry WHERE device_id = ?`, deviceID))
	if err != nil {
		return "", nil, fmt.Errorf("store.RotatePeerKey: re-read: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", nil, fmt.Errorf("store.RotatePeerKey: commit: %w", err)
	}
	return oldKey, rec, nil
}

// GetPeer returns the row keyed by device_id or ErrNotFound.
func (s *Store) GetPeer(ctx context.Context, deviceID string) (*PeerRecord, error) {
	const q = `
SELECT device_id, name, public_key,
       COALESCE(capabilities,''), COALESCE(last_seen,0), status
  FROM peer_registry WHERE device_id = ?`
	rec, err := scanPeerRow(s.db.QueryRowContext(ctx, q, deviceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// ListPeersOptions tunes ListPeers.
type ListPeersOptions struct {
	// Status filters to a single status value. Empty returns every row.
	Status string
	// Limit caps the row count. 0 = unlimited.
	Limit int
}

// ListPeers returns rows matching opts ordered by last_seen DESC then
// device_id ASC for deterministic output. last_seen=NULL sorts last so
// recently-active peers float to the top.
func (s *Store) ListPeers(ctx context.Context, opts ListPeersOptions) ([]*PeerRecord, error) {
	q := `
SELECT device_id, name, public_key,
       COALESCE(capabilities,''), COALESCE(last_seen,0), status
  FROM peer_registry
 WHERE 1=1`
	args := []any{}
	if opts.Status != "" {
		if !validPeerStatus(opts.Status) {
			return nil, fmt.Errorf("store.ListPeers: invalid status filter %q", opts.Status)
		}
		q += ` AND status = ?`
		args = append(args, opts.Status)
	}
	q += ` ORDER BY COALESCE(last_seen, 0) DESC, device_id ASC`
	if opts.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, opts.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PeerRecord
	for rows.Next() {
		rec, err := scanPeerRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// TouchPeer stamps last_seen = now and (optionally) updates status. If
// status is "" the existing status column is preserved. Used by the
// heartbeat handler so a noisy peer doesn't pay an UPSERT cycle just
// to update its last_seen.
//
// Returns ErrNotFound when device_id is not registered — callers
// (handler / Hub) must Upsert first; we don't auto-create rows here
// because that would let an unauthenticated peer materialize itself
// just by calling /heartbeat.
func (s *Store) TouchPeer(ctx context.Context, deviceID, status string, lastSeen int64) error {
	if deviceID == "" {
		return errors.New("store.TouchPeer: device_id required")
	}
	if status != "" && !validPeerStatus(status) {
		return fmt.Errorf("store.TouchPeer: invalid status %q", status)
	}
	if lastSeen == 0 {
		lastSeen = NowMillis()
	}
	var q string
	var args []any
	if status == "" {
		q = `UPDATE peer_registry SET last_seen = ? WHERE device_id = ?`
		args = []any{lastSeen, deviceID}
	} else {
		q = `UPDATE peer_registry SET last_seen = ?, status = ? WHERE device_id = ?`
		args = []any{lastSeen, status, deviceID}
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("store.TouchPeer: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.TouchPeer: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkStalePeersOffline flips every row whose `last_seen` is older
// than `before` (unix millis) AND whose `status` is currently
// 'online' to 'offline'. The optional `excludeDeviceID` is left
// untouched — pass the local peer's id so the sweeper can never
// race against its own registrar's heartbeat (the registrar is the
// authoritative writer for the self-row's liveness).
//
// Returns the number of rows flipped. The single UPDATE statement is
// race-safe against concurrent TouchPeer / RegisterPeerMetadata
// because SQLite serializes writers and the WHERE clause re-checks
// `status = 'online' AND last_seen < ?` at apply time — a heartbeat
// landing between the sweeper's intended-set scan and the UPDATE
// would refresh last_seen above `before`, fall outside the WHERE,
// and the row would correctly stay online.
//
// Rows already 'offline' or 'degraded' are not touched: 'offline'
// means a peer that has been gone long enough that we already gave
// up, and 'degraded' is reserved for "reachable with errors" cases
// where flipping straight to offline would lose useful state.
//
// `last_seen IS NULL` is treated as "never heartbeated" and *is*
// swept (NULL < `before` is false in SQL, so we OR-include it
// explicitly via COALESCE so a freshly-inserted row that the peer
// never actually checked in to gets cleared up by the next sweep).
func (s *Store) MarkStalePeersOffline(ctx context.Context, before int64, excludeDeviceID string) (int, error) {
	if before <= 0 {
		return 0, errors.New("store.MarkStalePeersOffline: before must be > 0")
	}
	const q = `
UPDATE peer_registry
   SET status = ?
 WHERE status = ?
   AND device_id != ?
   AND COALESCE(last_seen, 0) < ?`
	res, err := s.db.ExecContext(ctx, q,
		PeerStatusOffline,
		PeerStatusOnline,
		excludeDeviceID,
		before,
	)
	if err != nil {
		return 0, fmt.Errorf("store.MarkStalePeersOffline: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store.MarkStalePeersOffline: rows affected: %w", err)
	}
	return int(n), nil
}

// MarkStalePeersOfflineDetail flips stale online peers offline AND
// returns the device_ids it changed, so a caller (the cross-peer
// status push) can publish per-row events without an extra ListPeers
// round trip. SELECT-then-UPDATE inside a tx so a concurrent
// TouchPeer can't slip in between the candidate list and the
// commit.
//
// Same predicate as MarkStalePeersOffline; the splitting exists so
// callers that don't need the row list keep the cheaper count-only
// path.
func (s *Store) MarkStalePeersOfflineDetail(ctx context.Context, before int64, excludeDeviceID string) ([]string, error) {
	if before <= 0 {
		return nil, errors.New("store.MarkStalePeersOfflineDetail: before must be > 0")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store.MarkStalePeersOfflineDetail: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
SELECT device_id FROM peer_registry
 WHERE status = ?
   AND device_id != ?
   AND COALESCE(last_seen, 0) < ?`,
		PeerStatusOnline, excludeDeviceID, before,
	)
	if err != nil {
		return nil, fmt.Errorf("store.MarkStalePeersOfflineDetail: select: %w", err)
	}
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store.MarkStalePeersOfflineDetail: scan: %w", err)
		}
		stale = append(stale, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.MarkStalePeersOfflineDetail: rows: %w", err)
	}
	if len(stale) == 0 {
		return nil, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE peer_registry
   SET status = ?
 WHERE status = ?
   AND device_id != ?
   AND COALESCE(last_seen, 0) < ?`,
		PeerStatusOffline, PeerStatusOnline, excludeDeviceID, before,
	); err != nil {
		return nil, fmt.Errorf("store.MarkStalePeersOfflineDetail: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store.MarkStalePeersOfflineDetail: commit: %w", err)
	}
	return stale, nil
}

// DeletePeer removes the row keyed by device_id. Idempotent — a missing
// row returns nil. Callers driving a "decommission" flow should also
// audit any agent_locks rows whose holder_peer == deviceID; releasing
// those is a separate operation (see ReleaseAgentLockByPeer).
func (s *Store) DeletePeer(ctx context.Context, deviceID string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM peer_registry WHERE device_id = ?`,
		deviceID,
	); err != nil {
		return fmt.Errorf("store.DeletePeer: %w", err)
	}
	return nil
}

func scanPeerRow(r rowScanner) (*PeerRecord, error) {
	var rec PeerRecord
	if err := r.Scan(
		&rec.DeviceID, &rec.Name, &rec.PublicKey,
		&rec.Capabilities, &rec.LastSeen, &rec.Status,
	); err != nil {
		return nil, err
	}
	return &rec, nil
}

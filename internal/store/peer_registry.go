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
	DeviceID string
	// Name is the human-readable device label (OS hostname by default).
	// Operator-overridable from the UI; agents address peers by Name
	// rather than URL because the dial address (`<host>:<port>` or
	// `http://...`) is meaningless to a human and changes when the
	// network topology shifts.
	Name string
	// URL is the dial address other peers reach this row on. The
	// registrar stamps it from tsnet's FQDN (Hub) or the Tailscale
	// IPv4 (--peer). Empty until the daemon has been started at least
	// once so the listener bound its port.
	URL      string
	LastSeen int64 // unix millis, 0 = NULL
	Status   string
	// Trusted gates the privileged cross-peer surface: when set,
	// requests authenticated by this row's outbound Bearer are
	// admitted on /api/v1/sessions, /api/v1/ws, /api/v1/info,
	// /api/v1/dirs, /api/v1/files, /api/v1/git, /api/v1/upload.
	// Without it the bearer can only reach the minimal inter-peer
	// endpoints (peers/events, peers/blobs, peers/pull,
	// peers/agent-sync). Operator-controlled at pairing time (the
	// auto-pairing Approve flow flips it on; --peer-trust toggles
	// it after the fact).
	Trusted bool
}

// PeerStatus values accepted by the schema's CHECK constraint.
const (
	PeerStatusOnline   = "online"
	PeerStatusOffline  = "offline"
	PeerStatusDegraded = "degraded"
)

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
	if rec.Status == "" {
		rec.Status = PeerStatusOnline
	}
	if !validPeerStatus(rec.Status) {
		return nil, fmt.Errorf("store.UpsertPeer: invalid status %q", rec.Status)
	}

	// On conflict only the *mutable* columns update. `trusted` is
	// operator-controlled and preserved on conflict so a heartbeat
	// never silently flips trust. Status / last_seen / name / url
	// are expected to drift over time and overwrite cleanly.
	const q = `
INSERT INTO peer_registry (device_id, name, url, last_seen, status, trusted)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(device_id) DO UPDATE SET
  name      = excluded.name,
  url       = excluded.url,
  last_seen = excluded.last_seen,
  status    = excluded.status
`
	if _, err := s.db.ExecContext(ctx, q,
		rec.DeviceID, rec.Name, rec.URL,
		nullableInt64(rec.LastSeen),
		rec.Status, boolToInt(rec.Trusted),
	); err != nil {
		return nil, fmt.Errorf("store.UpsertPeer: %w", err)
	}
	return s.GetPeer(ctx, rec.DeviceID)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
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
	// On conflict, treat empty url as "no change" instead of
	// blanking the existing column. A startup-time register from a
	// peer that hasn't bound its listener yet would otherwise wipe
	// the Hub's known URL for that peer and break every subsequent
	// Hub→peer dial. The same applies to name — the operator-
	// supplied label on the Hub is more authoritative than the
	// bare hostname a fresh peer emits. `trusted` is operator-
	// controlled and preserved on conflict.
	const q = `
INSERT INTO peer_registry (device_id, name, url, last_seen, status, trusted)
VALUES (?, ?, ?, NULL, ?, ?)
ON CONFLICT(device_id) DO UPDATE SET
  name = CASE WHEN excluded.name = '' THEN peer_registry.name ELSE excluded.name END,
  url  = CASE WHEN excluded.url  = '' THEN peer_registry.url  ELSE excluded.url  END
  -- last_seen, status, trusted intentionally NOT touched on conflict
`
	if _, err := s.db.ExecContext(ctx, q,
		rec.DeviceID, rec.Name, rec.URL,
		PeerStatusOffline, boolToInt(rec.Trusted),
	); err != nil {
		return nil, fmt.Errorf("store.RegisterPeerMetadata: %w", err)
	}
	return s.GetPeer(ctx, rec.DeviceID)
}

// UpdatePeerMetadata mutates the human-editable name + url of an
// existing peer row without touching public_key, trusted,
// capabilities, last_seen, or status. Operator-driven path for
// the GUI's inline edit form.
//
// Why capabilities is out: the UI never edits it, so sending the
// current value alongside an edit would race a concurrent peer-
// reported capabilities refresh and silently roll it back. The
// other preserved columns are owned by separate code paths
// (identity rotation, trust flip, heartbeat) and likewise must
// not be overwritten by a metadata edit. `name` and `url` empty
// values are rejected because the operator-facing UI has no
// legitimate path to wipe them.
//
// Returns ErrNotFound when no row matches the device_id.
func (s *Store) UpdatePeerMetadata(ctx context.Context, deviceID, name, url string) error {
	if deviceID == "" {
		return errors.New("store.UpdatePeerMetadata: device_id required")
	}
	if name == "" {
		return errors.New("store.UpdatePeerMetadata: name required")
	}
	if url == "" {
		return errors.New("store.UpdatePeerMetadata: url required")
	}
	const q = `
UPDATE peer_registry
   SET name = ?, url = ?
 WHERE device_id = ?`
	res, err := s.db.ExecContext(ctx, q, name, url, deviceID)
	if err != nil {
		return fmt.Errorf("store.UpdatePeerMetadata: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.UpdatePeerMetadata: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdatePeerTrust flips the trusted bit on a paired peer row.
// Operator-driven only; the cross-peer register-push path
// preserves the existing trust state (any cluster member with a
// signing key would otherwise self-promote to trusted). Returns
// ErrNotFound when no row matches the device_id.
func (s *Store) UpdatePeerTrust(ctx context.Context, deviceID string, trusted bool) error {
	if deviceID == "" {
		return errors.New("store.UpdatePeerTrust: device_id required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE peer_registry SET trusted = ? WHERE device_id = ?`,
		boolToInt(trusted), deviceID,
	)
	if err != nil {
		return fmt.Errorf("store.UpdatePeerTrust: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.UpdatePeerTrust: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}


// GetPeer returns the row keyed by device_id or ErrNotFound.
func (s *Store) GetPeer(ctx context.Context, deviceID string) (*PeerRecord, error) {
	const q = `
SELECT device_id, name, url,
       COALESCE(last_seen,0), status, trusted
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
SELECT device_id, name, url,
       COALESCE(last_seen,0), status, trusted
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
	var trustedInt int
	if err := r.Scan(
		&rec.DeviceID, &rec.Name, &rec.URL,
		&rec.LastSeen, &rec.Status, &trustedInt,
	); err != nil {
		return nil, err
	}
	rec.Trusted = trustedInt != 0
	return &rec, nil
}

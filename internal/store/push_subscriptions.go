package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PushSubscriptionRecord mirrors the `push_subscriptions` table.
//
// §3.3 exception (documented in 0001_initial.sql line 367-378): this table
// does NOT carry version/etag/seq/deleted_at/peer_id. The endpoint URL is
// the entire identity (the user agent produces it; nothing kojo writes
// would optimistic-lock against), liveness is signalled by ExpiredAt
// (set on 401/410 from the push provider — a tombstone-without-soft-
// delete), and audit-by-device is supplanted by DeviceID copied straight
// from peer_registry. A row therefore has no etag to recompute on import,
// no seq to allocate against the global counter, and no peer_id to stamp.
//
// DeviceID and UserAgent are nullable: v0's push_subscriptions.json never
// stored either column (webpush.Subscription only carries endpoint+keys),
// so every imported row leaves both NULL. Live runtime that subscribes a
// new browser may populate them once the future API surfaces them.
type PushSubscriptionRecord struct {
	Endpoint       string
	DeviceID       *string // nullable; v0 never set it
	UserAgent      *string // nullable; v0 never set it
	VAPIDPublicKey string  // copied from vapid.json at import time
	P256dh         string
	Auth           string
	ExpiredAt      *int64 // tombstone marker; nil = active
	CreatedAt      int64
	UpdatedAt      int64
}

// PushSubscriptionInsertOptions narrows the bulk-insert path. There is
// intentionally no PeerID field: the schema has no peer_id column for
// this table (see §3.3 exception above). Now / CreatedAt / UpdatedAt
// follow the same precedence pattern as the other importers — caller
// record value wins, then opts, then the clock.
type PushSubscriptionInsertOptions struct {
	Now       int64
	CreatedAt int64
	UpdatedAt int64
}

// BulkInsertPushSubscriptions inserts many push_subscriptions rows in a
// single transaction. Used by the v0→v1 importer; live subscribe/
// unsubscribe goes through dedicated single-row APIs (not yet exposed,
// matching slice 8a's notify_cursors posture: poller cutover comes
// later).
//
// Idempotency contract matches BulkInsertSessions / BulkInsertNotify-
// Cursors: rows whose endpoint already exists are skipped via ON
// CONFLICT DO NOTHING + a preload-set so a re-run leaves the existing
// row untouched. Caller records are mutated in place AFTER commit with
// assigned timestamps; skipped rows are left untouched so callers can
// distinguish "imported now" (CreatedAt populated) from "already there"
// (CreatedAt zero) without re-querying.
//
// DeviceID / UserAgent are normalized: a non-nil pointer to "" is
// rewritten to nil on the staged copy so the persisted NULL round-trips
// to a nil pointer on read-back. There is no etag to keep symmetric (no
// etag column), but consistent NULL handling avoids surprising callers
// that compare records pre- and post-insert.
func (s *Store) BulkInsertPushSubscriptions(ctx context.Context, recs []*PushSubscriptionRecord, opts PushSubscriptionInsertOptions) (int, error) {
	if len(recs) == 0 {
		return 0, nil
	}

	now := opts.Now
	if now == 0 {
		now = NowMillis()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// Up-front validation. Surface here so a pathological record at
	// index N doesn't leave N-1 partial inserts on rollback. A row with
	// a missing keys.auth or keys.p256dh is unsendable (webpush-go
	// errors out at encryption time), and a missing vapid_public_key
	// makes the row unrecoverable on the next VAPID rotation — refuse
	// rather than persist it.
	for i, r := range recs {
		if r == nil {
			return 0, fmt.Errorf("store.BulkInsertPushSubscriptions: nil record at index %d", i)
		}
		if r.Endpoint == "" {
			return 0, fmt.Errorf("store.BulkInsertPushSubscriptions: index %d: endpoint required", i)
		}
		if r.VAPIDPublicKey == "" {
			return 0, fmt.Errorf("store.BulkInsertPushSubscriptions: index %d (endpoint=%s): vapid_public_key required", i, r.Endpoint)
		}
		if r.P256dh == "" {
			return 0, fmt.Errorf("store.BulkInsertPushSubscriptions: index %d (endpoint=%s): p256dh required", i, r.Endpoint)
		}
		if r.Auth == "" {
			return 0, fmt.Errorf("store.BulkInsertPushSubscriptions: index %d (endpoint=%s): auth required", i, r.Endpoint)
		}
	}

	existing, err := preloadExistingPushSubscriptionEndpoints(ctx, tx, recs)
	if err != nil {
		return 0, fmt.Errorf("store.BulkInsertPushSubscriptions: preload existing: %w", err)
	}

	const q = `
INSERT INTO push_subscriptions (
  endpoint, device_id, user_agent, vapid_public_key,
  p256dh, auth, expired_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(endpoint) DO NOTHING`
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	type stagedSub struct {
		idx int
		rec PushSubscriptionRecord
	}
	staged := make([]stagedSub, 0, len(recs))

	inserted := 0
	for i, r := range recs {
		if existing[r.Endpoint] {
			continue
		}

		created := r.CreatedAt
		if created == 0 {
			created = opts.CreatedAt
		}
		if created == 0 {
			created = now
		}
		updated := r.UpdatedAt
		if updated == 0 {
			updated = opts.UpdatedAt
		}
		if updated == 0 {
			updated = created
		}

		out := *r
		// Normalize *"" → nil on the staged copy so the persisted NULL
		// round-trips to nil on read-back. Keeps callers from being
		// surprised that recs[i].DeviceID came back as nil rather than
		// the &"" they passed.
		if out.DeviceID != nil && *out.DeviceID == "" {
			out.DeviceID = nil
		}
		if out.UserAgent != nil && *out.UserAgent == "" {
			out.UserAgent = nil
		}
		out.CreatedAt = created
		out.UpdatedAt = updated

		res, err := stmt.ExecContext(ctx,
			out.Endpoint, nullableTextPtr(out.DeviceID), nullableTextPtr(out.UserAgent),
			out.VAPIDPublicKey, out.P256dh, out.Auth,
			nullableInt64Ptr(out.ExpiredAt), out.CreatedAt, out.UpdatedAt,
		)
		if err != nil {
			return 0, fmt.Errorf("store.BulkInsertPushSubscriptions: index %d (endpoint=%s): %w", i, r.Endpoint, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		if n > 0 {
			inserted++
			staged = append(staged, stagedSub{idx: i, rec: out})
		}
		// n == 0 here means an in-batch duplicate endpoint: first wins
		// via ON CONFLICT DO NOTHING. SQLite holds the write lock from
		// our first INSERT through commit so concurrent writers can't
		// interleave.
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	for _, s := range staged {
		*recs[s.idx] = s.rec
	}
	return inserted, nil
}

// preloadExistingPushSubscriptionEndpoints returns the set of endpoints
// already in push_subscriptions for the given batch. Chunked to stay
// under SQLite's default 999-variable limit.
func preloadExistingPushSubscriptionEndpoints(ctx context.Context, tx *sql.Tx, recs []*PushSubscriptionRecord) (map[string]bool, error) {
	if len(recs) == 0 {
		return nil, nil
	}
	const chunkSize = 500
	out := make(map[string]bool, len(recs))
	for start := 0; start < len(recs); start += chunkSize {
		end := start + chunkSize
		if end > len(recs) {
			end = len(recs)
		}
		eps := make([]any, 0, end-start)
		placeholders := make([]byte, 0, (end-start)*2)
		for i := start; i < end; i++ {
			if i > start {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
			eps = append(eps, recs[i].Endpoint)
		}
		q := `SELECT endpoint FROM push_subscriptions WHERE endpoint IN (` + string(placeholders) + `)`
		rows, err := tx.QueryContext(ctx, q, eps...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var ep string
			if err := rows.Scan(&ep); err != nil {
				rows.Close()
				return nil, err
			}
			out[ep] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// GetPushSubscription returns the row by endpoint. ErrNotFound on miss.
func (s *Store) GetPushSubscription(ctx context.Context, endpoint string) (*PushSubscriptionRecord, error) {
	const q = `
SELECT endpoint, device_id, user_agent, vapid_public_key,
       p256dh, auth, expired_at, created_at, updated_at
  FROM push_subscriptions
 WHERE endpoint = ?`
	rec, err := scanPushSubscriptionRow(s.db.QueryRowContext(ctx, q, endpoint))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// ListActivePushSubscriptions returns every row whose expired_at IS NULL
// ordered by (created_at, endpoint) ASC. Expired rows are kept in the
// table as a debug breadcrumb but excluded here so the live notify
// path doesn't waste a webpush.Send round-trip on a known-dead
// endpoint.
//
// Tiebreak by endpoint: the v0→v1 importer stamps every row in a
// single batch with the same fileMTimeMillis(), so created_at alone
// would let SQLite return them in implementation-defined order — tests
// asserting an ordered slice would flake under `go test -count=N` and
// audit-log diffing across hosts would surface phantom changes.
func (s *Store) ListActivePushSubscriptions(ctx context.Context) ([]*PushSubscriptionRecord, error) {
	const q = `
SELECT endpoint, device_id, user_agent, vapid_public_key,
       p256dh, auth, expired_at, created_at, updated_at
  FROM push_subscriptions
 WHERE expired_at IS NULL
 ORDER BY created_at ASC, endpoint ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PushSubscriptionRecord
	for rows.Next() {
		rec, err := scanPushSubscriptionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func scanPushSubscriptionRow(r rowScanner) (*PushSubscriptionRecord, error) {
	var (
		rec       PushSubscriptionRecord
		deviceID  sql.NullString
		userAgent sql.NullString
		expiredAt sql.NullInt64
	)
	if err := r.Scan(
		&rec.Endpoint, &deviceID, &userAgent, &rec.VAPIDPublicKey,
		&rec.P256dh, &rec.Auth, &expiredAt, &rec.CreatedAt, &rec.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if deviceID.Valid {
		v := deviceID.String
		rec.DeviceID = &v
	}
	if userAgent.Valid {
		v := userAgent.String
		rec.UserAgent = &v
	}
	if expiredAt.Valid {
		v := expiredAt.Int64
		rec.ExpiredAt = &v
	}
	return &rec, nil
}

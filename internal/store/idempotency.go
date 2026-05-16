package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// IdempotencyEntry mirrors one row of `idempotency_keys`. The table
// is the 24-hour dedup window for write-API retries (3.5) and op-log
// replay (3.13.1): a client that retries the same write under the
// same Idempotency-Key gets back the prior response instead of
// risking a double-execute, and an op-log replay during Hub failover
// (3.6 / 3.13.1) uses the same gate to suppress duplicate apply.
//
// `RequestHash` captures method + canonical path + body sha256 so a
// client that reuses an Idempotency-Key for a *different* request is
// caught (409 Conflict) rather than served a stale cached response.
//
// `ResponseStatus == 0` is a sentinel for "claim row inserted, handler
// is still running". A second concurrent request with the same key
// must NOT execute the handler again — it returns 409 ("in flight").
// When the handler finishes the row is rewritten with the real
// status / etag / body.
type IdempotencyEntry struct {
	Key            string
	OpID           string
	RequestHash    string
	ResponseStatus int
	ResponseEtag   string
	ResponseBody   string
	ExpiresAt      int64
}

// ErrIdempotencyConflict is returned when the supplied key exists
// but the request_hash does not match — i.e. the caller reused the
// same Idempotency-Key for a *different* request. Per RFC standards
// for idempotency the correct response is 409 Conflict; surfacing a
// distinct sentinel lets the HTTP layer differentiate this from a
// generic "key exists" case.
var ErrIdempotencyConflict = errors.New("store: idempotency-key reused for a different request")

// ErrIdempotencyInFlight is returned when the supplied key matches an
// existing row whose ResponseStatus is still 0 — i.e. another worker
// is currently executing this request. Callers should respond 409
// rather than re-execute (which would defeat the dedup).
var ErrIdempotencyInFlight = errors.New("store: idempotency-key request still in flight")

// ClaimIdempotencyKey atomically reserves the key for the caller. It
// inserts a "pending" row (response_status=0, response_etag/body NULL)
// when no live row exists for `key`, and returns (nil, nil) — the
// caller should run the handler and then call FinalizeIdempotencyKey.
//
// When a live row is already present:
//   - request_hash mismatch → ErrIdempotencyConflict
//   - request_hash match, response_status == 0 → ErrIdempotencyInFlight
//   - request_hash match, response_status != 0 → returns the saved
//     entry; the caller short-circuits the handler and replays the
//     stored response verbatim.
//
// "Live" means expires_at > now (we evaluate it inside the SQL so the
// behaviour matches the index-driven sweep). Expired rows are
// transparently overwritten so a delayed retry beyond the dedup
// window correctly re-executes (per RFC the only client guarantee is
// "within the dedup window").
//
// The atomicity rests on a single statement (INSERT … ON CONFLICT DO
// UPDATE WHERE expires_at <= now), which SQLite serializes at the
// database level — two concurrent claim attempts cannot both win.
func (s *Store) ClaimIdempotencyKey(ctx context.Context, key, opID, requestHash string, expiresAt int64) (*IdempotencyEntry, error) {
	if key == "" {
		return nil, errors.New("store.ClaimIdempotencyKey: key required")
	}
	if opID == "" {
		// op_id is repurposed here as the claim token: a unique
		// per-attempt nonce that lets Finalize / Abandon scope to the
		// exact row this caller wrote, even if a sweep + a fresh
		// claim re-used the key in between.
		return nil, errors.New("store.ClaimIdempotencyKey: op_id (claim token) required")
	}
	if requestHash == "" {
		return nil, errors.New("store.ClaimIdempotencyKey: request_hash required")
	}
	if expiresAt <= 0 {
		return nil, errors.New("store.ClaimIdempotencyKey: expires_at must be > 0")
	}

	// Retry the upsert+select cycle a few times. The race we need to
	// handle: between our upsert (n==0, blocked by live row) and our
	// select, a sweeper or abandon can delete the blocking row,
	// leaving us with no row to read. Treating that as fresh-claim is
	// unsafe (Finalize would write to nothing) — so we loop and let
	// the next iteration's upsert succeed.
	const maxClaimAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxClaimAttempts; attempt++ {
		entry, err := s.tryClaimOnce(ctx, key, opID, requestHash, expiresAt)
		if err == errClaimRetry {
			lastErr = err
			continue
		}
		return entry, err
	}
	return nil, fmt.Errorf("store.ClaimIdempotencyKey: exhausted %d attempts: %w", maxClaimAttempts, lastErr)
}

// errClaimRetry is the internal sentinel for "the upsert was blocked
// (n==0) but the blocking row vanished before we could read it".
// tryClaimOnce returns it; ClaimIdempotencyKey re-runs the cycle.
var errClaimRetry = errors.New("store: idempotency claim race; retry")

func (s *Store) tryClaimOnce(ctx context.Context, key, opID, requestHash string, expiresAt int64) (*IdempotencyEntry, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store.ClaimIdempotencyKey: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := NowMillis()

	// INSERT … ON CONFLICT DO UPDATE — only the expired-row branch
	// rewrites; live rows are left alone and the rows-affected count
	// tells us which path fired:
	//
	//   - n == 1: we just claimed (either fresh insert or overwrote
	//     an expired row). Return nil so the caller runs the handler.
	//   - n == 0: an existing LIVE row blocked us. Read it to decide
	//     between conflict / in-flight / saved-replay.
	//
	// Using DO UPDATE rather than DO NOTHING because we *want* to
	// overwrite expired rows transparently — without that the unique
	// key would block re-use across the dedup window even when the
	// stored row is no longer authoritative.
	const upsertQ = `
INSERT INTO idempotency_keys (key, op_id, request_hash, response_status, expires_at)
VALUES (?, ?, ?, 0, ?)
ON CONFLICT(key) DO UPDATE SET
  op_id           = excluded.op_id,
  request_hash    = excluded.request_hash,
  response_status = 0,
  response_etag   = NULL,
  response_body   = NULL,
  expires_at      = excluded.expires_at
WHERE idempotency_keys.expires_at <= ?`
	res, err := tx.ExecContext(ctx, upsertQ,
		key, nullableText(opID), requestHash, expiresAt, now,
	)
	if err != nil {
		return nil, fmt.Errorf("store.ClaimIdempotencyKey: upsert: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("store.ClaimIdempotencyKey: rows affected: %w", err)
	}
	if n > 0 {
		// Claim winner. Commit the new pending row and return.
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("store.ClaimIdempotencyKey: commit: %w", err)
		}
		return nil, nil
	}

	// n == 0: a live row blocked our upsert. Read it inside the same
	// tx so the row we describe is exactly the one that blocked us.
	const selectQ = `
SELECT key, COALESCE(op_id,''), request_hash,
       response_status, COALESCE(response_etag,''), COALESCE(response_body,''),
       expires_at
  FROM idempotency_keys
 WHERE key = ?`
	var rec IdempotencyEntry
	if err := tx.QueryRowContext(ctx, selectQ, key).Scan(
		&rec.Key, &rec.OpID, &rec.RequestHash,
		&rec.ResponseStatus, &rec.ResponseEtag, &rec.ResponseBody,
		&rec.ExpiresAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Race window: the blocking row was deleted between
			// upsert and select (sweep ran, abandon ran). Signal a
			// retry rather than treating as fresh-claim — the latter
			// would leave the caller with no claim row to finalize.
			return nil, errClaimRetry
		}
		return nil, fmt.Errorf("store.ClaimIdempotencyKey: select: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store.ClaimIdempotencyKey: commit: %w", err)
	}

	if rec.RequestHash != requestHash {
		return nil, ErrIdempotencyConflict
	}
	if rec.ResponseStatus == 0 {
		return nil, ErrIdempotencyInFlight
	}
	// Saved response — caller replays.
	return &rec, nil
}

// FinalizeIdempotencyKey rewrites the pending row with the handler's
// response. Called once the wrapped handler has produced its full
// status/body. The expires_at is left intact (set at claim time).
//
// `claimToken` MUST be the value the caller passed as `opID` to
// ClaimIdempotencyKey — the WHERE clause scopes the UPDATE to the
// exact claim row. Without that scope, an old handler whose claim
// expired and was overwritten by a fresh claim could finalize over
// the new claim's pending row, replaying its response under the new
// requester's expectations.
//
// Returns ErrNotFound when the row is missing or no longer matches
// the claim token — surfaces an actionable failure rather than a
// silent no-op.
func (s *Store) FinalizeIdempotencyKey(ctx context.Context, key, claimToken string, status int, etag, body string) error {
	if key == "" {
		return errors.New("store.FinalizeIdempotencyKey: key required")
	}
	if claimToken == "" {
		return errors.New("store.FinalizeIdempotencyKey: claim token required")
	}
	if status <= 0 {
		return errors.New("store.FinalizeIdempotencyKey: status must be > 0")
	}
	const q = `
UPDATE idempotency_keys
   SET response_status = ?, response_etag = ?, response_body = ?
 WHERE key = ? AND op_id = ? AND response_status = 0`
	res, err := s.db.ExecContext(ctx, q,
		status, nullableText(etag), nullableText(body),
		key, claimToken,
	)
	if err != nil {
		return fmt.Errorf("store.FinalizeIdempotencyKey: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.FinalizeIdempotencyKey: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AbandonIdempotencyKey deletes the claim row when the handler
// panicked / context-cancelled before producing a saveable response.
// Scoped to (key, claim_token) so a stale handler can't drop a fresh
// claim's pending row. Idempotent — a missing or already-completed
// row returns nil.
func (s *Store) AbandonIdempotencyKey(ctx context.Context, key, claimToken string) error {
	if key == "" {
		return errors.New("store.AbandonIdempotencyKey: key required")
	}
	if claimToken == "" {
		return errors.New("store.AbandonIdempotencyKey: claim token required")
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM idempotency_keys WHERE key = ? AND op_id = ? AND response_status = 0`,
		key, claimToken,
	); err != nil {
		return fmt.Errorf("store.AbandonIdempotencyKey: %w", err)
	}
	return nil
}

// ExpireIdempotencyKeys deletes rows whose expires_at <= cutoff.
// Returns the count deleted. Run periodically (1× / hour is fine —
// the dedup window is 24 h) so the table doesn't grow unbounded.
func (s *Store) ExpireIdempotencyKeys(ctx context.Context, cutoff int64) (int, error) {
	if cutoff <= 0 {
		return 0, errors.New("store.ExpireIdempotencyKeys: cutoff must be > 0")
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM idempotency_keys WHERE expires_at <= ?`,
		cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("store.ExpireIdempotencyKeys: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store.ExpireIdempotencyKeys: rows affected: %w", err)
	}
	return int(n), nil
}

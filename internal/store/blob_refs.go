package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// BlobRefRecord mirrors one row of the `blob_refs` table. The blob URI
// is `kojo://<scope>/<path>` and is the primary key. Bodies live on
// the filesystem under internal/blob; this row is the canonical
// metadata cache so Head / IfMatch / List avoid reading the full body
// to recompute sha256.
//
// Refcount, PinPolicy, LastSeenOK, MarkedForGCAt and HandoffPending
// are reserved for later phases (pin / dedup, scrub repair, device
// handoff). Phase 3 slice 2 keeps refcount at 1 for every row and
// leaves the rest at their default zero values; slice 3+ will use
// them. They are surfaced on the record so the schema and the API
// type stay aligned across slices.
type BlobRefRecord struct {
	URI            string
	Scope          string
	HomePeer       string
	Size           int64
	SHA256         string // hex
	Refcount       int64
	PinPolicy      string // raw JSON, empty = NULL
	LastSeenOK     int64  // unix millis, 0 = NULL
	MarkedForGCAt  int64  // unix millis, 0 = NULL
	HandoffPending bool
	CreatedAt      int64
	UpdatedAt      int64

	// ExpectedCurrentSHA256 / ExpectedCurrentUpdatedAt are
	// consumed ONLY by RestoreBlobRef to enforce optimistic-
	// concurrency: the restore refuses if either disagrees with
	// the row's current state. (sha256, updated_at) together
	// catch every meaningful concurrent mutation — sha256 moves
	// on body replace, updated_at moves on every write including
	// handoff_pending flips and scrub timestamp bumps that keep
	// sha256 stable. Both fields must be set to enable the OCC
	// gate; either empty disables it.
	ExpectedCurrentSHA256    string
	ExpectedCurrentUpdatedAt int64
}

// BlobRefInsertOptions tunes timestamps for InsertOrReplaceBlobRef.
// Mirrors the pattern used by other Insert* helpers so v0→v1 importers
// can preserve original CreatedAt values.
type BlobRefInsertOptions struct {
	CreatedAt int64
	UpdatedAt int64
	// AllowHandoffPending lets the §3.7 target-side pull
	// (peer.PullClient via blob.Store.Put with
	// BypassHandoffPending=true) update a row whose existing
	// handoff_pending flag would otherwise refuse the write.
	// Reserved for the orchestrator's pull path; agent-runtime
	// writes MUST leave this false so the §3.7 invariant holds.
	//
	// v1 trust model: the orchestrator (Hub) is the only
	// authority that runs begin/complete/abort, so concurrent
	// handoffs on the same URI can't happen and an unconditional
	// UPDATE is safe. A future multi-orchestrator slice would
	// need to thread an expected-pre-state token through the
	// bypass to refuse "I expected source X but the row says Y".
	AllowHandoffPending bool
}

// handoffPendingWriteSQL builds the INSERT ... ON CONFLICT statement
// for InsertOrReplaceBlobRef. The handoff_pending guard's WHERE
// clause is the only piece that varies across the two callsites:
//   - allowPending=false (default): refuse mutating writes when the
//     existing row is mid-handoff.
//   - allowPending=true (orchestrator pull): admit the write
//     unconditionally; the §3.7 invariants are owned by the
//     caller, not the row state.
//
// Keeping the two queries side-by-side in one helper means a future
// schema change can't drift them apart.
func handoffPendingWriteSQL(allowPending bool) string {
	const base = `
INSERT INTO blob_refs (
  uri, scope, home_peer, size, sha256, refcount,
  pin_policy, last_seen_ok, marked_for_gc_at, handoff_pending,
  created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(uri) DO UPDATE SET
  scope            = excluded.scope,
  home_peer        = excluded.home_peer,
  size             = excluded.size,
  sha256           = excluded.sha256,
  updated_at       = excluded.updated_at,
  marked_for_gc_at = NULL,
  -- last_seen_ok is bound to a specific sha256 (§3.15-bis). When
  -- a Put replaces the body with a NEW digest, clear the stamp so
  -- the next scrub re-verifies the fresh body. When the digest is
  -- unchanged (idempotent re-Put), keep the prior timestamp so the
  -- scrub-skip heuristic isn't reset.
  last_seen_ok = CASE
    WHEN blob_refs.sha256 = excluded.sha256 THEN blob_refs.last_seen_ok
    ELSE NULL
  END
`
	// RETURNING gives us the post-write row in the same
	// statement as the upsert, so the StoreRefs caller can
	// observe updated_at without a follow-up GetBlobRef that
	// could race a concurrent writer's timestamp. SQLite 3.35+
	// supports RETURNING; modernc.org/sqlite (the driver we
	// link) bundles a recent enough version.
	const returning = `
RETURNING uri, scope, home_peer, size, sha256, refcount,
          COALESCE(pin_policy,''), COALESCE(last_seen_ok,0), COALESCE(marked_for_gc_at,0),
          handoff_pending, created_at, updated_at
`
	if allowPending {
		return base + returning
	}
	return base + `
WHERE blob_refs.handoff_pending = 0
   OR (blob_refs.sha256    = excluded.sha256
       AND blob_refs.home_peer = excluded.home_peer
       AND blob_refs.scope     = excluded.scope
       AND blob_refs.size      = excluded.size)
` + returning
}

// ErrHandoffPending is returned by InsertOrReplaceBlobRef when the
// existing row's handoff_pending flag is set. A device-switch
// (§3.7) has marked this URI as mid-handoff, and accepting a new
// body would silently overwrite the digest the orchestrator
// committed the target peer to pull. Callers above the store
// layer (blob.Store.Put → agent-runtime writes) should surface
// this as 409 to whatever client tried the write; the operator
// can finish or abort the handoff and retry.
var ErrHandoffPending = errors.New("store: blob_refs row is mid-handoff (handoff_pending=1)")

// InsertOrReplaceBlobRef writes rec into blob_refs. On URI conflict
// the existing row is overwritten in place (Put-style semantics —
// blob bodies are replace-on-write at the URI level; refcount stays
// at the table default until pin / dedup phases use it). The
// CreatedAt of the existing row is preserved so audit history holds.
//
// Returns ErrHandoffPending when the existing row has
// handoff_pending=1. The §3.7 device-switch state machine
// requires both source and target to refuse agent-runtime writes
// against rows whose flag is set; this guard is the canonical
// enforcement point. SwitchBlobRefHome / SetBlobRefHandoffPending
// bypass it (they are the state-machine drivers themselves) and
// edit the row through dedicated targeted UPDATEs.
func (s *Store) InsertOrReplaceBlobRef(ctx context.Context, rec *BlobRefRecord, opts BlobRefInsertOptions) (*BlobRefRecord, error) {
	if rec == nil {
		return nil, errors.New("store.InsertOrReplaceBlobRef: nil record")
	}
	if rec.URI == "" {
		return nil, errors.New("store.InsertOrReplaceBlobRef: uri required")
	}
	if rec.Scope == "" {
		return nil, errors.New("store.InsertOrReplaceBlobRef: scope required")
	}
	if rec.SHA256 == "" {
		return nil, errors.New("store.InsertOrReplaceBlobRef: sha256 required")
	}
	if rec.HomePeer == "" {
		return nil, errors.New("store.InsertOrReplaceBlobRef: home_peer required")
	}

	now := opts.UpdatedAt
	if now == 0 {
		now = NowMillis()
	}
	created := opts.CreatedAt
	if created == 0 {
		created = now
	}

	// Use INSERT ... ON CONFLICT DO UPDATE so a Put on an existing URI
	// refreshes the body-derived columns (scope, home_peer, size,
	// sha256, updated_at) and clears any prior GC mark, while leaving
	// caller-managed state (refcount, pin_policy, last_seen_ok,
	// handoff_pending) and audit history (created_at) untouched. A
	// blob layer that always re-Puts with empty management fields
	// would otherwise silently wipe a pin policy or a device-handoff
	// flag set by another subsystem.
	// The ON CONFLICT WHERE clause refuses to overwrite a row
	// whose handoff_pending flag is set when the new write would
	// actually change body-derived columns — a runtime write
	// during the §3.7 window would otherwise mutate those out
	// from under the orchestrator. Idempotent re-Puts (every
	// body-derived column matches) are allowed because they're
	// how the scrubber + idempotency-key retries + migration
	// importers verify a body without changing it. The opt-in
	// AllowHandoffPending fork is reserved for the orchestrator's
	// pull path, where the orchestrator IS the authority that
	// owns the pending state.
	q := handoffPendingWriteSQL(opts.AllowHandoffPending)
	refcount := rec.Refcount
	if refcount == 0 {
		refcount = 1
	}
	handoff := 0
	if rec.HandoffPending {
		handoff = 1
	}
	row := s.db.QueryRowContext(ctx, q,
		rec.URI, rec.Scope, rec.HomePeer, rec.Size, rec.SHA256, refcount,
		nullableText(rec.PinPolicy), nullableInt64(rec.LastSeenOK), nullableInt64(rec.MarkedForGCAt), handoff,
		created, now,
	)
	out, err := scanBlobRefRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		// RETURNING produced no row. With AllowHandoffPending=true
		// the WHERE is always true (the clause is omitted), so
		// a zero-row outcome here would indicate a driver-level
		// anomaly rather than the handoff guard — fall through
		// to a generic error in that branch.
		if !opts.AllowHandoffPending {
			// Conflict + DO UPDATE WHERE false: the existing
			// row had handoff_pending=1 AND the proposed
			// write would change body-derived columns. The
			// idempotent path is admitted by the WHERE itself.
			return nil, ErrHandoffPending
		}
		return nil, fmt.Errorf("store.InsertOrReplaceBlobRef: RETURNING produced no row")
	}
	if err != nil {
		return nil, fmt.Errorf("store.InsertOrReplaceBlobRef: %w", err)
	}
	return out, nil
}

// ErrRestoreSuperseded is returned by RestoreBlobRef when the
// optimistic-concurrency check (rec.ExpectedCurrentSHA256) does
// NOT match the row's current sha256. A concurrent abort /
// complete / scrub repaired or advanced the row while the blob
// layer was inside its commit window; restoring our stale
// snapshot would undo their work, so the helper refuses. The
// blob layer treats this as "leave the row alone" and the next
// scrub pass picks up any residual inconsistency.
var ErrRestoreSuperseded = errors.New("store: RestoreBlobRef: row was modified concurrently; leaving current state")

// RestoreBlobRef writes rec back to blob_refs verbatim — every
// column (including managed fields refcount, pin_policy,
// last_seen_ok, marked_for_gc_at, handoff_pending, created_at,
// updated_at) is set from the record. Used by the blob layer to
// roll back a failed Put after the prior row was snapshotted via
// StoreRefs.Snapshot — InsertOrReplaceBlobRef's ON-CONFLICT path
// would silently re-derive last_seen_ok / clear marked_for_gc_at,
// which is wrong on a restore.
//
// Optimistic concurrency: when rec.ExpectedCurrentSHA256 is
// non-empty, the UPDATE branch only fires if the row's current
// sha256 matches that value. A mismatch (or absent row when
// ExpectedCurrentSHA256 is set to the snapshot's digest) returns
// ErrRestoreSuperseded so a concurrent state-machine driver
// isn't clobbered. ExpectedCurrentSHA256=="" disables the check
// — used by importers that rebuild the row from scratch.
//
// If no row exists for rec.URI and ExpectedCurrentSHA256 is "",
// RestoreBlobRef inserts one instead of failing — keeps the
// importer path idempotent.
func (s *Store) RestoreBlobRef(ctx context.Context, rec *BlobRefRecord) error {
	if rec == nil {
		return errors.New("store.RestoreBlobRef: nil record")
	}
	if rec.URI == "" {
		return errors.New("store.RestoreBlobRef: uri required")
	}
	now := rec.UpdatedAt
	if now == 0 {
		now = NowMillis()
	}
	created := rec.CreatedAt
	if created == 0 {
		created = now
	}
	handoff := 0
	if rec.HandoffPending {
		handoff = 1
	}
	refcount := rec.Refcount
	if refcount == 0 {
		refcount = 1
	}
	if rec.ExpectedCurrentSHA256 != "" && rec.ExpectedCurrentUpdatedAt != 0 {
		// Optimistic-concurrency restore: pure UPDATE gated on
		// (sha256, updated_at) — both must still match what
		// the caller saw before its failed Put. Without
		// updated_at a concurrent same-digest mutation (scrub
		// last_seen_ok bump, handoff_pending flip, GC mark)
		// would slip through the OCC and our restore would
		// clobber it.
		const qOpt = `
UPDATE blob_refs
   SET scope            = ?,
       home_peer        = ?,
       size             = ?,
       sha256           = ?,
       refcount         = ?,
       pin_policy       = ?,
       last_seen_ok     = ?,
       marked_for_gc_at = ?,
       handoff_pending  = ?,
       updated_at       = ?
 WHERE uri = ? AND sha256 = ? AND updated_at = ?
`
		res, err := s.db.ExecContext(ctx, qOpt,
			rec.Scope, rec.HomePeer, rec.Size, rec.SHA256, refcount,
			nullableText(rec.PinPolicy), nullableInt64(rec.LastSeenOK), nullableInt64(rec.MarkedForGCAt), handoff,
			now,
			rec.URI, rec.ExpectedCurrentSHA256, rec.ExpectedCurrentUpdatedAt,
		)
		if err != nil {
			return fmt.Errorf("store.RestoreBlobRef: %w", err)
		}
		if n, raErr := res.RowsAffected(); raErr == nil && n == 0 {
			return ErrRestoreSuperseded
		}
		return nil
	}
	const q = `
INSERT INTO blob_refs (
  uri, scope, home_peer, size, sha256, refcount,
  pin_policy, last_seen_ok, marked_for_gc_at, handoff_pending,
  created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(uri) DO UPDATE SET
  scope            = excluded.scope,
  home_peer        = excluded.home_peer,
  size             = excluded.size,
  sha256           = excluded.sha256,
  refcount         = excluded.refcount,
  pin_policy       = excluded.pin_policy,
  last_seen_ok     = excluded.last_seen_ok,
  marked_for_gc_at = excluded.marked_for_gc_at,
  handoff_pending  = excluded.handoff_pending,
  updated_at       = excluded.updated_at
`
	if _, err := s.db.ExecContext(ctx, q,
		rec.URI, rec.Scope, rec.HomePeer, rec.Size, rec.SHA256, refcount,
		nullableText(rec.PinPolicy), nullableInt64(rec.LastSeenOK), nullableInt64(rec.MarkedForGCAt), handoff,
		created, now,
	); err != nil {
		return fmt.Errorf("store.RestoreBlobRef: %w", err)
	}
	return nil
}

// GetBlobRef returns the row keyed by uri or ErrNotFound.
func (s *Store) GetBlobRef(ctx context.Context, uri string) (*BlobRefRecord, error) {
	const q = `
SELECT uri, scope, home_peer, size, sha256, refcount,
       COALESCE(pin_policy,''), COALESCE(last_seen_ok,0), COALESCE(marked_for_gc_at,0),
       handoff_pending, created_at, updated_at
  FROM blob_refs WHERE uri = ?`
	rec, err := scanBlobRefRow(s.db.QueryRowContext(ctx, q, uri))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

// DeleteBlobRef removes the row keyed by uri. Returns nil even if the
// row was already absent so callers can drive idempotent cleanup loops
// without sniffing ErrNotFound. The on-disk body is the caller's
// responsibility (internal/blob.Delete handles both halves under one
// pathLock).
func (s *Store) DeleteBlobRef(ctx context.Context, uri string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM blob_refs WHERE uri = ?`, uri); err != nil {
		return fmt.Errorf("store.DeleteBlobRef: %w", err)
	}
	return nil
}

// DeleteBlobRefIfMatches removes the row keyed by uri only when its
// current (sha256, updated_at) match the supplied tuple. blob.Store
// uses this on the rollback path when its prior Snapshot saw NO row:
// a 3rd-party writer that recreated the URI between Snapshot and
// rollback would otherwise be silently deleted. Returns
// (deleted bool, err error): deleted=false with err=nil means the
// row state moved on (or the row was already absent) and the caller
// should leave it alone.
func (s *Store) DeleteBlobRefIfMatches(ctx context.Context, uri, sha256 string, updatedAt int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM blob_refs WHERE uri = ? AND sha256 = ? AND updated_at = ?`,
		uri, sha256, updatedAt)
	if err != nil {
		return false, fmt.Errorf("store.DeleteBlobRefIfMatches: %w", err)
	}
	n, raErr := res.RowsAffected()
	if raErr != nil {
		// RowsAffected unreliable: be conservative and report
		// "did not delete" rather than risk a phantom success.
		return false, nil
	}
	return n > 0, nil
}

// ListBlobRefsOptions tunes ListBlobRefs.
type ListBlobRefsOptions struct {
	// Scope filters to one scope; "" returns every scope.
	Scope string
	// URIPrefix filters to URIs that HasPrefix(uri, URIPrefix). The
	// caller is expected to build the full `kojo://<scope>/<path>`
	// form; this helper does no scope-prefixing of its own so a
	// caller can list across scopes when Scope == "".
	URIPrefix string
	// IncludeMarkedForGC: false hides rows whose marked_for_gc_at is
	// non-null. Slice 2 always passes false; the scrub job (slice 3+)
	// passes true to enumerate sweep candidates.
	IncludeMarkedForGC bool
	// Limit caps the row count. 0 = unlimited.
	Limit int
}

// ListBlobRefs returns rows matching opts, ordered by uri ASC for
// deterministic output. The query uses the (scope, uri) prefix on the
// PK so single-scope listings stay index-friendly.
func (s *Store) ListBlobRefs(ctx context.Context, opts ListBlobRefsOptions) ([]*BlobRefRecord, error) {
	q := `
SELECT uri, scope, home_peer, size, sha256, refcount,
       COALESCE(pin_policy,''), COALESCE(last_seen_ok,0), COALESCE(marked_for_gc_at,0),
       handoff_pending, created_at, updated_at
  FROM blob_refs
 WHERE 1=1`
	args := []any{}
	if opts.Scope != "" {
		q += ` AND scope = ?`
		args = append(args, opts.Scope)
	}
	if opts.URIPrefix != "" {
		// Range scan on the PK rather than LIKE: SQLite's default
		// LIKE is ASCII case-insensitive, which would over-match a
		// case-sensitive blob URI prefix (e.g. `/Avatars/` would also
		// hit `/avatars/`). The half-open `[prefix, nextPrefix(prefix))`
		// pair is index-friendly and inherently case-sensitive because
		// text comparison defaults to BINARY. When the prefix is
		// already at the maximum byte sequence (every byte is 0xFF —
		// not possible for a "kojo://" URI but handled for
		// correctness), no finite upper bound exists and we drop the
		// upper-bound clause so every "successor" row still matches.
		q += ` AND uri >= ?`
		args = append(args, opts.URIPrefix)
		if upper, ok := nextPrefix(opts.URIPrefix); ok {
			q += ` AND uri < ?`
			args = append(args, upper)
		}
	}
	if !opts.IncludeMarkedForGC {
		q += ` AND marked_for_gc_at IS NULL`
	}
	q += ` ORDER BY uri ASC`
	if opts.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, opts.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*BlobRefRecord
	for rows.Next() {
		rec, err := scanBlobRefRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// TouchBlobRefLastSeenOK stamps last_seen_ok on the row. Called by
// the scrub job (§3.15-bis) after a successful sha256 re-hash
// verifies the on-disk body matches the canonical hash recorded in
// the row. The timestamp lets the scrub job skip rows it has
// recently checked and gives operators a way to see when a blob
// was last known-good.
//
// `expectedSHA256` scopes the UPDATE to the row whose sha256 is
// still the value the scrubber hashed against: a concurrent Put
// that landed between the scrubber's Open and this Touch would
// have advanced the row's sha256, and stamping last_seen_ok on a
// row whose body we did NOT actually verify would lie to operators
// + later scrubs. Returns ErrNotFound when no row matches (skipped
// in that case; the next pass will re-hash the fresh body).
//
// `ts` is taken from the caller so tests can supply a deterministic
// clock; production callers pass NowMillis().
func (s *Store) TouchBlobRefLastSeenOK(ctx context.Context, uri, expectedSHA256 string, ts int64) error {
	if uri == "" {
		return errors.New("store.TouchBlobRefLastSeenOK: uri required")
	}
	if expectedSHA256 == "" {
		return errors.New("store.TouchBlobRefLastSeenOK: expected_sha256 required")
	}
	if ts == 0 {
		ts = NowMillis()
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE blob_refs SET last_seen_ok = ?, updated_at = ? WHERE uri = ? AND sha256 = ?`,
		ts, ts, uri, expectedSHA256,
	)
	if err != nil {
		return fmt.Errorf("store.TouchBlobRefLastSeenOK: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.TouchBlobRefLastSeenOK: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetBlobRefHandoffPending toggles the handoff_pending flag for
// the row, returning ErrNotFound if no row matches. Used by the
// device-switch state machine (docs §3.7):
//
//   - true at step 3 (source peer marks the row mid-switch so
//     concurrent writes on either side surface as 409),
//   - false at step 5 success or rollback (post-pull commit or
//     pull failure → revert).
//
// Idempotent on the boolean (re-setting to the same value is a
// no-op). updated_at is bumped unconditionally so a subscriber
// of the changes cursor sees the activity.
func (s *Store) SetBlobRefHandoffPending(ctx context.Context, uri string, pending bool) error {
	if uri == "" {
		return errors.New("store.SetBlobRefHandoffPending: uri required")
	}
	val := 0
	if pending {
		val = 1
	}
	now := NowMillis()
	res, err := s.db.ExecContext(ctx,
		`UPDATE blob_refs SET handoff_pending = ?, updated_at = ? WHERE uri = ?`,
		val, now, uri,
	)
	if err != nil {
		return fmt.Errorf("store.SetBlobRefHandoffPending: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.SetBlobRefHandoffPending: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SwitchBlobRefHome flips home_peer to the new owner AND clears
// handoff_pending in a single tx. Used at §3.7 step 5 when the
// target peer's pull succeeded and the row's authoritative home
// moves to the target. Returns ErrNotFound on missing row.
//
// The single-statement update is atomic at the SQLite level so a
// concurrent reader either sees the old (source, pending=true)
// pair or the new (target, pending=false) pair, never a half-
// committed mix.
func (s *Store) SwitchBlobRefHome(ctx context.Context, uri, newHomePeer string) error {
	if uri == "" {
		return errors.New("store.SwitchBlobRefHome: uri required")
	}
	if newHomePeer == "" {
		return errors.New("store.SwitchBlobRefHome: new_home_peer required")
	}
	now := NowMillis()
	res, err := s.db.ExecContext(ctx,
		`UPDATE blob_refs
		    SET home_peer = ?, handoff_pending = 0, updated_at = ?
		  WHERE uri = ?`,
		newHomePeer, now, uri,
	)
	if err != nil {
		return fmt.Errorf("store.SwitchBlobRefHome: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.SwitchBlobRefHome: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkBlobRefForGC stamps marked_for_gc_at = now on the row. Used by
// internal/blob.Delete when slice 3+ adds the 24h grace window; slice
// 2 calls DeleteBlobRef directly. Idempotent.
func (s *Store) MarkBlobRefForGC(ctx context.Context, uri string) error {
	now := NowMillis()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE blob_refs SET marked_for_gc_at = ?, updated_at = ? WHERE uri = ?`,
		now, now, uri,
	); err != nil {
		return fmt.Errorf("store.MarkBlobRefForGC: %w", err)
	}
	return nil
}

// scanBlobRefRow reads one row's columns in the order produced by
// every SELECT in this file.
func scanBlobRefRow(r rowScanner) (*BlobRefRecord, error) {
	var (
		rec      BlobRefRecord
		handoff  int
	)
	if err := r.Scan(
		&rec.URI, &rec.Scope, &rec.HomePeer, &rec.Size, &rec.SHA256, &rec.Refcount,
		&rec.PinPolicy, &rec.LastSeenOK, &rec.MarkedForGCAt,
		&handoff, &rec.CreatedAt, &rec.UpdatedAt,
	); err != nil {
		return nil, err
	}
	rec.HandoffPending = handoff != 0
	return &rec, nil
}

// nullableInt64 returns sql.NullInt64{} for zero so the column stores
// NULL rather than 0 — preserves the schema's "missing" semantic for
// last_seen_ok / marked_for_gc_at.
func nullableInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// nextPrefix returns the lexicographically smallest byte string that
// is strictly greater than every string starting with p, paired with
// a boolean indicating whether such a successor exists.
//
// Algorithm: increment the rightmost byte that is < 0xFF and truncate
// everything to its right. If every byte is 0xFF no finite successor
// exists (any candidate `p + suffix` is itself a string starting with
// p), and the function returns ("", false) so the caller can drop
// the upper-bound clause from its range scan.
func nextPrefix(p string) (string, bool) {
	b := []byte(p)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xFF {
			b[i]++
			return string(b[:i+1]), true
		}
	}
	return "", false
}

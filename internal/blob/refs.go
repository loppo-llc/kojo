package blob

import (
	"context"
	"errors"
)

// ErrRefNotFound mirrors store.ErrNotFound; refs implementations MUST
// return this exact value (or wrap it via fmt.Errorf with %w) so the
// blob layer can sniff it via errors.Is regardless of the concrete
// store backend.
var ErrRefNotFound = errors.New("blob: ref not found")

// Ref is the metadata cache entry for one blob URI. It mirrors the
// columns the blob package actually reads from store.BlobRefRecord.
// HandoffPending was added so Store.Put can preflight the §3.7
// guard before writing the body to disk — without it a refused
// row update would leave the new body orphaned at the rename
// site.
type Ref struct {
	URI            string
	Scope          string
	HomePeer       string
	Size           int64
	SHA256         string
	HandoffPending bool
	// UpdatedAt mirrors blob_refs.updated_at (unix millis).
	// blob.Store.Put captures the post-write timestamp so the
	// rollback path's OCC tuple is (sha256, updated_at) — a
	// concurrent same-digest mutation (scrub bump) advances
	// updated_at and our restore correctly refuses. Zero when
	// the backend doesn't track timestamps (slice-1 noopRefs).
	UpdatedAt int64
}

// RefStore is the minimal contract internal/blob needs from the DB.
// store.BlobRefStore (defined in this package) adapts *store.Store to
// it; tests that don't care about the cache pass nil to New() and get
// the noop implementation, which behaves like slice 1 (Head / List /
// Get leave SHA256 / ETag empty, IfMatch / Verify read the file).
type RefStore interface {
	// Get returns the row keyed by uri or ErrRefNotFound when absent.
	// Implementations MUST translate their internal not-found error
	// to ErrRefNotFound so the blob layer can branch on it without
	// importing the underlying store.
	Get(ctx context.Context, uri string) (*Ref, error)

	// Put inserts or replaces the row. Returns the new
	// updated_at unix-millis stamp so the blob layer's
	// rollback path can gate Restore / DeleteIfMatches on the
	// (sha256, updated_at) tuple WITHOUT a follow-up Get that
	// would race a concurrent writer. Backends that don't
	// track timestamps return 0 — the blob layer's OCC then
	// degrades to the disabled branch (best-effort cleanup).
	Put(ctx context.Context, ref *Ref) (updatedAt int64, err error)

	// Delete removes the row. Idempotent: callers may invoke even
	// when the URI was never present, so implementations must NOT
	// return ErrRefNotFound for missing rows.
	Delete(ctx context.Context, uri string) error

	// List returns rows whose URI starts with the given prefix and
	// (optionally) belongs to the given scope. An empty scope means
	// "any scope". Output is ordered by URI ASC.
	List(ctx context.Context, scope, uriPrefix string) ([]*Ref, error)
}

// bypassPutter is implemented by RefStore backends that can write
// a row even when the existing row's handoff_pending=1. blob.Store
// type-asserts on this interface when PutOptions.BypassHandoffPending
// is set; backends that don't implement it fall back to the normal
// Put (still correct for cluster-fresh target rows).
type bypassPutter interface {
	PutBypass(ctx context.Context, ref *Ref) (updatedAt int64, err error)
}

// conditionalDeleter is implemented by RefStore backends that
// can delete a row only when its current (sha256, updatedAt)
// match the supplied tuple. blob.Store uses this on the
// rollback path when its prior Snapshot saw NO row — a 3rd-
// party writer that recreated the URI in the commit-failure
// window would otherwise be silently deleted by an
// unconditional refs.Delete. The bool return reports whether
// the row actually went away; backends that don't implement
// the interface degrade to refs.Delete (the v1 trust model
// makes the race vanishingly rare).
type conditionalDeleter interface {
	DeleteIfMatches(ctx context.Context, uri, sha256 string, updatedAt int64) (bool, error)
}

// RefSnapshot is an opaque pre-write snapshot of one blob_refs
// row. blob.Store grabs one before any write that might need to
// be rolled back; the refSnapshotter implementation owns the
// concrete shape (StoreRefs stashes a full *store.BlobRefRecord
// so managed fields — refcount, pin_policy, last_seen_ok,
// marked_for_gc_at, handoff_pending — are preserved on restore).
type RefSnapshot any

// refSnapshotter is implemented by RefStore backends that can
// produce a full-fidelity snapshot of a blob_refs row and later
// restore it byte-for-byte. blob.Store uses this around the
// atomicStage / refs.Put / atomicCommit sequence so a commit
// failure can revert the row state without losing managed
// fields. Backends that don't implement the interface degrade
// to "no snapshot; on commit failure, leave the row as-is and
// rely on the next scrub to repair".
//
// Restore takes the OCC tuple (expectedCurrentSHA256,
// expectedCurrentUpdatedAt) the caller asserts is the row's
// CURRENT state (typically the (sha256, updated_at) of the row
// right after the failing Put advanced it). Backends use both
// for optimistic concurrency: if a third party touched the row
// past that state — even with the same sha256, e.g. a scrub
// last_seen_ok bump — restoring the snapshot would clobber them.
// Empty expectedCurrentSHA256 OR zero expectedCurrentUpdatedAt
// disables the check.
type refSnapshotter interface {
	Snapshot(ctx context.Context, uri string) (RefSnapshot, error)
	Restore(ctx context.Context, snap RefSnapshot, expectedCurrentSHA256 string, expectedCurrentUpdatedAt int64) error
}

// noopRefs is the zero-cost stand-in used when the caller didn't
// thread a RefStore. Its existence lets the per-call code path stay
// uniform — every operation calls s.refs.Whatever(...) without a nil
// guard. Get always returns ErrRefNotFound so callers fall through
// to the slice-1 file-based behavior.
type noopRefs struct{}

func (noopRefs) Get(context.Context, string) (*Ref, error) {
	return nil, ErrRefNotFound
}
func (noopRefs) Put(context.Context, *Ref) (int64, error)       { return 0, nil }
func (noopRefs) PutBypass(context.Context, *Ref) (int64, error) { return 0, nil }
func (noopRefs) Snapshot(context.Context, string) (RefSnapshot, error) {
	// Matches Get(): there's nothing to snapshot in slice-1
	// fs-only mode, so the caller treats a refSnapshotter that
	// returns ErrRefNotFound as "no prior state to restore".
	return nil, ErrRefNotFound
}
func (noopRefs) Restore(context.Context, RefSnapshot, string, int64) error { return nil }
func (noopRefs) Delete(context.Context, string) error       { return nil }
func (noopRefs) List(context.Context, string, string) ([]*Ref, error) {
	return nil, nil
}

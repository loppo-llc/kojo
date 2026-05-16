package blob

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/loppo-llc/kojo/internal/store"
)

// StoreRefs adapts *store.Store to the RefStore interface so the blob
// package can stay decoupled from the SQL schema details. The adapter
// translates store.ErrNotFound into the blob-local ErrRefNotFound and
// pre-builds the `kojo://<scope>/<path>` URI that blob_refs uses as
// its primary key.
type StoreRefs struct {
	st       *store.Store
	homePeer string
}

// NewStoreRefs builds a RefStore-compatible adapter. homePeer is the
// peer id this daemon advertises in blob_refs.home_peer; for slice 2
// any non-empty string works (the field is consumed by Phase 4 device
// switch, not by anything in this slice).
func NewStoreRefs(st *store.Store, homePeer string) *StoreRefs {
	return &StoreRefs{st: st, homePeer: homePeer}
}

// BuildURI produces the canonical blob_refs primary key for a
// (scope, path) pair. Each path segment is percent-encoded per
// RFC 3986 path-segment rules so URIs are unambiguous when round-
// tripped through HTTP / log lines (a literal `#` would otherwise
// be parsed as a fragment, a space would be displayed as `+` by
// some readers, and non-ASCII NFC characters could be re-encoded
// inconsistently). The `/` separators between segments are
// preserved as-is — a percent-encoded `/` would defeat the prefix
// range-scan that ListBlobRefs depends on.
func BuildURI(scope Scope, path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return "kojo://" + string(scope) + "/" + strings.Join(parts, "/")
}

func (r *StoreRefs) Get(ctx context.Context, uri string) (*Ref, error) {
	rec, err := r.st.GetBlobRef(ctx, uri)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrRefNotFound
	}
	if err != nil {
		return nil, err
	}
	return &Ref{
		URI:            rec.URI,
		Scope:          rec.Scope,
		HomePeer:       rec.HomePeer,
		Size:           rec.Size,
		SHA256:         rec.SHA256,
		HandoffPending: rec.HandoffPending,
		UpdatedAt:      rec.UpdatedAt,
	}, nil
}

func (r *StoreRefs) Put(ctx context.Context, ref *Ref) (int64, error) {
	return r.putWith(ctx, ref, store.BlobRefInsertOptions{})
}

// PutBypass satisfies the bypassPutter interface so blob.Store
// can route §3.7 pull writes through the AllowHandoffPending
// branch. Normal callers MUST use Put — the bypass is reserved
// for the orchestrator's pull, which already authoritatively
// owns the row's pending state via the begin/complete transitions.
func (r *StoreRefs) PutBypass(ctx context.Context, ref *Ref) (int64, error) {
	return r.putWith(ctx, ref, store.BlobRefInsertOptions{AllowHandoffPending: true})
}

// Snapshot captures the full blob_refs row for uri so blob.Store
// can roll back a failed write byte-for-byte (every managed field
// — refcount, pin_policy, last_seen_ok, marked_for_gc_at,
// handoff_pending — survives the round trip). Returns
// ErrRefNotFound when no row exists; the caller treats that as
// "rollback means delete the new row".
func (r *StoreRefs) Snapshot(ctx context.Context, uri string) (RefSnapshot, error) {
	rec, err := r.st.GetBlobRef(ctx, uri)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrRefNotFound
	}
	if err != nil {
		return nil, err
	}
	// Defensive copy so the caller can't mutate the snapshot
	// between snapshot and restore.
	dup := *rec
	return &dup, nil
}

// Restore writes a Snapshot back to blob_refs verbatim. The
// expectedCurrent OCC tuple the blob layer passes (typically the
// (sha256, updated_at) of the row right after the just-failed
// refs.Put) gates the UPDATE — if another writer advanced the
// row past that state in the commit-failure window, store.
// RestoreBlobRef returns ErrRestoreSuperseded and we leave
// their state alone. The store-level helper uses a full-column
// UPDATE so managed fields (refcount, pin_policy, last_seen_ok,
// marked_for_gc_at, handoff_pending) survive the round trip.
func (r *StoreRefs) Restore(ctx context.Context, snap RefSnapshot, expectedCurrentSHA256 string, expectedCurrentUpdatedAt int64) error {
	rec, ok := snap.(*store.BlobRefRecord)
	if !ok || rec == nil {
		return fmt.Errorf("blob.StoreRefs.Restore: invalid snapshot type %T", snap)
	}
	// Copy again so this call doesn't mutate the snapshot — a
	// retry of Restore should be safe.
	row := *rec
	row.ExpectedCurrentSHA256 = expectedCurrentSHA256
	row.ExpectedCurrentUpdatedAt = expectedCurrentUpdatedAt
	return r.st.RestoreBlobRef(ctx, &row)
}

func (r *StoreRefs) putWith(ctx context.Context, ref *Ref, opts store.BlobRefInsertOptions) (int64, error) {
	if ref == nil {
		return 0, fmt.Errorf("blob.StoreRefs.Put: nil ref")
	}
	homePeer := ref.HomePeer
	if homePeer == "" {
		homePeer = r.homePeer
	}
	if homePeer == "" {
		// Without a home_peer the row violates a NOT NULL — fail
		// loudly so a misconfigured daemon can't silently degrade.
		return 0, fmt.Errorf("blob.StoreRefs.Put: home_peer not configured")
	}
	rec, err := r.st.InsertOrReplaceBlobRef(ctx, &store.BlobRefRecord{
		URI:      ref.URI,
		Scope:    ref.Scope,
		HomePeer: homePeer,
		Size:     ref.Size,
		SHA256:   ref.SHA256,
	}, opts)
	if err != nil {
		// Translate the store-layer sentinel into the blob-layer
		// sentinel so callers above (blob.Store.Put → HTTP
		// handler) can errors.Is(err, blob.ErrHandoffPending)
		// without importing internal/store. Wrap %w so the
		// underlying message is still visible in logs.
		if errors.Is(err, store.ErrHandoffPending) {
			return 0, fmt.Errorf("blob.StoreRefs.Put: %w", ErrHandoffPending)
		}
		return 0, err
	}
	if rec == nil {
		return 0, nil
	}
	return rec.UpdatedAt, nil
}

func (r *StoreRefs) Delete(ctx context.Context, uri string) error {
	return r.st.DeleteBlobRef(ctx, uri)
}

// DeleteIfMatches satisfies the conditionalDeleter interface so
// blob.Store can roll back a "we created this row" Put without
// nuking a row a 3rd-party writer materialised in the commit-
// failure window. Returns (deleted, err); the blob layer treats
// deleted=false as "leave it alone".
func (r *StoreRefs) DeleteIfMatches(ctx context.Context, uri, sha256 string, updatedAt int64) (bool, error) {
	return r.st.DeleteBlobRefIfMatches(ctx, uri, sha256, updatedAt)
}

func (r *StoreRefs) List(ctx context.Context, scope, uriPrefix string) ([]*Ref, error) {
	recs, err := r.st.ListBlobRefs(ctx, store.ListBlobRefsOptions{
		Scope:     scope,
		URIPrefix: uriPrefix,
	})
	if err != nil {
		return nil, err
	}
	out := make([]*Ref, 0, len(recs))
	for _, rec := range recs {
		out = append(out, &Ref{
			URI:      rec.URI,
			Scope:    rec.Scope,
			HomePeer: rec.HomePeer,
			Size:     rec.Size,
			SHA256:   rec.SHA256,
		})
	}
	return out, nil
}

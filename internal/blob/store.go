package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ErrNotFound is returned by Get / Head / Delete / Verify when no blob
// exists at the requested (scope, path).
var ErrNotFound = errors.New("blob: not found")

// ErrETagMismatch is returned by Put / Delete when an If-Match was
// supplied and the current etag does not match. Mirrors the store's
// ErrETagMismatch so callers get a consistent type across DB and blob
// optimistic concurrency.
var ErrETagMismatch = errors.New("blob: etag mismatch")

// ErrExpectedSHA256Mismatch is returned by Put when PutOptions.
// ExpectedSHA256 was set and the streamed body hashed to a different
// digest. atomicWrite wraps this with the actual / expected pair via
// fmt.Errorf("...: %w", ...) so callers can `errors.Is` cleanly while
// still getting an informative message in logs.
var ErrExpectedSHA256Mismatch = errors.New("blob: sha256 mismatch")

// ErrDurabilityDegraded signals that blob.Store.Put committed the
// body to disk AND the blob_refs row, but the parent directory
// fsync failed — a power-loss within seconds of the call could
// resurrect the prior leaf in the dir entry. The body/row pair is
// internally consistent, so a caller orchestrating a §3.7
// device-switch can treat the write as logically successful
// (don't abort the handoff) while still surfacing the degradation
// to operators / metrics. errors.Is the sentinel out of the wrap
// chain to distinguish from a hard-failure error.
var ErrDurabilityDegraded = errors.New("blob: durability degraded (fsync dir failed)")

// ErrHandoffPending mirrors store.ErrHandoffPending: a device-switch
// (§3.7) has marked this blob's row as mid-handoff and runtime
// writes that would actually change the body are refused. The
// underlying ref store returns the sentinel; blob.Store.Put
// re-exports it under this package so callers can errors.Is
// without importing internal/store.
var ErrHandoffPending = errors.New("blob: row is mid-handoff (handoff_pending=1)")

// ErrScopeContainmentBroken signals a runtime defect in the on-disk
// blob tree: the scope dir or one of its parent components is a
// symlink, or an intermediate component is a regular file where a
// directory belongs. Distinct from ErrInvalidPath, which is a
// logical-path validation failure (NFC, reserved chars).
//
// Migration callers MUST abort on this error rather than warn-and-
// skip — silently skipping it would let the importer mark the domain
// "imported" while the v1 blob tree is still structurally unsafe to
// write into. HTTP callers map it to 500 (server-side defect, not
// client input).
var ErrScopeContainmentBroken = errors.New("blob: scope containment broken")

// Object is the metadata view of a blob. SHA256 / ETag come from the
// blob_refs cache when the Store was constructed With Refs; otherwise
// (slice 1 fs-only mode) they are populated only by Put / Verify.
type Object struct {
	Scope   Scope
	Path    string // logical path under scope, "/"-separated
	Size    int64
	ModTime int64  // unix milliseconds
	SHA256  string // hex; "" when unknown
	ETag    string // strong etag = "sha256:<hex>"; "" when unknown
}

// Store is the entry point. Construct one per kojo data root; goroutine
// safe because each operation re-resolves paths and uses its own file
// descriptors. Per-(scope, path) write serialization is provided so
// the IfMatch + write pair is not trivially racy with another writer
// holding the same etag.
type Store struct {
	root     string
	locks    sync.Map // key=scope|path, value=*sync.Mutex
	refs     RefStore
	homePeer string
}

// Option configures a Store at construction time.
type Option func(*Store)

// WithRefs wires a RefStore (typically blob.NewStoreRefs(*store.Store,
// homePeer)). When set, Head / Get populate ETag / SHA256 directly
// from the cache, IfMatch becomes a single SELECT, and Put / Delete
// keep the row in lock-step with the on-disk file. List is not
// touched in slice 2 — it still walks the filesystem and leaves
// SHA256 / ETag empty on each entry; callers who need digests for a
// listing iterate Head per result. Slice 3+ may switch List to a
// blob_refs query if profiling shows large fs walks dominate.
func WithRefs(refs RefStore) Option {
	return func(s *Store) {
		if refs == nil {
			return
		}
		s.refs = refs
	}
}

// WithHomePeer sets the peer id stamped onto blob_refs.home_peer for
// every Put. Must be set whenever WithRefs is — without it
// StoreRefs.Put rejects the call (the column is NOT NULL). Tests that
// use noopRefs may leave this empty.
func WithHomePeer(peer string) Option {
	return func(s *Store) {
		s.homePeer = peer
	}
}

// New builds a Store anchored at root (typically configdir.Path()).
// The caller is responsible for creating root before the first Put;
// individual scope subdirs are auto-created by Put as needed. Without
// WithRefs the store runs in slice-1 fs-only mode.
// BaseDir returns the absolute root path the Store writes blob
// bodies under. Exported for callers (notably the integrity
// scrubber) that need to write quarantine files *outside* the
// scope sub-trees so the corrupt body cannot be served back via
// Open / List. Returns "" when called on a nil receiver.
func (s *Store) BaseDir() string {
	if s == nil {
		return ""
	}
	return s.root
}

func New(root string, opts ...Option) *Store {
	s := &Store{
		root: filepath.Clean(root),
		refs: noopRefs{},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// pathLock returns a mutex unique to (scope, path). The returned lock
// is shared with any concurrent caller of Put / Delete on the same
// blob — Get / Head / Verify / List don't take it because their reads
// are individually atomic via the kernel and they observe whatever
// rename was last to win. The map grows monotonically; in practice
// path count is bounded by agent×asset and a few thousand entries are
// negligible. Slice 2 will replace this with a blob_refs row lock so
// the IfMatch + INSERT/UPDATE pair is one transaction.
func (s *Store) pathLock(scope Scope, p string) *sync.Mutex {
	key := string(scope) + "|" + p
	if v, ok := s.locks.Load(key); ok {
		return v.(*sync.Mutex)
	}
	mu := &sync.Mutex{}
	actual, _ := s.locks.LoadOrStore(key, mu)
	return actual.(*sync.Mutex)
}

// assertScopeContained walks each path component from scopeDir down to
// fullPath via Lstat, rejecting any symlink it sees. A leaf that does
// not yet exist is fine — Put will create it on the spot. The check is
// per-component (not EvalSymlinks-then-prefix) because EvalSymlinks
// silently dereferences symlinks whose target also resolves under the
// scope dir, which is still an attack surface (an agent_id directory
// re-pointed at /etc would pass an EvalSymlinks-prefix check if the
// hostile peer placed a link at /etc/<root>).
//
// Slice 1 best-effort: there is a TOCTOU window between this check
// and the subsequent open / mkdir / rename. Slice 2 will move the
// whole IfMatch + write into a blob_refs DB transaction and gain real
// linearizability.
func assertScopeContained(scopeDir, fullPath string) error {
	rel, err := filepath.Rel(scopeDir, fullPath)
	if err != nil {
		return ErrInvalidPath
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ErrInvalidPath
	}
	// Verify the scope dir itself isn't a symlink — defense against an
	// adversary that prepared `<root>/global` as a link before the
	// daemon started. This is an *environment* defect, not a logical
	// path issue, so callers (especially the migration importer)
	// can branch on ErrScopeContainmentBroken to abort rather than
	// warn-and-skip.
	if st, err := os.Lstat(scopeDir); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return ErrScopeContainmentBroken
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	cur := scopeDir
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			// Missing component — Put will create it. Anything below
			// this point is also necessarily missing.
			return nil
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return ErrScopeContainmentBroken
		}
		// Intermediate components must be directories. A regular file
		// in the middle of the path is malformed — refuse rather than
		// stat-then-fail-with-EEXIST later.
		if i < len(parts)-1 && !st.IsDir() {
			return ErrScopeContainmentBroken
		}
	}
	return nil
}

// PutOptions tunes a Put. ExpectedSHA256 lets the caller assert what
// the digest should be — if the streamed body hashes to anything else
// the temp file is unlinked and the publish aborts before rename.
// IfMatch performs optimistic concurrency against the existing blob's
// digest (slice-1 etag = "sha256:<hex>"); a non-matching value returns
// ErrETagMismatch without touching the on-disk file.
type PutOptions struct {
	IfMatch        string
	ExpectedSHA256 string
	Mode           os.FileMode
	// BypassHandoffPending lets a §3.7 pull (target-side blob
	// fetch) write a row whose existing handoff_pending flag
	// would otherwise refuse the update. The agent runtime
	// MUST NOT set this — it's reserved for the orchestrator's
	// pull path, which is itself the authority that clears the
	// flag. Reflected in the underlying RefStore via a type-
	// asserted bypass-capable interface; backends that don't
	// implement it fall back to the normal Put (which is
	// already correct for cluster-fresh target rows).
	BypassHandoffPending bool
}

// DeleteOptions mirrors PutOptions's IfMatch semantics for removals.
type DeleteOptions struct {
	IfMatch string
}

// fsPath resolves a logical (scope, path) into an on-disk path,
// validating both. Callers MUST go through this helper — direct
// filepath.Join would skip the path validation, the scope sandbox,
// and the symlink-escape guard.
func (s *Store) fsPath(scope Scope, p string) (string, string, error) {
	if !scope.Valid() {
		return "", "", ErrInvalidScope
	}
	cleaned, err := validatePath(p)
	if err != nil {
		return "", "", err
	}
	scopeDir := resolveDir(s.root, scope)
	full := filepath.Join(scopeDir, filepath.FromSlash(cleaned))
	if err := assertScopeContained(scopeDir, full); err != nil {
		return "", "", err
	}
	return full, cleaned, nil
}

// Get opens the blob for streaming reads. The returned Object carries
// Size / ModTime; SHA256 / ETag are populated from the blob_refs cache
// when WithRefs is set (slice 2) and otherwise left empty (slice 1
// callers can call Verify to compute). Callers MUST Close the
// returned reader.
func (s *Store) Get(scope Scope, p string) (io.ReadCloser, *Object, error) {
	f, obj, err := s.Open(scope, p)
	if err != nil {
		return nil, nil, err
	}
	return f, obj, nil
}

// Open is identical to Get but returns *os.File so callers can drive
// http.ServeContent (Range, conditional GET) without an interface
// assertion. Callers MUST Close the returned file. The Object is
// populated the same way as Get.
func (s *Store) Open(scope Scope, p string) (*os.File, *Object, error) {
	full, cleaned, err := s.fsPath(scope, p)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, nil, ErrNotFound
	}
	obj := &Object{
		Scope:   scope,
		Path:    cleaned,
		Size:    st.Size(),
		ModTime: st.ModTime().UnixMilli(),
	}
	s.populateDigestFromRefs(scope, cleaned, obj)
	return f, obj, nil
}

// Head returns Size / ModTime without opening the body. SHA256 / ETag
// are populated from blob_refs when WithRefs is set; otherwise empty
// (slice-1 fs-only mode).
func (s *Store) Head(scope Scope, p string) (*Object, error) {
	full, cleaned, err := s.fsPath(scope, p)
	if err != nil {
		return nil, err
	}
	st, err := os.Lstat(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, ErrNotFound
	}
	obj := &Object{
		Scope:   scope,
		Path:    cleaned,
		Size:    st.Size(),
		ModTime: st.ModTime().UnixMilli(),
	}
	s.populateDigestFromRefs(scope, cleaned, obj)
	return obj, nil
}

// populateDigestFromRefs fills SHA256 / ETag on obj from the cache
// when present. Cache misses (RefNotFound, transient DB errors) leave
// the fields empty; callers that need a guaranteed digest fall back
// to Verify, and a stale-or-missing cache is repaired by the scrub
// job (slice 3+). This helper is intentionally side-effect free so
// read paths cannot fail just because the cache is unavailable.
func (s *Store) populateDigestFromRefs(scope Scope, cleaned string, obj *Object) {
	ref, err := s.refs.Get(context.Background(), BuildURI(scope, cleaned))
	if err != nil || ref == nil {
		return
	}
	obj.SHA256 = ref.SHA256
	if ref.SHA256 != "" {
		obj.ETag = "sha256:" + ref.SHA256
	}
}

// Verify computes the sha256 of the on-disk blob and returns the
// fully-populated Object (Size / ModTime / SHA256 / ETag). Slice 1
// has no digest cache so this reads the whole file; slice 2 will
// short-circuit through blob_refs. Used by repair / scrub paths and
// by Put's IfMatch handling.
func (s *Store) Verify(scope Scope, p string) (*Object, error) {
	full, cleaned, err := s.fsPath(scope, p)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, ErrNotFound
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return nil, err
	}
	digest := hex.EncodeToString(h.Sum(nil))
	return &Object{
		Scope:   scope,
		Path:    cleaned,
		Size:    n,
		ModTime: st.ModTime().UnixMilli(),
		SHA256:  digest,
		ETag:    "sha256:" + digest,
	}, nil
}

// Put publishes src under (scope, path). On success the returned
// Object has SHA256 / ETag filled in (computed in a single pass over
// the input stream). If opts.IfMatch is set, the current blob's
// sha256 is computed and compared; a mismatch returns ErrETagMismatch
// without touching the file. If opts.ExpectedSHA256 is set and the
// streamed body hashes differently, the publish aborts before rename
// and the on-disk file is left unchanged.
func (s *Store) Put(scope Scope, p string, src io.Reader, opts PutOptions) (*Object, error) {
	full, cleaned, err := s.fsPath(scope, p)
	if err != nil {
		return nil, err
	}
	// Serialize concurrent IfMatch writers on the same path — without
	// this two callers holding the same etag could each Verify, see
	// the same digest, and both rename their temp into place. The
	// loser would silently lose its update. Slice 2's blob_refs tx
	// makes this a true single-writer operation.
	mu := s.pathLock(scope, cleaned)
	mu.Lock()
	defer mu.Unlock()
	if opts.IfMatch != "" {
		// Cache-first: when WithRefs is wired, the digest comes from
		// blob_refs (single SELECT). On cache miss we fall back to
		// Verify (read the file) so a row that hasn't been
		// backfilled yet still gates correctly.
		curETag, etagErr := s.currentETag(scope, cleaned)
		switch {
		case errors.Is(etagErr, ErrNotFound):
			// IfMatch on a missing blob never matches.
			return nil, ErrETagMismatch
		case etagErr != nil:
			return nil, etagErr
		case curETag != opts.IfMatch:
			return nil, ErrETagMismatch
		}
	}
	// §3.7 preflight: refuse before staging when the existing
	// row is mid-handoff AND the proposed write would change
	// body-derived columns. Best-effort because noopRefs / stale
	// reads return ErrRefNotFound; ref.Put after the stage is
	// the authoritative gate, and a TOCTOU flip between
	// preflight and ref.Put is handled by the stage/commit
	// rollback below.
	if !opts.BypassHandoffPending {
		uri := BuildURI(scope, cleaned)
		if curRef, gerr := s.refs.Get(context.Background(), uri); gerr == nil &&
			curRef != nil && curRef.HandoffPending {
			if opts.ExpectedSHA256 == "" ||
				opts.ExpectedSHA256 != curRef.SHA256 ||
				s.homePeer != curRef.HomePeer {
				return nil, ErrHandoffPending
			}
		}
	}
	mode := opts.Mode
	if mode == 0 {
		mode = 0o644
	}
	// Snapshot the prior ref state BEFORE we mutate the row, so
	// a failed atomicCommit (rename) can restore blob_refs
	// byte-for-byte (managed fields included). A non-NotFound
	// Get error means we can't safely roll back — fail closed
	// before the body is staged rather than risk deleting a row
	// that still exists.
	uri := BuildURI(scope, cleaned)
	var priorSnap RefSnapshot
	var hadPrior bool
	if snapper, ok := s.refs.(refSnapshotter); ok {
		snap, gerr := snapper.Snapshot(context.Background(), uri)
		switch {
		case gerr == nil:
			priorSnap = snap
			hadPrior = true
		case errors.Is(gerr, ErrRefNotFound):
			// No row to snapshot; rollback would Delete.
		default:
			return nil, fmt.Errorf("blob: snapshot prior ref: %w", gerr)
		}
	}

	tmp, size, digest, err := atomicStage(full, src, mode, opts.ExpectedSHA256)
	if err != nil {
		return nil, err
	}
	// From here on we own a staged temp file. cleanup tracks
	// the two reversible mutations:
	//   - tmpStaged: the body sits in a temp file waiting for
	//     atomicCommit. Unlink if we don't commit.
	//   - refAdvanced: blob_refs already moved to the new
	//     digest. Restore via the Snapshot when we abandon the
	//     write — expecting the row's CURRENT (sha256,
	//     updated_at) to still match what refs.Put left so a
	//     concurrent same-digest mutation (scrub last_seen_ok
	//     bump, handoff_pending flip) isn't clobbered.
	committed := false
	refAdvanced := false
	var postPutUpdatedAt int64
	defer func() {
		if committed {
			return
		}
		atomicRollback(tmp)
		if !refAdvanced {
			return
		}
		// Roll the ref back to its pre-write state. Errors
		// here are best-effort: we're already on a failure
		// path and surfacing them would bury the original
		// cause. The next scrub pass or Verify call catches
		// any lingering inconsistency.
		restoreCtx := context.Background()
		if hadPrior {
			if snapper, ok := s.refs.(refSnapshotter); ok {
				// expected-current = (digest, post-put
				// updated_at). Restore refuses with
				// ErrRestoreSuperseded if a third party
				// already moved past us — including a
				// same-digest scrub bump.
				_ = snapper.Restore(restoreCtx, priorSnap, digest, postPutUpdatedAt)
			} else if bp, ok := s.refs.(bypassPutter); ok {
				// Fallback: backend lacks Snapshot/Restore
				// but supports the bypass put. Lossy (managed
				// fields aren't preserved) but better than
				// leaving the row at the new digest.
				if pr, _ := s.refs.Get(restoreCtx, uri); pr != nil {
					_, _ = bp.PutBypass(restoreCtx, pr)
				}
			}
		} else {
			// We created the row from nothing. Conditional
			// delete first so a 3rd party that materialised
			// the same URI in the commit-failure window keeps
			// their row.
			if cd, ok := s.refs.(conditionalDeleter); ok {
				_, _ = cd.DeleteIfMatches(restoreCtx, uri, digest, postPutUpdatedAt)
			} else {
				_ = s.refs.Delete(restoreCtx, uri)
			}
		}
	}()

	ref := &Ref{
		URI:      uri,
		Scope:    string(scope),
		HomePeer: s.homePeer,
		Size:     size,
		SHA256:   digest,
	}
	var refPutErr error
	if opts.BypassHandoffPending {
		if bp, ok := s.refs.(bypassPutter); ok {
			postPutUpdatedAt, refPutErr = bp.PutBypass(context.Background(), ref)
		} else {
			postPutUpdatedAt, refPutErr = s.refs.Put(context.Background(), ref)
		}
	} else {
		postPutUpdatedAt, refPutErr = s.refs.Put(context.Background(), ref)
	}
	if refPutErr != nil {
		// Body never lands on dst when ref.Put refuses (the
		// deferred atomicRollback unlinks the staged temp).
		// errors.Is preserves the typed sentinel so the
		// handler layer can map ErrHandoffPending → 409
		// without seeing the underlying body-already-written
		// failure mode.
		return nil, fmt.Errorf("blob: ref put: %w", refPutErr)
	}
	refAdvanced = true
	// postPutUpdatedAt arrives from the same statement that
	// performed the row write — no follow-up Get, so a
	// concurrent writer can't slip in between and feed the
	// rollback path a stale OCC tuple. Backends that don't
	// track timestamps (noopRefs) return 0; the rollback then
	// degrades to the OCC-disabled path.
	renamed, cerr := atomicCommit(tmp, full)
	if cerr != nil {
		if !renamed {
			// rename failed — body never landed at dst.
			// Deferred cleanup runs: temp unlinked, ref
			// restored to prior snapshot.
			return nil, cerr
		}
		// Rename succeeded but fsyncDir failed. The new body
		// IS at dst and matches the row we just put —
		// restoring the prior ref would create a body vs.
		// row inconsistency. Mark the write as committed so
		// the deferred cleanup short-circuits, then fall
		// through to the success path with a wrapped
		// ErrDurabilityDegraded so the caller can decide
		// whether to treat this as a hard failure or just a
		// metrics blip.
		refAdvanced = false
		committed = true
		st, sterr := os.Stat(full)
		if sterr != nil {
			return nil, fmt.Errorf("blob: stat after publish: %w", sterr)
		}
		degraded := fmt.Errorf("%w: %v", ErrDurabilityDegraded, cerr)
		return &Object{
			Scope:   scope,
			Path:    cleaned,
			Size:    size,
			ModTime: st.ModTime().UnixMilli(),
			SHA256:  digest,
			ETag:    "sha256:" + digest,
		}, degraded
	}
	committed = true
	st, err := os.Stat(full)
	if err != nil {
		return nil, fmt.Errorf("blob: stat after publish: %w", err)
	}
	return &Object{
		Scope:   scope,
		Path:    cleaned,
		Size:    size,
		ModTime: st.ModTime().UnixMilli(),
		SHA256:  digest,
		ETag:    "sha256:" + digest,
	}, nil
}

// currentETag returns the canonical etag for (scope, cleaned). When
// the ref cache has the row, that's a single SELECT — no file read.
// On cache miss the helper falls through to Verify so callers gate
// correctly during the backfill window.
func (s *Store) currentETag(scope Scope, cleaned string) (string, error) {
	ref, err := s.refs.Get(context.Background(), BuildURI(scope, cleaned))
	if err == nil && ref != nil && ref.SHA256 != "" {
		return "sha256:" + ref.SHA256, nil
	}
	if err != nil && !errors.Is(err, ErrRefNotFound) {
		return "", err
	}
	cur, verr := s.Verify(scope, cleaned)
	if verr != nil {
		return "", verr
	}
	return cur.ETag, nil
}

// Delete removes the blob. ErrNotFound on miss. opts.IfMatch is
// honored the same way Put honors it.
func (s *Store) Delete(scope Scope, p string, opts DeleteOptions) error {
	full, cleaned, err := s.fsPath(scope, p)
	if err != nil {
		return err
	}
	mu := s.pathLock(scope, cleaned)
	mu.Lock()
	defer mu.Unlock()
	// Treat anything other than a regular file as not-found so
	// `Delete(scope, "agents/ag_1")` can never accidentally rmdir a
	// scope subtree, and a symlink at the leaf can't slip past the
	// scope sandbox via the kernel's symlink-following Remove
	// semantics. When the caller supplied IfMatch, a missing-or-
	// non-regular target is reported as ErrETagMismatch — the
	// precondition cannot have matched anything, and Put behaves the
	// same way for the missing-path-with-IfMatch case.
	st, err := os.Lstat(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if opts.IfMatch != "" {
				return ErrETagMismatch
			}
			return ErrNotFound
		}
		return err
	}
	if !st.Mode().IsRegular() {
		if opts.IfMatch != "" {
			return ErrETagMismatch
		}
		return ErrNotFound
	}
	if opts.IfMatch != "" {
		curETag, etagErr := s.currentETag(scope, cleaned)
		switch {
		case errors.Is(etagErr, ErrNotFound):
			return ErrETagMismatch
		case etagErr != nil:
			return etagErr
		case curETag != opts.IfMatch:
			return ErrETagMismatch
		}
	}
	if err := os.Remove(full); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	// Drop the cache row to keep the DB in lock-step with the on-disk
	// state. Errors here surface to the caller — leaving a stale row
	// pointing at a removed file is exactly the inconsistency the
	// scrub job is supposed to repair, but slice 2 prefers to fail
	// the call so operator dashboards see the issue.
	if err := s.refs.Delete(context.Background(), BuildURI(scope, cleaned)); err != nil {
		return fmt.Errorf("blob: ref delete: %w", err)
	}
	// Best-effort: don't fsync the parent on Delete. The semantics
	// expected by callers ("eventually gone") are weaker than Put's
	// ("durably published"), and Delete is also a no-op if the
	// parent dir is empty afterwards (we leave the dir for now; the
	// eventual `--clean blobs` target (planned Phase 6 #18; not yet
	// implemented) prunes empty trees).
	return nil
}

// List walks the scope's subtree and returns every regular file whose
// logical path begins with `prefix`. Reserved names / prefixes /
// suffixes are skipped — they cannot legally exist via Put, but a
// human-edited blob dir might still contain them and we'd rather hide
// them than surprise downstream code with paths it would reject on a
// Put round-trip.
//
// Output is sorted by logical path (filepath.Walk gives lexicographic
// order naturally). Size / ModTime are populated; SHA256 / ETag are
// left empty.
func (s *Store) List(scope Scope, prefix string) ([]Object, error) {
	if !scope.Valid() {
		return nil, ErrInvalidScope
	}
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}
	base := resolveDir(s.root, scope)
	var out []Object
	err := filepath.WalkDir(base, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) && p == base {
				// Empty scope (no Put has run yet) — return an empty
				// list rather than ErrNotFound so callers can treat
				// "no blobs yet" uniformly.
				return filepath.SkipDir
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		logical := filepath.ToSlash(rel)
		if prefix != "" && !strings.HasPrefix(logical, prefix) {
			return nil
		}
		// Defense in depth: never expose a reserved name even if some
		// out-of-band process dropped one in the dir. validateSegment
		// already case-folds and applies every reserved-name rule, so
		// reuse it instead of re-implementing the table here.
		skipped := false
		for _, seg := range strings.Split(logical, "/") {
			if validateSegment(seg) != nil {
				skipped = true
				break
			}
		}
		if skipped {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, Object{
			Scope:   scope,
			Path:    logical,
			Size:    info.Size(),
			ModTime: info.ModTime().UnixMilli(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

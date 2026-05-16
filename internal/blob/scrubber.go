package blob

// Blob integrity scrubber (docs/multi-device-storage.md §3.15-bis).
//
// Walks every blob_refs row and verifies the on-disk body's sha256
// matches the canonical hash recorded in the row. Outcomes:
//
//   - Match: last_seen_ok stamped with now. A future scrub can skip
//     rows it has recently verified (not implemented in this slice;
//     the loop currently re-scrubs everything).
//   - File missing: logged Warn — the row references a body that's
//     gone. Repair (re-fetch from another peer / mark missing) is
//     left to the operator; scrub does not auto-delete the row.
//   - Hash mismatch: the on-disk file is renamed to a sibling
//     `<orig>.corrupt.<ts>` so a serving handler can no longer hand
//     out a body that disagrees with its advertised etag, and the
//     row is logged Error. The row itself is left alone for now —
//     blob_refs has no `status='degraded'` column yet, and adding
//     one is a separate schema migration. Operators inspect logs
//     to find quarantined files; an automated repair path is a
//     follow-up slice.
//
// Wiring: cmd/kojo/main.go starts one Scrubber per binary with a
// large interval (default 24h). A future tuneable can drive the
// snapshot-take-time scrub the docs prescribe.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// DefaultScrubInterval is how often Scrubber.loop runs ScrubOnce.
// 24 h: scrub is heavy (re-hashes every blob body) and the failure
// modes it catches (silent bitrot, partial-write torn pages) are
// rare enough that hourly cadence would waste CPU. Tests pass a
// shorter interval via opts.
const DefaultScrubInterval = 24 * time.Hour

// scrubOpTimeout bounds a single per-row Open + ReadAll + hash. A
// large blob (100s of MB) can take seconds even on local SSD; the
// timeout is generous so a legitimately big avatar / attachment
// doesn't get killed mid-hash.
const scrubOpTimeout = 5 * time.Minute

// quarantineSuffix is the format string appended to a corrupt
// blob's filename when the scrubber renames it. The timestamp is
// the scrubber's NowMillis so multiple corrupt-and-quarantine
// rounds for the same URI sort chronologically.
const quarantineSuffix = ".corrupt.%d"

// Scrubber periodically re-hashes blob bodies against blob_refs
// rows. One per process; not concurrency-safe across multiple
// Start invocations (the sync.Once on stopCh would block them).
type Scrubber struct {
	store    *store.Store
	blobs    *Store
	logger   *slog.Logger
	interval time.Duration

	stopCh   chan struct{}
	wg       sync.WaitGroup
	stopOnce sync.Once

	// runCtx is cancelled by Stop so in-flight scrubOne calls can
	// unwind quickly. Set by Start.
	runCtx    context.Context
	runCancel context.CancelFunc
}

// ScrubberOpts overrides defaults.
type ScrubberOpts struct {
	// Interval between full scrubs. Zero uses DefaultScrubInterval.
	Interval time.Duration
}

// NewScrubber wires the deps. Returns nil if either store is nil so
// the caller doesn't have to worry about test setups that skip the
// blob layer.
func NewScrubber(st *store.Store, blobs *Store, logger *slog.Logger, opts ScrubberOpts) *Scrubber {
	if st == nil || blobs == nil {
		return nil
	}
	iv := opts.Interval
	if iv <= 0 {
		iv = DefaultScrubInterval
	}
	return &Scrubber{
		store:    st,
		blobs:    blobs,
		logger:   logger,
		interval: iv,
		stopCh:   make(chan struct{}),
	}
}

// Start launches the background goroutine. Runs one scrub
// immediately so a freshly-restored snapshot doesn't have to wait
// a full interval for the first verification. Stops on Stop().
//
// Derives an internal cancellable context from the caller's so
// Stop can interrupt a multi-minute hash of a large blob without
// the caller having to thread its own cancellation through. The
// caller's ctx is still honoured — a parent cancel propagates.
func (s *Scrubber) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.runCtx, s.runCancel = context.WithCancel(ctx)
	s.wg.Add(1)
	go s.loop(s.runCtx)
}

// Stop signals the loop to exit and cancels in-flight work.
// Bounded: copyCtx in scrubOne polls ctx.Done so a running hash
// returns ctx.Err() within one read-buffer iteration after the
// cancel fires. wg.Wait still blocks until the goroutine exits,
// but the cancellation makes that exit prompt.
func (s *Scrubber) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.runCancel != nil {
			s.runCancel()
		}
		close(s.stopCh)
		s.wg.Wait()
	})
}

func (s *Scrubber) loop(ctx context.Context) {
	defer s.wg.Done()
	// Run once at startup so a long-stopped binary picks up any
	// rot that accumulated while it was down.
	s.scrubOnce(ctx)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			s.scrubOnce(ctx)
		}
	}
}

// ScrubOnce is exposed for tests + a future manual trigger (CLI
// flag / API endpoint). Returns the per-outcome counts.
type ScrubResult struct {
	Total     int
	OK        int
	Missing   int
	Mismatch  int
	Errors    int
}

func (s *Scrubber) ScrubOnce(ctx context.Context) ScrubResult {
	return s.scrubOnce(ctx)
}

func (s *Scrubber) scrubOnce(ctx context.Context) ScrubResult {
	var result ScrubResult
	if s == nil {
		return result
	}
	rows, err := s.store.ListBlobRefs(ctx, store.ListBlobRefsOptions{})
	if err != nil {
		if s.logger != nil {
			s.logger.Error("blob_scrub: list blob_refs failed", "err", err)
		}
		return result
	}
	for _, rec := range rows {
		select {
		case <-s.stopCh:
			return result
		case <-ctx.Done():
			return result
		default:
		}
		result.Total++
		outcome := s.scrubOne(ctx, rec)
		switch outcome {
		case scrubOK:
			result.OK++
		case scrubMissing:
			result.Missing++
		case scrubMismatch:
			result.Mismatch++
		case scrubError:
			result.Errors++
		}
	}
	if s.logger != nil {
		s.logger.Info("blob_scrub: pass complete",
			"total", result.Total,
			"ok", result.OK,
			"missing", result.Missing,
			"mismatch", result.Mismatch,
			"errors", result.Errors)
	}
	return result
}

type scrubOutcome int

const (
	scrubOK scrubOutcome = iota
	scrubMissing
	scrubMismatch
	scrubError
)

func (s *Scrubber) scrubOne(parentCtx context.Context, rec *store.BlobRefRecord) scrubOutcome {
	ctx, cancel := context.WithTimeout(parentCtx, scrubOpTimeout)
	defer cancel()

	scope, path, err := parseBlobURI(rec.URI)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("blob_scrub: malformed URI; skipped",
				"uri", rec.URI, "err", err)
		}
		return scrubError
	}
	f, _, err := s.blobs.Open(scope, path)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			if s.logger != nil {
				s.logger.Warn("blob_scrub: body missing for live row",
					"uri", rec.URI, "expected_sha256", rec.SHA256)
			}
			return scrubMissing
		}
		if s.logger != nil {
			s.logger.Error("blob_scrub: open failed",
				"uri", rec.URI, "err", err)
		}
		return scrubError
	}
	// Capture the file path while we still own the descriptor.
	// f.Name() returns the path passed to Open(), which is the
	// concrete on-disk path under the scope root. Close is
	// deferred AFTER the path snapshot so the rename path can
	// access it without re-resolving (Open might race a
	// concurrent Delete).
	openedPath := f.Name()

	h := sha256.New()
	// Cancellable copy so Stop() doesn't have to wait for a
	// large io.Copy to finish on a multi-GB blob.
	if _, err := copyCtx(ctx, h, f); err != nil {
		_ = f.Close()
		if s.logger != nil {
			s.logger.Error("blob_scrub: read failed",
				"uri", rec.URI, "err", err)
		}
		return scrubError
	}
	got := hex.EncodeToString(h.Sum(nil))
	_ = f.Close()

	if got != rec.SHA256 {
		// Concurrent Put race: a writer that landed between our
		// Open and the row's row.sha256 we captured at list time
		// would have replaced the body. Re-read the row; if its
		// sha256 has moved on, the file we hashed is no longer
		// authoritative — skip rather than quarantine the
		// already-replaced (and likely correct) body. Next pass
		// will catch any real corruption.
		fresh, freshErr := s.store.GetBlobRef(ctx, rec.URI)
		if freshErr == nil && fresh.SHA256 != rec.SHA256 {
			if s.logger != nil {
				s.logger.Info("blob_scrub: sha256 differs but row advanced under us; skipped",
					"uri", rec.URI,
					"hashed_against", rec.SHA256,
					"row_now", fresh.SHA256)
			}
			return scrubOK
		}
		qPath, qErr := s.quarantine(openedPath, rec.URI)
		if s.logger != nil {
			s.logger.Error("blob_scrub: sha256 mismatch; quarantined",
				"uri", rec.URI,
				"expected", rec.SHA256,
				"got", got,
				"quarantine_path", qPath,
				"quarantine_err", qErr)
		}
		return scrubMismatch
	}

	// OK — stamp last_seen_ok against the sha256 we just verified.
	// If a concurrent Put advanced the row's sha256 between our
	// hash and this UPDATE, the WHERE clause skips the row
	// (ErrNotFound) so we don't lie about having verified the new
	// body. Best-effort: any other DB error is logged and the
	// next pass will retry.
	if err := s.store.TouchBlobRefLastSeenOK(ctx, rec.URI, rec.SHA256, store.NowMillis()); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		if s.logger != nil {
			s.logger.Warn("blob_scrub: touch last_seen_ok failed",
				"uri", rec.URI, "err", err)
		}
	}
	return scrubOK
}

// copyCtx is a context-aware io.Copy: it polls ctx.Done() between
// each chunk so Stop() / shutdown can interrupt a multi-minute hash
// of a large blob. Returns ctx.Err() on cancellation.
func copyCtx(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 64*1024)
	var total int64
	for {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			w, werr := dst.Write(buf[:n])
			total += int64(w)
			if werr != nil {
				return total, werr
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// quarantine moves the corrupt file out of the blob root entirely.
// Returns the destination path + any rename error.
//
// Two reasons not to leave the `.corrupt.<ts>` sibling in the same
// scope dir:
//
//   - blob.Store.List would surface the corrupt file alongside
//     real bodies, and a curious caller might try to Open it.
//   - the on-disk layout under <root>/<scope>/... is treated as
//     "every leaf is a blob body"; corrupting that invariant
//     would break future tooling that walks the tree (e.g. the
//     v0 → v1 importer's catchall scan).
//
// Instead, the corrupt body is renamed to
// `<root>/.quarantine/<scope>/<path>.corrupt.<ts>`, with `<path>`
// preserved verbatim so an operator can correlate it back to the
// original URI. `.quarantine` starts with a dot so blob.List
// (which never traverses out of `<root>/<scope>`) cannot reach it
// even if a future bug accidentally registered `.quarantine` as a
// valid scope name.
//
// `fmt.Sprintf` is NOT used on the original path: a path with a
// literal `%` would be parsed as a format directive and corrupt
// the rename target. The timestamp is composed via string
// concatenation only.
func (s *Scrubber) quarantine(orig, uri string) (string, error) {
	scope, path, parseErr := parseBlobURI(uri)
	if parseErr != nil {
		// We managed to Open the file so the URI parsed once, but
		// be defensive — fall back to a flat name under
		// .quarantine if parse fails here.
		scope = "unknown"
		path = filepath.Base(orig)
	}
	root := s.blobs.BaseDir()
	if root == "" {
		return "", fmt.Errorf("blob.scrubber.quarantine: store root unset")
	}
	suffix := fmt.Sprintf(".corrupt.%d", store.NowMillis())
	target := filepath.Join(root, ".quarantine", string(scope), path+suffix)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return target, fmt.Errorf("blob.scrubber.quarantine: ensure dir: %w", err)
	}
	if err := os.Rename(orig, target); err != nil {
		return target, fmt.Errorf("blob.scrubber.quarantine: rename %s -> %s: %w",
			orig, target, err)
	}
	return target, nil
}

// ParseURI parses `kojo://<scope>/<path>` back into the typed
// scope + path expected by Store.Open / Head. Mirrors BuildURI:
// each path segment between `/` separators is url.PathEscape-
// encoded by the producer, so PathUnescape is applied here per
// segment. Returns an error on missing scheme, missing scope,
// empty path, or invalid percent-encoding.
//
// Exposed alongside BuildURI so the device-switch handoff fetch
// (server-side) can use the same canonical decoder the scrubber
// uses.
func ParseURI(uri string) (Scope, string, error) {
	return parseBlobURI(uri)
}

// parseBlobURI is the internal implementation. ParseURI is the
// exported alias kept here so external callers don't need to
// chase the rename.
func parseBlobURI(uri string) (Scope, string, error) {
	const scheme = "kojo://"
	if !strings.HasPrefix(uri, scheme) {
		return "", "", fmt.Errorf("missing %q prefix", scheme)
	}
	rest := uri[len(scheme):]
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return "", "", fmt.Errorf("missing path component")
	}
	sc := Scope(rest[:slash])
	if !sc.Valid() {
		return "", "", fmt.Errorf("invalid scope %q", string(sc))
	}
	encPath := rest[slash+1:]
	if encPath == "" {
		return "", "", fmt.Errorf("empty path")
	}
	parts := strings.Split(encPath, "/")
	for i, p := range parts {
		dec, err := url.PathUnescape(p)
		if err != nil {
			return "", "", fmt.Errorf("invalid percent-encoding in segment %d: %w", i, err)
		}
		parts[i] = dec
	}
	return sc, strings.Join(parts, "/"), nil
}

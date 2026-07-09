package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// ErrApplyInFlight is returned by Apply when another Apply is already
// downloading or swapping. Callers should retry later rather than
// start a second install race.
var ErrApplyInFlight = errors.New("selfupdate: update already in progress")

// ErrUpToDate is returned by Apply when CheckNow finds no newer
// release. Distinct from a fetch error so the UI can say "already
// current" without treating it as failure.
var ErrUpToDate = errors.New("selfupdate: already up to date")

// Status is a point-in-time snapshot of the update check. Zero value
// means no successful check has completed yet (CheckedAt is zero).
type Status struct {
	Current         string
	Latest          string
	NotesURL        string
	UpdateAvailable bool
	CheckedAt       time.Time
}

// Checker polls GitHub Releases for a newer binary and can apply it
// in place. One Checker per process; StartLoop and Apply share last
// Status under mu, and Apply is single-flight so two UI clicks cannot
// race two swaps.
type Checker struct {
	client  *Client
	current string
	logger  *slog.Logger

	mu       sync.Mutex
	last     Status
	notified map[string]struct{}
	applying bool
}

// NewChecker builds a Checker for currentVersion. logger may be nil
// (falls back to slog.Default). client must be non-nil.
func NewChecker(client *Client, currentVersion string, logger *slog.Logger) *Checker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Checker{
		client:   client,
		current:  currentVersion,
		logger:   logger,
		notified: make(map[string]struct{}),
	}
}

// CheckNow fetches the latest release and updates the stored Status.
// On fetch failure the previous good Status is left untouched and the
// error is returned. The first time a given newer tag is observed an
// Info log is emitted; subsequent sightings of the same tag log at
// Debug so a 6-hour poll does not spam the journal.
func (ch *Checker) CheckNow(ctx context.Context) (Status, error) {
	if ch == nil || ch.client == nil {
		return Status{}, fmt.Errorf("nil selfupdate checker")
	}
	rel, err := ch.client.LatestRelease(ctx)
	if err != nil {
		return Status{}, err
	}
	st := Status{
		Current:         ch.current,
		Latest:          rel.TagName,
		NotesURL:        rel.HTMLURL,
		UpdateAvailable: IsNewer(rel.TagName, ch.current),
		CheckedAt:       time.Now(),
	}

	ch.mu.Lock()
	ch.last = st
	firstNotify := false
	if st.UpdateAvailable {
		if _, seen := ch.notified[rel.TagName]; !seen {
			ch.notified[rel.TagName] = struct{}{}
			firstNotify = true
		}
	}
	ch.mu.Unlock()

	if st.UpdateAvailable {
		if firstNotify {
			ch.logger.Info("kojo update available",
				"current", ch.current,
				"latest", rel.TagName,
				"notes", rel.HTMLURL,
				"hint", "run 'kojo update' or update from the web UI")
		} else {
			ch.logger.Debug("kojo update available",
				"current", ch.current,
				"latest", rel.TagName,
				"notes", rel.HTMLURL)
		}
	}
	return st, nil
}

// Status returns a copy of the last successful check snapshot.
func (ch *Checker) Status() Status {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.last
}

// StartLoop runs CheckNow periodically in a background goroutine.
// initialDelay waits before the first check (so boot is not blocked
// on GitHub); interval is the steady-state period. Each tick uses a
// 30s-derived context so a hung request cannot outlive the next tick
// by much. Network errors log at Debug — flaps are routine.
//
// Modeled on the select/ticker loops in cmd/kojo (peer subscriber
// poll and friends): exit cleanly on ctx.Done().
func (ch *Checker) StartLoop(ctx context.Context, initialDelay, interval time.Duration) {
	go func() {
		if initialDelay > 0 {
			t := time.NewTimer(initialDelay)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
		ch.loopTick(ctx)

		if interval <= 0 {
			return
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ch.loopTick(ctx)
			}
		}
	}()
}

func (ch *Checker) loopTick(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	if _, err := ch.CheckNow(ctx); err != nil {
		ch.logger.Debug("selfupdate check failed", "err", err)
	}
}

// Apply fetches the latest release once, and when it is newer downloads
// THAT release's platform archive into the executable's directory and
// swaps it in place. A single LatestRelease call drives the IsNewer
// decision, the download, the returned Status, and last-status/notify
// bookkeeping — no second fetch that could disagree (TOCTOU). Concurrent
// Apply calls fail with ErrApplyInFlight. The caller decides when to
// restart into the new binary.
func (ch *Checker) Apply(ctx context.Context) (Status, error) {
	if ch == nil || ch.client == nil {
		return Status{}, fmt.Errorf("nil selfupdate checker")
	}

	ch.mu.Lock()
	if ch.applying {
		ch.mu.Unlock()
		return Status{}, ErrApplyInFlight
	}
	ch.applying = true
	ch.mu.Unlock()
	defer func() {
		ch.mu.Lock()
		ch.applying = false
		ch.mu.Unlock()
	}()

	// One fetch only: evaluate, download, and report from the same Release.
	rel, err := ch.client.LatestRelease(ctx)
	if err != nil {
		return Status{}, err
	}
	st := Status{
		Current:         ch.current,
		Latest:          rel.TagName,
		NotesURL:        rel.HTMLURL,
		UpdateAvailable: IsNewer(rel.TagName, ch.current),
		CheckedAt:       time.Now(),
	}

	ch.mu.Lock()
	ch.last = st
	firstNotify := false
	if st.UpdateAvailable {
		if _, seen := ch.notified[rel.TagName]; !seen {
			ch.notified[rel.TagName] = struct{}{}
			firstNotify = true
		}
	}
	ch.mu.Unlock()

	if st.UpdateAvailable {
		if firstNotify {
			ch.logger.Info("kojo update available",
				"current", ch.current,
				"latest", rel.TagName,
				"notes", rel.HTMLURL,
				"hint", "run 'kojo update' or update from the web UI")
		} else {
			ch.logger.Debug("kojo update available",
				"current", ch.current,
				"latest", rel.TagName,
				"notes", rel.HTMLURL)
		}
	}
	if !st.UpdateAvailable {
		return st, ErrUpToDate
	}

	exe, err := resolveExecPath()
	if err != nil {
		return Status{}, fmt.Errorf("resolve executable: %w", err)
	}
	// destDir must be on the same filesystem as the final rename —
	// never os.TempDir(), which is often a different volume.
	destDir := filepath.Dir(exe)

	bin, err := ch.client.DownloadAndExtract(ctx, rel, runtime.GOOS, runtime.GOARCH, destDir)
	if err != nil {
		return Status{}, err
	}
	// After a successful Unix swap the extracted file remains (swap
	// copies); on Windows rename may consume it. Always best-effort
	// remove so Apply does not litter destDir.
	defer os.Remove(bin)

	if err := SwapExecutable(bin); err != nil {
		return Status{}, err
	}
	return st, nil
}

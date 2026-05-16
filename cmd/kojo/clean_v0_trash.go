package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/loppo-llc/kojo/internal/configdir"
)

// trashDirRe matches the soft-delete sibling dir name format produced
// by clean_v0.go's applyV0CleanPlan: `kojo.deleted-<unix-millis>`.
// Anything else in the v0 parent dir (the live `kojo` / `kojo-v1` /
// unrelated user data) is left untouched. Underscored as a package
// var so a future operator-facing flag (e.g. --clean-trash-pattern)
// can swap it without rewriting the discovery loop.
var trashDirRe = regexp.MustCompile(`^kojo\.deleted-(\d+)$`)

// v0TrashEntry is one kojo.deleted-<ts>/ candidate.
type v0TrashEntry struct {
	Path  string    // absolute path
	Stamp time.Time // parsed from the dir name
	Age   time.Duration
}

// v0TrashCleanPlan lists trash dirs eligible for physical deletion.
// Reasons are recorded per-entry so the dry-run printout explains
// why a dir is or isn't being purged.
type v0TrashCleanPlan struct {
	// Purge: dirs that pass the age filter and will be removed on
	// --clean-apply.
	Purge []v0TrashEntry
	// KeepYoung: dirs younger than --clean-min-age-days, retained
	// to preserve the operator's recovery window. Reported so the
	// dry-run shows what would later age in.
	KeepYoung []v0TrashEntry
	// Anomalies: entries whose name matched the trash pattern but
	// otherwise failed safety checks — non-directory (regular file
	// at that path), symlink (something other than our soft-delete
	// produced it), or unparseable timestamp suffix (wraparound /
	// manual edit / numeric overflow). Reported but never
	// auto-purged.
	Anomalies []v0TrashEntry
}

// planV0TrashCleanup discovers kojo.deleted-<ts>/ siblings of the v0
// path. The v0 dir itself does NOT have to exist — operators may
// have already wiped it manually after a soft-delete and now just
// want to free disk. minAgeDays selects how old (by name-stamp) a
// trash dir must be to qualify for purge; 0 means "any age". Negative
// values are treated as 0 to match the rest of the clean machinery.
func planV0TrashCleanup(minAgeDays int, logger *slog.Logger) (*v0TrashCleanPlan, error) {
	v0 := configdir.V0Path()
	if v0PathOverride != nil {
		v0 = v0PathOverride()
	}
	parent := filepath.Dir(v0)
	if minAgeDays < 0 {
		minAgeDays = 0
	}
	cutoff := time.Now().UTC().Add(-time.Duration(minAgeDays) * 24 * time.Hour)

	plan := &v0TrashCleanPlan{}
	entries, err := os.ReadDir(parent)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return plan, nil
		}
		return nil, fmt.Errorf("readdir trash parent %s: %w", parent, err)
	}
	for _, e := range entries {
		// Match the trash pattern. We deliberately do NOT EvalSymlinks
		// here — clean_v0.go refuses to rename symlinks, so a symlink
		// matching this pattern was created by something other than
		// our soft-delete and is treated as an anomaly.
		m := trashDirRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		path := filepath.Join(parent, e.Name())
		// Reject non-dirs / symlinks outright. Lstat (not Stat) so a
		// symlink trash entry is detected even if its target dir
		// exists.
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("lstat %s: %w", path, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			plan.Anomalies = append(plan.Anomalies, v0TrashEntry{Path: path})
			continue
		}
		// Parse the timestamp from the dir name (more reliable than
		// mtime on copy-restored hosts that lose mtimes).
		ms, parseErr := strconv.ParseInt(m[1], 10, 64)
		if parseErr != nil {
			plan.Anomalies = append(plan.Anomalies, v0TrashEntry{Path: path})
			continue
		}
		stamp := time.UnixMilli(ms).UTC()
		entry := v0TrashEntry{
			Path:  path,
			Stamp: stamp,
			Age:   time.Since(stamp),
		}
		if stamp.After(cutoff) {
			plan.KeepYoung = append(plan.KeepYoung, entry)
		} else {
			plan.Purge = append(plan.Purge, entry)
		}
	}
	// Stable order by path so re-runs print identically.
	sort.Slice(plan.Purge, func(i, j int) bool { return plan.Purge[i].Path < plan.Purge[j].Path })
	sort.Slice(plan.KeepYoung, func(i, j int) bool { return plan.KeepYoung[i].Path < plan.KeepYoung[j].Path })
	sort.Slice(plan.Anomalies, func(i, j int) bool { return plan.Anomalies[i].Path < plan.Anomalies[j].Path })
	if logger != nil && len(plan.Anomalies) > 0 {
		logger.Warn("v0 trash scan saw anomalous entries (left untouched)",
			"count", len(plan.Anomalies))
	}
	return plan, nil
}

// printV0TrashCleanPlan reports the plan in the same dry-run shape
// the snapshot/legacy/v0 targets use. apply=false produces "would
// remove"; apply=true produces "removing".
func printV0TrashCleanPlan(plan *v0TrashCleanPlan, apply bool) {
	verb := "would remove"
	if apply {
		verb = "removing"
	}
	if n := len(plan.Purge); n > 0 {
		fmt.Fprintf(os.Stderr, "%s %d v0 trash dir(s):\n", verb, n)
		for _, e := range plan.Purge {
			fmt.Fprintf(os.Stderr, "  %s  (stamp=%s, age=%s)\n",
				e.Path, e.Stamp.Format(time.RFC3339), e.Age.Round(time.Second))
		}
	}
	if n := len(plan.KeepYoung); n > 0 {
		fmt.Fprintf(os.Stderr, "keeping %d trash dir(s) younger than --clean-min-age-days:\n", n)
		for _, e := range plan.KeepYoung {
			fmt.Fprintf(os.Stderr, "  %s  (stamp=%s, age=%s)\n",
				e.Path, e.Stamp.Format(time.RFC3339), e.Age.Round(time.Second))
		}
	}
	if n := len(plan.Anomalies); n > 0 {
		fmt.Fprintf(os.Stderr, "skipping %d anomalous trash entry/entries (non-dir, symlink, unparseable stamp):\n", n)
		for _, e := range plan.Anomalies {
			fmt.Fprintf(os.Stderr, "  %s\n", e.Path)
		}
	}
	if len(plan.Purge)+len(plan.KeepYoung)+len(plan.Anomalies) == 0 {
		fmt.Fprintln(os.Stderr, "no v0 trash dirs to clean")
	}
}

// applyV0TrashCleanPlan physically removes the planned entries via
// os.RemoveAll. Each failure is collected; the loop never aborts
// early so an unreadable entry doesn't strand the rest. Anomalies
// are NEVER touched.
//
// Per-entry re-validation: between scan and apply the operator may
// have replaced an entry with a symlink, swapped it for a regular
// file, or recreated it with a fresh timestamp. Each candidate is
// re-Lstat'd + name-checked + stamp-re-parsed + stamp-equality
// (must match scan-time Stamp) + future guard (stamp must still be
// in the past) right before RemoveAll. The min-age cutoff itself
// is NOT recomputed: time only moves forward, so any entry that
// satisfied the cutoff at scan time still satisfies it at apply
// time, AS LONG AS its stamp is unchanged (the equality check
// catches a rename to a fresher stamp). Drift moves the entry to
// the "skipped" error list rather than purging the new state.
func applyV0TrashCleanPlan(plan *v0TrashCleanPlan, logger *slog.Logger) []error {
	if plan == nil {
		return nil
	}
	// Time only moves forward, so every Purge entry stays past the
	// scan-time age cutoff as long as its stamp has not been
	// replaced by a future-stamped impostor between scan and apply.
	// We re-check `freshStamp.After(now)` per entry below to catch
	// that case; no separate cutoff reconstruction is needed.
	now := time.Now().UTC()

	var errs []error
	for _, e := range plan.Purge {
		// Re-Lstat: catches replacement by symlink / regular file
		// in the scan→apply window.
		info, lerr := os.Lstat(e.Path)
		if lerr != nil {
			if errors.Is(lerr, fs.ErrNotExist) {
				// Already gone (operator manually rm'd it). Skip,
				// not an error.
				continue
			}
			errs = append(errs, fmt.Errorf("re-lstat %s: %w", e.Path, lerr))
			continue
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			errs = append(errs, fmt.Errorf("skipping %s: type changed since scan (mode=%v)", e.Path, info.Mode()))
			continue
		}
		// Re-parse the name + stamp. A path like /tmp/xxx/kojo.deleted-1700000
		// was matched at scan time; we re-derive the stamp now to
		// confirm the operator did not somehow rename the dir to a
		// future-dated stamp during the dry-run.
		base := filepath.Base(e.Path)
		m := trashDirRe.FindStringSubmatch(base)
		if m == nil {
			errs = append(errs, fmt.Errorf("skipping %s: name no longer matches trash pattern", e.Path))
			continue
		}
		ms, perr := strconv.ParseInt(m[1], 10, 64)
		if perr != nil {
			errs = append(errs, fmt.Errorf("skipping %s: re-parse stamp %v", e.Path, perr))
			continue
		}
		freshStamp := time.UnixMilli(ms).UTC()
		if !freshStamp.Equal(e.Stamp) {
			// Stamp on disk drifted (renamed). Refuse — the new
			// stamp's age may not satisfy the operator's filter.
			errs = append(errs, fmt.Errorf("skipping %s: stamp drifted from %s to %s", e.Path, e.Stamp, freshStamp))
			continue
		}
		// Final invariant: the entry's stamp must still be in the
		// past relative to now. (Time-travel by the operator is
		// the only way this fails; we still defend against it.)
		if freshStamp.After(now) {
			errs = append(errs, fmt.Errorf("skipping %s: stamp %s is in the future", e.Path, freshStamp))
			continue
		}
		if err := os.RemoveAll(e.Path); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", e.Path, err))
			continue
		}
		if logger != nil {
			logger.Info("v0 trash dir purged", "path", e.Path, "stamp", e.Stamp)
		}
	}
	return errs
}

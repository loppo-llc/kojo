package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/configdir"
	"github.com/loppo-llc/kojo/internal/snapshot"
	"github.com/loppo-llc/kojo/internal/store"
)

// cleanFlags carries the parsed `--clean*` flag values from main.go.
type cleanFlags struct {
	target        string // "snapshots" | "legacy" | "v0" | "v0-trash" | "all"
	apply         bool
	maxAgeDays    int
	keepLatest    int
	force         bool // --clean-force; only consulted by the v0 target
	minAgeDays    int  // --clean-min-age-days; only consulted by the v0-trash target
	logger        *slog.Logger
	configDirPath string
}

// runCleanCommand drives Phase 6 #18's `kojo --clean ...` housekeeping.
//
// The default mode is DRY-RUN: planned removals are printed but no
// files are touched. Operators add `--clean-apply` to commit. This
// matches the convention of other destructive admin tools (`docker
// system prune`, `gh repo gc`) — the safer default is a preview.
//
// Targets implemented today:
//
//   - "snapshots": drops <configdir>/snapshots/<TS>/ entries that are
//     either (a) older than --clean-max-age-days (default 7) AND not
//     among the --clean-keep-latest most-recent (default 3), or
//     (b) have no manifest.json (a partial / abandoned snapshot
//     dir that snapshot.Take() left behind on a failure)
//
//   - "legacy": drops post-Phase-2c-2 legacy on-disk files
//     (cron_paused, .cron_last, autosummary_marker, owner.token,
//     agent_tokens/<id>) ONLY when their canonical kv row exists.
//     Files without a kv mirror are reported but kept so the
//     runtime's lazy migration can still pick them up. See
//     clean_legacy.go for the inventory and the safety gate.
//
//   - "v0": soft-deletes the entire v0 dir (post-migration rollback
//     fallback) by renaming it to a sibling kojo.deleted-<ts>/.
//     Refuses without migration_complete.json, on manifest
//     divergence (overridable with --clean-force), on a running v0
//     binary, on symlinks, on non-dirs, and on trash-path
//     collisions. See clean_v0.go for the gate inventory. NOT
//     folded into "all" because v0 is the only destructive target
//     and operators must opt in explicitly.
//
//   - "v0-trash": physically removes kojo.deleted-<ts>/ siblings
//     produced by the "v0" target (slice 30). --clean-min-age-days
//     filters by the timestamp encoded in the dir name (CLI
//     default 7; pass --clean-min-age-days=0 explicitly to remove
//     every age, which defeats the recovery window). Anomalous
//     entries — non-dirs, symlinks, unparseable timestamps —
//     are reported but never auto-purged. Future-dated stamps land
//     in "KeepYoung" (after the cutoff) and are also rejected by
//     an apply-time future guard. Also NOT folded into "all"
//     because the trash dirs ARE the soft-delete recovery window.
//
//   - "all": runs "snapshots" + "legacy". Intentionally excludes
//     "v0" and "v0-trash" (see above).
//
// Future slices add "blobs" (orphan blob files, GC-marked refs) and
// "agents" (hard-delete soft-deleted agents past grace), each isolated
// behind its own target so an operator can opt in piecewise.
//
// Exit codes:
//
//	0 — nothing to do or apply succeeded
//	1 — error during scan / apply
//	2 — invalid flag combination
func runCleanCommand(f cleanFlags) int {
	if f.configDirPath == "" {
		f.configDirPath = configdir.Path()
	}
	if f.logger == nil {
		f.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	if f.maxAgeDays <= 0 {
		f.maxAgeDays = 7
	}
	if f.keepLatest < 0 {
		f.keepLatest = 0
	}

	switch f.target {
	case "", "all", "snapshots", "legacy", "v0", "v0-trash":
		// fall through
	default:
		fmt.Fprintf(os.Stderr, "kojo: --clean target %q not recognized (try 'snapshots', 'legacy', 'v0', 'v0-trash', or 'all')\n", f.target)
		return 2
	}

	runSnapshots := f.target == "" || f.target == "all" || f.target == "snapshots"
	runLegacy := f.target == "all" || f.target == "legacy"
	// v0 is intentionally NOT folded into "all". It is the only
	// destructive target that touches the v0 rollback dir, and an
	// operator running periodic `--clean all` snapshot/legacy
	// housekeeping should never lose their v0 fallback by accident.
	// Require explicit `--clean v0`.
	runV0 := f.target == "v0"
	// v0-trash is also explicit-only. The trash dirs ARE the
	// recovery window the soft-delete pattern preserves; folding
	// them into `all` would defeat the design. The CLI default
	// `--clean-min-age-days=7` keeps the same 7-day window the
	// §5.8 design specified; operators who want to purge every
	// age must pass `--clean-min-age-days=0` explicitly.
	runV0Trash := f.target == "v0-trash"

	var snapshotPlan *cleanPlan
	if runSnapshots {
		p, err := planSnapshotCleanup(f)
		if err != nil {
			f.logger.Error("clean: snapshot scan failed", "err", err)
			return 1
		}
		snapshotPlan = p
		printCleanPlan(snapshotPlan, f.apply)
	}

	var (
		legacyPlan *legacyCleanPlan
		legacyKV   *store.Store
	)
	if runLegacy {
		// The legacy target needs a kv handle. Open kojo.db read-only:
		// SQLite WAL admits multiple readers alongside a live writer,
		// so this co-exists with a running kojo. ReadOnly skips
		// migrations as well — clean must never silently bump the
		// schema version. The handle is held across both scan and
		// apply so apply-time re-validation sees the same connection.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		st, err := store.Open(ctx, store.Options{ConfigDir: f.configDirPath, ReadOnly: true})
		if err != nil {
			f.logger.Error("clean: open kv store (read-only) failed", "err", err)
			return 1
		}
		defer st.Close()
		legacyKV = st
		p, err := planLegacyCleanup(ctx, st, f.configDirPath)
		if err != nil {
			f.logger.Error("clean: legacy scan failed", "err", err)
			return 1
		}
		legacyPlan = p
		printLegacyCleanPlan(legacyPlan, f.apply)
	}

	var v0TrashPlan *v0TrashCleanPlan
	if runV0Trash {
		p, err := planV0TrashCleanup(f.minAgeDays, f.logger)
		if err != nil {
			f.logger.Error("clean: v0-trash scan failed", "err", err)
			return 1
		}
		v0TrashPlan = p
		printV0TrashCleanPlan(v0TrashPlan, f.apply)
	}

	var v0Plan *v0CleanPlan
	if runV0 {
		p, err := planV0Cleanup(f.configDirPath, f.logger)
		if err != nil {
			f.logger.Error("clean: v0 scan failed", "err", err)
			return 1
		}
		// --clean-force only matters when the plan flagged a
		// ForceableReason (currently only manifest divergence is
		// overridable; every other PartialReason — non-dir,
		// symlink, lock held, missing/mismatched complete file —
		// fails closed regardless of --force; trash-path
		// collision is a separate apply-time error, not a
		// PartialReason). Recording ForceUsed here keeps the
		// dry-run printout honest.
		if p != nil && f.force {
			p.ForceUsed = true
		}
		v0Plan = p
		printV0CleanPlan(v0Plan, f.apply)
	}

	if !f.apply {
		fmt.Fprintln(os.Stderr, "kojo: dry-run; pass --clean-apply to delete the listed entries")
		return 0
	}

	rc := 0
	if snapshotPlan != nil {
		if errs := applyCleanPlan(snapshotPlan); len(errs) > 0 {
			for _, e := range errs {
				f.logger.Error("clean: remove snapshot failed", "err", e)
			}
			rc = 1
		}
	}
	if legacyPlan != nil {
		if errs := applyLegacyCleanPlan(legacyPlan, legacyKV); len(errs) > 0 {
			for _, e := range errs {
				f.logger.Error("clean: remove legacy file failed", "err", e)
			}
			rc = 1
		}
	}
	if v0Plan != nil {
		if err := applyV0CleanPlan(v0Plan, f.logger); err != nil {
			f.logger.Error("clean: v0 soft-delete failed", "err", err)
			rc = 1
		}
	}
	if v0TrashPlan != nil {
		if errs := applyV0TrashCleanPlan(v0TrashPlan, f.logger); len(errs) > 0 {
			for _, e := range errs {
				f.logger.Error("clean: remove v0 trash dir failed", "err", e)
			}
			rc = 1
		}
	}
	return rc
}

// snapshotEntry is one candidate snapshot directory considered by the
// cleanup pass. PartialReason is non-empty when the directory has no
// manifest (corrupt / abandoned).
type snapshotEntry struct {
	Path          string
	ModTime       time.Time
	PartialReason string
}

// cleanPlan is the result of a scan: a list of paths that the apply
// step would remove. Categorized so the printout can explain why
// each entry is on the list.
type cleanPlan struct {
	StaleSnapshots   []snapshotEntry
	PartialSnapshots []snapshotEntry
	Kept             []snapshotEntry
}

func planSnapshotCleanup(f cleanFlags) (*cleanPlan, error) {
	dir := filepath.Join(f.configDirPath, "snapshots")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &cleanPlan{}, nil // nothing to do
		}
		return nil, err
	}

	var all []snapshotEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		entry := snapshotEntry{Path: path, ModTime: info.ModTime()}
		// A snapshot is "complete" iff it has a parsable manifest.
		// We use snapshot.LoadManifest so the validation logic stays
		// in one place — version-bumps that change manifest shape only
		// need to update the package.
		if _, mErr := snapshot.LoadManifest(path); mErr != nil {
			entry.PartialReason = mErr.Error()
		}
		all = append(all, entry)
	}

	// Sort newest-first so the keep-latest cut is easy.
	sort.Slice(all, func(i, j int) bool { return all[i].ModTime.After(all[j].ModTime) })

	plan := &cleanPlan{}
	cutoff := time.Now().Add(-time.Duration(f.maxAgeDays) * 24 * time.Hour)

	for i, e := range all {
		switch {
		case e.PartialReason != "":
			plan.PartialSnapshots = append(plan.PartialSnapshots, e)
		case i < f.keepLatest:
			// Always keep the N most-recent successful snapshots,
			// regardless of age. Operators rely on at least 1
			// reachable snapshot at all times.
			plan.Kept = append(plan.Kept, e)
		case e.ModTime.Before(cutoff):
			plan.StaleSnapshots = append(plan.StaleSnapshots, e)
		default:
			plan.Kept = append(plan.Kept, e)
		}
	}
	return plan, nil
}

func printCleanPlan(plan *cleanPlan, apply bool) {
	verb := "would remove"
	if apply {
		verb = "removing"
	}
	if n := len(plan.PartialSnapshots); n > 0 {
		fmt.Fprintf(os.Stderr, "%s %d partial snapshot dir(s):\n", verb, n)
		for _, e := range plan.PartialSnapshots {
			fmt.Fprintf(os.Stderr, "  %s  (no manifest: %s)\n", e.Path, shortReason(e.PartialReason))
		}
	}
	if n := len(plan.StaleSnapshots); n > 0 {
		fmt.Fprintf(os.Stderr, "%s %d stale snapshot dir(s):\n", verb, n)
		for _, e := range plan.StaleSnapshots {
			fmt.Fprintf(os.Stderr, "  %s  (mtime=%s)\n", e.Path, e.ModTime.UTC().Format(time.RFC3339))
		}
	}
	if n := len(plan.Kept); n > 0 {
		fmt.Fprintf(os.Stderr, "keeping %d snapshot dir(s)\n", n)
	}
	if len(plan.PartialSnapshots)+len(plan.StaleSnapshots) == 0 {
		fmt.Fprintln(os.Stderr, "no snapshot cleanup needed")
	}
}

func applyCleanPlan(plan *cleanPlan) []error {
	var errs []error
	for _, e := range append(plan.PartialSnapshots, plan.StaleSnapshots...) {
		if err := os.RemoveAll(e.Path); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", e.Path, err))
		}
	}
	return errs
}

// shortReason trims a manifest error to one line so the dry-run output
// stays scannable.
func shortReason(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

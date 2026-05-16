package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/loppo-llc/kojo/internal/configdir"
	"github.com/loppo-llc/kojo/internal/migrate"
)

// v0CleanPlan is the result of scanning for the `--clean v0` target.
// PartialReason is non-empty when something blocks apply;
// ForceableReason is non-empty ONLY when the block is something
// --clean-force can override (currently: manifest divergence).
// Other PartialReasons (symlink v0, non-dir v0, running v0 binary,
// lock probe failure, missing/parse-failed migration_complete.json,
// empty v0_sha256_manifest, v0_path mismatch) are NEVER
// override-able — those represent operator error or unsafe state,
// not a v0-edited-after-migration choice. Trash-path collision is
// NOT modeled as PartialReason; it is detected at apply time as a
// hard error.
type v0CleanPlan struct {
	V0Path        string // absolute path of the v0 dir to remove (empty = nothing to do)
	TrashPath     string // destination of the soft-delete rename
	Reason        string // why this plan exists (informational only)
	ConfigDirPath string // v1 dir resolved at planning time; threaded back into apply for the fresh re-plan
	// PartialReason: any block. Apply refuses unless this is also
	// the same string as ForceableReason AND ForceUsed is true.
	PartialReason string
	// ForceableReason: subset of PartialReason that --clean-force
	// is allowed to override. When set, applies the same string as
	// PartialReason; otherwise empty.
	ForceableReason string
	// ForceUsed records whether --clean-force was honoured. Apply
	// only consults this when ForceableReason is also non-empty.
	ForceUsed bool
}

// v0PathOverride lets tests pin the v0 path without touching the
// process-wide HOME / APPDATA env, which other parallel tests may
// rely on. Production callers leave it nil; planV0Cleanup falls
// back to configdir.V0Path() which honours the platform defaults.
var v0PathOverride func() string

// planV0Cleanup decides whether the v0 dir is safe to soft-delete
// (rename to <home>/.config/kojo.deleted-<ts>/, sibling of the v0
// path so we never cross a filesystem boundary mid-rename).
//
// Safety order — surface the first failure as PartialReason so the
// dry-run printout explains exactly what's blocking apply. Only
// step 7 (manifest divergence) is force-overridable; every other
// gate fails closed.
//
//  1. v0 dir absent → nothing to do, V0Path empty.
//  2. v0 path is a symlink → refuse (NEVER force-able). configdir.V0Path
//     resolves to a fixed conventional location; a symlink there is
//     either operator confusion or hostile, and EvalSymlinks-style
//     handling would let the rename move the symlink target instead
//     of the v0 dir, which is not what the operator asked for.
//  3. v0 path is not a directory → refuse (NEVER force-able).
//  4. v0 binary holds its lock (configdir.Probe) → refuse (NEVER
//     force-able). Renaming the live v0 dir under a running v0
//     binary would corrupt that binary's open files.
//  5. v1 migration_complete.json missing / parse-failed / empty
//     manifest → refuse (NEVER force-able). Without a recorded
//     manifest we cannot tell whether v0 was migrated.
//  6. cf.V0Path mismatch — migration_complete.json records a v0
//     path that does not match the cleanup target → refuse (NEVER
//     force-able). Catches multi-config-dir mix-ups. An empty
//     cf.V0Path is tolerated for older / malformed
//     migration_complete.json compatibility (the field has been
//     written since the original migrate package landed, but a
//     hand-edited or third-party-tool-produced file might omit it).
//  7. v0 manifest diverges from migration_complete.json's recorded
//     value → block, but THIS is the case --clean-force overrides.
//     Operator has acknowledged v0 was edited after migration.
//
// The actual rename is deferred to applyV0CleanPlan; this function
// is read-only against both v0 and v1 dirs (other than running
// content-hashing of v0 file bodies for the manifest comparison).
func planV0Cleanup(configDirPath string, logger *slog.Logger) (*v0CleanPlan, error) {
	plan := &v0CleanPlan{}
	v0 := configdir.V0Path()
	if v0PathOverride != nil {
		v0 = v0PathOverride()
	}
	v1 := configDirPath
	if v1 == "" {
		v1 = configdir.V1Path()
	}
	plan.ConfigDirPath = v1

	// 1. Is there even a v0 dir? Use Lstat so a symlinked v0 path
	//    is detected here rather than silently dereferenced.
	info, err := os.Lstat(v0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			plan.Reason = "v0 dir absent — nothing to clean"
			return plan, nil
		}
		return nil, fmt.Errorf("lstat v0 dir %s: %w", v0, err)
	}
	// 2. Symlink v0 path — check BEFORE IsDir, because lstat on a
	//    symlink reports mode bits with ModeSymlink set and IsDir
	//    false, so the non-dir branch would otherwise eat this
	//    case with a less-specific reason. Renaming a symlink moves
	//    the link, not the target — operator probably did not mean
	//    that. Refuse.
	if info.Mode()&os.ModeSymlink != 0 {
		plan.V0Path = v0
		plan.PartialReason = fmt.Sprintf("v0 path %s is a symlink; refusing to rename (resolve manually if intentional)", v0)
		return plan, nil
	}
	// 3. Non-symlink, non-dir v0 path. Surface V0Path so the
	//    printout can show operator what's there, but never
	//    force-able.
	if !info.IsDir() {
		plan.V0Path = v0
		plan.PartialReason = fmt.Sprintf("v0 path %s is not a directory (mode=%v); refusing to touch it", v0, info.Mode())
		return plan, nil
	}
	plan.V0Path = v0
	plan.TrashPath = filepath.Join(filepath.Dir(v0), fmt.Sprintf("kojo.deleted-%d", time.Now().UTC().UnixMilli()))

	// 4. v0 binary running under that dir? configdir.Probe opens v0
	//    O_RDONLY and tries flock(LOCK_EX|LOCK_NB); held=true means a
	//    v0 process holds the lock right now. Renaming v0 out from
	//    under it would crash the v0 binary.
	if held, perr := configdir.Probe(v0); perr != nil {
		// Probe failure is itself unsafe state — refuse.
		plan.PartialReason = fmt.Sprintf("v0 lock probe failed: %v", perr)
		return plan, nil
	} else if held {
		plan.PartialReason = fmt.Sprintf("v0 binary appears to be running (lock held on %s); stop it before cleanup", v0)
		return plan, nil
	}

	// 5. v1 migration_complete.json must exist + parse + carry a
	//    manifest. Otherwise we cannot establish whether v0 was
	//    migrated, much less whether it has been edited since.
	completePath := filepath.Join(v1, migrate.CompleteFileName)
	body, err := os.ReadFile(completePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			plan.PartialReason = fmt.Sprintf("v1 %s missing; migration not complete (run --migrate first)", migrate.CompleteFileName)
			return plan, nil
		}
		return nil, fmt.Errorf("read %s: %w", completePath, err)
	}
	var cf migrate.CompleteFile
	if err := json.Unmarshal(body, &cf); err != nil {
		plan.PartialReason = fmt.Sprintf("v1 %s parse failed: %v", migrate.CompleteFileName, err)
		return plan, nil
	}
	if cf.V0SHA256Manifest == "" {
		plan.PartialReason = fmt.Sprintf("v1 %s has empty v0_sha256_manifest", migrate.CompleteFileName)
		return plan, nil
	}
	// Cross-check the v0 path the migration recorded: if it points
	// at a different dir than what we're about to clean, refuse.
	// Catches the case where an operator copied a v1 dir from
	// another machine (or a different v0 layout) and the manifest
	// happens to match by coincidence. Empty cf.V0Path is tolerated
	// for older / malformed migration_complete.json compatibility
	// (the field has been written since the original migrate package
	// landed, but a hand-edited or third-party-tool-produced file
	// might omit it). The manifest gate still protects those cases.
	if cf.V0Path != "" && cf.V0Path != v0 {
		plan.PartialReason = fmt.Sprintf("v1 %s recorded v0_path=%q but cleanup targets %q; mismatched migration source",
			migrate.CompleteFileName, cf.V0Path, v0)
		return plan, nil
	}

	// 7. Re-walk v0 manifest and compare against the recorded value.
	//    A mismatch means the operator edited v0 since migration
	//    completed; this is the ONLY case --clean-force can override.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	current, err := manifestWithCtx(ctx, v0)
	if err != nil {
		return nil, fmt.Errorf("walk v0 manifest: %w", err)
	}
	if current != cf.V0SHA256Manifest {
		reason := fmt.Sprintf("v0 manifest diverged from %s (expected %s, got %s); v0 was edited after migration completed",
			migrate.CompleteFileName, shortHex(cf.V0SHA256Manifest), shortHex(current))
		plan.PartialReason = reason
		plan.ForceableReason = reason
		plan.Reason = "v0 manifest mismatch"
		if logger != nil {
			logger.Warn("v0 manifest divergence detected; --clean-force required to override",
				"expected", cf.V0SHA256Manifest, "actual", current, "path", v0)
		}
		return plan, nil
	}
	plan.Reason = "v0 manifest matches migration_complete.json — safe to soft-delete"
	return plan, nil
}

// shortHex returns up to the first 12 chars of a hex digest, with an
// ellipsis. Safe for short / corrupt manifest strings (a digest that
// somehow ended up under 12 chars is returned in full).
func shortHex(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12] + "…"
}

// manifestWithCtx wraps migrate.ManifestSHA256 with a context-aware
// abort so a pathologically large v0 dir doesn't hang clean.
func manifestWithCtx(ctx context.Context, root string) (string, error) {
	type result struct {
		sum string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		s, e := migrate.ManifestSHA256(root)
		ch <- result{sum: s, err: e}
	}()
	select {
	case r := <-ch:
		return r.sum, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// printV0CleanPlan reports the plan in the same dry-run shape the
// snapshot/legacy targets use.
func printV0CleanPlan(plan *v0CleanPlan, apply bool) {
	if plan.V0Path == "" {
		fmt.Fprintln(os.Stderr, "no v0 cleanup needed: ", plan.Reason)
		return
	}
	overridable := plan.ForceableReason != "" && plan.ForceUsed
	blocked := plan.PartialReason != "" && !overridable
	verb := "would soft-delete"
	if apply {
		if blocked {
			verb = "REFUSING to soft-delete"
		} else if overridable {
			verb = "soft-deleting (--clean-force overrides manifest divergence)"
		} else {
			verb = "soft-deleting"
		}
	}
	fmt.Fprintf(os.Stderr, "%s v0 dir:\n  %s\n  → %s\n", verb, plan.V0Path, plan.TrashPath)
	if plan.Reason != "" {
		fmt.Fprintf(os.Stderr, "  (%s)\n", plan.Reason)
	}
	if plan.PartialReason != "" {
		if overridable {
			fmt.Fprintf(os.Stderr, "  override engaged: %s\n", plan.PartialReason)
		} else {
			fmt.Fprintf(os.Stderr, "  blocked: %s\n", plan.PartialReason)
			if plan.ForceableReason != "" {
				fmt.Fprintf(os.Stderr, "  to override, re-run with --clean-force\n")
			} else {
				fmt.Fprintf(os.Stderr, "  this block is NOT --clean-force overridable; resolve the underlying condition first\n")
			}
		}
	}
}

// applyV0CleanPlan executes the soft-delete via os.Rename. The
// destination is on the same filesystem as v0Path (sibling), so the
// rename is atomic on POSIX. Refuses when PartialReason is set,
// honouring ForceUsed ONLY when ForceableReason matches (currently
// only manifest divergence is force-able).
func applyV0CleanPlan(plan *v0CleanPlan, logger *slog.Logger) error {
	if plan == nil || plan.V0Path == "" {
		return nil
	}
	if plan.PartialReason != "" {
		if plan.ForceableReason == "" {
			return fmt.Errorf("v0 cleanup blocked (not --clean-force overridable): %s", plan.PartialReason)
		}
		if !plan.ForceUsed {
			return fmt.Errorf("v0 cleanup blocked: %s (re-run with --clean-force to override)", plan.PartialReason)
		}
	}
	if plan.TrashPath == "" {
		return errors.New("v0 cleanup: empty trash path (planning bug)")
	}
	// Re-validate every gate at apply time. The original plan was
	// computed some time ago (operator may have eyeballed it for
	// minutes before passing --clean-apply); v0 may have flipped to
	// a symlink, a v0 binary may have started, the manifest may
	// have drifted, etc. A full re-plan against the same configDir
	// gives us a fresh PartialReason set; we then require that the
	// fresh state is still apply-safe under the same ForceUsed
	// flag the caller set on the original plan.
	if plan.ConfigDirPath != "" {
		fresh, perr := planV0Cleanup(plan.ConfigDirPath, logger)
		if perr != nil {
			return fmt.Errorf("v0 cleanup: re-plan failed at apply: %w", perr)
		}
		if fresh.V0Path != plan.V0Path {
			return fmt.Errorf("v0 cleanup: v0 path changed between plan and apply (was %q, now %q)", plan.V0Path, fresh.V0Path)
		}
		if fresh.PartialReason != "" {
			if fresh.ForceableReason == "" || !plan.ForceUsed {
				return fmt.Errorf("v0 cleanup: state changed between plan and apply: %s", fresh.PartialReason)
			}
			// drift but still force-overridable → proceed
		}
	} else {
		// Plan didn't capture ConfigDirPath (legacy callers / tests
		// that built a plan by hand). Fall back to a final lock
		// probe so we still catch the most common race.
		if held, perr := configdir.Probe(plan.V0Path); perr != nil {
			return fmt.Errorf("v0 cleanup: re-probe v0 lock: %w", perr)
		} else if held {
			return fmt.Errorf("v0 cleanup: v0 binary started after planning; aborting (re-run --clean v0 once stopped)")
		}
	}
	// Trash-path collision check moved post-re-plan so a directory
	// created during the re-plan window is still caught right
	// before the rename.
	if _, err := os.Stat(plan.TrashPath); err == nil {
		return fmt.Errorf("v0 cleanup: trash path %s already exists; refusing to clobber", plan.TrashPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("v0 cleanup: stat trash %s: %w", plan.TrashPath, err)
	}
	if err := os.Rename(plan.V0Path, plan.TrashPath); err != nil {
		// On Windows the rename may fail if a process has open
		// handles inside v0 (e.g. an old kojo binary). Surface the
		// platform name in the error to help operator diagnosis.
		return fmt.Errorf("v0 cleanup: rename %s → %s on %s: %w", plan.V0Path, plan.TrashPath, runtime.GOOS, err)
	}
	if logger != nil {
		logger.Info("v0 dir soft-deleted", "from", plan.V0Path, "to", plan.TrashPath)
	}
	fmt.Fprintf(os.Stderr, "kojo: soft-deleted v0 dir to %s\n", plan.TrashPath)
	fmt.Fprintln(os.Stderr, "kojo: physical removal: `kojo --clean v0-trash --clean-apply` (slice 30; default --clean-min-age-days=7) — or `rm -rf` if you skip the recovery window")
	return nil
}

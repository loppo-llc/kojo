package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/loppo-llc/kojo/internal/configdir"
	"github.com/loppo-llc/kojo/internal/migrate"
	"github.com/loppo-llc/kojo/internal/migrate/externalcli"
	// Side-effect import: importers register themselves with the migrate
	// orchestrator via init(). Without this blank import, migrate.Run
	// fails fast with ErrNoImporters.
	_ "github.com/loppo-llc/kojo/internal/migrate/importers"
	"github.com/loppo-llc/kojo/internal/store"
)

// migrationFlags holds every flag that participates in v0 → v1 migration.
// Grouped here so the startup-gate logic in main.go stays readable.
type migrationFlags struct {
	migrate            bool
	migrateRestart     bool
	fresh              bool
	migrateExternalCLI bool
	migrateBackup      string
	rollbackExternal   bool
	// migrateForceRecentMtime, when true, bypasses the v0 mtime
	// safety window (--migrate-force-recent-mtime). Operators reach
	// for this when they're certain v0 is dead but the migrate
	// scanner keeps tripping on a recently-touched file (manual
	// inspection, backup-extract timestamps, etc.). The risk is
	// that a still-active v0 writer would corrupt the import
	// silently; the flag's existence is documented in the help text
	// so the trade-off is visible.
	migrateForceRecentMtime bool
}

// primaryModeCount returns how many "primary" modes the user requested at
// once. Exactly one (or zero) is allowed; conflicting combinations are
// rejected before any disk side effects so the operator gets a clear error
// instead of having one flag silently take precedence over another.
func (f migrationFlags) primaryModeCount() int {
	n := 0
	if f.migrate {
		n++
	}
	if f.migrateRestart {
		n++
	}
	if f.fresh {
		n++
	}
	if f.rollbackExternal {
		n++
	}
	return n
}

// startupMode enumerates the primary action the operator requested.
// Used by classifyStartupMode to separate "what did they ask for"
// (pure, testable) from "what side effects does that imply" (still
// in applyStartupGate).
type startupMode int

const (
	// startupModeNormal is the no-primary-flag boot path: applyStartupGate
	// falls through to the 5.3 trigger table.
	startupModeNormal startupMode = iota
	// startupModeMigrate runs runMigrate against a v0 dir, refusing if v1
	// is already complete (unless migrateRestart is also set).
	startupModeMigrate
	// startupModeMigrateRestart is migrate with the "force redo" override.
	startupModeMigrateRestart
	// startupModeFresh starts a clean v1 install, refusing if v1 is non-empty.
	startupModeFresh
	// startupModeRollbackExternal undoes the claude/codex/gemini CLI
	// symlinks the migrator installed. Independent of v1 dir state.
	startupModeRollbackExternal
	// startupModeInvalid means multiple primary flags were set; the caller
	// must print the mutually-exclusive error and exit.
	startupModeInvalid
)

// classifyStartupMode returns the primary action requested by f. Pure:
// looks at flag values only, no I/O. The actual dispatch (and its
// per-mode side effects) lives in applyStartupGate; tests pin this
// function to lock in flag-combination semantics without spinning up
// the migrator.
func classifyStartupMode(f migrationFlags) startupMode {
	if f.primaryModeCount() > 1 {
		return startupModeInvalid
	}
	switch {
	case f.rollbackExternal:
		return startupModeRollbackExternal
	case f.migrateRestart:
		return startupModeMigrateRestart
	case f.migrate:
		return startupModeMigrate
	case f.fresh:
		return startupModeFresh
	}
	return startupModeNormal
}

// dirState is what mainline cares about: which of {v0, v1, complete file}
// exist on disk at startup. This drives the 5.3 trigger table.
type dirState struct {
	v0Path     string
	v1Path     string
	v0Exists   bool
	v1Exists   bool
	v1Complete bool // migration_complete.json present in v1Path
}

func probeDirs() dirState {
	v0 := configdir.V0Path()
	v1 := configdir.V1Path()
	st := dirState{v0Path: v0, v1Path: v1}
	if fi, err := os.Stat(v0); err == nil && fi.IsDir() {
		st.v0Exists = true
	}
	if fi, err := os.Stat(v1); err == nil && fi.IsDir() {
		st.v1Exists = true
	}
	if _, err := os.Stat(filepath.Join(v1, migrate.CompleteFileName)); err == nil {
		st.v1Complete = true
	}
	return st
}

// applyStartupGate enforces the 5.3 startup trigger rules. It runs migration
// (or rollback) when the corresponding flag is set, then returns:
//   - proceed=true if normal startup should follow (fresh setup or completed v1)
//   - proceed=false if the binary did its job and should exit cleanly (after a
//     migration or rollback completed)
//
// Any non-recoverable condition is communicated by exiting via os.Exit so the
// caller never sees a half-initialized state.
func applyStartupGate(ctx context.Context, flags migrationFlags, logger *slog.Logger, version string) (proceed bool) {
	mode := classifyStartupMode(flags)
	if mode == startupModeInvalid {
		fmt.Fprintln(os.Stderr,
			"kojo: --migrate, --migrate-restart, --fresh and --rollback-external-cli are mutually exclusive.")
		os.Exit(1)
	}

	st := probeDirs()

	// rollback-external-cli is independent of v1 dir state; it only touches
	// claude / codex symlinks and gemini projects.json. Run it first if set.
	if mode == startupModeRollbackExternal {
		if err := runRollbackExternalCLI(st, logger); err != nil {
			logger.Error("rollback-external-cli failed", "err", err)
			fmt.Fprintf(os.Stderr, "kojo: %v\n", err)
			os.Exit(1)
		}
		// runRollbackExternalCLI prints its own success message; do not
		// claim success here so a stub implementation cannot lie about
		// having done work.
		return false
	}

	if mode == startupModeMigrate || mode == startupModeMigrateRestart {
		if !st.v0Exists {
			fmt.Fprintf(os.Stderr, "kojo: --migrate requires v0 data at %s, but the directory does not exist.\n", st.v0Path)
			fmt.Fprintln(os.Stderr, "       use --fresh for a clean v1 install.")
			os.Exit(1)
		}
		if st.v1Complete && mode != startupModeMigrateRestart {
			fmt.Fprintf(os.Stderr, "kojo: v1 directory is already migrated (%s/%s).\n", st.v1Path, migrate.CompleteFileName)
			fmt.Fprintln(os.Stderr, "       remove --migrate to start normally.")
			fmt.Fprintf(os.Stderr, "       to soft-delete the v0 dir at %s, run `kojo --clean v0 --clean-apply`\n", st.v0Path)
			fmt.Fprintln(os.Stderr, "       (Phase 2c-2 slice 29; --clean-force for manifest divergence).")
			os.Exit(1)
		}
		if err := runMigrate(ctx, st, flags, logger, version); err != nil {
			logger.Error("migration failed", "err", err)
			fmt.Fprintf(os.Stderr, "\nkojo: migration failed: %v\n", err)
			fmt.Fprintln(os.Stderr, "       v0 data is unchanged. v1 dir kept for inspection.")
			fmt.Fprintln(os.Stderr, "       re-run with --migrate-restart to start over.")
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "\nkojo: migration complete. v0 data still at %s.\n", st.v0Path)
		fmt.Fprintln(os.Stderr, "       restart kojo without --migrate to begin normal v1 operation.")
		fmt.Fprintln(os.Stderr, "       once the v1 install is healthy and you no longer need v0 rollback,")
		fmt.Fprintln(os.Stderr, "       run `kojo --clean v0 --clean-apply` to soft-delete the v0 dir")
		fmt.Fprintln(os.Stderr, "       (Phase 2c-2 slice 29; the rename target is `kojo.deleted-<ts>/`,")
		fmt.Fprintln(os.Stderr, "       physical removal is operator-driven).")
		return false
	}

	if mode == startupModeFresh {
		// User explicitly opted out of importing v0. Refuse if a v1 dir
		// is already present in any non-empty / non-complete state — we
		// cannot safely "start fresh" on top of someone else's data
		// without explicit confirmation. The operator can either remove
		// the dir manually or run `--migrate-restart` to wipe it under
		// the same guards used by migration.
		if st.v1Complete {
			fmt.Fprintf(os.Stderr, "kojo: --fresh refuses to start: %s already contains a completed v1 install.\n", st.v1Path)
			os.Exit(1)
		}
		if st.v1Exists {
			entries, err := os.ReadDir(st.v1Path)
			if err != nil {
				logger.Error("could not read v1 dir for --fresh check", "path", st.v1Path, "err", err)
				fmt.Fprintf(os.Stderr, "kojo: --fresh: could not inspect %s: %v\n", st.v1Path, err)
				os.Exit(1)
			}
			for _, e := range entries {
				// Allow only artifacts that the v1 binary itself drops:
				// kojo.lock from a previous boot. Anything else means a
				// previous --migrate left state behind, or the operator
				// pointed --config-dir at a populated directory.
				if e.Name() == "kojo.lock" {
					continue
				}
				fmt.Fprintf(os.Stderr, "kojo: --fresh refuses to start: %s is not empty (found %q).\n", st.v1Path, e.Name())
				fmt.Fprintln(os.Stderr, "       remove it manually, or use `--migrate-restart` if you wanted to redo migration.")
				os.Exit(1)
			}
		}
		if err := os.MkdirAll(st.v1Path, 0o755); err != nil {
			logger.Error("could not create v1 dir", "path", st.v1Path, "err", err)
			os.Exit(1)
		}
		return true
	}

	// 5.3 trigger table:
	switch {
	case st.v1Complete:
		// 5.9: complete file's self-integrity must hold (schema_version
		// matches v1 build expectation, migrator_version is well-formed).
		// v0 manifest divergence is NOT checked here — that gate lives
		// in `kojo --clean v0` (see verifyCompleteFile for why).
		if err := verifyCompleteFile(filepath.Join(st.v1Path, migrate.CompleteFileName)); err != nil {
			fmt.Fprintf(os.Stderr, "kojo: %s is invalid: %v\n",
				migrate.CompleteFileName, err)
			fmt.Fprintln(os.Stderr, "       refusing to start; restore from snapshot or rerun with --migrate-restart.")
			os.Exit(1)
		}
		// 5.9: v0 dir still on disk after a completed migration is a
		// supported steady state (rollback window), but surface it on
		// every boot so the operator doesn't forget. The UI banner is
		// the canonical channel; this stderr nudge covers CLI / headless
		// invocations until that banner ships.
		if st.v0Exists {
			fmt.Fprintf(os.Stderr,
				"kojo: v0 data is still present at %s. "+
					"Once you no longer need v0 rollback, run "+
					"`kojo --clean v0 --clean-apply` to soft-delete it.\n",
				st.v0Path)
			logger.Warn("v0 data still present after migration", "v0", st.v0Path)
		}
		// Self-heal: if a previous Run crashed between publishing
		// migration_complete.json and removing migration_in_progress.lock,
		// both files coexist. The complete file is authoritative once
		// present (verifyCompleteFile passed), so it is safe to drop the
		// stale lock here. Without this, --migrate would later refuse on
		// ErrResumeMismatch even though the migration finished.
		stalePaths := []string{
			filepath.Join(st.v1Path, migrate.LockFileName),
		}
		if entries, err := os.ReadDir(st.v1Path); err == nil {
			for _, e := range entries {
				if len(e.Name()) > 9 && e.Name()[:9] == ".migrate-" {
					stalePaths = append(stalePaths, filepath.Join(st.v1Path, e.Name()))
				}
			}
		}
		for _, p := range stalePaths {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				logger.Warn("failed to clean stale migration artifact", "path", p, "err", err)
			}
		}
		return true
	case st.v1Exists && !st.v0Exists:
		// v1 dir present but no completion marker. Either a previous
		// migration crashed (lock file should be there) or someone made
		// the directory by accident. Refuse to silently start fresh.
		fmt.Fprintf(os.Stderr, "kojo: v1 directory %s exists but is not marked complete.\n", st.v1Path)
		if _, err := os.Stat(filepath.Join(st.v1Path, migrate.LockFileName)); err == nil {
			fmt.Fprintln(os.Stderr, "       a previous --migrate run did not finish. Re-run --migrate or --migrate-restart.")
		} else {
			fmt.Fprintln(os.Stderr, "       remove the directory or pass --fresh to start over.")
		}
		os.Exit(1)
		return false // unreachable
	case st.v0Exists:
		// v0 found but no v1 yet — refuse to silently start fresh, which
		// would orphan the user's data.
		fmt.Fprintf(os.Stderr, "kojo: v0 data detected at %s.\n", st.v0Path)
		fmt.Fprintln(os.Stderr, "       run `kojo --migrate` to import it, or `kojo --fresh` for a new install.")
		os.Exit(1)
		return false // unreachable
	default:
		// First-time install. Create the v1 dir and proceed.
		if err := os.MkdirAll(st.v1Path, 0o755); err != nil {
			logger.Error("could not create v1 dir", "path", st.v1Path, "err", err)
			os.Exit(1)
		}
		return true
	}
}

func runMigrate(ctx context.Context, st dirState, flags migrationFlags, logger *slog.Logger, version string) error {
	logger.Info("starting v0 → v1 migration", "v0", st.v0Path, "v1", st.v1Path)

	// 5.4: refuse to migrate while a v0 binary holds its config-dir lock.
	// configdir.Probe is non-destructive: it opens the file O_RDONLY,
	// tries to flock(LOCK_EX|LOCK_NB), releases on success. The v0 dir is
	// never created or modified, even if no lock file is present.
	//
	// There is a probe-then-act window: a v0 binary started milliseconds
	// after Probe returns "free" would not be detected. migrate.Run's
	// pre/post manifest comparison closes that window in practice — any
	// write the v0 binary issues will diverge the manifest and fail
	// migration. Holding the v0 lock for the whole migration would be
	// stronger but requires write-mode open of v0/kojo.lock, which the
	// read-only contract forbids.
	if held, err := configdir.Probe(st.v0Path); err != nil {
		return fmt.Errorf("probe v0 lock: %w", err)
	} else if held {
		return fmt.Errorf("v0 binary appears to be running (lock held on %s)", st.v0Path)
	}

	// The v1 advisory lock is acquired inside migrate.Run for the entire
	// duration of Run; do not acquire it here.

	// Credentials carry-forward: copy v0's encrypted credential store
	// into v1 root BEFORE migrate.Run, not after. Two reasons:
	//   1. migrate.Run writes migration_complete.json at the end. A crash
	//      after Run-success but before this step would leave the v1
	//      install "complete" with no credentials — and applyStartupGate
	//      refuses any further --migrate against a completed v1 dir. The
	//      operator would have no in-product way to recover.
	//   2. migrate.Run's --migrate-restart wipe preserves credentialFiles
	//      (internal/migrate.credentialFiles), so dropping the files in
	//      now is safe across both fresh and restart paths.
	// Best-effort: failures become warnings so the v1 install can still
	// boot; an empty credentials.db is recoverable manually, an aborted
	// migration is not.
	if err := os.MkdirAll(st.v1Path, 0o755); err != nil {
		return fmt.Errorf("create v1 dir for credentials carry-forward: %w", err)
	}

	// HomePeer is the placeholder column written into blob_refs by the
	// blobs importer. Phase 4 peer_registry will rewrite these via a
	// one-shot UPDATE; until then any non-empty stable string is fine,
	// and the hostname matches the convention cmd/kojo/main.go uses
	// when wiring the live blob.Store.
	homePeer, _ := os.Hostname()
	if homePeer == "" {
		homePeer = "kojo-local"
	}
	// Credentials carry-forward runs INSIDE migrate.Run via the
	// PreCompleteHook so it lands after canResumeV1 / wipe
	// accepts the state and inside Run's advisory-lock critical
	// section. Doing it before Run would leave kojo.lock +
	// credentials.* in v1 with no migration sentinel — which
	// canResumeV1 treats as a partial v1 dir and refuses. The
	// hook fires after importers + post-import manifest verify
	// and before migration_complete.json publishes, so a hook
	// failure leaves the migration in a retryable state.
	res, err := migrate.Run(ctx, migrate.Options{
		V0Dir:              st.v0Path,
		V1Dir:              st.v1Path,
		MigratorVersion:    version,
		Restart:            flags.migrateRestart,
		SkipMtimeCheck:     flags.migrateForceRecentMtime,
		MigrateExternalCLI: flags.migrateExternalCLI,
		BackupZipPath:      flags.migrateBackup,
		HomePeer:           homePeer,
		Now:                time.Now,
		PreCompleteHook: func() error {
			for _, w := range applyCredentialsCarryForward(ctx, st.v0Path, st.v1Path, logger) {
				fmt.Fprintf(os.Stderr, "kojo: warning: %s\n", w)
			}
			return nil
		},
	})
	if err != nil {
		return err
	}
	logger.Info("migration finished",
		"manifest", res.V0Manifest,
		"domains", res.DomainsImported,
		"warnings", res.Warnings,
	)
	for _, w := range res.Warnings {
		fmt.Fprintf(os.Stderr, "kojo: warning: %s\n", w)
	}

	// External CLI continuity: runs AFTER a successful migration so we
	// know the v1 store is populated and the agent list is finalized.
	// Best-effort — failures land as warnings and a fresh chat session
	// is the natural fallback. The manifest is written under the v1 dir
	// so `kojo --rollback-external-cli` can undo every operation later.
	// Intentionally placed AFTER the err-return above: if migration
	// failed we must not touch ~/.claude or ~/.gemini, since the v1
	// store may not exist or may be inconsistent.
	if flags.migrateExternalCLI {
		for _, w := range applyExternalCLIForward(ctx, st.v0Path, st.v1Path, logger) {
			fmt.Fprintf(os.Stderr, "kojo: warning: %s\n", w)
		}
	}
	return nil
}

// runRollbackExternalCLI reverts the symlinks / projects.json entries
// introduced by --migrate-external-cli. Reads the manifest written by
// the forward pass and undoes each operation. Best-effort: any
// individual undo failure surfaces as a warning but does not abort
// the remaining entries.
func runRollbackExternalCLI(st dirState, logger *slog.Logger) error {
	if !st.v1Exists {
		return errors.New("rollback-external-cli: no v1 dir; nothing to revert")
	}
	warnings, err := externalcli.Rollback(st.v1Path)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "kojo: warning: %s\n", w)
		logger.Warn("rollback-external-cli", "msg", w)
	}
	fmt.Fprintln(os.Stderr, "kojo: rollback-external-cli complete")
	return nil
}

// verifyCompleteFile parses migration_complete.json, validates the fields
// the startup path depends on, and cross-checks them against kojo.db's
// actual state.
//
// The cross-check matters: if a snapshot restore swapped in a kojo.db whose
// schema is newer (or older) than what the complete file recorded, starting
// up against it would silently mix versions. By opening the DB read-only
// and comparing schema_version, we surface the inconsistency at boot.
//
// We do NOT re-walk the v0 manifest here. docs §5.9 puts that comparison
// in `--clean v0`'s scope (a `--clean-force` override gate), not boot:
// once `migration_complete.json` exists, v1 is canonical and v0 is a
// rollback snapshot. Forcing the operator to soft-delete v0 just to
// boot v1 turns a documented "v0 still present" steady state into a
// startup failure, which is the bug this commit fixes. The "v0 data
// still present" UI banner (also §5.9) carries the user-visible nudge.
func verifyCompleteFile(path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cf migrate.CompleteFile
	if err := json.Unmarshal(body, &cf); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if cf.V1SchemaVersion <= 0 {
		return fmt.Errorf("v1_schema_version missing or non-positive (%d)", cf.V1SchemaVersion)
	}
	if cf.MigratorVersion == "" {
		return errors.New("migrator_version missing")
	}
	// v0_sha256_manifest is intentionally NOT required at boot. It is
	// consumed only by `kojo --clean v0` to gate `--clean-force`; an
	// older `migration_complete.json` that legitimately predates the
	// manifest field must still boot. The clean-side enforces its own
	// "missing manifest" guard (see cmd/kojo/clean_v0.go).
	if cf.CompletedAt <= 0 {
		return errors.New("completed_at missing")
	}

	// Cross-check schema version against the actual DB. Open read-only
	// so the gate cannot inadvertently apply a pending migration before
	// the user has explicitly asked for an upgrade.
	dbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := store.Open(dbCtx, store.Options{
		ConfigDir: filepath.Dir(path),
		ReadOnly:  true,
	})
	if err != nil {
		return fmt.Errorf("open kojo.db: %w", err)
	}
	defer st.Close()
	dbVer, err := st.SchemaVersion(dbCtx)
	if err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}
	if dbVer < cf.V1SchemaVersion {
		return fmt.Errorf("kojo.db schema_version=%d is older than migration_complete.json (%d): possible truncated restore",
			dbVer, cf.V1SchemaVersion)
	}
	supported, err := store.MaxSupportedSchemaVersion()
	if err != nil {
		return fmt.Errorf("probe supported schema version: %w", err)
	}
	if dbVer > supported {
		return fmt.Errorf("kojo.db schema_version=%d is newer than this binary supports (max %d): downgrade not supported",
			dbVer, supported)
	}

	// v0 manifest divergence is intentionally NOT checked here; see the
	// function-level comment. `kojo --clean v0` is the gate that refuses
	// to soft-delete a diverged v0 dir without `--clean-force`.
	return nil
}

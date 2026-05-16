// Package migrate is the one-shot v0 → v1 importer described in
// docs/multi-device-storage.md section 5.
//
// Design contract:
//
//   - v0 directory is treated as immutable. This package opens every v0 file
//     with O_RDONLY (enforced by readOnlyOpen) and never issues mkdir/unlink
//     against a path under the v0 root.
//   - Import is idempotent at the domain granularity. `migration_status`
//     records each domain's phase so a kill -9 partway through a domain can be
//     resumed by re-running `kojo --migrate`.
//   - Manifest verification brackets the run. The pre-run manifest is written
//     into `migration_in_progress.lock`; the same calculation is repeated at
//     completion time. Mismatch (= v0 changed under us) fails the migration
//     without producing `migration_complete.json`.
//
// The bulk of per-domain logic lives in domain-specific Importer
// implementations under this package. This file orchestrates them.
package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/configdir"
	"github.com/loppo-llc/kojo/internal/store"
)

// runtimeLockFileName mirrors the v1 binary's runtime advisory lock so
// migrate code can recognize / preserve / acquire it. Hard-coded rather
// than imported because internal/configdir does not export this constant.
const runtimeLockFileName = "kojo.lock"

// LockFileName is dropped into the v1 directory while a migration is in
// flight. Removed (renamed to CompleteFileName) on success.
const (
	LockFileName     = "migration_in_progress.lock"
	CompleteFileName = "migration_complete.json"

	// MtimeSafetyWindow is the window during which a recent v0 file
	// modification is considered suspicious (5.5 step 2). Operators can
	// override via Options.MtimeSafetyWindow.
	MtimeSafetyWindow = 5 * time.Minute
)

// Options configures Run.
type Options struct {
	V0Dir            string
	V1Dir            string
	MigratorVersion  string        // e.g. "v1.0.0"; written into complete.json
	MtimeSafetyWindow time.Duration

	// SkipMtimeCheck, when true, bypasses the v0 mtime safety window
	// (5.5 step 2). The window exists to catch the "v0 binary still
	// active" footgun where mid-write files would copy half-finished
	// state — but operators who have stopped v0 and just touched the
	// dir manually (e.g. extracting from backup, ls -la for inspection)
	// hit the window even though v0 is dead. Use sparingly: a
	// mid-write at migration time produces silent corruption with no
	// recovery. Wired at the cmd/ layer as --migrate-force-recent-mtime.
	SkipMtimeCheck bool

	// Restart, when true, deletes a partial v1 dir (without
	// migration_complete.json) before starting fresh. Refuses to touch a
	// completed v1 dir (5.6).
	Restart bool

	// MigrateExternalCLI controls 5.5.1 symlink/projects.json updates.
	// Default true at the cmd/ layer; this package treats nil-vs-set
	// equivalently and uses the field as-is.
	MigrateExternalCLI bool

	// BackupZipPath, if non-empty, captures a read-only zip of v0 dir to the
	// given path before importing (5.5 step 9).
	BackupZipPath string

	// HomePeer is stamped on every blob_refs row created by the blobs
	// importer. Empty falls back to os.Hostname() inside the importer;
	// tests pass a fixed value so source_checksum and the row stamp stay
	// deterministic. Phase 4 peer_registry will rewrite these columns
	// via a one-shot UPDATE once peer ids are real.
	HomePeer string

	// Now is injectable for tests. Defaults to time.Now.
	Now func() time.Time

	// SkipV1Acquire, when true, suppresses migrate.Run's own
	// configdir.Acquire on V1Dir. The caller MUST have already
	// acquired the lock and MUST keep it held for the entire
	// Run call. Reserved for callers that want to coordinate the
	// lock externally; the normal flow lets Run take the lock.
	SkipV1Acquire bool

	// PreCompleteHook fires AFTER importers + post-import
	// manifest verify but BEFORE migration_complete.json is
	// published. Used by the cmd/ layer to drop the v0
	// credentials carry-forward into v1 at the right phase —
	// after Run's own canResumeV1 / wipe gate has accepted the
	// state, and inside Run's advisory-lock critical section,
	// so the carry-forward never trips canResumeV1's "credentials
	// without sentinel = partial v1" branch on the next retry.
	// A non-nil error returned by the hook fails Run; the
	// migration lockfile stays so an operator can retry.
	PreCompleteHook func() error
}

// LockFile is the on-disk representation of an in-progress migration.
type LockFile struct {
	V0Path           string `json:"v0_path"`
	V0SHA256Manifest string `json:"v0_sha256_manifest"`
	StartedAt        int64  `json:"started_at"`
	MigratorVersion  string `json:"migrator_version"`
}

// CompleteFile records a successful migration.
type CompleteFile struct {
	V0Path           string `json:"v0_path"`
	V0SHA256Manifest string `json:"v0_sha256_manifest"`
	V1SchemaVersion  int    `json:"v1_schema_version"`
	CompletedAt      int64  `json:"completed_at"`
	MigratorVersion  string `json:"migrator_version"`
}

// Result summarizes a successful or failed Run for the caller (the cmd
// layer). Even on error, partial fields may be populated for diagnostics.
type Result struct {
	V0Manifest      string
	DomainsImported []string
	Skipped         []string
	Warnings        []string
	CompletedAt     time.Time
}

// Sentinel errors. Callers can branch on these via errors.Is.
var (
	ErrV0Locked          = errors.New("migrate: v0 dir is locked (kojo v0 still running)")
	ErrV0RecentlyChanged = errors.New("migrate: v0 dir modified within the safety window")
	ErrAlreadyComplete   = errors.New("migrate: v1 dir already has migration_complete.json")
	ErrPartialV1         = errors.New("migrate: partial v1 dir exists; pass Restart=true or remove it manually")
	ErrManifestMismatch  = errors.New("migrate: v0 manifest changed during migration")
	ErrResumeMismatch    = errors.New("migrate: existing migration_in_progress.lock has a different v0 manifest; rerun with --migrate-restart")
	ErrNoImporters       = errors.New("migrate: no domain importers registered (Phase 1 build); run a build with importers wired")
	ErrV0EqualsV1        = errors.New("migrate: V0Dir and V1Dir resolve to the same canonical path; refusing to migrate")
)

// Run performs the v0 → v1 migration. The function is intentionally long and
// linear so the state transitions match section 5.5 step-by-step.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.V0Dir == "" || opts.V1Dir == "" {
		return nil, errors.New("migrate: V0Dir and V1Dir are required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MtimeSafetyWindow == 0 {
		opts.MtimeSafetyWindow = MtimeSafetyWindow
	}

	// Resolve canonical paths so subsequent equality checks can't be
	// fooled by symlinks / trailing slashes. EvalSymlinks is allowed to
	// fail on a non-existent v1 dir; we fall back to filepath.Clean in
	// that case so the equality check still runs.
	canonicalV0, err := canonicalPath(opts.V0Dir)
	if err != nil {
		return nil, fmt.Errorf("migrate: canonicalize v0: %w", err)
	}
	canonicalV1, err := canonicalPath(opts.V1Dir)
	if err != nil {
		return nil, fmt.Errorf("migrate: canonicalize v1: %w", err)
	}
	if canonicalV0 == canonicalV1 {
		return nil, ErrV0EqualsV1
	}
	opts.V0Dir = canonicalV0
	opts.V1Dir = canonicalV1

	// 5.5 step 1: refuse if v0 is currently held by a running v0 binary.
	// Uses configdir.Probe so we never touch v0's lock file.
	if locked, err := v0Locked(opts.V0Dir); err != nil {
		return nil, fmt.Errorf("migrate: probe v0 lock: %w", err)
	} else if locked {
		return nil, ErrV0Locked
	}

	// Snapshot the v1 dir's pre-existing state BEFORE MkdirAll +
	// configdir.Acquire add our own kojo.lock. wipeIncompleteV1 needs
	// to distinguish between two superficially identical "kojo.lock-
	// only" outcomes:
	//
	//   (a) the dir didn't exist (or was empty) — we just created it
	//       and the only file is our freshly Acquire'd lock. This is
	//       a clean-slate --migrate-restart and the wipe is a no-op.
	//   (b) the dir already held a kojo.lock from somewhere else (a
	//       sibling kojo dir mis-targeted via --config-dir, or a
	//       leftover from a v0 binary, etc.). The sentinel guard MUST
	//       still refuse the wipe — kojo.lock alone is ambiguous.
	//
	// dirWasFresh is the (a) signal threaded through to wipeIncompleteV1.
	dirWasFresh := wasFreshV1Dir(opts.V1Dir)

	// Acquire the v1 runtime lock for the duration of Run. This:
	//   - blocks a concurrent --migrate or normal `kojo` boot on the
	//     same v1 dir (configdir.Acquire is non-blocking; failure here
	//     means another holder)
	//   - keeps the lock file we just created on the inode the v1
	//     runtime expects, so a follow-up `kojo` boot reuses it
	//     (vs. a fresh inode that would invalidate fcntl ranges)
	//
	// MkdirAll is safe before the equality check above: opts.V1Dir is
	// canonicalized and known different from v0.
	if err := os.MkdirAll(opts.V1Dir, 0o755); err != nil {
		return nil, fmt.Errorf("migrate: create v1 dir: %w", err)
	}
	if !opts.SkipV1Acquire {
		v1Lock, err := configdir.Acquire(opts.V1Dir)
		if err != nil {
			return nil, fmt.Errorf("migrate: acquire v1 lock (another kojo or migrate is running on %s): %w", opts.V1Dir, err)
		}
		defer v1Lock.Release()
	}

	// 5.5 step 2: mtime safety window. Skipped when caller passed
	// --migrate-force-recent-mtime; see SkipMtimeCheck for the
	// "operator knows v0 is dead, file just got `ls`'d" rationale.
	if !opts.SkipMtimeCheck {
		if recent, path, err := walkRecentMtime(opts.V0Dir, opts.Now(), opts.MtimeSafetyWindow); err != nil {
			return nil, fmt.Errorf("migrate: scan mtime: %w", err)
		} else if recent {
			return nil, fmt.Errorf("%w: %s changed in the last %s", ErrV0RecentlyChanged, path, opts.MtimeSafetyWindow)
		}
	}

	// Refuse if v1 already completed; allow Restart only against incomplete v1.
	completePath := filepath.Join(opts.V1Dir, CompleteFileName)
	if _, err := os.Stat(completePath); err == nil {
		return nil, ErrAlreadyComplete
	}
	lockPath := filepath.Join(opts.V1Dir, LockFileName)
	if opts.Restart {
		if err := wipeIncompleteV1(opts.V1Dir, opts.V0Dir, dirWasFresh); err != nil {
			return nil, fmt.Errorf("migrate: --migrate-restart cleanup: %w", err)
		}
	} else {
		// Allow fresh start over an empty v1 dir; reject otherwise unless the
		// existing v1 dir is exactly a stale lock + nothing else (resume).
		// A ReadDir failure here is fail-closed: a permission error could
		// otherwise let us treat a populated dir as empty and import on
		// top of it.
		entries, err := os.ReadDir(opts.V1Dir)
		if err != nil {
			return nil, fmt.Errorf("migrate: read v1 dir: %w", err)
		}
		if !canResumeV1(entries) {
			return nil, ErrPartialV1
		}
	}

	// 5.5 step 3: pre-import manifest.
	manifest, err := ManifestSHA256(opts.V0Dir)
	if err != nil {
		return nil, fmt.Errorf("migrate: pre-import manifest: %w", err)
	}

	// 5.6: if a previous Run left an in-progress lock, compare its
	// manifest against the current v0 state. Mismatch means v0 was
	// edited between runs, so resume cannot guarantee idempotency.
	// The caller must drop the partial v1 (--migrate-restart) explicitly.
	if existing, err := readLock(lockPath); err == nil {
		if existing.V0SHA256Manifest != manifest {
			return nil, fmt.Errorf("%w (lock=%s, current=%s)",
				ErrResumeMismatch,
				existing.V0SHA256Manifest, manifest)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("migrate: read prior lock: %w", err)
	}

	if err := writeLock(lockPath, LockFile{
		V0Path:           opts.V0Dir,
		V0SHA256Manifest: manifest,
		StartedAt:        opts.Now().UnixMilli(),
		MigratorVersion:  opts.MigratorVersion,
	}); err != nil {
		return nil, fmt.Errorf("migrate: write lock: %w", err)
	}

	// 5.5 step 4: disk space sanity (best-effort; only warn on failure).
	warnings := []string{}
	if w, err := checkDiskSpace(opts.V0Dir, opts.V1Dir); err != nil {
		warnings = append(warnings, fmt.Sprintf("disk space probe failed: %v", err))
	} else if w != "" {
		warnings = append(warnings, w)
	}

	// 5.5 step 5: schema apply.
	st, err := store.Open(ctx, store.Options{ConfigDir: opts.V1Dir})
	if err != nil {
		return nil, fmt.Errorf("migrate: open v1 store: %w", err)
	}
	defer st.Close()

	// 5.5 step 6 & 7: per-domain importers. Phase 1 ships only the
	// orchestrator; concrete importers land in subsequent phases. Each
	// importer must update migration_status so a re-run resumes cleanly.
	//
	// Refuse to publish migration_complete.json with no importers
	// registered: that would convince a future v1 startup that v0 had
	// already been imported, and the next-phase importers would skip
	// the user's data permanently. The lock file is left in place so a
	// subsequent build with importers can resume (after manifest match).
	imp := importers()
	if len(imp) == 0 {
		return nil, ErrNoImporters
	}
	imported := []string{}
	skipped := []string{}
	for _, im := range imp {
		if err := im.Run(ctx, st, opts); err != nil {
			return nil, fmt.Errorf("migrate: domain %q: %w", im.Domain(), err)
		}
		imported = append(imported, im.Domain())
	}

	// 5.5 step 9: optional backup zip.
	if opts.BackupZipPath != "" {
		if err := backupV0(opts.V0Dir, opts.BackupZipPath); err != nil {
			return nil, fmt.Errorf("migrate: backup: %w", err)
		}
	}

	// 5.5 step 10: post-import manifest verify.
	postManifest, err := ManifestSHA256(opts.V0Dir)
	if err != nil {
		return nil, fmt.Errorf("migrate: post-import manifest: %w", err)
	}
	if postManifest != manifest {
		return nil, ErrManifestMismatch
	}

	// Pre-complete hook: cmd/ layer uses this to land
	// applyCredentialsCarryForward inside the migration's
	// advisory-lock critical section, so the carry-forward
	// happens after the partial-v1 gate has cleared. A failed
	// hook leaves the migration lockfile in place; a retry
	// with --migrate-restart will wipe + re-run.
	if opts.PreCompleteHook != nil {
		if err := opts.PreCompleteHook(); err != nil {
			return nil, fmt.Errorf("migrate: pre-complete hook: %w", err)
		}
	}

	// 5.5 step 11: rename lock → complete.
	schemaVersion, err := st.SchemaVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: read schema version: %w", err)
	}
	completeBody, err := json.MarshalIndent(CompleteFile{
		V0Path:           opts.V0Dir,
		V0SHA256Manifest: manifest,
		V1SchemaVersion:  schemaVersion,
		CompletedAt:      opts.Now().UnixMilli(),
		MigratorVersion:  opts.MigratorVersion,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("migrate: marshal complete: %w", err)
	}
	if err := atomicWrite(completePath+".tmp", completeBody); err != nil {
		return nil, fmt.Errorf("migrate: stage complete: %w", err)
	}
	if err := os.Rename(completePath+".tmp", completePath); err != nil {
		return nil, fmt.Errorf("migrate: publish complete: %w", err)
	}
	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		warnings = append(warnings, fmt.Sprintf("could not remove lock file: %v", err))
	}

	return &Result{
		V0Manifest:      manifest,
		DomainsImported: imported,
		Skipped:         skipped,
		Warnings:        warnings,
		CompletedAt:     opts.Now(),
	}, nil
}

// canResumeV1 returns true when the v1 dir contains nothing that would
// conflict with re-running the importer. The acceptable set is:
//   - migration_in_progress.lock  (stale state from prior crash)
//   - kojo.lock                   (the runtime advisory lock just acquired
//                                  by Run, or left behind by a prior Run)
//   - kojo.db / kojo.db-wal / kojo.db-shm  (partial schema/import)
//   - .migrate-* atomic-write temp files (from a crashed atomicWrite)
//   - global / local / machine    (blob scope subtrees published by
//                                  the blobs importer; the importer's
//                                  own resume logic reconciles partial
//                                  publishes via Verify+Head)
//   - credentials.{db,db-wal,db-shm,key}  (encrypted user secret store
//                                  owned by internal/agent/credential.go;
//                                  allowed but NOT a sentinel — see the
//                                  isCredentialFile gate below)
//
// Scope dirs / `auth/` / credentials.* alone (no kojo.db / no
// migration_in_progress.lock) do NOT count as resumable — those
// patterns match a v1 dir a user hand-crafted (or a normal v1
// startup that wrote credentials before --migrate ever ran), and
// starting a fresh import on top of them would silently overwrite
// the operator's bytes. Resume requires a *migration* sentinel
// (kojo.db family or migration_in_progress.lock) for those
// specific entries. A truly empty v1 dir, or one containing only
// the runtime kojo.lock, IS allowed (that is the fresh-install
// case where Run is the first thing to write into the dir).
//
// Anything outside the allow-list suggests user data was created in
// v1 dir before completion; caller must force --migrate-restart,
// which routes through wipeIncompleteV1 for a stricter check.
func canResumeV1(entries []os.DirEntry) bool {
	hasMigrationSentinel := false
	for _, e := range entries {
		switch e.Name() {
		case LockFileName, store.DBFileName,
			store.DBFileName + "-wal", store.DBFileName + "-shm":
			hasMigrationSentinel = true
		}
	}
	for _, e := range entries {
		name := e.Name()
		if !isResumeAllowed(name) {
			return false
		}
		// Scope dirs are only resume-safe alongside a migration
		// sentinel — without it we can't tell a partial-import scope
		// tree from a hand-crafted user dir that just happens to be
		// named `global/`.
		if isBlobScopeDir(name) && !hasMigrationSentinel {
			return false
		}
		// Same gate for `auth/` — a hand-bootstrapped v1 dir with just
		// `auth/kek.bin` (e.g. operator wired secretcrypto out-of-band)
		// shouldn't be silently treated as a migration to resume; that
		// would let a fresh --migrate write into a populated v1 dir.
		// Require a migration sentinel so resume only triggers when
		// the prior --migrate actually started.
		if name == "auth" && !hasMigrationSentinel {
			return false
		}
		// Same gate for credentials.{db,key,...}: agent.Manager
		// creates them on every normal v1 startup. We allow them
		// alongside a migration sentinel (operator launched the
		// binary, then ran --migrate to import v0), but a v1 dir
		// containing ONLY credential files — no kojo.db / no
		// migration_in_progress.lock — would be a hand-crafted /
		// disaster-recovery state we should refuse rather than
		// silently start a fresh import on top of.
		if isCredentialFile(name) && !hasMigrationSentinel {
			return false
		}
		// Same gate for scratch siblings — they only ever exist
		// transiently during PreCompleteHook, which fires after
		// kojo.db has already been written. A scratch file without
		// a migration sentinel is therefore not a real resume
		// shape; treat it as user-created junk and refuse.
		if isCredentialScratch(name) && !hasMigrationSentinel {
			return false
		}
	}
	return true
}

// blobScopeDirs lists the top-level dirs the blobs importer creates
// under V1Dir. Kept as a single source of truth for both the resume
// allow-list and the restart-wipe allow-list so the two stay in sync.
var blobScopeDirs = [...]string{"global", "local", "machine"}

// credentialFiles names the per-host encrypted credential store
// owned by internal/agent/credential.go (NewCredentialStore opens
// these on the first agent.Manager construction). They sit at the
// v1 dir root and have a lifecycle independent of v0→v1 migration:
// an operator who launches the v1 binary normally before running
// --migrate will already have these files. Both the resume gate
// and the --migrate-restart wipe loop must recognise them — the
// resume gate to admit the v1 dir as not "user-created junk", the
// wipe loop to PRESERVE them (they hold encrypted user secrets
// that the migration has no business destroying).
//
// The .db file may have SQLite WAL / SHM siblings; both are listed
// so a busy crash leaves all three under the same allow-list.
var credentialFiles = [...]string{
	"credentials.db", "credentials.db-wal", "credentials.db-shm",
	"credentials.key",
}

func isCredentialFile(name string) bool {
	for _, s := range credentialFiles {
		if name == s {
			return true
		}
	}
	return false
}

// isCredentialScratch reports whether name is a scratch file left
// behind by applyCredentialsCarryForward's stage/replace dance —
// `credentials.<leaf>.tmp-XXXXXX` (incoming bytes pre-rename) or
// `credentials.<leaf>.bak-XXXXXX` (prior dst preserved during a
// crash-safe replaceFile). A crash mid-PreCompleteHook (after Run
// took the lock but before migration_complete.json publishes) can
// leave these on disk; both the resume gate and the --migrate-restart
// wipe loop must recognise them so the operator isn't told "v1 dir
// has unexpected entries — manual cleanup required" for files this
// codebase itself created.
//
// Matching is structural rather than glob: starts with a known
// credential basename, followed by `.tmp-` or `.bak-`. We don't
// validate the random suffix — os.CreateTemp picks it.
func isCredentialScratch(name string) bool {
	for _, s := range credentialFiles {
		if strings.HasPrefix(name, s+".tmp-") ||
			strings.HasPrefix(name, s+".bak-") {
			return true
		}
	}
	return false
}

func isBlobScopeDir(name string) bool {
	for _, s := range blobScopeDirs {
		if name == s {
			return true
		}
	}
	return false
}

func isResumeAllowed(name string) bool {
	switch name {
	case LockFileName, runtimeLockFileName,
		store.DBFileName, store.DBFileName + "-wal", store.DBFileName + "-shm":
		return true
	}
	if isBlobScopeDir(name) {
		return true
	}
	// internal/agent/credential.go's NewCredentialStore is invoked
	// on every agent.Manager startup, including the case where an
	// operator launched the v1 binary normally before running
	// `--migrate`. The resulting credentials.{db,db-wal,db-shm,key}
	// files are out of migration scope but block --migrate's
	// "is the v1 dir resumable?" check unless allow-listed here.
	// They are NOT a migration sentinel — see canResumeV1's
	// hasMigrationSentinel computation; resume is still gated on
	// kojo.db / migration_in_progress.lock as before.
	if isCredentialFile(name) {
		return true
	}
	// Scratch files left behind by applyCredentialsCarryForward when
	// a crash interrupts PreCompleteHook. We created them; we admit
	// them. The resume path doesn't delete them — wipeIncompleteV1
	// does, and a successful PreCompleteHook removes them itself.
	if isCredentialScratch(name) {
		return true
	}
	// `auth/` holds the host-bound KEK (auth/kek.bin, see design doc
	// §3.4), created on first use by secretcrypto.LoadOrCreateKEK —
	// the vapid importer materializes it during migration to envelope-
	// seal the v0 VAPID private key. A subsequent --migrate (resume
	// after crash) must recognise it as a benign artifact rather than
	// treating its
	// presence as "user data in v1, refuse".
	if name == "auth" {
		return true
	}
	return strings.HasPrefix(name, ".migrate-")
}

// wipeIncompleteV1 removes the v1 directory contents when --migrate-restart
// is set. Defensive against catastrophic mis-targeting: refuses unless
//   - dir != v0Dir (canonical paths)
//   - dir contains v1-specific sentinel(s): migration_in_progress.lock or
//     kojo.db. The v0 sentinels (kojo.lock, agents/) are NOT accepted because
//     the v0 dir matches them too.
//   - migration_complete.json is absent.
//
// Removes only entries the importer is known to write; bails out if it sees
// anything unexpected at the top level.
// wasFreshV1Dir reports whether the v1 dir was effectively unused by
// any prior migration attempt when Run was entered. "Effectively
// unused" means: missing, empty, or holding nothing but credentials
// files (encrypted user secrets owned by internal/agent/credential.go,
// independent of v0→v1 migration). Credentials files alone do not
// indicate a half-written migration — the operator may have typed
// an API key into the v1 binary before deciding to run --migrate /
// --migrate-restart.
//
// Used by wipeIncompleteV1's preserve-only escape hatch so we
// don't silently no-op on a populated lock-only target the operator
// didn't intend to wipe, while still letting credentials-only state
// (a real on-disk shape after the v1 binary has been started once
// without migrating) trigger the no-op restart path.
func wasFreshV1Dir(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true
		}
		// Permission / I/O error: fail closed (treat as not-fresh).
		// The downstream sentinel guard will surface the real error.
		return false
	}
	for _, e := range entries {
		if !isCredentialFile(e.Name()) {
			return false
		}
		// Defense in depth: a directory or symlink named
		// "credentials.db" / "credentials.key" / ... does NOT
		// participate in the credentials-only fresh-dir judgement.
		// internal/agent/credential.go only ever materializes
		// those names as regular files; anything else at the same
		// path is either operator confusion or an attacker trying
		// to slip past the sentinel guard with a directory dropped
		// in beforehand.
		//
		// e.Info() over e.Type(): DirEntry.Type() reads the
		// readdir(3) `d_type` hint and can be DT_UNKNOWN on some
		// filesystems (NFS, FUSE), in which case IsRegular() is
		// true on a Mode value of 0 and a symlink would sneak
		// through. info.Mode() goes through lstat and is
		// authoritative. info() errors are treated as
		// "not-regular" (fail closed).
		info, ierr := e.Info()
		if ierr != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}

func wipeIncompleteV1(dir, v0Dir string, allowFreshRestart bool) error {
	canDir, err := canonicalPath(dir)
	if err != nil {
		return err
	}
	canV0, err := canonicalPath(v0Dir)
	if err != nil {
		return err
	}
	if canDir == canV0 {
		return ErrV0EqualsV1
	}

	complete := filepath.Join(dir, CompleteFileName)
	if _, err := os.Stat(complete); err == nil {
		return ErrAlreadyComplete
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}

	// "Effectively empty" v1 dir: nothing in it except the runtime
	// kojo.lock (created by migrate.Run's configdir.Acquire above)
	// and/or credentials files that this code path never deletes.
	// On a true first-run `--migrate-restart` (no prior migration
	// state, no kojo.db, no migration_in_progress.lock) the dir
	// looks exactly like this: just kojo.lock that we ourselves
	// just opened. Treat it as no-op and let the importer proceed
	// — the sentinel guard below is meant for "wipe a half-written
	// v1 dir", not "block --migrate-restart on a clean slate".
	//
	// allowFreshRestart=true means Run saw the dir as
	// missing/empty BEFORE it added its own lock. Without that
	// witness we cannot tell apart "we just created the lock" from
	// "the lock was already here when we attached", so the
	// preserve-only escape hatch refuses to fire and the sentinel
	// guard below stays authoritative.
	if allowFreshRestart {
		preserveOnly := true
		for _, e := range entries {
			if !(e.Name() == runtimeLockFileName || isCredentialFile(e.Name())) {
				preserveOnly = false
				break
			}
		}
		if preserveOnly {
			return nil
		}
	}

	// Require a v1-specific sentinel before touching anything. The
	// runtime kojo.lock alone is NOT enough: v0 dirs have one too, and
	// while we already rejected v0Dir==v1Dir above, that does not protect
	// against a sibling kojo dir mis-targeted via --config-dir.
	hasV1Sentinel := false
	for _, e := range entries {
		switch e.Name() {
		case LockFileName, store.DBFileName,
			store.DBFileName + "-wal", store.DBFileName + "-shm":
			hasV1Sentinel = true
		}
	}
	if !hasV1Sentinel {
		return fmt.Errorf("refusing to wipe %s: no v1 sentinel (%s or %s) present",
			dir, LockFileName, store.DBFileName)
	}

	// Whitelist of names this importer is allowed to delete from a
	// partially-written v1 dir. Anything else (including stray user
	// files dropped in by mistake) aborts the wipe.
	//
	// The runtime kojo.lock is *kept* even on Restart: at the call site
	// we are holding that lock's flock; deleting the inode would split
	// it from the descriptor on Linux (releasing locks via st_dev/ino
	// reuse) and fail outright on Windows where the file is in use.
	allowedDel := func(name string) bool {
		switch name {
		case LockFileName,
			CompleteFileName, CompleteFileName + ".tmp",
			store.DBFileName,
			store.DBFileName + "-wal", store.DBFileName + "-shm":
			return true
		}
		// blobs importer creates `global/`, `local/`, `machine/`
		// scope subtrees under V1Dir. On --migrate-restart the
		// caller wants those gone too — otherwise a half-published
		// avatar from the prior crashed run survives into the fresh
		// import and confuses blobResumeDecision (Head succeeds but
		// the matching ref row is gone, since kojo.db was wiped).
		if isBlobScopeDir(name) {
			return true
		}
		// `auth/` holds the host-bound KEK (auth/kek.bin, see design
		// doc §3.4), materialized by the vapid importer during a prior
		// crashed run. On --migrate-restart the caller wants a fresh
		// start — wiping auth/ is the correct behaviour because the
		// kv rows that were sealed under the prior KEK live in kojo.db
		// (also being wiped here), so retaining the KEK without its
		// sealed payloads is meaningless.
		// Recursive removal because auth/ may contain temp files from
		// secretcrypto.LoadOrCreateKEK's atomic-write dance.
		if name == "auth" {
			return true
		}
		// atomicWrite leaves ".migrate-XXXXXX" temp files when a crash
		// happens between create and rename. We are allowed to clean
		// these up because they have no significance after a restart.
		if strings.HasPrefix(name, ".migrate-") {
			return true
		}
		// applyCredentialsCarryForward's stage/replace dance leaves
		// `credentials.<leaf>.tmp-XXXXXX` (incoming bytes) and
		// `credentials.<leaf>.bak-XXXXXX` (prior dst preserved during
		// replaceFile) on a mid-PreCompleteHook crash. --migrate-restart's
		// scope is "redo the migration from scratch", and these scratch
		// files have no value across a restart — drop them and let the
		// next carry-forward re-stage from v0.
		return isCredentialScratch(name)
	}
	preserve := func(name string) bool {
		// runtimeLockFileName: the flock'd kojo.lock we are still
		// holding — deleting the inode would split the lock from
		// the descriptor.
		// credentials.{db,db-wal,db-shm,key}: encrypted user
		// secrets owned by internal/agent/credential.go,
		// independent of v0→v1 migration. --migrate-restart's
		// scope is "redo the migration" — it has no business
		// destroying secrets the operator entered through the
		// running v1 binary before deciding to re-run --migrate.
		return name == runtimeLockFileName || isCredentialFile(name)
	}
	for _, e := range entries {
		name := e.Name()
		if preserve(name) {
			continue
		}
		if !allowedDel(name) {
			return fmt.Errorf("refusing to wipe %s: contains unexpected entry %q (manual cleanup required)",
				dir, name)
		}
	}
	for _, e := range entries {
		name := e.Name()
		if preserve(name) {
			continue
		}
		path := filepath.Join(dir, name)
		// Scope subtrees (global/, local/, machine/) and the auth/
		// directory need recursive removal because they hold subtrees
		// (every published blob from the prior partial run, or the KEK
		// file plus any leftover atomic-write temps); everything else
		// on the allow-list is a single file (kojo.db, kojo.db-wal,
		// .migrate-* temps).
		var rmErr error
		if isBlobScopeDir(name) || name == "auth" {
			rmErr = os.RemoveAll(path)
		} else {
			rmErr = os.Remove(path)
		}
		if rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return fmt.Errorf("wipe %s/%s: %w", dir, name, rmErr)
		}
	}
	return nil
}

// canonicalPath resolves symlinks if possible and falls back to filepath.Clean
// when the target does not yet exist (the v1 dir on a first run).
func canonicalPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("empty path")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return filepath.Clean(abs), nil
		}
		return "", err
	}
	return resolved, nil
}

func writeLock(path string, lf LockFile) error {
	body, err := json.MarshalIndent(lf, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, body)
}

// readLock reads an existing migration_in_progress.lock; returns
// os.ErrNotExist (wrapped) if absent so callers can branch with errors.Is.
func readLock(path string) (LockFile, error) {
	var lf LockFile
	body, err := os.ReadFile(path)
	if err != nil {
		return lf, err
	}
	if err := json.Unmarshal(body, &lf); err != nil {
		return lf, fmt.Errorf("parse lock %s: %w", path, err)
	}
	return lf, nil
}

// atomicWrite writes body to path via a temp file + rename + parent dir
// fsync. We use this both for the lock file and the complete file so
// migration crash recovery has well-defined semantics.
func atomicWrite(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".migrate-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return fsyncDir(dir)
}

// readOnlyOpen is the only file-open primitive migration code is allowed to
// use against the v0 directory. Any code path that tries to open a v0 file
// for write must instead copy the bytes into the v1 dir; see 5.5.1.6 for the
// rationale.
func readOnlyOpen(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY, 0)
}

// readAllRO reads the entire content of a v0 file under O_RDONLY. Wraps
// readOnlyOpen so callers can stay one-liners while still going through the
// guard.
func readAllRO(path string) ([]byte, error) {
	f, err := readOnlyOpen(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// v0Locked reports whether v0's `kojo.lock` is currently held by another
// process. Uses configdir.Probe so the v0 directory is opened O_RDONLY and
// the advisory lock is immediately released after the probe — no v0 file
// is created, modified, or has its mtime touched.
func v0Locked(v0 string) (bool, error) {
	return configdir.Probe(v0)
}

// walkRecentMtime returns true if any file under root was modified within
// `window` of `now`. The first offending file's path is returned for the
// error message.
func walkRecentMtime(root string, now time.Time, window time.Duration) (bool, string, error) {
	threshold := now.Add(-window)
	var found string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries; manifest pass will catch real corruption
		}
		// Skip the lock file itself; v0 truncates/touches it on every start.
		if d.Name() == "kojo.lock" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(threshold) {
			found = path
			return io.EOF // sentinel to stop walk early
		}
		return nil
	})
	if errors.Is(err, io.EOF) {
		return true, found, nil
	}
	return false, "", err
}

// checkDiskSpace is best-effort and platform-specific. Phase 1 stub returns
// "" with no error; a real implementation lands with the v0_size × 1.2 rule.
func checkDiskSpace(v0, v1 string) (string, error) {
	_ = v0
	_ = v1
	return "", nil
}

// backupV0 captures a zip of v0 dir to dst. Phase 1 stub is a TODO; the
// importers don't depend on backup, and shipping it broken is worse than
// shipping it absent. Wire up via --migrate-backup once the zipper exists.
func backupV0(v0, dst string) error {
	// Intentionally a placeholder; the cmd layer surfaces this as
	// "--migrate-backup not yet implemented" until phase 6.
	if !strings.HasSuffix(dst, ".zip") {
		return fmt.Errorf("backup path must end in .zip, got %q", dst)
	}
	return errors.New("migrate: --migrate-backup is not implemented yet (planned for phase 6)")
}

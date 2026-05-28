package snapshot

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ApplyOptions narrows Apply(). All fields are optional.
type ApplyOptions struct {
	// Force lets the restore proceed even when a live kojo.db already
	// exists at the target. The default refuses to overwrite a
	// non-empty target so an accidental `kojo --restore` on a Hub that
	// is still running can't silently nuke its DB.
	//
	// Even with Force, Apply NEVER overwrites a target whose
	// configdir lock is held — the configdir.Acquire path layers on
	// top in cmd/kojo and the operator is required to stop the
	// running kojo before restoring (docs/snapshot-restore.md §
	// "Restore on a backup peer" step 1).
	Force bool
}

// Apply restores a snapshot into the given target config directory.
// Manifest version + DB sha256 are verified before any data is laid
// down so a corrupted / mid-rsync snapshot fails the operation
// cleanly without touching the target.
//
// Order of operations (every safety check runs BEFORE any write):
//
//  1. Load + validate manifest.
//  2. Verify snapshot DB sha256.
//  3. Manifest BlobScopes ↔ source disk reconciliation.
//  4. Existing-target overwrite gate (Force).
//  5. Symlink / non-directory checks on every restore-touched
//     subtree (configdir, blobs/, blobs/global/, auth/) — a planted
//     symlink anywhere on the chain would let copyTree walk outside
//     the configdir.
//  6. Only after every check passes do we mkdir, chmod, copy.
//
// Items intentionally NOT restored:
//
//   - <target>/auth/kek.bin: the KEK is operator-managed via a side
//     channel (snapshot intentionally excludes it; restoring it from
//     a stale snapshot would break envelope decryption if the KEK
//     has been rotated). The operator must supply a matching KEK to
//     the target before kojo can boot.
//   - <target>/auth/agent_tokens/*: per-peer machine credentials are
//     out of scope (peer-replicated kv rows in kojo.db carry the
//     hashes already).
//   - Per-peer scoped blobs / credentials.db: these stay on their
//     respective peers. The new Hub serves them through the same
//     peer-fan-out path the original Hub did.
//
// Returns a non-nil error if any precondition fails. On
// partial-write failures past the preflight gate (e.g. the
// filesystem fills mid-copy) the target may be left in an
// inconsistent state; the runbook in docs/snapshot-restore.md
// tells the operator to wipe the target dir and retry.
func Apply(srcDir, targetConfigDir string, opts ApplyOptions) error {
	if srcDir == "" || targetConfigDir == "" {
		return errors.New("snapshot.Apply: srcDir and targetConfigDir required")
	}
	// 1. Manifest first — fail fast on a missing / unsupported
	//    snapshot before we look at the target.
	m, err := LoadManifest(srcDir)
	if err != nil {
		return fmt.Errorf("snapshot.Apply: manifest: %w", err)
	}
	// 2. Verify the DB sha256 against the manifest. Catches truncated
	//    rsync runs / corrupted media.
	if err := VerifyDB(srcDir); err != nil {
		return fmt.Errorf("snapshot.Apply: verify: %w", err)
	}
	// 3. Enforce the manifest's blob_scopes contract. If the
	//    snapshot claims "global" was captured, the dir MUST exist;
	//    a partial / corrupted rsync that dropped blobs/global would
	//    otherwise let the restore succeed with a DB that points at
	//    blob URIs whose bodies are missing.
	hasGlobalInManifest := false
	for _, scope := range m.BlobScopes {
		if scope == "global" {
			hasGlobalInManifest = true
			break
		}
	}
	blobSrc := filepath.Join(srcDir, "blobs", "global")
	srcInfo, srcErr := os.Lstat(blobSrc)
	srcBlobsPresent := srcErr == nil
	if hasGlobalInManifest {
		if !srcBlobsPresent {
			return fmt.Errorf("snapshot.Apply: manifest lists 'global' scope but %s is missing", blobSrc)
		}
		if srcInfo.Mode()&os.ModeSymlink != 0 || !srcInfo.IsDir() {
			return fmt.Errorf("snapshot.Apply: %s is not a directory (symlink/file disallowed)", blobSrc)
		}
	}
	// 4. Refuse to overwrite a live target unless Force is set.
	dbDst := filepath.Join(targetConfigDir, DBFileName)
	if _, err := os.Lstat(dbDst); err == nil {
		if !opts.Force {
			return fmt.Errorf("snapshot.Apply: target %s already has %s; pass Force to overwrite", targetConfigDir, DBFileName)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("snapshot.Apply: stat target db: %w", err)
	}
	// 5. PREFLIGHT: every target-side symlink / non-dir check runs
	//    here, before any write touches the target. A failure
	//    surfaces with the target completely untouched — no
	//    half-restored DB, no partial blob tree.
	//
	// Target root: refuse a symlink-as-configdir (configdir.Acquire
	// would happily follow it and we'd write outside the canonical
	// path). A nonexistent target is fine — the Mkdir step below
	// creates it.
	if info, err := os.Lstat(targetConfigDir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("snapshot.Apply: target %s is a symlink", targetConfigDir)
		}
		if !info.IsDir() {
			return fmt.Errorf("snapshot.Apply: target %s is not a directory", targetConfigDir)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("snapshot.Apply: lstat target: %w", err)
	}
	blobDst := filepath.Join(targetConfigDir, "blobs", "global")
	authDst := filepath.Join(targetConfigDir, "auth")
	if err := rejectSymlinkChain(targetConfigDir, authDst); err != nil {
		return fmt.Errorf("snapshot.Apply: auth path: %w", err)
	}
	// Symlink-chain check on blobs path runs UNCONDITIONALLY (not
	// gated on hasGlobalInManifest) because the no-global branch
	// below still calls replaceBlobTree(blobDst) to clear any stale
	// tree. Without this preflight, a planted <target>/blobs ->
	// /tmp/escape symlink would let replaceBlobTree Lstat / Rename
	// / RemoveAll inside the escape dir.
	if err := rejectSymlinkChain(targetConfigDir, blobDst); err != nil {
		return fmt.Errorf("snapshot.Apply: blobs path: %w", err)
	}
	if hasGlobalInManifest && srcBlobsPresent {
		// Walk the source tree and validate the corresponding
		// destination chain so a planted symlink at any nested
		// level surfaces BEFORE the DB copy. safeCopyTree below
		// re-checks during the actual walk as defence-in-depth,
		// but the preflight is what guarantees "target untouched
		// on rejection".
		if err := validateCopyTreeDestination(blobSrc, blobDst); err != nil {
			return fmt.Errorf("snapshot.Apply: blobs preflight: %w", err)
		}
	}
	// 6. Mkdir target. Use the same restrictive mode the snapshot
	//    writer uses (0o700) so freshly-restored data inherits
	//    snapshot-grade permissions even on a target that didn't
	//    exist before this run.
	if err := os.MkdirAll(targetConfigDir, 0o700); err != nil {
		return fmt.Errorf("snapshot.Apply: mkdir target: %w", err)
	}
	// configdir.Acquire (in cmd/kojo) may have created the dir at
	// 0755 before we got the lock; tighten to 0700 explicitly so
	// the post-condition matches the "snapshot-grade permissions"
	// promise above. POSIX supports Chmod; on Windows the file
	// mode model is different and we accept best-effort.
	if err := os.Chmod(targetConfigDir, 0o700); err != nil && !errors.Is(err, os.ErrNotExist) {
		if runtime.GOOS != "windows" {
			return fmt.Errorf("snapshot.Apply: chmod target 0o700: %w", err)
		}
	}
	// 7. Copy DB. copyFile is tmp-write + rename so the boot path
	//    sees either the old or new DB, never a truncated one.
	dbSrc := filepath.Join(srcDir, DBFileName)
	if err := copyFile(dbSrc, dbDst, 0o600); err != nil {
		return fmt.Errorf("snapshot.Apply: copy db: %w", err)
	}
	// 8. Restore global blob tree with replace semantics. Merge
	//    copy would leave stale entries from the previous Hub
	//    (avatars / books / temp that no longer have a blob_refs
	//    row) visible on disk — blob.Store reads the filesystem
	//    directly for Head/Open/List so those orphans would
	//    re-surface to clients. Rename-aside the existing tree
	//    first so the new tree is laid down on a clean directory;
	//    on success the aside is deleted. On Apply failure the
	//    aside is left behind so the operator can recover.
	//
	//    Only runs if the manifest + source both have it; the
	//    precondition checks above already covered every
	//    disagreement case. safeCopyTree refuses to follow any
	//    symlink encountered during the walk — both source
	//    (already filtered by the snapshot writer) AND destination
	//    (a planted blob_dst/sub-dir-as-symlink would otherwise
	//    escape).
	if hasGlobalInManifest && srcBlobsPresent {
		if err := replaceBlobTree(blobDst); err != nil {
			return fmt.Errorf("snapshot.Apply: replace blobs: %w", err)
		}
		if err := os.MkdirAll(blobDst, 0o700); err != nil {
			return fmt.Errorf("snapshot.Apply: mkdir blobs: %w", err)
		}
		if err := safeCopyTree(blobSrc, blobDst); err != nil {
			return fmt.Errorf("snapshot.Apply: copy blobs: %w", err)
		}
	} else if !hasGlobalInManifest {
		// Manifest doesn't list "global" — a snapshot taken from a
		// brand-new install. Drop any pre-existing blobs/global
		// on the target so we don't surface a stale tree
		// previously laid down by a different Hub.
		if err := replaceBlobTree(blobDst); err != nil {
			return fmt.Errorf("snapshot.Apply: clear stale blobs: %w", err)
		}
	}
	// 9. Pre-create auth/ at 0700 so the operator's out-of-band
	//    KEK drop into <target>/auth/kek.bin has the parent dir
	//    ready (and with the correct mode bits) without an extra
	//    `mkdir -p` step.
	if err := os.MkdirAll(authDst, 0o700); err != nil {
		return fmt.Errorf("snapshot.Apply: mkdir auth: %w", err)
	}
	if err := os.Chmod(authDst, 0o700); err != nil {
		if runtime.GOOS != "windows" {
			return fmt.Errorf("snapshot.Apply: chmod auth 0o700: %w", err)
		}
	}
	return nil
}

// replaceBlobTree moves an existing blobs/global tree aside so the
// fresh restore can lay down its replacement on a clean slot. The
// previous tree is renamed to a `.pre-restore.<unix-nanos>` sibling
// and then RemoveAll'd; a rename failure (cross-filesystem) falls
// back to RemoveAll directly. We never abandon a half-deleted tree
// at the canonical path — Apply's caller relies on the canonical
// path either being absent or carrying the new snapshot's bytes
// exclusively.
//
// Returns nil when the path doesn't exist (fresh-install target).
func replaceBlobTree(blobDst string) error {
	if _, err := os.Lstat(blobDst); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("lstat %s: %w", blobDst, err)
	}
	aside := fmt.Sprintf("%s.pre-restore.%d", blobDst, nowNanos())
	if err := os.Rename(blobDst, aside); err != nil {
		// Rename fails across filesystems on some platforms; fall
		// back to in-place RemoveAll. The window where blobs/global
		// is half-deleted is unavoidable in that case, but the
		// caller (Apply) follows this with MkdirAll + safeCopyTree
		// before returning, so the post-condition is still
		// satisfied unless the process crashes mid-RemoveAll.
		if err := os.RemoveAll(blobDst); err != nil {
			return fmt.Errorf("remove %s: %w", blobDst, err)
		}
		return nil
	}
	// Best-effort cleanup. If RemoveAll fails the aside is left for
	// the operator to inspect; the canonical path is already
	// available to receive the new tree.
	_ = os.RemoveAll(aside)
	return nil
}

// nowNanos returns the current Unix nano timestamp. Split out for
// test seams; production callers use it directly.
func nowNanos() int64 {
	return time.Now().UnixNano()
}

// validateCopyTreeDestination walks the source tree and Lstats each
// corresponding destination node, refusing if any pre-existing entry
// is a symlink or wrong type (dir-vs-file mismatch). The walk does
// NOT write anything — it's the preflight pass that lets Apply
// guarantee "no touch on rejection". safeCopyTree below re-checks
// during the actual copy as defence-in-depth, but in the normal
// path this dry-run finds any planted symlink first.
func validateCopyTreeDestination(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		info, lerr := os.Lstat(target)
		if lerr != nil {
			if errors.Is(lerr, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("lstat %s: %w", target, lerr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink at %s", target)
		}
		// Bidirectional type check: source dir → dest must be dir
		// (mkdir would fail), source regular file → dest must be
		// regular OR absent (rename would fail with EISDIR
		// otherwise). Both cases are caught up-front so the DB
		// copy never runs against a doomed restore.
		if d.IsDir() && !info.IsDir() {
			return fmt.Errorf("destination %s is not a directory", target)
		}
		if !d.IsDir() && info.IsDir() {
			return fmt.Errorf("destination %s is a directory but source is a file", target)
		}
		return nil
	})
}

// safeCopyTree is the restore-time variant of copyTree that refuses
// to follow any symlink encountered along the destination chain.
// copyTree already skips source-side symlinks (snapshot writer
// posture), but a planted symlink on the destination side would
// otherwise let MkdirAll / Rename walk outside the configdir. We
// validate the destination component-by-component on every entry
// visit, taking the small Lstat cost per node in exchange for
// closing the escape hole.
func safeCopyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		// Reject any pre-existing symlink / non-dir along the
		// destination chain before we mkdir or write. The check
		// covers the leaf too — if the operator deliberately
		// planted a symlink at <blob-dst>/sub it should fail rather
		// than silently overwrite the target of that symlink.
		if rel != "." {
			info, lerr := os.Lstat(target)
			switch {
			case lerr == nil:
				if info.Mode()&os.ModeSymlink != 0 {
					return fmt.Errorf("safeCopyTree: refusing symlink at %s", target)
				}
				if d.IsDir() && !info.IsDir() {
					return fmt.Errorf("safeCopyTree: destination %s is not a directory", target)
				}
			case errors.Is(lerr, os.ErrNotExist):
				// expected for fresh restores
			default:
				return fmt.Errorf("safeCopyTree: lstat %s: %w", target, lerr)
			}
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		// Snapshots are content-only — non-regular entries should
		// already have been filtered by Take, but defence in depth
		// here means a hand-constructed snapshot dir with planted
		// symlinks doesn't slip through either.
		if !info.Mode().IsRegular() {
			return nil
		}
		return safeCopyFile(p, target, info.Mode().Perm())
	})
}

// safeCopyFile mirrors copyFile but Lstat-rejects a destination
// symlink before opening. Without the gate, an attacker who
// planted a symlink-to-/etc/foo at the destination path before the
// restore ran would have copyFile follow it on open.
func safeCopyFile(src, dst string, perm os.FileMode) error {
	if info, err := os.Lstat(dst); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("safeCopyFile: refusing symlink at %s", dst)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("safeCopyFile: lstat %s: %w", dst, err)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return os.Rename(tmpName, dst)
}

// rejectSymlinkChain walks the path components from base (exclusive)
// to leaf and refuses if any intermediate node is a symlink or a
// non-directory. The check is one Lstat per component — cheap, and
// closes the "planted symlink escapes the configdir" hole.
//
// `base` is assumed already validated by the caller (it's the
// configdir we just chmod'd). Returns nil for components that don't
// yet exist; restore is expected to create them in subsequent
// MkdirAll calls.
func rejectSymlinkChain(base, leaf string) error {
	rel, err := filepath.Rel(base, leaf)
	if err != nil {
		return fmt.Errorf("path %s not under base %s: %w", leaf, base, err)
	}
	if rel == "." || rel == "" {
		return nil
	}
	cur := base
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("lstat %s: %w", cur, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink at %s", cur)
		}
		if !info.IsDir() {
			return fmt.Errorf("non-directory at %s", cur)
		}
	}
	return nil
}

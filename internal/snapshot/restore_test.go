package snapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
)

// takeFixture builds a snapshot rooted at a temp dir + returns its
// path. Helpers shared with snapshot_test.go would normally live in
// a *_test.go file together, but the existing test file uses a
// different fixture style; this builder is private to restore tests
// to keep the two suites independent.
func takeFixture(t *testing.T) (snapDir string, srcConfigDir string) {
	t.Helper()
	src := t.TempDir()
	// Open a store, write something so the snapshot has content.
	st, err := store.Open(context.Background(), store.Options{ConfigDir: src})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	// Seed a blob tree so the copy path is exercised. The agents
	// table is not touched here — VACUUM INTO is content-agnostic so
	// an empty schema is sufficient to validate the round-trip.
	blobPath := filepath.Join(src, "blobs", "global", "sub", "file.txt")
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o700); err != nil {
		t.Fatalf("mkdir blob: %v", err)
	}
	if err := os.WriteFile(blobPath, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write blob: %v", err)
	}

	dir, err := Take(context.Background(), st, filepath.Join(src, "blobs"), src, Options{HostHint: "test"})
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	return dir, src
}

func TestApply_HappyPath(t *testing.T) {
	snap, _ := takeFixture(t)
	target := t.TempDir()
	if err := Apply(snap, target, ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// DB must exist and verify against the manifest.
	if _, err := os.Stat(filepath.Join(target, DBFileName)); err != nil {
		t.Errorf("DB missing in target: %v", err)
	}
	// Blob tree must be restored intact.
	got, err := os.ReadFile(filepath.Join(target, "blobs", "global", "sub", "file.txt"))
	if err != nil {
		t.Fatalf("read restored blob: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("blob content mismatch: %q", got)
	}
}

func TestApply_RefusesOverwriteWithoutForce(t *testing.T) {
	snap, _ := takeFixture(t)
	target := t.TempDir()
	// Lay down a sentinel DB so the existence check trips.
	if err := os.WriteFile(filepath.Join(target, DBFileName), []byte("existing"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := Apply(snap, target, ApplyOptions{Force: false})
	if err == nil {
		t.Fatalf("Apply: want error on existing target")
	}
	// Sentinel must still be intact — Apply must not have written
	// anything before failing the overwrite check.
	body, err := os.ReadFile(filepath.Join(target, DBFileName))
	if err != nil {
		t.Fatalf("re-read sentinel: %v", err)
	}
	if string(body) != "existing" {
		t.Errorf("sentinel overwritten despite refusal: %q", body)
	}
}

func TestApply_ForceAllowsOverwrite(t *testing.T) {
	snap, _ := takeFixture(t)
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, DBFileName), []byte("existing"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Apply(snap, target, ApplyOptions{Force: true}); err != nil {
		t.Fatalf("Apply with Force: %v", err)
	}
	// Sentinel must have been overwritten — the new DB is a real
	// SQLite file, not literal "existing".
	body, err := os.ReadFile(filepath.Join(target, DBFileName))
	if err != nil {
		t.Fatalf("re-read after restore: %v", err)
	}
	if string(body) == "existing" {
		t.Errorf("sentinel survived Force restore")
	}
}

func TestApply_FailsOnCorruptedDB(t *testing.T) {
	snap, _ := takeFixture(t)
	// Tamper with the snapshot DB so the sha256 verification fails.
	if err := os.WriteFile(filepath.Join(snap, DBFileName), []byte("corrupt"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	target := t.TempDir()
	err := Apply(snap, target, ApplyOptions{})
	if err == nil {
		t.Fatalf("Apply: want error on corrupted snapshot")
	}
	// Target must NOT have a DB — the verify failure happens before
	// any copy operation.
	if _, err := os.Stat(filepath.Join(target, DBFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("target got a DB despite verify failure (err=%v)", err)
	}
}

func TestApply_FailsOnMissingManifest(t *testing.T) {
	snap, _ := takeFixture(t)
	// Drop the manifest — corrupted / abandoned snapshots
	// shouldn't be applyable.
	if err := os.Remove(filepath.Join(snap, ManifestFileName)); err != nil {
		t.Fatalf("rm manifest: %v", err)
	}
	target := t.TempDir()
	if err := Apply(snap, target, ApplyOptions{}); err == nil {
		t.Fatalf("Apply: want error on manifest-less snapshot")
	}
}

func TestApply_EmptyArgsFail(t *testing.T) {
	if err := Apply("", "/tmp/x", ApplyOptions{}); err == nil {
		t.Errorf("missing srcDir: want error")
	}
	if err := Apply("/tmp/x", "", ApplyOptions{}); err == nil {
		t.Errorf("missing target: want error")
	}
}

func TestApply_RefusesMissingBlobScope(t *testing.T) {
	snap, _ := takeFixture(t)
	// Manifest claims "global" scope but we delete the dir from
	// under it — simulates a truncated rsync that dropped the blob
	// tree but kept the manifest.
	if err := os.RemoveAll(filepath.Join(snap, "blobs", "global")); err != nil {
		t.Fatalf("rm blobs: %v", err)
	}
	target := t.TempDir()
	err := Apply(snap, target, ApplyOptions{})
	if err == nil {
		t.Fatalf("Apply: want error on missing global blob scope")
	}
	// Target must NOT have a DB — the manifest-vs-srcdir check
	// happens before any copy.
	if _, sErr := os.Stat(filepath.Join(target, DBFileName)); !errors.Is(sErr, os.ErrNotExist) {
		t.Errorf("target got a DB despite missing scope (err=%v)", sErr)
	}
}

func TestApply_RefusesTargetSymlink(t *testing.T) {
	snap, _ := takeFixture(t)
	target := t.TempDir()
	// Plant blobs as a symlink pointing outside the target. A
	// naive copyTree call would have written restored blobs into
	// /tmp/escape; the restore must refuse.
	escape := t.TempDir()
	if err := os.Symlink(escape, filepath.Join(target, "blobs")); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}
	err := Apply(snap, target, ApplyOptions{})
	if err == nil {
		t.Fatalf("Apply: want error when target blobs is a symlink")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("err should mention symlink: %v", err)
	}
}

func TestApply_RefusesNestedTargetSymlink(t *testing.T) {
	snap, _ := takeFixture(t)
	target := t.TempDir()
	// blobs/global is a real dir, but a nested sub-dir is a
	// symlink — the kind of trap a `--restore-force` re-seed
	// against a sabotaged target would need to detect.
	if err := os.MkdirAll(filepath.Join(target, "blobs", "global"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	escape := t.TempDir()
	if err := os.Symlink(escape, filepath.Join(target, "blobs", "global", "sub")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	// Re-seed with --force so the existence check doesn't trip first.
	if err := os.WriteFile(filepath.Join(target, DBFileName), []byte("old"), 0o600); err != nil {
		t.Fatalf("seed old DB: %v", err)
	}
	err := Apply(snap, target, ApplyOptions{Force: true})
	if err == nil {
		t.Fatalf("Apply: want error on nested target symlink")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("err should mention symlink: %v", err)
	}
	// The DB on disk must STILL be the seed value (preflight
	// rejection must happen before the DB copy). This pins the
	// "no touch on rejection" guarantee that the runbook depends
	// on.
	body, err := os.ReadFile(filepath.Join(target, DBFileName))
	if err != nil {
		t.Fatalf("re-read seeded DB: %v", err)
	}
	if string(body) != "old" {
		t.Errorf("DB overwritten despite preflight rejection: %q", body)
	}
	// Make sure the escape dir wasn't written to.
	entries, _ := os.ReadDir(escape)
	if len(entries) != 0 {
		t.Errorf("escape dir got %d entries: %v", len(entries), entries)
	}
}

func TestApply_RefusesTargetRootSymlink(t *testing.T) {
	snap, _ := takeFixture(t)
	// configdir parent
	parent := t.TempDir()
	realTarget := filepath.Join(parent, "real")
	if err := os.MkdirAll(realTarget, 0o700); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(realTarget, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	err := Apply(snap, link, ApplyOptions{})
	if err == nil {
		t.Fatalf("Apply: want error on symlink-as-target-root")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("err should mention symlink: %v", err)
	}
}

func TestApply_RefusesAuthSymlink(t *testing.T) {
	snap, _ := takeFixture(t)
	target := t.TempDir()
	escape := t.TempDir()
	if err := os.Symlink(escape, filepath.Join(target, "auth")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	err := Apply(snap, target, ApplyOptions{})
	if err == nil {
		t.Fatalf("Apply: want error on target auth symlink")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("err should mention symlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, DBFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("DB should not have been copied (preflight failed): %v", err)
	}
}

// TestApply_NoGlobalRefusesBlobsSymlink covers the no-global-manifest
// branch's symlink preflight: replaceBlobTree(blobDst) would Rename/
// RemoveAll into the symlink target if the chain check were gated on
// hasGlobalInManifest. Force the manifest to drop "global" by
// snapshotting an install with no blobs/global/.
func TestApply_NoGlobalRefusesBlobsSymlink(t *testing.T) {
	// Take a snapshot of an install with no blobs/global directory.
	src := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{ConfigDir: src})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	snap, err := Take(context.Background(), st, filepath.Join(src, "blobs"), src, Options{HostHint: "no-blobs"})
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	// Plant the symlink on the target side.
	target := t.TempDir()
	escape := t.TempDir()
	if err := os.WriteFile(filepath.Join(escape, "global"), []byte("survive"), 0o600); err != nil {
		t.Fatalf("seed escape: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(target, "blobs")), 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.Symlink(escape, filepath.Join(target, "blobs")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	err = Apply(snap, target, ApplyOptions{})
	if err == nil {
		t.Fatalf("Apply: want error on target blobs symlink under no-global snapshot")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("err should mention symlink: %v", err)
	}
	// Escape contents must survive (replaceBlobTree never ran).
	body, err := os.ReadFile(filepath.Join(escape, "global"))
	if err != nil {
		t.Errorf("escape file deleted: %v", err)
	} else if string(body) != "survive" {
		t.Errorf("escape file mutated: %q", body)
	}
	// DB must NOT have been laid down (preflight gate fired
	// before DB copy).
	if _, err := os.Stat(filepath.Join(target, DBFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("DB copied despite preflight rejection: %v", err)
	}
}

// TestApply_RemovesStaleBlobs pins the replace-semantics promise:
// the target already has a previous-Hub avatar; the snapshot doesn't
// know about it. After restore the orphan must be gone so the
// filesystem-fed blob.Store doesn't serve a stale image whose
// blob_refs row is no longer in the restored DB.
func TestApply_RemovesStaleBlobs(t *testing.T) {
	snap, _ := takeFixture(t)
	target := t.TempDir()
	// Seed a stale avatar on the target. This file is NOT in the
	// snapshot.
	staleDir := filepath.Join(target, "blobs", "global", "agents", "ag_stale")
	if err := os.MkdirAll(staleDir, 0o700); err != nil {
		t.Fatalf("mkdir stale: %v", err)
	}
	stalePath := filepath.Join(staleDir, "avatar.png")
	if err := os.WriteFile(stalePath, []byte("stale-bytes"), 0o600); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	// Seed an old DB so the existence check requires Force.
	if err := os.WriteFile(filepath.Join(target, DBFileName), []byte("old"), 0o600); err != nil {
		t.Fatalf("seed db: %v", err)
	}
	if err := Apply(snap, target, ApplyOptions{Force: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Snapshot's expected blob must be present.
	body, err := os.ReadFile(filepath.Join(target, "blobs", "global", "sub", "file.txt"))
	if err != nil {
		t.Errorf("expected blob missing: %v", err)
	} else if string(body) != "hello" {
		t.Errorf("blob content = %q", body)
	}
	// Stale blob must be gone.
	if _, err := os.Stat(stalePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale blob survived restore: err=%v", err)
	}
	// And the aside dir (if any) should also have been cleaned up.
	asides, _ := filepath.Glob(filepath.Join(target, "blobs", "global.pre-restore.*"))
	if len(asides) != 0 {
		t.Errorf("aside dir not cleaned up: %v", asides)
	}
}

func TestApply_PrecreatesAuthDir(t *testing.T) {
	snap, _ := takeFixture(t)
	target := t.TempDir()
	if err := Apply(snap, target, ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	authPath := filepath.Join(target, "auth")
	info, err := os.Stat(authPath)
	if err != nil {
		t.Fatalf("auth dir missing: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("auth path is not a directory: %v", info.Mode())
	}
	// Mode bits check is best-effort: on filesystems that don't
	// fully honour POSIX mode (e.g. some Windows configurations)
	// Chmod is a no-op. We only assert if Chmod is supported by
	// the underlying FS (POSIX mode bits non-zero past 0o400).
	if info.Mode().Perm()&0o077 != 0 && info.Mode().Perm() != 0o755 {
		t.Errorf("auth dir mode = %o, want 0700 (or platform default)", info.Mode().Perm())
	}
}

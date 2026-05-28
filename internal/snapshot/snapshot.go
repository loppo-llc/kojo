// Package snapshot implements docs/multi-device-storage.md §3.6's
// "snapshot 起点復旧" path: a periodic, atomic point-in-time copy of
// the Hub's authoritative state that a backup peer can take over.
//
// One snapshot is one directory under <configdir>/snapshots/<ts>/:
//
//	kojo.db                       — SQLite VACUUM INTO copy (consistent)
//	blobs/global/...              — copy of the global blob tree
//	manifest.json                 — schema version, ts, sha256 of kojo.db
//
// The KEK (auth/kek.bin) is intentionally NOT included — operators
// must back it up separately so a snapshot leak cannot decrypt
// secret kv rows on its own. Per-peer secrets (machine credentials,
// local-scope blobs) are also excluded; restoring those is the new
// Hub's own responsibility.
//
// This package owns the on-disk layout and the atomic creation
// guarantees; cron / rsync / failover scripting is out of scope (a
// future slice or operator runbook). All exposed functions are safe
// to call concurrently with running writers — VACUUM INTO takes a
// SQLite read lock that snapshots a consistent view, and the blob
// tree copy walks the filesystem.
package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// ManifestFileName lives in every snapshot dir and lists the things
// that should be there. Restore tooling reads this first to validate
// the snapshot before swapping it in.
const ManifestFileName = "manifest.json"

// DBFileName inside a snapshot dir mirrors the live store filename so
// a restore is "stop service, swap kojo.db, start service".
const DBFileName = "kojo.db"

// Manifest is the on-disk metadata file. Stable shape — restore
// tooling and human operators rely on it.
type Manifest struct {
	// Version of the manifest format itself. Bumped only when the
	// shape changes incompatibly.
	Version int `json:"version"`
	// Timestamp the snapshot started, RFC3339.
	StartedAt string `json:"started_at"`
	// SchemaVersion of kojo.db at the time of snapshot.
	SchemaVersion int `json:"schema_version"`
	// DBSHA256 over the snapshot's kojo.db file. Lets the restore
	// path verify the file wasn't truncated mid-rsync.
	DBSHA256 string `json:"db_sha256"`
	// DBSize bytes; redundant with sha256 but cheap and fast to check.
	DBSize int64 `json:"db_size"`
	// BlobScopes describes which blob scopes were captured; v1 always
	// only captures "global" (peer-shared content). "local"/"machine"
	// are per-peer and stay on the original Hub.
	BlobScopes []string `json:"blob_scopes"`
	// HostHint records the os.Hostname() at snapshot time. Purely
	// informational; restore on a different host is the whole point.
	HostHint string `json:"host_hint"`
}

const manifestVersion = 1

// Options narrows Take(). All fields are optional.
type Options struct {
	// HostHint overrides the manifest's host_hint field. Empty falls
	// back to os.Hostname().
	HostHint string
	// Now overrides the wall clock for tests. Zero means time.Now().
	Now time.Time
}

// Take creates a new snapshot under <root>/snapshots/<ts>/ and returns
// the absolute path to the directory. The directory layout is atomic
// in the sense that the manifest is written LAST — a crashed snapshot
// has no manifest and restore tooling treats manifest-less directories
// as in-progress / abandoned.
//
// `st` provides the SQLite database connection. `blobRoot` is the
// directory containing the live blob scope subdirs; only `blobRoot/global`
// is copied. `root` is the Hub's config dir (the function creates
// `root/snapshots/<ts>/` under it).
//
// On any failure mid-way the partially-written snapshot dir is left
// in place — the function does NOT clean up so an operator can
// inspect and recover. `kojo --clean snapshots` (Phase 6 #18)
// removes stale partials.
func Take(ctx context.Context, st *store.Store, blobRoot, root string, opts Options) (string, error) {
	if st == nil {
		return "", errors.New("snapshot.Take: nil store")
	}
	if blobRoot == "" || root == "" {
		return "", errors.New("snapshot.Take: blobRoot and root required")
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	tsName := now.UTC().Format("20060102T150405Z")
	dst := filepath.Join(root, "snapshots", tsName)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return "", fmt.Errorf("snapshot.Take: mkdir %s: %w", dst, err)
	}

	// 1. SQLite consistent copy via VACUUM INTO. This is the canonical
	// online-backup primitive in modernc.org/sqlite — it takes a
	// read lock for the duration, writes a fully-consistent file,
	// and runs while writers continue (they queue behind the lock at
	// commit time).
	dbPath := filepath.Join(dst, DBFileName)
	if err := vacuumInto(ctx, st, dbPath); err != nil {
		return dst, fmt.Errorf("snapshot.Take: vacuum into: %w", err)
	}

	// 2. SHA-256 the produced db so restore can verify integrity.
	hash, size, err := hashFile(dbPath)
	if err != nil {
		return dst, fmt.Errorf("snapshot.Take: hash db: %w", err)
	}

	// 3. Copy the global blob tree if it exists. Local/machine scopes
	// are intentionally skipped — those carry per-peer secrets that
	// the new Hub cannot use.
	srcGlobal := filepath.Join(blobRoot, "global")
	dstGlobal := filepath.Join(dst, "blobs", "global")
	scopes := []string{}
	// Lstat (not Stat) so a symlink-to-dir at the blobs/global root
	// is rejected. Without this gate we'd record "global" in the
	// manifest while the snapshot's blobs/global is missing —
	// Apply would refuse the resulting snapshot as inconsistent.
	//
	// Non-existence is the only "skip silently" case (a fresh
	// install with no global blobs yet). Symlink, non-dir, and
	// permission errors all surface so an operator notices the
	// snapshot isn't capturing what it should.
	srcInfo, srcErr := os.Lstat(srcGlobal)
	switch {
	case srcErr == nil && srcInfo.IsDir() && srcInfo.Mode()&os.ModeSymlink == 0:
		if err := copyTree(srcGlobal, dstGlobal); err != nil {
			return dst, fmt.Errorf("snapshot.Take: copy blobs/global: %w", err)
		}
		scopes = append(scopes, "global")
	case srcErr == nil && srcInfo.Mode()&os.ModeSymlink != 0:
		return dst, fmt.Errorf("snapshot.Take: %s is a symlink (refusing to follow)", srcGlobal)
	case srcErr == nil && !srcInfo.IsDir():
		return dst, fmt.Errorf("snapshot.Take: %s is not a directory", srcGlobal)
	case srcErr != nil && !errors.Is(srcErr, fs.ErrNotExist):
		return dst, fmt.Errorf("snapshot.Take: lstat %s: %w", srcGlobal, srcErr)
	}

	// 4. Schema version for the manifest.
	schemaVer, err := st.SchemaVersion(ctx)
	if err != nil {
		return dst, fmt.Errorf("snapshot.Take: schema version: %w", err)
	}

	hostHint := opts.HostHint
	if hostHint == "" {
		hostHint, _ = os.Hostname()
	}

	manifest := Manifest{
		Version:       manifestVersion,
		StartedAt:     now.UTC().Format(time.RFC3339),
		SchemaVersion: schemaVer,
		DBSHA256:      hash,
		DBSize:        size,
		BlobScopes:    scopes,
		HostHint:      hostHint,
	}
	manPath := filepath.Join(dst, ManifestFileName)
	if err := writeJSONAtomic(manPath, manifest); err != nil {
		return dst, fmt.Errorf("snapshot.Take: manifest: %w", err)
	}

	return dst, nil
}

// LoadManifest reads and validates the manifest at dir/manifest.json.
// Used by restore tooling and `kojo snapshot list` (Phase 6).
func LoadManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestFileName))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("snapshot: parse manifest %s: %w", dir, err)
	}
	if m.Version != manifestVersion {
		return nil, fmt.Errorf("snapshot: manifest %s version %d not supported (want %d)", dir, m.Version, manifestVersion)
	}
	return &m, nil
}

// VerifyDB returns nil if dir/kojo.db hashes to the manifest's
// DBSHA256. Used after rsync / cp -R to confirm the copy is intact.
func VerifyDB(dir string) error {
	m, err := LoadManifest(dir)
	if err != nil {
		return err
	}
	hash, size, err := hashFile(filepath.Join(dir, DBFileName))
	if err != nil {
		return err
	}
	if size != m.DBSize {
		return fmt.Errorf("snapshot.Verify: db size %d != manifest %d", size, m.DBSize)
	}
	if hash != m.DBSHA256 {
		return fmt.Errorf("snapshot.Verify: db sha256 mismatch")
	}
	return nil
}

// --- internals -------------------------------------------------------

// vacuumInto runs `VACUUM INTO 'path'`. modernc.org/sqlite supports
// this and it's the simplest online-backup form: one statement, takes
// a read lock, writes a fully-consistent file. The destination must
// not already exist.
func vacuumInto(ctx context.Context, st *store.Store, path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("snapshot: target %s already exists", path)
	}
	// SQLite VACUUM INTO needs a literal path string with single
	// quotes escaped — they can't appear in our absolute path because
	// we control it, but escape just in case to fail loudly on a
	// surprise.
	if strings.ContainsAny(path, "'\x00") {
		return fmt.Errorf("snapshot: refusing path with quote/NUL: %q", path)
	}
	q := "VACUUM INTO '" + path + "'"
	_, err := st.DB().ExecContext(ctx, q)
	return err
}

// copyTree mirrors src into dst. Files are copied with the same mode;
// directories are created mode 0700 (snapshots inherit Hub's stricter
// posture). Symlinks are not followed — they would point at paths
// outside the snapshot anyway.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		// Skip non-regular entries (symlinks, devices, sockets) —
		// snapshots are content-only.
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFile(p, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

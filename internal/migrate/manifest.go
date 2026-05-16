package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ManifestSHA256 walks `root` in deterministic order and returns a single
// hex-encoded SHA256 digest covering every regular file's relative path,
// size, and content hash. This is the input to the pre/post manifest
// comparison in section 5.5 (steps 3 and 10).
//
// Determinism guarantees:
//   - Walk order is sorted by relative path (case-sensitive byte order).
//   - Symlinks are NOT followed (lstat). A symlink contributes its target
//     string but no file content. This matches the read-only contract: we
//     never traverse outside `root`.
//   - The lock file (`kojo.lock`) is excluded so v0 binary
//     starts/stops while we're idle do not invalidate the manifest.
//   - Sockets / fifos / device files are excluded (they should not exist in
//     a v0 config dir, and including them would make the manifest depend on
//     transient OS state).
//
// The output is a 64-char hex string, suitable for storing in
// LockFile.V0SHA256Manifest and CompleteFile.V0SHA256Manifest.
func ManifestSHA256(root string) (string, error) {
	if _, err := os.Stat(root); err != nil {
		return "", fmt.Errorf("manifest: stat %s: %w", root, err)
	}
	type entry struct {
		rel  string
		mode os.FileMode
		size int64
	}
	var files []entry

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Skip individual unreadable entries rather than aborting the
			// whole walk: a v0 dir may have one-off permission glitches and
			// we still want a manifest to detect *changes* between pre/post.
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		// Always ignore the v0 lock file; it changes every v0 start.
		if rel == "kojo.lock" {
			return nil
		}
		// Use forward slash in the manifest so a Windows v0 dir hashes the
		// same as the equivalent Unix layout.
		rel = filepath.ToSlash(rel)

		info, err := d.Info()
		if err != nil {
			return nil
		}
		mode := info.Mode()
		if mode.IsDir() {
			files = append(files, entry{rel: rel + "/", mode: mode, size: 0})
			return nil
		}
		// Symlinks: include the target string (lstat already gave us this is
		// a symlink); do not follow.
		if mode&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				target = ""
			}
			files = append(files, entry{rel: rel + "@", mode: mode, size: int64(len(target))})
			return nil
		}
		if !mode.IsRegular() {
			// devices, sockets, etc. — skip
			return nil
		}
		files = append(files, entry{rel: rel, mode: mode, size: info.Size()})
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("manifest: walk %s: %w", root, err)
	}

	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })

	hash := sha256.New()
	var rowBuf strings.Builder
	for _, e := range files {
		rowBuf.Reset()
		// "<rel>\0<size>\0<content_sha256>\n"
		rowBuf.WriteString(e.rel)
		rowBuf.WriteByte(0)
		fmt.Fprintf(&rowBuf, "%d", e.size)
		rowBuf.WriteByte(0)

		var contentSum string
		switch {
		case strings.HasSuffix(e.rel, "/"):
			contentSum = "" // directories have no content
		case strings.HasSuffix(e.rel, "@"):
			abs := filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(e.rel, "@")))
			target, err := os.Readlink(abs)
			if err != nil {
				return "", fmt.Errorf("manifest: readlink %s: %w", abs, err)
			}
			h := sha256.Sum256([]byte(target))
			contentSum = hex.EncodeToString(h[:])
		default:
			abs := filepath.Join(root, filepath.FromSlash(e.rel))
			sum, err := fileSHA256(abs)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					// File disappeared between WalkDir and our re-open;
					// treat as a manifest event by recording empty hash.
					contentSum = "missing"
				} else {
					return "", fmt.Errorf("manifest: hash %s: %w", abs, err)
				}
			} else {
				contentSum = sum
			}
		}
		rowBuf.WriteString(contentSum)
		rowBuf.WriteByte('\n')
		if _, err := hash.Write([]byte(rowBuf.String())); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// fileSHA256 hashes a single regular file, opening it O_RDONLY through the
// v0 read-only guard. Buffered streaming so multi-GB v0 dirs don't blow the
// heap.
func fileSHA256(path string) (string, error) {
	f, err := readOnlyOpen(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

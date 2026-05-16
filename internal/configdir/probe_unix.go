//go:build unix

package configdir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Probe reports whether another process currently holds the kojo.lock file
// inside `dir`. Unlike Acquire, Probe is non-destructive:
//
//   - it never creates the lock file (returns held=false on ENOENT)
//   - it opens the file O_RDONLY so the v0 dir is never opened for write
//   - it never updates the file's mtime / inode
//   - if it manages to grab the advisory lock, it releases it before
//     returning so the holder's view is unchanged
//
// This is the only safe way to ask "is v0 currently running?" from the v1
// migration code path; Acquire would clobber the lock file's metadata and
// could even create one inside an empty v0 dir.
func Probe(dir string) (held bool, err error) {
	path := filepath.Join(dir, lockFileName)
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("probe %s: %w", path, err)
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// EWOULDBLOCK / EAGAIN means another process holds the lock.
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return true, nil
		}
		return false, fmt.Errorf("probe flock %s: %w", path, err)
	}
	// We grabbed it; immediately release so the real holder (if any) is
	// unaffected.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		return false, fmt.Errorf("probe unlock %s: %w", path, err)
	}
	return false, nil
}

//go:build windows

package configdir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Probe reports whether another process currently holds the kojo.lock file
// inside `dir`. Non-destructive: read-only open, no creation, lock released
// immediately if acquired. See probe_unix.go for the rationale.
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

	h := windows.Handle(f.Fd())
	var ol windows.Overlapped
	if err := windows.LockFileEx(h,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &ol,
	); err != nil {
		// ERROR_LOCK_VIOLATION == another holder. Anything else is real.
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return true, nil
		}
		return false, fmt.Errorf("probe LockFileEx %s: %w", path, err)
	}
	if err := windows.UnlockFileEx(h, 0, 1, 0, &ol); err != nil {
		return false, fmt.Errorf("probe UnlockFileEx %s: %w", path, err)
	}
	return false, nil
}

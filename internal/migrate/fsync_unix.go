//go:build !windows

package migrate

import "os"

// fsyncDir flushes the directory entry on POSIX. Required by atomicWrite to
// guarantee the rename is persisted across crashes.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

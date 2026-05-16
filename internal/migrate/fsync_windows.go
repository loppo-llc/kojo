//go:build windows

package migrate

// fsyncDir is a no-op on Windows. NTFS does not expose a portable directory
// fsync, and Windows guarantees that MoveFile(Ex) is durable to the parent
// volume's metadata journal once it returns. Documented divergence vs POSIX.
func fsyncDir(dir string) error {
	_ = dir
	return nil
}

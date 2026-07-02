// Package fsyncdir centralizes kojo's directory-fsync recipe: open a
// directory read-only and Sync it so a preceding rename/unlink is durable
// across a crash. It replaces four near-identical copies that previously
// lived in internal/{blob,oplog,agent,migrate}.
//
// The single entry point is Dir, which propagates real fsync errors and
// treats only platform "unsupported" errors as non-fatal. An earlier
// DirLenient variant that swallowed every Sync error was removed once its
// last caller (internal/agent's memory truncate) switched to Dir so real
// durability failures surface.
package fsyncdir

import "os"

// Dir fsyncs the directory at path. It opens the directory (O_RDONLY) and
// calls Sync. Filesystems / platforms that do not support fsync on a
// directory handle (Windows, some network filesystems) report an
// "unsupported" error, which is treated as a non-fatal best-effort success:
// the preceding rename is already on disk. Real errors are propagated.
//
// This matches the semantics historically used by internal/blob,
// internal/oplog and internal/agent.
func Dir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		// Windows / some network filesystems refuse fsync on directory
		// handles — non-fatal; the rename is still on disk.
		if isUnsupported(err) {
			return nil
		}
		return err
	}
	return nil
}

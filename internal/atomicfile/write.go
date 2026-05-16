package atomicfile

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// WriteJSON atomically writes data as indented JSON to path using a temp file + rename.
func WriteJSON(path string, data any, perm os.FileMode) error {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return WriteBytes(path, b, perm)
}

// WriteBytes atomically writes raw bytes to path using a unique temp
// file in the same directory + rename. A concurrent reader sees
// either the prior file (or ENOENT) or the new file in full — never
// a partially-written truncation. Two concurrent WriteBytes calls on
// the same path each get a distinct tmp via os.CreateTemp, so neither
// can clobber the other's tmp before rename — only the rename itself
// races, and the loser's content is still atomically visible to any
// reader that catches the file between the two renames.
//
// perm applies after rename. CreateTemp opens with 0600; we Chmod
// before rename so the final inode lands with the requested perm
// regardless of the OS's umask handling.
func WriteBytes(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	f, err := os.CreateTemp(dir, base+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpPath)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

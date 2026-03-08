package atomicfile

import (
	"encoding/json"
	"os"
)

// WriteJSON atomically writes data as indented JSON to path using a temp file + rename.
func WriteJSON(path string, data any, perm os.FileMode) error {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

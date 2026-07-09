//go:build windows

package selfupdate

import (
	"fmt"
	"os"
)

// SwapExecutable installs newBin as the running executable.
//
// Windows refuses to overwrite a running .exe, but does allow renaming
// it out of the way. Sequence:
//
//  1. Remove any leftover "<exe>.old" from a previous update.
//  2. Rename the current exe to "<exe>.old" (releases the path name
//     while the process keeps running from the open handle).
//  3. Rename newBin into the original path; if that fails (cross-
//     volume), copy into a temp file IN the target directory then
//     rename that temp over target. Never write target directly —
//     a torn mid-copy would leave a truncated exe and block the
//     restore rename (target already exists).
//
// The .old file stays until CleanupStaleBinaries runs after the next
// restart (the previous image is still mapped until then).
func SwapExecutable(newBin string) error {
	target, err := resolveExecPath()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	oldPath := target + ".old"
	_ = os.Remove(oldPath)
	if err := os.Rename(target, oldPath); err != nil {
		return fmt.Errorf("rename running exe to .old: %w", err)
	}
	// Prefer rename so newBin itself moves into place (no leftover
	// temp). Fall back to same-dir temp+rename when source and dest
	// are on different volumes — Windows rename cannot cross devices.
	// After step 2, target is free, so rename(temp, target) always
	// succeeds when both share a filesystem.
	if err := os.Rename(newBin, target); err != nil {
		if copyErr := swapViaTempCopy(newBin, target); copyErr != nil {
			// Drop any partial target so oldPath can reclaim the name
			// (os.Rename fails if the destination already exists).
			_ = os.Remove(target)
			_ = os.Rename(oldPath, target)
			return fmt.Errorf("install new executable: %w", copyErr)
		}
	}
	return nil
}

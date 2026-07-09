//go:build !windows

package selfupdate

import "fmt"

// SwapExecutable installs newBin as the running executable.
//
// On Unix a running binary can be unlinked/overwritten while still
// mapped, so we copy newBin into a temp file beside the target and
// os.Rename over it (atomic, same filesystem). See deployBuiltBinary
// for the same pattern used by the in-tree rebuild path.
func SwapExecutable(newBin string) error {
	target, err := resolveExecPath()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	return swapViaTempCopy(newBin, target)
}

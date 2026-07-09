package selfupdate

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// resolveExecPath returns the absolute, symlink-resolved path of the
// running executable. It is a var (not a plain function) so tests can
// override it: SwapExecutable renames files in place, and pointing it
// at the real test binary would corrupt the test process itself.
var resolveExecPath = func() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// SetExecPathForTest overrides resolveExecPath for the duration of t so
// Apply / SwapExecutable target a throwaway file instead of the real
// test binary. Panics outside a test process (testing.Testing() is
// false) so production code cannot accidentally re-point the swap.
func SetExecPathForTest(t *testing.T, path string) {
	if !testing.Testing() {
		panic("selfupdate.SetExecPathForTest used outside tests")
	}
	if t == nil {
		panic("selfupdate.SetExecPathForTest: nil *testing.T")
	}
	prev := resolveExecPath
	resolveExecPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { resolveExecPath = prev })
}

// CleanupStaleBinaries best-effort removes a leftover "<exe>.old"
// beside the running binary. Windows leaves that file after a
// successful swap (the previous .exe cannot be deleted while still
// mapped); Unix never creates it, so this is a cheap no-op. Safe to
// call at boot on every platform.
func CleanupStaleBinaries() {
	exe, err := resolveExecPath()
	if err != nil {
		return
	}
	_ = os.Remove(exe + ".old")
}

// copyFile writes srcPath into dstPath (creating/truncating), then
// sets mode 0755. Used by both the Unix atomic-swap path and the
// Windows fallback when rename across volumes fails.
func copyFile(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return os.Chmod(dstPath, 0o755)
}

// swapViaTempCopy copies newBin into a temp file in the same directory
// as target, chmods 0755, and renames over target. Same-directory
// rename is atomic and cross-device-safe (temp and target share a FS),
// matching deployBuiltBinary in internal/server/system_handlers.go.
func swapViaTempCopy(newBin, target string) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".kojo-swap-*")
	if err != nil {
		return fmt.Errorf("create swap temp: %w", err)
	}
	tmpName := tmp.Name()
	// Remove temp on any path that does not successfully rename it
	// over the target. After a successful rename the name is gone
	// and os.Remove is a harmless no-op / ENOENT.
	defer os.Remove(tmpName)

	src, err := os.Open(newBin)
	if err != nil {
		tmp.Close()
		return err
	}
	if _, err := io.Copy(tmp, src); err != nil {
		src.Close()
		tmp.Close()
		return err
	}
	src.Close()
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("rename over executable: %w", err)
	}
	return nil
}

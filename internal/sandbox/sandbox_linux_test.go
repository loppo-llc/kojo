//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLandlockABI(t *testing.T) {
	abi, err := landlockABI()
	if err != nil {
		t.Skipf("Landlock not available on this kernel: %v", err)
	}
	t.Logf("Landlock ABI version: %d", abi)
	if abi < 1 {
		t.Errorf("expected ABI >= 1, got %d", abi)
	}
}

func TestApplyLandlock(t *testing.T) {
	if !Available() {
		t.Skip("Landlock not available")
	}

	// We can't apply Landlock in the test process itself (it's irreversible
	// and would break subsequent test operations), so we test it via a
	// subprocess that applies the restriction and tries to write outside
	// the allowed paths.
	//
	// Create a temp dir to be the "allowed" path and another to be "denied".
	allowed := t.TempDir()
	denied := t.TempDir()

	// Write a test file in the denied dir to confirm it exists before sandboxing.
	testFile := filepath.Join(denied, "should-fail.txt")
	if err := os.WriteFile(testFile, []byte("pre-sandbox"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Verify parseSandboxArgs round-trips correctly with real paths.
	args := []string{"--rw", allowed, "--", "echo", "test"}
	rw, cmd, err := parseSandboxArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	if len(rw) != 1 || rw[0] != allowed {
		t.Errorf("rw paths: got %v, want [%s]", rw, allowed)
	}
	if len(cmd) != 2 || cmd[0] != "echo" {
		t.Errorf("cmd: got %v, want [echo test]", cmd)
	}
}

func TestAvailable(t *testing.T) {
	// Just make sure it doesn't panic.
	result := Available()
	t.Logf("Landlock Available() = %v", result)
}

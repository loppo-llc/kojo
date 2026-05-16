//go:build linux

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// landlockHelperEnv switches a re-execed test binary into "helper mode": the
// process applies Landlock with the given allow/deny dirs and tries to write
// to each, encoding the result in its exit code. The parent test process
// reads back that exit code to verify enforcement actually happened.
const landlockHelperEnv = "KOJO_LANDLOCK_HELPER_MODE"

func TestLandlockABI(t *testing.T) {
	abi, err := landlockABI()
	if err != nil {
		t.Skipf("Landlock not available on this kernel: %v", err)
	}
	t.Logf("Landlock ABI version: %d (kojo requires >= %d)", abi, minLandlockABI)
	if abi < 1 {
		t.Errorf("expected ABI >= 1, got %d", abi)
	}
}

// TestAvailableRequiresMinABI documents that Available() now gates on
// minLandlockABI rather than ABI >= 1. Older kernels (5.13-6.1) report a
// non-zero ABI but lack LANDLOCK_ACCESS_FS_TRUNCATE, so the package treats
// them as unsupported to avoid silently degrading write isolation.
func TestAvailableRequiresMinABI(t *testing.T) {
	abi, err := landlockABI()
	if err != nil {
		t.Skipf("Landlock not available on this kernel: %v", err)
	}
	got := Available()
	want := abi >= minLandlockABI
	if got != want {
		t.Errorf("Available() = %v, want %v (abi=%d, min=%d)", got, want, abi, minLandlockABI)
	}
}

// TestMain dispatches into the Landlock helper subprocess when the env var
// is set, otherwise runs tests normally. Re-exec is the standard pattern
// for testing irreversible per-process kernel state — applying Landlock in
// the test process itself would taint every subsequent test in the binary
// because restrict_self has no inverse.
func TestMain(m *testing.M) {
	if os.Getenv(landlockHelperEnv) != "" {
		runLandlockHelper()
		// runLandlockHelper always exits.
	}
	os.Exit(m.Run())
}

// helperExit codes communicate enforcement state from the subprocess back
// to the parent test. Each is a distinct code so a mis-paired success/fail
// doesn't masquerade as the other.
const (
	helperExitSandboxAndAllowWroteFine      = 10 // good: write to allowed dir succeeded after Landlock
	helperExitSandboxAndDeniedWriteBlocked  = 11 // good: write to denied dir failed after Landlock
	helperExitSandboxAndDeniedWriteAccepted = 12 // bad : write to denied dir succeeded — enforcement broken
	helperExitSandboxAndAllowWriteFailed    = 13 // bad : write to allowed dir failed — over-restrictive
	helperExitApplyLandlockFailed           = 14 // env/test mistake — applyLandlock returned error
)

// runLandlockHelper is executed inside the re-execed test binary. The
// parent passes the allow path via argv[1], the denied path via argv[2],
// and one of "allowed" / "denied" via argv[3] selecting which write to
// attempt after applying Landlock.
func runLandlockHelper() {
	allowed := os.Args[1]
	denied := os.Args[2]
	mode := os.Args[3]

	if err := applyLandlock([]string{allowed}); err != nil {
		// Pre-Landlock failure — surface separately so the parent test
		// can distinguish "sandbox setup broke" from "sandbox didn't
		// enforce". Without distinct codes a regression in
		// applyLandlock would look like an enforcement bug.
		os.Stderr.WriteString("helper: applyLandlock failed: " + err.Error() + "\n")
		os.Exit(helperExitApplyLandlockFailed)
	}

	switch mode {
	case "allowed":
		err := os.WriteFile(filepath.Join(allowed, "ok.txt"), []byte("ok"), 0o644)
		if err != nil {
			os.Stderr.WriteString("helper: allowed-write failed: " + err.Error() + "\n")
			os.Exit(helperExitSandboxAndAllowWriteFailed)
		}
		os.Exit(helperExitSandboxAndAllowWroteFine)
	case "denied":
		err := os.WriteFile(filepath.Join(denied, "should-fail.txt"), []byte("bypass"), 0o644)
		if err != nil {
			// EACCES (or similar) — Landlock is doing its job.
			os.Exit(helperExitSandboxAndDeniedWriteBlocked)
		}
		os.Exit(helperExitSandboxAndDeniedWriteAccepted)
	default:
		os.Stderr.WriteString("helper: unknown mode: " + mode + "\n")
		os.Exit(99)
	}
}

// TestApplyLandlock_DeniesWritesOutsideAllowlist actually exercises
// applyLandlock end-to-end in a subprocess and asserts the kernel denies a
// write outside the allowlist. Without this, the enforcement code path
// could regress (e.g. wrong access mask, missing path-beneath rule,
// restrict_self never called) and no test would notice.
func TestApplyLandlock_DeniesWritesOutsideAllowlist(t *testing.T) {
	if !Available() {
		t.Skip("Landlock not available")
	}

	allowed := t.TempDir()
	denied := t.TempDir()

	// Sanity: both paths are writable before sandboxing. If this fails it's
	// a test infrastructure problem, not a Landlock problem.
	for _, p := range []string{allowed, denied} {
		if err := os.WriteFile(filepath.Join(p, "pre-sandbox.txt"), []byte("ok"), 0o644); err != nil {
			t.Fatalf("pre-sandbox write to %s failed: %v", p, err)
		}
	}

	execPath, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	t.Run("denied write is blocked", func(t *testing.T) {
		cmd := exec.Command(execPath, allowed, denied, "denied")
		cmd.Env = append(os.Environ(), landlockHelperEnv+"=1")
		out, err := cmd.CombinedOutput()
		var ec int
		if exitErr, ok := err.(*exec.ExitError); ok {
			ec = exitErr.ExitCode()
		} else if err == nil {
			ec = 0
		} else {
			t.Fatalf("subprocess error: %v\noutput:\n%s", err, out)
		}
		if ec != helperExitSandboxAndDeniedWriteBlocked {
			t.Fatalf("expected exit %d (denied-write blocked), got %d\noutput:\n%s",
				helperExitSandboxAndDeniedWriteBlocked, ec, out)
		}
	})

	t.Run("allowed write still succeeds", func(t *testing.T) {
		cmd := exec.Command(execPath, allowed, denied, "allowed")
		cmd.Env = append(os.Environ(), landlockHelperEnv+"=1")
		out, err := cmd.CombinedOutput()
		var ec int
		if exitErr, ok := err.(*exec.ExitError); ok {
			ec = exitErr.ExitCode()
		} else if err == nil {
			ec = 0
		} else {
			t.Fatalf("subprocess error: %v\noutput:\n%s", err, out)
		}
		if ec != helperExitSandboxAndAllowWroteFine {
			t.Fatalf("expected exit %d (allowed-write ok), got %d\noutput:\n%s",
				helperExitSandboxAndAllowWroteFine, ec, out)
		}
	})
}

// TestParseSandboxArgs_RealPaths keeps the original parseSandboxArgs round-
// trip coverage that used to live in TestApplyLandlock. The Landlock-
// enforcement test above no longer touches parseSandboxArgs, so this stands
// alone to document that --rw and -- separator parsing handle absolute
// temp dirs without dropping segments.
func TestParseSandboxArgs_RealPaths(t *testing.T) {
	allowed := t.TempDir()
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

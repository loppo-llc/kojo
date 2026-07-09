package selfupdate

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSwapExecutable_replacesBytes(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "kojo")
	if err := os.WriteFile(current, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBin := filepath.Join(dir, "new")
	want := []byte("new-binary-contents")
	if err := os.WriteFile(newBin, want, 0o755); err != nil {
		t.Fatal(err)
	}

	prev := resolveExecPath
	resolveExecPath = func() (string, error) { return current, nil }
	t.Cleanup(func() { resolveExecPath = prev })

	if err := SwapExecutable(newBin); err != nil {
		t.Fatalf("SwapExecutable: %v", err)
	}
	got, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("current = %q, want %q", got, want)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(current)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o111 == 0 {
			t.Fatalf("mode = %v, want executable", fi.Mode())
		}
	}
}

func TestCleanupStaleBinaries_removesOld(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "kojo")
	if err := os.WriteFile(current, []byte("running"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := current + ".old"
	if err := os.WriteFile(old, []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}

	prev := resolveExecPath
	resolveExecPath = func() (string, error) { return current, nil }
	t.Cleanup(func() { resolveExecPath = prev })

	CleanupStaleBinaries()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("expected .old removed, stat err = %v", err)
	}
	// Running path must stay.
	if _, err := os.Stat(current); err != nil {
		t.Fatalf("current missing after cleanup: %v", err)
	}
}

func TestCleanupStaleBinaries_absentIsOK(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "kojo")
	if err := os.WriteFile(current, []byte("running"), 0o755); err != nil {
		t.Fatal(err)
	}

	prev := resolveExecPath
	resolveExecPath = func() (string, error) { return current, nil }
	t.Cleanup(func() { resolveExecPath = prev })

	// No .old planted — must not panic or error.
	CleanupStaleBinaries()
}

package configdir

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPathHasV1Suffix(t *testing.T) {
	got := Path()
	if !strings.HasSuffix(got, v1DirName) {
		t.Errorf("Path() = %q, want suffix %q", got, v1DirName)
	}
	if strings.Contains(filepath.Base(got), v0DirName+string(filepath.Separator)) {
		t.Errorf("Path() = %q must not nest v0 dir", got)
	}
}

func TestV0PathHasNoV1Suffix(t *testing.T) {
	got := V0Path()
	if strings.HasSuffix(got, v1DirName) {
		t.Errorf("V0Path() = %q, must not be the v1 dir", got)
	}
	if !strings.HasSuffix(got, v0DirName) {
		t.Errorf("V0Path() = %q, want suffix %q", got, v0DirName)
	}
}

func TestV0AndV1AreSiblings(t *testing.T) {
	v0 := V0Path()
	v1 := V1Path()
	if filepath.Dir(v0) != filepath.Dir(v1) {
		t.Errorf("v0=%q v1=%q should share parent dir", v0, v1)
	}
	if v0 == v1 {
		t.Errorf("v0 and v1 paths must differ, both = %q", v0)
	}
}

func TestXDGConfigHomeHonored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG_CONFIG_HOME is a POSIX convention")
	}
	abs := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", abs)
	got := defaultPath(v1DirName)
	want := filepath.Join(abs, v1DirName)
	if got != want {
		t.Errorf("defaultPath = %q, want %q", got, want)
	}
}

func TestXDGConfigHomeIgnoredWhenRelative(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG_CONFIG_HOME is a POSIX convention")
	}
	t.Setenv("XDG_CONFIG_HOME", "relative/path")
	got := defaultPath(v1DirName)
	if strings.HasPrefix(got, "relative/path") {
		t.Errorf("relative XDG_CONFIG_HOME must be ignored, got %q", got)
	}
}

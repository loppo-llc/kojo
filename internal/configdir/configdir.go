// Package configdir resolves the on-disk location for kojo's configuration
// directory.
//
// Starting with v1, kojo uses a major-version-suffixed directory name so that
// breaking storage changes can ship without touching the previous-major data
// in place. The v0 directory remains read-only from v1 binaries; v1 reads it
// during the one-shot import (`internal/migrate`) and afterwards only in
// narrow runtime fallback paths (e.g. session reattach via the v0
// `sessions.json` so live tmux panes from a v0 daemon stay reachable —
// gated on `v1Complete && v0Exists` in `cmd/kojo/main.go`). Boot does NOT
// re-walk the v0 manifest: once `migration_complete.json` exists v1 is
// canonical and v0 is a rollback snapshot — divergence is the operator's
// problem to resolve, not a boot blocker (a missing v0 dir post-cleanup is
// also the expected steady state). The manifest comparison still happens,
// but only inside `kojo --clean v0` (cmd/kojo/clean_v0.go), which refuses
// to soft-delete a diverged v0 dir without `--clean-force`. See docs §5.8 /
// §5.9 for the safety gates and the still-deferred sub-targets
// (--keep-blobs, --hard-delete, --purge-trash, 7-day auto-sweep).
//
// Path resolution (no override):
//
//	macOS:                       ~/.config/kojo-v1/             (v1) ; ~/.config/kojo/             (v0)
//	Linux (XDG_CONFIG_HOME=$X):  $X/kojo-v1/                    (v1) ; $X/kojo/                    (v0)
//	Linux (XDG unset):           ~/.config/kojo-v1/             (v1) ; ~/.config/kojo/             (v0)
//	Windows:                     %APPDATA%\kojo-v1\             (v1) ; %APPDATA%\kojo\             (v0)
//
// `kojo` (the bare name) is permanently the v0 path. Future v2 will use
// `kojo-v2`. Never collapse the suffix back to the bare name; doing so would
// silently overwrite v0 data.
package configdir

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// Major version suffix. Bump when introducing a breaking on-disk schema.
const (
	v0DirName = "kojo"
	v1DirName = "kojo-v1"
)

var (
	setOnce  sync.Once
	override string
)

// Set overrides the v1 config directory path. Must be called at most once,
// before any subsystem accesses Path(). Subsequent calls are ignored so the
// resolved directory cannot change under a running process.
//
// Set does NOT affect V0Path(); v0 path resolution always follows the platform
// defaults so migration tooling cannot be redirected away from real user data
// by an accidental --config-dir flag.
func Set(path string) {
	setOnce.Do(func() {
		override = path
	})
}

// Path returns the v1 configuration directory.
//
//   - If Set() was called, that path
//   - Otherwise the platform default for the current major version (v1)
func Path() string {
	if override != "" {
		return override
	}
	return defaultPath(v1DirName)
}

// DefaultPath returns the v1 platform-default config directory, ignoring any
// override. Exposed so callers (e.g. --dev mode) can derive a sibling dir.
func DefaultPath() string {
	return defaultPath(v1DirName)
}

// V0Path returns the v0 configuration directory (the legacy `kojo` dir),
// honoring XDG_CONFIG_HOME / APPDATA the same way v0 itself did. This is the
// only entry point migration tooling (and the v0-dir cleanup target
// landed in slice 29) should use to locate v0 data.
//
// V0Path is unaffected by Set(): v0 layout is fixed by history.
func V0Path() string {
	return defaultPath(v0DirName)
}

// V1Path is an alias for Path() that reads better at call sites that also
// touch V0Path() (e.g. migration code).
func V1Path() string {
	return Path()
}

func defaultPath(name string) string {
	switch runtime.GOOS {
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, name)
		}
	default:
		// XDG Base Directory: only honor XDG_CONFIG_HOME when it is set to an
		// absolute path. The spec says relative values must be ignored.
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" && filepath.IsAbs(xdg) {
			return filepath.Join(xdg, name)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), name)
	}
	return filepath.Join(home, ".config", name)
}

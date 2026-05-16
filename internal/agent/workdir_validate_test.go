package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestValidateWorkDir_RejectsInsideAgentsRoot covers the Copilot-flagged
// sandbox bypass where an agent could PATCH its own WorkDir to point at
// another agent's data directory (or the agents root itself), causing the
// next backend launch to add that path to the Landlock allowlist.
//
// Each case constructs a path that resolves inside, equal to, or above
// the agents root and asserts validateWorkDir rejects it. A "safe sibling
// directory" case verifies that legitimate external WorkDirs still pass.
func TestValidateWorkDir_RejectsInsideAgentsRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := agentsDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir agents root: %v", err)
	}

	// Build a directory representing another agent's data dir.
	otherAgentDir := filepath.Join(root, "ag_other_test")
	if err := os.MkdirAll(otherAgentDir, 0o755); err != nil {
		t.Fatalf("mkdir other agent dir: %v", err)
	}

	// Sibling directory under HOME but outside the agents root — should pass.
	sibling := filepath.Join(home, "projects", "demo")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}

	// Symlink that resolves inside the agents root — should be rejected
	// even though its literal path is outside.
	symlinkOutside := filepath.Join(home, "agent-link")
	if err := os.Symlink(otherAgentDir, symlinkOutside); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"equal to agents root", root, true},
		{"inside agents root (sibling agent)", otherAgentDir, true},
		{"deeper inside another agent's dir", filepath.Join(otherAgentDir, "memory"), true},
		{"ancestor of agents root (HOME)", home, true},
		{"symlink resolving inside agents root", symlinkOutside, true},
		{"safe sibling directory outside root", sibling, false},
		{"relative path always rejected", "relative/dir", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// "deeper inside another agent's dir" requires the dir to exist.
			if tc.path != "" && tc.path != "relative/dir" {
				_ = os.MkdirAll(tc.path, 0o755)
			}
			err := validateWorkDir(tc.path)
			if tc.wantErr && err == nil {
				t.Errorf("validateWorkDir(%q) = nil, want error", tc.path)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateWorkDir(%q) = %v, want nil", tc.path, err)
			}
		})
	}
}

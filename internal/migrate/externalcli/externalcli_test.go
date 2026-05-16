package externalcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHash is a deterministic stand-in for claudeEncodePath. Replaces
// path separators with "-" so we can predict the output in assertions.
func fakeHash(s string) string {
	return strings.ReplaceAll(s, string(filepath.Separator), "-")
}

func TestPlanSymlinksHappyPath(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "claude", "projects")
	v0Dir := filepath.Join(root, "v0", "agents", "ag_1")
	v1Dir := filepath.Join(root, "v1", "agents", "ag_1")

	// Pretend claude already has a project dir for v0.
	v0Hash := fakeHash(v0Dir)
	if err := os.MkdirAll(filepath.Join(projects, v0Hash), 0o700); err != nil {
		t.Fatalf("mkdir v0 hash: %v", err)
	}

	ops := PlanSymlinks(
		[]CLISpec{{Name: "claude", ProjectsRoot: projects, Hash: fakeHash}},
		[]PlanInput{{AgentID: "ag_1", V0Dir: v0Dir, V1Dir: v1Dir}},
	)
	if len(ops) != 1 {
		t.Fatalf("len(ops)=%d, want 1", len(ops))
	}
	if ops[0].Kind != OpSymlink {
		t.Errorf("Kind=%q", ops[0].Kind)
	}
	if !strings.HasSuffix(ops[0].Path, fakeHash(v1Dir)) {
		t.Errorf("Path=%q", ops[0].Path)
	}
}

func TestPlanSymlinksSkipsMissingTarget(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "claude", "projects")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ops := PlanSymlinks(
		[]CLISpec{{Name: "claude", ProjectsRoot: projects, Hash: fakeHash}},
		[]PlanInput{{AgentID: "ag_1",
			V0Dir: filepath.Join(root, "v0/agents/ag_1"),
			V1Dir: filepath.Join(root, "v1/agents/ag_1"),
		}},
	)
	if len(ops) != 0 {
		t.Errorf("expected no ops when target missing, got %v", ops)
	}
}

func TestPlanSymlinksSkipsExistingLink(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "claude", "projects")
	v0Dir := filepath.Join(root, "v0/agents/ag_1")
	v1Dir := filepath.Join(root, "v1/agents/ag_1")
	v0Hash := fakeHash(v0Dir)
	v1Hash := fakeHash(v1Dir)
	if err := os.MkdirAll(filepath.Join(projects, v0Hash), 0o700); err != nil {
		t.Fatalf("mkdir v0: %v", err)
	}
	// Already-present link path: must be left alone.
	if err := os.MkdirAll(filepath.Join(projects, v1Hash), 0o700); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}
	ops := PlanSymlinks(
		[]CLISpec{{Name: "claude", ProjectsRoot: projects, Hash: fakeHash}},
		[]PlanInput{{AgentID: "ag_1", V0Dir: v0Dir, V1Dir: v1Dir}},
	)
	if len(ops) != 0 {
		t.Errorf("PlanSymlinks must skip when link path exists, got %v", ops)
	}
}

func TestApplyForwardCreatesSymlinkAndPersistsManifest(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "claude", "projects")
	v0Dir := filepath.Join(root, "v0/agents/ag_1")
	v1Dir := filepath.Join(root, "v1/agents/ag_1")
	v0Hash := fakeHash(v0Dir)
	if err := os.MkdirAll(filepath.Join(projects, v0Hash), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	v1Root := filepath.Join(root, "v1")
	if err := os.MkdirAll(v1Root, 0o700); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}

	plan := PlanSymlinks(
		[]CLISpec{{ProjectsRoot: projects, Hash: fakeHash}},
		[]PlanInput{{AgentID: "ag_1", V0Dir: v0Dir, V1Dir: v1Dir}},
	)
	m, warnings := ApplyForward(v1Root, plan)
	if len(warnings) != 0 {
		t.Errorf("warnings = %v", warnings)
	}
	if len(m.Ops) != 1 {
		t.Fatalf("manifest ops = %d, want 1", len(m.Ops))
	}
	// Symlink must be readable as such.
	target, err := os.Readlink(m.Ops[0].Path)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if target != filepath.Join(projects, v0Hash) {
		t.Errorf("symlink target = %q", target)
	}
	// Manifest persists.
	loaded, err := LoadManifest(v1Root)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(loaded.Ops) != 1 {
		t.Errorf("loaded ops = %d", len(loaded.Ops))
	}
}

func TestRollbackRemovesSymlinks(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "claude", "projects")
	v0Dir := filepath.Join(root, "v0/agents/ag_1")
	v1Dir := filepath.Join(root, "v1/agents/ag_1")
	v0Hash := fakeHash(v0Dir)
	v0Path := filepath.Join(projects, v0Hash)
	if err := os.MkdirAll(v0Path, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Mark the v0 dir with a sentinel file so we can later confirm it
	// wasn't deleted along with the symlink.
	sentinel := filepath.Join(v0Path, "important.txt")
	if err := os.WriteFile(sentinel, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}
	v1Root := filepath.Join(root, "v1")
	if err := os.MkdirAll(v1Root, 0o700); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}

	plan := PlanSymlinks(
		[]CLISpec{{ProjectsRoot: projects, Hash: fakeHash}},
		[]PlanInput{{AgentID: "ag_1", V0Dir: v0Dir, V1Dir: v1Dir}},
	)
	if _, w := ApplyForward(v1Root, plan); len(w) != 0 {
		t.Fatalf("forward warnings: %v", w)
	}
	warnings, err := Rollback(v1Root)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("rollback warnings: %v", warnings)
	}
	// Symlink gone.
	if _, err := os.Lstat(filepath.Join(projects, fakeHash(v1Dir))); !os.IsNotExist(err) {
		t.Errorf("symlink still present: %v", err)
	}
	// v0 dir + sentinel survive.
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("v0 sentinel was destroyed by rollback: %v", err)
	}
}

func TestRollbackSkipsLinkPointingElsewhere(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "claude", "projects")
	v0Dir := filepath.Join(root, "v0/agents/ag_1")
	v1Dir := filepath.Join(root, "v1/agents/ag_1")
	v0Path := filepath.Join(projects, fakeHash(v0Dir))
	v1Path := filepath.Join(projects, fakeHash(v1Dir))
	if err := os.MkdirAll(v0Path, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	v1Root := filepath.Join(root, "v1")
	if err := os.MkdirAll(v1Root, 0o700); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}

	plan := PlanSymlinks(
		[]CLISpec{{ProjectsRoot: projects, Hash: fakeHash}},
		[]PlanInput{{AgentID: "ag_1", V0Dir: v0Dir, V1Dir: v1Dir}},
	)
	if _, w := ApplyForward(v1Root, plan); len(w) != 0 {
		t.Fatalf("forward warnings: %v", w)
	}
	// Operator-replaced the symlink with a different one.
	if err := os.Remove(v1Path); err != nil {
		t.Fatalf("remove orig: %v", err)
	}
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatalf("mkdir other: %v", err)
	}
	if err := os.Symlink(other, v1Path); err != nil {
		t.Fatalf("relink: %v", err)
	}
	warnings, err := Rollback(v1Root)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	// Expect a warning, but the link must still be there (we don't
	// destroy operator state).
	if len(warnings) == 0 {
		t.Error("expected warning when link target diverges")
	}
	if _, err := os.Lstat(v1Path); err != nil {
		t.Errorf("link removed despite divergence: %v", err)
	}
}

func TestGeminiProjectAddAndRollback(t *testing.T) {
	root := t.TempDir()
	v1Root := filepath.Join(root, "v1")
	if err := os.MkdirAll(v1Root, 0o700); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}
	projectsPath := filepath.Join(root, "gemini", "projects.json")
	if err := os.MkdirAll(filepath.Dir(projectsPath), 0o700); err != nil {
		t.Fatalf("mkdir gemini: %v", err)
	}
	// Seed: v0 entry exists.
	v0Key := "/old/agents/ag_1"
	v1Key := "/new/agents/ag_1"
	doc := map[string]any{
		"projects": map[string]any{v0Key: "kojo-ag_1"},
	}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(projectsPath, data, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	op := Op{Kind: OpGeminiProjectAdd, Path: projectsPath, Target: v1Key, AgentID: "ag_1"}

	m, w := ApplyForward(v1Root, []Op{op})
	if len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}
	if len(m.Ops) != 1 {
		t.Fatalf("manifest ops: %d", len(m.Ops))
	}
	// Re-read projects.json to confirm v1 entry exists with same value.
	raw, _ := os.ReadFile(projectsPath)
	var got map[string]any
	json.Unmarshal(raw, &got)
	projs := got["projects"].(map[string]any)
	if projs[v1Key] != "kojo-ag_1" {
		t.Errorf("projects.json[v1Key] = %v, want kojo-ag_1", projs[v1Key])
	}

	if w, err := Rollback(v1Root); err != nil {
		t.Fatalf("Rollback: %v", err)
	} else if len(w) != 0 {
		t.Errorf("rollback warnings: %v", w)
	}
	raw, _ = os.ReadFile(projectsPath)
	json.Unmarshal(raw, &got)
	projs = got["projects"].(map[string]any)
	if _, ok := projs[v1Key]; ok {
		t.Errorf("v1Key still in projects.json after rollback: %v", projs)
	}
	if _, ok := projs[v0Key]; !ok {
		t.Error("v0 entry destroyed by rollback")
	}
}

func TestRollbackOnAbsentManifestIsNoop(t *testing.T) {
	v1Root := t.TempDir()
	w, err := Rollback(v1Root)
	if err != nil {
		t.Errorf("Rollback: %v", err)
	}
	if len(w) != 1 {
		t.Errorf("expected 1 warning ('no manifest'), got %v", w)
	}
}

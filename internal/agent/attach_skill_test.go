package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSyncAttachSkill_Install verifies the SKILL.md is written at
// the expected per-agent path with the staging directory mentioned
// verbatim — agents on non-claude backends rely on the system-prompt
// block carrying the same constant, so a drift between the two
// would silently break the contract.
func TestSyncAttachSkill_Install(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", "")

	agentID := "ag_skill_install"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	SyncAttachSkill(agentID, true, logger)

	skillPath := filepath.Join(agentDir(agentID), ".claude", "skills",
		attachSkillDirName, "SKILL.md")
	body, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("SKILL.md not written: %v", err)
	}
	str := string(body)
	if !strings.Contains(str, "name: kojo-attach") {
		t.Errorf("SKILL.md missing frontmatter name: %q", str[:80])
	}
	if !strings.Contains(str, ".kojo/attach") {
		t.Errorf("SKILL.md missing staging dir reference")
	}
}

// TestSyncAttachSkill_Disable removes a prior install and leaves
// the surrounding .claude/ tree intact so operator-disabling the
// feature does not nuke unrelated skill installs.
func TestSyncAttachSkill_Disable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", "")

	agentID := "ag_skill_toggle"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	SyncAttachSkill(agentID, true, logger)

	// Plant a sibling skill so we can prove RemoveAll only hit
	// kojo-attach/.
	siblingDir := filepath.Join(agentDir(agentID), ".claude", "skills", "sibling")
	if err := os.MkdirAll(siblingDir, 0o755); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}
	if err := os.WriteFile(filepath.Join(siblingDir, "SKILL.md"),
		[]byte("# sibling"), 0o644); err != nil {
		t.Fatalf("write sibling: %v", err)
	}

	SyncAttachSkill(agentID, false, logger)

	if _, err := os.Stat(filepath.Join(agentDir(agentID), ".claude", "skills",
		attachSkillDirName)); !os.IsNotExist(err) {
		t.Errorf("kojo-attach skill dir still present after disable: err=%v", err)
	}
	if _, err := os.Stat(siblingDir); err != nil {
		t.Errorf("sibling skill collateral damage: %v", err)
	}
}

// TestSyncAttachSkill_Idempotent calls sync twice with the same
// inputs and asserts the file is identical — a race where the second
// call left a half-written body would cause claude to load garbled
// markdown.
func TestSyncAttachSkill_Idempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", "")

	agentID := "ag_skill_idemp"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	SyncAttachSkill(agentID, true, logger)
	skillPath := filepath.Join(agentDir(agentID), ".claude", "skills",
		attachSkillDirName, "SKILL.md")
	first, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	SyncAttachSkill(agentID, true, logger)
	second, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("SKILL.md body changed across idempotent sync")
	}
}

func TestSyncAttachSkillForTool_CodexInstallsCodexSkill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", "")

	agentID := "ag_skill_codex"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	SyncAttachSkillForTool(agentID, "codex", true, logger)

	skillPath := filepath.Join(agentDir(agentID), ".codex", "skills",
		attachSkillDirName, "SKILL.md")
	body, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("codex SKILL.md not written: %v", err)
	}
	if !strings.Contains(string(body), "name: kojo-attach") {
		t.Errorf("codex SKILL.md missing frontmatter name")
	}
}

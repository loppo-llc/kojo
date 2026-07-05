package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadClaudeSessionFiles_RecordsOversizedSkip pins the Task C
// contract: an oversized session JSONL is excluded from the transfer
// payload AND recorded as a structured skip (path + reason + size)
// instead of only a warn log. The oversized file is created sparse
// (Truncate) so the test doesn't actually write 32 MiB.
func TestReadClaudeSessionFiles_RecordsOversizedSkip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))

	agentID := "ag_skip_test"
	absDir, err := filepath.Abs(AgentDir(agentID))
	if err != nil {
		t.Fatalf("abs agent dir: %v", err)
	}
	projectDir := claudeProjectDir(absDir)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "small.jsonl"),
		[]byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write small: %v", err)
	}
	bigPath := filepath.Join(projectDir, "big.jsonl")
	f, err := os.Create(bigPath)
	if err != nil {
		t.Fatalf("create big: %v", err)
	}
	wantSize := int64(claudeSessionMaxBytes + 1)
	if err := f.Truncate(wantSize); err != nil {
		t.Fatalf("truncate big: %v", err)
	}
	_ = f.Close()

	files, skipped, err := ReadClaudeSessionFiles(agentID)
	if err != nil {
		t.Fatalf("ReadClaudeSessionFiles: %v", err)
	}
	if len(files) != 1 || files[0].SessionID != "small" {
		t.Fatalf("files = %+v; want only small", files)
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %+v; want exactly one entry", skipped)
	}
	got := skipped[0]
	if got.Path != "big.jsonl" || got.Reason != "oversized" || got.SizeBytes != wantSize {
		t.Errorf("skip = %+v; want {big.jsonl oversized %d}", got, wantSize)
	}
}

// TestBuildArrivalPrompt_NotesAppended covers the arrival-prompt
// caveat block: token auto re-issue, degraded flushes, and transfer
// skips must all surface to the agent; a clean transfer keeps the
// prompt byte-identical to the pre-notes shape.
func TestBuildArrivalPrompt_NotesAppended(t *testing.T) {
	ctx := context.Background()
	mgr := &Manager{} // nil store → generic fallback prompt

	clean := buildArrivalPrompt(ctx, mgr, "ag_x", "SRC", ArrivalNotes{})
	if strings.Contains(clean, "転移に関する注意") {
		t.Errorf("clean transfer must not carry a notes block: %q", clean)
	}

	notes := ArrivalNotes{
		TokenReissued:   true,
		DegradedFlushes: []string{"memory_flush"},
		TransferSkips: []SkippedSessionFile{
			{Path: "big.jsonl", Reason: "oversized", SizeBytes: 42},
			{Path: "main.json", Reason: "unreadable_ref"},
		},
	}
	got := buildArrivalPrompt(ctx, mgr, "ag_x", "SRC", notes)
	if !strings.HasPrefix(got, clean) {
		t.Errorf("notes must append after the base prompt;\nbase=%q\ngot=%q", clean, got)
	}
	for _, want := range []string{
		"転移に関する注意",
		"自動再発行済み",
		"memory_flush",
		"big.jsonl (oversized, 42 bytes)",
		"main.json (unreadable_ref)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

// TestDeviceSwitchSkillBodies_MentionDegradedRetry pins the Task B
// skill-text change: every flavor of the SKILL.md must tell the
// agent that memory_flush_failed / persona_flush_failed supports an
// opt-in degraded retry via "degraded": true.
func TestDeviceSwitchSkillBodies_MentionDegradedRetry(t *testing.T) {
	bodies := map[string]string{
		"claude_posix":   deviceSwitchSkillBody,
		"claude_windows": deviceSwitchSkillBodyWindows,
		"grok_posix":     deviceSwitchSkillBodyGrokPOSIX,
		"grok_windows":   deviceSwitchSkillBodyGrokWindows,
		"codex":          codexDeviceSwitchSkillBody(deviceSwitchSkillBodyGrokPOSIX),
	}
	for name, body := range bodies {
		for _, want := range []string{"memory_flush_failed", "persona_flush_failed", `"degraded": true`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s body missing %q", name, want)
			}
		}
	}
}

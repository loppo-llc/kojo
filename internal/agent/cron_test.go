package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCronPromptAt_Default(t *testing.T) {
	now := time.Date(2026, 4, 25, 15, 4, 0, 0, time.Local)
	next := time.Date(2026, 4, 25, 15, 34, 0, 0, time.Local)

	got := cronPromptAt(now, next, 10, "")

	if !strings.Contains(got, "[system message]") {
		t.Errorf("missing system header: %q", got)
	}
	if !strings.Contains(got, "2026年4月25日 15:04") {
		t.Errorf("missing localized timestamp: %q", got)
	}
	if !strings.Contains(got, "今回のタイムアウトは10分") {
		t.Errorf("missing timeout: %q", got)
	}
	if !strings.Contains(got, "memory/2026-04-25.md") {
		t.Errorf("missing default memory instruction with today's date: %q", got)
	}
}

func TestCronPromptAt_CustomReplacesDefault(t *testing.T) {
	now := time.Date(2026, 4, 25, 15, 4, 0, 0, time.Local)
	custom := "本日({date})の予定を確認して。"

	got := cronPromptAt(now, time.Time{}, 10, custom)

	if strings.Contains(got, "memory/2026-04-25.md") {
		t.Errorf("custom message should replace default trailing instruction, got: %q", got)
	}
	if !strings.Contains(got, "本日(2026-04-25)の予定を確認して。") {
		t.Errorf("{date} not substituted: %q", got)
	}
	if !strings.Contains(got, "--- Instructions ---") {
		t.Errorf("custom section header missing (visual separator): %q", got)
	}
}

func TestCronPromptAt_WhitespaceOnlyTreatedAsEmpty(t *testing.T) {
	now := time.Date(2026, 4, 25, 15, 4, 0, 0, time.Local)

	got := cronPromptAt(now, time.Time{}, 10, "  \n\t  ")

	if !strings.Contains(got, "memory/2026-04-25.md") {
		t.Errorf("whitespace-only custom message should fall back to default, got: %q", got)
	}
}

func TestCronPromptAt_NoNextRun(t *testing.T) {
	now := time.Date(2026, 4, 25, 15, 4, 0, 0, time.Local)

	got := cronPromptAt(now, time.Time{}, 10, "")

	if strings.Contains(got, "次回のチェックイン") {
		t.Errorf("zero nextRun should suppress next-run clause, got: %q", got)
	}
}

// TestReadCheckinFile guards the error contract that cron / Manager.Checkin
// rely on: an absent file is the only condition under which the default
// check-in prompt may be silently substituted. Any other I/O error (broken
// symlink, unreadable entry, etc.) must propagate so that callers can abort
// instead of executing a default that would violate the operator's
// configured rules.
func TestReadCheckinFile(t *testing.T) {
	t.Run("absent file returns empty without error", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		agentID := "ag_readcheckin_absent"
		if err := os.MkdirAll(agentDir(agentID), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		got, err := readCheckinFile(agentID)
		if err != nil {
			t.Fatalf("absent file should not error, got: %v", err)
		}
		if got != "" {
			t.Errorf("absent file should return empty content, got: %q", got)
		}
	})

	t.Run("custom content is returned verbatim", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		agentID := "ag_readcheckin_custom"
		if err := os.MkdirAll(agentDir(agentID), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		content := "custom check-in instructions\n"
		if err := os.WriteFile(filepath.Join(agentDir(agentID), "checkin.md"), []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		got, err := readCheckinFile(agentID)
		if err != nil {
			t.Fatalf("readable file should not error, got: %v", err)
		}
		if got != content {
			t.Errorf("content mismatch: got %q want %q", got, content)
		}
	})

	t.Run("broken symlink propagates error instead of silent fallback", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		agentID := "ag_readcheckin_broken_symlink"
		if err := os.MkdirAll(agentDir(agentID), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// Point checkin.md at a nonexistent target. ReadFile follows the
		// symlink and fails with ENOENT, but Lstat sees the link itself
		// exists — the read error must propagate, not be coerced to "absent".
		linkPath := filepath.Join(agentDir(agentID), "checkin.md")
		if err := os.Symlink(filepath.Join(home, "missing-target"), linkPath); err != nil {
			t.Skipf("symlink unsupported on this platform: %v", err)
		}

		_, err := readCheckinFile(agentID)
		if err == nil {
			t.Errorf("broken symlink should propagate error, got nil")
		}
	})

	t.Run("unreadable directory entry propagates error", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		agentID := "ag_readcheckin_isdir"
		// Make checkin.md a directory: ReadFile will fail with EISDIR but
		// the entry exists, so the error must propagate (not silent fallback).
		dirAsFile := filepath.Join(agentDir(agentID), "checkin.md")
		if err := os.MkdirAll(dirAsFile, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		_, err := readCheckinFile(agentID)
		if err == nil {
			t.Errorf("directory-shaped checkin.md should propagate error, got nil")
		}
	})
}

// TestReadCheckinFileOrDefault ensures the UI/API surface inherits the same
// "absent vs unreadable" classification as readCheckinFile. A broken symlink
// must surface as an error here too — otherwise the UI would show the default
// template while cron/manual check-ins abort, leaving the operator unable to
// see what was wrong from the settings screen.
func TestReadCheckinFileOrDefault(t *testing.T) {
	t.Run("absent file returns default template", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		agentID := "ag_readcheckindef_absent"
		if err := os.MkdirAll(agentDir(agentID), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		content, isDefault, err := ReadCheckinFileOrDefault(agentID)
		if err != nil {
			t.Fatalf("absent file should not error, got: %v", err)
		}
		if !isDefault {
			t.Errorf("absent file should mark isDefault=true")
		}
		if content != DefaultCheckinContent {
			t.Errorf("absent file should return default template, got: %q", content)
		}
	})

	t.Run("custom content is returned with isDefault=false", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		agentID := "ag_readcheckindef_custom"
		if err := os.MkdirAll(agentDir(agentID), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		content := "custom check-in instructions"
		if err := os.WriteFile(filepath.Join(agentDir(agentID), "checkin.md"), []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		got, isDefault, err := ReadCheckinFileOrDefault(agentID)
		if err != nil {
			t.Fatalf("readable file should not error, got: %v", err)
		}
		if isDefault {
			t.Errorf("custom content should mark isDefault=false")
		}
		if got != content {
			t.Errorf("content mismatch: got %q want %q", got, content)
		}
	})

	t.Run("whitespace-only file is treated as absent", func(t *testing.T) {
		// UI must show the default template (isDefault=true) when the file
		// is blank, because cronPromptAt / checkinPrompt also TrimSpace the
		// body and fall back to the default. Otherwise the settings screen
		// would say "custom" while the actual prompt runs the default.
		home := t.TempDir()
		t.Setenv("HOME", home)
		agentID := "ag_readcheckindef_blank"
		if err := os.MkdirAll(agentDir(agentID), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(agentDir(agentID), "checkin.md"), []byte("   \n\t\n  "), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		content, isDefault, err := ReadCheckinFileOrDefault(agentID)
		if err != nil {
			t.Fatalf("blank file should not error, got: %v", err)
		}
		if !isDefault {
			t.Errorf("whitespace-only file should mark isDefault=true")
		}
		if content != DefaultCheckinContent {
			t.Errorf("whitespace-only file should return default template, got: %q", content)
		}
	})

	t.Run("broken symlink propagates error instead of returning default", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		agentID := "ag_readcheckindef_broken"
		if err := os.MkdirAll(agentDir(agentID), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		linkPath := filepath.Join(agentDir(agentID), "checkin.md")
		if err := os.Symlink(filepath.Join(home, "missing-target"), linkPath); err != nil {
			t.Skipf("symlink unsupported on this platform: %v", err)
		}

		_, _, err := ReadCheckinFileOrDefault(agentID)
		if err == nil {
			t.Errorf("broken symlink should propagate error, got nil")
		}
	})
}

func TestWriteCheckinFile(t *testing.T) {
	t.Run("no size limit", func(t *testing.T) {
		// checkin.md has no size limit — large content should not be rejected.
		big := strings.Repeat("a", 10000)
		err := WriteCheckinFile("nonexistent-test-agent", big)
		// Write itself will fail (no dir), but there must be no "too long" error.
		if err != nil && strings.Contains(err.Error(), "too long") {
			t.Errorf("unexpected size error for large input: %v", err)
		}
	})
}

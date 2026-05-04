package agent

import (
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
	if !strings.Contains(got, "--- 指示 ---") {
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

func TestWriteCheckinFile(t *testing.T) {
	t.Run("rejects oversized input", func(t *testing.T) {
		too := strings.Repeat("a", MaxCheckinRunes+1)
		if err := WriteCheckinFile("nonexistent-test-agent", too); err == nil {
			t.Errorf("expected error for %d-rune input, got nil", len([]rune(too)))
		}
	})

	t.Run("counts runes not bytes", func(t *testing.T) {
		// 日本語は3バイトだが1ルーン。MaxCheckinRunes ルーンまで許容される。
		ok := strings.Repeat("あ", MaxCheckinRunes)
		// This will fail on the write (no dir exists) but validation should pass
		err := WriteCheckinFile("nonexistent-test-agent", ok)
		if err != nil && strings.Contains(err.Error(), "too long") {
			t.Errorf("unexpected size error for %d-rune input: %v", MaxCheckinRunes, err)
		}
	})
}

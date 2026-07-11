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
	if strings.Contains(got, "タイムアウト") {
		t.Errorf("timeout must not be shown to the model (fable5 guide): %q", got)
	}
	if !strings.Contains(got, "次回のチェックインは最短30分後 (15:34)") {
		t.Errorf("missing next-checkin schedule: %q", got)
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

func TestValidateCronMessage(t *testing.T) {
	t.Run("trims surrounding whitespace", func(t *testing.T) {
		got, err := validateCronMessage("  hello  ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "hello" {
			t.Errorf("got %q, want %q", got, "hello")
		}
	})

	t.Run("rejects oversized input", func(t *testing.T) {
		too := strings.Repeat("a", MaxCronMessageRunes+1)
		if _, err := validateCronMessage(too); err == nil {
			t.Errorf("expected error for %d-rune input, got nil", len(too))
		}
	})

	t.Run("counts runes not bytes", func(t *testing.T) {
		// 日本語は3バイトだが1ルーン。MaxCronMessageRunes ルーンまで許容される。
		ok := strings.Repeat("あ", MaxCronMessageRunes)
		if got, err := validateCronMessage(ok); err != nil {
			t.Errorf("unexpected error for %d-rune input: %v (got=%d)", MaxCronMessageRunes, err, len([]rune(got)))
		}
	})
}

func TestCronRunContext(t *testing.T) {
	// 0 = default timeout
	ctx, cancel, timeout := cronRunContext(0)
	defer cancel()
	if timeout != cronTimeout {
		t.Errorf("default: timeout = %v, want %v", timeout, cronTimeout)
	}
	if _, ok := ctx.Deadline(); !ok {
		t.Error("default: expected a deadline")
	}

	// >0 = explicit minutes
	ctx, cancel, timeout = cronRunContext(30)
	defer cancel()
	if timeout != 30*time.Minute {
		t.Errorf("explicit: timeout = %v, want 30m", timeout)
	}
	if _, ok := ctx.Deadline(); !ok {
		t.Error("explicit: expected a deadline")
	}

	// <0 = no timeout: unbounded context, zero duration reported
	ctx, cancel, timeout = cronRunContext(-1)
	if timeout != 0 {
		t.Errorf("unlimited: timeout = %v, want 0", timeout)
	}
	if _, ok := ctx.Deadline(); ok {
		t.Error("unlimited: expected no deadline")
	}
	// cancel must still work (context becomes done)
	cancel()
	select {
	case <-ctx.Done():
	default:
		t.Error("unlimited: cancel did not cancel the context")
	}
}

func TestValidTimeoutAllowsNoTimeout(t *testing.T) {
	if !ValidTimeout(-1) {
		t.Error("ValidTimeout(-1) = false, want true (no-timeout sentinel)")
	}
	if ValidTimeout(-2) {
		t.Error("ValidTimeout(-2) = true, want false")
	}
}

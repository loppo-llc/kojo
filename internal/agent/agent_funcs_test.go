package agent

import (
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFormatUntil(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"past returns 1分", -time.Minute, "1分"},
		{"zero returns 1分", 0, "1分"},
		{"29s rounds down to 1分", 29 * time.Second, "1分"},
		{"30s rounds to 1分", 30 * time.Second, "1分"},
		{"5m exact", 5 * time.Minute, "5分"},
		{"59m29s → 59分", 59*time.Minute + 29*time.Second, "59分"},
		{"59m30s → 1時間", 59*time.Minute + 30*time.Second, "1時間"},
		{"60m → 1時間", 60 * time.Minute, "1時間"},
		{"60m30s → 1時間1分", 60*time.Minute + 30*time.Second, "1時間1分"},
		{"2h15m", 2*time.Hour + 15*time.Minute, "2時間15分"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatUntil(now.Add(tt.d), now)
			if got != tt.want {
				t.Errorf("formatUntil(+%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestIntervalToCron(t *testing.T) {
	tests := []struct {
		name     string
		interval int
		agentID  string
		wantEmpty bool
	}{
		{"zero returns empty", 0, "ag_test", true},
		{"negative returns empty", -1, "ag_test", true},
		{"5 min produces sub-hourly", 5, "ag_test", false},
		{"10 min produces sub-hourly", 10, "ag_test", false},
		{"30 min produces sub-hourly", 30, "ag_test", false},
		{"60 min produces hourly", 60, "ag_test", false},
		{"180 min produces 3-hourly", 180, "ag_test", false},
		{"1440 min produces daily", 1440, "ag_test", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := intervalToCron(tt.interval, tt.agentID)
			if tt.wantEmpty && got != "" {
				t.Errorf("expected empty, got %q", got)
			}
			if !tt.wantEmpty && got == "" {
				t.Error("expected non-empty cron expression")
			}
		})
	}

	// Deterministic: same ID always produces same result
	a := intervalToCron(10, "ag_fixed")
	b := intervalToCron(10, "ag_fixed")
	if a != b {
		t.Errorf("expected deterministic output, got %q and %q", a, b)
	}

	// Different IDs may produce different offsets
	c := intervalToCron(10, "ag_other")
	// Not guaranteed different, but the function should at least return valid cron
	if c == "" {
		t.Error("expected non-empty for different ID")
	}
}

func TestNormalizeTimestamp(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"UTC timestamp", "2024-01-15T10:30:00Z"},
		{"with offset", "2024-01-15T10:30:00+09:00"},
		{"invalid returns as-is", "not-a-timestamp"},
		{"empty returns as-is", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeTimestamp(tt.input)
			if tt.input == "not-a-timestamp" || tt.input == "" {
				if got != tt.input {
					t.Errorf("expected %q unchanged, got %q", tt.input, got)
				}
			} else if got == "" {
				t.Error("expected non-empty normalized timestamp")
			}
		})
	}
}

func TestValidActiveHours(t *testing.T) {
	tests := []struct {
		name    string
		start   string
		end     string
		wantErr bool
	}{
		{"both empty is valid", "", "", false},
		{"valid range", "09:00", "17:00", false},
		{"overnight range", "22:00", "06:00", false},
		{"start only", "09:00", "", true},
		{"end only", "", "17:00", true},
		{"same values", "09:00", "09:00", true},
		{"invalid start format", "25:00", "17:00", true},
		{"invalid end format", "09:00", "99:99", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidActiveHours(tt.start, tt.end)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidActiveHours(%q, %q) error = %v, wantErr %v", tt.start, tt.end, err, tt.wantErr)
			}
		})
	}
}

func TestValidInterval(t *testing.T) {
	valid := []int{0, 5, 10, 30, 60, 180, 360, 720, 1440}
	for _, v := range valid {
		if !ValidInterval(v) {
			t.Errorf("expected %d to be valid", v)
		}
	}
	invalid := []int{-1, 1, 15, 20, 45, 90, 120, 240}
	for _, v := range invalid {
		if ValidInterval(v) {
			t.Errorf("expected %d to be invalid", v)
		}
	}
}

func TestValidEffort(t *testing.T) {
	valid := []string{"", "low", "medium", "high", "xhigh", "max"}
	for _, v := range valid {
		if !ValidEffort(v) {
			t.Errorf("expected %q to be valid", v)
		}
	}
	if ValidEffort("extreme") {
		t.Error("expected 'extreme' to be invalid")
	}
}

func TestValidModelEffort(t *testing.T) {
	// xhigh is valid for opus models
	for _, m := range []string{"opus", "claude-opus-4-7"} {
		if !ValidModelEffort(m, "xhigh") {
			t.Errorf("expected xhigh to be valid for %q", m)
		}
	}
	// xhigh is invalid for non-opus models
	for _, m := range []string{"sonnet", "claude-opus-4-6", "haiku", ""} {
		if ValidModelEffort(m, "xhigh") {
			t.Errorf("expected xhigh to be invalid for %q", m)
		}
	}
	// other effort levels are valid for any model
	for _, e := range []string{"", "low", "medium", "high", "max"} {
		if !ValidModelEffort("sonnet", e) {
			t.Errorf("expected %q to be valid for sonnet", e)
		}
	}
}

// TestBuildSystemPrompt_MemoryWriteDirective guards against accidentally
// softening the mandatory memory-write instructions. Persona instructions
// alone have proven unreliable — agents skip writes when they judge a turn
// "not important enough" — so the system prompt must contain an explicit,
// MANDATORY directive that names the diary file, covers short turns, and
// frames memory files as the only state that survives conversation reset.
func TestBuildSystemPrompt_MemoryWriteDirective(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	a := &Agent{ID: "ag_test_prompt"}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	prompt := buildSystemPrompt(a, logger, "", nil, false)

	today := time.Now().In(jst).Format("2006-01-02")
	mustContain := []string{
		"Memory Write — MANDATORY",
		"kojo will reset it automatically",
		"memory/" + today + ".md",
		"Short exchanges count",
		"MEMORY.md",
	}
	for _, s := range mustContain {
		if !strings.Contains(prompt, s) {
			t.Errorf("system prompt missing mandatory directive fragment %q", s)
		}
	}
}

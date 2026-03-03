package agent

import (
	"testing"
	"time"
)

func TestParseSchedule(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
		err   bool
	}{
		{"hourly", time.Hour, false},
		{"daily 09:00", 24 * time.Hour, false},
		{"daily", 24 * time.Hour, false},
		{"every 6h", 6 * time.Hour, false},
		{"every 30m", 30 * time.Minute, false},
		{"every 1h", time.Hour, false},
		{"bogus", 0, true},
		{"", 0, true},
	}

	for _, tt := range tests {
		got, err := parseSchedule(tt.input)
		if tt.err {
			if err == nil {
				t.Errorf("parseSchedule(%q) expected error, got %v", tt.input, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSchedule(%q) unexpected error: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseSchedule(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestShouldRun(t *testing.T) {
	now := time.Now()

	// Never run before → should run
	a := &Agent{Schedule: "hourly", Enabled: true}
	if !shouldRun(a, now) {
		t.Error("expected shouldRun=true for never-run agent")
	}

	// Ran 30 min ago with hourly schedule → should not run
	a.LastRunAt = now.Add(-30 * time.Minute).UTC().Format(time.RFC3339)
	if shouldRun(a, now) {
		t.Error("expected shouldRun=false for recently-run agent")
	}

	// Ran 2 hours ago with hourly schedule → should run
	a.LastRunAt = now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	if !shouldRun(a, now) {
		t.Error("expected shouldRun=true for overdue agent")
	}

	// No schedule → should not run
	a.Schedule = ""
	if shouldRun(a, now) {
		t.Error("expected shouldRun=false for no-schedule agent")
	}
}

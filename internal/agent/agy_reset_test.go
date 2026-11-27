package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeAgyTranscript(t *testing.T, convID string, idle time.Duration, lines ...string) string {
	t.Helper()
	path := agyTranscriptPath(convID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-idle)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
	return path
}

func agyPlanner(input, cached int) string {
	return fmt.Sprintf(`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","input_tokens":%d,"cache_read_tokens":%d,"output_tokens":10}`, input, cached)
}

func TestLastAgyContextTokens(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := writeAgyTranscript(t, agyIDA, 0,
		agyPlanner(3000, 200000),
		`{"step_index":2,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"PLANNER_RESPONSE"}`,
		agyPlanner(250000, 0),
		// A CHECKPOINT's counts are the compaction call over the old
		// context; the planner call after it is the real context size.
		`{"step_index":4,"source":"SYSTEM","type":"CHECKPOINT","input_tokens":2000,"cache_read_tokens":510000}`,
		agyPlanner(90000, 0),
		`{"step_index":6,"source":"MODEL","type":"GENERIC","content":"done"}`,
	)
	if got, ok := lastAgyContextTokens(path); !ok || got != 90000 {
		t.Fatalf("got %d,%v; want 90000,true", got, ok)
	}
	if _, ok := lastAgyContextTokens(path + ".missing"); ok {
		t.Fatal("missing transcript reported conclusive")
	}
}

func TestLastAgyContextTokensTailWindow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	filler := `{"type":"GENERIC","content":"` + strings.Repeat("x", 4000) + `"}`
	lines := []string{agyPlanner(1, 999999)}
	for i := 0; i < 400; i++ { // ~1.6 MiB, past the tail window
		lines = append(lines, filler)
	}
	path := writeAgyTranscript(t, agyIDA, 0, lines...)
	if _, ok := lastAgyContextTokens(path); ok {
		t.Fatal("tail window without a planner record must be inconclusive")
	}
	writeAgyTranscript(t, agyIDA, 0, append(lines, agyPlanner(1000, 120000))...)
	if got, ok := lastAgyContextTokens(path); !ok || got != 121000 {
		t.Fatalf("got %d,%v; want 121000,true", got, ok)
	}
}

func TestAgyConversationResetDue(t *testing.T) {
	cases := []struct {
		name       string
		ctx        int
		idle       time.Duration
		automated  bool
		sessionKey string
		guard      time.Duration
		want       bool
	}{
		{name: "warm under threshold", ctx: 390000, idle: time.Minute, automated: true, want: false},
		{name: "warm over threshold automated", ctx: 410000, idle: time.Minute, automated: true, want: true},
		{name: "cold over floor automated", ctx: 150000, idle: 15 * time.Minute, automated: true, want: true},
		{name: "cold under floor", ctx: 90000, idle: 2 * time.Hour, automated: true, want: false},
		{name: "warm 150k is not cold", ctx: 150000, idle: 5 * time.Minute, automated: true, want: false},
		{name: "interactive within guard", ctx: 450000, idle: 15 * time.Minute, guard: 30 * time.Minute, want: false},
		{name: "interactive past guard", ctx: 150000, idle: 45 * time.Minute, guard: 30 * time.Minute, want: true},
		{name: "keyed session resets", ctx: 450000, idle: time.Hour, automated: true, sessionKey: "slack:C:1", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			writeAgyTranscript(t, agyIDA, tc.idle, agyPlanner(1000, tc.ctx-1000))
			got := agyConversationResetDue(agyIDA, tc.sessionKey, tc.automated, "ag_agyresetdue", tc.guard, silentLogger())
			if got != tc.want {
				t.Errorf("reset = %v; want %v", got, tc.want)
			}
		})
	}
	t.Run("no transcript", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if agyConversationResetDue(agyIDA, "", true, "ag_agyresetdue", 0, silentLogger()) {
			t.Error("reset without a transcript to measure")
		}
	})
}

func TestDropAgyConversationKeepsSharedState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := agentDir("ag_agydropshared")
	writeAgyConversationID(dir, "", agyIDA, silentLogger())
	writeAgyConversationID(dir, "slack:C:1", agyIDA, silentLogger())
	db := seedAgyConversation(t, agyIDA)
	if !dropAgyConversation(dir, "", agyIDA, silentLogger()) {
		t.Fatal("drop failed")
	}
	if readAgyConversationID(dir, "") != "" {
		t.Error("main ref survived")
	}
	if _, err := os.Stat(db); err != nil {
		t.Errorf("conversation still referenced by a thread was removed: %v", err)
	}
}

func TestAgyChatResetsOversizedConversation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &Agent{ID: "ag_agychatreset"}
	dir := agentDir(a.ID)
	writeAgyConversationID(dir, "", agyIDA, silentLogger())
	writeAgyConversationExtra(dir, agyIDA, "", silentLogger())
	oldDB := seedAgyConversation(t, agyIDA)
	writeAgyTranscript(t, agyIDA, time.Hour, agyPlanner(2000, 200000))

	bin := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > '%s'\ncat >/dev/null\nprintf '%%s\\n' '{\"event\":\"init\",\"conversation_id\":\"%s\"}' '{\"event\":\"result\",\"result\":{\"conversation_id\":\"%s\",\"status\":\"SUCCESS\",\"response\":\"ok\"}}'\n", argsFile, agyIDB, agyIDB)
	if err := os.WriteFile(filepath.Join(bin, "agy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ch, err := NewAgyBackend(silentLogger()).Chat(context.Background(), a, "hello", "system", ChatOptions{AutomatedTrigger: true})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "--conversation") {
		t.Errorf("resumed an over-threshold conversation: %q", args)
	}
	if _, err := os.Stat(oldDB); !os.IsNotExist(err) {
		t.Errorf("old conversation state survived: %v", err)
	}
	if _, err := os.Stat(agyExtraFile(dir, agyIDA)); !os.IsNotExist(err) {
		t.Errorf("old extra record survived: %v", err)
	}
	if got := readAgyConversationID(dir, ""); got != agyIDB {
		t.Errorf("ref = %q; want new conversation %q", got, agyIDB)
	}
}

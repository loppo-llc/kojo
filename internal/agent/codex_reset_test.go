package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const codexResetTID = "019e7cc9-dd5e-7971-b654-7840c683879e"

func codexTokenCount(input int) string {
	return fmt.Sprintf(`{"timestamp":"2026-11-18T04:44:36.724Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":9999999},"last_token_usage":{"input_tokens":%d,"cached_input_tokens":0},"model_context_window":258400}}}`, input)
}

func writeCodexRollout(t *testing.T, idle time.Duration, lines ...string) string {
	t.Helper()
	if os.Getenv("CODEX_HOME") == "" || !strings.HasPrefix(os.Getenv("CODEX_HOME"), os.TempDir()) {
		t.Setenv("CODEX_HOME", t.TempDir())
	}
	path := filepath.Join(codexHome(), "sessions", "2026", "11", "18", "rollout-2026-11-18T13-44-23-"+codexResetTID+".jsonl")
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

func TestLastCodexContextTokens(t *testing.T) {
	path := writeCodexRollout(t, 0,
		codexTokenCount(150000),
		`{"type":"response_item","payload":{"type":"message","content":"token_count"}}`,
		codexTokenCount(180000),
		`{"type":"event_msg","payload":{"type":"token_count","info":null}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":0}}}}`,
	)
	if got, ok := lastCodexContextTokens(path); !ok || got != 180000 {
		t.Fatalf("got %d,%v; want 180000,true", got, ok)
	}
	path = writeCodexRollout(t, 0, codexTokenCount(230000), `{"type":"compacted","payload":{"message":""}}`)
	if got, ok := lastCodexContextTokens(path); !ok || got != 0 {
		t.Fatalf("after compaction got %d,%v; want 0,true", got, ok)
	}
}

func TestCodexRolloutUsable(t *testing.T) {
	good := writeCodexRollout(t, 0, codexTokenCount(1))
	name := filepath.Base(good)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, name), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(codexHome(), "sessions", "2026", "11", "19")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		good:               true,
		"":                 false,
		"relative/" + name: false,
		filepath.Join(filepath.Dir(good), "rollout-x-01a1ecf8-5e9a-7b61-aff0-3299a2e830ff.jsonl"): false,
		"/etc/passwd":                                      false,
		filepath.Join(outside, name):                       false,
		filepath.Join(codexHome(), name):                   false,
		filepath.Join(linkDir, name):                       false,
		filepath.Join(codexHome(), "sessions", "..", name): false,
	} {
		if got := codexRolloutUsable(path, codexResetTID); got != want {
			t.Errorf("%q: got %v want %v", path, got, want)
		}
	}
}

func TestCodexThreadResetDueRejectsSymlinkedRollout(t *testing.T) {
	stubCLIResetSummary(t, nil)
	target := writeCodexRollout(t, 4*time.Hour, codexTokenCount(300000))
	link := filepath.Join(filepath.Dir(target), "rollout-2026-11-18T13-44-24-"+codexResetTID+".jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	ref := &codexThreadRef{ThreadID: codexResetTID, RolloutPath: link}
	if codexThreadResetDue(ref, "", true, "ag_codexresetlink", 0, silentLogger()) {
		t.Fatal("followed a symlinked rollout")
	}
}

func TestDropCodexThreadRefForResetRechecksRef(t *testing.T) {
	id, _ := setupCodexTransferTest(t)
	writeCodexThreadRef(id, "", codexThreadRef{ThreadID: codexResetTID, Goal: &GoalBinding{}}, silentLogger())
	if dropped, err := dropCodexThreadRefForReset(id, "", codexResetTID); err != nil || dropped {
		t.Fatalf("dropped a goal-bound ref: %v %v", dropped, err)
	}
	other := "01a1ecf8-5e9a-7b61-aff0-3299a2e830ff"
	writeCodexThreadRef(id, "", codexThreadRef{ThreadID: other}, silentLogger())
	if dropped, err := dropCodexThreadRefForReset(id, "", codexResetTID); err != nil || dropped {
		t.Fatalf("dropped a ref that moved to another thread: %v %v", dropped, err)
	}
	writeCodexThreadRef(id, "", codexThreadRef{ThreadID: codexResetTID}, silentLogger())
	if dropped, err := dropCodexThreadRefForReset(id, "", codexResetTID); err != nil || !dropped {
		t.Fatalf("did not drop: %v %v", dropped, err)
	}
	if ref, _ := readCodexThreadRef(id, ""); ref != nil {
		t.Fatal("ref survived")
	}
}

func TestCodexThreadResetDue(t *testing.T) {
	cases := []struct {
		name       string
		ctx        int
		idle       time.Duration
		automated  bool
		goal       bool
		summaryErr error
		want       bool
	}{
		{name: "warm large thread left to codex", ctx: 240000, idle: time.Hour, automated: true, want: false},
		{name: "cold over floor", ctx: 120000, idle: 4 * time.Hour, automated: true, want: true},
		{name: "cold under floor", ctx: 90000, idle: 4 * time.Hour, automated: true, want: false},
		{name: "goal thread kept", ctx: 240000, idle: 4 * time.Hour, automated: true, goal: true, want: false},
		{name: "summary failure keeps", ctx: 240000, idle: 4 * time.Hour, automated: true, summaryErr: errors.New("boom"), want: false},
		{name: "interactive cold past guard", ctx: 240000, idle: 4 * time.Hour, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubCLIResetSummary(t, tc.summaryErr)
			ref := &codexThreadRef{ThreadID: codexResetTID, RolloutPath: writeCodexRollout(t, tc.idle, codexTokenCount(tc.ctx))}
			if tc.goal {
				ref.Goal = &GoalBinding{}
			}
			if got := codexThreadResetDue(ref, "", tc.automated, "ag_codexresetdue", 0, silentLogger()); got != tc.want {
				t.Errorf("reset = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestCodexChatStartsFreshThreadAfterColdReset(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture launcher uses POSIX shell")
	}
	for _, cold := range []bool{true, false} {
		t.Run(fmt.Sprintf("cold=%v", cold), func(t *testing.T) { runCodexResetChat(t, cold) })
	}
}

func runCodexResetChat(t *testing.T, cold bool) {
	stubCLIResetSummary(t, nil)
	id, _ := setupCodexTransferTest(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	calls := filepath.Join(dir, "calls")
	if err := os.WriteFile(state, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^TestGoalRPCProcess$ -- goal-rpc-fixture %q %q\n", exe, state, calls)
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	idle := 4 * time.Hour
	if !cold {
		idle = time.Minute
	}
	rollout := writeCodexRollout(t, idle, codexTokenCount(200000))
	writeCodexThreadRef(id, "", codexThreadRef{ThreadID: codexResetTID, RolloutPath: rollout}, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ev, err := NewCodexBackend(slog.Default()).Chat(ctx, &Agent{ID: id, Tool: ToolCodex}, "hello", "", ChatOptions{
		AutomatedTrigger:      true,
		RecentMessagesContext: "<recent-marker>\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	for e := range ev {
		if e.Type == "error" {
			t.Fatalf("backend error: %s", e.ErrorMessage)
		}
	}
	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	resumed := strings.Contains(string(log), `"thread/resume"`)
	if resumed == cold {
		t.Errorf("thread/resume called = %v; want %v:\n%s", resumed, !cold, log)
	}
	if got := strings.Count(string(log), "recent-marker"); got != map[bool]int{true: 1, false: 0}[cold] {
		t.Errorf("recent-messages bootstrap injected %d times (cold=%v)", got, cold)
	}
}

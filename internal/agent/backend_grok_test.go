package agent

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// silentLogger discards all logs in tests.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// collectGrokEvents runs parseGrokStream against the given lines and
// returns all ChatEvents that were emitted plus the final result.
func collectGrokEvents(t *testing.T, lines ...string) ([]ChatEvent, *grokStreamResult) {
	t.Helper()
	r := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var events []ChatEvent
	send := func(e ChatEvent) bool {
		events = append(events, e)
		return true
	}
	res := parseGrokStream(r, silentLogger(), send)
	return events, res
}

func stringSliceContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestParseGrokStream_TextOnly(t *testing.T) {
	events, res := collectGrokEvents(t,
		`{"type":"text","data":"Hi"}`,
		`{"type":"text","data":" there"}`,
		`{"type":"end","stopReason":"EndTurn","sessionId":"019e5527-7b78-70f3-b488-2f005dbcc2fe","requestId":"r1"}`,
	)
	if res.cancelled {
		t.Fatal("unexpected cancel")
	}
	if res.text != "Hi there" {
		t.Errorf("text = %q, want %q", res.text, "Hi there")
	}
	if res.thinking != "" {
		t.Errorf("thinking = %q, want empty", res.thinking)
	}
	if res.sessionID != "019e5527-7b78-70f3-b488-2f005dbcc2fe" {
		t.Errorf("sessionID = %q, want 019e5527-7b78-70f3-b488-2f005dbcc2fe", res.sessionID)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	for _, e := range events {
		if e.Type != "text" {
			t.Errorf("event type = %q, want text", e.Type)
		}
	}
}

func TestParseGrokStream_ThoughtAndText(t *testing.T) {
	events, res := collectGrokEvents(t,
		`{"type":"thought","data":"reasoning"}`,
		`{"type":"text","data":"answer"}`,
		`{"type":"end","stopReason":"EndTurn","sessionId":"s","requestId":"r"}`,
	)
	if res.thinking != "reasoning" {
		t.Errorf("thinking = %q", res.thinking)
	}
	if res.text != "answer" {
		t.Errorf("text = %q", res.text)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].Type != "thinking" || events[0].Delta != "reasoning" {
		t.Errorf("event[0] = %+v, want thinking/reasoning", events[0])
	}
	if events[1].Type != "text" || events[1].Delta != "answer" {
		t.Errorf("event[1] = %+v, want text/answer", events[1])
	}
}

func TestParseGrokStream_SkipsMalformedAndUnknown(t *testing.T) {
	_, res := collectGrokEvents(t,
		``,
		`not-json`,
		`{"type":"phase_changed","phase":"streaming"}`, // unknown type ignored
		`{"type":"text","data":"ok"}`,
		`{"type":"end","sessionId":"s"}`,
	)
	if res.text != "ok" {
		t.Errorf("text = %q, want ok", res.text)
	}
	if res.sessionID != "s" {
		t.Errorf("sessionID = %q", res.sessionID)
	}
}

func TestParseGrokStream_CancelStopsEarly(t *testing.T) {
	r := strings.NewReader(strings.Join([]string{
		`{"type":"text","data":"a"}`,
		`{"type":"text","data":"b"}`,
		`{"type":"text","data":"c"}`,
		`{"type":"end","sessionId":"s"}`,
	}, "\n") + "\n")
	var count int
	send := func(e ChatEvent) bool {
		count++
		return count < 2 // refuse the second event onward
	}
	res := parseGrokStream(r, silentLogger(), send)
	if !res.cancelled {
		t.Fatal("expected cancelled=true")
	}
	if count != 2 {
		t.Errorf("send called %d times, want 2", count)
	}
	// Only the accepted delta should be in res.text — parseGrokStream
	// snapshots the buffer at the cancel point.
	if res.text != "ab" {
		t.Errorf("text = %q, want %q (cancel snapshot includes the rejected delta)", res.text, "ab")
	}
}

func TestParseGrokStream_ToolCallStartedCompleted(t *testing.T) {
	events, res := collectGrokEvents(t,
		`{"type":"tool_call_started","tool_call_id":"call_1","tool_name":"read_file","tool_args_json":"{\"path\":\"go.mod\"}"}`,
		`{"type":"tool_call_completed","tool_call_id":"call_1","tool_name":"read_file","result":{"content":"module kojo"}}`,
		`{"type":"text","data":"done"}`,
	)
	if len(res.toolUses) != 1 {
		t.Fatalf("toolUses len = %d, want 1", len(res.toolUses))
	}
	tu := res.toolUses[0]
	if tu.ID != "call_1" || tu.Name != "read_file" {
		t.Errorf("tool use id/name = %q/%q, want call_1/read_file", tu.ID, tu.Name)
	}
	if tu.Input != `{"path":"go.mod"}` {
		t.Errorf("tool input = %q", tu.Input)
	}
	if !strings.Contains(tu.Output, "module kojo") {
		t.Errorf("tool output = %q, want module kojo", tu.Output)
	}

	var sawUse, sawResult bool
	for _, e := range events {
		switch e.Type {
		case "tool_use":
			if e.ToolUseID == "call_1" && e.ToolName == "read_file" {
				sawUse = true
			}
		case "tool_result":
			if e.ToolUseID == "call_1" && e.ToolName == "read_file" && strings.Contains(e.ToolOutput, "module kojo") {
				sawResult = true
			}
		}
	}
	if !sawUse {
		t.Error("expected tool_use event")
	}
	if !sawResult {
		t.Error("expected tool_result event")
	}
}

func TestParseGrokStream_ACPToolCall(t *testing.T) {
	events, res := collectGrokEvents(t,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"call_2","title":"Reading README","kind":"read","status":"pending","rawInput":{"path":"README.md"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_2","status":"completed","content":[{"type":"content","content":{"type":"text","text":"README contents"}}],"rawOutput":{"content":"README contents"}}}}`,
	)
	if len(res.toolUses) != 1 {
		t.Fatalf("toolUses len = %d, want 1", len(res.toolUses))
	}
	tu := res.toolUses[0]
	if tu.ID != "call_2" || tu.Name != "Reading README" {
		t.Errorf("tool use id/name = %q/%q, want call_2/Reading README", tu.ID, tu.Name)
	}
	if !strings.Contains(tu.Input, "README.md") {
		t.Errorf("tool input = %q, want README.md", tu.Input)
	}
	if tu.Output != "README contents" {
		t.Errorf("tool output = %q, want README contents", tu.Output)
	}
	if len(events) != 2 {
		t.Fatalf("events len = %d, want 2", len(events))
	}
	if events[0].Type != "tool_use" || events[1].Type != "tool_result" {
		t.Errorf("events = %+v, want tool_use then tool_result", events)
	}
}

func TestParseGrokStream_ACPMessageChunks(t *testing.T) {
	events, res := collectGrokEvents(t,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"answer"}}}}`,
	)
	if res.thinking != "thinking" {
		t.Errorf("thinking = %q, want thinking", res.thinking)
	}
	if res.text != "answer" {
		t.Errorf("text = %q, want answer", res.text)
	}
	if len(events) != 2 || events[0].Type != "thinking" || events[1].Type != "text" {
		t.Errorf("events = %+v, want thinking then text", events)
	}
}

func TestParseGrokStream_ACPProgressContentDoesNotCompleteTool(t *testing.T) {
	events, res := collectGrokEvents(t,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"call_3","title":"Searching","status":"pending","rawInput":{"query":"todo"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_3","status":"in_progress","content":[{"type":"content","content":{"type":"text","text":"Found 1 file"}}]}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_3","status":"completed","content":[{"type":"content","content":{"type":"text","text":"Final result"}}]}}}`,
	)
	if len(events) != 2 {
		t.Fatalf("events len = %d, want 2: %+v", len(events), events)
	}
	if events[0].Type != "tool_use" || events[1].Type != "tool_result" {
		t.Errorf("events = %+v, want tool_use then tool_result", events)
	}
	if events[1].ToolOutput != "Final result" {
		t.Errorf("tool result output = %q, want Final result", events[1].ToolOutput)
	}
	if len(res.toolUses) != 1 || res.toolUses[0].Output != "Final result" {
		t.Errorf("res.toolUses = %+v, want final output", res.toolUses)
	}
}

func TestParseGrokChatHistoryToolUses_CurrentTurnBasicTools(t *testing.T) {
	history := strings.Join([]string{
		`{"type":"system","content":"system prompt"}`,
		`{"type":"user","content":[{"type":"text","text":"previous turn"}]}`,
		`{"type":"assistant","content":"","tool_calls":[{"id":"call-old","name":"Read","arguments":"{\"path\":\"old.txt\"}"}]}`,
		`{"type":"tool_result","tool_call_id":"call-old","content":"old output"}`,
		`{"type":"user","content":[{"type":"text","text":"current turn"}]}`,
		`{"type":"assistant","content":"","tool_calls":[{"id":"call-read","name":"Read","arguments":"{\"path\":\"README.md\"}"},{"id":"call-write","name":"Write","arguments":"{\"path\":\"out.txt\",\"content\":\"ok\"}"}]}`,
		`{"type":"tool_result","tool_call_id":"call-read","content":"README contents"}`,
		`{"type":"tool_result","tool_call_id":"call-write","content":"The file out.txt has been written successfully."}`,
		`{"type":"assistant","content":"done"}`,
	}, "\n") + "\n"

	tools, err := parseGrokChatHistoryToolUses(strings.NewReader(history))
	if err != nil {
		t.Fatalf("parseGrokChatHistoryToolUses: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("toolUses len = %d, want 2: %+v", len(tools), tools)
	}
	if tools[0].ID != "call-read" || tools[0].Name != "Read" {
		t.Errorf("tools[0] id/name = %q/%q, want call-read/Read", tools[0].ID, tools[0].Name)
	}
	if tools[0].Input != `{"path":"README.md"}` {
		t.Errorf("tools[0].Input = %q", tools[0].Input)
	}
	if tools[0].Output != "README contents" {
		t.Errorf("tools[0].Output = %q", tools[0].Output)
	}
	if tools[1].ID != "call-write" || tools[1].Name != "Write" {
		t.Errorf("tools[1] id/name = %q/%q, want call-write/Write", tools[1].ID, tools[1].Name)
	}
	if !strings.Contains(tools[1].Input, `"path":"out.txt"`) {
		t.Errorf("tools[1].Input = %q, want out.txt", tools[1].Input)
	}
	if tools[1].Output != "The file out.txt has been written successfully." {
		t.Errorf("tools[1].Output = %q", tools[1].Output)
	}
	for _, tool := range tools {
		if tool.ID == "call-old" {
			t.Fatalf("old turn tool leaked into current turn: %+v", tools)
		}
	}
}

func TestParseGrokChatHistoryToolUses_FunctionToolCallAndContentBlocks(t *testing.T) {
	history := strings.Join([]string{
		`{"type":"user","content":[{"type":"text","text":"search"}]}`,
		`{"type":"assistant","content":"","tool_calls":[{"id":"call-grep","function":{"name":"Grep","arguments":"{\"pattern\":\"todo\"}"}}]}`,
		`{"type":"tool_result","tool_call_id":"call-grep","content":[{"type":"text","text":"internal/agent/backend_grok.go"}]}`,
	}, "\n") + "\n"

	tools, err := parseGrokChatHistoryToolUses(strings.NewReader(history))
	if err != nil {
		t.Fatalf("parseGrokChatHistoryToolUses: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("toolUses len = %d, want 1: %+v", len(tools), tools)
	}
	if tools[0].Name != "Grep" {
		t.Errorf("tool name = %q, want Grep", tools[0].Name)
	}
	if tools[0].Input != `{"pattern":"todo"}` {
		t.Errorf("tool input = %q", tools[0].Input)
	}
	if tools[0].Output != "internal/agent/backend_grok.go" {
		t.Errorf("tool output = %q", tools[0].Output)
	}
}

func TestGrokEscapePath(t *testing.T) {
	// Verified empirically against grok 0.1.216: each input is the
	// cwd grok ran in, each output is the directory name it wrote
	// under ~/.grok/sessions/.
	cases := []struct {
		in, want string
	}{
		{
			"/private/tmp/grok-test",
			"%2Fprivate%2Ftmp%2Fgrok-test",
		},
		{
			"/private/tmp/grok-test-special+ε=ω@&dir",
			"%2Fprivate%2Ftmp%2Fgrok-test-special%2B%CE%B5%3D%CF%89%40%26dir",
		},
		{
			"/Users/loppo",
			"%2FUsers%2Floppo",
		},
		{
			"/path-with_unreserved.chars~ok",
			"%2Fpath-with_unreserved.chars~ok",
		},
	}
	for _, tc := range cases {
		got := grokEscapePath(tc.in)
		if got != tc.want {
			t.Errorf("grokEscapePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGrokSessionDir_HonorsGROK_HOME(t *testing.T) {
	t.Setenv("GROK_HOME", "/custom/grok-root")
	got := grokSessionDir("/Users/loppo")
	want := filepath.Join("/custom/grok-root", "sessions", "%2FUsers%2Floppo")
	if got != want {
		t.Errorf("grokSessionDir(GROK_HOME) = %q, want %q", got, want)
	}
}

func TestGrokSessionDir_FallsBackToHome(t *testing.T) {
	t.Setenv("GROK_HOME", "")
	home, _ := os.UserHomeDir()
	if home == "" {
		t.Skip("no HOME")
	}
	got := grokSessionDir(home)
	want := filepath.Join(home, ".grok", "sessions", grokEscapePath(home))
	if got != want {
		t.Errorf("grokSessionDir(home) = %q, want %q", got, want)
	}
}

func TestReadWriteGrokSessionID(t *testing.T) {
	tmp := t.TempDir()
	const validID = "019e5527-7b78-70f3-b488-2f005dbcc2fe"

	if got := readGrokSessionID(tmp); got != "" {
		t.Errorf("readGrokSessionID(empty) = %q, want \"\"", got)
	}
	writeGrokSessionID(tmp, validID, silentLogger())
	if got := readGrokSessionID(tmp); got != validID {
		t.Errorf("readGrokSessionID after write = %q, want %q", got, validID)
	}
	// Removing the file restores "" (clearGrokSession behaviour).
	os.Remove(grokSessionIDFile(tmp))
	if got := readGrokSessionID(tmp); got != "" {
		t.Errorf("readGrokSessionID after remove = %q, want \"\"", got)
	}
}

func TestWriteGrokSessionID_RejectsNonUUID(t *testing.T) {
	tmp := t.TempDir()
	for _, bad := range []string{
		"",
		"abc-123",
		"--cwd=/etc",
		"../../etc/passwd",
		"019e5527-7b78-70f3-b488-2f005dbcc2fe\n--bad", // newline + extra arg
		"019E5527-7B78-70F3-B488-2F005DBCC2FE",        // uppercase
	} {
		writeGrokSessionID(tmp, bad, silentLogger())
		if got := readGrokSessionID(tmp); got != "" {
			t.Errorf("write+read round-trip of %q leaked %q (should reject)", bad, got)
		}
	}
}

func TestReadGrokSessionID_DropsPoisonedFile(t *testing.T) {
	tmp := t.TempDir()
	// Write a poisoned file directly, bypassing the writer guard.
	if err := os.MkdirAll(filepath.Dir(grokSessionIDFile(tmp)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(grokSessionIDFile(tmp), []byte("--cwd=/etc/passwd"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readGrokSessionID(tmp); got != "" {
		t.Errorf("readGrokSessionID(poisoned) = %q, want \"\"", got)
	}
	// And the poisoned file should be removed so it can't keep
	// triggering the rejection on every turn.
	if _, err := os.Stat(grokSessionIDFile(tmp)); !os.IsNotExist(err) {
		t.Errorf("poisoned session_id file not removed (err=%v)", err)
	}
}

func TestBuildGrokArgs_DisablesNativeMemory(t *testing.T) {
	args := buildGrokArgs("/tmp/prompt.txt", "/tmp/agent", "", &Agent{ID: "ag_grok"}, "system")
	if !stringSliceContains(args, "--no-memory") {
		t.Fatalf("grok args missing --no-memory: %#v", args)
	}
	if stringSliceContains(args, "--experimental-memory") {
		t.Fatalf("grok args must not enable native memory: %#v", args)
	}
}

func TestGrokCommandEnv_ForcesNativeMemoryOff(t *testing.T) {
	t.Setenv("GROK_MEMORY", "1")
	env := grokCommandEnv("ag_grok_env", "/tmp/ag_grok_env")

	var values []string
	for _, e := range env {
		if strings.HasPrefix(e, "GROK_MEMORY=") {
			values = append(values, e)
		}
	}
	if len(values) != 1 || values[0] != "GROK_MEMORY=0" {
		t.Fatalf("GROK_MEMORY env = %#v, want exactly GROK_MEMORY=0", values)
	}
}

func TestIsStaleSessionError(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// observed in grok 0.1.x stdout on --resume <missing>
		{"Couldn't create session: Session does not exist", true},
		{"Error: No session found for current directory", true},
		{"NO SESSION FOUND", true}, // case-insensitive
		{"", false},
		{"some other error", false},
	}
	for _, tc := range cases {
		if got := isStaleSessionError(tc.in); got != tc.want {
			t.Errorf("isStaleSessionError(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestClassifyGrokProcessError(t *testing.T) {
	exitErr := errors.New("exit status 1")
	// Real-world sample: grok logs non-fatal tool failures (e.g.
	// read_file on a missing path) to stderr while the process still
	// exits 0. The classifier MUST ignore stderr in that case so the
	// user's reply isn't decorated with a bogus error.
	toolErrLog := "2026-05-23T22:48:54.687587Z ERROR tool_error: tool_output_error session_id=019e5706 tool_name=\"read_file\" error_message=\"Error: /a/b does not exist.\""

	cases := []struct {
		name        string
		waitErr     error
		stderr      string
		streamError string
		want        string
	}{
		{
			name: "clean exit no stderr",
			want: "",
		},
		{
			name:   "clean exit with stderr-only tool log is NOT fatal",
			stderr: toolErrLog,
			want:   "",
		},
		{
			name:        "stream error wins over everything",
			waitErr:     exitErr,
			stderr:      "ignored",
			streamError: "Session does not exist",
			want:        "Session does not exist",
		},
		{
			name:    "non-zero exit augments wait error with stderr",
			waitErr: exitErr,
			stderr:  "  fatal: oom\n",
			want:    "exit status 1: fatal: oom",
		},
		{
			name:    "non-zero exit falls back to wait error when stderr empty",
			waitErr: exitErr,
			want:    "exit status 1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyGrokProcessError(tc.waitErr, tc.stderr, tc.streamError)
			if got != tc.want {
				t.Errorf("classifyGrokProcessError(%v, %q, %q) = %q, want %q",
					tc.waitErr, tc.stderr, tc.streamError, got, tc.want)
			}
		})
	}
}

func TestRemoveGrokSessionDir_RejectsTraversal(t *testing.T) {
	t.Setenv("GROK_HOME", t.TempDir())
	cwd := "/Users/test"
	dir := grokSessionDir(cwd)
	if dir == "" {
		t.Fatal("grokSessionDir empty")
	}
	// Pre-create a sibling that traversal would attack.
	parent := filepath.Dir(dir)
	sibling := filepath.Join(parent, "other-victim")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	// Attempt removal with hostile sessionID; the validator should
	// refuse before any path-join happens.
	removeGrokSessionDir(cwd, "../other-victim")
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("sibling removed despite traversal attempt: %v", err)
	}
	// And a non-UUID is a no-op too.
	removeGrokSessionDir(cwd, "not-a-uuid")
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("sibling removed by non-UUID input: %v", err)
	}
}

func TestHasGrokSession_FalseWhenMissing(t *testing.T) {
	// A path that almost certainly has no grok session.
	if hasGrokSession("/tmp/this-does-not-exist-kojo-test-" + randomSuffix()) {
		t.Error("hasGrokSession returned true for nonexistent cwd")
	}
}

func randomSuffix() string {
	// Cheap unique-enough suffix; collisions would only weaken the
	// test, not break it.
	return filepath.Base(os.TempDir()) + "-x"
}

// TestClearGrokSessionCounted_ReportsCounts plants a primary + OneShot
// session, then asserts clearGrokSessionCounted removes both subtrees
// and the resume pointer file, reporting the correct counts.
func TestClearGrokSessionCounted_ReportsCounts(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_clear_counted"
	const primaryID = "019e588f-419b-7202-9cff-1647e57116d5"
	const oneShotID = "019e58a0-2222-7202-9cff-1647e57116d5"
	plantGrokSession(t, agentID, primaryID)

	// Plant a OneShot directory alongside (no pointer file written
	// for OneShot — clearGrokSessionCounted should still wipe it).
	dir := agentDir(agentID)
	oneShotRoot := filepath.Join(grokSessionDir(dir), oneShotID)
	if err := os.MkdirAll(oneShotRoot, 0o755); err != nil {
		t.Fatalf("mkdir oneshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(oneShotRoot, "events.jsonl"), []byte(`{"type":"end"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write oneshot events: %v", err)
	}

	files, sessions, err := clearGrokSessionCounted(agentID)
	if err != nil {
		t.Fatalf("clearGrokSessionCounted: %v", err)
	}
	// Primary session has 5 files (events / chat_history / summary /
	// system_prompt / terminal/call-abc-1.log); OneShot has 1.
	const wantFiles = 5 + 1
	if files != wantFiles {
		t.Errorf("filesRemoved = %d, want %d", files, wantFiles)
	}
	if sessions != 2 {
		t.Errorf("sessionsRemoved = %d, want 2", sessions)
	}

	// Pointer file gone.
	if _, err := os.Stat(grokSessionIDFile(dir)); !os.IsNotExist(err) {
		t.Errorf("session_id pointer still present: err=%v", err)
	}
	// Session subtree gone.
	if _, err := os.Stat(grokSessionDir(dir)); !os.IsNotExist(err) {
		t.Errorf("session subtree still present: err=%v", err)
	}
}

// TestClearGrokSessionCounted_NoState covers the non-grok agent /
// fresh agent path: no .grok/session_id, no $GROK_HOME/sessions
// entry. Helper must no-op cleanly and report zeroes.
func TestClearGrokSessionCounted_NoState(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_no_state"
	if err := os.MkdirAll(agentDir(agentID), 0o755); err != nil {
		t.Fatalf("mkdir agentDir: %v", err)
	}

	files, sessions, err := clearGrokSessionCounted(agentID)
	if err != nil {
		t.Fatalf("clearGrokSessionCounted on fresh agent: %v", err)
	}
	if files != 0 || sessions != 0 {
		t.Errorf("counts on fresh agent: files=%d sessions=%d, want 0/0", files, sessions)
	}
}

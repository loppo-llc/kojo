package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/loppo-llc/kojo/internal/chathistory"
)

// rpcLine builds a JSON-RPC notification line for testing.
func rpcLine(method string, params any) string {
	raw, _ := json.Marshal(params)
	rawMsg := json.RawMessage(raw)
	msg := rpcMessage{
		Method: method,
		Params: &rawMsg,
	}
	data, _ := json.Marshal(msg)
	return string(data)
}

// rpcResponseLine builds a JSON-RPC response line with an ID.
func rpcResponseLine(id int64, result any, rpcErr *rpcError) string {
	rawID := json.RawMessage(strconv.FormatInt(id, 10))
	msg := rpcMessage{ID: &rawID}
	if result != nil {
		raw, _ := json.Marshal(result)
		rawMsg := json.RawMessage(raw)
		msg.Result = &rawMsg
	}
	if rpcErr != nil {
		msg.Error = rpcErr
	}
	data, _ := json.Marshal(msg)
	return string(data)
}

// rpcServerRequestLine builds a server-initiated JSON-RPC request. Codex may
// use either a numeric or string request ID, so keep id polymorphic here.
func rpcServerRequestLine(id any, method string, params any) string {
	data, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	return string(data)
}

func TestBuildCodexResumeParamsExcludesHistoricalTurns(t *testing.T) {
	base := map[string]any{
		"cwd":     "/workspace",
		"sandbox": "danger-full-access",
	}

	got := buildCodexResumeParams(base, "thread-123")

	if got["threadId"] != "thread-123" {
		t.Fatalf("threadId = %v, want thread-123", got["threadId"])
	}
	if exclude, ok := got["excludeTurns"].(bool); !ok || !exclude {
		t.Fatalf("excludeTurns = %#v, want true", got["excludeTurns"])
	}
	if got["cwd"] != base["cwd"] || got["sandbox"] != base["sandbox"] {
		t.Fatalf("resume params lost base fields: got %#v, base %#v", got, base)
	}
	if _, ok := base["threadId"]; ok {
		t.Fatal("buildCodexResumeParams mutated base thread params with threadId")
	}
	if _, ok := base["excludeTurns"]; ok {
		t.Fatal("buildCodexResumeParams mutated base thread params with excludeTurns")
	}
}

// collectCodexEvents runs parseCodexStream on the given lines and collects all emitted events.
func collectCodexEvents(t *testing.T, turnStartID int64, lines ...string) ([]ChatEvent, *codexStreamResult) {
	t.Helper()
	input := strings.Join(lines, "\n") + "\n"
	scanner := newCodexLineScanner(strings.NewReader(input))

	var events []ChatEvent
	send := func(e ChatEvent) bool {
		events = append(events, e)
		return true
	}

	result := parseCodexStream(scanner, turnStartID, nil, nil, testLogger(), send)
	return events, result
}

func TestCodexLineScanner(t *testing.T) {
	collect := func(input string) ([]string, error) {
		s := newCodexLineScanner(strings.NewReader(input))
		var lines []string
		for s.Scan() {
			lines = append(lines, s.Text())
		}
		return lines, s.Err()
	}

	t.Run("terminated lines", func(t *testing.T) {
		lines, err := collect("a\nbb\nccc\n")
		if err != nil {
			t.Fatalf("Err = %v, want nil", err)
		}
		if want := []string{"a", "bb", "ccc"}; !equalStrings(lines, want) {
			t.Errorf("lines = %v, want %v", lines, want)
		}
	})

	t.Run("final line without trailing newline", func(t *testing.T) {
		lines, err := collect("a\nlast-no-nl")
		if err != nil {
			t.Fatalf("Err = %v, want nil (EOF must not surface as error)", err)
		}
		if want := []string{"a", "last-no-nl"}; !equalStrings(lines, want) {
			t.Errorf("lines = %v, want %v", lines, want)
		}
	})

	t.Run("blank lines preserved as empty", func(t *testing.T) {
		lines, _ := collect("a\n\nb\n")
		if want := []string{"a", "", "b"}; !equalStrings(lines, want) {
			t.Errorf("lines = %v, want %v", lines, want)
		}
	})

	t.Run("CRLF trimmed", func(t *testing.T) {
		lines, _ := collect("a\r\nb\r\n")
		if want := []string{"a", "b"}; !equalStrings(lines, want) {
			t.Errorf("lines = %v, want %v", lines, want)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		lines, err := collect("")
		if err != nil {
			t.Fatalf("Err = %v, want nil", err)
		}
		if len(lines) != 0 {
			t.Errorf("lines = %v, want empty", lines)
		}
	})

	t.Run("large line under cap", func(t *testing.T) {
		big := strings.Repeat("z", 5*1024*1024)
		lines, err := collect(big + "\n")
		if err != nil {
			t.Fatalf("Err = %v, want nil", err)
		}
		if len(lines) != 1 || len(lines[0]) != len(big) {
			t.Fatalf("got %d lines (first len %d), want 1 line of len %d", len(lines), func() int {
				if len(lines) > 0 {
					return len(lines[0])
				}
				return -1
			}(), len(big))
		}
	})

	t.Run("line over cap", func(t *testing.T) {
		tooBig := strings.Repeat("z", chathistory.MaxJSONLLineBytes+1)
		lines, err := collect(tooBig + "\n")
		if !errors.Is(err, chathistory.ErrLineTooLarge) {
			t.Fatalf("Err = %v, want ErrLineTooLarge", err)
		}
		if len(lines) != 0 {
			t.Fatalf("lines = %d, want 0 when line exceeds cap", len(lines))
		}
	})
}

// TestCodexLineScanner_NonEOFErrorYieldsPartialLine verifies that a partial
// line followed by a non-EOF read error is yielded once (matching
// bufio.Scanner) and the error is then reported via Err().
func TestCodexLineScanner_NonEOFErrorYieldsPartialLine(t *testing.T) {
	wantErr := io.ErrUnexpectedEOF
	r := io.MultiReader(strings.NewReader("partial-no-newline"), &errReader{err: wantErr})
	s := newCodexLineScanner(r)

	if !s.Scan() {
		t.Fatal("Scan() = false, want true (partial line must be yielded)")
	}
	if got := s.Text(); got != "partial-no-newline" {
		t.Errorf("Text() = %q, want %q", got, "partial-no-newline")
	}
	if s.Scan() {
		t.Error("second Scan() = true, want false")
	}
	if s.Err() != wantErr {
		t.Errorf("Err() = %v, want %v", s.Err(), wantErr)
	}
}

type errReader struct{ err error }

func (e *errReader) Read([]byte) (int, error) { return 0, e.err }

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestParseCodexStream_OversizedLine is a regression test for the
// "bufio.Scanner: token too long" hang: a single item/completed notification
// carrying a multi-MB aggregatedOutput (e.g. a command that printed a huge
// blob) must be read intact instead of killing the stream. bufio.Scanner with
// a 1MB max token would have failed here; jsonlLineScanner must read it while
// still enforcing its larger MaxJSONLLineBytes safety cap.
func TestParseCodexStream_OversizedLine(t *testing.T) {
	bigOutput := strings.Repeat("x", 3*1024*1024) // 3MB, well over the old 1MB cap
	events, result := collectCodexEvents(t, 1,
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{
				"id":               "i1",
				"type":             "commandExecution",
				"command":          "cat huge.log",
				"aggregatedOutput": bigOutput,
				"exitCode":         0,
			},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if !result.turnCompleted {
		t.Fatal("expected turnCompleted = true (stream must survive the oversized line)")
	}
	var gotOutput string
	for _, e := range events {
		if e.Type == "tool_result" {
			gotOutput = e.ToolOutput
		}
	}
	if len(gotOutput) != len(bigOutput) {
		t.Errorf("tool_result output length = %d, want %d (output truncated/lost)", len(gotOutput), len(bigOutput))
	}
}

func TestParseCodexStream_TextDelta(t *testing.T) {
	events, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "i1", "type": "agentMessage", "phase": "final_answer"},
		}),
		rpcLine("item/agentMessage/delta", map[string]any{
			"itemId": "i1", "delta": "Hello ",
		}),
		rpcLine("item/agentMessage/delta", map[string]any{
			"itemId": "i1", "delta": "world",
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if result.fullText.String() != "Hello world" {
		t.Errorf("fullText = %q, want %q", result.fullText.String(), "Hello world")
	}
	if !result.turnCompleted {
		t.Error("expected turnCompleted = true")
	}

	textEvents := 0
	for _, e := range events {
		if e.Type == "text" {
			textEvents++
		}
	}
	if textEvents != 2 {
		t.Errorf("expected 2 text events, got %d", textEvents)
	}
}

func TestParseCodexStream_AgentMessageCompletedSnapshotFallback(t *testing.T) {
	events, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "i1", "type": "agentMessage", "phase": "final_answer"},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{"id": "i1", "type": "agentMessage", "text": "snapshot answer"},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if got := result.fullText.String(); got != "snapshot answer" {
		t.Fatalf("fullText = %q, want completed-item snapshot", got)
	}
	if len(events) != 1 || events[0].Type != "text" || events[0].Delta != "snapshot answer" {
		t.Fatalf("events = %+v, want one snapshot text event", events)
	}
}

func TestParseCodexStream_ThinkingDelta(t *testing.T) {
	events, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "i1", "type": "agentMessage", "phase": "commentary"},
		}),
		rpcLine("item/agentMessage/delta", map[string]any{
			"itemId": "i1", "delta": "thinking...",
		}),
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "i2", "type": "agentMessage", "phase": "final_answer"},
		}),
		rpcLine("item/agentMessage/delta", map[string]any{
			"itemId": "i2", "delta": "Answer",
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if result.thinking.String() != "thinking..." {
		t.Errorf("thinking = %q, want %q", result.thinking.String(), "thinking...")
	}
	if result.fullText.String() != "Answer" {
		t.Errorf("fullText = %q, want %q", result.fullText.String(), "Answer")
	}

	foundThinking := false
	for _, e := range events {
		if e.Type == "thinking" && e.Delta == "thinking..." {
			foundThinking = true
		}
	}
	if !foundThinking {
		t.Error("expected thinking event")
	}
}

func TestParseCodexStream_CommandExecution(t *testing.T) {
	events, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "cmd1", "type": "commandExecution", "command": "ls -la"},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{"id": "cmd1", "type": "commandExecution", "aggregatedOutput": "file1\nfile2"},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if len(result.toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(result.toolUses))
	}
	tu := result.toolUses[0]
	if tu.Name != "shell" {
		t.Errorf("tool name = %q, want %q", tu.Name, "shell")
	}
	if tu.Input != "ls -la" {
		t.Errorf("tool input = %q, want %q", tu.Input, "ls -la")
	}
	if tu.Output != "file1\nfile2" {
		t.Errorf("tool output = %q, want %q", tu.Output, "file1\nfile2")
	}

	foundToolUse := false
	foundToolResult := false
	for _, e := range events {
		if e.Type == "tool_use" && e.ToolName == "shell" {
			foundToolUse = true
		}
		if e.Type == "tool_result" && e.ToolName == "shell" && e.ToolOutput == "file1\nfile2" {
			foundToolResult = true
		}
	}
	if !foundToolUse {
		t.Error("expected tool_use event")
	}
	if !foundToolResult {
		t.Error("expected tool_result event")
	}
}

func TestParseCodexStream_FileChange(t *testing.T) {
	// Captured from codex app-server 0.153.3: apply_patch surfaces as a
	// fileChange item, not a commandExecution.
	events, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{
				"id": "exec-1", "type": "fileChange", "status": "inProgress",
				"changes": []map[string]any{
					{"path": "/w/a.txt", "kind": map[string]any{"type": "add"}, "diff": "hello\n"},
					{"path": "/w/b.go", "kind": map[string]any{"type": "update", "move_path": "/w/c.go"}, "diff": "@@ -1 +1 @@\n-x\n+y\n"},
					{"path": "/w/d.txt", "kind": map[string]any{"type": "delete"}},
				},
			},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{
				"id": "exec-1", "type": "fileChange", "status": "completed",
				"changes": []map[string]any{
					{"path": "/w/a.txt", "kind": map[string]any{"type": "add"}, "diff": "hello\n"},
				},
			},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if len(result.toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(result.toolUses))
	}
	tu := result.toolUses[0]
	if tu.Name != "apply_patch" {
		t.Errorf("tool name = %q, want %q", tu.Name, "apply_patch")
	}
	wantInput := "add /w/a.txt\nhello\n\nupdate /w/b.go -> /w/c.go\n@@ -1 +1 @@\n-x\n+y\n\ndelete /w/d.txt"
	if tu.Input != wantInput {
		t.Errorf("tool input = %q, want %q", tu.Input, wantInput)
	}
	if tu.Output != "completed" {
		t.Errorf("tool output = %q, want %q", tu.Output, "completed")
	}

	var got []ChatEvent
	for _, e := range events {
		if e.Type == "tool_use" || e.Type == "tool_result" {
			got = append(got, e)
		}
	}
	want := []ChatEvent{
		{Type: "tool_use", ToolUseID: "exec-1", ToolName: "apply_patch", ToolInput: wantInput},
		{Type: "tool_result", ToolUseID: "exec-1", ToolName: "apply_patch", ToolOutput: "completed"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tool events = %+v, want %+v", got, want)
	}
}

func TestCodexFileChangeInput_TruncatesOnRuneBoundary(t *testing.T) {
	// A 3-byte rune straddling the limit must be dropped whole.
	diff := strings.Repeat("x", codexFileChangeDiffLimit-1) + "あ" + "yy"
	got := codexFileChangeInput([]codexFileChange{{Path: "/w/u", Diff: diff}})
	if !utf8.ValidString(got) {
		t.Fatalf("truncated input is not valid UTF-8")
	}
	want := "update /w/u\n" + strings.Repeat("x", codexFileChangeDiffLimit-1) + "\n[...truncated 5 bytes...]"
	if got != want {
		t.Errorf("tail=%q", got[len(got)-40:])
	}
}

func TestCodexFileChangeInput_TruncatesLargeDiff(t *testing.T) {
	big := strings.Repeat("x", codexFileChangeDiffLimit+100)
	got := codexFileChangeInput([]codexFileChange{{Path: "/w/big", Diff: big + "\n"}})
	want := "update /w/big\n" + big[:codexFileChangeDiffLimit] + "\n[...truncated 100 bytes...]"
	if got != want {
		t.Errorf("len(got)=%d want %d; tail=%q", len(got), len(want), got[len(got)-40:])
	}
}

func TestParseCodexStream_FileChangeBackfillsInputFromCompleted(t *testing.T) {
	// With apply_patch streaming events the started item may carry no
	// changes yet; the completed item must fill the stored input.
	_, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "exec-3", "type": "fileChange", "changes": []map[string]any{}},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{"id": "exec-3", "type": "fileChange", "status": "completed",
				"changes": []map[string]any{{"path": "/w/a.txt", "kind": map[string]any{"type": "add"}, "diff": "hi\n"}}},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)
	if len(result.toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(result.toolUses))
	}
	if got := result.toolUses[0].Input; got != "add /w/a.txt\nhi" {
		t.Errorf("input = %q", got)
	}
	if got := result.toolUses[0].Output; got != "completed" {
		t.Errorf("output = %q", got)
	}
}

func TestParseCodexStream_FileChangeFailed(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "exec-2", "type": "fileChange",
				"changes": []map[string]any{{"path": "/w/a.txt", "kind": map[string]any{"type": "add"}, "diff": "x"}}},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{"id": "exec-2", "type": "fileChange", "status": "failed"},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)
	if len(result.toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(result.toolUses))
	}
	if got := result.toolUses[0].Output; got != "failed" {
		t.Errorf("output = %q, want %q", got, "failed")
	}
}

func TestParseCodexStream_CommandExitCode(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "cmd1", "type": "commandExecution", "command": "false"},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{"id": "cmd1", "type": "commandExecution", "exitCode": 1},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if len(result.toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(result.toolUses))
	}
	if result.toolUses[0].Output != "exit code: 1" {
		t.Errorf("output = %q, want %q", result.toolUses[0].Output, "exit code: 1")
	}
}

func TestParseCodexStream_MCPToolCall(t *testing.T) {
	events, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{
				"id": "mcp1", "type": "mcpToolCall",
				"tool": "read_file", "server": "filesystem",
				"arguments": json.RawMessage(`{"path":"foo.txt"}`),
			},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{
				"id": "mcp1", "type": "mcpToolCall",
				"tool": "read_file", "server": "filesystem",
				"result": json.RawMessage(`"file contents here"`),
			},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if len(result.toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(result.toolUses))
	}
	tu := result.toolUses[0]
	if tu.Name != "filesystem/read_file" {
		t.Errorf("tool name = %q, want %q", tu.Name, "filesystem/read_file")
	}

	foundResult := false
	for _, e := range events {
		if e.Type == "tool_result" && e.ToolName == "filesystem/read_file" {
			foundResult = true
		}
	}
	if !foundResult {
		t.Error("expected tool_result event for MCP tool")
	}
}

func TestParseCodexStream_MCPToolError(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "mcp1", "type": "mcpToolCall", "tool": "broken"},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{
				"id": "mcp1", "type": "mcpToolCall", "tool": "broken",
				"error": map[string]string{"message": "tool failed"},
			},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if len(result.toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(result.toolUses))
	}
	if result.toolUses[0].Output != "error: tool failed" {
		t.Errorf("output = %q, want %q", result.toolUses[0].Output, "error: tool failed")
	}
}

func TestParseCodexStream_DynamicToolCall(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "dyn1", "type": "dynamicToolCall", "tool": "search"},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{
				"id": "dyn1", "type": "dynamicToolCall", "tool": "search",
				"contentItems": json.RawMessage(`[{"text":"result"}]`),
			},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if len(result.toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(result.toolUses))
	}
	if result.toolUses[0].Output != `[{"text":"result"}]` {
		t.Errorf("output = %q", result.toolUses[0].Output)
	}
}

func TestParseCodexStream_DynamicToolFailed(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "dyn1", "type": "dynamicToolCall", "tool": "broken"},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{
				"id": "dyn1", "type": "dynamicToolCall", "tool": "broken",
				"success": false,
			},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if len(result.toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(result.toolUses))
	}
	if result.toolUses[0].Output != "failed" {
		t.Errorf("output = %q, want %q", result.toolUses[0].Output, "failed")
	}
}

func TestParseCodexStream_TokenUsage(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("thread/tokenUsage/updated", map[string]any{
			"tokenUsage": map[string]any{
				"last": map[string]int{"inputTokens": 100, "outputTokens": 50},
			},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if result.usage == nil {
		t.Fatal("expected usage")
	}
	if result.usage.InputTokens != 100 || result.usage.OutputTokens != 50 {
		t.Errorf("usage = %+v, want {100, 50}", result.usage)
	}
}

func TestParseCodexStream_TurnFailed(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{
				"status": "failed",
				"error":  map[string]any{"code": -1, "message": "something broke"},
			},
		}),
	)

	if !result.turnCompleted {
		t.Error("expected turnCompleted = true")
	}
	if result.processError != "something broke" {
		t.Errorf("processError = %q, want %q", result.processError, "something broke")
	}
}

func TestParseCodexStream_TurnInterrupted(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "interrupted"},
		}),
	)

	if result.processError != "codex turn interrupted" {
		t.Errorf("processError = %q, want %q", result.processError, "codex turn interrupted")
	}
}

func TestRunCodexTurns_RetriesEmptyCompletionAndPreservesWork(t *testing.T) {
	input := strings.Join([]string{
		rpcResponseLine(1, map[string]any{"turn": map[string]any{"id": "turn-1"}}, nil),
		rpcLine("item/reasoning/textDelta", map[string]any{"delta": "first thought;"}),
		rpcLine("thread/tokenUsage/updated", map[string]any{
			"tokenUsage": map[string]any{"last": map[string]int{"inputTokens": 10, "outputTokens": 2}},
		}),
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "cmd-1", "type": "commandExecution", "command": "touch result.txt"},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{"id": "cmd-1", "type": "commandExecution", "aggregatedOutput": "done", "exitCode": 0},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"id": "turn-1", "status": "completed"},
		}),
		rpcResponseLine(2, map[string]any{"turn": map[string]any{"id": "turn-2"}}, nil),
		rpcLine("item/reasoning/textDelta", map[string]any{"delta": "second thought"}),
		rpcLine("thread/tokenUsage/updated", map[string]any{
			"tokenUsage": map[string]any{"last": map[string]int{"inputTokens": 20, "outputTokens": 3}},
		}),
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "msg-2", "type": "agentMessage", "phase": "final_answer"},
		}),
		rpcLine("item/agentMessage/delta", map[string]any{
			"itemId": "msg-2", "delta": "Recovered final answer",
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"id": "turn-2", "status": "completed"},
		}),
	}, "\n") + "\n"

	var starts []string
	startTurn := func(turnInput string) (int64, error) {
		starts = append(starts, turnInput)
		return int64(len(starts)), nil
	}
	var events []ChatEvent
	result := runCodexTurns(
		context.Background(),
		newCodexLineScanner(strings.NewReader(input)),
		"original request",
		codexTurnRetryPolicy{maxEmptyRetries: codexEmptyCompletionMaxRetries},
		startTurn,
		nil,
		nil,
		testLogger(),
		func(event ChatEvent) bool {
			events = append(events, event)
			return true
		},
	)

	if len(starts) != 2 {
		t.Fatalf("turn starts = %d, want 2", len(starts))
	}
	if starts[0] != "original request" || starts[1] != codexEmptyCompletionRetryPrompt {
		t.Fatalf("turn inputs = %#v, want original then recovery prompt", starts)
	}
	if !result.turnCompleted || result.processError != "" || result.cancelled {
		t.Fatalf("result terminal state = %+v, want clean completion", result)
	}
	if got := result.fullText.String(); got != "Recovered final answer" {
		t.Errorf("fullText = %q, want recovered answer", got)
	}
	if len(result.toolUses) != 1 || result.toolUses[0].Input != "touch result.txt" || result.toolUses[0].Output != "done" {
		t.Errorf("preserved tool uses = %+v, want first-turn command", result.toolUses)
	}
	if got := result.thinking.String(); got != "first thought;second thought" {
		t.Errorf("thinking = %q, want both attempts", got)
	}
	if result.usage == nil || result.usage.InputTokens != 30 || result.usage.OutputTokens != 5 {
		t.Errorf("usage = %+v, want sum across attempts", result.usage)
	}
	var textEvents int
	for _, event := range events {
		if event.Type == "text" {
			textEvents++
		}
	}
	if textEvents != 1 {
		t.Errorf("text events = %d, want only the recovered final answer", textEvents)
	}
}

func TestRunCodexTurns_StopsAfterBoundedEmptyCompletionRetries(t *testing.T) {
	var lines []string
	for i := int64(1); i <= codexEmptyCompletionMaxRetries+1; i++ {
		lines = append(lines,
			rpcResponseLine(i, map[string]any{"turn": map[string]any{"id": "empty"}}, nil),
			rpcLine("turn/completed", map[string]any{
				"turn": map[string]any{"id": "empty", "status": "completed"},
			}),
		)
	}

	var starts int
	result := runCodexTurns(
		context.Background(),
		newCodexLineScanner(strings.NewReader(strings.Join(lines, "\n")+"\n")),
		"original request",
		codexTurnRetryPolicy{maxEmptyRetries: codexEmptyCompletionMaxRetries},
		func(string) (int64, error) {
			starts++
			return int64(starts), nil
		},
		nil,
		nil,
		testLogger(),
		func(ChatEvent) bool { return true },
	)

	wantStarts := codexEmptyCompletionMaxRetries + 1
	if starts != wantStarts {
		t.Fatalf("turn starts = %d, want %d", starts, wantStarts)
	}
	if !result.turnCompleted || result.processError != codexEmptyCompletionError {
		t.Fatalf("result = %+v, want completed with bounded-retry error", result)
	}
}

func TestRunCodexTurns_DoesNotRetryFailedOrAnsweredTurn(t *testing.T) {
	tests := []struct {
		name     string
		lines    []string
		wantText string
		wantErr  string
	}{
		{
			name: "failed",
			lines: []string{
				rpcResponseLine(1, map[string]any{"turn": map[string]any{"id": "failed"}}, nil),
				rpcLine("turn/completed", map[string]any{
					"turn": map[string]any{"id": "failed", "status": "failed", "error": map[string]any{"message": "service failed"}},
				}),
			},
			wantErr: "service failed",
		},
		{
			name: "answered",
			lines: []string{
				rpcResponseLine(1, map[string]any{"turn": map[string]any{"id": "answered"}}, nil),
				rpcLine("item/started", map[string]any{
					"item": map[string]any{"id": "msg", "type": "agentMessage", "phase": "final_answer"},
				}),
				rpcLine("item/agentMessage/delta", map[string]any{"itemId": "msg", "delta": "done"}),
				rpcLine("turn/completed", map[string]any{
					"turn": map[string]any{"id": "answered", "status": "completed"},
				}),
			},
			wantText: "done",
		},
		{
			name: "missing status is not retryable",
			lines: []string{
				rpcResponseLine(1, map[string]any{"turn": map[string]any{"id": "unknown"}}, nil),
				rpcLine("turn/completed", map[string]any{"turn": map[string]any{"id": "unknown"}}),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var starts int
			result := runCodexTurns(
				context.Background(),
				newCodexLineScanner(strings.NewReader(strings.Join(tt.lines, "\n")+"\n")),
				"original",
				codexTurnRetryPolicy{maxEmptyRetries: codexEmptyCompletionMaxRetries},
				func(string) (int64, error) { starts++; return 1, nil },
				nil,
				nil,
				testLogger(),
				func(ChatEvent) bool { return true },
			)
			if starts != 1 {
				t.Fatalf("turn starts = %d, want no retry", starts)
			}
			if got := result.fullText.String(); got != tt.wantText {
				t.Errorf("fullText = %q, want %q", got, tt.wantText)
			}
			if result.processError != tt.wantErr {
				t.Errorf("processError = %q, want %q", result.processError, tt.wantErr)
			}
		})
	}
}

func TestRunCodexTurns_ContextCancelledBeforeRetryStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var starts int
	result := runCodexTurns(
		ctx,
		newCodexLineScanner(strings.NewReader("")),
		"original",
		codexTurnRetryPolicy{maxEmptyRetries: codexEmptyCompletionMaxRetries},
		func(string) (int64, error) { starts++; return 1, nil },
		nil,
		nil,
		testLogger(),
		func(ChatEvent) bool { return true },
	)
	if starts != 0 || !result.cancelled || result.turnCompleted {
		t.Fatalf("starts=%d result=%+v, want cancellation before turn/start", starts, result)
	}
}

func TestParseCodexStream_ReasoningSendCancellationStops(t *testing.T) {
	input := strings.Join([]string{
		rpcLine("item/reasoning/textDelta", map[string]any{"delta": "reasoning"}),
		rpcLine("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}}),
	}, "\n") + "\n"
	result := parseCodexStream(
		newCodexLineScanner(strings.NewReader(input)),
		1,
		nil,
		nil,
		testLogger(),
		func(ChatEvent) bool { return false },
	)
	if !result.cancelled || result.turnCompleted {
		t.Fatalf("result = %+v, want immediate cancellation on rejected reasoning event", result)
	}
}

func TestParseCodexStream_TurnStartError(t *testing.T) {
	var turnID int64 = 5
	events, result := collectCodexEvents(t, turnID,
		rpcResponseLine(turnID, nil, &rpcError{Code: -32600, Message: "invalid request"}),
	)

	if !result.cancelled {
		t.Error("expected cancelled = true on turn/start error")
	}

	foundError := false
	for _, e := range events {
		if e.Type == "error" && strings.Contains(e.ErrorMessage, "invalid request") {
			foundError = true
		}
	}
	if !foundError {
		t.Error("expected error event for turn/start failure")
	}
}

func TestParseCodexStream_ReasoningDelta(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("item/reasoning/summaryTextDelta", map[string]any{
			"delta": "reasoning step",
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if result.thinking.String() != "reasoning step" {
		t.Errorf("thinking = %q, want %q", result.thinking.String(), "reasoning step")
	}
}

// A custom model provider (custom-codex) publishes no reasoning deltas at
// all: app-server only emits the finished reasoning item, with the thinking
// text in plain-string `content` entries.
func TestParseCodexStream_ReasoningCompletedItemWithoutDeltas(t *testing.T) {
	events, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "rs_1", "type": "reasoning", "summary": []any{}, "content": []any{}},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{
				"id": "rs_1", "type": "reasoning",
				"summary": []any{},
				"content": []any{"step one", "step two"},
			},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if want := "step one\nstep two"; result.thinking.String() != want {
		t.Errorf("thinking = %q, want %q", result.thinking.String(), want)
	}
	found := false
	for _, e := range events {
		if e.Type == "thinking" && e.Delta == "step one\nstep two" {
			found = true
		}
	}
	if !found {
		t.Error("expected a thinking event carrying the completed reasoning item")
	}
}

// The OpenAI provider streams reasoning deltas and then repeats the same text
// in the completed item's `summary`; the completed item must not double it.
func TestParseCodexStream_ReasoningCompletedItemDoesNotDuplicateDeltas(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("item/reasoning/summaryTextDelta", map[string]any{
			"itemId": "rs_1", "delta": "thought",
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{
				"id": "rs_1", "type": "reasoning",
				"summary": []any{map[string]any{"type": "summary_text", "text": "thought"}},
			},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if result.thinking.String() != "thought" {
		t.Errorf("thinking = %q, want %q", result.thinking.String(), "thought")
	}
}

// A provider that streams deltas but leaves itemId empty cannot be matched per
// id, so the completed item is suppressed wholesale rather than duplicated.
func TestParseCodexStream_ReasoningCompletedItemWithoutIDsAfterDeltas(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("item/reasoning/summaryTextDelta", map[string]any{
			"delta": "thought",
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{
				"type":    "reasoning",
				"summary": []any{"thought"},
			},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if result.thinking.String() != "thought" {
		t.Errorf("thinking = %q, want %q", result.thinking.String(), "thought")
	}
}

// An object-shaped summary from a provider that sends no deltas still renders
// as text rather than raw JSON.
func TestParseCodexStream_ReasoningCompletedItemObjectSummary(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{
				"id": "rs_1", "type": "reasoning",
				"summary": []any{map[string]any{"type": "summary_text", "text": "planned it"}},
				"content": []any{"raw trace"},
			},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if result.thinking.String() != "planned it" {
		t.Errorf("thinking = %q, want %q", result.thinking.String(), "planned it")
	}
}

func TestParseCodexStream_EmptyLines(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		"",
		rpcLine("item/agentMessage/delta", map[string]any{
			"itemId": "i1", "delta": "ok",
		}),
		"",
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if result.fullText.String() != "ok" {
		t.Errorf("fullText = %q, want %q", result.fullText.String(), "ok")
	}
}

func TestParseCodexStream_InvalidJSON(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		"not json",
		rpcLine("item/agentMessage/delta", map[string]any{
			"itemId": "i1", "delta": "valid",
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if result.fullText.String() != "valid" {
		t.Errorf("fullText = %q, want %q", result.fullText.String(), "valid")
	}
}

func TestParseCodexStream_FailsPluginInstallAndContinues(t *testing.T) {
	input := strings.Join([]string{
		rpcLine("item/started", map[string]any{
			"item": map[string]any{
				"id": "plugin-tool-1", "type": "dynamicToolCall",
				"tool": "request_plugin_install",
			},
		}),
		rpcServerRequestLine("plugin-install-1", "item/tool/call", map[string]any{
			"tool":      "request_plugin_install",
			"arguments": map[string]string{"plugin_id": "google-drive@openai-curated-remote"},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{
				"id": "plugin-tool-1", "type": "dynamicToolCall",
				"tool": "request_plugin_install", "success": false,
			},
		}),
		rpcLine("item/agentMessage/delta", map[string]any{
			"itemId": "i1", "delta": "fallback answer",
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	}, "\n") + "\n"

	var wire strings.Builder
	respond := newCodexServerRequestResponder(func(v any) error {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		wire.Write(data)
		wire.WriteByte('\n')
		return nil
	})
	result := parseCodexStream(
		newCodexLineScanner(strings.NewReader(input)),
		1,
		nil,
		respond,
		testLogger(),
		func(ChatEvent) bool { return true },
	)

	var response struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  struct {
			Success      bool `json:"success"`
			ContentItems []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"contentItems"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(wire.String())), &response); err != nil {
		t.Fatalf("decode tool failure response: %v; wire=%q", err, wire.String())
	}
	if got := string(response.ID); got != `"plugin-install-1"` {
		t.Errorf("tool failure response ID = %s, want quoted string ID", got)
	}
	if response.JSONRPC != "2.0" || response.Result.Success {
		t.Errorf("tool failure response = %+v, want unsuccessful JSON-RPC result", response)
	}
	if len(response.Result.ContentItems) != 1 || !strings.Contains(response.Result.ContentItems[0].Text, "normal chat") {
		t.Errorf("tool failure contentItems = %+v, want fallback guidance", response.Result.ContentItems)
	}
	if !result.turnCompleted {
		t.Fatal("turnCompleted = false; server request must not stop the stream")
	}
	if got := result.fullText.String(); got != "fallback answer" {
		t.Errorf("fullText = %q, want fallback answer", got)
	}
	if len(result.toolUses) != 1 || result.toolUses[0].Output != "failed" {
		t.Errorf("tool uses = %+v, want completed failed request_plugin_install call", result.toolUses)
	}
}

func TestWaitCodexRPCResponse_RejectsServerRequestAndContinues(t *testing.T) {
	input := strings.Join([]string{
		rpcServerRequestLine(91, "mcpServer/elicitation/request", map[string]any{}),
		rpcResponseLine(7, map[string]any{"ok": true}, nil),
	}, "\n") + "\n"

	var wire []byte
	respond := newCodexServerRequestResponder(func(v any) error {
		var err error
		wire, err = json.Marshal(v)
		return err
	})
	msg, ok, err := waitCodexRPCResponse(
		newCodexLineScanner(strings.NewReader(input)),
		7,
		respond,
		testLogger(),
	)
	if err != nil {
		t.Fatalf("waitCodexRPCResponse error: %v", err)
	}
	if !ok || msg == nil {
		t.Fatal("waitCodexRPCResponse did not return the target response")
	}
	var response struct {
		ID     json.RawMessage   `json:"id"`
		Result map[string]string `json:"result"`
	}
	if err := json.Unmarshal(wire, &response); err != nil {
		t.Fatalf("decode elicitation response: %v", err)
	}
	if got := string(response.ID); got != "91" || response.Result["action"] != "decline" {
		t.Errorf("elicitation response = %+v, want numeric ID 91 and decline", response)
	}
	if id, ok := msg.numericID(); !ok || id != 7 {
		t.Errorf("response ID = (%d, %v), want (7, true)", id, ok)
	}
}

func TestParseCodexStream_ServerRequestRejectionWriteFailureStops(t *testing.T) {
	input := strings.Join([]string{
		rpcServerRequestLine(91, "item/tool/call", map[string]any{"tool": "request_plugin_install"}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	}, "\n") + "\n"

	var events []ChatEvent
	result := parseCodexStream(
		newCodexLineScanner(strings.NewReader(input)),
		1,
		nil,
		newCodexServerRequestResponder(func(any) error { return errors.New("broken stdin") }),
		testLogger(),
		func(event ChatEvent) bool {
			events = append(events, event)
			return true
		},
	)

	if !result.cancelled || result.turnCompleted {
		t.Fatalf("result = %+v, want cancelled before turn completion", result)
	}
	if len(events) != 1 || events[0].Type != "error" || !strings.Contains(events[0].ErrorMessage, "broken stdin") {
		t.Fatalf("events = %+v, want rejection write error", events)
	}
}

func TestCodexServerRequestResponder_HandlesKnownInteractiveMethods(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		wantResult map[string]any
	}{
		{name: "command approval", method: "item/commandExecution/requestApproval", wantResult: map[string]any{"decision": "decline"}},
		{name: "file approval", method: "item/fileChange/requestApproval", wantResult: map[string]any{"decision": "decline"}},
		{name: "legacy command approval", method: "execCommandApproval", wantResult: map[string]any{"decision": "denied"}},
		{name: "legacy patch approval", method: "applyPatchApproval", wantResult: map[string]any{"decision": "denied"}},
		{name: "MCP elicitation", method: "mcpServer/elicitation/request", wantResult: map[string]any{"action": "decline"}},
		{name: "user input", method: "item/tool/requestUserInput", wantResult: map[string]any{"answers": map[string]any{}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var wire []byte
			respond := newCodexServerRequestResponder(func(v any) error {
				var err error
				wire, err = json.Marshal(v)
				return err
			})
			id := json.RawMessage(`42`)
			params := json.RawMessage(`{}`)
			if _, err := respond(&rpcMessage{ID: &id, Method: tt.method, Params: &params}); err != nil {
				t.Fatalf("respond: %v", err)
			}

			var response struct {
				Result map[string]any `json:"result"`
			}
			if err := json.Unmarshal(wire, &response); err != nil {
				t.Fatalf("decode result response: %v", err)
			}
			for key, want := range tt.wantResult {
				if got := response.Result[key]; !reflect.DeepEqual(got, want) {
					t.Errorf("result[%q] = %#v, want %#v", key, got, want)
				}
			}
		})
	}
}

func TestCodexServerRequestResponder_DeclinesAdditionalPermissions(t *testing.T) {
	var wire []byte
	respond := newCodexServerRequestResponder(func(v any) error {
		var err error
		wire, err = json.Marshal(v)
		return err
	})
	id := json.RawMessage(`43`)
	params := json.RawMessage(`{}`)
	if _, err := respond(&rpcMessage{ID: &id, Method: "item/permissions/requestApproval", Params: &params}); err != nil {
		t.Fatalf("respond: %v", err)
	}
	var response struct {
		Result struct {
			Permissions struct {
				FileSystem any `json:"fileSystem"`
				Network    any `json:"network"`
			} `json:"permissions"`
			Scope string `json:"scope"`
		} `json:"result"`
	}
	if err := json.Unmarshal(wire, &response); err != nil {
		t.Fatalf("decode permissions response: %v", err)
	}
	if response.Result.Scope != "turn" || response.Result.Permissions.FileSystem != nil || response.Result.Permissions.Network != nil {
		t.Errorf("permissions response = %+v, want no additional permissions for turn", response.Result)
	}
}

func TestCodexServerRequestResponder_ReturnsCurrentTime(t *testing.T) {
	var wire []byte
	respond := newCodexServerRequestResponder(func(v any) error {
		var err error
		wire, err = json.Marshal(v)
		return err
	})
	id := json.RawMessage(`44`)
	params := json.RawMessage(`{}`)
	before := time.Now().Unix()
	if _, err := respond(&rpcMessage{ID: &id, Method: "currentTime/read", Params: &params}); err != nil {
		t.Fatalf("respond: %v", err)
	}
	after := time.Now().Unix()
	var response struct {
		Result struct {
			CurrentTimeAt int64 `json:"currentTimeAt"`
		} `json:"result"`
	}
	if err := json.Unmarshal(wire, &response); err != nil {
		t.Fatalf("decode current time response: %v", err)
	}
	if got := response.Result.CurrentTimeAt; got < before || got > after {
		t.Errorf("currentTimeAt = %d, want between %d and %d", got, before, after)
	}
}

func TestParseCodexStream_CriticalOrUnknownServerRequestStops(t *testing.T) {
	tests := []struct {
		name   string
		method string
		params any
		want   string
	}{
		{name: "auth refresh", method: "account/chatgptAuthTokens/refresh", params: map[string]any{}, want: "infrastructure request"},
		{name: "attestation", method: "attestation/generate", params: map[string]any{}, want: "infrastructure request"},
		{name: "unknown method", method: "future/newRequest", params: map[string]any{}, want: "unknown Codex server request"},
		{name: "unknown dynamic tool", method: "item/tool/call", params: map[string]any{"tool": "future_tool"}, want: `dynamic client tool "future_tool"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := strings.Join([]string{
				rpcServerRequestLine("server-1", tt.method, tt.params),
				rpcLine("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}}),
			}, "\n") + "\n"
			var wireCalls int
			var events []ChatEvent
			result := parseCodexStream(
				newCodexLineScanner(strings.NewReader(input)),
				1,
				nil,
				newCodexServerRequestResponder(func(any) error { wireCalls++; return nil }),
				testLogger(),
				func(event ChatEvent) bool { events = append(events, event); return true },
			)
			if !result.cancelled || result.turnCompleted {
				t.Fatalf("result = %+v, want immediate stop", result)
			}
			if wireCalls != 0 {
				t.Fatalf("wire calls = %d, want no generic response for critical request", wireCalls)
			}
			if len(events) != 1 || events[0].Type != "error" || !strings.Contains(events[0].ErrorMessage, tt.want) {
				t.Fatalf("events = %+v, want explicit compatibility error containing %q", events, tt.want)
			}
		})
	}
}

func TestCodexServerRequestResponder_PreservesRequestID(t *testing.T) {
	for _, rawID := range []string{`91`, `"plugin-install-1"`} {
		t.Run(rawID, func(t *testing.T) {
			var wire []byte
			respond := newCodexServerRequestResponder(func(v any) error {
				var err error
				wire, err = json.Marshal(v)
				return err
			})

			id := json.RawMessage(rawID)
			params := json.RawMessage(`{"tool":"request_plugin_install","arguments":{}}`)
			if _, err := respond(&rpcMessage{ID: &id, Method: "item/tool/call", Params: &params}); err != nil {
				t.Fatalf("respond: %v", err)
			}
			var response rpcResultResponse
			if err := json.Unmarshal(wire, &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if got := string(response.ID); got != rawID {
				t.Errorf("response ID = %s, want %s", got, rawID)
			}
		})
	}
}

func TestParseCodexStream_Cancelled(t *testing.T) {
	input := rpcLine("item/agentMessage/delta", map[string]any{
		"itemId": "i1", "delta": "a",
	}) + "\n" + rpcLine("item/agentMessage/delta", map[string]any{
		"itemId": "i1", "delta": "b",
	}) + "\n"

	scanner := newCodexLineScanner(strings.NewReader(input))

	callCount := 0
	send := func(e ChatEvent) bool {
		callCount++
		return callCount < 2
	}

	result := parseCodexStream(scanner, 1, nil, nil, testLogger(), send)
	if !result.cancelled {
		t.Error("expected cancelled = true")
	}
}

func TestParseCodexStream_NoTurnCompleted(t *testing.T) {
	// Scanner ends without turn/completed
	_, result := collectCodexEvents(t, 1,
		rpcLine("item/agentMessage/delta", map[string]any{
			"itemId": "i1", "delta": "partial",
		}),
	)

	if result.turnCompleted {
		t.Error("expected turnCompleted = false")
	}
	if !result.hasOutput() {
		t.Error("expected hasOutput = true")
	}
	if result.fullText.String() != "partial" {
		t.Errorf("fullText = %q, want %q", result.fullText.String(), "partial")
	}
}

func TestCodexStreamResult_BuildMessage(t *testing.T) {
	res := &codexStreamResult{
		usage: &Usage{InputTokens: 10, OutputTokens: 5},
	}
	res.fullText.WriteString("hello")
	res.thinking.WriteString("thought")
	res.toolUses = []ToolUse{{Name: "shell"}}

	msg := res.buildMessage()
	if msg.Content != "hello" {
		t.Errorf("Content = %q, want %q", msg.Content, "hello")
	}
	if msg.Thinking != "thought" {
		t.Errorf("Thinking = %q, want %q", msg.Thinking, "thought")
	}
	if len(msg.ToolUses) != 1 {
		t.Errorf("ToolUses len = %d, want 1", len(msg.ToolUses))
	}
	if msg.Usage != res.usage {
		t.Error("Usage should match")
	}
}

func TestCodexStreamResult_HasOutput(t *testing.T) {
	res := &codexStreamResult{}
	if res.hasOutput() {
		t.Error("empty result should not have output")
	}

	res.fullText.WriteString("text")
	if !res.hasOutput() {
		t.Error("result with text should have output")
	}

	res2 := &codexStreamResult{}
	res2.toolUses = []ToolUse{{Name: "shell"}}
	if !res2.hasOutput() {
		t.Error("result with tool uses should have output")
	}
}

func TestCodexEffortForProtocol_MaxGating(t *testing.T) {
	// The gpt-6.1 family (sol, terra), the gpt-6 family (sol, astra, luna) and the
	// gpt-5.6 family: max passes through (codex CLI 0.164.0).
	for _, m := range []string{"gpt-6.1-sol", "gpt-6.1-terra", "gpt-6-sol", "gpt-6-astra", "gpt-6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		if got := codexEffortForProtocol(m, "max"); got != "max" {
			t.Errorf("codexEffortForProtocol(%q, max) = %q, want max", m, got)
		}
	}
	// Older codex models and the empty (CLI-default) model drop max so
	// the CLI falls back to its default effort instead of erroring.
	for _, m := range []string{"gpt-5.5", ""} {
		if got := codexEffortForProtocol(m, "max"); got != "" {
			t.Errorf("codexEffortForProtocol(%q, max) = %q, want empty", m, got)
		}
	}
	// Non-max levels are model-independent.
	if got := codexEffortForProtocol("", "xhigh"); got != "xhigh" {
		t.Errorf("codexEffortForProtocol(\"\", xhigh) = %q, want xhigh", got)
	}
	if got := codexEffortForProtocol("gpt-5.6-sol", "bogus"); got != "" {
		t.Errorf("expected bogus effort to map to empty, got %q", got)
	}
}

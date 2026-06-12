package agent

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

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
	msg := rpcMessage{ID: &id}
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

	result := parseCodexStream(scanner, turnStartID, testLogger(), send)
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
// a 1MB max token would have failed here; codexLineScanner must read it while
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

	result := parseCodexStream(scanner, 1, testLogger(), send)
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

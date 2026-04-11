package agent

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"
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
	scanner := bufio.NewScanner(strings.NewReader(input))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var events []ChatEvent
	send := func(e ChatEvent) bool {
		events = append(events, e)
		return true
	}

	result := parseCodexStream(scanner, turnStartID, testLogger(), send)
	return events, result
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

	scanner := bufio.NewScanner(strings.NewReader(input))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

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

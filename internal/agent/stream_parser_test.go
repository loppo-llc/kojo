package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// --- parseClaudeStream characterization tests ---

// collectEvents runs parseClaudeStream on the given JSONL lines and collects all emitted events.
func collectEvents(t *testing.T, lines ...string) ([]ChatEvent, *streamParseResult) {
	t.Helper()
	input := strings.Join(lines, "\n") + "\n"
	r := strings.NewReader(input)
	logger := testLogger()

	var events []ChatEvent
	send := func(e ChatEvent) bool {
		events = append(events, e)
		return true
	}

	result := parseClaudeStream(r, logger, send)
	return events, result
}

func TestParseClaudeStream_TextDelta(t *testing.T) {
	events, result := collectEvents(t,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello "}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"world"}}`,
	)

	if result.fullText != "Hello world" {
		t.Errorf("fullText = %q, want %q", result.fullText, "Hello world")
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

func TestParseClaudeStream_ThinkingDelta(t *testing.T) {
	events, result := collectEvents(t,
		`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"Let me think..."}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"Answer"}}`,
	)

	if result.thinking != "Let me think..." {
		t.Errorf("thinking = %q, want %q", result.thinking, "Let me think...")
	}
	if result.fullText != "Answer" {
		t.Errorf("fullText = %q, want %q", result.fullText, "Answer")
	}

	foundThinking := false
	for _, e := range events {
		if e.Type == "thinking" && e.Delta == "Let me think..." {
			foundThinking = true
		}
	}
	if !foundThinking {
		t.Error("expected thinking event")
	}
}

func TestParseClaudeStream_StreamEventWrapper(t *testing.T) {
	// stream_event wraps the actual event in an "event" field
	inner := `{"type":"content_block_delta","delta":{"type":"text_delta","text":"wrapped"}}`
	wrapper := `{"type":"stream_event","event":` + inner + `}`

	_, result := collectEvents(t, wrapper)
	if result.fullText != "wrapped" {
		t.Errorf("fullText = %q, want %q", result.fullText, "wrapped")
	}
}

func TestParseClaudeStream_ToolUseFlow(t *testing.T) {
	events, result := collectEvents(t,
		`{"type":"content_block_start","content_block":{"type":"tool_use","id":"t1","name":"Read"}}`,
		`{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		`{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"\"foo\"}"}}`,
		`{"type":"content_block_stop"}`,
	)

	if len(result.toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(result.toolUses))
	}
	tu := result.toolUses[0]
	if tu.ID != "t1" {
		t.Errorf("tool ID = %q, want %q", tu.ID, "t1")
	}
	if tu.Name != "Read" {
		t.Errorf("tool name = %q, want %q", tu.Name, "Read")
	}
	if tu.Input != `{"path":"foo"}` {
		t.Errorf("tool input = %q, want %q", tu.Input, `{"path":"foo"}`)
	}

	// Check tool_use event was emitted
	foundToolUse := false
	for _, e := range events {
		if e.Type == "tool_use" && e.ToolUseID == "t1" && e.ToolName == "Read" {
			foundToolUse = true
		}
	}
	if !foundToolUse {
		t.Error("expected tool_use event")
	}
}

func TestParseClaudeStream_ToolResult(t *testing.T) {
	events, result := collectEvents(t,
		`{"type":"content_block_start","content_block":{"type":"tool_use","id":"t1","name":"Read"}}`,
		`{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`{"type":"content_block_stop"}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"file contents"}]}}`,
	)

	if len(result.toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(result.toolUses))
	}
	if result.toolUses[0].Output != "file contents" {
		t.Errorf("tool output = %q, want %q", result.toolUses[0].Output, "file contents")
	}

	foundResult := false
	for _, e := range events {
		if e.Type == "tool_result" && e.ToolUseID == "t1" && e.ToolOutput == "file contents" {
			foundResult = true
		}
	}
	if !foundResult {
		t.Error("expected tool_result event")
	}
}

func TestParseClaudeStream_AssistantFallback(t *testing.T) {
	// When no content_block_delta events are received, assistant event text is used as fallback
	_, result := collectEvents(t,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"fallback text"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}}`,
	)

	if result.lastAssistantText != "fallback text" {
		t.Errorf("lastAssistantText = %q, want %q", result.lastAssistantText, "fallback text")
	}
	if result.fullText != "" {
		t.Errorf("fullText should be empty (no deltas), got %q", result.fullText)
	}
	if result.usage == nil {
		t.Fatal("expected usage")
	}
	if result.usage.InputTokens != 10 || result.usage.OutputTokens != 5 {
		t.Errorf("usage = %+v, want {10, 5}", result.usage)
	}
}

// TestParseClaudeStream_FableUsage reproduces the fable-5 shape: the
// top-level "assistant" event carries usage with stop_reason=null (so the
// old StopReason guard dropped it), and the finalized output_tokens arrives
// separately on a message_delta event wrapped in stream_event. The parser
// must end up with the complete usage — input/cache from the assistant
// snapshot, output_tokens corrected by message_delta.
func TestParseClaudeStream_FableUsage(t *testing.T) {
	assistant := `{"type":"assistant","message":{"stop_reason":null,"content":[{"type":"text","text":"Hi"}],"usage":{"input_tokens":3050,"cache_creation_input_tokens":21439,"cache_read_input_tokens":0,"output_tokens":1}}}`
	delta := `{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":3050,"cache_creation_input_tokens":21439,"cache_read_input_tokens":0,"output_tokens":13}}}`

	_, result := collectEvents(t, assistant, delta)

	if result.usage == nil {
		t.Fatal("expected usage to be captured despite null stop_reason")
	}
	if result.usage.InputTokens != 3050 {
		t.Errorf("InputTokens = %d, want 3050", result.usage.InputTokens)
	}
	if result.usage.OutputTokens != 13 {
		t.Errorf("OutputTokens = %d, want 13 (message_delta should correct the placeholder)", result.usage.OutputTokens)
	}
	if result.usage.CacheCreationInputTokens != 21439 {
		t.Errorf("CacheCreationInputTokens = %d, want 21439", result.usage.CacheCreationInputTokens)
	}
}

func TestParseClaudeStream_ResultEvent(t *testing.T) {
	_, result := collectEvents(t,
		`{"type":"result","result":"final text","session_id":"sess-123"}`,
	)

	if result.streamSessionID != "sess-123" {
		t.Errorf("sessionID = %q, want %q", result.streamSessionID, "sess-123")
	}
	// result text is used as fallback when fullText is empty
	if result.fullText != "final text" {
		t.Errorf("fullText = %q, want %q", result.fullText, "final text")
	}
}

func TestParseClaudeStream_ResultIgnoredWhenDeltasPresent(t *testing.T) {
	_, result := collectEvents(t,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"streamed"}}`,
		`{"type":"result","result":"should be ignored"}`,
	)

	if result.fullText != "streamed" {
		t.Errorf("fullText = %q, want %q", result.fullText, "streamed")
	}
}

func TestParseClaudeStream_SystemEvent(t *testing.T) {
	events, _ := collectEvents(t,
		`{"type":"system","subtype":"init"}`,
	)

	if len(events) == 0 {
		t.Fatal("expected at least 1 event")
	}
	if events[0].Type != "status" || events[0].Status != "thinking" {
		t.Errorf("expected status/thinking event, got %+v", events[0])
	}
}

func TestParseClaudeStream_EmptyLines(t *testing.T) {
	_, result := collectEvents(t,
		``,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"ok"}}`,
		``,
	)

	if result.fullText != "ok" {
		t.Errorf("fullText = %q, want %q", result.fullText, "ok")
	}
}

func TestParseClaudeStream_InvalidJSON(t *testing.T) {
	// Invalid JSON lines should be skipped
	_, result := collectEvents(t,
		`not json`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"valid"}}`,
	)

	if result.fullText != "valid" {
		t.Errorf("fullText = %q, want %q", result.fullText, "valid")
	}
}

func TestParseClaudeStream_Cancelled(t *testing.T) {
	input := `{"type":"content_block_delta","delta":{"type":"text_delta","text":"a"}}` + "\n" +
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"b"}}` + "\n"
	r := strings.NewReader(input)

	callCount := 0
	send := func(e ChatEvent) bool {
		callCount++
		return callCount < 2 // cancel after first event
	}

	result := parseClaudeStream(r, testLogger(), send)
	if !result.cancelled {
		t.Error("expected cancelled = true")
	}
}

// --- contentText characterization tests ---

func TestContentText_PlainString(t *testing.T) {
	raw := json.RawMessage(`"hello world"`)
	b := &claudeContentBlock{Content: raw}
	got := b.contentText()
	if got != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
}

func TestContentText_ArrayOfBlocks(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"Part 1"},{"type":"text","text":" Part 2"}]`)
	b := &claudeContentBlock{Content: raw}
	got := b.contentText()
	if got != "Part 1 Part 2" {
		t.Errorf("got %q, want %q", got, "Part 1 Part 2")
	}
}

func TestContentText_EmptyContent(t *testing.T) {
	b := &claudeContentBlock{Content: nil}
	got := b.contentText()
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestContentText_NonTextBlocks(t *testing.T) {
	raw := json.RawMessage(`[{"type":"image","text":"ignored"},{"type":"text","text":"visible"}]`)
	b := &claudeContentBlock{Content: raw}
	got := b.contentText()
	if got != "visible" {
		t.Errorf("got %q, want %q", got, "visible")
	}
}

func TestContentText_InvalidJSON(t *testing.T) {
	raw := json.RawMessage(`not json at all`)
	b := &claudeContentBlock{Content: raw}
	got := b.contentText()
	if got != "not json at all" {
		t.Errorf("got %q, want %q", got, "not json at all")
	}
}

// --- limitedWriter characterization tests ---

func TestLimitedWriter_WithinLimit(t *testing.T) {
	var buf bytes.Buffer
	lw := &limitedWriter{w: &buf, limit: 100}
	n, err := lw.Write([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("n = %d, want 5", n)
	}
	if buf.String() != "hello" {
		t.Errorf("buf = %q, want %q", buf.String(), "hello")
	}
}

func TestLimitedWriter_ExceedsLimit(t *testing.T) {
	var buf bytes.Buffer
	lw := &limitedWriter{w: &buf, limit: 5}
	n, err := lw.Write([]byte("hello world"))
	if err != nil {
		t.Fatal(err)
	}
	// Reports full length to avoid ErrShortWrite
	if n != 11 {
		t.Errorf("n = %d, want 11", n)
	}
	if buf.String() != "hello" {
		t.Errorf("buf = %q, want %q", buf.String(), "hello")
	}
}

func TestLimitedWriter_MultipleWrites(t *testing.T) {
	var buf bytes.Buffer
	lw := &limitedWriter{w: &buf, limit: 8}
	lw.Write([]byte("hello"))
	lw.Write([]byte(" world"))
	if buf.String() != "hello wo" {
		t.Errorf("buf = %q, want %q", buf.String(), "hello wo")
	}
}

func TestLimitedWriter_ZeroLimit(t *testing.T) {
	var buf bytes.Buffer
	lw := &limitedWriter{w: &buf, limit: 0}
	n, err := lw.Write([]byte("data"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("n = %d, want 4", n)
	}
	if buf.Len() != 0 {
		t.Error("expected empty buffer with zero limit")
	}
}

// --- claudeEncodePath characterization tests ---

func TestClaudeEncodePath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/Users/test/project", "-Users-test-project"},
		{"path.with.dots", "path-with-dots"},
		{"under_score", "under-score"},
		{"mixed/path.name_here", "mixed-path-name-here"},
	}
	for _, tt := range tests {
		got := claudeEncodePath(tt.input)
		if got != tt.want {
			t.Errorf("claudeEncodePath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// --- matchToolOutput characterization tests ---

func TestMatchToolOutput_ByID(t *testing.T) {
	tools := []ToolUse{
		{ID: "t1", Name: "Read", Output: ""},
		{ID: "t2", Name: "Write", Output: ""},
	}
	matchToolOutput(tools, "t1", "Read", "file contents")
	if tools[0].Output != "file contents" {
		t.Errorf("tools[0].Output = %q, want %q", tools[0].Output, "file contents")
	}
	if tools[1].Output != "" {
		t.Error("tools[1] should not be matched")
	}
}

func TestMatchToolOutput_ByName(t *testing.T) {
	tools := []ToolUse{
		{Name: "Read", Output: ""},
		{Name: "Read", Output: ""},
	}
	matchToolOutput(tools, "", "Read", "output")
	// Should match last unmatched
	if tools[0].Output != "" {
		t.Error("should match last, not first")
	}
	if tools[1].Output != "output" {
		t.Errorf("tools[1].Output = %q, want %q", tools[1].Output, "output")
	}
}

func TestMatchToolOutput_IDNotFound(t *testing.T) {
	tools := []ToolUse{
		{ID: "t1", Name: "Read", Output: ""},
	}
	matchToolOutput(tools, "nonexistent", "Read", "output")
	// Should NOT fall back to name matching when ID is provided
	if tools[0].Output != "" {
		t.Error("should not match by name when ID is provided but not found")
	}
}

// --- filterEnv characterization tests ---

func TestFilterEnv_RemovesPrefixes(t *testing.T) {
	t.Setenv("CLAUDE_CODE_TEST", "val1")
	t.Setenv("AGENT_BROWSER_SESSION", "old")
	t.Setenv("AGENT_BROWSER_COOKIE_DIR", "/old/path")

	env := filterEnv([]string{"CLAUDE_CODE", "AGENT_BROWSER_SESSION", "AGENT_BROWSER_COOKIE_DIR"}, "ag_test", "/tmp/ag_test")

	for _, e := range env {
		if strings.HasPrefix(e, "CLAUDE_CODE_TEST=") {
			t.Error("CLAUDE_CODE_TEST should be filtered")
		}
	}

	// Should have the new AGENT_BROWSER_SESSION, with old value filtered
	sessionCount := 0
	for _, e := range env {
		if e == "AGENT_BROWSER_SESSION=old" {
			t.Error("stale AGENT_BROWSER_SESSION=old should be filtered")
		}
		if e == "AGENT_BROWSER_SESSION=ag_test" {
			sessionCount++
		}
	}
	if sessionCount != 1 {
		t.Errorf("expected exactly 1 AGENT_BROWSER_SESSION=ag_test, got %d", sessionCount)
	}

	// Should have AGENT_BROWSER_COOKIE_DIR set to dataDir, with old value filtered
	cookieCount := 0
	for _, e := range env {
		if e == "AGENT_BROWSER_COOKIE_DIR=/old/path" {
			t.Error("stale AGENT_BROWSER_COOKIE_DIR=/old/path should be filtered")
		}
		if e == "AGENT_BROWSER_COOKIE_DIR=/tmp/ag_test" {
			cookieCount++
		}
	}
	if cookieCount != 1 {
		t.Errorf("expected exactly 1 AGENT_BROWSER_COOKIE_DIR=/tmp/ag_test, got %d", cookieCount)
	}
}

// --- isRealUserEntry characterization tests ---

func TestIsRealUserEntry_PlainString(t *testing.T) {
	raw := json.RawMessage(`{"content":"hello"}`)
	if !isRealUserEntry(raw) {
		t.Error("plain string content should be a real user entry")
	}
}

func TestIsRealUserEntry_ToolResult(t *testing.T) {
	raw := json.RawMessage(`{"content":[{"type":"tool_result","tool_use_id":"t1"}]}`)
	if isRealUserEntry(raw) {
		t.Error("tool_result content should NOT be a real user entry")
	}
}

func TestIsRealUserEntry_TextBlock(t *testing.T) {
	raw := json.RawMessage(`{"content":[{"type":"text"}]}`)
	if !isRealUserEntry(raw) {
		t.Error("text content block should be a real user entry")
	}
}

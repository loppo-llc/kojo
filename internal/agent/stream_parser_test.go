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

	result := parseClaudeStream(r, logger, send, nil)
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

// TestParseClaudeStream_ResultModelUsage verifies that the result event's
// modelUsage map (invocation-wide totals, including subagent usage) replaces
// the per-turn usage accumulated from assistant/message_delta events, sums
// across models, and carries total_cost_usd. A later result event (emitted
// after a background subagent finishes) must win over an earlier one.
func TestParseClaudeStream_ResultModelUsage(t *testing.T) {
	assistant := `{"type":"assistant","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"Hi"}],"usage":{"input_tokens":10,"output_tokens":5}}}`
	result1 := `{"type":"result","result":"r1","session_id":"s","total_cost_usd":0.01,"modelUsage":{"claude-fable-5":{"inputTokens":100,"outputTokens":50,"cacheReadInputTokens":1000,"cacheCreationInputTokens":200}}}`
	result2 := `{"type":"result","result":"r2","session_id":"s","total_cost_usd":0.05,"modelUsage":{"claude-fable-5":{"inputTokens":150,"outputTokens":80,"cacheReadInputTokens":2000,"cacheCreationInputTokens":300},"claude-opus-4-8":{"inputTokens":20,"outputTokens":40,"cacheReadInputTokens":500,"cacheCreationInputTokens":100}}}`

	_, result := collectEvents(t, assistant, result1, result2)

	if result.usage == nil {
		t.Fatal("expected usage")
	}
	u := result.usage
	if u.InputTokens != 170 || u.OutputTokens != 120 ||
		u.CacheReadInputTokens != 2500 || u.CacheCreationInputTokens != 400 {
		t.Errorf("usage = %+v, want summed modelUsage from last result {170 120 2500 400}", u)
	}
	if u.CostUSD != 0.05 {
		t.Errorf("CostUSD = %v, want 0.05", u.CostUSD)
	}
}

// TestParseClaudeStream_ResultWithoutModelUsage: a result event lacking
// modelUsage (older CLI) must not clobber usage accumulated from the stream.
func TestParseClaudeStream_ResultWithoutModelUsage(t *testing.T) {
	assistant := `{"type":"assistant","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"Hi"}],"usage":{"input_tokens":10,"output_tokens":5}}}`
	_, result := collectEvents(t, assistant, `{"type":"result","result":"done","session_id":"s"}`)

	if result.usage == nil || result.usage.InputTokens != 10 || result.usage.OutputTokens != 5 {
		t.Errorf("usage = %+v, want {10 5} preserved", result.usage)
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

	result := parseClaudeStream(r, testLogger(), send, nil)
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

// --- subagent (Task tool / parent_tool_use_id) characterization tests ---
//
// Line shapes below mirror a captured real stream from:
//   claude -p --model haiku --output-format stream-json --include-partial-messages \
//     "Use the Agent tool ... to have a subagent list files ... then summarize."
// Subagent turns arrived as complete (non-streamed-delta) "assistant"/"user"
// events carrying a top-level parent_tool_use_id — never via stream_event
// content_block_* deltas in that capture — so these tests exercise that
// shape. The content_block_delta path (guarded identically in
// parseClaudeStream) is covered by TestParseClaudeStream_SubagentStreamedDeltas.

func TestParseClaudeStream_SubagentDoesNotPolluteMainText(t *testing.T) {
	events, result := collectEvents(t,
		// Main turn spawns a Task tool call.
		`{"type":"content_block_start","content_block":{"type":"tool_use","id":"toolu_task1","name":"Task"}}`,
		`{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`{"type":"content_block_stop"}`,
		// Subagent turn: complete assistant event with parent_tool_use_id,
		// containing its own tool_use (Bash) block.
		`{"type":"assistant","parent_tool_use_id":"toolu_task1","message":{"content":[`+
			`{"type":"text","text":"Listing /tmp"},`+
			`{"type":"tool_use","id":"toolu_sub_bash1","name":"Bash","input":{"command":"ls /tmp"}}`+
			`]}}`,
		// Subagent tool result.
		`{"type":"user","parent_tool_use_id":"toolu_task1","message":{"content":[`+
			`{"type":"tool_result","tool_use_id":"toolu_sub_bash1","content":"file1\nfile2"}`+
			`]}}`,
		// Subagent final summary text.
		`{"type":"assistant","parent_tool_use_id":"toolu_task1","message":{"content":[`+
			`{"type":"text","text":"Found 2 files."}`+
			`]}}`,
		// Main turn's own text, after the subagent finishes.
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"Done."}}`,
	)

	if result.fullText != "Done." {
		t.Errorf("fullText = %q, want %q (subagent text must not leak into the main turn)", result.fullText, "Done.")
	}
	if len(result.toolUses) != 1 {
		t.Fatalf("toolUses = %d, want 1 (only the top-level Task call)", len(result.toolUses))
	}

	task := result.toolUses[0]
	if task.Name != "Task" {
		t.Errorf("toolUses[0].Name = %q, want Task", task.Name)
	}
	// Children preserve arrival order (text, then tool call, then text) —
	// they are NOT collapsed into a single trailing text bubble, so a
	// live UI can render "said X, ran Y, said Z" instead of "ran Y, said
	// X+Z".
	if len(task.Children) != 3 {
		t.Fatalf("Task.Children = %d, want 3 (text, Bash call, text), got %+v", len(task.Children), task.Children)
	}
	if task.Children[0].Text != "Listing /tmp" {
		t.Errorf("Task.Children[0].Text = %q, want %q", task.Children[0].Text, "Listing /tmp")
	}
	if task.Children[1].Name != "Bash" || task.Children[1].Output != "file1\nfile2" {
		t.Errorf("Task.Children[1] = %+v, want Bash call with matched output", task.Children[1])
	}
	if task.Children[2].Text != "Found 2 files." {
		t.Errorf("Task.Children[2].Text = %q, want %q", task.Children[2].Text, "Found 2 files.")
	}

	// Every event tagged with the subagent's parent id must carry
	// ParentToolUseID on the wire too, so a live UI can route it without
	// re-deriving ownership.
	for _, e := range events {
		if e.Type == "text" && e.Delta == "Listing /tmp" && e.ParentToolUseID != "toolu_task1" {
			t.Errorf("subagent text event missing ParentToolUseID: %+v", e)
		}
		if e.Type == "tool_use" && e.ToolUseID == "toolu_sub_bash1" && e.ParentToolUseID != "toolu_task1" {
			t.Errorf("subagent tool_use event missing ParentToolUseID: %+v", e)
		}
		if e.Type == "text" && e.Delta == "Done." && e.ParentToolUseID != "" {
			t.Errorf("main-turn text event must not carry ParentToolUseID: %+v", e)
		}
	}
}

func TestParseClaudeStream_SubagentStreamedDeltas(t *testing.T) {
	// Alternate shape: subagent tool_use streamed via content_block_start/
	// input_json_delta/content_block_stop, with parent_tool_use_id riding
	// the stream_event wrapper (as documented on --include-partial-messages).
	events, result := collectEvents(t,
		`{"type":"content_block_start","content_block":{"type":"tool_use","id":"toolu_task1","name":"Task"}}`,
		`{"type":"content_block_stop"}`,
		`{"type":"stream_event","parent_tool_use_id":"toolu_task1","event":{"type":"content_block_start","content_block":{"type":"tool_use","id":"toolu_sub1","name":"Read"}}}`,
		`{"type":"stream_event","parent_tool_use_id":"toolu_task1","event":{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{}"}}}`,
		`{"type":"stream_event","parent_tool_use_id":"toolu_task1","event":{"type":"content_block_stop"}}`,
		`{"type":"user","parent_tool_use_id":"toolu_task1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_sub1","content":"ok"}]}}`,
	)

	if result.fullText != "" {
		t.Errorf("fullText = %q, want empty", result.fullText)
	}
	if len(result.toolUses) != 1 || len(result.toolUses[0].Children) != 1 {
		t.Fatalf("unexpected toolUses shape: %+v", result.toolUses)
	}
	child := result.toolUses[0].Children[0]
	if child.Name != "Read" || child.Output != "ok" {
		t.Errorf("child = %+v, want Read tool with matched output", child)
	}

	sawParented := false
	for _, e := range events {
		if e.ToolUseID == "toolu_sub1" && e.ParentToolUseID == "toolu_task1" {
			sawParented = true
		}
	}
	if !sawParented {
		t.Error("expected at least one event for toolu_sub1 carrying ParentToolUseID")
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

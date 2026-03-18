package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

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

	env := filterEnv([]string{"CLAUDE_CODE", "AGENT_BROWSER_SESSION"}, "ag_test")

	for _, e := range env {
		if strings.HasPrefix(e, "CLAUDE_CODE_TEST=") {
			t.Error("CLAUDE_CODE_TEST should be filtered")
		}
	}

	// Should have the new AGENT_BROWSER_SESSION
	found := false
	for _, e := range env {
		if e == "AGENT_BROWSER_SESSION=ag_test" {
			found = true
		}
	}
	if !found {
		t.Error("expected AGENT_BROWSER_SESSION=ag_test in env")
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

package agent

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestReadSubagentMeta(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "agent-a.meta.json")
	if err := os.WriteFile(good, []byte(`{"toolUseId":"toolu_task1","other":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readSubagentMeta(good); got != "toolu_task1" {
		t.Errorf("toolUseId: got %q want toolu_task1", got)
	}
	if got := readSubagentMeta(filepath.Join(dir, "missing.meta.json")); got != "" {
		t.Errorf("missing meta: got %q want empty", got)
	}
	bad := filepath.Join(dir, "bad.meta.json")
	_ = os.WriteFile(bad, []byte(`{not json`), 0o644)
	if got := readSubagentMeta(bad); got != "" {
		t.Errorf("bad meta: got %q want empty", got)
	}
	noField := filepath.Join(dir, "nofield.meta.json")
	_ = os.WriteFile(noField, []byte(`{"agentId":"x"}`), 0o644)
	if got := readSubagentMeta(noField); got != "" {
		t.Errorf("no field: got %q want empty", got)
	}
}

func TestSubagentEventsFromLine(t *testing.T) {
	const parent = "toolu_task1"

	t.Run("assistant text and tool_use", func(t *testing.T) {
		line := []byte(`{"type":"assistant","uuid":"u1","isSidechain":true,"message":{"content":[` +
			`{"type":"text","text":"working on it"},` +
			`{"type":"tool_use","id":"toolu_child1","name":"Bash","input":{"command":"ls"}}` +
			`]}}`)
		events, children := subagentEventsFromLine(line, parent)
		if len(events) != 2 {
			t.Fatalf("events: got %d want 2 (%+v)", len(events), events)
		}
		if events[0].Type != "text" || events[0].Delta != "working on it" || events[0].ParentToolUseID != parent {
			t.Errorf("text event wrong: %+v", events[0])
		}
		if events[1].Type != "tool_use" || events[1].ToolUseID != "toolu_child1" || events[1].ToolName != "Bash" || events[1].ParentToolUseID != parent {
			t.Errorf("tool_use event wrong: %+v", events[1])
		}
		if len(children) != 2 {
			t.Fatalf("children: got %d want 2", len(children))
		}
		if children[0].Text != "working on it" || children[0].Name != "" {
			t.Errorf("text child wrong: %+v", children[0])
		}
		if children[0].ID == "" {
			t.Errorf("text child should carry a stable id for dedupe")
		}
		if children[1].ID != "toolu_child1" || children[1].Name != "Bash" {
			t.Errorf("tool child wrong: %+v", children[1])
		}
	})

	t.Run("user tool_result", func(t *testing.T) {
		line := []byte(`{"type":"user","uuid":"u2","message":{"content":[` +
			`{"type":"tool_result","tool_use_id":"toolu_child1","content":"file listing"}` +
			`]}}`)
		events, children := subagentEventsFromLine(line, parent)
		if len(events) != 1 || events[0].Type != "tool_result" || events[0].ToolUseID != "toolu_child1" || events[0].ToolOutput != "file listing" {
			t.Fatalf("tool_result event wrong: %+v", events)
		}
		if len(children) != 1 || children[0].ID != "toolu_child1" || children[0].Output != "file listing" || children[0].Name != "" {
			t.Fatalf("output-only child wrong: %+v", children)
		}
	})

	t.Run("unparseable and empty", func(t *testing.T) {
		if e, c := subagentEventsFromLine([]byte("{bad"), parent); e != nil || c != nil {
			t.Errorf("bad line should yield nothing: %v %v", e, c)
		}
		if e, c := subagentEventsFromLine([]byte(`{"type":"system"}`), parent); e != nil || c != nil {
			t.Errorf("system line should yield nothing: %v %v", e, c)
		}
	})
}

func TestMergeSubagentChildren(t *testing.T) {
	toolUse := ToolUse{ID: "toolu_c1", Name: "Bash", Input: `{"command":"ls"}`}
	textChild := ToolUse{ID: "txt:u1:0", Text: "hello"}

	t.Run("append new, idempotent on repeat", func(t *testing.T) {
		merged, changed := mergeSubagentChildren(nil, []ToolUse{textChild, toolUse})
		if !changed || len(merged) != 2 {
			t.Fatalf("first merge: changed=%v len=%d", changed, len(merged))
		}
		// Re-applying the exact same children must be a no-op.
		merged2, changed2 := mergeSubagentChildren(merged, []ToolUse{textChild, toolUse})
		if changed2 {
			t.Errorf("re-merge should be idempotent, changed=%v", changed2)
		}
		if len(merged2) != 2 {
			t.Errorf("re-merge len: got %d want 2", len(merged2))
		}
	})

	t.Run("tool_result folds output onto existing tool_use", func(t *testing.T) {
		base := []ToolUse{toolUse}
		merged, changed := mergeSubagentChildren(base, []ToolUse{{ID: "toolu_c1", Output: "done"}})
		if !changed {
			t.Fatal("output fold should change")
		}
		if len(merged) != 1 || merged[0].Output != "done" || merged[0].Name != "Bash" {
			t.Errorf("folded child wrong: %+v", merged)
		}
	})

	t.Run("orphan output-only entry dropped", func(t *testing.T) {
		merged, changed := mergeSubagentChildren(nil, []ToolUse{{ID: "toolu_unknown", Output: "x"}})
		if changed || len(merged) != 0 {
			t.Errorf("orphan output should be dropped: changed=%v len=%d", changed, len(merged))
		}
	})

	t.Run("cap enforced", func(t *testing.T) {
		var existing []ToolUse
		for i := 0; i < subagentMaxChildren; i++ {
			existing = append(existing, ToolUse{ID: "id" + strings.Repeat("x", 1) + itoa(i), Name: "T"})
		}
		merged, changed := mergeSubagentChildren(existing, []ToolUse{{ID: "overflow", Name: "T"}})
		if changed || len(merged) != subagentMaxChildren {
			t.Errorf("cap: changed=%v len=%d want %d", changed, len(merged), subagentMaxChildren)
		}
	})

	t.Run("empty id skipped", func(t *testing.T) {
		merged, changed := mergeSubagentChildren(nil, []ToolUse{{Text: "noid"}})
		if changed || len(merged) != 0 {
			t.Errorf("empty-id child should be skipped: changed=%v len=%d", changed, len(merged))
		}
	})
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestScanAppendedLines(t *testing.T) {
	// A trailing partial line (no newline) must not be consumed.
	content := "line1\nline2\npartial-no-newline"
	f := newReaderAt(content)
	lines, off := scanAppendedLines(f, 0, 1<<20)
	if len(lines) != 2 || string(lines[0]) != "line1" || string(lines[1]) != "line2" {
		t.Fatalf("lines: %q", lines)
	}
	if off != int64(len("line1\nline2\n")) {
		t.Errorf("offset: got %d want %d", off, len("line1\nline2\n"))
	}

	// Resuming from the offset picks up the rest once its newline lands.
	rest := content + "\nline3\n"
	f2 := newReaderAt(rest)
	lines2, off2 := scanAppendedLines(f2, off, 1<<20)
	if len(lines2) != 2 || string(lines2[0]) != "partial-no-newline" || string(lines2[1]) != "line3" {
		t.Fatalf("resumed lines: %q", lines2)
	}
	if off2 != int64(len(rest)) {
		t.Errorf("resumed offset: got %d want %d", off2, len(rest))
	}

	// Empty lines are skipped but their bytes still advance the offset.
	f3 := newReaderAt("\n\nx\n")
	lines3, off3 := scanAppendedLines(f3, 0, 1<<20)
	if len(lines3) != 1 || string(lines3[0]) != "x" {
		t.Fatalf("empty-line handling: %q", lines3)
	}
	if off3 != 4 {
		t.Errorf("empty-line offset: got %d want 4", off3)
	}
}

// readerAt adapts a string to io.ReaderAt for scanAppendedLines tests.
type readerAt struct{ s string }

func newReaderAt(s string) *readerAt { return &readerAt{s: s} }

func (r *readerAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(r.s)) {
		return 0, io.EOF
	}
	n := copy(p, r.s[off:])
	if int(off)+n >= len(r.s) {
		return n, io.EOF
	}
	return n, nil
}

func TestMergeChildrenIntoMessage(t *testing.T) {
	msg := &Message{
		ID: "m1",
		ToolUses: []ToolUse{
			{ID: "toolu_other", Name: "Read"},
			{ID: "toolu_task1", Name: "Task"},
		},
	}
	if !messageOwnsToolUse(msg, "toolu_task1") {
		t.Fatal("should own toolu_task1")
	}
	if messageOwnsToolUse(msg, "nope") {
		t.Fatal("should not own nope")
	}
	changed := mergeChildrenIntoMessage(msg, "toolu_task1", []ToolUse{{ID: "c1", Name: "Bash"}})
	if !changed || len(msg.ToolUses[1].Children) != 1 {
		t.Fatalf("attach: changed=%v children=%d", changed, len(msg.ToolUses[1].Children))
	}
	// Idempotent re-apply.
	if mergeChildrenIntoMessage(msg, "toolu_task1", []ToolUse{{ID: "c1", Name: "Bash"}}) {
		t.Error("re-apply should be no-op")
	}
	// Unknown tool_use → no change.
	if mergeChildrenIntoMessage(msg, "toolu_absent", []ToolUse{{ID: "c2", Name: "Bash"}}) {
		t.Error("absent tool_use should not change")
	}
}

// TestSubagentTailerEndToEnd exercises meta mapping + JSONL→event conversion +
// offset bookkeeping through the tailer against fixture files on disk.
func TestSubagentTailerEndToEnd(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	workDir := t.TempDir()
	const sid = "sess-abc"

	subDir := filepath.Join(claudeProjectDir(mustAbs(t, workDir)), sid, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(subDir, "agent-x.meta.json")
	jsonlPath := filepath.Join(subDir, "agent-x.jsonl")
	if err := os.WriteFile(metaPath, []byte(`{"toolUseId":"toolu_task1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	line1 := `{"type":"assistant","uuid":"u1","isSidechain":true,"message":{"content":[{"type":"tool_use","id":"toolu_c1","name":"Bash","input":{"command":"ls"}}]}}` + "\n"
	if err := os.WriteFile(jsonlPath, []byte(line1), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var batches []subagentActivity
	emit := func(agentID string, act subagentActivity) {
		mu.Lock()
		defer mu.Unlock()
		if agentID != "ag_x" {
			t.Errorf("agentID: got %q", agentID)
		}
		batches = append(batches, act)
	}

	tl := newSubagentTailer("ag_x", workDir, testLogger(), func() string { return sid }, emit)

	tl.scanOnce()
	mu.Lock()
	if len(batches) != 1 {
		mu.Unlock()
		t.Fatalf("first scan: got %d batches want 1", len(batches))
	}
	if batches[0].ToolUseID != "toolu_task1" {
		t.Errorf("toolUseId: got %q", batches[0].ToolUseID)
	}
	if len(batches[0].Events) != 1 || batches[0].Events[0].ToolUseID != "toolu_c1" {
		t.Errorf("events: %+v", batches[0].Events)
	}
	mu.Unlock()

	// Second scan with no new bytes: no emit (offset consumed → idempotent).
	tl.scanOnce()
	mu.Lock()
	if len(batches) != 1 {
		mu.Unlock()
		t.Fatalf("idempotent scan: got %d batches want 1", len(batches))
	}
	mu.Unlock()

	// Append a tool_result line, then scan: only the new line surfaces.
	line2 := `{"type":"user","uuid":"u2","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_c1","content":"ok"}]}}` + "\n"
	appendToFile(t, jsonlPath, line2)
	tl.scanOnce()
	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 2 {
		t.Fatalf("after append: got %d batches want 2", len(batches))
	}
	last := batches[1]
	if len(last.Events) != 1 || last.Events[0].Type != "tool_result" || last.Events[0].ToolOutput != "ok" {
		t.Errorf("appended events: %+v", last.Events)
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func appendToFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// TestAttachSubagentChildrenDurable verifies the store-backed durable attach:
// children land on the owning message's Task ToolUse and survive a reload, and
// a repeat attach is idempotent.
func TestAttachSubagentChildrenDurable(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	agentID := "ag_subagent_attach"
	dir := filepath.Join(tmp, ".config", "kojo-v1", "agents", agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcriptTestSetup(t, agentID)

	msg := &Message{
		ID:        "m_task",
		Role:      "assistant",
		Content:   "spawning a background agent",
		Timestamp: "2024-01-01T00:00:01Z",
		ToolUses:  []ToolUse{{ID: "toolu_task1", Name: "Task", Input: `{}`, Output: "Async agent launched"}},
	}
	if err := appendMessage(agentID, msg); err != nil {
		t.Fatal(err)
	}

	m := &Manager{logger: testLogger(), agents: map[string]*Agent{agentID: {ID: agentID}}}

	if err := m.attachSubagentChildren(agentID, "toolu_task1", []ToolUse{
		{ID: "toolu_c1", Name: "Bash", Input: `{"command":"ls"}`},
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}

	reload := func() *Message {
		msgs, err := loadMessages(agentID, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, mm := range msgs {
			if mm.ID == "m_task" {
				return mm
			}
		}
		t.Fatal("message vanished")
		return nil
	}
	got := reload()
	if len(got.ToolUses) != 1 || len(got.ToolUses[0].Children) != 1 || got.ToolUses[0].Children[0].ID != "toolu_c1" {
		t.Fatalf("durable children not persisted: %+v", got.ToolUses)
	}

	// Fold a tool_result output, then re-apply the same → idempotent.
	if err := m.attachSubagentChildren(agentID, "toolu_task1", []ToolUse{{ID: "toolu_c1", Output: "listing"}}); err != nil {
		t.Fatalf("attach output: %v", err)
	}
	if err := m.attachSubagentChildren(agentID, "toolu_task1", []ToolUse{{ID: "toolu_c1", Output: "listing"}}); err != nil {
		t.Fatalf("attach output idempotent: %v", err)
	}
	got = reload()
	if got.ToolUses[0].Children[0].Output != "listing" {
		t.Fatalf("output not folded: %+v", got.ToolUses[0].Children)
	}

	// Unknown tool_use id → ErrMessageNotFound (benign miss).
	err := m.attachSubagentChildren(agentID, "toolu_absent", []ToolUse{{ID: "c", Name: "X"}})
	if err == nil {
		t.Fatal("expected ErrMessageNotFound for unknown tool_use")
	}
}

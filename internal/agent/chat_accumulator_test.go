package agent

import (
	"testing"
)

func TestChatAccumulator_FoldsTextThinkingToolUse(t *testing.T) {
	a := newChatAccumulator()
	a.OnEvent(&ChatEvent{Type: "text", Delta: "Hello "})
	a.OnEvent(&ChatEvent{Type: "text", Delta: "world"})
	a.OnEvent(&ChatEvent{Type: "thinking", Delta: "reasoning..."})
	a.OnEvent(&ChatEvent{Type: "tool_use", ToolUseID: "t1", ToolName: "Bash", ToolInput: `{"command":"echo hi"}`})
	a.OnEvent(&ChatEvent{Type: "tool_result", ToolUseID: "t1", ToolName: "Bash", ToolOutput: "hi\n"})

	text, thinking, tools := a.Snapshot()
	if text != "Hello world" {
		t.Errorf("text = %q; want %q", text, "Hello world")
	}
	if thinking != "reasoning..." {
		t.Errorf("thinking = %q; want %q", thinking, "reasoning...")
	}
	if len(tools) != 1 {
		t.Fatalf("toolUses len = %d; want 1", len(tools))
	}
	if tools[0].ID != "t1" || tools[0].Name != "Bash" {
		t.Errorf("toolUse[0] = %+v; want id=t1 name=Bash", tools[0])
	}
	if tools[0].Output != "hi\n" {
		t.Errorf("toolUse[0].Output = %q; want %q", tools[0].Output, "hi\n")
	}
}

func TestChatAccumulator_NilSafe(t *testing.T) {
	var a *chatAccumulator
	a.OnEvent(&ChatEvent{Type: "text", Delta: "x"}) // must not panic
	if !a.IsEmpty() {
		t.Error("nil accumulator must report IsEmpty=true")
	}
	text, thinking, tools := a.Snapshot()
	if text != "" || thinking != "" || tools != nil {
		t.Errorf("nil snapshot returned non-zero values: %q %q %v", text, thinking, tools)
	}
}

func TestChatAccumulator_SnapshotDeepCopiesTools(t *testing.T) {
	a := newChatAccumulator()
	a.OnEvent(&ChatEvent{Type: "tool_use", ToolUseID: "t1", ToolName: "Bash"})

	_, _, tools1 := a.Snapshot()
	if len(tools1) != 1 {
		t.Fatalf("snapshot tools len = %d; want 1", len(tools1))
	}
	// Caller-side mutation must not leak back into the accumulator.
	tools1[0].Name = "MUTATED"

	_, _, tools2 := a.Snapshot()
	if tools2[0].Name != "Bash" {
		t.Errorf("internal state mutated by caller: tools2[0].Name = %q; want Bash", tools2[0].Name)
	}
}

func TestChatAccumulator_IsEmptyTracksContent(t *testing.T) {
	a := newChatAccumulator()
	if !a.IsEmpty() {
		t.Error("fresh accumulator must be empty")
	}
	a.OnEvent(&ChatEvent{Type: "text", Delta: "x"})
	if a.IsEmpty() {
		t.Error("after text event, must not be empty")
	}

	b := newChatAccumulator()
	b.OnEvent(&ChatEvent{Type: "thinking", Delta: "x"})
	if b.IsEmpty() {
		t.Error("after thinking event, must not be empty")
	}

	c := newChatAccumulator()
	c.OnEvent(&ChatEvent{Type: "tool_use", ToolUseID: "t1", ToolName: "Bash"})
	if c.IsEmpty() {
		t.Error("after tool_use event, must not be empty")
	}
}

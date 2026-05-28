package agent

import (
	"context"
	"sync"
	"testing"
	"time"
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

// TestChatAccumulator_WaitDoneReturnsMessage exercises the done-wait
// path used by the device-switch deferred finalize goroutine: after
// MarkDone fires, WaitDone returns a copy of the captured Message.
func TestChatAccumulator_WaitDoneReturnsMessage(t *testing.T) {
	a := newChatAccumulator()
	msg := &Message{Role: "assistant", Content: "到着したらセキュリティチェックを実施する"}

	// Fire MarkDone from a goroutine to mirror the production
	// timing: the switch handler spawns a goroutine that sits on
	// WaitDone, the chat goroutine fires MarkDone when claude's
	// done event lands.
	go func() {
		time.Sleep(10 * time.Millisecond)
		a.MarkDone(msg)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := a.WaitDone(ctx)
	if got == nil {
		t.Fatal("WaitDone returned nil; expected captured Message")
	}
	if got.Content != msg.Content {
		t.Errorf("got.Content = %q; want %q", got.Content, msg.Content)
	}
	if got == msg {
		t.Error("WaitDone returned the same pointer; must be a copy")
	}
}

// TestChatAccumulator_WaitDoneCtxCancel: a cancelled ctx returns nil
// without blocking forever. The deferred finalize goroutine relies on
// this to fall back to no-tail finalize when source's claude hangs.
func TestChatAccumulator_WaitDoneCtxCancel(t *testing.T) {
	a := newChatAccumulator()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	got := a.WaitDone(ctx)
	elapsed := time.Since(start)
	if got != nil {
		t.Errorf("WaitDone returned %+v; want nil after ctx cancel", got)
	}
	if elapsed < 40*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Errorf("WaitDone returned in %v; want ~50ms after ctx cancel", elapsed)
	}
}

// TestChatAccumulator_MarkDoneIdempotent: a second MarkDone call
// must not panic on the already-closed doneCh and must not overwrite
// the first message.
func TestChatAccumulator_MarkDoneIdempotent(t *testing.T) {
	a := newChatAccumulator()
	first := &Message{Role: "assistant", Content: "first"}
	second := &Message{Role: "assistant", Content: "second"}
	a.MarkDone(first)
	a.MarkDone(second) // must not panic

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got := a.WaitDone(ctx)
	if got == nil || got.Content != "first" {
		t.Errorf("WaitDone returned %+v; want first message", got)
	}
}

// TestChatAccumulator_WaitDoneBeforeAndAfter: multiple WaitDone
// callers (some pre-MarkDone, some post) all observe the same
// captured Message.
func TestChatAccumulator_WaitDoneMultipleWaiters(t *testing.T) {
	a := newChatAccumulator()
	msg := &Message{Role: "assistant", Content: "tail"}

	var wg sync.WaitGroup
	results := make([]*Message, 4)
	for i := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			results[idx] = a.WaitDone(ctx)
		}(i)
	}

	time.Sleep(20 * time.Millisecond)
	a.MarkDone(msg)
	wg.Wait()

	for i, r := range results {
		if r == nil || r.Content != "tail" {
			t.Errorf("waiter %d got %+v; want %q", i, r, "tail")
		}
	}
}

// TestChatAccumulator_NilWaitDone: nil receiver returns nil without
// panicking. Mirrors the SnapshotAccumulatedMessageRecord nil-safe
// posture for test fixtures that skip the accumulator.
func TestChatAccumulator_NilWaitDone(t *testing.T) {
	var a *chatAccumulator
	if got := a.WaitDone(context.Background()); got != nil {
		t.Errorf("nil receiver returned %+v; want nil", got)
	}
	a.MarkDone(&Message{}) // must not panic
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

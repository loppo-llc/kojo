package agent

import (
	"context"
	"strings"
	"sync"
)

// chatAccumulator records the in-flight assistant turn as it streams.
//
// processChatEvents already keeps its own local strings.Builder
// fan-in for abort-time persistence, but the broadcaster (which
// SnapshotAccumulatedMessageRecord used to read) is fed via a
// non-blocking send on a 64-event buffered outCh — so under a slow
// reader / long claude turn the broadcaster's log silently misses
// events. For the §3.7 self-call snapshot (target receives the
// in-flight assistant turn so the migrated session has the kojo-
// switch-device tool_use), that meant the wire payload could be
// missing recent text / tool_use rows.
//
// chatAccumulator is the second consumer of every chat event, fed
// inline from processChatEvents (NOT through outCh). Snapshots read
// from this struct under the same mutex so the §3.7 payload always
// reflects exactly what the chat goroutine has seen, including
// events the broadcaster never logged.
//
// Safe for concurrent OnEvent / Snapshot. Nil receivers are no-ops
// (test fixtures and the legacy hand-rolled Manager paths construct
// busy entries without an accumulator).
type chatAccumulator struct {
	mu       sync.Mutex
	text     strings.Builder
	thinking strings.Builder
	toolUses []ToolUse

	// doneCh closes once MarkDone fires. WaitDone blocks on it.
	// Allocated by newChatAccumulator so a Wait that fires BEFORE
	// MarkDone still sees a non-nil channel under a nil-safe receiver.
	doneCh   chan struct{}
	doneMsg  *Message
	doneOnce sync.Once
}

func newChatAccumulator() *chatAccumulator {
	return &chatAccumulator{doneCh: make(chan struct{})}
}

// OnEvent folds a streaming event into the accumulator. Mirrors the
// switch in processChatEvents.accumulate but stores results in shared
// state so a SnapshotAccumulatedMessageRecord caller can read them.
func (a *chatAccumulator) OnEvent(ev *ChatEvent) {
	if a == nil || ev == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch ev.Type {
	case "text":
		a.text.WriteString(ev.Delta)
	case "thinking":
		a.thinking.WriteString(ev.Delta)
	case "tool_use":
		a.toolUses = append(a.toolUses, ToolUse{
			ID:    ev.ToolUseID,
			Name:  ev.ToolName,
			Input: ev.ToolInput,
		})
	case "tool_result":
		matchToolOutput(a.toolUses, ev.ToolUseID, ev.ToolName, ev.ToolOutput)
	}
}

// Snapshot returns a deep copy of the accumulated state so the
// caller can construct a MessageRecord without holding the lock.
// Returns ("", "", nil) when no streaming data has arrived yet.
func (a *chatAccumulator) Snapshot() (text, thinking string, toolUses []ToolUse) {
	if a == nil {
		return "", "", nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	text = a.text.String()
	thinking = a.thinking.String()
	if len(a.toolUses) > 0 {
		toolUses = make([]ToolUse, len(a.toolUses))
		copy(toolUses, a.toolUses)
	}
	return
}

// IsEmpty reports whether any streaming data has been recorded. Used
// by SnapshotAccumulatedMessageRecord to bail before allocating a
// MessageRecord we'd just discard.
func (a *chatAccumulator) IsEmpty() bool {
	if a == nil {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.text.Len() == 0 && a.thinking.Len() == 0 && len(a.toolUses) == 0
}

// MarkDone records the final assistant Message produced by the
// backend and unblocks every WaitDone waiter. Idempotent under
// sync.Once so a duplicate done event (rare — accumulator survives
// processChatEvents's drain loop) does not panic on a closed channel.
//
// Called from processChatEvents.handleTerminal BEFORE the §3.7
// release guard short-circuits, so the snapshot path stays useful
// even when the source peer has been evicted mid-turn (the very
// case A in `/handoff/switch` defers finalize for — the agent's
// post-tool-result text is generated AFTER the lock has moved, and
// without MarkDone there's no way to recover it for shipment to
// target).
func (a *chatAccumulator) MarkDone(msg *Message) {
	if a == nil || msg == nil {
		return
	}
	a.doneOnce.Do(func() {
		a.mu.Lock()
		cp := *msg
		a.doneMsg = &cp
		a.mu.Unlock()
		close(a.doneCh)
	})
}

// WaitDone blocks until MarkDone fires or ctx is cancelled. Returns a
// copy of the captured assistant Message on success; nil on ctx
// cancellation. Safe to call BEFORE MarkDone — the channel was
// allocated at constructor time.
//
// Returns nil for a nil receiver so the device-switch goroutine that
// uses this can bail without panicking when the busy entry vanished
// between the switch handler's accumulator grab and the goroutine
// run.
func (a *chatAccumulator) WaitDone(ctx context.Context) *Message {
	if a == nil {
		return nil
	}
	select {
	case <-a.doneCh:
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.doneMsg == nil {
			return nil
		}
		cp := *a.doneMsg
		return &cp
	case <-ctx.Done():
		return nil
	}
}


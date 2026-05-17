package agent

import (
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
}

func newChatAccumulator() *chatAccumulator {
	return &chatAccumulator{}
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


package agent

import (
	"encoding/json"
	"time"
)

// MessageAttachment represents a file attached to a chat message.
type MessageAttachment struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	Mime string `json:"mime"`
}

// Message represents a single chat message in the transcript.
type Message struct {
	ID          string              `json:"id"`
	Role        string              `json:"role"` // "user", "assistant", "system"
	Content     string              `json:"content"`
	Thinking    string              `json:"thinking,omitempty"` // intermediate reasoning (shown collapsed in UI)
	ToolUses    []ToolUse           `json:"toolUses,omitempty"`
	Attachments []MessageAttachment `json:"attachments,omitempty"`
	Timestamp   string              `json:"timestamp"` // RFC3339
	// CreatedAtMillis is the row's created_at in epoch-millis. Not
	// serialized (the wire carries the RFC3339 Timestamp) but retained
	// in-process so list-ordering can break same-second ties that the
	// seconds-resolution RFC3339 string cannot. Zero when the message
	// did not originate from a store record.
	CreatedAtMillis int64  `json:"-"`
	Usage           *Usage `json:"usage,omitempty"`
	// ETag is the inner identifier of the row's strong HTTP entity
	// tag — UNQUOTED (e.g. "v3-deadbeef") so the JSON looks natural;
	// the matching HTTP ETag header carries the same identifier
	// wrapped in double quotes per RFC 7232 (`"v3-deadbeef"`). The
	// Web UI's httpClient.patchWithIfMatch quotes this raw value
	// before putting it on the wire as If-Match.
	//
	// Surfaced inside the JSON so a list fetch
	// (GET /api/v1/agents/{id}/messages) gives the Web UI per-message
	// etags without a per-row HEAD; PATCH /messages/{msgId} then
	// sends this value as If-Match for optimistic concurrency.
	// Empty when the message originated outside the v1 store (legacy
	// in-memory paths, transitional rows).
	ETag string `json:"etag,omitempty"`
}

// ToolUse represents a single tool invocation within a message.
type ToolUse struct {
	ID     string `json:"id,omitempty"` // tool call ID for matching results
	Name   string `json:"name"`
	Input  string `json:"input"`  // JSON string
	Output string `json:"output"` // truncated output
	// Text holds a subagent narrative snippet when this entry is a
	// synthetic "text bubble" child rather than a real tool call (Name
	// and Input are empty in that case). Only ever populated on entries
	// that live inside Children.
	Text string `json:"text,omitempty"`
	// Children holds tool calls (and narrative text bubbles) emitted by
	// a subagent spawned via the Task tool, when this ToolUse is that
	// Task invocation. Populated flat (one level) even for nested
	// sub-subagents — see parseClaudeStream's subagentOwner mapping,
	// which folds any deeper nesting onto the top-level Task ToolUse
	// instead of building a recursive tree.
	Children []ToolUse `json:"children,omitempty"`
}

// Usage tracks token consumption for a message.
//
// CacheReadInputTokens / CacheCreationInputTokens are surfaced from
// Claude's stream so we can diagnose prompt-cache hit/miss patterns. A
// high CacheCreation:CacheRead ratio across consecutive turns indicates
// the system prompt or message prefix is changing between turns and
// breaking the cache — which directly inflates input cost and adds
// per-turn latency. (Note: cache state itself does not change the
// model's logical context length; what does grow is the volume of input
// tokens billed.)
type Usage struct {
	InputTokens              int `json:"inputTokens"`
	OutputTokens             int `json:"outputTokens"`
	CacheReadInputTokens     int `json:"cacheReadInputTokens,omitempty"`
	CacheCreationInputTokens int `json:"cacheCreationInputTokens,omitempty"`
	// CostUSD is the backend-reported total cost for the invocation (from
	// the Claude CLI "result" event's total_cost_usd), covering subagent
	// usage and per-model rates. Zero when the backend didn't report a
	// cost; the UI then falls back to a client-side estimate.
	CostUSD float64 `json:"costUSD,omitempty"`
}

// ChatEvent is streamed from backend to WebSocket during a chat.
type ChatEvent struct {
	Type         string              `json:"type"` // "status", "text", "thinking", "tool_use", "tool_result", "done", "error", "message", "attachment"
	Status       string              `json:"status,omitempty"`
	Delta        string              `json:"delta,omitempty"`
	ToolUseID    string              `json:"toolUseId,omitempty"`
	ToolName     string              `json:"toolName,omitempty"`
	ToolInput    string              `json:"toolInput,omitempty"`
	ToolOutput   string              `json:"toolOutput,omitempty"`
	Message      *Message            `json:"message,omitempty"`
	Attachments  []MessageAttachment `json:"attachments,omitempty"` // streamed kojo-attach files
	Usage        *Usage              `json:"usage,omitempty"`
	ErrorMessage string              `json:"errorMessage,omitempty"`
	// ParentToolUseID is set when this event originates from a subagent
	// spawned by a Task tool call rather than the main assistant turn.
	// Its value is the tool_use ID of the parent Task invocation (or, for
	// a nested sub-subagent, the ID of an intermediate subagent tool call
	// — callers should resolve that up to the nearest top-level Task via
	// the same flat-owner logic parseClaudeStream uses). Consumers that
	// accumulate the main turn's text/toolUses MUST skip events carrying
	// a non-empty ParentToolUseID; the UI instead nests them under the
	// matching Task tool chip.
	ParentToolUseID string `json:"parentToolUseId,omitempty"`
	// RequestID identifies a control_request the CLI raised for an
	// interactive tool (currently AskUserQuestion). Set only on
	// "user_question" events; the Web UI echoes it back to
	// POST /agents/{id}/answer so the server can pair the answer with the
	// blocked control_request.
	RequestID string `json:"requestId,omitempty"`
	// Questions carries the raw AskUserQuestion input.questions array
	// (the CLI's tool input) so the UI can render the question card. Only
	// populated on "user_question" events.
	Questions json.RawMessage `json:"questions,omitempty"`
	// RateLimit carries the latest rate-limit snapshot parsed from the
	// backend stream. Populated only on "rate_limit" events, which arrive
	// mid-turn whenever the Claude CLI reports a usage threshold crossing.
	RateLimit *RateLimitInfo `json:"rateLimit,omitempty"`
}

func generateMessageID() string {
	return generatePrefixedID("m_")
}

func newUserMessage(content string, attachments []MessageAttachment) *Message {
	return &Message{
		ID:          generateMessageID(),
		Role:        "user",
		Content:     content,
		Attachments: attachments,
		Timestamp:   time.Now().Format(time.RFC3339),
	}
}

func newSystemMessage(content string) *Message {
	return &Message{
		ID:        generateMessageID(),
		Role:      "system",
		Content:   content,
		Timestamp: time.Now().Format(time.RFC3339),
	}
}

func newAssistantMessage() *Message {
	return &Message{
		ID:        generateMessageID(),
		Role:      "assistant",
		Timestamp: time.Now().Format(time.RFC3339),
	}
}

// assembleAssistantMessage builds a completed assistant Message from
// accumulated stream data. Backends pass nil for fields they never
// populate (grok has no Usage; llama.cpp has no ToolUses).
func assembleAssistantMessage(content, thinking string, toolUses []ToolUse, usage *Usage) *Message {
	msg := newAssistantMessage()
	msg.Content = content
	msg.Thinking = thinking
	msg.ToolUses = toolUses
	msg.Usage = usage
	return msg
}

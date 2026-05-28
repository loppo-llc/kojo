package agent

import (
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
	Usage       *Usage              `json:"usage,omitempty"`
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
}

// ChatEvent is streamed from backend to WebSocket during a chat.
type ChatEvent struct {
	Type         string   `json:"type"` // "status", "text", "thinking", "tool_use", "tool_result", "done", "error", "message"
	Status       string   `json:"status,omitempty"`
	Delta        string   `json:"delta,omitempty"`
	ToolUseID    string   `json:"toolUseId,omitempty"`
	ToolName     string   `json:"toolName,omitempty"`
	ToolInput    string   `json:"toolInput,omitempty"`
	ToolOutput   string   `json:"toolOutput,omitempty"`
	Message      *Message `json:"message,omitempty"`
	Usage        *Usage   `json:"usage,omitempty"`
	ErrorMessage string   `json:"errorMessage,omitempty"`
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

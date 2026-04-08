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
}

// ToolUse represents a single tool invocation within a message.
type ToolUse struct {
	ID     string `json:"id,omitempty"` // tool call ID for matching results
	Name   string `json:"name"`
	Input  string `json:"input"`  // JSON string
	Output string `json:"output"` // truncated output
}

// Usage tracks token consumption for a message.
type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
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

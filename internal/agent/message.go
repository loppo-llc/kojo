package agent

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Message represents a single chat message in the transcript.
type Message struct {
	ID        string     `json:"id"`
	Role      string     `json:"role"` // "user", "assistant", "system"
	Content   string     `json:"content"`
	ToolUses  []ToolUse  `json:"toolUses,omitempty"`
	Timestamp string     `json:"timestamp"` // RFC3339
	Usage     *Usage     `json:"usage,omitempty"`
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
	Type         string   `json:"type"` // "status", "text", "tool_use", "tool_result", "done", "error"
	Status       string   `json:"status,omitempty"`
	Delta        string   `json:"delta,omitempty"`
	ToolName     string   `json:"toolName,omitempty"`
	ToolInput    string   `json:"toolInput,omitempty"`
	ToolOutput   string   `json:"toolOutput,omitempty"`
	Message      *Message `json:"message,omitempty"`
	Usage        *Usage   `json:"usage,omitempty"`
	ErrorMessage string   `json:"errorMessage,omitempty"`
}

func generateMessageID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "m_" + hex.EncodeToString(b)
}

func newUserMessage(content string) *Message {
	return &Message{
		ID:        generateMessageID(),
		Role:      "user",
		Content:   content,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
}

func newSystemMessage(content string) *Message {
	return &Message{
		ID:        generateMessageID(),
		Role:      "system",
		Content:   content,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
}

func newAssistantMessage() *Message {
	return &Message{
		ID:        generateMessageID(),
		Role:      "assistant",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
}

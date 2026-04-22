package agent

import (
	"context"
	"os"
	"strings"
)

// ErrMsgTimeout is the error message attached to a "done" event when
// the backend process is terminated due to a context timeout.
const ErrMsgTimeout = "timeout: process was terminated"

// ErrMsgCancelled is the error message attached to a "done" event when
// the backend process is terminated due to a user-initiated cancellation
// (context.Canceled) rather than a deadline.
const ErrMsgCancelled = "cancelled: process was terminated"

// ChatOptions holds optional parameters for a chat invocation.
type ChatOptions struct {
	// OneShot skips session resumption, running a fresh ephemeral session.
	// Used for Slack and other external platform conversations that have
	// their own conversation context.
	OneShot bool

	// MCPServers is the set of MCP tool servers to make available for this
	// chat session. Each backend injects them in its own way (CLI args,
	// config files, etc.). May be nil if no MCP servers are configured.
	MCPServers map[string]mcpServerEntry

	// AutomatedTrigger marks the chat as a non-interactive system fire
	// (cron, groupdm notification, notify poller, etc.) rather than a
	// human-driven turn. Backends use this to disable the idle-window
	// guard on session resets: there is no interactive conversation to
	// preserve, so token conservation wins over continuity.
	AutomatedTrigger bool
}

// ChatBackend abstracts a CLI tool for agent chat.
type ChatBackend interface {
	// Chat sends a message and returns a channel of streaming events.
	// The channel is closed when the response is complete.
	Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, error)

	// Name returns the tool identifier (e.g. "claude", "codex", "gemini").
	Name() string

	// Available returns true if the CLI tool is installed and accessible.
	Available() bool
}

// filterEnv returns a copy of os.Environ() with entries matching any of the
// given prefixes removed, and AGENT_BROWSER_SESSION / AGENT_BROWSER_COOKIE_DIR
// vars set to agentID / dataDir.
func filterEnv(removePrefixes []string, agentID, dataDir string) []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		skip := false
		for _, prefix := range removePrefixes {
			if strings.HasPrefix(e, prefix) {
				skip = true
				break
			}
		}
		if !skip {
			filtered = append(filtered, e)
		}
	}
	filtered = append(filtered,
		"AGENT_BROWSER_SESSION="+agentID,
		"AGENT_BROWSER_SESSION_NAME="+agentID,
		"AGENT_BROWSER_COOKIE_DIR="+dataDir,
	)
	return filtered
}

// emitCancelDone sends a partial "done" event when the backend process is
// terminated due to context cancellation. The ErrorMessage distinguishes
// timeout (context.DeadlineExceeded) from user-initiated abort
// (context.Canceled), so the transcript does not mislabel aborts as timeouts.
func emitCancelDone(ctx context.Context, ch chan<- ChatEvent, content, thinking string, toolUses []ToolUse, usage *Usage) {
	msg := newAssistantMessage()
	msg.Content = content
	msg.Thinking = thinking
	msg.ToolUses = toolUses
	msg.Usage = usage
	errMsg := ErrMsgTimeout
	if ctx.Err() == context.Canceled {
		errMsg = ErrMsgCancelled
	}
	ch <- ChatEvent{
		Type:         "done",
		Message:      msg,
		Usage:        usage,
		ErrorMessage: errMsg,
	}
}

// matchToolOutput pairs a tool result with the most recent matching ToolUse
// that has no output yet. When a tool use ID is provided, only ID-based
// matching is used to avoid mispairing parallel calls with the same name.
func matchToolOutput(toolUses []ToolUse, id, name, output string) {
	if id != "" {
		for i := len(toolUses) - 1; i >= 0; i-- {
			if toolUses[i].ID == id && toolUses[i].Output == "" {
				toolUses[i].Output = output
				return
			}
		}
		return // ID was provided but not found; don't fall back to name
	}
	for i := len(toolUses) - 1; i >= 0; i-- {
		if toolUses[i].Name == name && toolUses[i].Output == "" {
			toolUses[i].Output = output
			return
		}
	}
}

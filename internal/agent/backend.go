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

// kojoAPIBase is the URL agents use to reach kojo's auth-required API
// listener. Set by the server at startup via SetKojoAPIBase. Empty when
// the auth listener is not configured (e.g. --no-auth dev mode), in
// which case KOJO_API_BASE is omitted from the agent's environment.
var kojoAPIBase string

// SetKojoAPIBase records the URL agents should use for self-authenticated
// API calls. Idempotent; safe to call repeatedly during boot.
func SetKojoAPIBase(base string) { kojoAPIBase = base }

// agentTokenLookup, when set, returns the per-agent auth token kojo
// should expose via $KOJO_AGENT_TOKEN to the PTY. Wired up by the
// server using the auth.TokenStore.
var agentTokenLookup func(agentID string) (string, bool)

// SetAgentTokenLookup wires the token lookup callback. May be nil
// (disables token injection).
func SetAgentTokenLookup(fn func(string) (string, bool)) { agentTokenLookup = fn }

// peerCountLookup, when set, returns the number of OTHER peers
// (excluding the local self row) currently registered in
// peer_registry. Wired up by cmd/kojo/main.go after the peer
// registrar starts. Used by SyncDeviceSwitchSkill to suppress the
// kojo-switch-device skill on single-node installs (no target =
// no useful skill to expose).
//
// Unset (callback nil) is treated as "unknown peer count → assume
// zero" so a misconfigured boot doesn't fail open and spam the
// skill into a single-node setup.
var peerCountLookup func() int

// SetPeerCountLookup wires the peer-count callback. May be nil
// (disables the device-switch skill auto-install).
func SetPeerCountLookup(fn func() int) { peerCountLookup = fn }

// LookupPeerCount returns the number of OTHER registered peers via
// the wired callback. Returns 0 when no callback is set.
func LookupPeerCount() int {
	if peerCountLookup == nil {
		return 0
	}
	return peerCountLookup()
}

// LookupAgentToken returns the raw $KOJO_AGENT_TOKEN value for the
// given agent_id via the wired callback. Returns ("", false) when
// the callback is unset or the agent has no available raw value
// (e.g. post-restart on a peer that only holds the kv hash, not
// the raw — see auth.ErrTokenRawUnavailable). Used by the §3.7
// orchestrator to capture the source's raw so it can be replayed
// to target's TokenStore via /api/v1/peers/agent-sync.
func LookupAgentToken(agentID string) (string, bool) {
	if agentTokenLookup == nil {
		return "", false
	}
	return agentTokenLookup(agentID)
}

// filterEnv returns a copy of os.Environ() with entries matching any of the
// given prefixes removed, and AGENT_BROWSER_SESSION / AGENT_BROWSER_COOKIE_DIR
// vars set to agentID / dataDir. KOJO_AGENT_ID, KOJO_AGENT_TOKEN and
// KOJO_API_BASE are also injected so the agent can identify itself when
// curling kojo's API.
//
// Every KOJO_* var inherited from the parent process is stripped before the
// per-agent values are appended. This prevents an Owner-only secret like
// KOJO_OWNER_TOKEN — which a deployment may set on the kojo process to
// bootstrap the owner role — from ever leaking into a PTY where the agent
// could read it via $KOJO_OWNER_TOKEN.
func filterEnv(removePrefixes []string, agentID, dataDir string) []string {
	stripPrefixes := append([]string(nil), removePrefixes...)
	stripPrefixes = append(stripPrefixes, "KOJO_")
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		skip := false
		for _, prefix := range stripPrefixes {
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
		"KOJO_AGENT_ID="+agentID,
	)
	if agentTokenLookup != nil {
		if tok, ok := agentTokenLookup(agentID); ok && tok != "" {
			filtered = append(filtered, "KOJO_AGENT_TOKEN="+tok)
		}
	}
	if kojoAPIBase != "" {
		filtered = append(filtered, "KOJO_API_BASE="+kojoAPIBase)
	}
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

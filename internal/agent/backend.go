package agent

import (
	"context"
	"errors"
	"os"
	"strings"
)

// ctxSend delivers e on ch, returning false without blocking further if
// ctx is cancelled before the send completes. Backends wrap it in a local
// send closure and use the false return to stop stream parsing on cancel.
func ctxSend(ctx context.Context, ch chan<- ChatEvent, e ChatEvent) bool {
	select {
	case ch <- e:
		return true
	case <-ctx.Done():
		return false
	}
}

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
	// (cron, groupdm notification, Slack one-shot, etc.) rather than a
	// human-driven turn. Backends use this to disable the idle-window
	// guard on session resets: there is no interactive conversation to
	// preserve, so token conservation wins over continuity.
	AutomatedTrigger bool

	// SessionKey overrides the default agent-ID-based session identifier.
	// The key is hashed into a deterministic UUID so callers can pass any
	// stable string (e.g. "slack:<channel>:<thread>") to get an independent
	// Claude session JSONL. Used for per-Slack-thread session resumption
	// where each conversation thread maintains its own context, isolated
	// from the agent's WebUI session and from other threads.
	//
	// Empty string means "fall back to the agent-wide session ID" — i.e.
	// the existing single-session-per-agent behaviour. Backends that don't
	// honor SessionKey (see backendSupportsSessionKey) must treat any
	// non-empty value as if it were empty and degrade to OneShot=true at
	// the call site rather than silently leaking context across keys.
	SessionKey string

	// RecentMessagesContext is a short, bounded transcript excerpt the
	// backend may prepend when it has to start a fresh persistent session
	// instead of resuming an existing one. Claude uses this as a continuity
	// fallback for missing/empty/reset JSONL sessions; when --resume works,
	// it is intentionally ignored to avoid duplicating history.
	RecentMessagesContext string

	// SystemPromptExtra is appended verbatim to the systemPrompt argument
	// AFTER the backend's normal prompt assembly. Use it to inject
	// per-conversation context (e.g. Slack channel/thread info) without
	// disturbing the cached system-prompt prefix shared across turns
	// within the same session.
	//
	// Why a separate field instead of pre-concatenating into systemPrompt:
	// the volatile per-turn data must land at a stable offset so the
	// claude prompt cache treats the leading shared prompt as a cacheable
	// prefix. Mixing channel-context tokens into the middle of the prompt
	// would invalidate the cache for every Slack message.
	SystemPromptExtra string

	// OnSteerReady, if set, is invoked once by a backend that supports
	// mid-turn steering (currently only claude) as soon as its stdin pipe
	// is ready to accept a second user-message JSON line. The callback
	// receives a SteerFunc the caller can register (e.g. in a busy-turn
	// handle) and call later to inject text into the running turn.
	// Backends that don't support steering simply never call this.
	OnSteerReady func(SteerFunc)
}

// SteerFunc injects an additional user message into an in-flight turn.
// Returns an error if the turn has already finished (process exited /
// stdin closed).
type SteerFunc func(text string) error

// ErrSteerUnsupported is returned by Manager.Steer / Manager.SteerOneShot
// when the target turn is running on a backend that doesn't support
// mid-turn steering (only the claude backend does today).
var ErrSteerUnsupported = errors.New("steering is not supported for this backend")

// backendSupportsSessionKey reports whether the backend honors
// ChatOptions.SessionKey when building its CLI invocation. Backends that
// ignore SessionKey (grok, llama.cpp) cannot isolate a
// per-thread session from the agent's main session, so the manager drops
// SessionKey for them and degrades to OneShot=true — keeping the chat
// ephemeral rather than silently mixing thread contexts.
//
// Keep this list in sync with the backends that actually read
// opts.SessionKey when assembling their argv / config.
func backendSupportsSessionKey(b ChatBackend) bool {
	if b == nil {
		return false
	}
	switch b.Name() {
	case "claude", "custom", "codex":
		// custom delegates to ClaudeBackend.Chat, and codex stores a
		// deterministic per-key thread ref under agentDir/.codex/threads.
		return true
	default:
		return false
	}
}

// backendLoadsClaudeSkills reports whether the given Agent.Tool value
// belongs to a backend that loads `.claude/skills/<name>/SKILL.md`
// at session start. claude / custom obviously do. grok also does:
// its skill loader treats `.claude/skills/` as a Claude-Code-
// compatibility source (verified empirically via `grok inspect` from
// an agentDir that has the kojo-* skills installed — they list as
// `project` scope alongside the user-scoped `~/.grok/skills/` ones).
// codex has its own `.codex/skills/<name>/SKILL.md` loader, so it
// intentionally remains false here even though Kojo can install
// codex-native skills via Sync*SkillForTool dispatchers. llama.cpp
// has no skill loader.
//
// Used to gate SyncAttachSkill installation: writing the SKILL.md
// into an agentDir whose backend never reads it just leaves dead
// bytes on disk.
//
// NOTE: kojo-switch-device has a STRICTER gate — see
// backendSupportsDeviceSwitch. Loading the skill is necessary but
// not sufficient; the handoff orchestrator must also know how to
// migrate the backend's session state to the target peer.
func backendLoadsClaudeSkills(tool string) bool {
	switch tool {
	case "claude", "custom", "grok":
		return true
	default:
		return false
	}
}

// backendSupportsDeviceSwitch reports whether the device-switch
// handoff can carry the backend's session state to the target peer.
//
//   - claude / custom: switch_device_handler.go transfers the
//     ~/.claude/projects/<encoded-cwd>/<uuid>.jsonl session files
//     via ClaudeSessions on the peer-sync wire; backend_claude.go
//     on target resumes with `claude --continue`.
//
//   - grok: same orchestrator transfers `<agentDir>/.grok/session_id`
//     and every regular file under
//     $GROK_HOME/sessions/<encoded(absAgentDir)>/<uuid>/ via
//     GrokSession on the peer-sync wire (see
//     grok_session_transfer.go); backend_grok.go on target picks up
//     the resume pointer at the next chat and issues
//     `grok --resume <uuid>`.
//
//   - codex: switch_device_handler.go transfers the per-agent
//     `.codex/threads/*.json` refs, rollout JSONLs, and Codex state
//     sqlite rows via CodexSession; backend_codex.go resumes with
//     app-server `thread/resume`.
//
// llama.cpp has no session transfer wired up; it stays off.
//
// Gating the SKILL.md install (instead of, say, a runtime 4xx) keeps
// the failure mode obvious for unsupported tools: the skill simply
// doesn't appear in the backend's skill listing, so the agent never
// offers a switch it cannot fulfil.
func backendSupportsDeviceSwitch(tool string) bool {
	switch tool {
	case "claude", "custom", "grok", "codex":
		return true
	default:
		return false
	}
}

// ChatBackend abstracts a CLI tool for agent chat.
type ChatBackend interface {
	// Chat sends a message and returns a channel of streaming events.
	// The channel is closed when the response is complete.
	Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, error)

	// Name returns the tool identifier (e.g. "claude", "codex", "grok").
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
//
// The send is ctx-aware so a receiver that has already disengaged
// (channel buffer full + reader gone after ctx.Done) does not deadlock
// the backend goroutine and leak post-stream cleanup work like temp
// file removal.
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
	ev := ChatEvent{
		Type:         "done",
		Message:      msg,
		Usage:        usage,
		ErrorMessage: errMsg,
	}
	// Try a non-blocking send first so a live receiver with buffer
	// space always wins over the (already-signaled) ctx.Done case
	// — select with two ready cases would otherwise pick randomly
	// and drop the event for an active consumer.
	select {
	case ch <- ev:
		return
	default:
	}
	select {
	case ch <- ev:
	case <-ctx.Done():
		// Receiver already gave up and the buffer stayed full.
		// Drop the partial done event rather than block forever —
		// the goroutine still needs to close(ch) and run deferred
		// cleanup.
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

package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/loppo-llc/kojo/internal/sandbox"
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

// agentTokenLookup, when set, returns the per-agent auth token for
// direct injection into the system prompt and MCP config. The token is
// NOT exposed as an environment variable — see filterEnv doc. Wired up
// by the server using the auth.TokenStore.
var agentTokenLookup func(agentID string) (string, bool)

// SetAgentTokenLookup wires the token lookup callback. May be nil
// (disables token injection).
func SetAgentTokenLookup(fn func(string) (string, bool)) { agentTokenLookup = fn }

// filterEnv returns a copy of os.Environ() with entries matching any of the
// given prefixes removed, and AGENT_BROWSER_SESSION / AGENT_BROWSER_COOKIE_DIR
// vars set to agentID / dataDir. KOJO_AGENT_ID and KOJO_API_BASE are injected
// so the agent can identify itself.
//
// KOJO_AGENT_TOKEN is intentionally NOT set as an environment variable.
// Agent tokens are injected directly into the system prompt to avoid
// exposure via /proc/{pid}/environ, which any process running as the same
// UID can read.
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

// sharedHomeToolDirs lists per-user CLI config dirs that every agent on
// the host shares because all agents run as the same OS user. They are
// included in the sandbox allowlist so the underlying CLI (claude / codex
// / gemini) can read auth tokens and write session state — without them
// the tools refuse to start.
//
// Known limitation: any agent can modify these dirs and influence every
// other agent that uses the same tool (e.g. by writing a hook into
// ~/.claude/settings.json). True per-agent isolation requires redirecting
// each tool to a per-agent config dir via env vars, which depends on the
// per-agent CLI auth work tracked separately. Until that lands,
// sandboxConfig emits a warning the first time it allows these paths so
// the limitation is visible in operator logs rather than buried in the
// PR description.
var sharedHomeToolDirs = []string{
	".cache",  // npm/pip/go caches — typically benign but still shared
	".claude", // Claude Code sessions & config
	".gemini", // Gemini CLI config
	".codex",  // Codex CLI config
}

// sandboxConfig builds a sandbox.Config for the given agent.  The config
// allows writes to the agent's own data directory plus common paths that
// CLI tools need (temp dirs, caches, tool-specific config dirs).
//
// When Landlock is unavailable on the host (or the kernel is below the
// required ABI), sandboxConfig logs a warning so operators know agent
// processes are running unrestricted — silently degrading would hide a
// material security posture change.
func sandboxConfig(agent *Agent, logger *slog.Logger) sandbox.Config {
	if !sandbox.Available() {
		// Surface degraded state. The original PR description promised a
		// "warning, no restriction" path here; without this log, agents
		// would silently run unsandboxed on macOS, in CI, or on Linux
		// kernels below the required Landlock ABI.
		logger.Warn("filesystem sandbox unavailable — agent processes will run UNRESTRICTED. "+
			"Cross-agent FS isolation requires Linux 6.2+ with Landlock ABI v3 enabled.",
			"agent", agent.ID,
		)
		return sandbox.Config{Enabled: false}
	}

	dir := agentDir(agent.ID)
	home, _ := os.UserHomeDir()

	rw := []string{
		dir,          // agent's own data
		os.TempDir(), // /tmp
		"/var/tmp",   // persistent temp
	}

	if home != "" {
		for _, sub := range sharedHomeToolDirs {
			rw = append(rw, filepath.Join(home, sub))
		}
		logger.Warn("sandbox allows shared per-user CLI dirs — "+
			"these are NOT isolated between agents on the same host. "+
			"An agent writing to ~/.claude (etc.) can influence every other "+
			"agent that uses the same tool. Per-agent isolation depends on "+
			"future per-agent CLI auth work.",
			"agent", agent.ID,
			"shared_dirs", sharedHomeToolDirs,
		)
	}

	// Custom work directory (e.g. a project checkout). The WorkDir must
	// not point inside the agents-root, because that would let an agent
	// elevate its sandbox to cover another agent's data dir simply by
	// PATCHing its own WorkDir. agent.go and manager.go validate this on
	// the write path; the check here is defense-in-depth in case a stale
	// or hand-edited on-disk config slipped past validation.
	if agent.WorkDir != "" && agent.WorkDir != dir {
		if isWithinAgentsRoot(agent.WorkDir) {
			logger.Error("refusing to add WorkDir to sandbox: path is inside agents root — "+
				"this would let the agent write into other agents' data dirs",
				"agent", agent.ID,
				"workDir", agent.WorkDir,
				"agentsRoot", agentsDir(),
			)
		} else {
			rw = append(rw, agent.WorkDir)
		}
	}

	// Pre-create kojo-owned RW paths so Landlock's path-beneath rules can
	// open them. Once Landlock enforces, the sandboxed CLI can only create
	// a dir if an ancestor is already on the allowlist — and home-relative
	// tool dirs like ~/.claude have no such ancestor. mkdir is idempotent
	// and uses 0o700 (caller-only access) to match the privacy expectations
	// of tool config dirs.
	for _, p := range rw {
		if p == "" {
			continue
		}
		if err := os.MkdirAll(p, 0o700); err != nil {
			logger.Warn("failed to pre-create sandbox RW path; sandbox will skip it and the agent may fail to write there",
				"agent", agent.ID,
				"path", p,
				"err", err,
			)
		}
	}

	logger.Info("sandbox enabled",
		"agent", agent.ID,
		"rw_paths", rw,
	)
	return sandbox.Config{
		RWPaths: rw,
		Enabled: true,
	}
}

// isWithinAgentsRoot reports whether path is the agents-root directory
// itself, a parent of it, or a path located under it. Symlinks are
// resolved before comparison so an agent cannot use a symlink as a
// WorkDir to bypass the check.
//
// We treat all three forms ("equal", "ancestor", "descendant") as inside
// the agents root, because each grants the agent write access to another
// agent's data directory in different ways. Equal and ancestor cover the
// whole root in one path-beneath rule; descendant points directly into
// another agent's dir.
func isWithinAgentsRoot(path string) bool {
	if path == "" {
		return false
	}
	root := agentsDir()
	rootResolved := resolvePath(root)
	pathResolved := resolvePath(path)
	// Path is the root itself or an ancestor that contains the root.
	if pathResolved == rootResolved {
		return true
	}
	rel, err := filepath.Rel(rootResolved, pathResolved)
	if err != nil {
		return false
	}
	// If Rel produces a result that does not start with "..", path is
	// inside root. ".." prefix means path is a sibling / ancestor /
	// elsewhere — and an ancestor would let the agent encompass root, so
	// we additionally check for the inverse: root inside path.
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	if relInv, err := filepath.Rel(pathResolved, rootResolved); err == nil {
		if relInv == "." || (relInv != ".." && !strings.HasPrefix(relInv, ".."+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

// resolvePath returns the absolute, symlink-resolved form of path. When
// the path doesn't exist yet (Stat would fail before EvalSymlinks
// resolves), we fall back to filepath.Clean(Abs(path)) so the check still
// gives a deterministic comparison. Used by isWithinAgentsRoot to defeat
// symlink-based attempts to escape WorkDir validation.
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return filepath.Clean(abs)
	}
	return resolved
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

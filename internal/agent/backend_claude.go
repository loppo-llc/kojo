package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ClaudeBackend implements ChatBackend using the Claude CLI with stream-json output.
type ClaudeBackend struct {
	logger   *slog.Logger
	proxyURL string // if set, injected as ANTHROPIC_BASE_URL
}

func NewClaudeBackend(logger *slog.Logger) *ClaudeBackend {
	return &ClaudeBackend{logger: logger}
}

// SetProxyURL configures an ANTHROPIC_BASE_URL to inject into Claude CLI env.
func (b *ClaudeBackend) SetProxyURL(url string) {
	b.proxyURL = url
}

func (b *ClaudeBackend) Name() string { return "claude" }

func (b *ClaudeBackend) Available() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

func (b *ClaudeBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, error) {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("claude not found in PATH")
	}

	dir := agentDir(agent.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create agent dir: %w", err)
	}

	args := b.buildClaudeArgs(agent, systemPrompt, dir, opts.OneShot, opts.MCPServers, opts.AutomatedTrigger)

	cmd := exec.CommandContext(ctx, claudePath, args...)
	cmd.Env = filterEnv([]string{"CLAUDE_CODE", "CLAUDECODE", "AGENT_BROWSER_SESSION", "AGENT_BROWSER_COOKIE_DIR"}, agent.ID, dir)
	// Token conservation: agents persist state in files (MEMORY.md, memory/),
	// not in Claude's conversation history. 1M context only inflates
	// cache_read/cache_creation across runs without adding real value, and its
	// write cost is 2x input (vs 1.25x for 5m cache). Force 200k and rely on
	// kojo's own session-reset logic (sessionFileUsable) for history pruning.
	// Auto-compact stays enabled as a late safety net but threshold is
	// tightened so that if reset misses, claude compacts before pricing spikes.
	cmd.Env = append(cmd.Env,
		"CLAUDE_CODE_DISABLE_1M_CONTEXT=1",
		"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=85",
	)
	if b.proxyURL != "" {
		cmd.Env = append(cmd.Env, "ANTHROPIC_BASE_URL="+b.proxyURL)
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			cmd.Env = append(cmd.Env, "ANTHROPIC_API_KEY=dummy")
		}
	}
	cmd.Dir = dir
	// Send SIGTERM on context cancellation, then SIGKILL after 10s grace period.
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second

	// Pass user message via stdin to avoid option injection when the message
	// starts with "-" (which would be misinterpreted as a CLI flag).
	cmd.Stdin = strings.NewReader(userMessage)

	// Capture stderr for error diagnostics (limit to 4KB to prevent memory issues)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderrBuf, limit: 4096}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}

	ch := make(chan ChatEvent, 64)

	go func() {
		defer close(ch)

		// send is a helper that respects context cancellation to avoid goroutine leaks.
		send := func(e ChatEvent) bool {
			select {
			case ch <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}

		result := parseClaudeStream(stdout, b.logger, send)

		// If stream was cancelled (send returned false), clean up process
		// and emit a partial done event so the transcript is persisted.
		if result.cancelled {
			cmd.Wait()
			content := result.fullText
			if content == "" {
				content = result.lastAssistantText
			}
			emitCancelDone(ctx, ch, content, result.thinking, result.toolUses, result.usage)
			return
		}

		// Check process exit status
		var processError string
		if err := cmd.Wait(); err != nil {
			b.logger.Warn("claude process exited with error", "err", err, "stderr", stderrBuf.String())
			processError = strings.TrimSpace(stderrBuf.String())
			if processError == "" {
				processError = err.Error()
			}
			if result.fullText == "" && result.lastAssistantText == "" && len(result.toolUses) == 0 {
				send(ChatEvent{Type: "error", ErrorMessage: processError})
				return
			}
		}

		// Determine final text with fallback chain
		finalText := result.fullText

		// Fallback: text extracted from assistant event content blocks
		if finalText == "" && result.lastAssistantText != "" {
			finalText = result.lastAssistantText
			b.logger.Info("used assistant event text as fallback", "agent", agent.ID, "len", len(finalText))
		}

		// Last resort: recover from Claude session JSONL when the stream
		// produced no usable text. Only used as fallback, never overrides
		// text that was successfully captured from the stream.
		if finalText == "" {
			if sessionText := recoverFromSession(agent.ID, result.streamSessionID, b.logger); sessionText != "" {
				b.logger.Info("recovered text from session log",
					"agent", agent.ID,
					"sessionLen", len(sessionText))
				finalText = sessionText
			}
		}

		// Send recovered text if nothing was streamed to client
		if finalText != "" && result.fullText == "" {
			send(ChatEvent{Type: "text", Delta: finalText})
		}

		msg := newAssistantMessage()
		msg.Content = finalText
		msg.Thinking = result.thinking
		msg.ToolUses = result.toolUses
		msg.Usage = result.usage

		// Cache-hit telemetry. Logged on every turn so we can see whether
		// the system-prompt / volatile-context split is actually keeping
		// the prompt cache warm. A high cacheCreation:cacheRead ratio
		// across consecutive turns means the cache prefix is being
		// invalidated and we're paying full input cost each turn.
		if u := result.usage; u != nil {
			b.logger.Info("claude usage",
				"agent", agent.ID,
				"input", u.InputTokens,
				"output", u.OutputTokens,
				"cacheRead", u.CacheReadInputTokens,
				"cacheCreation", u.CacheCreationInputTokens,
			)
		}

		send(ChatEvent{Type: "done", Message: msg, Usage: result.usage, ErrorMessage: processError})
	}()

	return ch, nil
}

// buildClaudeArgs constructs the CLI arguments for a Claude chat invocation.
func (b *ClaudeBackend) buildClaudeArgs(agent *Agent, systemPrompt string, dir string, oneShot bool, mcpServers map[string]mcpServerEntry, automatedTrigger bool) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--dangerously-skip-permissions",
		// Recent claude-code versions added an "auto" permission classifier
		// that can still prompt for Edit/Write despite --dangerously-skip-permissions.
		// Setting permission-mode explicitly forces bypass regardless of the
		// default mode the CLI resolves to.
		"--permission-mode", "bypassPermissions",
		// Scheduling tools don't make sense in kojo's non-interactive agent
		// context — the scheduled job would fire against the user's claude,
		// not this agent's session.
		"--disallowedTools", "CronCreate,CronDelete,CronList,ScheduleWakeup",
	}

	if systemPrompt != "" {
		args = append(args, "--system-prompt", systemPrompt)
	}
	if agent.Model != "" {
		args = append(args, "--model", agent.Model)
	}
	if agent.Effort != "" {
		args = append(args, "--effort", agent.Effort)
	}
	if len(agent.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(agent.AllowedTools, ","))
	}

	// Inject MCP servers via --mcp-config inline JSON (session-scoped, no files).
	if len(mcpServers) > 0 {
		if cfg, err := mcpConfigJSON(mcpServers); err == nil {
			args = append(args, "--mcp-config", cfg)
		} else {
			b.logger.Warn("failed to marshal MCP config for Claude", "err", err)
		}
	}

	// Remove CLAUDE.local.md to prevent persona autoload hook from
	// overriding --system-prompt.
	if err := os.Remove(filepath.Join(dir, "CLAUDE.local.md")); err != nil && !os.IsNotExist(err) {
		b.logger.Warn("failed to remove CLAUDE.local.md from agent dir", "dir", dir, "err", err)
	}

	// Use --resume to append to the same persistent session, or --session-id
	// to create the first one. --continue creates a new session file each
	// time, causing cron check-ins and user messages to branch into parallel
	// sessions — then the next --continue picks whichever branch was most
	// recent, losing the other's context.
	//
	// OneShot mode (e.g. Slack conversations) skips session resumption entirely,
	// running a fresh ephemeral session each time.
	if !oneShot {
		sessionID := agentIDToUUID(agent.ID)
		if sessionFileUsable(dir, sessionID, automatedTrigger, agent.ID, agent.ResumeIdleDuration(), b.logger) {
			args = append(args, "--resume", sessionID)
		} else {
			args = append(args, "--session-id", sessionID)
		}
	}

	return args
}

// streamParseResult holds the accumulated state from parsing a Claude stream.
type streamParseResult struct {
	fullText          string
	thinking          string
	lastAssistantText string
	streamSessionID   string
	toolUses          []ToolUse
	usage             *Usage
	cancelled         bool // true if send returned false (context cancelled)
}

// parseClaudeStream reads Claude's stream-json output from r and emits ChatEvents
// via the send callback. Returns the accumulated parse result.
// If send returns false (channel full / context cancelled), parsing stops immediately.
func parseClaudeStream(r io.Reader, logger *slog.Logger, send func(ChatEvent) bool) *streamParseResult {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	res := &streamParseResult{}
	var fullText strings.Builder
	var thinking strings.Builder
	var toolUses []ToolUse

	// Tool use tracking for content_block_start/delta/stop flow
	var currentToolName string
	var currentToolID string
	var currentToolInput strings.Builder
	toolIDToName := make(map[string]string)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var event claudeStreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			logger.Debug("failed to parse claude stream event", "err", err)
			continue
		}

		// Unwrap stream_event wrapper emitted by --include-partial-messages.
		if event.Type == "stream_event" && len(event.Event) > 0 {
			var inner claudeStreamEvent
			if err := json.Unmarshal(event.Event, &inner); err != nil {
				logger.Debug("failed to parse inner stream event", "err", err)
				continue
			}
			if inner.Type == "" {
				continue
			}
			event = inner
		}

		switch event.Type {
		case "system":
			status := "thinking"
			if event.Subtype == "compact_boundary" {
				status = "compacting"
			}
			if !send(ChatEvent{Type: "status", Status: status}) {
				res.cancelled = true
				return res
			}

		case "assistant":
			var atext strings.Builder
			for _, block := range event.Message.Content {
				switch block.Type {
				case "text":
					if block.Text != "" {
						atext.WriteString(block.Text)
					}
				case "thinking":
					if block.Thinking != "" && thinking.Len() == 0 {
						thinking.WriteString(block.Thinking)
					}
				}
			}
			if atext.Len() > 0 {
				res.lastAssistantText = atext.String()
			}

			if event.Message.StopReason != "" {
				// Record usage whenever the assistant turn produced any
				// metric, not just output_tokens. Cache fields are the
				// signal we care about for diagnosing prompt-cache
				// behaviour, and they can be non-zero even on stop
				// reasons that yield no output text (e.g. tool-only
				// turns or refusals truncated by max_tokens).
				u := event.Message.Usage
				if u.InputTokens > 0 || u.OutputTokens > 0 ||
					u.CacheReadInputTokens > 0 || u.CacheCreationInputTokens > 0 {
					res.usage = &Usage{
						InputTokens:              u.InputTokens,
						OutputTokens:             u.OutputTokens,
						CacheReadInputTokens:     u.CacheReadInputTokens,
						CacheCreationInputTokens: u.CacheCreationInputTokens,
					}
				}
			}

		case "content_block_start":
			if event.ContentBlock.Type == "tool_use" {
				currentToolName = event.ContentBlock.Name
				currentToolID = event.ContentBlock.ID
				currentToolInput.Reset()
				toolIDToName[currentToolID] = currentToolName
			}

		case "content_block_delta":
			switch event.Delta.Type {
			case "text_delta":
				if event.Delta.Text != "" {
					fullText.WriteString(event.Delta.Text)
					if !send(ChatEvent{Type: "text", Delta: event.Delta.Text}) {
						res.cancelled = true
						return res
					}
				}
			case "thinking_delta":
				if event.Delta.Thinking != "" {
					thinking.WriteString(event.Delta.Thinking)
					if !send(ChatEvent{Type: "thinking", Delta: event.Delta.Thinking}) {
						res.cancelled = true
						return res
					}
				}
			case "input_json_delta":
				currentToolInput.WriteString(event.Delta.PartialJSON)
			}

		case "content_block_stop":
			if currentToolName != "" {
				input := currentToolInput.String()
				tu := ToolUse{
					ID:    currentToolID,
					Name:  currentToolName,
					Input: input,
				}
				toolUses = append(toolUses, tu)
				if !send(ChatEvent{Type: "tool_use", ToolUseID: currentToolID, ToolName: currentToolName, ToolInput: input}) {
					res.cancelled = true
					return res
				}
				currentToolName = ""
				currentToolID = ""
				currentToolInput.Reset()
			}

		case "user":
			for _, block := range event.Message.Content {
				if block.Type == "tool_result" && block.ToolUseID != "" {
					toolName := toolIDToName[block.ToolUseID]
					if toolName != "" {
						output := block.contentText()
						if !send(ChatEvent{Type: "tool_result", ToolUseID: block.ToolUseID, ToolName: toolName, ToolOutput: output}) {
							res.cancelled = true
							return res
						}
						matchToolOutput(toolUses, block.ToolUseID, toolName, output)
					}
				}
			}

		case "result":
			if event.SessionID != "" {
				res.streamSessionID = event.SessionID
			}
			if event.Result != "" {
				if fullText.Len() == 0 {
					fullText.WriteString(event.Result)
					if !send(ChatEvent{Type: "text", Delta: event.Result}) {
						res.cancelled = true
						return res
					}
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		logger.Warn("claude stream scanner error", "err", err)
	}

	res.fullText = fullText.String()
	res.thinking = thinking.String()
	res.toolUses = toolUses
	return res
}

// Claude stream-json event types
type claudeStreamEvent struct {
	Type string `json:"type"`

	// "stream_event" wrapper (--include-partial-messages)
	Event json.RawMessage `json:"event,omitempty"`

	// "system" event
	Subtype string `json:"subtype,omitempty"`

	// "assistant" event
	Message struct {
		StopReason string               `json:"stop_reason,omitempty"`
		Content    []claudeContentBlock `json:"content,omitempty"`
		Usage      struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage,omitempty"`
	} `json:"message,omitempty"`

	// "content_block_start" event
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id,omitempty"`
		Name string `json:"name,omitempty"`
	} `json:"content_block,omitempty"`

	// "content_block_delta" event
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text,omitempty"`
		Thinking    string `json:"thinking,omitempty"`
		PartialJSON string `json:"partial_json,omitempty"`
	} `json:"delta,omitempty"`

	// "result" event
	Result    string `json:"result,omitempty"`
	Duration  int    `json:"duration_ms,omitempty"`
	SessionID string `json:"session_id,omitempty"`

	// "tool_use" / "tool_result" events
	Name    string `json:"name,omitempty"`
	Input   string `json:"input,omitempty"`
	Content string `json:"content,omitempty"`
}

// claudeContentBlock represents a content block in a Claude message.
type claudeContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`    // thinking block
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result
	Content   json.RawMessage `json:"content,omitempty"`     // tool_result output (string or array)
}

// contentText extracts a plain-text representation from a claudeContentBlock's Content field.
// Content may be a JSON string or an array of content blocks with "text" entries.
func (b *claudeContentBlock) contentText() string {
	if len(b.Content) == 0 {
		return ""
	}
	// Try as plain string first
	var s string
	if json.Unmarshal(b.Content, &s) == nil {
		return s
	}
	// Try as array of content blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(b.Content, &blocks) == nil {
		var sb strings.Builder
		for _, bl := range blocks {
			if bl.Type == "text" && bl.Text != "" {
				sb.WriteString(bl.Text)
			}
		}
		return sb.String()
	}
	// Fallback: raw string
	return string(b.Content)
}

// limitedWriter wraps a bytes.Buffer and stops writing after limit bytes.
type limitedWriter struct {
	w     *bytes.Buffer
	limit int
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	remaining := lw.limit - lw.w.Len()
	if remaining <= 0 {
		return len(p), nil // discard silently
	}
	toWrite := p
	if len(toWrite) > remaining {
		toWrite = toWrite[:remaining]
	}
	lw.w.Write(toWrite)
	// Always report full len(p) to avoid io.ErrShortWrite from callers.
	return len(p), nil
}

// claudeEncodePath encodes a directory path using Claude's project path scheme:
// "/" (or separator), ".", "_" are all replaced with "-".
func claudeEncodePath(dir string) string {
	return strings.NewReplacer(
		string(filepath.Separator), "-",
		".", "-",
		"_", "-",
	).Replace(dir)
}

// claudeProjectDir returns the Claude project directory for the given absolute path.
func claudeProjectDir(absDir string) string {
	return filepath.Join(claudeConfigDir(), "projects", claudeEncodePath(absDir))
}

// claudeConfigDir returns the Claude configuration root, respecting
// CLAUDE_CONFIG_DIR if set, otherwise falling back to ~/.claude.
func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".claude")
}

// hasExistingSession checks whether a Claude session JSONL file already exists
// for the given agent working directory by looking at Claude's project data.
func hasExistingSession(agentDir string) bool {
	return hasSessionFile(agentDir, "")
}

// hasSessionFile checks whether a specific session JSONL file exists.
// If sessionID is empty, it returns true if any session file exists.
func hasSessionFile(agentDir string, sessionID string) bool {
	absDir, err := filepath.Abs(agentDir)
	if err != nil {
		return false
	}
	projectDir := claudeProjectDir(absDir)
	if sessionID != "" {
		_, err := os.Stat(filepath.Join(projectDir, sessionID+".jsonl"))
		return err == nil
	}
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			return true
		}
	}
	return false
}

// sessionResetThresholdTokens is the cumulative context size (input +
// cache_read + cache_creation) at which kojo abandons --resume and starts a
// fresh claude session. Chosen below claude's 200k model limit and below the
// CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=85 threshold (~170k) so that kojo's reset
// fires first and claude's compaction stays a safety net.
//
// Why reset instead of compact: kojo persists long-term state in agent files
// (MEMORY.md, memory/, data/), so the conversation JSONL is just an ephemeral
// work log. /compact would spend output tokens to summarize history that will
// be re-derived from files on the next turn anyway — pure waste for
// interval-driven agents. Resetting is cheaper and keeps context tight.
const sessionResetThresholdTokens = 150_000

// sessionTailReadBytes caps how much of the session JSONL is read when
// measuring the last recorded context size. Chosen to fit at least one full
// assistant turn (tool_result payloads can reach several hundred KiB) so the
// latest usage record is within the window even for multi-hundred-MB session
// logs. When the last record happens to straddle the window boundary and
// gets discarded as a leading partial line, lastSessionContextTokens marks
// the read as untrusted so the reset retries on the next chat.
const sessionTailReadBytes = 1 * 1024 * 1024

// preResetSummarize is the summarization hook invoked by sessionFileUsable
// right before it deletes a session file. Defaults to PreCompactSummarize,
// which writes a diary entry from the live session JSONL. Exposed as a
// package variable so tests can substitute a deterministic fake instead of
// spawning a real claude CLI process.
var preResetSummarize = func(agentID, tool string, logger *slog.Logger) error {
	// Reset path doesn't have the PreCompact-hook stdin payload (claude
	// isn't telling us about a compaction here — kojo is initiating a
	// session wipe), so transcriptPath is left empty and the function
	// falls back to discovery.
	return PreCompactSummarize(agentID, tool, "", logger)
}

// sessionResetMinIdleDuration is the package-default idle window used when
// a caller doesn't supply a per-agent override (legacy callers and tests).
// Per-agent override comes through Agent.ResumeIdleMinutes →
// Agent.ResumeIdleDuration().
//
// Rationale for the 5-minute default: an active chat produces many short
// turns that never reach the autosummary / MEMORY.md path, so resetting
// mid-conversation would discard recent context the agent can't recover
// from files. The interval-driven agents we're actually trying to curb
// fire every 10+ minutes and always fall well outside this window, so the
// gate only protects interactive users. 5 minutes also matches Anthropic's
// default prompt cache TTL, keeping cache warm across back-to-back turns.
// If an active chat genuinely blows past the token threshold, claude's
// auto-compact (CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=85) still fires as a
// safety net.
const sessionResetMinIdleDuration = defaultResumeIdleDuration

// sessionFileUsable checks whether the deterministic session file exists and
// is minimally valid. Returns false (and removes the file) when the file is
// empty or the last recorded usage exceeds sessionResetThresholdTokens —
// forcing the caller to start a fresh session with --session-id instead of
// --resume.
//
// When the usage cannot be read reliably (e.g. the claude process is mid-write
// or the scanner tripped on an oversized line), we conservatively return true
// without touching the file: resetting on uncertainty would race with a live
// writer and could discard a healthy session. The check will run again on the
// next chat.
//
// automatedTrigger disables the interactive-chat idle guard. Pass true when
// the caller is a non-interactive trigger (cron fire, groupdm notification,
// notify poller) — there is no human conversation to protect and the guard
// would otherwise prevent resets on agents whose interval is shorter than
// the idle window.
//
// idleThreshold is the per-agent active-chat protection window. Pass
// agent.ResumeIdleDuration(); pass <=0 to fall back to sessionResetMinIdleDuration.
//
// agentID and logger are used to summarize the session into the agent's
// diary (via PreCompactSummarize) just before a reset fires. This is kojo's
// system-level guarantee that recent turns aren't lost simply because the
// agent forgot to update MEMORY.md itself — production experience has shown
// that persona / system-prompt instructions to "write to MEMORY.md" are not
// reliable, so the reset path writes a summary regardless.
func sessionFileUsable(agentDir string, sessionID string, automatedTrigger bool, agentID string, idleThreshold time.Duration, logger *slog.Logger) bool {
	absDir, err := filepath.Abs(agentDir)
	if err != nil {
		return false
	}
	path := filepath.Join(claudeProjectDir(absDir), sessionID+".jsonl")
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.Size() == 0 {
		if err := os.Remove(path); err != nil {
			// Same rationale as the threshold branch below — a file we
			// can't delete but also can't --resume (empty) is best left
			// alone; next chat will retry.
			slog.Warn("empty session remove failed, keeping as --resume fallback",
				"path", path, "err", err)
			return true
		}
		return false
	}
	ctx, ok := lastSessionContextTokens(path)
	if !ok {
		// Couldn't trust the tail read — keep the session for now and
		// re-evaluate on the next chat.
		return true
	}
	if ctx <= sessionResetThresholdTokens {
		return true
	}
	if !automatedTrigger {
		threshold := idleThreshold
		if threshold <= 0 {
			threshold = sessionResetMinIdleDuration
		}
		if idle := time.Since(info.ModTime()); idle < threshold {
			slog.Debug("claude session over threshold but recently active, keeping",
				"path", path, "contextTokens", ctx,
				"idle", idle, "idleThreshold", threshold)
			return true
		}
	}

	// Summarize the session into the agent's diary BEFORE we delete its
	// JSONL. PreCompactSummarize reads the live session file (which we're
	// about to remove), so ordering matters. If the summary fails we
	// abort the reset — losing context silently would be worse than
	// carrying a slightly-over-threshold session for one more turn.
	if agentID != "" {
		if err := preResetSummarize(agentID, "claude", logger); err != nil {
			slog.Warn("pre-reset summary failed, keeping session to avoid context loss",
				"path", path, "agent", agentID, "err", err)
			return true
		}
	}

	slog.Info("claude session context over threshold, resetting",
		"path", path, "contextTokens", ctx,
		"threshold", sessionResetThresholdTokens,
		"automatedTrigger", automatedTrigger)
	if err := os.Remove(path); err != nil {
		// Couldn't delete the session file — don't lie to the caller by
		// returning false. With our deterministic session ID, a subsequent
		// --session-id <id> invocation would either resurrect the existing
		// session or fail, neither of which is what we want. Keep using
		// --resume; the next run will retry the reset.
		slog.Warn("session reset: remove failed, keeping session",
			"path", path, "err", err)
		return true
	}
	return false
}

// lastSessionContextTokens returns (contextTokens, trusted). contextTokens is
// the approximate claude context size (input + cache_read + cache_creation)
// taken from the most recent usage record in the session JSONL. trusted is
// false when the read cannot be relied upon (missing file, seek error,
// oversized line, concurrent mid-write). Callers should treat untrusted reads
// as "skip reset this round" — never as "reset immediately" — to avoid racing
// with a live claude writer.
//
// Only the tail of the file (sessionTailReadBytes) is scanned; session logs
// for long-lived agents routinely reach tens to hundreds of MiB and the last
// usage record is always near the end, so a full-file scan on every Chat()
// call would add non-trivial latency for no benefit.
func lastSessionContextTokens(sessionPath string) (int, bool) {
	f, err := os.Open(sessionPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return 0, false
	}

	var startOffset int64
	if info.Size() > sessionTailReadBytes {
		startOffset = info.Size() - sessionTailReadBytes
	}
	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		return 0, false
	}

	scanner := bufio.NewScanner(f)
	// 1 MiB is the largest single JSONL line we've observed in practice;
	// anything larger gets reported via scanner.Err() and we mark the read
	// as untrusted.
	scanner.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)

	// If we seeked into the middle of the file, the first partial line is
	// unparseable — drop it before accumulating.
	if startOffset > 0 {
		scanner.Scan()
	}

	last := 0
	for scanner.Scan() {
		var entry struct {
			Message struct {
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		u := entry.Message.Usage
		ctx := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		if ctx > 0 {
			last = ctx
		}
	}
	if err := scanner.Err(); err != nil {
		// Oversized line or a transient read error (possibly mid-write from
		// a live claude process). Refuse to make a decision.
		return 0, false
	}
	// Tail read guarantees a conclusive result only when we either scanned
	// from the start of the file or found at least one usage record in the
	// tail window. Otherwise the single line that straddled the window start
	// may have carried the most recent usage — we can't tell, so leave the
	// decision to the next run rather than return a falsely low (0, true).
	if last == 0 && startOffset > 0 {
		return 0, false
	}
	return last, true
}

// agentIDToUUID converts an agent ID (e.g. "ag_8cf247118ad856e8") to a
// deterministic UUID v3 string that claude CLI accepts as --session-id.
func agentIDToUUID(agentID string) string {
	h := md5.Sum([]byte(agentID))
	h[6] = (h[6] & 0x0f) | 0x30 // version 3
	h[8] = (h[8] & 0x3f) | 0x80 // variant RFC4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// recoverFromSession reads the Claude session JSONL for the agent and
// returns the text that Claude actually generated for the last user message.
// If sessionID is non-empty, the matching session file is used; otherwise
// the most recently modified session file is selected as a fallback.
func recoverFromSession(agentID string, sessionID string, logger *slog.Logger) string {
	dir := agentDir(agentID)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}

	projectDir := claudeProjectDir(absDir)

	sessionFile := findSessionFile(projectDir, sessionID)
	if sessionFile == "" {
		return ""
	}

	f, err := os.Open(sessionFile)
	if err != nil {
		return ""
	}
	defer f.Close()

	// Walk session entries, keeping only the assistant text that appears
	// after the last real user message (tool_result entries are skipped).
	// Instead of storing all entries, we just reset on real user messages
	// and accumulate assistant text.
	var texts []string
	foundUser := false

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	for scanner.Scan() {
		var raw struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &raw) != nil {
			continue
		}

		switch raw.Type {
		case "assistant":
			var msg struct {
				Content []claudeContentBlock `json:"content"`
			}
			if json.Unmarshal(raw.Message, &msg) != nil {
				continue
			}
			var text strings.Builder
			for _, block := range msg.Content {
				if block.Type == "text" && block.Text != "" {
					text.WriteString(block.Text)
				}
			}
			if text.Len() > 0 {
				texts = append(texts, text.String())
			}

		case "user":
			if isRealUserEntry(raw.Message) {
				// New user turn — reset collected assistant text.
				texts = nil
				foundUser = true
			}
		}
	}

	// If the scanner hit an error (e.g. oversized line), discard
	// partial results to avoid returning truncated/stale text.
	if scanner.Err() != nil {
		logger.Warn("session JSONL scanner error", "err", scanner.Err())
		return ""
	}

	if !foundUser {
		return ""
	}

	return strings.Join(texts, "")
}

// findSessionFile locates the Claude session JSONL in projectDir.
// When sessionID is provided, it looks for "<sessionID>.jsonl" only.
// When sessionID is empty, falls back to the most recently modified .jsonl file.
func findSessionFile(projectDir string, sessionID string) string {
	if sessionID != "" {
		path := filepath.Join(projectDir, sessionID+".jsonl")
		if _, err := os.Stat(path); err == nil {
			return path
		}
		return ""
	}

	// Fallback for callers that don't have a session ID (e.g. loadSessionMessages).
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return ""
	}
	var best string
	var bestMod time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(bestMod) {
			bestMod = info.ModTime()
			best = filepath.Join(projectDir, e.Name())
		}
	}
	return best
}

// clearClaudeSession removes Claude session JSONL files from the global
// config store for the given agent, forcing the next chat to start fresh.
func clearClaudeSession(agentID string) {
	dir := agentDir(agentID)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		slog.Warn("clearClaudeSession: Abs failed", "agent", agentID, "err", err)
		return
	}
	projectDir := claudeProjectDir(absDir)
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		slog.Info("clearClaudeSession: no project dir", "agent", agentID, "dir", projectDir)
		return
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			path := filepath.Join(projectDir, e.Name())
			if err := os.Remove(path); err != nil {
				slog.Warn("clearClaudeSession: remove failed", "path", path, "err", err)
			} else {
				slog.Info("clearClaudeSession: removed", "path", path)
			}
		}
	}
}

// allowedProtectedPaths is the set of protected directory names for which
// per-agent permission bypass may be granted. Claude guards these dirs even
// under bypassPermissions, so explicit permissions.allow rules are the only
// way to suppress prompts for headless agents.
var allowedProtectedPaths = map[string]bool{
	"claude": true,
	"git":    true,
	"husky":  true,
}

// normalizeAllowProtectedPaths filters a slice to the known-valid set and
// deduplicates entries while preserving caller-provided order.
func normalizeAllowProtectedPaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if !allowedProtectedPaths[p] || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// buildProtectedPathAllowRules emits permissions.allow rule strings for the
// given protected directory names (e.g. "claude" → Edit/Write/MultiEdit for
// any **/.claude/** path). Returns a JSON array body (without the surrounding
// brackets) suitable for inlining into settings.local.json.
func buildProtectedPathAllowRules(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	var rules []string
	for _, name := range paths {
		if !allowedProtectedPaths[name] {
			continue
		}
		for _, tool := range []string{"Edit", "Write", "MultiEdit"} {
			rules = append(rules, fmt.Sprintf(`"%s(**/.%s/**)"`, tool, name))
		}
	}
	return strings.Join(rules, ",")
}

// PrepareClaudeSettings writes .claude/settings.local.json with persona
// override and (when apiBase is available) a PreCompact hook that calls
// kojo's API to save a conversation summary before Claude compacts context.
// Called from Manager.Chat before backend.Chat to ensure settings are in
// place before the Claude process reads them.
func PrepareClaudeSettings(agentID, apiBase string, allowProtectedPaths []string, logger *slog.Logger) {
	dir := agentDir(agentID)
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		logger.Warn("failed to create .claude dir", "agent", agentID, "err", err)
		return
	}

	allowRules := buildProtectedPathAllowRules(allowProtectedPaths)

	var settings string
	if apiBase == "" {
		// No API base — just disable persona hook
		settings = `{"persona":"agent-managed","permissions":{"defaultMode":"bypassPermissions","allow":[` + allowRules + `]}}` + "\n"
	} else {
		curlFlags := "-s"
		if strings.HasPrefix(apiBase, "https://") {
			curlFlags = "-sk"
		}
		// Hook payload: claude pipes a JSON object to stdin describing the
		// triggering event. The fields kojo cares about are
		// `transcript_path` (so we read the exact session being compacted
		// instead of guessing) and `trigger` (manual vs auto, useful for
		// telemetry but not yet used for branching). We pass that through
		// verbatim with `--data-binary @-`. Older builds sent `-d '{}'`
		// which dropped the payload, forcing the API to probe for the
		// session file by mtime.
		settings = fmt.Sprintf(`{
  "persona": "agent-managed",
  "permissions": {
    "defaultMode": "bypassPermissions",
    "allow": [%s]
  },
  "hooks": {
    "PreCompact": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "curl %s -X POST '%s/api/v1/agents/%s/pre-compact' -H 'Content-Type: application/json' --data-binary @- --max-time 120"
          }
        ]
      }
    ]
  }
}
`, allowRules, curlFlags, apiBase, agentID)
	}

	settingsPath := filepath.Join(claudeDir, "settings.local.json")
	if err := os.WriteFile(settingsPath, []byte(settings), 0o644); err != nil {
		logger.Warn("failed to write claude settings", "agent", agentID, "err", err)
	}
}

// isRealUserEntry returns true if the session JSONL "user" entry
// represents a real user message (not a tool_result feedback).
func isRealUserEntry(msgRaw json.RawMessage) bool {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(msgRaw, &msg) != nil {
		return false
	}
	// Try parsing content as an array of typed blocks.
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(msg.Content, &blocks) != nil {
		// Not an array — plain string content is a real user message.
		return true
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return false
		}
	}
	return true
}

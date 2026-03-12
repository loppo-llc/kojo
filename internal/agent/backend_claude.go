package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ClaudeBackend implements ChatBackend using the Claude CLI with stream-json output.
type ClaudeBackend struct {
	logger *slog.Logger
}

func NewClaudeBackend(logger *slog.Logger) *ClaudeBackend {
	return &ClaudeBackend{logger: logger}
}

func (b *ClaudeBackend) Name() string { return "claude" }

func (b *ClaudeBackend) Available() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

func (b *ClaudeBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string) (<-chan ChatEvent, error) {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("claude not found in PATH")
	}

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--dangerously-skip-permissions",
	}

	if systemPrompt != "" {
		args = append(args, "--system-prompt", systemPrompt)
	}

	if agent.Model != "" {
		args = append(args, "--model", agent.Model)
	}

	// Use --continue for conversation continuity when a session already exists
	// in the agent's working directory. Otherwise use --session-id to create
	// a new session with a deterministic UUID derived from the agent ID.
	dir := agentDir(agent.ID)
	os.MkdirAll(dir, 0o755)

	// Prevent user's persona autoload hook from overriding the agent's persona.
	// The SessionStart hook writes to CLAUDE.local.md in the working directory,
	// which overrides --system-prompt. Remove it and create .claude/settings.local.json
	// so the hook sees a local persona override and skips writing.
	disablePersonaHook(dir, b.logger)

	if hasExistingSession(dir) {
		args = append(args, "--continue")
	} else {
		sessionID := agentIDToUUID(agent.ID)
		args = append(args, "--session-id", sessionID)
	}

	cmd := exec.CommandContext(ctx, claudePath, args...)

	cmd.Env = filterEnv([]string{"CLAUDE_CODE", "CLAUDECODE", "AGENT_BROWSER_SESSION"}, agent.ID)
	cmd.Dir = dir

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

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		var fullText strings.Builder
		var lastAssistantText string
		var streamSessionID string
		var toolUses []ToolUse
		var usage *Usage

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			var event claudeStreamEvent
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				b.logger.Debug("failed to parse claude stream event", "err", err)
				continue
			}

			// Unwrap stream_event wrapper emitted by --include-partial-messages.
			// The inner "event" field contains the actual streaming event
			// (content_block_start, content_block_delta, etc.).
			if event.Type == "stream_event" && len(event.Event) > 0 {
				var inner claudeStreamEvent
				if err := json.Unmarshal(event.Event, &inner); err != nil {
					b.logger.Debug("failed to parse inner stream event", "err", err)
					continue
				}
				if inner.Type == "" {
					continue
				}
				event = inner
			}

			switch event.Type {
			case "system":
				if !send(ChatEvent{Type: "status", Status: "thinking"}) {
					cmd.Wait()
					return
				}

			case "assistant":
				// Extract text from content blocks as fallback when
				// content_block_delta events are not emitted.
				var atext strings.Builder
				for _, block := range event.Message.Content {
					if block.Type == "text" && block.Text != "" {
						atext.WriteString(block.Text)
					}
				}
				if atext.Len() > 0 {
					lastAssistantText = atext.String()
				}

				if event.Message.StopReason != "" {
					if event.Message.Usage.OutputTokens > 0 {
						usage = &Usage{
							InputTokens:  event.Message.Usage.InputTokens,
							OutputTokens: event.Message.Usage.OutputTokens,
						}
					}
				}

			case "content_block_start":
				// tool_use is sent later when the full input is available (see "tool_use" case).

			case "content_block_delta":
				if event.Delta.Type == "text_delta" && event.Delta.Text != "" {
					fullText.WriteString(event.Delta.Text)
					if !send(ChatEvent{Type: "text", Delta: event.Delta.Text}) {
						cmd.Wait()
						return
					}
				}

			case "content_block_stop":
				// If it was a tool use block, we'll get the result next

			case "result":
				if event.SessionID != "" {
					streamSessionID = event.SessionID
				}
				if event.Result != "" {
					if fullText.Len() == 0 {
						fullText.WriteString(event.Result)
						if !send(ChatEvent{Type: "text", Delta: event.Result}) {
							cmd.Wait()
							return
						}
					}
				}

			case "tool_use":
				tu := ToolUse{
					Name:  event.Name,
					Input: truncate(event.Input, 2000),
				}
				toolUses = append(toolUses, tu)
				if !send(ChatEvent{Type: "tool_use", ToolName: event.Name, ToolInput: truncate(event.Input, 2000)}) {
					cmd.Wait()
					return
				}

			case "tool_result":
				if !send(ChatEvent{Type: "tool_result", ToolName: event.Name, ToolOutput: truncate(event.Content, 2000)}) {
					cmd.Wait()
					return
				}
				matchToolOutput(toolUses, "", event.Name, truncate(event.Content, 2000))
			}
		}

		// Check for scanner errors
		if err := scanner.Err(); err != nil {
			b.logger.Warn("claude stream scanner error", "err", err)
		}

		// Check process exit status
		var processError string
		if err := cmd.Wait(); err != nil {
			b.logger.Warn("claude process exited with error", "err", err, "stderr", stderrBuf.String())
			processError = strings.TrimSpace(stderrBuf.String())
			if processError == "" {
				processError = err.Error()
			}
			if fullText.Len() == 0 && lastAssistantText == "" && len(toolUses) == 0 {
				send(ChatEvent{Type: "error", ErrorMessage: processError})
				return
			}
		}

		// Determine final text with fallback chain
		finalText := fullText.String()

		// Fallback: text extracted from assistant event content blocks
		if finalText == "" && lastAssistantText != "" {
			finalText = lastAssistantText
			b.logger.Info("used assistant event text as fallback", "agent", agent.ID, "len", len(finalText))
		}

		// Last resort: recover from Claude session JSONL when the stream
		// produced no usable text. Only used as fallback, never overrides
		// text that was successfully captured from the stream.
		if finalText == "" {
			if sessionText := recoverFromSession(agent.ID, streamSessionID, b.logger); sessionText != "" {
				b.logger.Info("recovered text from session log",
					"agent", agent.ID,
					"sessionLen", len(sessionText))
				finalText = sessionText
			}
		}

		// Send recovered text if nothing was streamed to client
		if finalText != "" && fullText.Len() == 0 {
			send(ChatEvent{Type: "text", Delta: finalText})
		}

		msg := newAssistantMessage()
		msg.Content = finalText
		msg.ToolUses = toolUses
		msg.Usage = usage

		send(ChatEvent{Type: "done", Message: msg, Usage: usage, ErrorMessage: processError})
	}()

	return ch, nil
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
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage,omitempty"`
	} `json:"message,omitempty"`

	// "content_block_start" event
	ContentBlock struct {
		Type string `json:"type"`
		Name string `json:"name,omitempty"`
	} `json:"content_block,omitempty"`

	// "content_block_delta" event
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text,omitempty"`
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

// claudeContentBlock represents a content block in a Claude assistant message.
type claudeContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
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

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
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
	absDir, err := filepath.Abs(agentDir)
	if err != nil {
		return false
	}
	projectDir := claudeProjectDir(absDir)
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
// When sessionID is provided, it looks for "<sessionID>.jsonl" first.
// Falls back to the most recently modified .jsonl file.
func findSessionFile(projectDir string, sessionID string) string {
	// Try exact match first.
	if sessionID != "" {
		path := filepath.Join(projectDir, sessionID+".jsonl")
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	// Fallback: newest .jsonl by modification time.
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
		return
	}
	projectDir := claudeProjectDir(absDir)
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			os.Remove(filepath.Join(projectDir, e.Name()))
		}
	}
}

// disablePersonaHook removes any CLAUDE.local.md written by the user's
// persona autoload hook and writes .claude/settings.local.json with a
// dummy persona name so the hook won't recreate the file on session start.
func disablePersonaHook(dir string, logger *slog.Logger) {
	if err := os.Remove(filepath.Join(dir, "CLAUDE.local.md")); err != nil && !os.IsNotExist(err) {
		logger.Warn("failed to remove CLAUDE.local.md from agent dir", "dir", dir, "err", err)
	}

	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		logger.Warn("failed to create .claude dir in agent dir", "dir", dir, "err", err)
		return
	}
	settingsPath := filepath.Join(claudeDir, "settings.local.json")
	if err := os.WriteFile(settingsPath, []byte("{\"persona\":\"agent-managed\"}\n"), 0o644); err != nil {
		logger.Warn("failed to write .claude/settings.local.json", "dir", dir, "err", err)
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

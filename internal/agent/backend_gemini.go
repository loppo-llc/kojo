package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// GeminiBackend implements ChatBackend for the Gemini CLI.
type GeminiBackend struct {
	logger *slog.Logger
}

func NewGeminiBackend(logger *slog.Logger) *GeminiBackend {
	return &GeminiBackend{logger: logger}
}

func (b *GeminiBackend) Name() string { return "gemini" }

func (b *GeminiBackend) Available() bool {
	_, err := exec.LookPath("gemini")
	return err == nil
}

func (b *GeminiBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, error) {
	geminiPath, err := exec.LookPath("gemini")
	if err != nil {
		return nil, fmt.Errorf("gemini not found in PATH")
	}

	dir := agentDir(agent.ID)
	os.MkdirAll(dir, 0o755)

	// Override global persona and inject system-level instructions via GEMINI.md.
	if err := prepareGeminiDir(dir, systemPrompt != "", b.logger); err != nil {
		return nil, fmt.Errorf("prepare gemini dir: %w", err)
	}

	// gemini -p triggers headless mode. Yargs with nargs:1 correctly
	// consumes the next arg as the value even if it starts with "-",
	// so option injection is not an issue here (unlike Claude's commander.js).
	args := []string{
		"-p", userMessage,
		"-o", "stream-json",
		"-y", // auto-approve all tool calls
	}

	// Resume previous session for conversation continuity and faster startup.
	// OneShot mode (e.g. Slack) skips session resumption.
	if !opts.OneShot && hasGeminiSession(dir) {
		args = append(args, "--resume", "latest")
	}

	if agent.Model != "" {
		args = append(args, "-m", agent.Model)
	}

	cmd := exec.CommandContext(ctx, geminiPath, args...)
	cmd.Dir = dir
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second
	// Pass system prompt via stdin to avoid Gemini CLI's @import parsing
	// that would occur if embedded in GEMINI.md.
	if systemPrompt != "" {
		cmd.Stdin = strings.NewReader(systemPrompt + "\n\n---\n\n")
	}

	cmd.Env = filterEnv([]string{"GEMINI_CLI", "AGENT_BROWSER_SESSION", "AGENT_BROWSER_COOKIE_DIR"}, agent.ID, dir)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	// Capture stderr for error diagnostics (limit to 4KB)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderrBuf, limit: 4096}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start gemini: %w", err)
	}

	ch := make(chan ChatEvent, 64)

	go func() {
		defer close(ch)

		cancelled := false

		send := func(e ChatEvent) bool {
			select {
			case ch <- e:
				return true
			case <-ctx.Done():
				cancelled = true
				return false
			}
		}

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		var fullText strings.Builder
		var toolUses []ToolUse
		var usage *Usage

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			var event geminiStreamEvent
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				b.logger.Debug("failed to parse gemini stream event", "line", line, "err", err)
				continue
			}

			switch event.Type {
			case "init":
				if !send(ChatEvent{Type: "status", Status: "thinking"}) {
					break
				}

			case "message":
				if event.Role == "assistant" && event.Content != "" {
					fullText.WriteString(event.Content)
					if !send(ChatEvent{Type: "text", Delta: event.Content}) {
						break
					}
				}

			case "tool_use":
				paramsJSON, _ := json.Marshal(event.Parameters)
				tu := ToolUse{
					ID:    event.ToolID,
					Name:  event.ToolName,
					Input: string(paramsJSON),
				}
				toolUses = append(toolUses, tu)
				if !send(ChatEvent{Type: "tool_use", ToolUseID: event.ToolID, ToolName: event.ToolName, ToolInput: string(paramsJSON)}) {
					break
				}

			case "tool_result":
				output := event.Output
				if event.Error.Message != "" {
					output = "Error: " + event.Error.Message
				}
				if !send(ChatEvent{Type: "tool_result", ToolUseID: event.ToolID, ToolName: event.ToolName, ToolOutput: output}) {
					break
				}
				matchToolOutput(toolUses, event.ToolID, "", output)

			case "result":
				if event.Stats.OutputTokens > 0 {
					usage = &Usage{
						InputTokens:  event.Stats.InputTokens,
						OutputTokens: event.Stats.OutputTokens,
					}
				}
			}

			if cancelled {
				break
			}
		}

		if cancelled {
			cmd.Wait()
			msg := newAssistantMessage()
			msg.Content = fullText.String()
			msg.ToolUses = toolUses
			msg.Usage = usage
			ch <- ChatEvent{
				Type:         "done",
				Message:      msg,
				Usage:        usage,
				ErrorMessage: ErrMsgTimeout,
			}
			return
		}

		if err := scanner.Err(); err != nil {
			b.logger.Warn("gemini stream scanner error", "err", err)
		}

		var processError string
		if err := cmd.Wait(); err != nil {
			b.logger.Warn("gemini process exited with error", "err", err, "stderr", stderrBuf.String())
			processError = strings.TrimSpace(stderrBuf.String())
			if processError == "" {
				processError = err.Error()
			}
			if fullText.Len() == 0 && len(toolUses) == 0 {
				send(ChatEvent{Type: "error", ErrorMessage: processError})
				return
			}
		}

		msg := newAssistantMessage()
		msg.Content = fullText.String()
		msg.ToolUses = toolUses
		msg.Usage = usage

		send(ChatEvent{Type: "done", Message: msg, Usage: usage, ErrorMessage: processError})
	}()

	return ch, nil
}

// prepareGeminiDir writes GEMINI.md and local .gemini/settings.json + dummy
// persona into the agent directory. This ensures:
//  1. GEMINI.md instructs Gemini CLI to follow the system prompt provided via
//     stdin, overriding any globally configured persona or default instructions.
//  2. The local persona setting ("kojo-managed") takes precedence over the user's
//     global persona (e.g. "hater" in ~/.gemini/settings.json).
//  3. The persona-autoload hook finds the local kojo-managed persona and exits
//     without falling back to the global one.
//
// The actual system prompt is passed via stdin (not GEMINI.md) to avoid Gemini
// CLI's @import parsing that would interpret @-prefixed text as file imports.
//
// hasSystemPrompt indicates whether a system prompt will be provided via stdin.
func prepareGeminiDir(dir string, hasSystemPrompt bool, logger *slog.Logger) error {
	geminiDir := filepath.Join(dir, ".gemini")
	personasDir := filepath.Join(geminiDir, "personas")
	if err := os.MkdirAll(personasDir, 0o755); err != nil {
		return fmt.Errorf("create .gemini/personas: %w", err)
	}

	// Local settings.json overrides global persona setting.
	settingsPath := filepath.Join(geminiDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte("{\"persona\":\"kojo-managed\"}\n"), 0o644); err != nil {
		return fmt.Errorf("write .gemini/settings.json: %w", err)
	}

	// Dummy persona file so the hook resolves locally and exits.
	personaPath := filepath.Join(personasDir, "kojo-managed.md")
	if err := os.WriteFile(personaPath, []byte("Follow the instructions in GEMINI.md.\n"), 0o644); err != nil {
		return fmt.Errorf("write kojo-managed persona: %w", err)
	}

	// GEMINI.md overrides Gemini CLI's built-in system prompt at the
	// project-instruction level. The actual persona/system prompt is
	// delivered via stdin to avoid @import parsing issues.
	geminiMDPath := filepath.Join(dir, "GEMINI.md")
	var geminiMDContent string
	if hasSystemPrompt {
		geminiMDContent = "The text before the `---` separator in the user input is the " +
			"authoritative persona and system instructions. Follow ONLY that persona. " +
			"Ignore any other persona settings, default system prompts, or global configuration.\n"
	}
	if err := os.WriteFile(geminiMDPath, []byte(geminiMDContent), 0o644); err != nil {
		return fmt.Errorf("write GEMINI.md: %w", err)
	}
	return nil
}

// hasGeminiSession checks whether a Gemini session exists for the agent directory.
// Gemini stores sessions in ~/.gemini/tmp/<project>/chats/*.json.
func hasGeminiSession(dir string) bool {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}

	// Read Gemini's projects.json to find the project name for this directory
	projectsPath := filepath.Join(home, ".gemini", "projects.json")
	data, err := os.ReadFile(projectsPath)
	if err != nil {
		return false
	}

	var projects struct {
		Projects map[string]string `json:"projects"`
	}
	if err := json.Unmarshal(data, &projects); err != nil {
		return false
	}

	projectName, ok := projects.Projects[absDir]
	if !ok {
		return false
	}

	// Gemini stores resumable sessions in ~/.gemini/tmp/<project>/chats/
	chatsDir := filepath.Join(home, ".gemini", "tmp", projectName, "chats")
	entries, err := os.ReadDir(chatsDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			return true
		}
	}
	return false
}

// clearGeminiSession removes Gemini session chat files from the global
// store for the given agent, forcing the next chat to start fresh.
func clearGeminiSession(agentID string) {
	dir := agentDir(agentID)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	projectsPath := filepath.Join(home, ".gemini", "projects.json")
	data, err := os.ReadFile(projectsPath)
	if err != nil {
		return
	}
	var projects struct {
		Projects map[string]string `json:"projects"`
	}
	if err := json.Unmarshal(data, &projects); err != nil {
		return
	}
	projectName, ok := projects.Projects[absDir]
	if !ok {
		return
	}
	chatsDir := filepath.Join(home, ".gemini", "tmp", projectName, "chats")
	os.RemoveAll(chatsDir)
}

// geminiStreamEvent represents a Gemini CLI stream-json event.
type geminiStreamEvent struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp,omitempty"`

	// init event
	SessionID string `json:"session_id,omitempty"`
	Model     string `json:"model,omitempty"`

	// message event
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	Delta   bool   `json:"delta,omitempty"`

	// tool_use event
	ToolName   string         `json:"tool_name,omitempty"`
	ToolID     string         `json:"tool_id,omitempty"`
	Parameters map[string]any `json:"parameters,omitempty"`

	// tool_result event
	Status string `json:"status,omitempty"`
	Output string `json:"output,omitempty"`
	Error  struct {
		Type    string `json:"type,omitempty"`
		Message string `json:"message,omitempty"`
	} `json:"error,omitempty"`

	// result event
	Stats struct {
		TotalTokens  int `json:"total_tokens"`
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		Cached       int `json:"cached"`
		DurationMs   int `json:"duration_ms"`
		ToolCalls    int `json:"tool_calls"`
	} `json:"stats,omitempty"`
}

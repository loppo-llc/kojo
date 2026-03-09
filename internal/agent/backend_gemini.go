package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func (b *GeminiBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string) (<-chan ChatEvent, error) {
	geminiPath, err := exec.LookPath("gemini")
	if err != nil {
		return nil, fmt.Errorf("gemini not found in PATH")
	}

	dir := agentDir(agent.ID)
	os.MkdirAll(dir, 0o755)

	// Prevent user's persona autoload hook from overriding the agent's persona.
	disableGeminiPersonaHook(dir, b.logger)

	// gemini -p triggers headless mode. Yargs with nargs:1 correctly
	// consumes the next arg as the value even if it starts with "-",
	// so option injection is not an issue here (unlike Claude's commander.js).
	args := []string{
		"-p", userMessage,
		"-o", "stream-json",
		"-y", // auto-approve all tool calls
	}

	// Resume previous session for conversation continuity and faster startup
	if hasGeminiSession(dir) {
		args = append(args, "--resume", "latest")
	}

	var stdinContent string
	if systemPrompt != "" {
		stdinContent = systemPrompt + "\n\n---\n\n"
	}

	if agent.Model != "" {
		args = append(args, "-m", agent.Model)
	}

	cmd := exec.CommandContext(ctx, geminiPath, args...)
	cmd.Dir = dir
	if stdinContent != "" {
		cmd.Stdin = strings.NewReader(stdinContent)
	}

	cmd.Env = filterEnv([]string{"GEMINI_CLI", "AGENT_BROWSER_SESSION"}, agent.ID)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	// Capture stderr for error reporting
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start gemini: %w", err)
	}

	ch := make(chan ChatEvent, 64)

	go func() {
		defer close(ch)

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
					cmd.Wait()
					return
				}

			case "message":
				if event.Role == "assistant" && event.Content != "" {
					fullText.WriteString(event.Content)
					if !send(ChatEvent{Type: "text", Delta: event.Content}) {
						cmd.Wait()
						return
					}
				}

			case "tool_use":
				paramsJSON, _ := json.Marshal(event.Parameters)
				tu := ToolUse{
					ID:    event.ToolID,
					Name:  event.ToolName,
					Input: truncate(string(paramsJSON), 2000),
				}
				toolUses = append(toolUses, tu)
				if !send(ChatEvent{Type: "tool_use", ToolName: event.ToolName, ToolInput: truncate(string(paramsJSON), 2000)}) {
					cmd.Wait()
					return
				}

			case "tool_result":
				output := event.Output
				if event.Error.Message != "" {
					output = "Error: " + event.Error.Message
				}
				if !send(ChatEvent{Type: "tool_result", ToolName: event.ToolName, ToolOutput: truncate(output, 2000)}) {
					cmd.Wait()
					return
				}
				matchToolOutput(toolUses, event.ToolID, "", truncate(output, 2000))

			case "result":
				if event.Stats.OutputTokens > 0 {
					usage = &Usage{
						InputTokens:  event.Stats.InputTokens,
						OutputTokens: event.Stats.OutputTokens,
					}
				}
			}
		}

		if err := scanner.Err(); err != nil {
			b.logger.Warn("gemini stream scanner error", "err", err)
		}

		if err := cmd.Wait(); err != nil {
			b.logger.Warn("gemini process exited with error", "err", err)
			if fullText.Len() == 0 && len(toolUses) == 0 {
				send(ChatEvent{Type: "error", ErrorMessage: fmt.Sprintf("gemini exited with error: %v", err)})
				return
			}
		}

		msg := newAssistantMessage()
		msg.Content = fullText.String()
		msg.ToolUses = toolUses
		msg.Usage = usage

		send(ChatEvent{Type: "done", Message: msg, Usage: usage})
	}()

	return ch, nil
}

// disableGeminiPersonaHook creates local .gemini/settings.json and a dummy
// persona file in the agent directory so the user's global persona-autoload
// hook does not override the agent's persona injected via system prompt.
func disableGeminiPersonaHook(dir string, logger *slog.Logger) {
	geminiDir := filepath.Join(dir, ".gemini")
	personasDir := filepath.Join(geminiDir, "personas")
	if err := os.MkdirAll(personasDir, 0o755); err != nil {
		logger.Warn("failed to create .gemini/personas in agent dir", "dir", dir, "err", err)
		return
	}

	settingsPath := filepath.Join(geminiDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte("{\"persona\":\"kojo-managed\"}\n"), 0o644); err != nil {
		logger.Warn("failed to write .gemini/settings.json", "dir", dir, "err", err)
	}

	personaPath := filepath.Join(personasDir, "kojo-managed.md")
	if err := os.WriteFile(personaPath, []byte("Follow the persona defined in the system prompt.\n"), 0o644); err != nil {
		logger.Warn("failed to write kojo-managed persona", "dir", dir, "err", err)
	}
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

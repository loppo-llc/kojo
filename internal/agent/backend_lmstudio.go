package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"sync"
)

// LMStudioBackend implements ChatBackend using the LM Studio REST API
// (/api/v1/chat with SSE streaming and stateful sessions).
type LMStudioBackend struct {
	logger *slog.Logger
	client *http.Client

	// responseIDs tracks the last response_id per agent for stateful continuation.
	mu          sync.Mutex
	responseIDs map[string]string // agentID → last response_id
}

func NewLMStudioBackend(logger *slog.Logger) *LMStudioBackend {
	return &LMStudioBackend{
		logger:      logger,
		client:      &http.Client{},
		responseIDs: make(map[string]string),
	}
}

func (b *LMStudioBackend) Name() string { return "lm-studio" }

func (b *LMStudioBackend) Available() bool {
	_, err := exec.LookPath("lms")
	return err == nil
}

// lmsBaseURL returns the LM Studio server URL by checking lms status for the port.
func lmsBaseURL() string {
	out, err := exec.Command("lms", "status").Output()
	if err != nil {
		return "http://localhost:1234"
	}
	// Parse "Server: ON (port: 8189)"
	text := string(out)
	if idx := strings.Index(text, "port:"); idx >= 0 {
		rest := strings.TrimSpace(text[idx+5:])
		if end := strings.IndexByte(rest, ')'); end > 0 {
			return "http://localhost:" + strings.TrimSpace(rest[:end])
		}
	}
	return "http://localhost:1234"
}

// lmsChatRequest is the request body for POST /api/v1/chat.
type lmsChatRequest struct {
	Model              string `json:"model"`
	Input              string `json:"input"`
	SystemPrompt       string `json:"system_prompt,omitempty"`
	Stream             bool   `json:"stream"`
	Store              bool   `json:"store"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
}

// lmsChatEndResult is the parsed result from a chat.end SSE event.
type lmsChatEndResult struct {
	Result struct {
		Output []struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		} `json:"output"`
		Stats struct {
			InputTokens      int     `json:"input_tokens"`
			TotalOutputTokens int    `json:"total_output_tokens"`
			TokensPerSecond  float64 `json:"tokens_per_second"`
		} `json:"stats"`
		ResponseID string `json:"response_id"`
	} `json:"result"`
}

func (b *LMStudioBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, error) {
	baseURL := lmsBaseURL()

	// Build request
	req := lmsChatRequest{
		Model:  agent.Model,
		Input:  userMessage,
		Stream: true,
		Store:  true,
	}

	// Only send system_prompt on first message (no previous_response_id)
	b.mu.Lock()
	prevID := b.responseIDs[agent.ID]
	b.mu.Unlock()

	if prevID != "" {
		req.PreviousResponseID = prevID
	} else {
		req.SystemPrompt = systemPrompt
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/api/v1/chat", strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("lm studio request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("lm studio returned %d", resp.StatusCode)
	}

	ch := make(chan ChatEvent, 64)

	go func() {
		defer close(ch)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 256*1024), 256*1024)

		var eventType string

		for scanner.Scan() {
			line := scanner.Text()

			if strings.HasPrefix(line, "event: ") {
				eventType = strings.TrimPrefix(line, "event: ")
				continue
			}

			if !strings.HasPrefix(line, "data: ") {
				continue
			}

			data := strings.TrimPrefix(line, "data: ")

			switch eventType {
			case "prompt_processing.start":
				ch <- ChatEvent{Type: "status", Status: "Processing prompt..."}

			case "message.delta":
				var delta struct {
					Content string `json:"content"`
				}
				if json.Unmarshal([]byte(data), &delta) == nil && delta.Content != "" {
					ch <- ChatEvent{Type: "text", Delta: delta.Content}
				}

			case "chat.end":
				var result lmsChatEndResult
				if json.Unmarshal([]byte(data), &result) == nil {
					// Save response_id for stateful continuation
					if result.Result.ResponseID != "" {
						b.mu.Lock()
						b.responseIDs[agent.ID] = result.Result.ResponseID
						b.mu.Unlock()
					}

					// Collect full text from output
					var fullText string
					for _, item := range result.Result.Output {
						if item.Type == "message" {
							fullText += item.Content
						}
					}

					ch <- ChatEvent{
						Type: "done",
						Message: &Message{
							ID:        generateMessageID(),
							Role:      "assistant",
							Content:   fullText,
							Timestamp: "",
						},
						Usage: &Usage{
							InputTokens:  result.Result.Stats.InputTokens,
							OutputTokens: result.Result.Stats.TotalOutputTokens,
						},
					}
				}

			case "chat.error":
				var errEvt struct {
					Error string `json:"error"`
				}
				if json.Unmarshal([]byte(data), &errEvt) == nil {
					ch <- ChatEvent{Type: "error", ErrorMessage: errEvt.Error}
				}
			}
		}

		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			b.logger.Debug("lm studio stream error", "err", err)
		}
	}()

	return ch, nil
}

// ListModels returns the list of LLM model keys available in LM Studio.
func (b *LMStudioBackend) ListModels() []string {
	out, err := exec.Command("lms", "ls", "--llm", "--json").Output()
	if err != nil {
		return nil
	}
	var models []struct {
		ModelKey string `json:"modelKey"`
	}
	if json.Unmarshal(out, &models) != nil {
		return nil
	}
	result := make([]string, 0, len(models))
	for _, m := range models {
		if m.ModelKey != "" {
			result = append(result, m.ModelKey)
		}
	}
	return result
}

// ResetSession clears the stateful response chain for an agent.
func (b *LMStudioBackend) ResetSession(agentID string) {
	b.mu.Lock()
	delete(b.responseIDs, agentID)
	b.mu.Unlock()
}

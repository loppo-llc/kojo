package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/loppo-llc/kojo/internal/customapi"
)

// CustomBareBackend implements ChatBackend by talking directly to an
// OpenAI-compatible /v1/chat/completions endpoint (llama-server and friends)
// via HTTP SSE streaming. "bare" because there is no CLI in between: the
// model gets a system prompt, a bounded replay of the conversation so far
// and the current user message, and nothing else — no tools, no MCP, no
// server-side session.
//
// The replay is the only continuity this backend has. /v1/chat/completions
// is stateless, so kojo rebuilds the transcript from its own store on
// every turn (ChatOptions.History, capped in bare_history.go).
type CustomBareBackend struct {
	logger *slog.Logger
	creds  *CredentialStore
}

func NewCustomBareBackend(logger *slog.Logger, creds *CredentialStore) *CustomBareBackend {
	return &CustomBareBackend{logger: logger, creds: creds}
}

func (b *CustomBareBackend) Name() string { return ToolCustomBare }

func (b *CustomBareBackend) Available() bool { return true }

func (b *CustomBareBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, error) {
	// Local callers provide the canonical transcript as structured History,
	// which is replayed below. Remote peer dispatch deliberately sends only the
	// bounded preformatted context instead, so use that fallback only when the
	// structured replay is absent. Applying both would duplicate every prior
	// turn in the request.
	if len(opts.History) == 0 {
		userMessage = injectSessionHistoryContext(userMessage, opts.FreshSessionContext, opts.ResumeSessionContext, false)
	}
	if agent.CustomBaseURL == "" {
		return nil, fmt.Errorf("customBaseURL is required for the custom-bare backend")
	}
	client, err := customapi.NewLocalOrTailnetClient(ctx, agent.CustomBaseURL, 0)
	if err != nil {
		return nil, fmt.Errorf("custom-bare customBaseURL: %w", err)
	}
	apiKey, err := LoadCustomAPIKey(b.creds, agent.ID, agent.CustomBaseURL)
	if err != nil {
		client.CloseIdleConnections()
		return nil, fmt.Errorf("load custom API key: %w", err)
	}

	effectivePrompt := systemPrompt
	if agent.ThinkingMode == "off" {
		const noThinkDirective = "Do not use <think> tags. Respond directly without showing your reasoning."
		if effectivePrompt != "" {
			effectivePrompt = noThinkDirective + "\n\n" + effectivePrompt
		} else {
			effectivePrompt = noThinkDirective
		}
	}

	messages := []llamaCppMessage{}
	if effectivePrompt != "" {
		messages = append(messages, llamaCppMessage{Role: "system", Content: effectivePrompt})
	}
	// Prior turns, oldest first, then the current one. History never
	// carries a system entry and never repeats the current message, so
	// it splices in verbatim.
	history := opts.History
	// A replay ending on a user turn (an unanswered question: the turn
	// errored, timed out, or was aborted) would sit right next to the
	// current user message, and chat templates that assume strict
	// alternation mangle or reject two user turns in a row. Fold it into
	// the current message instead of dropping it — it is usually still
	// waiting for an answer.
	if n := len(history); n > 0 && history[n-1].Role == "user" {
		userMessage = foldIntoUserMessage(userMessage, history[n-1].Content)
		history = history[:n-1]
	}
	for _, turn := range history {
		messages = append(messages, llamaCppMessage{Role: turn.Role, Content: turn.Content})
	}
	messages = append(messages, llamaCppMessage{Role: "user", Content: userMessage})

	reqBody := llamaCppRequest{
		Model:    agent.Model,
		Messages: messages,
		Stream:   true,
		StreamOptions: &llamaCppStreamOptions{
			IncludeUsage: true,
		},
	}

	switch agent.ThinkingMode {
	case "off":
		reqBody.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	case "on":
		reqBody.ChatTemplateKwargs = map[string]any{"enable_thinking": true}
		reqBody.ReasoningFormat = "deepseek"
	}

	thinkOff := agent.ThinkingMode == "off"

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		client.CloseIdleConnections()
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	endpoint := strings.TrimRight(agent.CustomBaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		client.CloseIdleConnections()
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey == "" {
		apiKey = "no-key"
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := client.Do(req)
	if err != nil {
		client.CloseIdleConnections()
		return nil, fmt.Errorf("request to %s: %w", endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		client.CloseIdleConnections()
		return nil, fmt.Errorf("llama-server returned %d: %s", resp.StatusCode, string(body))
	}

	ch := make(chan ChatEvent, 64)

	go func() {
		defer close(ch)
		defer client.CloseIdleConnections()
		defer resp.Body.Close()
		b.streamSSE(ctx, ch, resp.Body, thinkOff)
	}()

	return ch, nil
}

func (b *CustomBareBackend) streamSSE(ctx context.Context, ch chan<- ChatEvent, body io.Reader, thinkOff bool) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)

	var content strings.Builder
	var thinking strings.Builder
	var usage *Usage
	started := false

	var stripper *thinkStripper
	if thinkOff {
		stripper = &thinkStripper{}
	}

	send := func(e ChatEvent) bool { return ctxSend(ctx, ch, e) }

	emitText := func(text string) bool {
		if stripper != nil {
			text = stripper.Process(text)
		}
		if text == "" {
			return true
		}
		content.WriteString(text)
		return send(ChatEvent{Type: "text", Delta: text})
	}

	// SSE spec: events are delimited by blank lines.
	// Each event may have multiple "data:" lines that are concatenated with "\n".
	var dataBuf strings.Builder

	dispatch := func(data string) (done, cancelled bool) {
		if data == "[DONE]" {
			return true, false
		}

		var chunk llamaCppChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			b.logger.Warn("failed to parse SSE chunk", "err", err, "data", data)
			return false, false
		}

		if chunk.Usage != nil {
			usage = &Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
			}
		}

		if len(chunk.Choices) == 0 {
			return false, false
		}
		delta := chunk.Choices[0].Delta

		if !started {
			if !send(ChatEvent{Type: "status", Status: "thinking"}) {
				return false, true
			}
			started = true
		}

		if delta.ReasoningContent != "" && !thinkOff {
			thinking.WriteString(delta.ReasoningContent)
			if !send(ChatEvent{Type: "thinking", Delta: delta.ReasoningContent}) {
				return false, true
			}
		}

		if delta.Content != "" {
			if !emitText(delta.Content) {
				return false, true
			}
		}

		return false, false
	}

	streamDone := false
	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			if dataBuf.Len() == 0 {
				continue
			}
			data := dataBuf.String()
			dataBuf.Reset()

			done, cancelled := dispatch(data)
			if cancelled {
				emitCancelDone(ctx, ch, content.String(), thinking.String(), nil, usage)
				return
			}
			if done {
				streamDone = true
				break
			}
			continue
		}

		if strings.HasPrefix(line, "data:") {
			val := line[5:]
			if len(val) > 0 && val[0] == ' ' {
				val = val[1:]
			}
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(val)
		}
	}

	if !streamDone && dataBuf.Len() > 0 {
		data := dataBuf.String()
		_, cancelled := dispatch(data)
		if cancelled {
			emitCancelDone(ctx, ch, content.String(), thinking.String(), nil, usage)
			return
		}
	}

	// Flush any buffered content from the think stripper
	if stripper != nil {
		if remaining := stripper.Flush(); remaining != "" {
			content.WriteString(remaining)
			send(ChatEvent{Type: "text", Delta: remaining})
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		b.logger.Warn("SSE scan error", "err", err)
		send(ChatEvent{Type: "error", ErrorMessage: err.Error()})
		return
	}

	if ctx.Err() != nil {
		emitCancelDone(ctx, ch, content.String(), thinking.String(), nil, usage)
		return
	}

	msg := assembleAssistantMessage(content.String(), thinking.String(), nil, usage)

	ch <- ChatEvent{
		Type:    "done",
		Message: msg,
		Usage:   usage,
	}
}

// thinkStripper filters out thinking-related markers from streamed content.
// Handles Gemma-style channel markers and DeepSeek-style <think> blocks.
// Uses a tail buffer to handle tags split across chunk boundaries.
type thinkStripper struct {
	prefixBuf  strings.Builder
	prefixDone bool
	inBlock    bool            // inside <think>...</think>
	tailBuf    strings.Builder // suffix that might be part of a tag
}

var thinkChannelMarkers = []string{
	"<|channel>thought<channel|>",
	"<|channel>response<channel|>",
	"<|/channel|>",
}

var allThinkPatterns = []string{
	"<think>",
	"</think>",
	"<|channel>thought<channel|>",
	"<|channel>response<channel|>",
	"<|/channel|>",
}

func (s *thinkStripper) Process(delta string) string {
	if !s.prefixDone {
		return s.checkPrefix(delta)
	}
	return s.filter(delta)
}

func (s *thinkStripper) checkPrefix(delta string) string {
	s.prefixBuf.WriteString(delta)
	text := s.prefixBuf.String()

	for _, m := range thinkChannelMarkers {
		if strings.HasPrefix(text, m) {
			s.prefixDone = true
			s.prefixBuf.Reset()
			return s.filter(text[len(m):])
		}
		if len(text) < len(m) && strings.HasPrefix(m, text) {
			return ""
		}
	}
	const thinkOpen = "<think>"
	if strings.HasPrefix(text, thinkOpen) {
		s.prefixDone = true
		s.prefixBuf.Reset()
		s.inBlock = true
		return s.filter(text[len(thinkOpen):])
	}
	if len(text) < len(thinkOpen) && strings.HasPrefix(thinkOpen, text) {
		return ""
	}

	s.prefixDone = true
	s.prefixBuf.Reset()
	return s.filter(text)
}

func (s *thinkStripper) filter(delta string) string {
	if delta == "" && s.tailBuf.Len() == 0 {
		return ""
	}

	var text string
	if s.tailBuf.Len() > 0 {
		text = s.tailBuf.String() + delta
		s.tailBuf.Reset()
	} else {
		text = delta
	}

	for _, m := range thinkChannelMarkers {
		text = strings.ReplaceAll(text, m, "")
	}
	if text == "" {
		return ""
	}

	var result strings.Builder
	for len(text) > 0 {
		if s.inBlock {
			idx := strings.Index(text, "</think>")
			if idx >= 0 {
				text = text[idx+len("</think>"):]
				s.inBlock = false
				continue
			}
			if n := partialTagSuffix(text, "</think>"); n > 0 {
				s.tailBuf.WriteString(text[len(text)-n:])
			}
			break
		}

		idx := strings.Index(text, "<think>")
		if idx >= 0 {
			result.WriteString(text[:idx])
			text = text[idx+len("<think>"):]
			s.inBlock = true
			continue
		}

		if n := longestPartialTag(text); n > 0 {
			result.WriteString(text[:len(text)-n])
			s.tailBuf.WriteString(text[len(text)-n:])
		} else {
			result.WriteString(text)
		}
		break
	}

	return result.String()
}

// longestPartialTag returns the length of the longest suffix of text
// that is a proper prefix of any known think-related tag.
func longestPartialTag(text string) int {
	best := 0
	for _, pat := range allThinkPatterns {
		maxN := len(pat) - 1
		if maxN > len(text) {
			maxN = len(text)
		}
		for n := maxN; n > best; n-- {
			if text[len(text)-n:] == pat[:n] {
				best = n
				break
			}
		}
	}
	return best
}

func partialTagSuffix(text, tag string) int {
	maxN := len(tag) - 1
	if maxN > len(text) {
		maxN = len(text)
	}
	for n := maxN; n > 0; n-- {
		if text[len(text)-n:] == tag[:n] {
			return n
		}
	}
	return 0
}

func (s *thinkStripper) Flush() string {
	var result strings.Builder
	if !s.prefixDone && s.prefixBuf.Len() > 0 {
		s.prefixDone = true
		text := s.prefixBuf.String()
		s.prefixBuf.Reset()
		result.WriteString(s.filter(text))
	}
	if s.tailBuf.Len() > 0 {
		tail := s.tailBuf.String()
		s.tailBuf.Reset()
		if !s.inBlock {
			result.WriteString(tail)
		}
	}
	return result.String()
}

// --- Request/Response types ---

type llamaCppMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type llamaCppStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type llamaCppRequest struct {
	Model              string                 `json:"model"`
	Messages           []llamaCppMessage      `json:"messages"`
	Stream             bool                   `json:"stream"`
	StreamOptions      *llamaCppStreamOptions `json:"stream_options,omitempty"`
	ChatTemplateKwargs map[string]any         `json:"chat_template_kwargs,omitempty"`
	ReasoningFormat    string                 `json:"reasoning_format,omitempty"`
}

type llamaCppChunk struct {
	Choices []llamaCppChoice `json:"choices"`
	Usage   *llamaCppUsage   `json:"usage,omitempty"`
}

type llamaCppChoice struct {
	Delta llamaCppDelta `json:"delta"`
}

type llamaCppDelta struct {
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type llamaCppUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

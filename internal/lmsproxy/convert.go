package lmsproxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// --- Request conversion: Anthropic → OpenAI Responses ---

// BuildOAIRequest converts an Anthropic Messages request into an OpenAI Responses request.
// allowedTools filters which tools are forwarded (nil/empty = all tools).
func BuildOAIRequest(req *AnthropicRequest, prevID string, newMsgs []AnthropicMessage, allowedTools map[string]bool) (*OAIRequest, error) {
	items, err := convertMessages(newMsgs)
	if err != nil {
		return nil, fmt.Errorf("convert messages: %w", err)
	}

	inputJSON, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("marshal input: %w", err)
	}

	oai := &OAIRequest{
		Model:              req.Model,
		Input:              inputJSON,
		MaxOutputTokens:    req.MaxTokens,
		PreviousResponseID: prevID,
		Stream:             true,
		Store:              true,
	}

	// Instructions are only needed on the first request; LM Studio
	// restores them from previous_response_id on follow-ups.
	if prevID == "" {
		oai.Instructions = extractSystem(req.System)
	}

	// Always send tools: LM Studio does NOT restore tool definitions
	// from previous_response_id, so omitting them causes the model to
	// fall back to text-based pseudo-tool-calls.
	for _, t := range req.Tools {
		if len(allowedTools) > 0 && !allowedTools[t.Name] {
			continue
		}
		oai.Tools = append(oai.Tools, OAITool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}

	return oai, nil
}

// convertMessages converts Anthropic messages to OAI input items.
func convertMessages(msgs []AnthropicMessage) ([]OAIInputItem, error) {
	var items []OAIInputItem

	for _, msg := range msgs {
		blocks, err := parseContent(msg.Content)
		if err != nil {
			return nil, err
		}

		for _, b := range blocks {
			switch b.Type {
			case "text", "":
				textType := "input_text"
				if msg.Role == "assistant" {
					textType = "output_text"
				}
				items = append(items, OAIInputItem{
					Type: "message",
					Role: msg.Role,
					Content: []OAIContentPart{{
						Type: textType,
						Text: b.Text,
					}},
				})

			case "tool_use":
				// Assistant's tool call → include as a function_call input item
				// so LM Studio can reconstruct the conversation history.
				items = append(items, OAIInputItem{
					Type:      "function_call",
					CallID:    b.ID,
					Name:      b.Name,
					Arguments: string(b.Input),
				})

			case "tool_result":
				content := extractToolResultContent(b.Content)
				items = append(items, OAIInputItem{
					Type:   "function_call_output",
					CallID: b.ToolUseID,
					Output: content,
				})

			case "thinking":
				// Ignore thinking blocks.
			}
		}
	}

	return items, nil
}

// parseContent parses an Anthropic message content field.
// It can be a plain string or an array of content blocks.
func parseContent(raw json.RawMessage) ([]AnthropicContentBlock, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	// Try string first.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []AnthropicContentBlock{{Type: "text", Text: s}}, nil
	}

	// Must be an array.
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("parse content: %w", err)
	}
	return blocks, nil
}

// extractSystem parses the system field which may be a string or an array of
// {type:"text", text:"..."} blocks, and returns a single string.
func extractSystem(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n\n")
	}

	return ""
}

// extractToolResultContent extracts a string from a tool_result's content field.
func extractToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}

	// Array of content blocks.
	var blocks []AnthropicContentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}

	return string(raw)
}

// --- Response conversion: OpenAI Responses SSE → Anthropic SSE ---

// StreamConverter reads OAI Responses SSE events from an io.Reader and writes
// Anthropic Messages SSE events to an http.ResponseWriter.
type StreamConverter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	model   string

	// State tracking.
	responseID    string
	blockIndex    int  // current Anthropic content_block index
	hasToolUse    bool
	argsDeltaSent bool // whether any function_call_arguments.delta was sent for current block
}

// NewStreamConverter creates a converter that writes Anthropic SSE to w.
func NewStreamConverter(w http.ResponseWriter, model string) *StreamConverter {
	f, _ := w.(http.Flusher)
	return &StreamConverter{
		w:       w,
		flusher: f,
		model:   model,
	}
}

// ResponseID returns the response ID from LM Studio after processing completes.
func (c *StreamConverter) ResponseID() string {
	return c.responseID
}

// Process reads the OAI SSE stream and writes Anthropic SSE events.
// Returns an error only for I/O failures; LM Studio errors are forwarded as SSE.
func (c *StreamConverter) Process(body io.Reader) error {
	scanner := bufio.NewScanner(body)
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
		if data == "[DONE]" {
			break
		}

		if err := c.handleEvent(eventType, []byte(data)); err != nil {
			return err
		}
	}

	return scanner.Err()
}

func (c *StreamConverter) handleEvent(eventType string, data []byte) error {
	switch eventType {
	case "response.created":
		var evt OAIResponseCreated
		if json.Unmarshal(data, &evt) != nil {
			return nil
		}
		c.responseID = evt.Response.ID
		return c.writeMessageStart(evt.Response.ID)

	case "response.output_item.added":
		var evt OAIOutputItemAdded
		if json.Unmarshal(data, &evt) != nil {
			return nil
		}
		if evt.Item.Type == "function_call" {
			c.hasToolUse = true
			return c.writeContentBlockStart(c.blockIndex, "tool_use", evt.Item.ID, evt.Item.Name)
		}

	case "response.content_part.added":
		// Text content block starting.
		return c.writeContentBlockStart(c.blockIndex, "text", "", "")

	case "response.output_text.delta":
		var evt OAIOutputTextDelta
		if json.Unmarshal(data, &evt) != nil {
			return nil
		}
		return c.writeTextDelta(c.blockIndex, evt.Delta)

	case "response.function_call_arguments.delta":
		var evt OAIFuncCallArgsDelta
		if json.Unmarshal(data, &evt) != nil {
			return nil
		}
		c.argsDeltaSent = true
		return c.writeInputJSONDelta(c.blockIndex, evt.Delta)

	case "response.output_text.done":
		// Text block finished.
		if err := c.writeContentBlockStop(c.blockIndex); err != nil {
			return err
		}
		c.blockIndex++

	case "response.function_call_arguments.done":
		// If no delta events were sent (LM Studio sent args in one shot),
		// emit the full arguments as a single input_json_delta.
		if !c.argsDeltaSent {
			var evt OAIFuncCallArgsDone
			if json.Unmarshal(data, &evt) == nil && evt.Arguments != "" {
				if err := c.writeInputJSONDelta(c.blockIndex, evt.Arguments); err != nil {
					return err
				}
			}
		}
		c.argsDeltaSent = false
		// Tool call block finished.
		if err := c.writeContentBlockStop(c.blockIndex); err != nil {
			return err
		}
		c.blockIndex++

	case "response.output_item.done":
		// Nothing to emit; we close blocks in the specific done events above.

	case "response.completed":
		var evt OAIResponseCompleted
		if json.Unmarshal(data, &evt) != nil {
			return nil
		}
		c.responseID = evt.Response.ID

		stopReason := "end_turn"
		if c.hasToolUse {
			stopReason = "tool_use"
		}

		var outputTokens int
		if evt.Response.Usage != nil {
			outputTokens = evt.Response.Usage.OutputTokens
		}

		if err := c.writeMessageDelta(stopReason, outputTokens); err != nil {
			return err
		}
		return c.writeMessageStop()

	case "error":
		var errEvt struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &errEvt) == nil {
			return c.writeError(errEvt.Message)
		}
	}

	return nil
}

// --- Anthropic SSE writers ---

func (c *StreamConverter) writeSSE(event string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.w, "event: %s\ndata: %s\n\n", event, data)
	if c.flusher != nil {
		c.flusher.Flush()
	}
	return err
}

func (c *StreamConverter) writeMessageStart(id string) error {
	return c.writeSSE("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         c.model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         anthropicUsage(0, 0),
		},
	})
}

func (c *StreamConverter) writeContentBlockStart(index int, blockType, id, name string) error {
	cb := map[string]interface{}{"type": blockType}
	if blockType == "text" {
		cb["text"] = ""
	} else if blockType == "tool_use" {
		cb["id"] = id
		cb["name"] = name
		cb["input"] = map[string]interface{}{}
	}
	return c.writeSSE("content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         index,
		"content_block": cb,
	})
}

func (c *StreamConverter) writeTextDelta(index int, text string) error {
	return c.writeSSE("content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]string{
			"type": "text_delta",
			"text": text,
		},
	})
}

func (c *StreamConverter) writeInputJSONDelta(index int, partial string) error {
	return c.writeSSE("content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]string{
			"type":         "input_json_delta",
			"partial_json": partial,
		},
	})
}

func (c *StreamConverter) writeContentBlockStop(index int) error {
	return c.writeSSE("content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": index,
	})
}

func (c *StreamConverter) writeMessageDelta(stopReason string, outputTokens int) error {
	return c.writeSSE("message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": anthropicUsage(0, outputTokens),
	})
}

func (c *StreamConverter) writeMessageStop() error {
	return c.writeSSE("message_stop", map[string]interface{}{
		"type": "message_stop",
	})
}

// anthropicUsage returns a usage object matching the Anthropic Messages API
// schema. Claude CLI expects fields like cache_creation_input_tokens and speed
// to exist; omitting them causes JavaScript crashes in the CLI.
func anthropicUsage(inputTokens, outputTokens int) map[string]interface{} {
	return map[string]interface{}{
		"input_tokens":                inputTokens,
		"output_tokens":               outputTokens,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     0,
		"server_tool_use": map[string]int{
			"web_search_requests": 0,
			"web_fetch_requests":  0,
		},
		"service_tier": nil,
		"speed":        nil,
	}
}

// AccumulatedMessage holds the result of consuming an OAI SSE stream
// and converting it into a single Anthropic Messages API response.
type AccumulatedMessage struct {
	ResponseID string
	Message    map[string]interface{}
}

// AccumulateResponse reads an OAI Responses SSE stream to completion and
// returns a single Anthropic Messages API Message object. Used when the
// client sends stream=false (e.g. Claude CLI's non-streaming fallback).
func AccumulateResponse(body io.Reader, model string) (*AccumulatedMessage, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	var (
		eventType    string
		responseID   string
		contentBlocks []interface{}
		hasToolUse   bool
		stopReason   = "end_turn"
		inputTokens  int
		outputTokens int
		// Current tool_use state
		currentToolID   string
		currentToolName string
		currentToolArgs strings.Builder
	)

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
		if data == "[DONE]" {
			break
		}

		raw := []byte(data)

		switch eventType {
		case "response.created":
			var evt OAIResponseCreated
			if json.Unmarshal(raw, &evt) == nil {
				responseID = evt.Response.ID
			}

		case "response.content_part.added":
			// Will be filled by text deltas.

		case "response.output_text.delta":
			var evt OAIOutputTextDelta
			if json.Unmarshal(raw, &evt) == nil {
				// Append to last text block or create one.
				if n := len(contentBlocks); n > 0 {
					if tb, ok := contentBlocks[n-1].(map[string]interface{}); ok && tb["type"] == "text" {
						tb["text"] = tb["text"].(string) + evt.Delta
						continue
					}
				}
				contentBlocks = append(contentBlocks, map[string]interface{}{
					"type": "text",
					"text": evt.Delta,
				})
			}

		case "response.output_text.done":
			// Already accumulated.

		case "response.output_item.added":
			var evt OAIOutputItemAdded
			if json.Unmarshal(raw, &evt) == nil && evt.Item.Type == "function_call" {
				hasToolUse = true
				currentToolID = evt.Item.ID
				currentToolName = evt.Item.Name
				currentToolArgs.Reset()
			}

		case "response.function_call_arguments.delta":
			var evt OAIFuncCallArgsDelta
			if json.Unmarshal(raw, &evt) == nil {
				currentToolArgs.WriteString(evt.Delta)
			}

		case "response.function_call_arguments.done":
			var evt OAIFuncCallArgsDone
			if json.Unmarshal(raw, &evt) == nil {
				args := currentToolArgs.String()
				if args == "" {
					args = evt.Arguments
				}
				var inputObj interface{}
				if json.Unmarshal([]byte(args), &inputObj) != nil {
					inputObj = map[string]interface{}{}
				}
				// Prefer state from output_item.added, fall back to done event fields.
				toolID := currentToolID
				if toolID == "" {
					toolID = evt.CallID
				}
				toolName := currentToolName
				if toolName == "" {
					toolName = evt.Name
				}
				contentBlocks = append(contentBlocks, map[string]interface{}{
					"type":  "tool_use",
					"id":    toolID,
					"name":  toolName,
					"input": inputObj,
				})
				currentToolID = ""
				currentToolName = ""
				currentToolArgs.Reset()
			}

		case "response.completed":
			var evt OAIResponseCompleted
			if json.Unmarshal(raw, &evt) == nil {
				responseID = evt.Response.ID
				if evt.Response.Usage != nil {
					inputTokens = evt.Response.Usage.InputTokens
					outputTokens = evt.Response.Usage.OutputTokens
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	if hasToolUse {
		stopReason = "tool_use"
	}

	msg := map[string]interface{}{
		"id":            responseID,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       contentBlocks,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         anthropicUsage(inputTokens, outputTokens),
	}

	return &AccumulatedMessage{ResponseID: responseID, Message: msg}, nil
}

func (c *StreamConverter) writeError(msg string) error {
	return c.writeSSE("error", map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    "api_error",
			"message": msg,
		},
	})
}

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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CodexBackend implements ChatBackend for the Codex CLI using app-server
// (JSON-RPC 2.0 over stdio) for real streaming support.
type CodexBackend struct {
	logger *slog.Logger
}

func NewCodexBackend(logger *slog.Logger) *CodexBackend {
	return &CodexBackend{logger: logger}
}

func (b *CodexBackend) Name() string { return "codex" }

func (b *CodexBackend) Available() bool {
	_, err := exec.LookPath("codex")
	return err == nil
}

func (b *CodexBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, error) {
	codexPath, err := exec.LookPath("codex")
	if err != nil {
		return nil, fmt.Errorf("codex not found in PATH")
	}

	dir := agentDir(agent.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create agent dir: %w", err)
	}

	cmd := exec.CommandContext(ctx, codexPath, "app-server")
	cmd.Dir = dir
	cmd.Env = filterEnv([]string{"AGENT_BROWSER_SESSION", "AGENT_BROWSER_COOKIE_DIR"}, agent.ID, dir)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderrBuf, limit: 4096}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start codex app-server: %w", err)
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

		// JSON-RPC message sender (mutex-protected since stdin is shared)
		var reqID atomic.Int64
		var writeMu sync.Mutex
		sendRPC := func(method string, params any) int64 {
			id := reqID.Add(1)
			msg := rpcRequest{
				JSONRPC: "2.0",
				Method:  method,
				ID:      &id,
				Params:  params,
			}
			data, _ := json.Marshal(msg)
			data = append(data, '\n')
			writeMu.Lock()
			stdin.Write(data)
			writeMu.Unlock()
			b.logger.Debug("codex rpc send", "method", method, "id", id)
			return id
		}

		sendNotify := func(method string) {
			msg := struct {
				JSONRPC string `json:"jsonrpc"`
				Method  string `json:"method"`
			}{"2.0", method}
			data, _ := json.Marshal(msg)
			data = append(data, '\n')
			writeMu.Lock()
			stdin.Write(data)
			writeMu.Unlock()
			b.logger.Debug("codex rpc notify", "method", method)
		}

		shutdown := func() {
			stdin.Close()
			done := make(chan struct{})
			go func() {
				cmd.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				cmd.Process.Kill()
				<-done
			}
		}

		// Step 1: Initialize handshake
		initID := sendRPC("initialize", map[string]any{
			"clientInfo": map[string]any{
				"name":    "kojo",
				"title":   "Kojo",
				"version": "1.0.0",
			},
			"capabilities": map[string]any{
				"experimentalApi": true,
			},
		})

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		// Wait for initialize response
		var threadStartID, turnStartID int64
		var threadID string
		initDone := false
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			var msg rpcMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}
			if msg.ID != nil && *msg.ID == initID {
				if msg.Error != nil {
					send(ChatEvent{Type: "error", ErrorMessage: "codex initialize failed: " + msg.Error.Message})
					shutdown()
					return
				}
				initDone = true
				break
			}
		}
		if !initDone {
			send(ChatEvent{Type: "error", ErrorMessage: "codex app-server initialize failed"})
			shutdown()
			return
		}

		// Step 2: Send initialized notification (no params per protocol)
		sendNotify("initialized")

		// Step 3: Start thread
		threadParams := map[string]any{
			"cwd":            dir,
			"approvalPolicy": "never",
			"sandbox":        "danger-full-access",
		}
		if agent.Model != "" {
			threadParams["model"] = agent.Model
		}
		threadStartID = sendRPC("thread/start", threadParams)

		// Wait for thread/start response to get thread ID
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			var msg rpcMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}
			if msg.ID != nil && *msg.ID == threadStartID {
				if msg.Error != nil {
					send(ChatEvent{Type: "error", ErrorMessage: "codex thread/start failed: " + msg.Error.Message})
					shutdown()
					return
				}
				if msg.Result != nil {
					var result struct {
						Thread struct {
							ID string `json:"id"`
						} `json:"thread"`
					}
					json.Unmarshal(*msg.Result, &result)
					threadID = result.Thread.ID
				}
				break
			}
		}

		if threadID == "" {
			send(ChatEvent{Type: "error", ErrorMessage: "codex app-server: failed to get thread ID"})
			shutdown()
			return
		}

		// Step 4: Start turn with user message
		fullMessage := userMessage
		if systemPrompt != "" {
			fullMessage = systemPrompt + "\n\n---\n\n" + userMessage
		}

		turnStartID = sendRPC("turn/start", map[string]any{
			"threadId": threadID,
			"input": []map[string]any{
				{"type": "text", "text": fullMessage},
			},
		})

		if !send(ChatEvent{Type: "status", Status: "thinking"}) {
			shutdown()
			return
		}

		// Step 5: Process streaming events
		result := parseCodexStream(scanner, turnStartID, b.logger, send)
		if result.cancelled {
			shutdown()
			return
		}
		if result.turnCompleted {
			send(ChatEvent{Type: "done", Message: result.buildMessage(), Usage: result.usage, ErrorMessage: result.processError})
			shutdown()
			return
		}

		// Scanner exited without turn/completed — abnormal exit
		if err := scanner.Err(); err != nil {
			b.logger.Warn("codex app-server scanner error", "err", err)
		}

		var processError string
		if err := cmd.Wait(); err != nil {
			b.logger.Warn("codex app-server exited with error", "err", err, "stderr", stderrBuf.String())
			processError = strings.TrimSpace(stderrBuf.String())
			if processError == "" {
				processError = err.Error()
			}
		}

		errMsg := processError
		if errMsg == "" {
			errMsg = "codex app-server exited unexpectedly"
		}

		if result.hasOutput() {
			send(ChatEvent{Type: "done", Message: result.buildMessage(), Usage: result.usage, ErrorMessage: errMsg})
		} else {
			send(ChatEvent{Type: "error", ErrorMessage: errMsg})
		}
	}()

	return ch, nil
}

// codexStreamResult holds the accumulated state from parsing a Codex stream.
type codexStreamResult struct {
	fullText      strings.Builder
	thinking      strings.Builder
	toolUses      []ToolUse
	usage         *Usage
	processError  string // non-empty if turn/completed reported an error
	turnCompleted bool   // true if turn/completed was received
	cancelled     bool   // true if send returned false (context cancelled)
}

// buildMessage creates a Message from accumulated stream data.
func (r *codexStreamResult) buildMessage() *Message {
	msg := newAssistantMessage()
	msg.Content = r.fullText.String()
	msg.Thinking = r.thinking.String()
	msg.ToolUses = r.toolUses
	msg.Usage = r.usage
	return msg
}

// hasOutput returns true if the stream produced any text or tool uses.
func (r *codexStreamResult) hasOutput() bool {
	return r.fullText.Len() > 0 || len(r.toolUses) > 0
}

// parseCodexStream reads Codex app-server JSON-RPC notifications from a scanner
// and emits ChatEvents via the send callback. Returns the accumulated result.
// If send returns false (context cancelled), parsing stops immediately.
func parseCodexStream(scanner *bufio.Scanner, turnStartID int64, logger *slog.Logger, send func(ChatEvent) bool) *codexStreamResult {
	res := &codexStreamResult{}
	itemPhases := make(map[string]string) // itemID -> phase ("commentary" or "final_answer")

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var msg rpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			logger.Debug("codex rpc parse error", "line", line, "err", err)
			continue
		}

		// Handle RPC response errors
		if msg.ID != nil {
			if *msg.ID == turnStartID && msg.Error != nil {
				send(ChatEvent{Type: "error", ErrorMessage: msg.Error.Message})
				res.cancelled = true
				return res
			}
			continue
		}

		if res.handleNotification(&msg, itemPhases, logger, send) {
			return res
		}
	}

	return res
}

// handleNotification processes a single JSON-RPC notification.
// Returns true if the stream should stop (turn completed or cancelled).
func (res *codexStreamResult) handleNotification(msg *rpcMessage, itemPhases map[string]string, logger *slog.Logger, send func(ChatEvent) bool) bool {
	switch msg.Method {
	case "item/started":
		return res.handleItemStarted(msg, itemPhases, send)

	case "item/agentMessage/delta":
		return res.handleAgentMessageDelta(msg, itemPhases, send)

	case "item/reasoning/summaryTextDelta", "item/reasoning/textDelta":
		if msg.Params == nil {
			return false
		}
		var params struct {
			Delta string `json:"delta"`
		}
		json.Unmarshal(*msg.Params, &params)
		if params.Delta != "" {
			res.thinking.WriteString(params.Delta)
			send(ChatEvent{Type: "thinking", Delta: params.Delta})
		}
		return false

	case "item/completed":
		return res.handleItemCompleted(msg, send)

	case "thread/tokenUsage/updated":
		if msg.Params == nil {
			return false
		}
		var params struct {
			TokenUsage struct {
				Last struct {
					InputTokens  int `json:"inputTokens"`
					OutputTokens int `json:"outputTokens"`
				} `json:"last"`
			} `json:"tokenUsage"`
		}
		json.Unmarshal(*msg.Params, &params)
		if params.TokenUsage.Last.OutputTokens > 0 {
			res.usage = &Usage{
				InputTokens:  params.TokenUsage.Last.InputTokens,
				OutputTokens: params.TokenUsage.Last.OutputTokens,
			}
		}
		return false

	case "turn/completed":
		res.turnCompleted = true
		if msg.Params != nil {
			var params struct {
				Turn struct {
					Status string    `json:"status"`
					Error  *rpcError `json:"error"`
				} `json:"turn"`
			}
			json.Unmarshal(*msg.Params, &params)
			if params.Turn.Status == "failed" || params.Turn.Status == "interrupted" {
				res.processError = "codex turn " + params.Turn.Status
				if params.Turn.Error != nil {
					res.processError = params.Turn.Error.Message
				}
			}
		}
		return true
	}
	return false
}

func (res *codexStreamResult) handleItemStarted(msg *rpcMessage, itemPhases map[string]string, send func(ChatEvent) bool) bool {
	if msg.Params == nil {
		return false
	}
	var params struct {
		Item struct {
			ID        string          `json:"id"`
			Type      string          `json:"type"`
			Phase     string          `json:"phase"`
			Command   string          `json:"command"`
			Tool      string          `json:"tool"`
			Server    string          `json:"server"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"item"`
	}
	json.Unmarshal(*msg.Params, &params)

	if params.Item.Phase != "" {
		itemPhases[params.Item.ID] = params.Item.Phase
	}

	switch params.Item.Type {
	case "commandExecution":
		input := params.Item.Command
		res.toolUses = append(res.toolUses, ToolUse{
			ID:    params.Item.ID,
			Name:  "shell",
			Input: input,
		})
		if !send(ChatEvent{Type: "tool_use", ToolUseID: params.Item.ID, ToolName: "shell", ToolInput: input}) {
			res.cancelled = true
			return true
		}
	case "mcpToolCall", "dynamicToolCall":
		toolName := params.Item.Tool
		if params.Item.Server != "" {
			toolName = params.Item.Server + "/" + toolName
		}
		input := string(params.Item.Arguments)
		res.toolUses = append(res.toolUses, ToolUse{
			ID:    params.Item.ID,
			Name:  toolName,
			Input: input,
		})
		if !send(ChatEvent{Type: "tool_use", ToolUseID: params.Item.ID, ToolName: toolName, ToolInput: input}) {
			res.cancelled = true
			return true
		}
	}
	return false
}

func (res *codexStreamResult) handleAgentMessageDelta(msg *rpcMessage, itemPhases map[string]string, send func(ChatEvent) bool) bool {
	if msg.Params == nil {
		return false
	}
	var params struct {
		ItemID string `json:"itemId"`
		Delta  string `json:"delta"`
	}
	json.Unmarshal(*msg.Params, &params)
	if params.Delta == "" {
		return false
	}

	phase := itemPhases[params.ItemID]
	if phase == "commentary" {
		res.thinking.WriteString(params.Delta)
		if !send(ChatEvent{Type: "thinking", Delta: params.Delta}) {
			res.cancelled = true
			return true
		}
	} else {
		res.fullText.WriteString(params.Delta)
		if !send(ChatEvent{Type: "text", Delta: params.Delta}) {
			res.cancelled = true
			return true
		}
	}
	return false
}

func (res *codexStreamResult) handleItemCompleted(msg *rpcMessage, send func(ChatEvent) bool) bool {
	if msg.Params == nil {
		return false
	}
	var params struct {
		Item struct {
			ID               string          `json:"id"`
			Type             string          `json:"type"`
			Text             string          `json:"text"`
			Status           string          `json:"status"`
			Command          string          `json:"command"`
			AggregatedOutput string          `json:"aggregatedOutput"`
			ExitCode         *int            `json:"exitCode"`
			Tool             string          `json:"tool"`
			Server           string          `json:"server"`
			Result           json.RawMessage `json:"result"`
			Error            *struct {
				Message string `json:"message"`
			} `json:"error"`
			ContentItems json.RawMessage `json:"contentItems"`
			Success      *bool           `json:"success"`
		} `json:"item"`
	}
	json.Unmarshal(*msg.Params, &params)

	switch params.Item.Type {
	case "commandExecution":
		output := params.Item.AggregatedOutput
		if output == "" && params.Item.ExitCode != nil && *params.Item.ExitCode != 0 {
			output = fmt.Sprintf("exit code: %d", *params.Item.ExitCode)
		}
		toolName := "shell"
		if !send(ChatEvent{Type: "tool_result", ToolUseID: params.Item.ID, ToolName: toolName, ToolOutput: output}) {
			res.cancelled = true
			return true
		}
		matchToolOutput(res.toolUses, params.Item.ID, toolName, output)
	case "mcpToolCall":
		var output string
		if params.Item.Error != nil {
			output = "error: " + params.Item.Error.Message
		} else if len(params.Item.Result) > 0 && string(params.Item.Result) != "null" {
			output = string(params.Item.Result)
		}
		toolName := params.Item.Tool
		if params.Item.Server != "" {
			toolName = params.Item.Server + "/" + toolName
		}
		if !send(ChatEvent{Type: "tool_result", ToolUseID: params.Item.ID, ToolName: toolName, ToolOutput: output}) {
			res.cancelled = true
			return true
		}
		matchToolOutput(res.toolUses, params.Item.ID, toolName, output)
	case "dynamicToolCall":
		var output string
		if len(params.Item.ContentItems) > 0 && string(params.Item.ContentItems) != "null" {
			output = string(params.Item.ContentItems)
		} else if params.Item.Success != nil && !*params.Item.Success {
			output = "failed"
		}
		toolName := params.Item.Tool
		if !send(ChatEvent{Type: "tool_result", ToolUseID: params.Item.ID, ToolName: toolName, ToolOutput: output}) {
			res.cancelled = true
			return true
		}
		matchToolOutput(res.toolUses, params.Item.ID, toolName, output)
	}
	return false
}

// rpcRequest is a JSON-RPC 2.0 request.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	ID      *int64 `json:"id,omitempty"`
	Params  any    `json:"params,omitempty"`
}

// rpcMessage is a generic JSON-RPC 2.0 message (response or notification).
type rpcMessage struct {
	JSONRPC string           `json:"jsonrpc,omitempty"`
	Method  string           `json:"method,omitempty"`
	ID      *int64           `json:"id,omitempty"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
	Params  *json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}


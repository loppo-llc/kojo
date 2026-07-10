package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/loppo-llc/kojo/internal/chathistory"
)

// The codex app-server JSON-RPC stream is read via jsonlLineScanner (see
// jsonl_scanner.go) in strict mode: an oversized line means the RPC framing
// is broken and continuing is unsafe, so it surfaces as a fatal
// chathistory.ErrLineTooLarge (rendered by codexReadErrorMessage).

func codexReadErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, chathistory.ErrLineTooLarge) {
		return fmt.Sprintf("codex app-server emitted a JSON-RPC line over %d bytes; refusing to buffer it", chathistory.MaxJSONLLineBytes)
	}
	return "codex app-server read error: " + err.Error()
}

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

	args := []string{"app-server"}
	for name, srv := range opts.MCPServers {
		args = append(args, "-c", fmt.Sprintf("mcp_servers.%s.url=%q", name, srv.URL))
		// Codex's streamable HTTP MCP transport doesn't accept arbitrary request
		// headers (`mcp_servers.<name>.http_headers` is rejected as an invalid
		// transport); it only supports a bearer token read from an env var via
		// `bearer_token_env_var`. kojo's auth middleware (extractBearer) accepts
		// the per-agent token from either `X-Kojo-Token` or `Authorization:
		// Bearer`, and filterEnv already exports that exact token into the codex
		// process env as KOJO_AGENT_TOKEN, so point Codex at it. Without this the
		// /mcp call lands as a Guest principal (403) and Codex silently drops the
		// server from its tool list.
		if srv.Headers["X-Kojo-Token"] != "" {
			args = append(args, "-c", fmt.Sprintf("mcp_servers.%s.bearer_token_env_var=%q", name, "KOJO_AGENT_TOKEN"))
		}
	}
	cmd := exec.CommandContext(ctx, codexPath, args...)
	cmd.Dir = dir
	cmd.Env = filterEnv([]string{"AGENT_BROWSER_SESSION", "AGENT_BROWSER_COOKIE_DIR"}, agent.ID, dir)
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second

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

		send := func(e ChatEvent) bool { return ctxSend(ctx, ch, e) }

		// JSON-RPC message sender (mutex-protected since stdin is shared)
		var reqID atomic.Int64
		var writeMu sync.Mutex
		writeLine := func(msg any) error {
			data, err := json.Marshal(msg)
			if err != nil {
				return err
			}
			data = append(data, '\n')
			writeMu.Lock()
			defer writeMu.Unlock()
			_, werr := stdin.Write(data)
			return werr
		}
		sendRPCErr := func(method string, params any) (int64, error) {
			id := reqID.Add(1)
			err := writeLine(rpcRequest{
				JSONRPC: "2.0",
				Method:  method,
				ID:      &id,
				Params:  params,
			})
			b.logger.Debug("codex rpc send", "method", method, "id", id, "err", err)
			return id, err
		}
		sendRPC := func(method string, params any) int64 {
			id, _ := sendRPCErr(method, params)
			return id
		}

		sendNotify := func(method string) {
			writeLine(struct {
				JSONRPC string `json:"jsonrpc"`
				Method  string `json:"method"`
			}{"2.0", method})
			b.logger.Debug("codex rpc notify", "method", method)
		}

		shutdown := func() error {
			stdin.Close()
			var waitErr error
			done := make(chan struct{})
			go func() {
				waitErr = cmd.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				cmd.Process.Kill()
				<-done
			}
			return waitErr
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

		scanner := newCodexLineScanner(stdout)

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
			errMsg := "codex app-server initialize failed"
			if err := scanner.Err(); err != nil {
				errMsg = codexReadErrorMessage(err)
			}
			send(ChatEvent{Type: "error", ErrorMessage: errMsg})
			shutdown()
			return
		}

		// Step 2: Send initialized notification (no params per protocol)
		sendNotify("initialized")

		// Step 3: Start or resume thread.
		//
		// systemPrompt (already merged with any SystemPromptExtra by the
		// manager) flows into Codex's baseInstructions — set once at
		// thread/start / thread/resume — rather than being concatenated
		// onto the user message. This keeps Codex's prompt cache stable across turns:
		// the base instructions form a fixed prefix, and only the per-turn
		// user message changes. Mixing the system prompt into each turn's
		// input would invalidate the cache and force full re-tokenisation
		// on every reply.
		threadParams := map[string]any{
			"cwd":            dir,
			"approvalPolicy": "never",
			"sandbox":        "danger-full-access",
		}
		if opts.OneShot {
			threadParams["ephemeral"] = true
		}
		if agent.Model != "" {
			threadParams["model"] = agent.Model
		}
		if systemPrompt != "" {
			threadParams["baseInstructions"] = systemPrompt
		}

		var rolloutPath string
		var existingRef *codexThreadRef
		if !opts.OneShot {
			if ref, rerr := readCodexThreadRef(agent.ID, opts.SessionKey); rerr == nil && ref != nil && ref.ThreadID != "" {
				existingRef = ref
				resumeParams := cloneStringAnyMap(threadParams)
				resumeParams["threadId"] = ref.ThreadID
				threadStartID = sendRPC("thread/resume", resumeParams)
				msg, ok := waitCodexRPCResponse(scanner, threadStartID)
				if !ok {
					errMsg := "codex thread/resume failed: no response"
					if err := scanner.Err(); err != nil {
						errMsg = "codex thread/resume failed: " + codexReadErrorMessage(err)
					}
					send(ChatEvent{Type: "error", ErrorMessage: errMsg})
					shutdown()
					return
				}
				if msg.Error != nil {
					b.logger.Warn("codex thread/resume failed; starting a fresh thread",
						"agent", agent.ID, "sessionKey", opts.SessionKey,
						"thread_id", ref.ThreadID, "err", msg.Error.Message)
					deleteCodexThreadRef(agent.ID, opts.SessionKey, b.logger)
				} else {
					threadID, rolloutPath = decodeCodexThreadResult(msg.Result)
					if rolloutPath == "" {
						rolloutPath = ref.RolloutPath
					}
				}
			} else if rerr != nil {
				b.logger.Warn("codex thread ref read failed; starting a fresh thread",
					"agent", agent.ID, "sessionKey", opts.SessionKey, "err", rerr)
			}
		}

		if threadID == "" {
			threadStartID = sendRPC("thread/start", threadParams)
			msg, ok := waitCodexRPCResponse(scanner, threadStartID)
			if !ok {
				errMsg := "codex thread/start failed: no response"
				if err := scanner.Err(); err != nil {
					errMsg = "codex thread/start failed: " + codexReadErrorMessage(err)
				}
				send(ChatEvent{Type: "error", ErrorMessage: errMsg})
				shutdown()
				return
			}
			if msg.Error != nil {
				send(ChatEvent{Type: "error", ErrorMessage: "codex thread/start failed: " + msg.Error.Message})
				shutdown()
				return
			}
			threadID, rolloutPath = decodeCodexThreadResult(msg.Result)
		}

		if threadID == "" {
			send(ChatEvent{Type: "error", ErrorMessage: "codex app-server: failed to get thread ID"})
			shutdown()
			return
		}
		if !opts.OneShot {
			if rolloutPath == "" && existingRef != nil {
				rolloutPath = existingRef.RolloutPath
			}
			writeCodexThreadRef(agent.ID, opts.SessionKey, codexThreadRef{
				ThreadID:    threadID,
				RolloutPath: rolloutPath,
			}, b.logger)
		}

		// Step 4: Start turn with user message.
		// System prompt is NOT prepended here — it flows through
		// baseInstructions above so the prompt cache stays warm across turns.
		turnParams := map[string]any{
			"threadId": threadID,
			"input": []map[string]any{
				{"type": "text", "text": userMessage},
			},
		}
		if effort := codexEffortForProtocol(agent.Model, agent.Effort); effort != "" {
			turnParams["effort"] = effort
		} else if agent.Effort != "" {
			b.logger.Warn("codex: unsupported effort value; using CLI default",
				"agent", agent.ID, "effort", agent.Effort)
		}
		turnStartID = sendRPC("turn/start", turnParams)

		// Steering: turn/steer injects extra user input into the running
		// turn. It needs the active turn id (captured from the turn/start
		// response by parseCodexStream), so the steerer blocks steer calls
		// until that id lands.
		var steerer *codexSteerer
		if opts.OnSteerReady != nil {
			steerer = newCodexSteerer(threadID, sendRPCErr)
			defer steerer.close()
			opts.OnSteerReady(steerer.steer)
		}

		if !send(ChatEvent{Type: "status", Status: "thinking"}) {
			shutdown()
			return
		}

		// Step 5: Process streaming events
		result := parseCodexStream(scanner, turnStartID, steerer, b.logger, send)
		if steerer != nil {
			// The turn is over (or the stream broke) — refuse further
			// steers now rather than at goroutine exit, so a late steer
			// doesn't get written into a dead turn and silently dropped.
			steerer.close()
		}
		if result.cancelled {
			shutdown()
			if ctx.Err() != nil {
				emitCancelDone(ctx, ch, result.fullText.String(), result.thinking.String(), result.toolUses, result.usage)
			}
			return
		}
		if result.turnCompleted {
			send(ChatEvent{Type: "done", Message: result.buildMessage(), Usage: result.usage, ErrorMessage: result.processError})
			shutdown()
			return
		}

		// Stream ended without turn/completed — abnormal exit.
		scannerErr := scanner.Err()
		if scannerErr != nil {
			b.logger.Warn("codex app-server scanner error", "err", scannerErr)
		}

		// Reap the process via shutdown() rather than calling cmd.Wait()
		// directly. Codex app-server is a persistent JSON-RPC server: it does
		// not exit just because we stopped reading its stdout, so a bare
		// cmd.Wait() here would block forever — leaking the process and hanging
		// the Slack turn with no terminal event. shutdown() closes stdin to
		// request a clean exit, then force-kills after a grace period, so we
		// always reach the done/error send below.
		waitErr := shutdown()

		processError := ""
		if scannerErr != nil {
			processError = codexReadErrorMessage(scannerErr)
		}
		if processError == "" {
			processError = strings.TrimSpace(stderrBuf.String())
		}
		if processError == "" && waitErr != nil {
			processError = waitErr.Error()
		}
		if waitErr != nil || processError != "" {
			b.logger.Warn("codex app-server exited abnormally", "err", waitErr, "stderr", stderrBuf.String())
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
	return assembleAssistantMessage(r.fullText.String(), r.thinking.String(), r.toolUses, r.usage)
}

// hasOutput returns true if the stream produced any text or tool uses.
func (r *codexStreamResult) hasOutput() bool {
	return r.fullText.Len() > 0 || len(r.toolUses) > 0
}

// parseCodexStream reads Codex app-server JSON-RPC notifications from a scanner
// and emits ChatEvents via the send callback. Returns the accumulated result.
// If send returns false (context cancelled), parsing stops immediately.
// steer may be nil; when set, the active turn id from the turn/start
// response (or the turn/started notification) is forwarded to it so
// mid-turn turn/steer requests can be issued.
func parseCodexStream(scanner *jsonlLineScanner, turnStartID int64, steer *codexSteerer, logger *slog.Logger, send func(ChatEvent) bool) *codexStreamResult {
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

		// Handle RPC responses
		if msg.ID != nil {
			if *msg.ID == turnStartID {
				if msg.Error != nil {
					send(ChatEvent{Type: "error", ErrorMessage: msg.Error.Message})
					res.cancelled = true
					return res
				}
				if steer != nil {
					steer.setTurnID(decodeCodexTurnID(msg.Result))
				}
			} else if steer != nil && steer.resolve(*msg.ID, msg.Error) {
				// turn/steer response — delivered to the waiting steer
				// call. Log rejections for the record.
				if msg.Error != nil {
					logger.Warn("codex turn/steer rejected", "err", msg.Error.Message)
				}
			}
			continue
		}

		if steer != nil && msg.Method == "turn/started" && msg.Params != nil {
			// Fallback capture in case the turn/start response was missed.
			var params struct {
				Turn struct {
					ID string `json:"id"`
				} `json:"turn"`
			}
			json.Unmarshal(*msg.Params, &params)
			steer.setTurnID(params.Turn.ID)
		}

		if res.handleNotification(&msg, itemPhases, logger, send) {
			return res
		}
	}

	return res
}

// decodeCodexTurnID extracts the turn id from a turn/start RPC response,
// accepting both response shapes ({"turn":{"id":...}} and {"turnId":...}).
func decodeCodexTurnID(result *json.RawMessage) string {
	if result == nil {
		return ""
	}
	var r struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
		TurnID string `json:"turnId"`
	}
	json.Unmarshal(*result, &r)
	if r.Turn.ID != "" {
		return r.Turn.ID
	}
	return r.TurnID
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

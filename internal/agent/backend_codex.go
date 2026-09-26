package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/loppo-llc/kojo/internal/chathistory"
)

// The codex app-server JSON-RPC stream is read via jsonlLineScanner (see
// jsonl_scanner.go) in strict mode: an oversized line means the RPC framing
// is broken and continuing is unsafe, so it surfaces as a fatal
// chathistory.ErrLineTooLarge (rendered by codexReadErrorMessage).

const codexEmptyCompletionMaxRetries = 2

const codexEmptyCompletionRetryPrompt = "[automatic recovery] The previous Codex turn reported successful completion without producing a final assistant response. Continue the original request from the current thread and filesystem state. Do not repeat work that is already complete. Finish the remaining work, then provide a non-empty final response."

const codexEmptyCompletionError = "codex turn completed without a final assistant response after automatic retries"

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

	// extraConfig holds additional `-c key=value` overrides prepended to
	// every `codex app-server` invocation. Empty for the stock codex
	// backend; CustomCodexBackend uses it to point the CLI at an
	// operator-supplied OpenAI-compatible endpoint.
	extraConfig []string
}

func NewCodexBackend(logger *slog.Logger) *CodexBackend {
	return &CodexBackend{logger: logger}
}

func (b *CodexBackend) Name() string { return ToolCodex }

// SetConfigOverrides replaces the `-c key=value` overrides applied to the
// app-server invocation. Values must already be TOML-encoded (codex parses
// the right-hand side as TOML and falls back to a literal string).
func (b *CodexBackend) SetConfigOverrides(kv []string) {
	b.extraConfig = append([]string(nil), kv...)
}

func (b *CodexBackend) Available() bool {
	_, err := exec.LookPath("codex")
	return err == nil
}

func (b *CodexBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, error) {
	if err := checkGoalHandoffAdmission(agent.ID, opts.SessionKey, opts.Goal); err != nil {
		return nil, err
	}
	if err := opts.Goal.Validate(); err != nil {
		return nil, err
	}
	if opts.Goal != nil && opts.OneShot {
		return nil, errors.New("native goals require a persistent conversation")
	}
	goalKey := codexThreadRefPath(agent.ID, opts.SessionKey)
	runtime := &codexGoalRuntime{isGoal: opts.Goal != nil, runID: opts.GoalRunID, origin: opts.OriginPeerID, userID: opts.GoalUserID, agentID: agent.ID, key: opts.SessionKey, pending: make(map[int64]chan *rpcMessage)}
	if opts.Goal != nil {
		runtime.resumeHandoffID = opts.Goal.ExpectedHandoffID
	}
	if !opts.OneShot {
		if old, loaded := codexGoalRuntimes.LoadOrStore(goalKey, runtime); loaded {
			if opts.Goal == nil {
				return nil, ErrAgentBusy
			}
			g, err := old.(*codexGoalRuntime).control(ctx, opts.Goal)
			if err != nil {
				return nil, err
			}
			return goalControlEvents(g, agent.ID, opts.SessionKey), nil
		}
	}
	launched := false
	defer func() {
		if !launched && !opts.OneShot {
			codexGoalRuntimes.CompareAndDelete(goalKey, runtime)
		}
	}()
	codexPath, err := exec.LookPath("codex")
	if err != nil {
		return nil, fmt.Errorf("codex not found in PATH")
	}

	dir := agentDir(agent.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create agent dir: %w", err)
	}

	args := []string{"app-server"}
	binding, bindingErr := goalBindingFor(agent.ID, opts.SessionKey)
	if bindingErr != nil && (opts.Goal != nil || opts.ResumeGoalOnReply) {
		return nil, bindingErr
	}
	if opts.Goal != nil || binding != nil {
		args = append(args, "-c", "features.goals=true")
		if effort := codexEffortForProtocol(agent.Model, agent.Effort); effort != "" {
			args = append(args, "-c", "model_reasoning_effort="+tomlString(effort))
		}
	} else {
		// Only explicitly enabled conversations may create native goals.
		args = append(args, "-c", "features.goals=false")
	}
	for _, kv := range b.extraConfig {
		args = append(args, "-c", kv)
	}
	// Default mode otherwise rejects request_user_input before emitting its
	// server request. Enable it only when this caller can answer questions;
	// keep execution in Default mode (Plan mode would prevent normal work).
	if opts.OnQuestionReady != nil {
		args = append(args, "-c", "features.default_mode_request_user_input=true")
	}
	for name, srv := range opts.MCPServers {
		if srv.isStdio() {
			// Extension-contributed stdio server. Codex spawns it
			// itself, so it needs the command, its argv and the
			// KOJO_EXT_* environment as TOML config overrides.
			args = append(args, "-c", fmt.Sprintf("mcp_servers.%s.command=%s", name, tomlString(srv.Command)))
			if len(srv.Args) > 0 {
				args = append(args, "-c", fmt.Sprintf("mcp_servers.%s.args=%s", name, tomlStringArray(srv.Args)))
			}
			if len(srv.Env) > 0 {
				args = append(args, "-c", fmt.Sprintf("mcp_servers.%s.env=%s", name, tomlStringTable(srv.Env)))
			}
			continue
		}
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
	cmd.Env = appendKojoTurnEnv(cmd.Env, opts)
	cmd.Cancel = func() error {
		// Persist intent before killing a possibly unresponsive CLI. A later
		// thread/resume reconciles this fence before it can resume native goals.
		runtime.mu.Lock()
		stopped := runtime.stopRequested
		isGoal := runtime.isGoal
		runtime.mu.Unlock()
		preserve := opts.PreserveGoalOnCancel != nil && opts.PreserveGoalOnCancel()
		if opts.GoalRunID != "" && isGoal {
			preserve = true
		}
		if !opts.OneShot && (isGoal || (binding != nil && binding.State != nil)) {
			_ = updateGoalBinding(agent.ID, opts.SessionKey, func(b *GoalBinding) {
				if b.State != nil && b.State.Status != "complete" {
					if stopped || !preserve {
						cancelGoalHandoff(b, "goal execution cancelled")
						b.DesiredPaused = true
						b.Generation++
					}
					b.RecoveryPending = preserve && !stopped && !b.DesiredPaused
				}
			})
		}
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

	launched = true
	ch := make(chan ChatEvent, 64)

	go func() {
		defer close(ch)
		defer runtime.close()
		if !opts.OneShot {
			defer codexGoalRuntimes.CompareAndDelete(goalKey, runtime)
		}

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
			n, werr := stdin.Write(data)
			if werr == nil && n != len(data) {
				werr = io.ErrShortWrite
			}
			if werr != nil {
				return &codexRPCWriteError{Written: n, Err: werr}
			}
			return nil
		}
		fallbackResponder := newCodexServerRequestResponder(writeLine)
		respondServerRequest := codexServerRequestResponder(func(msg *rpcMessage) (string, error) {
			if msg.Method == "serverRequest/resolved" {
				return "resolved", nil
			}
			return fallbackResponder(msg)
		})
		var qs *codexQuestionState
		if opts.OnQuestionReady != nil {
			qs = newCodexQuestionState(writeLine, send, opts.OnQuestionResolved)
			qs.onWriteFailure = func() { _ = cmd.Process.Kill() }
			defer qs.close()
			opts.OnQuestionReady(qs.answer)
			fallback := respondServerRequest
			respondServerRequest = func(msg *rpcMessage) (string, error) {
				if msg.Method == "serverRequest/resolved" {
					var p struct {
						RequestID json.RawMessage `json:"requestId"`
					}
					if msg.Params != nil && json.Unmarshal(*msg.Params, &p) == nil {
						qs.resolveRPC(p.RequestID)
					}
					return "resolved", nil
				}
				if msg.Method == "item/tool/requestUserInput" {
					return qs.register(msg, opts.AutomatedTrigger)
				}
				return fallback(msg)
			}
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
		var threadStartID int64
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
			handled, err := handleCodexServerRequest(&msg, respondServerRequest, b.logger)
			if err != nil {
				send(ChatEvent{Type: "error", ErrorMessage: "codex server request handling failed: " + err.Error()})
				shutdown()
				return
			}
			if handled {
				continue
			}
			if id, ok := msg.numericID(); ok && id == initID {
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

		// Goal controls on an idle conversation operate on persisted native
		// state WITHOUT thread/resume (resuming an active goal can start work).
		refBefore, refErr := readCodexThreadRef(agent.ID, opts.SessionKey)
		if refErr != nil && (opts.Goal != nil || opts.ResumeGoalOnReply) {
			send(ChatEvent{Type: "error", ErrorMessage: refErr.Error()})
			shutdown()
			return
		}
		if opts.Goal != nil && opts.Goal.ExpectedGeneration != nil {
			if opts.Goal.ExpectedHandoffID != "" && !goalHandoffResumeAllowed(refBefore, opts.Goal) {
				send(ChatEvent{Type: "error", ErrorMessage: "goal handoff changed or was cancelled before resume"})
				shutdown()
				return
			}
			if refBefore == nil || refBefore.Goal == nil || (refBefore.Goal.DesiredPaused && opts.Goal.ExpectedHandoffID == "") || refBefore.ThreadID != opts.Goal.ExpectedThreadID || refBefore.Goal.Generation != *opts.Goal.ExpectedGeneration || (opts.Goal.ExpectedRunID != "" && opts.Goal.ExpectedRunID != refBefore.Goal.RunID) {
				send(ChatEvent{Type: "error", ErrorMessage: "goal changed or paused since recovery was scheduled"})
				shutdown()
				return
			}
		}
		if opts.Goal != nil && refBefore != nil && goalOperationSeen(refBefore.Goal, opts.Goal.OperationID) {
			send(ChatEvent{Type: "done", Message: assembleAssistantMessage(goalSummary(refBefore.Goal.State)+goalHandoffSummary(agent.ID, opts.SessionKey), "", nil, nil)})
			shutdown()
			return
		}
		controlOnly := opts.Goal != nil && opts.Goal.Action != "start" && opts.Goal.Action != "resume"
		if controlOnly {
			if refBefore == nil {
				send(ChatEvent{Type: "done", Message: assembleAssistantMessage("Goal: none.", "", nil, nil)})
				shutdown()
				return
			}
			if opts.Goal.Action == "pause" || opts.Goal.Action == "clear" {
				if err := updateGoalBinding(agent.ID, opts.SessionKey, func(b *GoalBinding) {
					b.DesiredPaused = true
					b.Generation++
					cancelGoalHandoff(b, "goal explicitly paused or cleared")
				}); err != nil {
					send(ChatEvent{Type: "error", ErrorMessage: err.Error()})
					shutdown()
					return
				}
			}
			method, params := goalRPC(opts.Goal, refBefore.ThreadID)
			id := sendRPC(method, params)
			msg, ok, err := waitCodexRPCResponse(scanner, id, respondServerRequest, b.logger)
			if err != nil || !ok || msg.Error != nil {
				detail := "goal API unavailable"
				if err != nil {
					detail = err.Error()
				} else if ok && msg.Error != nil {
					detail = msg.Error.Message
				}
				send(ChatEvent{Type: "error", ErrorMessage: detail})
				shutdown()
				return
			}
			goal := decodeGoal(msg.Result)
			if opts.Goal.Action == "clear" {
				goal = nil
			}
			if err := updateGoalBinding(agent.ID, opts.SessionKey, func(b *GoalBinding) { b.State = goal; rememberGoalOperation(b, opts.Goal.OperationID) }); err != nil {
				send(ChatEvent{Type: "error", ErrorMessage: err.Error()})
				shutdown()
				return
			}
			send(ChatEvent{Type: "goal", Goal: goal})
			send(ChatEvent{Type: "done", Message: assembleAssistantMessage(goalSummary(goal)+goalHandoffSummary(agent.ID, opts.SessionKey), "", nil, nil)})
			shutdown()
			return
		}
		if opts.Goal != nil && opts.Goal.Action == "resume" && (refBefore == nil || refBefore.Goal == nil || refBefore.Goal.State == nil) {
			send(ChatEvent{Type: "error", ErrorMessage: "no goal to resume"})
			shutdown()
			return
		}
		if refBefore != nil && refBefore.Goal != nil {
			// Never let resume activate a stored goal before this runner owns its
			// stream. Pausing preserves native usage accounting.
			id := sendRPC("thread/goal/get", map[string]any{"threadId": refBefore.ThreadID})
			msg, ok, err := waitCodexRPCResponse(scanner, id, respondServerRequest, b.logger)
			if err != nil || !ok || msg.Error != nil {
				send(ChatEvent{Type: "error", ErrorMessage: "cannot read native goal before resume"})
				shutdown()
				return
			}
			old := decodeGoal(msg.Result)
			if opts.ResumeGoalOnReply && opts.Goal == nil && goalResumesOnReply(refBefore.Goal, old) {
				opts.Goal = &GoalRequest{Action: "resume"}
				runtime.mu.Lock()
				runtime.isGoal = true
				runtime.mu.Unlock()
			}
			if opts.Goal != nil && opts.Goal.ExpectedGeneration != nil && (old == nil || (old.Status != "active" && !(old.Status == "paused" && (refBefore.Goal.ActivationPending || goalHandoffResumeAllowed(refBefore, opts.Goal))))) {
				if err := updateGoalBinding(agent.ID, opts.SessionKey, func(g *GoalBinding) { g.State = old }); err != nil {
					send(ChatEvent{Type: "error", ErrorMessage: err.Error()})
				} else {
					send(ChatEvent{Type: "done", Message: assembleAssistantMessage(goalSummary(old), "", nil, nil)})
				}
				shutdown()
				return
			}

			if opts.Goal != nil && opts.Goal.Action == "start" && old != nil && old.Status != "complete" {
				send(ChatEvent{Type: "error", ErrorMessage: "this conversation already has a goal; clear it before starting another"})
				shutdown()
				return
			}
			if opts.Goal != nil && opts.Goal.Action == "resume" && (old == nil || old.Status == "complete") {
				send(ChatEvent{Type: "error", ErrorMessage: "no unfinished goal to resume"})
				shutdown()
				return
			}
			if opts.Goal == nil && old != nil && old.Status == "active" && !refBefore.Goal.DesiredPaused {
				send(ChatEvent{Type: "error", ErrorMessage: "this conversation has an active goal without a runner; use !goal resume or !goal pause"})
				shutdown()
				return
			}
			if old != nil && old.Status == "active" {
				if opts.Goal != nil {
					if err := updateGoalBinding(agent.ID, opts.SessionKey, func(b *GoalBinding) { b.ActivationPending = true }); err != nil {
						send(ChatEvent{Type: "error", ErrorMessage: err.Error()})
						shutdown()
						return
					}
				}
				id = sendRPC("thread/goal/set", map[string]any{"threadId": refBefore.ThreadID, "status": "paused"})
				msg, ok, err = waitCodexRPCResponse(scanner, id, respondServerRequest, b.logger)
				if err != nil || !ok || msg.Error != nil {
					send(ChatEvent{Type: "error", ErrorMessage: "cannot pause native goal before resume"})
					shutdown()
					return
				}
			}
		}
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

		if opts.Goal != nil {
			setup := userMessage
			if opts.Goal.Action == "resume" && refBefore != nil && refBefore.Goal != nil {
				setup = refBefore.Goal.SetupContext
			}
			if len(setup) > 1<<20 {
				send(ChatEvent{Type: "error", ErrorMessage: "goal setup context exceeds 1 MiB"})
				shutdown()
				return
			}
			threadParams["baseInstructions"] = systemPrompt + "\n\nGoal setup context (reference data for the explicit goal):\n" + setup
		}
		var rolloutPath string
		var existingRef *codexThreadRef
		resumed := false
		if !opts.OneShot {
			if ref, rerr := readCodexThreadRef(agent.ID, opts.SessionKey); rerr == nil && ref != nil && ref.ThreadID != "" {
				existingRef = ref
				resumeParams := buildCodexResumeParams(threadParams, ref.ThreadID)
				threadStartID = sendRPC("thread/resume", resumeParams)
				msg, ok, waitErr := waitCodexRPCResponse(scanner, threadStartID, respondServerRequest, b.logger)
				if waitErr != nil {
					send(ChatEvent{Type: "error", ErrorMessage: "codex thread/resume failed: " + waitErr.Error()})
					shutdown()
					return
				}
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
					if ref.Goal != nil || opts.Goal != nil {
						send(ChatEvent{Type: "error", ErrorMessage: "goal thread resume failed: " + msg.Error.Message})
						shutdown()
						return
					}
					b.logger.Warn("codex thread/resume failed; starting a fresh thread",
						"agent", agent.ID, "sessionKey", opts.SessionKey,
						"thread_id", ref.ThreadID, "err", msg.Error.Message)
					deleteCodexThreadRef(agent.ID, opts.SessionKey, b.logger)
				} else {
					threadID, rolloutPath = decodeCodexThreadResult(msg.Result)
					resumed = threadID != ""
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
			msg, ok, waitErr := waitCodexRPCResponse(scanner, threadStartID, respondServerRequest, b.logger)
			if waitErr != nil {
				send(ChatEvent{Type: "error", ErrorMessage: "codex thread/start failed: " + waitErr.Error()})
				shutdown()
				return
			}
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
				Goal: func() *GoalBinding {
					if existingRef != nil {
						return existingRef.Goal
					}
					return nil
				}(),
			}, b.logger)
		}

		// Context fallback selection is centralized here because only the
		// backend knows whether thread/resume actually succeeded or
		// thread/start was used.
		userMessage = injectSessionHistoryContext(userMessage, opts.FreshSessionContext, opts.ResumeSessionContext, resumed)

		// Step 4: Start turn with user message.
		// System prompt is NOT prepended here — it flows through
		// baseInstructions above so the prompt cache stays warm across turns.
		effort := codexEffortForProtocol(agent.Model, agent.Effort)
		if effort == "" && agent.Effort != "" {
			b.logger.Warn("codex: unsupported effort value; using CLI default",
				"agent", agent.ID, "effort", agent.Effort)
		}
		startTurn := func(input string) (int64, error) {
			turnParams := map[string]any{
				"threadId": threadID,
				"input": []map[string]any{
					{"type": "text", "text": input},
				},
			}
			if effort != "" {
				turnParams["effort"] = effort
			}
			return sendRPCErr("turn/start", turnParams)
		}

		// Steering: turn/steer injects extra user input into the running
		// turn. It needs the active turn id (captured from the turn/start
		// response by parseCodexStream), so the steerer blocks steer calls
		// until that id lands.
		var steerer *codexSteerer
		if opts.OnSteerReady != nil || qs != nil {
			steerer = newCodexSteerer(threadID, sendRPCErr)
			defer steerer.close()
			if qs != nil {
				qs.steer = steerer.steer
			}
			if opts.Goal == nil && opts.OnSteerReady != nil {
				opts.OnSteerReady(func(text string) error {
					q, err := ParseGoalCommand(text)
					if err != nil {
						return err
					}
					if q != nil {
						return errors.New("wait for the ordinary turn to finish before changing its goal")
					}
					return steerer.steer(text)
				})
			}
		}

		if opts.Goal != nil {
			runtime.mu.Lock()
			runtime.write = sendRPCErr
			runtime.threadID = threadID
			runtime.mu.Unlock()
			if ctx.Err() != nil {
				shutdown()
				return
			}
			runtime.mu.Lock()
			stopped := runtime.stopRequested
			if stopped {
				runtime.mu.Unlock()
				shutdown()
				return
			}
			activationAdmitted := false
			if err := updateGoalBinding(agent.ID, opts.SessionKey, func(g *GoalBinding) {
				if ctx.Err() != nil {
					g.DesiredPaused = true
					return
				}
				if opts.Goal.Action == "start" {
					g.SetupContext = userMessage
					g.State = &CodexGoal{ThreadID: threadID, Objective: opts.Goal.Objective, Status: "active", TokenBudget: opts.Goal.TokenBudget}
				}
				if opts.Goal.ExpectedHandoffID != "" {
					if g.Handoff == nil || g.Handoff.ID != opts.Goal.ExpectedHandoffID || g.Handoff.Phase != "resume_pending" || g.Generation != *opts.Goal.ExpectedGeneration || g.RunID != opts.Goal.ExpectedRunID {
						return
					}
					g.Handoff.Phase = "resuming"
				} else if opts.Goal.ExpectedGeneration == nil {
					g.Handoff = nil // old operation must not stop this explicit replacement
				}
				g.Generation++
				g.DesiredPaused = false
				g.RecoveryPending = false
				g.ActivationPending = true
				if opts.Goal.ExpectedGeneration == nil {
					g.RecoveryAttempts = 0
					g.RuntimeFailures = 0
				}
				g.RunID = opts.GoalRunID
				if opts.GoalUserID != "" {
					g.UserID = opts.GoalUserID
				}
				if opts.OriginPeerID != "" {
					g.OriginPeerID = opts.OriginPeerID
				}
				activationAdmitted = true
				// Pending activation is fenced before RPC; success is recorded on ACK.
			}); err != nil {
				runtime.mu.Unlock()
				send(ChatEvent{Type: "error", ErrorMessage: err.Error()})
				shutdown()
				return
			}
			runtime.mu.Unlock()
			if !activationAdmitted {
				send(ChatEvent{Type: "error", ErrorMessage: "goal activation was cancelled"})
				shutdown()
				return
			}
			if opts.OnSteerReady != nil {
				opts.OnSteerReady(func(text string) error {
					q, err := ParseGoalCommand(text)
					if err != nil {
						return err
					}
					if q != nil {
						g, err := runtime.control(ctx, q)
						if err == nil {
							send(ChatEvent{Type: "text", Delta: "\n\n" + goalSummary(g) + "\n"})
						}
						return err
					}
					return steerer.steer(text)
				})
			}
			if ctx.Err() != nil {
				shutdown()
				return
			}
			var replyStart func() (int64, error)
			if opts.ResumeGoalOnReply {
				replyStart = func() (int64, error) { return startTurn(userMessage) }
			}
			result := runCodexGoalWithReply(scanner, opts.Goal, runtime, steerer, respondServerRequest, b.logger, send, replyStart, qs)
			if runtime.wantsHandoff() {
				shutdownErr := shutdown()
				if result.processError != "" {
					shutdownErr = errors.New(result.processError)
				}
				runtime.finishHandoff(shutdownErr)
				if shutdownErr != nil && result.processError == "" {
					result.processError = "Goal handoff failed: " + shutdownErr.Error()
				}
				result.fullText.WriteString(goalHandoffSummary(agent.ID, opts.SessionKey))
				send(ChatEvent{Type: "done", Message: result.buildMessage(), Usage: result.usage, ErrorMessage: result.processError})
				return
			}
			if ctx.Err() != nil {
				shutdown()
				emitCancelDone(ctx, ch, result.fullText.String(), result.thinking.String(), result.toolUses, result.usage)
				return
			}
			if result.processError != "" {
				_ = updateGoalBinding(agent.ID, opts.SessionKey, func(b *GoalBinding) {
					if b.State != nil && b.State.Status == "active" && !b.DesiredPaused {
						b.RuntimeFailures++
						b.RecoveryPending = b.RuntimeFailures < 3
						if b.RuntimeFailures >= 3 {
							b.DesiredPaused = true
							b.Generation++
						}
					}
				})
			}
			send(ChatEvent{Type: "done", Message: result.buildMessage(), Usage: result.usage, ErrorMessage: result.processError, ErrorCode: result.processErrorCode})
			shutdown()
			return
		}
		if !send(ChatEvent{Type: "status", Status: "thinking"}) {
			shutdown()
			return
		}

		// Step 5: Process streaming events. A successful turn/completed with
		// no final assistant text is not a usable completion: Codex occasionally
		// emits exactly that after a long tool-heavy turn. Continue the same
		// thread automatically (bounded to avoid an infinite retry loop), so the
		// filesystem work and model context survive and the caller receives a
		// real final response instead of a generic empty-result error.
		result := runCodexTurns(
			ctx,
			scanner,
			userMessage,
			codexRetryPolicy(opts),
			startTurn,
			steerer,
			respondServerRequest,
			b.logger.With("agent", agent.ID, "sessionKey", opts.SessionKey),
			send,
			qs,
		)
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
			send(ChatEvent{Type: "done", Message: result.buildMessage(), Usage: result.usage, ErrorMessage: result.processError, ErrorCode: result.processErrorCode})
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

// buildCodexResumeParams asks app-server to restore the thread without
// returning its historical turns. Kojo only needs the resumed thread's ID and
// rollout path; hydrating every turn can make the single JSON-RPC response
// exceed the bounded line reader when a thread contains large tool outputs or
// image data. excludeTurns affects only the response payload, not the history
// Codex restores for subsequent turns.
func buildCodexResumeParams(threadParams map[string]any, threadID string) map[string]any {
	resumeParams := cloneStringAnyMap(threadParams)
	resumeParams["threadId"] = threadID
	resumeParams["excludeTurns"] = true
	return resumeParams
}

// codexStreamResult holds the accumulated state from parsing a Codex stream.
type codexStreamResult struct {
	questions        *codexQuestionState
	questionText     string // fallback for a run ending immediately after an async question
	fullText         strings.Builder
	lastTextItemID   string // agentMessage item that produced the latest text delta
	thinking         strings.Builder
	toolUses         []ToolUse
	usage            *Usage
	processError     string // non-empty if turn/completed reported an error
	processErrorCode string // app-server codexErrorInfo classification
	activity         bool   // any model item, delta, or server request; fail closed for overload replay
	turnStatus       string // status reported by the newest turn/completed
	turnCompleted    bool   // true if turn/completed was received
	cancelled        bool   // true if send returned false (context cancelled)

	// streamedReasoning records the reasoning item ids that already arrived
	// as item/reasoning/*Delta notifications, so the completed item for the
	// same id is not appended a second time. See handleReasoningCompleted.
	streamedReasoning map[string]bool

	// anyStreamedReasoning is true once any reasoning delta arrived, with or
	// without an item id. It is the fallback guard for a provider that streams
	// deltas but leaves itemId empty, where the per-id set cannot match.
	anyStreamedReasoning bool
}

// codexTurnStarter writes turn/start and returns its JSON-RPC request id.
// Keeping this as a small function type makes the empty-completion recovery
// loop testable without spawning a real app-server process.
type codexTurnStarter func(input string) (int64, error)

// runCodexTurns processes one logical user request, automatically continuing
// the same Codex thread after empty successful completions, and (for check-ins)
// explicit overload failures before work starts. Tool/thinking state from each attempt
// is retained in the terminal Message; live events are already emitted by
// parseCodexStream as each attempt runs.
func runCodexTurns(
	ctx context.Context,
	scanner *jsonlLineScanner,
	initialInput string,
	policy codexTurnRetryPolicy,
	startTurn codexTurnStarter,
	steerer *codexSteerer,
	respondServerRequest codexServerRequestResponder,
	logger *slog.Logger,
	send func(ChatEvent) bool,
	questions ...*codexQuestionState,
) *codexStreamResult {
	combined := &codexStreamResult{}
	input := initialInput

	emptyRetries, overloadRetries := 0, 0
	for {
		if ctx.Err() != nil {
			combined.cancelled = true
			combined.turnCompleted = false
			return combined
		}

		turnStartID, err := startTurn(input)
		if err != nil {
			send(ChatEvent{Type: "error", ErrorMessage: "codex turn/start failed: " + err.Error()})
			combined.cancelled = true
			combined.turnCompleted = false
			return combined
		}

		result := parseCodexStream(scanner, turnStartID, steerer, respondServerRequest, logger, send, questions...)
		combined.absorb(result)

		// Retry only explicit overload failures before any work across the whole
		// logical request. Never replay tools, partial output, or unknown failures.
		if ctx.Err() == nil && !result.cancelled && result.turnCompleted &&
			result.turnStatus == "failed" && result.processErrorCode == "serverOverloaded" &&
			!combined.activity && overloadRetries < len(policy.overloadDelays) &&
			steerer.prepareOverloadRetry(policy.overloadDelays[overloadRetries]) {
			delay := policy.overloadDelays[overloadRetries]
			overloadRetries++
			logger.Warn("codex overloaded before work; waiting to retry",
				"retry", overloadRetries, "maxRetries", len(policy.overloadDelays),
				"delay", delay, "errorCode", result.processErrorCode, "err", result.processError)
			if !waitCodexRetry(ctx, delay) {
				combined.cancelled = true
				combined.turnCompleted = false
				return combined
			}
			input = codexOverloadRetryPrompt
			continue
		}

		// Only a clean, successful completion with no final answer is
		// recoverable here. Failed/interrupted turns, cancellation, and broken
		// streams keep their existing error paths.
		if result.cancelled || !result.turnCompleted || result.turnStatus != "completed" || result.processError != "" || result.hasFinalResponse() {
			return combined
		}
		if emptyRetries >= policy.maxEmptyRetries {
			combined.processError = codexEmptyCompletionError
			logger.Warn("codex turn completed without final response; automatic retries exhausted",
				"retries", policy.maxEmptyRetries)
			return combined
		}

		logger.Warn("codex turn completed without final response; starting automatic continuation",
			"retry", emptyRetries+1, "maxRetries", policy.maxEmptyRetries)
		emptyRetries++
		input = codexEmptyCompletionRetryPrompt
	}
}

// absorb folds one continuation attempt into the logical request result. The
// terminal flags belong to the newest attempt, while narrative/tool history is
// cumulative so persistence retains the work performed before recovery.
func (r *codexStreamResult) absorb(next *codexStreamResult) {
	if next == nil {
		return
	}
	r.fullText.WriteString(next.fullText.String())
	r.thinking.WriteString(next.thinking.String())
	if next.questionText != "" {
		r.questionText = strings.TrimSpace(r.questionText + "\n\n" + next.questionText)
	}
	r.toolUses = append(r.toolUses, next.toolUses...)
	if next.usage != nil {
		if r.usage == nil {
			usage := *next.usage
			r.usage = &usage
		} else {
			r.usage.InputTokens += next.usage.InputTokens
			r.usage.OutputTokens += next.usage.OutputTokens
			r.usage.CacheReadInputTokens += next.usage.CacheReadInputTokens
			r.usage.CacheCreationInputTokens += next.usage.CacheCreationInputTokens
			r.usage.CostUSD += next.usage.CostUSD
		}
	}
	r.activity = r.activity || next.activity
	r.processErrorCode = next.processErrorCode
	r.processError = next.processError
	r.turnStatus = next.turnStatus
	r.turnCompleted = next.turnCompleted
	r.cancelled = next.cancelled
}

// hasFinalResponse deliberately checks assistant text rather than hasOutput:
// tool calls and reasoning prove work happened, but they are not a response to
// the user. Whitespace-only model output is likewise not a usable completion.
func (r *codexStreamResult) hasFinalResponse() bool {
	return strings.TrimSpace(r.fullText.String()) != "" || strings.TrimSpace(r.questionText) != ""
}

// buildMessage creates a Message from accumulated stream data.
func (r *codexStreamResult) buildMessage() *Message {
	text := r.fullText.String()
	if strings.TrimSpace(text) == "" {
		text = r.questionText
	}
	return assembleAssistantMessage(text, r.thinking.String(), r.toolUses, r.usage)
}

// hasOutput returns true if the stream produced any text or tool uses.
func (r *codexStreamResult) hasOutput() bool {
	return r.fullText.Len() > 0 || r.questionText != "" || len(r.toolUses) > 0
}

// parseCodexStream reads Codex app-server JSON-RPC notifications from a scanner
// and emits ChatEvents via the send callback. Returns the accumulated result.
// If send returns false (context cancelled), parsing stops immediately.
// steer may be nil; when set, the active turn id from the turn/start
// response (or the turn/started notification) is forwarded to it so
// mid-turn turn/steer requests can be issued.
func parseCodexStream(scanner *jsonlLineScanner, turnStartID int64, steer *codexSteerer, respondServerRequest codexServerRequestResponder, logger *slog.Logger, send func(ChatEvent) bool, questions ...*codexQuestionState) *codexStreamResult {
	res := &codexStreamResult{}
	if len(questions) > 0 {
		res.questions = questions[0]
	}
	itemPhases := make(map[string]string) // itemID -> phase ("commentary" or "final_answer")

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var msg rpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			logger.Debug("codex rpc parse error", "line", line, "err", err)
			res.activity = true // Unreadable notifications cannot prove that no work ran.
			continue
		}

		// Server-initiated requests (for example item/tool/call emitted by
		// request_plugin_install) must be handled or fail the turn explicitly.
		// Treating every message with an ID as a response leaves Codex waiting
		// forever because kojo has no interactive client surface for them.
		handled, err := handleCodexServerRequest(&msg, respondServerRequest, logger)
		if err != nil {
			send(ChatEvent{Type: "error", ErrorMessage: "codex server request handling failed: " + err.Error()})
			res.cancelled = true
			return res
		}
		if handled {
			res.activity = true
			continue
		}

		// Handle RPC responses to requests sent by kojo. Those IDs are numeric,
		// while Codex server requests may use either numeric or string IDs.
		if id, ok := msg.numericID(); ok {
			if id == turnStartID {
				if msg.Error != nil {
					send(ChatEvent{Type: "error", ErrorMessage: msg.Error.Message})
					res.cancelled = true
					return res
				}
				if steer != nil {
					steer.setTurnID(decodeCodexTurnID(msg.Result))
				}
			} else if steer != nil && steer.resolve(id, msg.Error) {
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
		if steer != nil && msg.Method == "turn/completed" {
			// Atomically retire the completed turn id before any caller can
			// steer into it. finishTurn installs the readiness channel that an
			// automatic continuation (if needed) will satisfy with its new id.
			steer.finishTurn()
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
	// Include unrendered/unknown item kinds (file changes, collab tools, etc.)
	// in the safety guard, not just the subset the UI knows how to display.
	if msg.Method == "item/started" || msg.Method == "item/completed" {
		var params struct {
			Item struct {
				Type string `json:"type"`
			} `json:"item"`
		}
		if msg.Params == nil || json.Unmarshal(*msg.Params, &params) != nil || params.Item.Type != "userMessage" {
			res.activity = true
		}
	} else if strings.HasPrefix(msg.Method, "item/") {
		res.activity = true
	}
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
			ItemID string `json:"itemId"`
			Delta  string `json:"delta"`
		}
		json.Unmarshal(*msg.Params, &params)
		if params.Delta != "" {
			res.anyStreamedReasoning = true
			if params.ItemID != "" {
				if res.streamedReasoning == nil {
					res.streamedReasoning = map[string]bool{}
				}
				res.streamedReasoning[params.ItemID] = true
			}
			res.thinking.WriteString(params.Delta)
			if !send(ChatEvent{Type: "thinking", Delta: params.Delta}) {
				res.cancelled = true
				return true
			}
		}
		return false

	case "item/completed":
		return res.handleItemCompleted(msg, itemPhases, send)

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
					Status string          `json:"status"`
					Error  *codexTurnError `json:"error"`
				} `json:"turn"`
			}
			json.Unmarshal(*msg.Params, &params)
			res.turnStatus = params.Turn.Status
			if params.Turn.Status == "failed" || params.Turn.Status == "interrupted" {
				res.processError = "codex turn " + params.Turn.Status
				if params.Turn.Error != nil {
					if params.Turn.Error.Message != "" {
						res.processError = params.Turn.Error.Message
					}
					res.processErrorCode = codexErrorCode(params.Turn.Error.CodexErrorInfo)
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
			ID        string            `json:"id"`
			Type      string            `json:"type"`
			Phase     string            `json:"phase"`
			Command   string            `json:"command"`
			Tool      string            `json:"tool"`
			Server    string            `json:"server"`
			Arguments json.RawMessage   `json:"arguments"`
			Changes   []codexFileChange `json:"changes"`
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
	case "fileChange":
		// apply_patch. Codex has no separate read/write tools: reads are
		// shell commands (commandExecution) and writes are patches, which
		// app-server reports as a fileChange item rather than a command.
		input := codexFileChangeInput(params.Item.Changes)
		res.toolUses = append(res.toolUses, ToolUse{
			ID:    params.Item.ID,
			Name:  codexApplyPatchTool,
			Input: input,
		})
		if !send(ChatEvent{Type: "tool_use", ToolUseID: params.Item.ID, ToolName: codexApplyPatchTool, ToolInput: input}) {
			res.cancelled = true
			return true
		}
	}
	return false
}

// codexApplyPatchTool is the tool name shown for a fileChange item. It is the
// name of the built-in Codex tool that produces one.
const codexApplyPatchTool = "apply_patch"

// codexFileChange is one entry of a fileChange item's `changes` array.
type codexFileChange struct {
	Path string `json:"path"`
	Kind struct {
		Type     string `json:"type"` // add | delete | update
		MovePath string `json:"move_path"`
	} `json:"kind"`
	Diff string `json:"diff"`
}

// codexFileChangeDiffLimit caps the diff kept per file in the tool input. The
// input is streamed, stored on the message and replayed on every history
// fetch, so a multi-megabyte patch is trimmed to a preview.
const codexFileChangeDiffLimit = 16 * 1024

// codexFileChangeInput renders a fileChange item's changes as the tool input:
// one "<kind> <path>" header per file followed by its diff (the full content
// for an add, truncated past codexFileChangeDiffLimit), files separated by a
// blank line.
func codexFileChangeInput(changes []codexFileChange) string {
	var b strings.Builder
	for i, c := range changes {
		if i > 0 {
			b.WriteString("\n\n")
		}
		kind := c.Kind.Type
		if kind == "" {
			kind = "update"
		}
		b.WriteString(kind)
		b.WriteString(" ")
		b.WriteString(c.Path)
		if c.Kind.MovePath != "" {
			b.WriteString(" -> ")
			b.WriteString(c.Kind.MovePath)
		}
		diff := strings.TrimRight(c.Diff, "\n")
		if len(diff) > codexFileChangeDiffLimit {
			cut := codexFileChangeDiffLimit
			for cut > 0 && !utf8.RuneStart(diff[cut]) {
				cut--
			}
			diff = diff[:cut] + fmt.Sprintf("\n[...truncated %d bytes...]", len(diff)-cut)
		}
		if diff != "" {
			b.WriteString("\n")
			b.WriteString(diff)
		}
	}
	return b.String()
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
		segmentStart := params.ItemID != "" && params.ItemID != res.lastTextItemID
		res.lastTextItemID = params.ItemID
		res.fullText.WriteString(params.Delta)
		if !send(ChatEvent{Type: "text", Delta: params.Delta, textSegmentStart: segmentStart}) {
			res.cancelled = true
			return true
		}
	}
	return false
}

// codexReasoningItemText joins the entries of a completed reasoning item's
// `summary` / `content` array. app-server writes plain strings there for a
// custom provider and objects carrying a text field for OpenAI models, so
// accept both and ignore anything else rather than rendering raw JSON.
func codexReasoningItemText(entries []json.RawMessage) string {
	parts := make([]string, 0, len(entries))
	for _, raw := range entries {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			// Keep the entry verbatim; only skip it when it carries no text at
			// all, so a model's own indentation and blank lines survive.
			if strings.TrimSpace(s) != "" {
				parts = append(parts, s)
			}
			continue
		}
		var obj struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &obj); err == nil {
			if strings.TrimSpace(obj.Text) != "" {
				parts = append(parts, obj.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func (res *codexStreamResult) handleItemCompleted(msg *rpcMessage, itemPhases map[string]string, send func(ChatEvent) bool) bool {
	if msg.Params == nil {
		return false
	}
	var params struct {
		Item struct {
			Delivery         string          `json:"delivery"`
			Questions        json.RawMessage `json:"questions"`
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
			ContentItems json.RawMessage   `json:"contentItems"`
			Success      *bool             `json:"success"`
			Summary      []json.RawMessage `json:"summary"`
			Content      []json.RawMessage `json:"content"`
			Changes      []codexFileChange `json:"changes"`
		} `json:"item"`
	}
	json.Unmarshal(*msg.Params, &params)

	switch params.Item.Type {
	case "reasoning":
		// Reasoning does not always arrive as item/reasoning/*Delta. Against a
		// custom model provider (custom-codex) app-server emits no reasoning
		// deltas at all and only publishes the finished item, so without this
		// branch the model's thinking never reaches the UI. Skip ids that did
		// stream so the OpenAI provider does not duplicate its own summary.
		if res.streamedReasoning[params.Item.ID] {
			return false
		}
		if res.anyStreamedReasoning && params.Item.ID == "" {
			// A provider that streams deltas without an item id cannot be
			// matched per id, so fall back to suppressing the whole item.
			return false
		}
		text := codexReasoningItemText(params.Item.Summary)
		if text == "" {
			text = codexReasoningItemText(params.Item.Content)
		}
		if text == "" {
			return false
		}
		if prev := res.thinking.String(); prev != "" && !strings.HasSuffix(prev, "\n") {
			text = "\n" + text
		}
		res.thinking.WriteString(text)
		if !send(ChatEvent{Type: "thinking", Delta: text}) {
			res.cancelled = true
			return true
		}
	case "agentMessage":
		// Message-delivered questions have no server RPC awaiting a reply.
		// Do not mix their text into the later final answer (or post it twice).
		if res.questions != nil && params.Item.Delivery == "async" && len(params.Item.Questions) > 0 && string(params.Item.Questions) != "null" && string(params.Item.Questions) != "[]" {
			seen := res.questions.asyncSeen[params.Item.ID]
			if err := res.questions.registerAsync(msg); err == nil {
				if !seen {
					res.questionText = strings.TrimSpace(res.questionText + "\n\n" + params.Item.Text)
				}
				return false
			}
			// An unsupported form remains readable as ordinary text.
		}
		// app-server normally streams agentMessage deltas, but the completed
		// item is the authoritative snapshot. If no delta arrived at all, use
		// its text as a fallback so a valid final answer is not mistaken for an
		// empty completion and re-run.
		if params.Item.Text == "" {
			return false
		}
		if itemPhases[params.Item.ID] == "commentary" {
			if res.thinking.Len() == 0 {
				res.thinking.WriteString(params.Item.Text)
				if !send(ChatEvent{Type: "thinking", Delta: params.Item.Text}) {
					res.cancelled = true
					return true
				}
			}
		} else if res.fullText.Len() == 0 {
			res.fullText.WriteString(params.Item.Text)
			if !send(ChatEvent{Type: "text", Delta: params.Item.Text}) {
				res.cancelled = true
				return true
			}
		}
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
	case "fileChange":
		// status is completed | failed | declined. The v2 FileChange item
		// carries no error or per-file outcome, so the status is the whole
		// result. Record it before sending so a cancel that lands on this
		// event still leaves the stored tool use with its output.
		output := params.Item.Status
		// item/started normally carries the full change set, but with the
		// apply_patch_streaming_events feature the patch can still be
		// streaming at that point (item/fileChange/patchUpdated follows).
		// The completed item is authoritative, so backfill an input that
		// started out empty rather than storing a blank apply_patch.
		if input := codexFileChangeInput(params.Item.Changes); input != "" {
			for i := len(res.toolUses) - 1; i >= 0; i-- {
				if res.toolUses[i].ID == params.Item.ID {
					if res.toolUses[i].Input == "" {
						res.toolUses[i].Input = input
					}
					break
				}
			}
		}
		matchToolOutput(res.toolUses, params.Item.ID, codexApplyPatchTool, output)
		if !send(ChatEvent{Type: "tool_result", ToolUseID: params.Item.ID, ToolName: codexApplyPatchTool, ToolOutput: output}) {
			res.cancelled = true
			return true
		}
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

// JSON-RPC responses keep the ID as raw JSON because the Codex protocol
// permits string and integer request IDs and requires an exact echo.
type rpcResultResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

type codexServerRequestResponder func(msg *rpcMessage) (outcome string, err error)

func newCodexServerRequestResponder(writeLine func(any) error) codexServerRequestResponder {
	writeResult := func(id json.RawMessage, result any) error {
		return writeLine(rpcResultResponse{JSONRPC: "2.0", ID: id, Result: result})
	}
	return func(msg *rpcMessage) (string, error) {
		if msg == nil || msg.ID == nil {
			return "", errors.New("Codex server request has no id")
		}
		switch msg.Method {
		case "item/tool/call":
			tool, err := codexDynamicToolName(msg)
			if err != nil {
				return "", err
			}
			if tool != "request_plugin_install" {
				return "", fmt.Errorf("unsupported Codex dynamic client tool %q; kojo must add an explicit handler", tool)
			}
			err = writeResult(*msg.ID, map[string]any{
				"success": false,
				"contentItems": []map[string]string{{
					"type": "inputText",
					"text": "Plugin installation is not available in kojo; continue without it or ask the user in normal chat.",
				}},
			})
			return "dynamic_tool_failed", err

		case "item/tool/requestUserInput":
			// An empty typed response lets Codex continue without transport-level
			// failure. The model can then ask the user in normal chat if needed.
			return "user_input_empty", writeResult(*msg.ID, map[string]any{
				"answers": map[string]any{},
			})

		case "mcpServer/elicitation/request":
			return "declined", writeResult(*msg.ID, map[string]any{"action": "decline"})

		case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
			return "declined", writeResult(*msg.ID, map[string]any{"decision": "decline"})

		case "execCommandApproval", "applyPatchApproval":
			return "declined", writeResult(*msg.ID, map[string]any{"decision": "denied"})

		case "item/permissions/requestApproval":
			return "declined", writeResult(*msg.ID, map[string]any{
				"permissions": map[string]any{"fileSystem": nil, "network": nil},
				"scope":       "turn",
			})

		case "currentTime/read":
			return "current_time_returned", writeResult(*msg.ID, map[string]any{
				"currentTimeAt": time.Now().Unix(),
			})

		case "account/chatgptAuthTokens/refresh", "attestation/generate":
			return "", fmt.Errorf("unsupported Codex infrastructure request %q; refusing to hide an authentication or attestation failure", msg.Method)

		default:
			return "", fmt.Errorf("unknown Codex server request %q; kojo must add an explicit handler", msg.Method)
		}
	}
}

// rpcMessage is a generic JSON-RPC 2.0 message (response or notification).
type rpcMessage struct {
	JSONRPC string           `json:"jsonrpc,omitempty"`
	Method  string           `json:"method,omitempty"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
	Params  *json.RawMessage `json:"params,omitempty"`
}

func (m *rpcMessage) numericID() (int64, bool) {
	if m == nil || m.ID == nil {
		return 0, false
	}
	var id int64
	if err := json.Unmarshal(*m.ID, &id); err != nil {
		return 0, false
	}
	return id, true
}

func codexDynamicToolName(msg *rpcMessage) (string, error) {
	if msg == nil || msg.Params == nil {
		return "", errors.New("Codex item/tool/call request has no params")
	}
	var params struct {
		Tool string `json:"tool"`
	}
	if err := json.Unmarshal(*msg.Params, &params); err != nil {
		return "", fmt.Errorf("decode Codex item/tool/call params: %w", err)
	}
	if params.Tool == "" {
		return "", errors.New("Codex item/tool/call request has no tool name")
	}
	return params.Tool, nil
}

// handleCodexServerRequest dispatches every known server-initiated request
// explicitly. Interactive requests receive a typed decline/failure so Codex
// can continue. Infrastructure and unknown requests fail the turn instead of
// being silently converted into a recoverable tool error.
func handleCodexServerRequest(msg *rpcMessage, respond codexServerRequestResponder, logger *slog.Logger) (bool, error) {
	if msg != nil && msg.Method == "serverRequest/resolved" && msg.ID == nil {
		if respond != nil {
			_, err := respond(msg)
			return true, err
		}
		return true, nil
	}
	if msg == nil || msg.ID == nil || msg.Method == "" {
		return false, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	if respond == nil {
		err := fmt.Errorf("no responder for Codex server request %q", msg.Method)
		logger.Warn("codex server request handling failed", "method", msg.Method, "err", err)
		return true, err
	}
	tool, _ := codexDynamicToolName(msg)
	outcome, err := respond(msg)
	if err != nil {
		logger.Warn("codex server request handling failed", "method", msg.Method, "tool", tool, "err", err)
		return true, err
	}
	logger.Warn("codex server request handled", "method", msg.Method, "tool", tool, "outcome", outcome)
	return true, nil
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

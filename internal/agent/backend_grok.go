package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// grokSessionIDPattern matches grok's UUIDv7-style session IDs
// (8-4-4-4-12 lowercase hex). We validate every value we read from
// the on-disk session_id file or the streamed `end` event before
// passing it to `--resume` or to `filepath.Join` for cleanup — an
// agent has write access to its own workspace (and therefore to
// .grok/session_id), so an unvalidated value would let a hostile
// agent inject CLI flags (e.g. `--cwd=/`) or escape the session
// directory on cleanup (e.g. `../../etc`).
var grokSessionIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// isGrokSessionID reports whether s looks like a legitimate grok
// session UUID. Used as a gate before any path-join or argv
// concatenation with externally-influenced data.
func isGrokSessionID(s string) bool {
	return grokSessionIDPattern.MatchString(s)
}

// GrokBackend implements ChatBackend for the Grok Build CLI
// (https://x.ai/news/grok-build-cli). It drives `grok` headlessly via
// --prompt-file plus --output-format streaming-json and parses the
// resulting thought/text/end events into ChatEvents.
//
// Session continuity uses an explicit per-agent session ID captured
// from the streaming "end" event, persisted to <agentDir>/.grok/session_id,
// and replayed on the next non-OneShot turn via `--resume <id>`. We
// do NOT rely on grok's `--continue` (which picks the most-recently-
// modified session for a cwd) because that would let an OneShot Slack
// thread silently take over the agent's primary session — see the
// Codex review on the original `--continue` implementation.
//
// OneShot turns never read or write the stored session ID and the
// session directory grok creates for them is removed after the turn
// completes, so the persistent session stays isolated.
//
// MCP injection is intentionally unsupported here: the grok CLI loads
// MCP servers from `~/.grok/config.toml` (global) or a project-scoped
// `<cwd>/.grok/config.toml`. Per-session inline overrides are not
// available. When opts.MCPServers is non-empty we log a warning so
// the caller can see that the configured servers were dropped — use
// the operator's global grok config to wire MCP for now.
type GrokBackend struct {
	logger *slog.Logger
}

func NewGrokBackend(logger *slog.Logger) *GrokBackend {
	return &GrokBackend{logger: logger}
}

func (b *GrokBackend) Name() string { return "grok" }

func (b *GrokBackend) Available() bool {
	_, err := exec.LookPath("grok")
	return err == nil
}

// grokSessionIDFile returns the per-agent file that holds the primary
// grok session UUID for resume. Co-located with the agent's other
// CLI state under <agentDir>/.grok/ so ResetData (which removes the
// whole agentDir) wipes it as a side effect.
func grokSessionIDFile(agentDirPath string) string {
	return filepath.Join(agentDirPath, ".grok", "session_id")
}

// readGrokSessionID returns the saved primary grok session ID for the
// agent, or "" if none is stored / readable / well-formed. A
// malformed value (anything other than a grok UUID) is treated as
// absent AND deleted from disk so a poisoned file can't keep
// failing future turns.
func readGrokSessionID(agentDirPath string) string {
	path := grokSessionIDFile(agentDirPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(data))
	if !isGrokSessionID(id) {
		// The agent has write access to its own workspace and
		// therefore to .grok/session_id. Rejecting and removing
		// the bad value prevents an injected string like
		// "--cwd=/etc" or "../foo" from ever reaching `--resume`
		// or filepath.Join on the cleanup path.
		_ = os.Remove(path)
		return ""
	}
	return id
}

// writeGrokSessionID persists the primary grok session ID for the
// agent. Refuses to write a non-UUID so a malformed `end` event
// can't poison the file. Errors are logged but not fatal — losing
// the file just means the next turn starts a fresh session, which
// is correct fallback behaviour.
func writeGrokSessionID(agentDirPath, sessionID string, logger *slog.Logger) {
	if !isGrokSessionID(sessionID) {
		logger.Warn("grok: refusing to persist non-UUID session_id", "value", sessionID)
		return
	}
	path := grokSessionIDFile(agentDirPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		logger.Warn("grok: mkdir for session_id failed", "path", path, "err", err)
		return
	}
	if err := os.WriteFile(path, []byte(sessionID), 0o644); err != nil {
		logger.Warn("grok: write session_id failed", "path", path, "err", err)
	}
}

func buildGrokArgs(promptPath, dir, resumeID string, agent *Agent, systemPrompt string) []string {
	args := []string{
		"--prompt-file", promptPath,
		"--output-format", "streaming-json",
		"--cwd", dir,
		// Headless mode: never block waiting for an approval prompt.
		"--always-approve",
		// Kojo owns agent memory via MEMORY.md, memory/*.md, and DB-backed
		// memory_entries. Grok's separate ~/.grok/memory store would bypass
		// Kojo truncation/reset and re-inject stale facts on later turns.
		"--no-memory",
		// Skip plan mode + subagent spawning to keep behaviour predictable.
		"--no-plan",
		"--no-subagents",
	}
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	if agent.Model != "" {
		args = append(args, "--model", agent.Model)
	}
	if agent.Effort != "" {
		args = append(args, "--effort", agent.Effort)
	}
	if systemPrompt != "" {
		// `--system-prompt-override` replaces the entire built-in
		// system prompt, matching the Claude backend's
		// `--system-prompt` semantics.
		args = append(args, "--system-prompt-override", systemPrompt)
	}
	return args
}

func grokCommandEnv(agentID, dir string) []string {
	env := filterEnv([]string{"AGENT_BROWSER_SESSION", "AGENT_BROWSER_COOKIE_DIR", "GROK_MEMORY="}, agentID, dir)
	return append(env, "GROK_MEMORY=0")
}

func (b *GrokBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, error) {
	grokPath, err := exec.LookPath("grok")
	if err != nil {
		return nil, fmt.Errorf("grok not found in PATH")
	}

	dir := agentDir(agent.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create agent dir: %w", err)
	}

	// Materialise the user message via --prompt-file. Passing the
	// prompt through argv works for short inputs but risks ARG_MAX
	// truncation and removes ambiguity around messages that start
	// with "-" (which grok's clap parser would treat as flags).
	promptFile, err := os.CreateTemp(dir, "grok-prompt-*.txt")
	if err != nil {
		return nil, fmt.Errorf("create prompt file: %w", err)
	}
	promptPath := promptFile.Name()
	if _, err := promptFile.WriteString(userMessage); err != nil {
		promptFile.Close()
		os.Remove(promptPath)
		return nil, fmt.Errorf("write prompt file: %w", err)
	}
	if err := promptFile.Close(); err != nil {
		os.Remove(promptPath)
		return nil, fmt.Errorf("close prompt file: %w", err)
	}

	// Resume strategy:
	//   non-OneShot + stored session ID present → --resume <id>
	//   non-OneShot + no stored ID             → fresh session; capture ID on completion
	//   OneShot                                 → always fresh; never read/write stored ID
	var resumeID string
	if !opts.OneShot {
		resumeID = readGrokSessionID(dir)
	}

	args := buildGrokArgs(promptPath, dir, resumeID, agent, systemPrompt)
	mcpWarning := ""
	if len(opts.MCPServers) > 0 {
		names := make([]string, 0, len(opts.MCPServers))
		for name := range opts.MCPServers {
			names = append(names, name)
		}
		mcpWarning = fmt.Sprintf("grok backend: dropped %d MCP server(s) — grok only loads MCP from ~/.grok/config.toml or <cwd>/.grok/config.toml. Configure them there to use them with this agent. Dropped: %s",
			len(opts.MCPServers), strings.Join(names, ", "))
		b.logger.Warn("grok backend: dropped MCP servers", "agent", agent.ID, "count", len(opts.MCPServers), "names", names)
	}

	cmd := exec.CommandContext(ctx, grokPath, args...)
	cmd.Env = grokCommandEnv(agent.ID, dir)
	cmd.Dir = dir
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second

	var stderrBuf bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderrBuf, limit: 4096}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		os.Remove(promptPath)
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		os.Remove(promptPath)
		return nil, fmt.Errorf("start grok: %w", err)
	}

	ch := make(chan ChatEvent, 64)

	go func() {
		defer close(ch)
		defer os.Remove(promptPath)

		send := func(e ChatEvent) bool {
			select {
			case ch <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}

		// Surface MCP-drop warning ahead of the model output so the
		// caller's transcript records it. Use a Type:"message"
		// system event — the WebUI's AgentChat switch renders
		// "message" events into the conversation, whereas plain
		// "status" events are not displayed.
		//
		// CAVEAT: the manager's processChatEvents accumulator does
		// not currently persist backend-emitted "message" events to
		// the on-disk transcript (see internal/agent/manager.go
		// handleTerminal — only "done" and ErrorMessage land in the
		// store). The warning is therefore live-only: a client that
		// connects AFTER the turn sees the agent reply but not the
		// MCP-drop notice. logger.Warn above covers that gap for
		// operators. Persisting "message" centrally is tracked
		// separately; doing it here would mean wiring a per-backend
		// store handle that other backends don't carry today.
		if mcpWarning != "" {
			send(ChatEvent{Type: "message", Message: newSystemMessage(mcpWarning)})
		}

		result := parseGrokStream(stdout, b.logger, send)

		if result.cancelled {
			cmd.Wait()
			enrichGrokToolUsesFromSessionHistory(dir, result, b.logger)
			emitCancelDone(ctx, ch, result.text, result.thinking, result.toolUses, nil)
			// Best-effort: drop a OneShot session that was created
			// mid-cancel so it doesn't leak into the persistent
			// store. We tolerate empty sessionID (turn died before
			// the end event) — there's nothing reliable to remove.
			if opts.OneShot && result.sessionID != "" {
				removeGrokSessionDir(dir, result.sessionID)
			}
			return
		}

		// processError is set from at most three signals, in order of
		// preference:
		//   1. the streamed {"type":"error"} payload (most semantic)
		//   2. cmd.Wait's error — augmented with stderr if available
		//   3. nothing — clean exit, no stream error → success
		//
		// We deliberately do NOT promote stderr by itself to a fatal
		// error: grok routes routine operational logs to stderr,
		// including non-fatal `tool_error: tool_output_error` records
		// for things like `read_file` on a missing file (the agent
		// handles those gracefully and the turn still succeeds with
		// exit 0). Treating any stderr byte as fatal made every such
		// turn surface a bogus error attached to a perfectly good
		// reply.
		//
		// A grok stream can emit `{"type":"error","message":"..."}`
		// and STILL exit 0 (the CLI considers "we surfaced the error
		// to the caller" as a non-fatal outcome), so we must inspect
		// streamError even when Wait() returns nil — otherwise the
		// user sees an empty assistant message and never learns the
		// turn failed.
		waitErr := cmd.Wait()
		processError := classifyGrokProcessError(waitErr, stderrBuf.String(), result.streamError)
		if waitErr != nil {
			b.logger.Warn("grok process exited with error",
				"agent", agent.ID, "err", waitErr, "stderr", stderrBuf.String(), "streamError", result.streamError)
		} else if result.streamError != "" {
			b.logger.Warn("grok stream-level error with clean exit",
				"agent", agent.ID, "streamError", result.streamError)
		}

		// Stale-session recovery: if we asked grok to --resume a
		// stored ID and it bailed before producing any text, the
		// stored ID is almost certainly no longer valid (manual
		// `grok sessions delete`, GC, `clearGrokSession` on a
		// peer, …). Drop it so the next turn starts a fresh
		// session instead of looping on the same broken resume.
		//
		// We deliberately leave the user message untranslated —
		// the caller still sees the underlying grok error this
		// turn; the recovery only takes effect on the next call.
		if resumeID != "" && result.text == "" && isStaleSessionError(processError) {
			_ = os.Remove(grokSessionIDFile(dir))
			b.logger.Info("grok: dropped stale session_id after resume failure",
				"agent", agent.ID, "staleId", resumeID)
		}
		if processError != "" && result.text == "" {
			send(ChatEvent{Type: "error", ErrorMessage: processError})
			return
		}

		// Persist the primary session ID on first non-OneShot turn
		// so subsequent turns can `--resume <id>`. OneShot turns
		// must NEVER write here — that's the rule that keeps Slack
		// threads from hijacking the agent's WebUI session.
		if !opts.OneShot && resumeID == "" && result.sessionID != "" {
			writeGrokSessionID(dir, result.sessionID, b.logger)
		}

		enrichGrokToolUsesFromSessionHistory(dir, result, b.logger)

		// OneShot cleanup: remove the disposable session directory
		// grok just created. Done outside the resumeID branch so
		// a OneShot that happened to land on the same cwd as the
		// persistent session is still cleaned up (sessionID is per-
		// session, not per-cwd, so this can't accidentally delete
		// the agent's primary session).
		if opts.OneShot && result.sessionID != "" && result.sessionID != readGrokSessionID(dir) {
			removeGrokSessionDir(dir, result.sessionID)
		}

		msg := newAssistantMessage()
		msg.Content = result.text
		msg.Thinking = result.thinking
		msg.ToolUses = result.toolUses

		if result.sessionID != "" {
			b.logger.Debug("grok session", "agent", agent.ID, "sessionId", result.sessionID, "oneShot", opts.OneShot)
		}

		send(ChatEvent{Type: "done", Message: msg, ErrorMessage: processError})
	}()

	return ch, nil
}

// grokStreamResult collects the streamed output of one grok invocation.
type grokStreamResult struct {
	text        string
	thinking    string
	toolUses    []ToolUse
	sessionID   string
	streamError string // last {"type":"error","message":"..."} event payload
	cancelled   bool
}

type grokStreamEvent struct {
	grokToolPayload
	Type    string          `json:"type"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"` // {"type":"error","message":"..."}
	Method  string          `json:"method"`
	Params  *struct {
		Update grokToolPayload `json:"update"`
	} `json:"params"`
	StopReason string `json:"stopReason"`
	SessionID  string `json:"sessionId"`
}

type grokToolPayload struct {
	SessionUpdate   string          `json:"sessionUpdate"`
	ID              string          `json:"id"`
	ToolUseID       string          `json:"toolUseId"`
	ToolCallID      string          `json:"toolCallId"`
	ToolCallIDAlt   string          `json:"tool_call_id"`
	ToolID          string          `json:"tool_id"`
	ToolName        string          `json:"toolName"`
	ToolNameAlt     string          `json:"tool_name"`
	Name            string          `json:"name"`
	Title           string          `json:"title"`
	Kind            string          `json:"kind"`
	Status          string          `json:"status"`
	Command         string          `json:"command"`
	ToolArgsJSON    string          `json:"tool_args_json"`
	ToolInput       json.RawMessage `json:"toolInput"`
	ToolOutput      json.RawMessage `json:"toolOutput"`
	Input           json.RawMessage `json:"input"`
	Output          json.RawMessage `json:"output"`
	Args            json.RawMessage `json:"args"`
	Arguments       json.RawMessage `json:"arguments"`
	RawInput        json.RawMessage `json:"rawInput"`
	RawOutput       json.RawMessage `json:"rawOutput"`
	Result          json.RawMessage `json:"result"`
	Content         json.RawMessage `json:"content"`
	ErrorMessage    string          `json:"errorMessage"`
	ErrorMessageAlt string          `json:"error_message"`
	Message         string          `json:"message"`
}

type grokChatHistoryEntry struct {
	Type       string             `json:"type"`
	Content    json.RawMessage    `json:"content"`
	ToolCalls  []grokChatToolCall `json:"tool_calls"`
	ToolCallID string             `json:"tool_call_id"`
}

type grokChatToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Function  *struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// parseGrokStream reads grok --output-format streaming-json lines from r
// and emits ChatEvents through send. Recognised event shapes:
//
//	{"type":"thought","data":"..."}                         reasoning delta
//	{"type":"text","data":"..."}                            assistant text delta
//	{"type":"tool_call_started", ...}                        tool invocation
//	{"type":"tool_call_completed", ...}                      tool result
//	{"type":"end","stopReason":"...","sessionId":"..."}      turn end
//	{"method":"session/update","params":{"update":{...}}}    ACP-compatible updates
//
// Unknown event types are skipped — keeps us forward-compatible with
// future grok schema additions. A send that returns false (context
// cancelled) immediately stops parsing and marks the result cancelled.
func parseGrokStream(r io.Reader, logger *slog.Logger, send func(ChatEvent) bool) *grokStreamResult {
	res := &grokStreamResult{}
	scanner := bufio.NewScanner(r)
	// Single events are tiny but allow a large max so a runaway delta
	// (e.g. a single tool argument as one event) doesn't break the
	// stream with bufio.ErrTooLong.
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var textBuf, thinkBuf strings.Builder
	var toolUses []ToolUse
	resultSent := make(map[string]bool)
	cancelled := func() {
		res.text = textBuf.String()
		res.thinking = thinkBuf.String()
		res.toolUses = toolUses
		res.cancelled = true
	}

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev grokStreamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			logger.Debug("grok parse line", "err", err, "line", string(line))
			continue
		}
		eventType := ev.Type
		payload := ev.grokToolPayload
		if ev.Method == "session/update" && ev.Params != nil {
			payload = ev.Params.Update
			eventType = payload.SessionUpdate
		}
		if payload.Message == "" {
			payload.Message = ev.Message
		}
		if payload.SessionUpdate != "" && eventType == "" {
			eventType = payload.SessionUpdate
		}
		payload = mergeGrokDataPayload(payload, ev.Data)

		switch eventType {
		case "thought":
			delta := grokRawString(ev.Data)
			thinkBuf.WriteString(delta)
			if !send(ChatEvent{Type: "thinking", Delta: delta}) {
				cancelled()
				return res
			}
		case "text":
			delta := grokRawString(ev.Data)
			textBuf.WriteString(delta)
			if !send(ChatEvent{Type: "text", Delta: delta}) {
				cancelled()
				return res
			}
		case "agent_message_chunk":
			delta := grokContentText(payload.Content)
			if delta == "" {
				delta = grokRawString(ev.Data)
			}
			if delta != "" {
				textBuf.WriteString(delta)
				if !send(ChatEvent{Type: "text", Delta: delta}) {
					cancelled()
					return res
				}
			}
		case "agent_thought_chunk":
			delta := grokContentText(payload.Content)
			if delta == "" {
				delta = grokRawString(ev.Data)
			}
			if delta != "" {
				thinkBuf.WriteString(delta)
				if !send(ChatEvent{Type: "thinking", Delta: delta}) {
					cancelled()
					return res
				}
			}
		case "tool_call", "tool_call_started", "tool_started", "tool_use":
			tu := grokToolUseFromPayload(payload)
			var added bool
			toolUses, added = appendOrUpdateGrokToolUse(toolUses, tu)
			if added {
				if !send(ChatEvent{Type: "tool_use", ToolUseID: tu.ID, ToolName: tu.Name, ToolInput: tu.Input}) {
					cancelled()
					return res
				}
			}
			if grokToolPayloadIsTerminal(payload) || grokToolPayloadHasOutput(payload) {
				output := grokToolOutput(payload)
				key := grokToolKey(tu.ID, tu.Name)
				if !resultSent[key] {
					if !send(ChatEvent{Type: "tool_result", ToolUseID: tu.ID, ToolName: tu.Name, ToolOutput: output}) {
						cancelled()
						return res
					}
					matchToolOutput(toolUses, tu.ID, tu.Name, output)
					resultSent[key] = true
				}
			}
		case "tool_call_update", "tool_call_completed", "tool_completed", "tool_result", "tool_call_failed":
			tu := grokToolUseFromPayload(payload)
			var added bool
			toolUses, added = appendOrUpdateGrokToolUse(toolUses, tu)
			if added {
				if !send(ChatEvent{Type: "tool_use", ToolUseID: tu.ID, ToolName: tu.Name, ToolInput: tu.Input}) {
					cancelled()
					return res
				}
			}
			if eventType == "tool_result" || eventType == "tool_call_completed" ||
				eventType == "tool_completed" || eventType == "tool_call_failed" ||
				grokToolPayloadIsTerminal(payload) || grokToolPayloadHasOutput(payload) {
				output := grokToolOutput(payload)
				key := grokToolKey(tu.ID, tu.Name)
				if !resultSent[key] {
					if !send(ChatEvent{Type: "tool_result", ToolUseID: tu.ID, ToolName: tu.Name, ToolOutput: output}) {
						cancelled()
						return res
					}
					matchToolOutput(toolUses, tu.ID, tu.Name, output)
					resultSent[key] = true
				}
			}
		case "end":
			res.sessionID = ev.SessionID
		case "error":
			// grok emits `{"type":"error","message":"..."}` to
			// stdout for fatal stream-level errors (e.g. resume
			// against a missing session). Capture the latest
			// such message so the caller can surface it AND so
			// the stale-session detector can match against
			// stdout when stderr is empty.
			if ev.Message != "" {
				res.streamError = ev.Message
			}
		}
	}
	if err := scanner.Err(); err != nil {
		logger.Warn("grok scanner error", "err", err)
	}
	res.text = textBuf.String()
	res.thinking = thinkBuf.String()
	res.toolUses = toolUses
	return res
}

func enrichGrokToolUsesFromSessionHistory(cwd string, res *grokStreamResult, logger *slog.Logger) {
	if res == nil || res.sessionID == "" {
		return
	}
	toolUses, err := readGrokSessionHistoryToolUses(cwd, res.sessionID)
	if err != nil {
		logger.Debug("grok: read chat_history tool uses", "sessionId", res.sessionID, "err", err)
		return
	}
	if len(toolUses) > 0 {
		res.toolUses = toolUses
	}
}

func readGrokSessionHistoryToolUses(cwd, sessionID string) ([]ToolUse, error) {
	if !isGrokSessionID(sessionID) {
		return nil, fmt.Errorf("invalid session id")
	}
	dir := grokSessionDir(cwd)
	if dir == "" {
		return nil, fmt.Errorf("grok session dir unavailable")
	}
	f, err := os.Open(filepath.Join(dir, sessionID, "chat_history.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseGrokChatHistoryToolUses(f)
}

func parseGrokChatHistoryToolUses(r io.Reader) ([]ToolUse, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)

	var toolUses []ToolUse
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var entry grokChatHistoryEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		switch entry.Type {
		case "user":
			toolUses = nil
		case "assistant":
			for _, call := range entry.ToolCalls {
				toolUses = append(toolUses, grokToolUseFromChatHistoryCall(call))
			}
		case "tool_result":
			matchToolOutput(toolUses, entry.ToolCallID, "", grokChatHistoryContent(entry.Content))
		}
	}
	if err := scanner.Err(); err != nil {
		return toolUses, err
	}
	return toolUses, nil
}

func grokToolUseFromChatHistoryCall(call grokChatToolCall) ToolUse {
	name := call.Name
	args := call.Arguments
	if call.Function != nil {
		if name == "" {
			name = call.Function.Name
		}
		if len(args) == 0 {
			args = call.Function.Arguments
		}
	}
	if name == "" {
		name = "tool"
	}
	return ToolUse{
		ID:    call.ID,
		Name:  name,
		Input: grokJSONDisplay(args),
	}
}

func grokChatHistoryContent(raw json.RawMessage) string {
	if s := grokJSONString(raw); s != "" {
		return s
	}
	if s := grokContentText(raw); s != "" {
		return s
	}
	return grokJSONDisplay(raw)
}

func mergeGrokDataPayload(base grokToolPayload, data json.RawMessage) grokToolPayload {
	raw := bytes.TrimSpace(data)
	if len(raw) == 0 || raw[0] != '{' {
		return base
	}
	var extra grokToolPayload
	if err := json.Unmarshal(raw, &extra); err != nil {
		return base
	}
	return overlayGrokToolPayload(base, extra)
}

func overlayGrokToolPayload(base, extra grokToolPayload) grokToolPayload {
	if extra.SessionUpdate != "" {
		base.SessionUpdate = extra.SessionUpdate
	}
	if extra.ID != "" {
		base.ID = extra.ID
	}
	if extra.ToolUseID != "" {
		base.ToolUseID = extra.ToolUseID
	}
	if extra.ToolCallID != "" {
		base.ToolCallID = extra.ToolCallID
	}
	if extra.ToolCallIDAlt != "" {
		base.ToolCallIDAlt = extra.ToolCallIDAlt
	}
	if extra.ToolID != "" {
		base.ToolID = extra.ToolID
	}
	if extra.ToolName != "" {
		base.ToolName = extra.ToolName
	}
	if extra.ToolNameAlt != "" {
		base.ToolNameAlt = extra.ToolNameAlt
	}
	if extra.Name != "" {
		base.Name = extra.Name
	}
	if extra.Title != "" {
		base.Title = extra.Title
	}
	if extra.Kind != "" {
		base.Kind = extra.Kind
	}
	if extra.Status != "" {
		base.Status = extra.Status
	}
	if extra.Command != "" {
		base.Command = extra.Command
	}
	if extra.ToolArgsJSON != "" {
		base.ToolArgsJSON = extra.ToolArgsJSON
	}
	if len(extra.ToolInput) > 0 {
		base.ToolInput = extra.ToolInput
	}
	if len(extra.ToolOutput) > 0 {
		base.ToolOutput = extra.ToolOutput
	}
	if len(extra.Input) > 0 {
		base.Input = extra.Input
	}
	if len(extra.Output) > 0 {
		base.Output = extra.Output
	}
	if len(extra.Args) > 0 {
		base.Args = extra.Args
	}
	if len(extra.Arguments) > 0 {
		base.Arguments = extra.Arguments
	}
	if len(extra.RawInput) > 0 {
		base.RawInput = extra.RawInput
	}
	if len(extra.RawOutput) > 0 {
		base.RawOutput = extra.RawOutput
	}
	if len(extra.Result) > 0 {
		base.Result = extra.Result
	}
	if len(extra.Content) > 0 {
		base.Content = extra.Content
	}
	if extra.ErrorMessage != "" {
		base.ErrorMessage = extra.ErrorMessage
	}
	if extra.ErrorMessageAlt != "" {
		base.ErrorMessageAlt = extra.ErrorMessageAlt
	}
	if extra.Message != "" {
		base.Message = extra.Message
	}
	return base
}

func grokJSONString(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

func grokRawString(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

func grokJSONDisplay(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err == nil {
		return buf.String()
	}
	return string(raw)
}

func grokRawObjectString(raw json.RawMessage, keys ...string) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	for _, key := range keys {
		if v, ok := obj[key]; ok {
			if s := grokRawString(v); s != "" {
				return s
			}
		}
	}
	return ""
}

func grokContentText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		blocks = []json.RawMessage{raw}
	}
	var parts []string
	for _, block := range blocks {
		if text := grokContentBlockText(block); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func grokContentBlockText(raw json.RawMessage) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return grokRawString(raw)
	}
	if nested, ok := obj["content"]; ok {
		if text := grokContentBlockText(nested); text != "" {
			return text
		}
	}
	for _, key := range []string{"text", "output", "rawOutput", "result"} {
		if v, ok := obj[key]; ok {
			if text := grokRawString(v); text != "" {
				return text
			}
		}
	}
	return ""
}

func grokToolUseFromPayload(p grokToolPayload) ToolUse {
	return ToolUse{
		ID:    grokToolID(p),
		Name:  grokToolName(p),
		Input: grokToolInput(p),
	}
}

func grokToolID(p grokToolPayload) string {
	for _, v := range []string{p.ToolUseID, p.ToolCallID, p.ToolCallIDAlt, p.ToolID, p.ID} {
		if v != "" {
			return v
		}
	}
	return ""
}

func grokToolName(p grokToolPayload) string {
	for _, v := range []string{p.ToolName, p.ToolNameAlt, p.Name} {
		if v != "" {
			return v
		}
	}
	if v := grokRawObjectString(p.RawInput, "tool_name", "toolName", "name"); v != "" {
		return v
	}
	if p.Command != "" {
		return "shell"
	}
	if p.Title != "" {
		return p.Title
	}
	if p.Kind != "" {
		return p.Kind
	}
	return "tool"
}

func grokToolInput(p grokToolPayload) string {
	if s := grokJSONDisplay(p.ToolInput); s != "" {
		return s
	}
	if p.ToolArgsJSON != "" {
		return p.ToolArgsJSON
	}
	for _, raw := range []json.RawMessage{p.Args, p.Arguments, p.Input} {
		if s := grokJSONDisplay(raw); s != "" {
			return s
		}
	}
	if p.Command != "" {
		return p.Command
	}
	if s := grokRawObjectString(p.RawInput, "tool_args_json", "arguments", "args"); s != "" {
		return s
	}
	return grokJSONDisplay(p.RawInput)
}

func grokToolOutput(p grokToolPayload) string {
	for _, raw := range []json.RawMessage{p.ToolOutput, p.Output} {
		if s := grokJSONDisplay(raw); s != "" {
			return s
		}
	}
	if s := grokContentText(p.Content); s != "" {
		return s
	}
	for _, raw := range []json.RawMessage{p.RawOutput, p.Result} {
		if s := grokJSONDisplay(raw); s != "" {
			return s
		}
	}
	for _, v := range []string{p.ErrorMessage, p.ErrorMessageAlt} {
		if v != "" {
			return "error: " + v
		}
	}
	if p.Message != "" && strings.EqualFold(p.Status, "failed") {
		return "error: " + p.Message
	}
	return ""
}

func grokToolPayloadHasOutput(p grokToolPayload) bool {
	return len(bytes.TrimSpace(p.ToolOutput)) > 0 ||
		len(bytes.TrimSpace(p.Output)) > 0 ||
		len(bytes.TrimSpace(p.RawOutput)) > 0 ||
		len(bytes.TrimSpace(p.Result)) > 0 ||
		p.ErrorMessage != "" ||
		p.ErrorMessageAlt != ""
}

func grokToolPayloadIsTerminal(p grokToolPayload) bool {
	switch strings.ToLower(p.Status) {
	case "completed", "failed", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func appendOrUpdateGrokToolUse(toolUses []ToolUse, tu ToolUse) ([]ToolUse, bool) {
	if tu.Name == "" {
		tu.Name = "tool"
	}
	for i := len(toolUses) - 1; i >= 0; i-- {
		if tu.ID != "" {
			if toolUses[i].ID != tu.ID {
				continue
			}
		} else if toolUses[i].Name != tu.Name || toolUses[i].Output != "" {
			continue
		}
		if toolUses[i].Name == "" || toolUses[i].Name == "tool" {
			toolUses[i].Name = tu.Name
		}
		if toolUses[i].Input == "" && tu.Input != "" {
			toolUses[i].Input = tu.Input
		}
		return toolUses, false
	}
	return append(toolUses, tu), true
}

func grokToolKey(id, name string) string {
	if id != "" {
		return "id:" + id
	}
	return "name:" + name
}

// grokHome returns the root directory grok uses for its on-disk state.
// Honors the GROK_HOME env var (per grok's README) and falls back to
// $HOME/.grok otherwise. Returns "" if neither is resolvable so the
// caller can degrade gracefully (skip session-dir interactions
// rather than touching the wrong path).
func grokHome() string {
	if v := strings.TrimSpace(os.Getenv("GROK_HOME")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".grok")
}

// grokEscapePath percent-encodes an absolute path the way grok does
// when it builds its sessions directory entry. Grok escapes every
// byte that is not in the RFC 3986 unreserved set ([A-Za-z0-9-_.~]),
// matching JavaScript's encodeURIComponent / Rust's percent_encoding
// NON_ALPHANUMERIC profile. Go's net/url PathEscape is not strict
// enough — it leaves sub-delimiters like '+', '=', '@', '&', '$', ';'
// untouched and the resulting directory name would not match what
// grok actually wrote.
//
// Verified empirically (kojo/.claude/worktrees/feature+add-grok-cli):
//
//	/private/tmp/grok-test-special+ε=ω@&dir
//	→ %2Fprivate%2Ftmp%2Fgrok-test-special%2B%CE%B5%3D%CF%89%40%26dir
func grokEscapePath(p string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(p) * 3)
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= 'A' && c <= 'Z',
			c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0F])
		}
	}
	return b.String()
}

// grokSessionDir returns the directory grok uses to store per-cwd
// session data: $GROK_HOME/sessions/{encoded(abs(symlink-resolved cwd))}.
// Returns "" if the root cannot be determined.
func grokSessionDir(cwd string) string {
	root := grokHome()
	if root == "" {
		return ""
	}
	resolved := cwd
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		resolved = r
	}
	if !filepath.IsAbs(resolved) {
		if abs, err := filepath.Abs(resolved); err == nil {
			resolved = abs
		}
	}
	return filepath.Join(root, "sessions", grokEscapePath(resolved))
}

// removeGrokSessionDir deletes the {cwd-dir}/{sessionID}/ subtree for
// a single grok session. Used to clean up OneShot sessions whose
// state should not persist.
//
// sessionID is gated through isGrokSessionID AND the resolved path
// is confirmed to live inside the cwd's session directory before
// removal. Without these checks an attacker who could influence
// sessionID (e.g. a hostile MCP server emitting a crafted "end"
// event) could escape the session dir with "../../etc" or similar.
func removeGrokSessionDir(cwd, sessionID string) {
	if !isGrokSessionID(sessionID) {
		return
	}
	dir := grokSessionDir(cwd)
	if dir == "" {
		return
	}
	target := filepath.Join(dir, sessionID)
	// Defence in depth: ensure target is really inside dir after
	// joining, even though UUID validation already forbids "/" and "..".
	rel, err := filepath.Rel(dir, target)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || strings.ContainsRune(rel, filepath.Separator) {
		return
	}
	_ = os.RemoveAll(target)
}

// hasGrokSession reports whether grok has at least one stored session
// for the given cwd. Currently unused (replaced by explicit session ID
// resume) but retained for diagnostic / future fallback use.
func hasGrokSession(cwd string) bool {
	dir := grokSessionDir(cwd)
	if dir == "" {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			return true
		}
	}
	return false
}

// clearGrokSession removes grok's stored sessions for the agent's
// working directory AND clears the persisted resume-ID file. Used by
// ResetData / ResetSession to give the next turn a clean slate.
func clearGrokSession(agentID string) {
	_, _, _ = clearGrokSessionCounted(agentID)
}

// clearGrokSessionCounted is the same as clearGrokSession but reports
// what it removed. Used by truncate_memory.go so the TruncateMemory
// result can surface "how much grok state we dropped".
//
// sessionsRemoved counts UUID-named subdirectories under
// $GROK_HOME/sessions/<encoded(agentDir)>/ — including OneShot sessions
// that might still be lingering. filesRemoved counts every regular
// file under those subdirectories (events.jsonl, chat_history.jsonl,
// summary.json, system_prompt.txt, terminal/*.log, …).
//
// Per-subtree counters are committed only AFTER the subtree's
// RemoveAll succeeds, so a permission failure on one session does not
// inflate the "removed" totals for state that is still on disk.
//
// Concurrency: takes lockGrokSessionTransfer(agentID) across the whole
// clear so a §3.7 device-switch StageGrokSession / ReadGrokSessionFiles
// running on the same agent cannot race the rename-into-place / read
// against our RemoveAll. Lock order is (caller's reset guard) →
// lockGrokSessionTransfer; both ResetData and TruncateMemory hold the
// reset guard before reaching us.
//
// Errors removing individual files are tolerated and logged into the
// returned err (best-effort, matching ResetData's stance). A missing
// session directory or pointer file is NOT an error — both no-op.
func clearGrokSessionCounted(agentID string) (filesRemoved, sessionsRemoved int, err error) {
	release := lockGrokSessionTransfer(agentID)
	defer release()

	dir := agentDir(agentID)
	if sessionsDir := grokSessionDir(dir); sessionsDir != "" {
		entries, derr := os.ReadDir(sessionsDir)
		switch {
		case derr == nil:
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				// Only count grok-shaped session IDs so a stray
				// non-UUID directory (corruption, accidental
				// mkdir) doesn't inflate the counter. The
				// removal below still wipes the whole tree —
				// counting is best-effort metadata, not a gate.
				if !isGrokSessionID(e.Name()) {
					continue
				}
				sub := filepath.Join(sessionsDir, e.Name())
				// Pre-count this subtree's files but do NOT
				// commit the counters until RemoveAll succeeds
				// for the same subtree. Otherwise a permission
				// failure on one session would inflate the
				// "removed" totals for state still on disk.
				subFiles := 0
				_ = filepath.Walk(sub, func(_ string, info os.FileInfo, werr error) error {
					if werr != nil || info == nil {
						return nil
					}
					if !info.IsDir() {
						subFiles++
					}
					return nil
				})
				if rerr := os.RemoveAll(sub); rerr != nil && !os.IsNotExist(rerr) {
					if err == nil {
						err = rerr
					}
					continue
				}
				sessionsRemoved++
				filesRemoved += subFiles
			}
			// Sweep any non-UUID detritus + the now-empty
			// sessionsDir itself so the cwd's per-cwd grok root
			// disappears entirely. Errors here are non-fatal.
			if rerr := os.RemoveAll(sessionsDir); rerr != nil && !os.IsNotExist(rerr) {
				if err == nil {
					err = rerr
				}
			}
		case os.IsNotExist(derr):
			// nothing to remove — fine.
		default:
			err = derr
		}
	}
	if rerr := os.Remove(grokSessionIDFile(dir)); rerr != nil && !os.IsNotExist(rerr) {
		if err == nil {
			err = rerr
		}
	}
	return filesRemoved, sessionsRemoved, err
}

// classifyGrokProcessError picks the most informative error string
// for a finished grok process, or "" when the turn succeeded. See the
// long-form rationale at the Chat callsite for why stderr is NOT
// promoted on its own.
//
// When the process actually failed (waitErr != nil) we keep BOTH the
// wait-error string (carries exit code / signal, e.g. "signal: killed"
// for a SIGTERM after WaitDelay) AND the trimmed stderr (carries the
// human-readable diagnostic). Returning just stderr would let routine
// log lines mask the cause of death; returning just waitErr would
// drop the diagnostic the operator actually needs.
func classifyGrokProcessError(waitErr error, stderr, streamError string) string {
	if streamError != "" {
		return streamError
	}
	if waitErr == nil {
		return ""
	}
	if s := strings.TrimSpace(stderr); s != "" {
		return waitErr.Error() + ": " + s
	}
	return waitErr.Error()
}

// isStaleSessionError reports whether a grok stderr / exit message
// indicates the caller's `--resume <id>` target no longer exists.
// Strings are matched case-insensitively against substrings observed
// in grok 0.1.x; the match is intentionally fuzzy because grok's
// wording is not part of any contract.
func isStaleSessionError(msg string) bool {
	if msg == "" {
		return false
	}
	lower := strings.ToLower(msg)
	staleNeedles := []string{
		"no session found",
		"session not found",
		"unknown session",
		"could not find session",
		"session does not exist",  // grok 0.1.x: "Couldn't create session: Session does not exist"
		"couldn't create session", // same family, different prefix
	}
	for _, n := range staleNeedles {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}

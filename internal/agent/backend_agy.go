package agent

import (
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
	"strconv"
	"strings"
	"syscall"
	"time"
)

// AgyBackend implements ChatBackend for the "gemini-agy" tool: the
// Antigravity CLI (`agy`), Google's successor to gemini-cli. It runs agy
// headlessly with --input-format/--output-format stream-json: the user
// message goes in as one NDJSON "user" event on stdin (no ARG_MAX limit,
// no flag injection through a leading "-"), stdin is closed, and agy
// exits once that turn's "result" event is out.
//
// agy signs in with the operator's Google account (cached credentials in
// the macOS Keychain / ~/.gemini), like claude or grok use the operator's
// own login. kojo does not isolate its home.
//
// Session continuity: the conversation id of the first turn (init /
// result events) is stored per ChatOptions.SessionKey under
// <agentDir>/.agy/ and replayed with --conversation <id>. --continue is
// never used: it picks the most recent conversation for the cwd, which
// would let a Slack thread take over the agent's own conversation.
type AgyBackend struct {
	logger *slog.Logger
}

func NewAgyBackend(logger *slog.Logger) *AgyBackend {
	return &AgyBackend{logger: logger}
}

func (b *AgyBackend) Name() string { return ToolGeminiAgy }

func (b *AgyBackend) Available() bool { return agyPath() != "" }

// agyPath resolves the agy binary. The official installer puts it in
// ~/.local/bin, which a service-launched kojo may not have on PATH.
func agyPath() string {
	if p, err := exec.LookPath("agy"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".local", "bin", "agy")
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// agyEffortModels lists the models agy takes --effort (low/medium/high)
// for: the bare ids of the families `agy models` lists per effort level
// (agy 1.4.8, paid tier). "" is agy's default model (gemini-4-argon on the
// paid tier, gemini-3.8-flash on the free one; both take the ladder).
// Suffixed ids (gemini-4-argon-high, gpt-oss-120b-medium) conflict with
// --effort. Keep in sync with web toolModels.ts.
var agyEffortModels = map[string]bool{
	"":                  true,
	"gemini-4-argon":    true,
	"gemini-3.8-flash":  true,
	"claude-opus-5-5":   true,
	"claude-sonnet-5-5": true,
}

// agyConversationIDPattern matches agy conversation ids (UUIDs). Every
// value read from the agent-writable ref files is checked before it
// reaches argv.
var agyConversationIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func agyStateDir(agentDirPath string) string { return filepath.Join(agentDirPath, ".agy") }

// agyConversationRefPath maps a SessionKey to the file holding its
// conversation id. The key is hashed: it is caller-supplied and must not
// steer the join out of the directory.
func agyConversationRefPath(agentDirPath, sessionKey string) string {
	if sessionKey == "" {
		return filepath.Join(agyStateDir(agentDirPath), "conversation_id")
	}
	return filepath.Join(agyStateDir(agentDirPath), "threads", "key-"+agentIDToUUID(sessionKey))
}

// readAgyConversationID returns the stored id for sessionKey, or "". A
// malformed value is deleted so it cannot keep failing turns.
func readAgyConversationID(agentDirPath, sessionKey string) string {
	path := agyConversationRefPath(agentDirPath, sessionKey)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(data))
	if !agyConversationIDPattern.MatchString(id) {
		_ = os.Remove(path)
		return ""
	}
	return id
}

// agyConversationExists reports whether agy still has the conversation.
// agy keeps each one as ~/.gemini/antigravity-cli/conversations/<id>.db
// and silently starts a new conversation for an id it does not know, so
// the check is what lets a lost conversation get the recent-messages
// bootstrap of a fresh one. When the store cannot be located the id is
// trusted.
func agyConversationExists(id string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return true
	}
	dir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	if _, err := os.Stat(dir); err != nil {
		return true
	}
	_, err = os.Stat(filepath.Join(dir, id+".db"))
	return err == nil
}

func writeAgyConversationID(agentDirPath, sessionKey, id string, logger *slog.Logger) {
	if !agyConversationIDPattern.MatchString(id) {
		logger.Warn("agy: refusing to persist malformed conversation id", "value", id)
		return
	}
	path := agyConversationRefPath(agentDirPath, sessionKey)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		logger.Warn("agy: mkdir for conversation id failed", "path", path, "err", err)
		return
	}
	if err := os.WriteFile(path, []byte(id), 0o644); err != nil {
		logger.Warn("agy: write conversation id failed", "path", path, "err", err)
	}
}

// deleteAgyThreadRefStrict makes the next keyed turn start a fresh
// conversation.
func deleteAgyThreadRefStrict(agentID, sessionKey string) error {
	err := os.Remove(agyConversationRefPath(agentDir(agentID), sessionKey))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// agyCanResumeSession reports whether a keyed conversation ref exists.
// agy keeps no per-conversation file kojo can probe, so a ref whose
// conversation agy lost is only found out (and dropped) by the turn that
// tries to resume it.
func agyCanResumeSession(agentID, sessionKey string) bool {
	if sessionKey == "" {
		return false
	}
	id := readAgyConversationID(agentDir(agentID), sessionKey)
	return id != "" && agyConversationExists(id)
}

// clearAgySessionCounted removes every conversation ref of the agent, so
// the next turn of every key starts a fresh conversation.
func clearAgySessionCounted(agentID string) (int, error) {
	dir := agyStateDir(agentDir(agentID))
	n := 0
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	if err := os.RemoveAll(dir); err != nil {
		return 0, err
	}
	return n, nil
}

// agyEnvStrip lists the inherited variables a turn must not see.
var agyEnvStrip = []string{"AGENT_BROWSER_SESSION", "AGENT_BROWSER_COOKIE_DIR"}

// agyPrintTimeout disables agy's own print-mode deadline; kojo's turn
// timeout bounds the run.
const agyPrintTimeout = "0s"

// agyExitGrace bounds how long agy may keep running after it printed the
// turn's result event. With stdin closed it should exit on its own; one
// that does not (e.g. waiting on a background task) is terminated so the
// turn does not hang.
var agyExitGrace = 30 * time.Second

// buildAgyArgs renders the argv of one run; agentName is the custom agent
// to select, or "" for none.
func buildAgyArgs(resumeID string, agent *Agent, agentName string) []string {
	args := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--print-timeout", agyPrintTimeout,
		// Headless mode: tools needing approval would be soft-denied.
		"--dangerously-skip-permissions",
		// A message starting with "/" is text for the model, not a CLI
		// command (which would end a stream-json session with an error).
		"--disable-slash-commands",
	}
	if resumeID != "" {
		args = append(args, "--conversation", resumeID)
	}
	if agentName != "" {
		args = append(args, "--agent", agentName)
	}
	if agent.Model != "" {
		args = append(args, "--model", agent.Model)
	}
	// A stored effort the model has no ladder for would fail the run
	// ("--effort is not supported for model ...").
	if agent.Effort != "" && ValidToolModelEffort(ToolGeminiAgy, agent.Model, agent.Effort) {
		args = append(args, "--effort", agent.Effort)
	}
	return args
}

// agyUserMessageLine renders the stdin event carrying the prompt.
func agyUserMessageLine(text string) ([]byte, error) {
	b, err := json.Marshal(map[string]any{
		"event":   "user",
		"message": map[string]any{"content": text},
	})
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func (b *AgyBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, error) {
	bin := agyPath()
	if bin == "" {
		return nil, fmt.Errorf("agy (Antigravity CLI) not found in PATH")
	}
	dir := agentDir(agent.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create agent dir: %w", err)
	}

	persistKey := opts.SessionKey
	var resumeID string
	if !opts.OneShot {
		resumeID = readAgyConversationID(dir, persistKey)
		if resumeID != "" && !agyConversationExists(resumeID) {
			b.logger.Info("agy: stored conversation is gone, starting fresh",
				"agent", agent.ID, "staleId", resumeID)
			_ = os.Remove(agyConversationRefPath(dir, persistKey))
			resumeID = ""
		}
	}

	userMessage = injectSessionContexts(userMessage, opts, resumeID != "")
	// A fresh conversation has no history of its own: bootstrap it with the
	// bounded transcript excerpt, as the grok / claude backends do.
	if !opts.OneShot && resumeID == "" && opts.FreshSessionContext == "" && opts.RecentMessagesContext != "" {
		userMessage = injectRecentMessagesContext(userMessage, opts.RecentMessagesContext)
	}
	input, err := agyUserMessageLine(userMessage)
	if err != nil {
		return nil, fmt.Errorf("encode prompt: %w", err)
	}

	mcpConfig, err := renderAgyMCPConfig(opts.MCPServers)
	if err != nil {
		return nil, fmt.Errorf("render agy MCP config: %w", err)
	}
	// The custom agent is named after its prompt: a thread's prompt differs
	// from the main chat's, and a shared name would make one wait for the
	// other's agy to exit. (agy fixes the prompt when it creates a
	// conversation; a resumed one keeps its own whatever --agent says.)
	files := map[string]string{agyMCPConfigPath(dir): mcpConfig}
	var agentName string
	var ephemeral []string
	if systemPrompt != "" {
		agentName = agyAgentName(systemPrompt)
		defPath := agyAgentDefPath(dir, agentName)
		files[defPath] = renderAgyAgentDef(agentName, systemPrompt)
		ephemeral = append(ephemeral, defPath)
	}
	release, err := holdAgyWorkspace(ctx, dir, files, ephemeral...)
	if err != nil {
		return nil, fmt.Errorf("prepare agy workspace: %w", err)
	}

	cmd := exec.CommandContext(ctx, bin, buildAgyArgs(resumeID, agent, agentName)...)
	cmd.Env = appendKojoTurnEnv(filterEnv(agyEnvStrip, agent.ID, dir), opts)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(input)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second

	var stderrBuf bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderrBuf, limit: 4096}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		release()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		release()
		return nil, fmt.Errorf("start agy: %w", err)
	}

	ch := make(chan ChatEvent, 64)
	go func() {
		defer close(ch)
		defer release()

		send := func(e ChatEvent) bool { return ctxSend(ctx, ch, e) }

		var exitTimer *time.Timer
		onResult := func() {
			exitTimer = time.AfterFunc(agyExitGrace, func() {
				b.logger.Warn("agy: still running after its result event, terminating", "agent", agent.ID)
				_ = cmd.Process.Signal(syscall.SIGTERM)
			})
		}
		thoughts := newAgyThoughtReader(b.logger)
		result := parseAgyStream(stdout, b.logger, send, onResult, thoughts.Lookup)
		thoughts.Close()
		waitErr := cmd.Wait()
		killedAfterResult := false
		if exitTimer != nil {
			killedAfterResult = !exitTimer.Stop()
		}

		if result.cancelled || ctx.Err() != nil {
			emitCancelDone(ctx, ch, result.text, result.thinking, result.toolUses, result.usage)
			return
		}

		if killedAfterResult && result.gotResult {
			waitErr = nil
		}
		processError := classifyAgyError(result, waitErr, stderrBuf.String())
		if processError != "" {
			b.logger.Warn("agy turn failed", "agent", agent.ID, "err", processError,
				"waitErr", waitErr, "stderr", stderrBuf.String())
		}

		// A stored conversation agy no longer knows fails the turn before
		// any output; drop the ref so the next turn starts fresh instead of
		// failing the same way.
		if resumeID != "" && result.text == "" && processError != "" && isAgyStaleConversationError(processError) {
			_ = os.Remove(agyConversationRefPath(dir, persistKey))
			b.logger.Info("agy: dropped stale conversation id after resume failure",
				"agent", agent.ID, "staleId", resumeID)
		}

		if processError != "" && result.text == "" {
			send(ChatEvent{Type: "error", ErrorMessage: processError})
			return
		}

		if !opts.OneShot && result.conversationID != "" && result.conversationID != resumeID {
			writeAgyConversationID(dir, persistKey, result.conversationID, b.logger)
		}

		msg := assembleAssistantMessage(result.text, result.thinking, result.toolUses, result.usage)
		send(ChatEvent{Type: "done", Message: msg, Usage: result.usage, ErrorMessage: processError})
	}()
	return ch, nil
}

// classifyAgyError turns the outcome of one agy run into the message the
// user sees, or "" for a successful turn.
func classifyAgyError(res *agyStreamResult, waitErr error, stderr string) string {
	if res.gotResult && res.status != "SUCCESS" {
		msg := res.errorMessage
		if msg == "" {
			msg = "agy run ended with status " + res.status
		}
		return agyHintAuth(msg, stderr)
	}
	if waitErr != nil {
		msg := "agy: " + waitErr.Error()
		if s := strings.TrimSpace(stderr); s != "" {
			msg += ": " + s
		}
		return agyHintAuth(msg, stderr)
	}
	if !res.gotResult {
		msg := "agy exited without a result"
		if s := strings.TrimSpace(stderr); s != "" {
			msg += ": " + s
		}
		return msg
	}
	return ""
}

// agyHintAuth points a sign-in failure at its fix: headless agy cannot
// sign in by itself.
func agyHintAuth(msg, stderr string) string {
	lower := strings.ToLower(msg + "\n" + stderr)
	if strings.Contains(lower, "authentication required") || strings.Contains(lower, "authentication failed") {
		return msg + " — sign in by running `agy` once in a terminal on this host"
	}
	return msg
}

func isAgyStaleConversationError(msg string) bool {
	lower := strings.ToLower(msg)
	if !strings.Contains(lower, "conversation") {
		return false
	}
	for _, n := range []string{"not found", "no conversation", "does not exist", "unknown", "invalid"} {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}

// agyStreamResult collects the stream of one agy run.
type agyStreamResult struct {
	text           string
	thinking       string
	toolUses       []ToolUse
	usage          *Usage
	conversationID string
	status         string
	errorMessage   string
	response       string
	gotResult      bool
	cancelled      bool
}

type agyUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ThinkingTokens  int `json:"thinking_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
}

type agyToolInfo struct {
	Name       string          `json:"name"`
	Parameters json.RawMessage `json:"parameters"`
	Output     json.RawMessage `json:"output"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type agyStreamEvent struct {
	Event          string `json:"event"`
	ConversationID string `json:"conversation_id"`
	StepUpdate     *struct {
		ConversationID string       `json:"conversation_id"`
		StepIndex      int          `json:"step_index"`
		State          string       `json:"state"`
		StepType       string       `json:"step_type"`
		ToolName       string       `json:"tool_name"`
		TextDelta      string       `json:"text_delta"`
		ThinkingDelta  string       `json:"thinking_delta"`
		ToolInfo       *agyToolInfo `json:"tool_info"`
	} `json:"step_update"`
	Result *struct {
		ConversationID string    `json:"conversation_id"`
		Status         string    `json:"status"`
		Response       string    `json:"response"`
		Error          string    `json:"error"`
		Usage          *agyUsage `json:"usage"`
	} `json:"result"`
}

// parseAgyStream reads agy --output-format stream-json events:
//
//	{"event":"init","conversation_id":"...","init":{...}}
//	{"event":"step_update","step_update":{"step_index":n,"state":"ACTIVE|DONE",
//	    "step_type":"agent_response|tool|user_input|...","text_delta":"...",
//	    "tool_name":"...","tool_info":{"name","parameters","output","error"}}}
//	{"event":"result","result":{"status","response","error","usage",...}}
//
// text_delta fragments are incremental on both ACTIVE and DONE events.
// Unknown events and step types are skipped. onResult is called once, when
// the result event arrives. A send that returns false (context cancelled)
// stops parsing and marks the result cancelled.
//
// thought, when non-nil, fetches the thought summary of an agent_response
// step that stream-json withholds; ok=false means "not stored yet", so it
// is retried on that step's later events (until DONE).
func parseAgyStream(r io.Reader, logger *slog.Logger, send func(ChatEvent) bool, onResult func(), thought func(convID string, idx int) (string, bool)) *agyStreamResult {
	res := &agyStreamResult{}
	scanner := newSkippingLineScanner(r)
	loggedSkips := 0
	var textBuf, thinkBuf strings.Builder
	var toolUses []ToolUse
	toolStarted := map[int]bool{}
	toolDone := map[int]bool{}
	thoughtDone := map[int]bool{}
	var thoughtPending []int // agent_response steps whose thought was not readable by DONE
	emitThought := func(t string) bool {
		if thinkBuf.Len() > 0 {
			t = "\n\n" + t
		}
		thinkBuf.WriteString(t)
		return send(ChatEvent{Type: "thinking", Delta: t})
	}
	finish := func(cancelled bool) *agyStreamResult {
		res.text = textBuf.String()
		res.thinking = thinkBuf.String()
		res.toolUses = toolUses
		res.cancelled = cancelled
		if !cancelled && res.text == "" && res.response != "" {
			res.text = res.response
		}
		return res
	}

	for scanner.Scan() {
		if n := scanner.Skipped(); n > loggedSkips {
			logger.Warn("agy stream: dropped oversized JSONL line(s)", "dropped", n-loggedSkips)
			loggedSkips = n
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev agyStreamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			logger.Debug("agy parse line", "err", err, "line", string(line))
			continue
		}
		switch ev.Event {
		case "init":
			if ev.ConversationID != "" {
				res.conversationID = ev.ConversationID
			}
		case "step_update":
			su := ev.StepUpdate
			if su == nil {
				continue
			}
			if su.ConversationID != "" && res.conversationID == "" {
				res.conversationID = su.ConversationID
			}
			if su.ThinkingDelta != "" {
				thinkBuf.WriteString(su.ThinkingDelta)
				if !send(ChatEvent{Type: "thinking", Delta: su.ThinkingDelta}) {
					return finish(true)
				}
			}
			switch su.StepType {
			case "agent_response":
				if thought != nil && !thoughtDone[su.StepIndex] && su.ThinkingDelta == "" {
					convID := su.ConversationID
					if convID == "" {
						convID = res.conversationID
					}
					t, ok := thought(convID, su.StepIndex)
					if (ok && t != "") || su.State == "DONE" {
						thoughtDone[su.StepIndex] = true
						if !ok && su.State == "DONE" {
							thoughtPending = append(thoughtPending, su.StepIndex)
						}
					}
					if t != "" && !emitThought(t) {
						return finish(true)
					}
				}
				if su.TextDelta != "" {
					textBuf.WriteString(su.TextDelta)
					if !send(ChatEvent{Type: "text", Delta: su.TextDelta}) {
						return finish(true)
					}
				}
			case "tool":
				id := "agy-step-" + strconv.Itoa(su.StepIndex)
				name := su.ToolName
				var input string
				if su.ToolInfo != nil {
					if name == "" {
						name = su.ToolInfo.Name
					}
					input = agyRawJSON(su.ToolInfo.Parameters)
				}
				if !toolStarted[su.StepIndex] {
					toolStarted[su.StepIndex] = true
					toolUses = append(toolUses, ToolUse{ID: id, Name: name, Input: input})
					if !send(ChatEvent{Type: "tool_use", ToolUseID: id, ToolName: name, ToolInput: input}) {
						return finish(true)
					}
				} else if input != "" {
					for i := range toolUses {
						if toolUses[i].ID == id && toolUses[i].Input == "" {
							toolUses[i].Input = input
						}
					}
				}
				if su.State == "DONE" && !toolDone[su.StepIndex] {
					toolDone[su.StepIndex] = true
					output := ""
					if su.ToolInfo != nil {
						output = agyToolOutput(su.ToolInfo)
					}
					matchToolOutput(toolUses, id, name, output)
					if !send(ChatEvent{Type: "tool_result", ToolUseID: id, ToolName: name, ToolOutput: output}) {
						return finish(true)
					}
				}
			}
		case "result":
			r := ev.Result
			if r == nil {
				continue
			}
			res.gotResult = true
			if r.ConversationID != "" {
				res.conversationID = r.ConversationID
			}
			// Last chance for thoughts that were not readable when their
			// step finished (store write lagging the stream).
			for _, idx := range thoughtPending {
				if t, _ := thought(res.conversationID, idx); t != "" && !emitThought(t) {
					return finish(true)
				}
			}
			thoughtPending = nil
			res.status = r.Status
			res.errorMessage = r.Error
			res.response = r.Response
			if r.ConversationID != "" {
				res.conversationID = r.ConversationID
			}
			if r.Usage != nil {
				res.usage = &Usage{
					InputTokens:          r.Usage.InputTokens,
					OutputTokens:         r.Usage.OutputTokens + r.Usage.ThinkingTokens,
					CacheReadInputTokens: r.Usage.CacheReadTokens,
				}
			}
			if onResult != nil {
				onResult()
				onResult = nil
			}
		}
	}
	return finish(false)
}

// agyRawJSON renders a JSON value for display: strings unquoted, null as "".
func agyRawJSON(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if raw[0] == '"' && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func agyToolOutput(ti *agyToolInfo) string {
	out := agyRawJSON(ti.Output)
	if ti.Error != nil && ti.Error.Message != "" {
		if out != "" {
			return out + "\n" + ti.Error.Message
		}
		return ti.Error.Message
	}
	return out
}

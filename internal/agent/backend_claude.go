package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// claudeStdinWriter guards writes to a running Claude CLI process's stdin
// pipe, which is kept open for the duration of the turn (--input-format
// stream-json) so a mid-turn steer message can be injected as a second
// "user" JSON line. Closed exactly once, either after the stream's "result"
// event is observed or on context cancellation/process exit.
type claudeStdinWriter struct {
	mu     sync.Mutex
	w      io.WriteCloser
	closed bool
}

// writeUserLine marshals text as a stream-json user-message line and writes
// it to the pipe. Returns an error if the pipe has already been closed
// (i.e. the turn already finished or the process exited).
func (s *claudeStdinWriter) writeUserLine(text string) error {
	line, err := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": text},
			},
		},
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		// The turn already produced its result (or the process exited) —
		// surface this as "not busy" so the HTTP layer maps it to 409 and
		// the frontend falls back to a normal send.
		return ErrAgentNotBusy
	}
	if _, err = s.w.Write(append(line, '\n')); err != nil {
		// A broken/closed pipe means the process died mid-turn — same
		// caller-facing meaning as the closed flag above.
		if errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrClosed) {
			return ErrAgentNotBusy
		}
		return err
	}
	return nil
}

// writeControlResponse writes a control_response line answering a CLI
// control_request (the can_use_tool permission prompt). resp is the inner
// "response" object (e.g. {"behavior":"allow","updatedInput":{...}} or
// {"behavior":"deny","message":"..."}). Same closed/EPIPE handling as
// writeUserLine so a response after the turn ended surfaces ErrAgentNotBusy.
func (s *claudeStdinWriter) writeControlResponse(requestID string, resp map[string]any) error {
	line, err := json.Marshal(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   resp,
		},
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrAgentNotBusy
	}
	if _, err = s.w.Write(append(line, '\n')); err != nil {
		if errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrClosed) {
			return ErrAgentNotBusy
		}
		return err
	}
	return nil
}

// close closes the underlying pipe exactly once. Safe to call multiple
// times (e.g. once from the "result" observer and once from a deferred
// cleanup on an early-return path).
func (s *claudeStdinWriter) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	_ = s.w.Close()
}

// claudeTurnSteer gates steer writes to a SINGLE turn on a persistent claude
// session. The session's stdin pipe stays open across turns, so
// claudeStdinWriter.closed cannot distinguish "this turn is over" from "the
// process is dead" — writeUserLine would happily inject a steer line into a
// pipe the CLI has already stopped reading for the finished turn (the line
// then either merges into the NEXT turn or is dropped), yet return nil so the
// caller sees a false success. markOver, called at the result boundary before
// any teardown bookkeeping, closes that window so a late steer returns
// ErrAgentNotBusy and the Manager falls back to a fresh turn.
type claudeTurnSteer struct {
	stdinW *claudeStdinWriter
	over   atomic.Bool
	// writes counts user lines successfully injected into this turn. The
	// abort watcher reads it: the CLI does NOT discard queued steer lines on
	// interrupt (it auto-starts a fresh turn per queued line right after the
	// aborted turn's result), so an abort of a steered turn must arm the
	// queued-steer reaper (see claudeSession.killQueuedSteerTurns).
	writes atomic.Int32
}

// writeUserLine is the per-turn SteerFunc. It refuses (ErrAgentNotBusy) once
// the turn's result has been observed, otherwise delegates to the shared stdin
// writer. `over` is atomic (not a mutex held across the write) so markOver —
// called from the readLoop at the result boundary — never blocks behind an
// in-flight stdin write, which would stall turn completion and leave the
// agent stuck busy. The residual micro-race (over flips between the Load and
// the write) is benign: a steer line is smaller than the pipe buffer so the
// write can't wedge, and if the process already exited the write surfaces
// EPIPE → ErrAgentNotBusy, the same fallback outcome.
func (t *claudeTurnSteer) writeUserLine(text string) error {
	if t.over.Load() {
		return ErrAgentNotBusy
	}
	// Count BEFORE the write (rolled back on failure) so the abort watcher —
	// which reads the counter right after the turn's result — can never miss
	// a steer whose write succeeded but whose post-write increment hadn't
	// executed yet. Overcounting a failed write is corrected immediately;
	// the reaper deadline bounds any residual overshoot anyway.
	t.writes.Add(1)
	if err := t.stdinW.writeUserLine(text); err != nil {
		t.writes.Add(-1)
		return err
	}
	return nil
}

// markOver marks the turn finished so subsequent writeUserLine calls fail
// with ErrAgentNotBusy. Idempotent, nil-safe, and non-blocking.
func (t *claudeTurnSteer) markOver() {
	if t == nil {
		return
	}
	t.over.Store(true)
}

// controlRequestMsg is the CLI's can_use_tool permission prompt, emitted on
// stdout when --permission-prompt-tool stdio is set and the model calls a tool
// that needs a decision. Under bypassPermissions the only tool that emits one
// is AskUserQuestion (requires_user_interaction), but the generic can_use_tool
// shape is handled defensively.
type controlRequestMsg struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Request   struct {
		Subtype     string          `json:"subtype"`
		ToolName    string          `json:"tool_name"`
		DisplayName string          `json:"display_name"`
		Input       json.RawMessage `json:"input"`
		ToolUseID   string          `json:"tool_use_id"`
	} `json:"request"`
	RequiresUserInteraction bool `json:"requires_user_interaction"`
}

// maybeControlRequest cheaply detects and decodes a control_request line. It
// returns (nil,false) for any other line so the caller falls through to the
// normal stream decode.
func maybeControlRequest(line string) (*controlRequestMsg, bool) {
	if !strings.Contains(line, "\"control_request\"") {
		return nil, false
	}
	var cr controlRequestMsg
	if err := json.Unmarshal([]byte(line), &cr); err != nil {
		return nil, false
	}
	if cr.Type != "control_request" {
		return nil, false
	}
	return &cr, true
}

// automatedDenyMessage is written back when the model calls AskUserQuestion on
// a turn that can never surface a question to a user (no answer channel wired,
// e.g. a Slack one-shot). Automated turns that CAN surface a card use the
// timeout path (see automatedQuestionTimeout) instead of denying immediately.
const automatedDenyMessage = "No user is watching this automated turn; continue without asking and use your best judgment."

// automatedQuestionTimeout bounds how long an automated turn (cron, background
// notification, restart-wake) holds an AskUserQuestion open waiting for a human
// who may not be watching. On expiry the question is auto-denied so a blocked
// CLI eventually proceeds. User turns pass timeout 0 (held until turn end/abort,
// unchanged).
const automatedQuestionTimeout = 10 * time.Minute

// automatedQuestionTimeoutMessage is the deny wording used when an automated
// turn's question goes unanswered past automatedQuestionTimeout.
const automatedQuestionTimeoutMessage = "No answer arrived within the time limit for this automated turn; continue without asking and use your best judgment."

// claudeQuestionState tracks the interactive AskUserQuestion prompts pending on
// a live CLI process and answers them by writing control_response lines. One
// instance is shared between the stream reader (which registers a pending
// question and emits a user_question event) and the AnswerFunc the Manager
// calls to resolve it. Safe for concurrent use.
type claudeQuestionState struct {
	stdinW *claudeStdinWriter

	mu sync.Mutex
	// pending maps a requestID to the CLI's original tool input (the whole
	// {"questions":[...]} object) so the allow control_response can echo the
	// questions array back verbatim alongside the answers.
	pending map[string]json.RawMessage
	// timers holds the auto-deny timer for questions registered with a
	// timeout (automated turns). Stopped and cleared when the question is
	// answered or the turn ends, so a resolved question never fires a late deny.
	timers map[string]*time.Timer
	// onResolved, if set, is called with a resolved question's requestID
	// from answer() and denyAllPending() — the two paths that actually
	// remove an entry from pending. Mirrors ChatOptions.OnQuestionResolved;
	// set via setOnResolved so the Manager's callback (which closes over
	// the current turn's agentID) can be kept fresh across turns on a
	// long-lived persistent session without re-allocating the state.
	onResolved func(requestID string)
}

func newClaudeQuestionState(stdinW *claudeStdinWriter) *claudeQuestionState {
	return &claudeQuestionState{
		stdinW:  stdinW,
		pending: make(map[string]json.RawMessage),
		timers:  make(map[string]*time.Timer),
	}
}

// setOnResolved (re)wires the OnQuestionResolved callback. Safe to call
// concurrently with answer()/denyAllPending() (guarded by q.mu) and with a
// nil receiver (no-op) so callers don't need to nil-check qstate first.
func (q *claudeQuestionState) setOnResolved(fn func(requestID string)) {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.onResolved = fn
	q.mu.Unlock()
}

// register records a pending question so a later answer can rebuild its
// control_response. When timeout > 0 it also arms a timer that auto-denies the
// question with timeoutMsg if no answer arrives — used for automated turns
// nobody may be watching so a blocked CLI eventually proceeds.
func (q *claudeQuestionState) register(requestID string, input json.RawMessage, timeout time.Duration, timeoutMsg string) {
	q.mu.Lock()
	q.pending[requestID] = append(json.RawMessage(nil), input...)
	if timeout > 0 {
		q.timers[requestID] = time.AfterFunc(timeout, func() {
			// answer() is a no-op (ErrQuestionNotFound) if the question was
			// already resolved, so a race with a real answer is harmless.
			_ = q.answer(requestID, nil, true, timeoutMsg)
		})
	}
	q.mu.Unlock()
}

// stopTimerLocked stops and clears any auto-deny timer for requestID. Caller
// holds q.mu.
func (q *claudeQuestionState) stopTimerLocked(requestID string) {
	if t := q.timers[requestID]; t != nil {
		t.Stop()
		delete(q.timers, requestID)
	}
}

// answer implements AnswerFunc: it builds and writes the control_response for a
// pending question. Returns ErrQuestionNotFound if requestID is unknown.
func (q *claudeQuestionState) answer(requestID string, answers map[string]any, deny bool, denyMessage string) error {
	q.mu.Lock()
	input, ok := q.pending[requestID]
	var onResolved func(string)
	if ok {
		delete(q.pending, requestID)
		q.stopTimerLocked(requestID)
		onResolved = q.onResolved
	}
	q.mu.Unlock()
	if !ok {
		return ErrQuestionNotFound
	}
	if onResolved != nil {
		onResolved(requestID)
	}
	var resp map[string]any
	if deny {
		if denyMessage == "" {
			denyMessage = "The user declined to answer."
		}
		resp = map[string]any{"behavior": "deny", "message": denyMessage}
	} else {
		// Echo the original questions array back with the answers map, as the
		// CLI expects for behavior=allow (see updatedInput contract).
		var orig struct {
			Questions json.RawMessage `json:"questions"`
		}
		_ = json.Unmarshal(input, &orig)
		resp = map[string]any{
			"behavior": "allow",
			"updatedInput": map[string]any{
				"questions": orig.Questions,
				"answers":   answers,
			},
		}
	}
	return q.stdinW.writeControlResponse(requestID, resp)
}

// denyAllPending writes a deny control_response for every still-pending
// question and clears the map. Called when a turn ends/aborts with a question
// unanswered so the CLI isn't left blocked (best-effort — a dead process just
// fails the write). Safe to call with a nil receiver.
func (q *claudeQuestionState) denyAllPending(reason string) {
	if q == nil {
		return
	}
	q.mu.Lock()
	ids := make([]string, 0, len(q.pending))
	for id := range q.pending {
		ids = append(ids, id)
		q.stopTimerLocked(id)
	}
	q.pending = make(map[string]json.RawMessage)
	onResolved := q.onResolved
	q.mu.Unlock()
	for _, id := range ids {
		// Clear the caller's pending-question bookkeeping FIRST: the
		// control_response write below is best-effort (a dead/wedged
		// process can block or fail it), and if onResolved ran after a
		// blocked write, Agent.AwaitingAnswer would stay stuck on for as
		// long as the write stalls instead of clearing immediately.
		if onResolved != nil {
			onResolved(id)
		}
		_ = q.stdinW.writeControlResponse(id, map[string]any{"behavior": "deny", "message": reason})
	}
}

// handleControlRequest resolves a decoded can_use_tool control_request. It
// never blocks: non-AskUserQuestion tools are auto-allowed (preserving
// bypassPermissions semantics); AskUserQuestion is auto-denied only when no
// answer channel exists (qstate nil), and otherwise registered as pending with
// a user_question event emitted via send for the UI to render.
//
// questionTimeout controls how long the question is held: 0 means hold until
// the turn ends/aborts (watched user turns), and a positive value arms an
// auto-deny timer (automated turns nobody may be watching) so the CLI is not
// blocked indefinitely. Either way the card is surfaced so a watching human can
// answer, and an answer that arrives in time works identically to a user turn.
func handleControlRequest(cr *controlRequestMsg, stdinW *claudeStdinWriter, qstate *claudeQuestionState, questionTimeout time.Duration, logger *slog.Logger, send func(ChatEvent) bool) {
	if cr.Request.Subtype != "can_use_tool" {
		// Other control_request subtypes (e.g. our own interrupt's response
		// path) are not permission prompts; nothing to answer here.
		return
	}
	if cr.Request.ToolName != "AskUserQuestion" {
		// Under bypassPermissions normal tools don't emit these, but answer
		// defensively so an unexpected prompt never wedges the turn.
		if err := stdinW.writeControlResponse(cr.RequestID, map[string]any{
			"behavior":     "allow",
			"updatedInput": cr.Request.Input,
		}); err != nil {
			logger.Debug("auto-allow control_request write failed", "tool", cr.Request.ToolName, "err", err)
		}
		return
	}
	if qstate == nil {
		// No answer channel exists for this turn (e.g. a one-shot with no
		// OnQuestionReady): the question can never be surfaced or answered, so
		// deny immediately rather than leave the CLI blocked.
		if err := stdinW.writeControlResponse(cr.RequestID, map[string]any{
			"behavior": "deny",
			"message":  automatedDenyMessage,
		}); err != nil {
			logger.Debug("auto-deny AskUserQuestion write failed", "err", err)
		}
		return
	}
	// Register the pending question (arming an auto-deny timer for automated
	// turns) and surface it to the UI.
	qstate.register(cr.RequestID, cr.Request.Input, questionTimeout, automatedQuestionTimeoutMessage)
	var inp struct {
		Questions json.RawMessage `json:"questions"`
	}
	_ = json.Unmarshal(cr.Request.Input, &inp)
	if !send(ChatEvent{
		Type:      "user_question",
		RequestID: cr.RequestID,
		ToolUseID: cr.Request.ToolUseID,
		Questions: inp.Questions,
	}) {
		// The turn was cancelled / the consumer went away before the question
		// reached any UI: nobody can answer it, so deny now instead of leaving
		// the CLI blocked on a control_response.
		_ = qstate.answer(cr.RequestID, nil, true, automatedDenyMessage)
	}
}

// ClaudeBackend implements ChatBackend using the Claude CLI with stream-json output.
type ClaudeBackend struct {
	logger   *slog.Logger
	proxyURL string // if set, injected as ANTHROPIC_BASE_URL
	// ephemeral disables the persistent-session pool for throwaway backend
	// instances (e.g. the custom backend's per-turn delegate) that the
	// Manager cannot own or close.
	ephemeral bool

	// Persistent-session pool (Phase 3). Keyed by agent ID. Only the main
	// agent chat (non-oneShot, empty SessionKey) uses a persistent process;
	// oneShot/thread turns keep the per-turn spawn model. Guarded by sessMu.
	sessMu   sync.Mutex
	sessions map[string]*claudeSession
	// onBackgroundTurn is invoked by a session when it observes an
	// unsolicited turn (a background subagent notification) so the Manager
	// can persist + broadcast it as a background chat. Set by the Manager
	// at construction; nil disables background-turn surfacing.
	onBackgroundTurn BackgroundTurnFunc

	// onSubagentActivity is invoked by a session's subagent tailer when a
	// background subagent (Task run_in_background) emits output after the
	// spawning turn already finalized. The Manager durably attaches it to the
	// owning message's ToolUse.Children and pushes it live. nil disables
	// background-subagent surfacing.
	onSubagentActivity subagentActivityFunc

	// onRateLimit is invoked by a session when a rate_limit_event arrives
	// while the session is IDLE (post-result usage-window telemetry that
	// opens no turn). During a turn the same telemetry rides the turn sink to
	// the Manager tap; the idle path has no sink, so it routes here directly
	// so the snapshot is still recorded. nil drops idle telemetry.
	onRateLimit func(agentID string, info RateLimitInfo)
}

// BackgroundTurnFunc consumes an unsolicited turn's events (a background
// subagent notification arriving on the persistent process with no user
// input) and persists + broadcasts them. The channel is closed by the
// session when the turn's result arrives. answer resolves an AskUserQuestion
// raised during the turn (nil if the session has no answer channel), letting a
// watching human respond to a question on an otherwise-automated turn.
// abort interrupts the turn on the CLI itself (nil when the turn is already
// complete, e.g. an absorbed racing notification). Without it the Manager's
// Abort could only cancel event processing — the CLI kept generating, so a
// stop pressed on an unsolicited turn (including the auto-turn a queued steer
// starts after an abort) looked accepted and then visibly resumed, and the
// busy slot stayed wedged until a kojo restart.
type BackgroundTurnFunc func(agentID string, events <-chan ChatEvent, answer AnswerFunc, abort func())

func NewClaudeBackend(logger *slog.Logger) *ClaudeBackend {
	return &ClaudeBackend{logger: logger, sessions: make(map[string]*claudeSession)}
}

// SetBackgroundTurnHandler registers the Manager callback used to surface
// unsolicited (background subagent notification) turns.
func (b *ClaudeBackend) SetBackgroundTurnHandler(fn BackgroundTurnFunc) {
	b.onBackgroundTurn = fn
}

// SetSubagentActivityHandler registers the Manager callback used to surface
// live/backfilled background-subagent output (Option A + C).
func (b *ClaudeBackend) SetSubagentActivityHandler(fn subagentActivityFunc) {
	b.onSubagentActivity = fn
}

// SetRateLimitHandler registers the Manager callback used to record rate-limit
// telemetry that arrives while the persistent session is idle.
func (b *ClaudeBackend) SetRateLimitHandler(fn func(agentID string, info RateLimitInfo)) {
	b.onRateLimit = fn
}

// SetProxyURL configures an ANTHROPIC_BASE_URL to inject into Claude CLI env.
func (b *ClaudeBackend) SetProxyURL(url string) {
	b.proxyURL = url
}

func (b *ClaudeBackend) Name() string { return "claude" }

func (b *ClaudeBackend) Available() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

func (b *ClaudeBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, error) {
	// Persistent-session fast path: the agent's main chat (non-oneShot,
	// no per-conversation SessionKey) reuses one live process across turns
	// so background subagents and their late notifications survive between
	// turns. OneShot/thread turns keep the per-turn spawn model below.
	if persistentSessionsEnabled() && !b.ephemeral && !opts.OneShot && opts.SessionKey == "" {
		return b.chatViaSession(ctx, agent, userMessage, systemPrompt, opts)
	}

	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("claude not found in PATH")
	}

	dir := agentDir(agent.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create agent dir: %w", err)
	}

	// SystemPromptExtra is appended by the manager before reaching us — see
	// Manager.ChatOneShot. The backend treats systemPrompt as the final
	// system prompt to pass to --system-prompt, with the cacheable prefix
	// already placed at offset 0.
	inv := b.buildClaudeInvocation(agent, systemPrompt, dir, opts.OneShot, opts.MCPServers, opts.AutomatedTrigger, opts.SessionKey)
	args := inv.args
	if inv.bootstrapRecentContext && opts.RecentMessagesContext != "" {
		userMessage = injectRecentMessagesContext(userMessage, opts.RecentMessagesContext)
	}

	// Expected session ID for THIS branch, used as the recovery fallback if
	// Claude exits before result.streamSessionID is set. Without this anchor,
	// recoverFromSession would pick the most-recently-modified JSONL in the
	// project dir — which under concurrent Slack threads can be a different
	// thread's file, causing cross-thread text contamination.
	expectedSessionID := expectedClaudeSessionID(agent.ID, opts.SessionKey, opts.OneShot)

	cmd := exec.CommandContext(ctx, claudePath, args...)
	cmd.Env = filterEnv([]string{"CLAUDE_CODE", "CLAUDECODE", "AGENT_BROWSER_SESSION", "AGENT_BROWSER_COOKIE_DIR"}, agent.ID, dir)
	// Token conservation: agents persist state in files (MEMORY.md, memory/),
	// not in Claude's conversation history. 1M context only inflates
	// cache_read/cache_creation across runs without adding real value, and its
	// write cost is 2x input (vs 1.25x for 5m cache). Force 200k and rely on
	// kojo's own session-reset logic (sessionFileUsable) for history pruning.
	// Auto-compact stays enabled as a late safety net but threshold is
	// tightened so that if reset misses, claude compacts before pricing spikes.
	cmd.Env = append(cmd.Env,
		"CLAUDE_CODE_DISABLE_1M_CONTEXT=1",
		"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=85",
	)
	if b.proxyURL != "" {
		cmd.Env = append(cmd.Env, "ANTHROPIC_BASE_URL="+b.proxyURL)
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			cmd.Env = append(cmd.Env, "ANTHROPIC_API_KEY=dummy")
		}
	}
	cmd.Dir = dir
	// Send SIGTERM on context cancellation, then SIGKILL after 10s grace period.
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second

	// Capture stderr for error diagnostics (limit to 4KB to prevent memory issues)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderrBuf, limit: 4096}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdinW := &claudeStdinWriter{w: stdinPipe}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}

	// Write the initial user message as the first stream-json line. This
	// mirrors what plain stdin text delivery used to do implicitly, just
	// framed as JSON so a later Steer call can append a second line onto
	// the same open pipe.
	if err := stdinW.writeUserLine(userMessage); err != nil {
		stdinW.close()
		_ = cmd.Process.Kill()
		cmd.Wait()
		return nil, fmt.Errorf("write initial stdin message: %w", err)
	}

	if opts.OnSteerReady != nil {
		opts.OnSteerReady(stdinW.writeUserLine)
	}

	// qstate is allocated whenever the caller wired an answer channel
	// (OnQuestionReady). Automated turns get it too (unlike before) so an
	// AskUserQuestion surfaces a card and is held with a timeout rather than
	// auto-denied. Turns with no answer channel (e.g. one-shots) leave it nil
	// so questions are denied inline.
	var qstate *claudeQuestionState
	if opts.OnQuestionReady != nil {
		qstate = newClaudeQuestionState(stdinW)
		qstate.setOnResolved(opts.OnQuestionResolved)
		opts.OnQuestionReady(qstate.answer)
	}
	// Watched user turns hold a question until the turn ends; automated turns
	// hold it only for a bounded window so an unwatched turn eventually proceeds.
	questionTimeout := time.Duration(0)
	if opts.AutomatedTrigger {
		questionTimeout = automatedQuestionTimeout
	}

	ch := make(chan ChatEvent, 64)

	go func() {
		defer close(ch)
		// Ensure the pipe is always closed so the process can exit even on
		// an early-return path (cancelled stream, process error, etc.)
		// that never reaches the "result" event below.
		defer stdinW.close()
		// Any question left pending at turn end/abort must be released so a
		// blocked CLI can exit and a late answer maps to not-found.
		defer qstate.denyAllPending("turn ended before the question was answered")

		// send is a helper that respects context cancellation to avoid goroutine leaks.
		send := func(e ChatEvent) bool { return ctxSend(ctx, ch, e) }

		result := parseClaudeStream(stdout, b.logger, send, stdinW.close, stdinW, qstate, questionTimeout)

		// If stream was cancelled (send returned false), clean up process
		// and emit a partial done event so the transcript is persisted.
		if result.cancelled {
			cmd.Wait()
			content := mergeStreamTexts(result)
			emitCancelDone(ctx, ch, content, result.thinking, result.toolUses, result.usage)
			return
		}

		// Check process exit status
		var processError string
		if err := cmd.Wait(); err != nil {
			b.logger.Warn("claude process exited with error", "err", err, "stderr", stderrBuf.String())
			processError = strings.TrimSpace(stderrBuf.String())
			if processError == "" {
				processError = err.Error()
			}
			if result.fullText == "" && result.lastAssistantText == "" && len(result.toolUses) == 0 {
				send(ChatEvent{Type: "error", ErrorMessage: processError})
				return
			}
		}

		// Determine final text. mergeStreamTexts prepends text from
		// earlier assistant turns (lastAssistantText) that was captured
		// via "assistant" events but never appeared in content_block_delta
		// text_deltas (fullText). This happens when Claude CLI internally
		// retries a malformed tool call: the first turn's text lands in
		// lastAssistantText, and the retry's error message lands in
		// fullText. Without merging, the original text is silently lost.
		finalText := mergeStreamTexts(result)

		// Last resort: recover from Claude session JSONL when the stream
		// produced no usable text. Only used as fallback, never overrides
		// text that was successfully captured from the stream.
		//
		// Prefer streamSessionID (authoritative — Claude told us which
		// file it wrote to). When unavailable (process died before the
		// result event), fall back to the deterministic UUID computed
		// for this branch. Never fall through to "" because findSessionFile
		// then picks the most-recently-modified JSONL, which under
		// concurrent Slack threads can be a sibling thread's file.
		recoverSessionID := result.streamSessionID
		if recoverSessionID == "" {
			recoverSessionID = expectedSessionID
		}
		if finalText == "" && recoverSessionID != "" {
			if sessionText := recoverFromSession(agent.ID, recoverSessionID, b.logger); sessionText != "" {
				b.logger.Info("recovered text from session log",
					"agent", agent.ID,
					"sessionLen", len(sessionText))
				finalText = sessionText
			}
		}

		// Send recovered text if nothing was streamed to client
		if finalText != "" && result.fullText == "" {
			send(ChatEvent{Type: "text", Delta: finalText})
		}

		msg := assembleAssistantMessage(finalText, result.thinking, result.toolUses, result.usage)

		// Cache-hit telemetry. Logged on every turn so we can see whether
		// the system-prompt / volatile-context split is actually keeping
		// the prompt cache warm. A high cacheCreation:cacheRead ratio
		// across consecutive turns means the cache prefix is being
		// invalidated and we're paying full input cost each turn.
		if u := result.usage; u != nil {
			b.logger.Info("claude usage",
				"agent", agent.ID,
				"input", u.InputTokens,
				"output", u.OutputTokens,
				"cacheRead", u.CacheReadInputTokens,
				"cacheCreation", u.CacheCreationInputTokens,
			)
		}

		send(ChatEvent{Type: "done", Message: msg, Usage: result.usage, ErrorMessage: processError})
	}()

	return ch, nil
}

type claudeInvocation struct {
	args                   []string
	bootstrapRecentContext bool
	// sessionWasReset is true when the session JSONL was deliberately
	// deleted just now because its context exceeded the reset threshold.
	// Unlike a merely-missing file, this MUST kill a live persistent
	// process — its in-memory context is what the reset drops.
	sessionWasReset bool
}

// buildClaudeArgs constructs the CLI arguments for a Claude chat invocation.
//
// sessionKey, when non-empty, overrides the default agent-ID-based session
// identifier. The key hashes to a deterministic UUID so successive calls in
// the same logical conversation (e.g. a Slack thread) land on the same JSONL.
func (b *ClaudeBackend) buildClaudeArgs(agent *Agent, systemPrompt string, dir string, oneShot bool, mcpServers map[string]mcpServerEntry, automatedTrigger bool, sessionKey string) []string {
	return b.buildClaudeInvocation(agent, systemPrompt, dir, oneShot, mcpServers, automatedTrigger, sessionKey).args
}

func (b *ClaudeBackend) buildClaudeInvocation(agent *Agent, systemPrompt string, dir string, oneShot bool, mcpServers map[string]mcpServerEntry, automatedTrigger bool, sessionKey string) claudeInvocation {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		// Read the prompt (and any mid-turn steer messages) as stream-json
		// lines on stdin instead of a single plain-text blob. The pipe is
		// kept open for the duration of the turn so Manager.Steer /
		// SteerOneShot can inject a second user message that the running
		// process merges into the same turn (see claudeStdinWriter).
		"--input-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--dangerously-skip-permissions",
		// Recent claude-code versions added an "auto" permission classifier
		// that can still prompt for Edit/Write despite --dangerously-skip-permissions.
		// Setting permission-mode explicitly forces bypass regardless of the
		// default mode the CLI resolves to.
		"--permission-mode", "bypassPermissions",
		// Disable tools that don't apply in kojo's non-interactive agent
		// context:
		//   - EnterPlanMode / ExitPlanMode: prompt the human via the host
		//     CLI's UI; the agent chat backend has no way to surface those.
		//   - CronCreate / CronDelete / CronList / ScheduleWakeup: the
		//     scheduled job would fire against the user's claude, not
		//     this agent's session.
		// AskUserQuestion is NOT disallowed — it's routed through the
		// control_request/control_response protocol so the Web UI can render
		// the prompt (interactive turns) or the backend auto-denies it
		// (automated turns). See --permission-prompt-tool below.
		"--disallowedTools", "EnterPlanMode,ExitPlanMode,CronCreate,CronDelete,CronList,ScheduleWakeup",
		// Route interactive-tool permission decisions (AskUserQuestion) to
		// stdout control_request lines we answer on stdin, even under
		// bypassPermissions. Without this the CLI never surfaces the prompt.
		"--permission-prompt-tool", "stdio",
	}

	// Fable-family models emit some mid-turn prose as "narration" blocks
	// that ride the thinking channel. The API default (display: "omitted")
	// strips their content server-side — the client receives empty
	// thinking blocks with a signature only, so the text is unrecoverable.
	// "summarized" makes the content arrive via thinking_delta, which
	// parseClaudeStream already captures. Billing is unchanged (display
	// controls visibility only). Skipped when a custom base URL is set:
	// Anthropic-compatible endpoints (llama-server etc.) may reject the
	// thinking.display request field.
	if b.proxyURL == "" {
		args = append(args, "--thinking-display", "summarized")
	}

	if systemPrompt != "" {
		args = append(args, "--system-prompt", systemPrompt)
	}
	if agent.Model != "" {
		args = append(args, "--model", agent.Model)
	}
	if agent.Effort != "" {
		args = append(args, "--effort", agent.Effort)
	}
	if len(agent.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(agent.AllowedTools, ","))
	}

	// Inject MCP servers via --mcp-config inline JSON (session-scoped, no files).
	if len(mcpServers) > 0 {
		if cfg, err := mcpConfigJSON(mcpServers); err == nil {
			args = append(args, "--mcp-config", cfg)
		} else {
			b.logger.Warn("failed to marshal MCP config for Claude", "err", err)
		}
	}

	// Remove CLAUDE.local.md to prevent persona autoload hook from
	// overriding --system-prompt.
	if err := os.Remove(filepath.Join(dir, "CLAUDE.local.md")); err != nil && !os.IsNotExist(err) {
		b.logger.Warn("failed to remove CLAUDE.local.md from agent dir", "dir", dir, "err", err)
	}

	// Use --resume to append to the same persistent session, or --session-id
	// to create the first one. --continue creates a new session file each
	// time, causing cron check-ins and user messages to branch into parallel
	// sessions — then the next --continue picks whichever branch was most
	// recent, losing the other's context.
	//
	// OneShot mode (no SessionKey, e.g. ephemeral Discord chats) skips
	// session resumption entirely. When SessionKey is set the call still
	// resumes — but against the key-derived UUID, isolating that branch
	// (Slack thread, etc.) from the agent's main session.
	bootstrapRecentContext := false
	sessionWasReset := false
	if sessionID := expectedClaudeSessionID(agent.ID, sessionKey, oneShot); sessionID != "" {
		usable, reset := sessionFileUsableReset(dir, sessionID, automatedTrigger, agent.ID, agent.ResumeIdleDuration(), b.logger)
		sessionWasReset = reset
		if usable {
			args = append(args, "--resume", sessionID)
		} else {
			args = append(args, "--session-id", sessionID)
			bootstrapRecentContext = true
		}
	}

	return claudeInvocation{args: args, bootstrapRecentContext: bootstrapRecentContext, sessionWasReset: sessionWasReset}
}

// streamParseResult holds the accumulated state from parsing a Claude stream.
type streamParseResult struct {
	fullText          string
	thinking          string
	lastAssistantText string
	streamSessionID   string
	toolUses          []ToolUse
	usage             *Usage
	cancelled         bool   // true if send returned false (context cancelled)
	origin            string // "result" event origin.kind, e.g. "task-notification"
	usageCumulative   bool   // usage came from result.modelUsage (cumulative per process)
}

// applyUsage merges non-zero token metrics into res.usage, allocating it
// on first use. The overlay (rather than wholesale replace) is deliberate:
// fable-family streams split a turn's final usage across two events — the
// top-level "assistant" event carries accurate input/cache counts but a
// placeholder output_tokens, while the trailing "message_delta" event
// carries the complete output_tokens. Overlaying keeps whichever event
// last supplied each field with a non-zero value, so the finalized
// message_delta corrects the assistant snapshot without clobbering the
// input/cache fields it may omit.
func applyUsage(res *streamParseResult, in, out, cacheRead, cacheCreate int) {
	if in == 0 && out == 0 && cacheRead == 0 && cacheCreate == 0 {
		return
	}
	if res.usage == nil {
		res.usage = &Usage{}
	}
	if in > 0 {
		res.usage.InputTokens = in
	}
	if out > 0 {
		res.usage.OutputTokens = out
	}
	if cacheRead > 0 {
		res.usage.CacheReadInputTokens = cacheRead
	}
	if cacheCreate > 0 {
		res.usage.CacheCreationInputTokens = cacheCreate
	}
}

// mergeStreamTexts combines lastAssistantText and fullText from a stream parse
// result. When Claude CLI retries a malformed tool call, the first assistant
// turn's text is captured only in lastAssistantText (via "assistant" events),
// while the retry's output (often just an error message) goes into fullText
// (via content_block_delta text_deltas). Without merging, an abort or normal
// finish would persist only the retry text, silently discarding the original
// content the user saw streaming.
func mergeStreamTexts(r *streamParseResult) string {
	if r.fullText == "" {
		return r.lastAssistantText
	}
	if r.lastAssistantText == "" || r.lastAssistantText == r.fullText {
		return r.fullText
	}
	// lastAssistantText contains text from an earlier assistant turn that
	// didn't make it into fullText (content_block_delta). Prepend it so
	// the persisted message reflects everything the user saw.
	if strings.Contains(r.fullText, r.lastAssistantText) {
		return r.fullText
	}
	return r.lastAssistantText + "\n\n" + r.fullText
}

// parseClaudeStream reads Claude's stream-json output from r and emits ChatEvents
// via the send callback. Returns the accumulated parse result.
// If send returns false (channel full / context cancelled), parsing stops immediately.
// qstate/questionTimeout drive interactive AskUserQuestion handling: qstate is
// nil for turns with no answer channel (question auto-denied inline);
// questionTimeout is 0 for watched user turns (held until turn end) and positive
// for automated turns (held with an auto-deny timeout).
func parseClaudeStream(r io.Reader, logger *slog.Logger, send func(ChatEvent) bool, onResult func(), stdinW *claudeStdinWriter, qstate *claudeQuestionState, questionTimeout time.Duration) *streamParseResult {
	// Skip-mode scanner: see claudeSession.readLoop — an oversized
	// stream-json line drops only that event instead of killing the parse.
	scanner := newSkippingLineScanner(r)
	loggedSkips := 0

	acc := newTurnAccumulator(logger, send)
	for scanner.Scan() {
		if n := scanner.Skipped(); n > loggedSkips {
			logger.Warn("claude stream: dropped oversized JSONL line(s); event content lost, stream continues",
				"dropped", n-loggedSkips, "totalDropped", n)
			loggedSkips = n
		}
		line := scanner.Text()
		if line == "" {
			continue
		}
		if cr, ok := maybeControlRequest(line); ok {
			// stdinW is nil only on hand-rolled test streams; a live process
			// always has one so a control_request never goes unanswered (an
			// unanswered can_use_tool would block the CLI forever).
			if stdinW != nil {
				handleControlRequest(cr, stdinW, qstate, questionTimeout, logger, send)
			}
			continue
		}
		event, rawParentID, ok := decodeClaudeStreamLine(line, logger)
		if !ok {
			continue
		}
		isResult := acc.feed(event, rawParentID)
		if isResult && onResult != nil {
			// The result event is the turn's terminal event in
			// --input-format stream-json mode; the process now blocks
			// waiting for either another stdin line (a steer message
			// arriving too late to merge into this turn) or EOF. Close
			// stdin now so it exits promptly instead of idling until
			// context cancellation/timeout. Runs even on a cancelled send
			// so the process is never left hanging on stdin.
			onResult()
		}
		if acc.res.cancelled {
			return acc.finalize()
		}
	}
	if n := scanner.Skipped(); n > loggedSkips {
		logger.Warn("claude stream: dropped oversized JSONL line(s) at stream end",
			"dropped", n-loggedSkips, "totalDropped", n)
	}
	if err := scanner.Err(); err != nil {
		logger.Warn("claude stream scanner error", "err", err)
	}
	return acc.finalize()
}

// decodeClaudeStreamLine unmarshals one stream-json line, capturing
// parent_tool_use_id before unwrapping the --include-partial-messages
// "stream_event" wrapper. ok is false for lines that should be skipped.
func decodeClaudeStreamLine(line string, logger *slog.Logger) (event claudeStreamEvent, rawParentID string, ok bool) {
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		logger.Debug("failed to parse claude stream event", "err", err)
		return event, "", false
	}
	// Capture parent_tool_use_id BEFORE any stream_event unwrap below
	// overwrites `event` with the inner payload — the field only ever
	// appears on the outer wrapper (for streamed deltas) or directly
	// on a complete top-level assistant/user event, never on the
	// inner content_block_* payload itself.
	rawParentID = event.ParentToolUseID

	// Unwrap stream_event wrapper emitted by --include-partial-messages.
	if event.Type == "stream_event" && len(event.Event) > 0 {
		var inner claudeStreamEvent
		if err := json.Unmarshal(event.Event, &inner); err != nil {
			logger.Debug("failed to parse inner stream event", "err", err)
			return event, "", false
		}
		if inner.Type == "" {
			return event, "", false
		}
		event = inner
	}
	return event, rawParentID, true
}

// turnAccumulator holds the per-turn parse state extracted from
// parseClaudeStream so both the one-shot spawn path and the persistent
// session loop share identical stream semantics (subagent nesting, usage
// overlay, tool_result matching). One accumulator spans exactly one turn:
// the persistent loop discards it and allocates a fresh one at each
// "result" boundary.
type turnAccumulator struct {
	logger           *slog.Logger
	send             func(ChatEvent) bool
	res              *streamParseResult
	fullText         strings.Builder
	thinking         strings.Builder
	toolUses         []ToolUse
	currentToolName  string
	currentToolID    string
	currentToolInput strings.Builder
	toolIDToName     map[string]string
	subagents        map[string]*subagentState
	subagentOwner    map[string]string
}

func newTurnAccumulator(logger *slog.Logger, send func(ChatEvent) bool) *turnAccumulator {
	return &turnAccumulator{
		logger:        logger,
		send:          send,
		res:           &streamParseResult{},
		toolIDToName:  make(map[string]string),
		subagents:     make(map[string]*subagentState),
		subagentOwner: make(map[string]string),
	}
}

func (a *turnAccumulator) resolveOwner(parentID string) string {
	if o, ok := a.subagentOwner[parentID]; ok {
		return o
	}
	return parentID
}

func (a *turnAccumulator) getSubagent(owner string) *subagentState {
	s, ok := a.subagents[owner]
	if !ok {
		s = &subagentState{toolIDToName: make(map[string]string)}
		a.subagents[owner] = s
	}
	return s
}

// feed processes one already-decoded stream event. It returns true when the
// event is the turn's terminal "result" event. On a cancelled send it sets
// a.res.cancelled; callers must check that flag.
func (a *turnAccumulator) feed(event claudeStreamEvent, rawParentID string) (isResult bool) {
	res := a.res
	send := a.send
	fullText := &a.fullText
	thinking := &a.thinking
	toolUses := a.toolUses
	defer func() { a.toolUses = toolUses }()

	parentID := ""
	if rawParentID != "" {
		parentID = a.resolveOwner(rawParentID)
	}
	// currentTool* live on the accumulator; alias for the moved switch body.
	getSubagent := a.getSubagent
	subagentOwner := a.subagentOwner
	toolIDToName := a.toolIDToName

	switch event.Type {
	case "rate_limit_event":
		// Usage-window telemetry emitted mid-turn. It belongs to the
		// whole session, not any subagent, so it's forwarded regardless
		// of parentID and never accumulated into the turn's text. The
		// Manager taps this event to persist the snapshot; the UI badge
		// updates live off the same event.
		if event.RateLimitInfo != nil {
			info := *event.RateLimitInfo
			if !send(ChatEvent{Type: "rate_limit", RateLimit: &info}) {
				res.cancelled = true
				return false
			}
		}

	case "system":
		status := "thinking"
		if event.Subtype == "compact_boundary" {
			status = "compacting"
		}
		if !send(ChatEvent{Type: "status", Status: status}) {
			res.cancelled = true
			return false
		}

	case "assistant":
		if parentID != "" {
			// Subagent turn: complete (non-streamed-delta) assistant
			// message belonging to a Task-spawned subagent. Route its
			// text and tool_use blocks into the subagent accumulator
			// instead of the main fullText/toolUses — this is the fix
			// for the pre-existing leak where an unwrapped subagent
			// message content polluted the parent turn.
			sub := getSubagent(parentID)
			for _, block := range event.Message.Content {
				switch block.Type {
				case "text":
					if block.Text != "" {
						sub.appendText(block.Text)
						if !send(ChatEvent{Type: "text", Delta: block.Text, ParentToolUseID: parentID}) {
							res.cancelled = true
							return false
						}
					}
				case "tool_use":
					input := string(block.Input)
					sub.children = append(sub.children, ToolUse{ID: block.ID, Name: block.Name, Input: input})
					sub.toolIDToName[block.ID] = block.Name
					subagentOwner[block.ID] = parentID
					if !send(ChatEvent{Type: "tool_use", ToolUseID: block.ID, ToolName: block.Name, ToolInput: input, ParentToolUseID: parentID}) {
						res.cancelled = true
						return false
					}
				}
			}
			break
		}
		var atext strings.Builder
		for _, block := range event.Message.Content {
			switch block.Type {
			case "text":
				if block.Text != "" {
					atext.WriteString(block.Text)
				}
			case "thinking":
				if block.Thinking != "" && thinking.Len() == 0 {
					thinking.WriteString(block.Thinking)
				}
			}
		}
		if atext.Len() > 0 {
			res.lastAssistantText = atext.String()
		}

		// Record usage whenever the assistant turn reports any metric.
		// The StopReason guard was removed: fable-family models emit
		// the top-level "assistant" event with usage populated but
		// stop_reason=null, so gating on stop_reason dropped their
		// usage entirely (it never persisted). The complete
		// output_tokens still arrives later in "message_delta"; the
		// non-zero overlay in applyUsage lets that event correct the
		// placeholder output_tokens this snapshot carries.
		u := event.Message.Usage
		applyUsage(res, u.InputTokens, u.OutputTokens,
			u.CacheReadInputTokens, u.CacheCreationInputTokens)

	case "message_delta":
		// Finalized usage for the turn. Anthropic's streaming protocol
		// puts the complete token counts (notably the full
		// output_tokens) on message_delta, at the top level of the
		// event rather than under "message". This is the authoritative
		// source for the per-turn usage line in the chat UI.
		d := event.Usage
		applyUsage(res, d.InputTokens, d.OutputTokens,
			d.CacheReadInputTokens, d.CacheCreationInputTokens)

	case "content_block_start":
		if event.ContentBlock.Type == "tool_use" {
			if parentID != "" {
				sub := getSubagent(parentID)
				sub.currentToolName = event.ContentBlock.Name
				sub.currentToolID = event.ContentBlock.ID
				sub.currentToolInput.Reset()
				sub.toolIDToName[sub.currentToolID] = sub.currentToolName
			} else {
				a.currentToolName = event.ContentBlock.Name
				a.currentToolID = event.ContentBlock.ID
				a.currentToolInput.Reset()
				toolIDToName[a.currentToolID] = a.currentToolName
			}
		}

	case "content_block_delta":
		if parentID != "" {
			sub := getSubagent(parentID)
			switch event.Delta.Type {
			case "text_delta":
				if event.Delta.Text != "" {
					sub.appendText(event.Delta.Text)
					if !send(ChatEvent{Type: "text", Delta: event.Delta.Text, ParentToolUseID: parentID}) {
						res.cancelled = true
						return false
					}
				}
			case "thinking_delta":
				// Subagent thinking isn't surfaced today — no UI slot
				// for nested reasoning text, and it isn't persisted on
				// the parent ToolUse's Children. Dropped silently.
			case "input_json_delta":
				sub.currentToolInput.WriteString(event.Delta.PartialJSON)
			}
			break
		}
		switch event.Delta.Type {
		case "text_delta":
			if event.Delta.Text != "" {
				fullText.WriteString(event.Delta.Text)
				if !send(ChatEvent{Type: "text", Delta: event.Delta.Text}) {
					res.cancelled = true
					return false
				}
			}
		case "thinking_delta":
			if event.Delta.Thinking != "" {
				thinking.WriteString(event.Delta.Thinking)
				if !send(ChatEvent{Type: "thinking", Delta: event.Delta.Thinking}) {
					res.cancelled = true
					return false
				}
			}
		case "input_json_delta":
			a.currentToolInput.WriteString(event.Delta.PartialJSON)
		}

	case "content_block_stop":
		if parentID != "" {
			sub := getSubagent(parentID)
			if sub.currentToolName != "" {
				input := sub.currentToolInput.String()
				sub.children = append(sub.children, ToolUse{ID: sub.currentToolID, Name: sub.currentToolName, Input: input})
				subagentOwner[sub.currentToolID] = parentID
				if !send(ChatEvent{Type: "tool_use", ToolUseID: sub.currentToolID, ToolName: sub.currentToolName, ToolInput: input, ParentToolUseID: parentID}) {
					res.cancelled = true
					return false
				}
				sub.currentToolName = ""
				sub.currentToolID = ""
				sub.currentToolInput.Reset()
			}
			break
		}
		if a.currentToolName != "" {
			input := a.currentToolInput.String()
			tu := ToolUse{
				ID:    a.currentToolID,
				Name:  a.currentToolName,
				Input: input,
			}
			toolUses = append(toolUses, tu)
			if !send(ChatEvent{Type: "tool_use", ToolUseID: a.currentToolID, ToolName: a.currentToolName, ToolInput: input}) {
				res.cancelled = true
				return false
			}
			a.currentToolName = ""
			a.currentToolID = ""
			a.currentToolInput.Reset()
		}

	case "user":
		if parentID != "" {
			sub := getSubagent(parentID)
			for _, block := range event.Message.Content {
				if block.Type == "tool_result" && block.ToolUseID != "" {
					toolName := sub.toolIDToName[block.ToolUseID]
					if toolName != "" {
						output := block.contentText()
						if !send(ChatEvent{Type: "tool_result", ToolUseID: block.ToolUseID, ToolName: toolName, ToolOutput: output, ParentToolUseID: parentID}) {
							res.cancelled = true
							return false
						}
						matchToolOutput(sub.children, block.ToolUseID, toolName, output)
					}
				}
			}
			break
		}
		for _, block := range event.Message.Content {
			if block.Type == "tool_result" && block.ToolUseID != "" {
				toolName := toolIDToName[block.ToolUseID]
				if toolName != "" {
					output := block.contentText()
					if !send(ChatEvent{Type: "tool_result", ToolUseID: block.ToolUseID, ToolName: toolName, ToolOutput: output}) {
						res.cancelled = true
						return false
					}
					matchToolOutput(toolUses, block.ToolUseID, toolName, output)
				}
			}
		}

	case "result":
		isResult = true
		if event.SessionID != "" {
			res.streamSessionID = event.SessionID
		}
		if event.Origin != nil {
			res.origin = event.Origin.Kind
		}
		// The result event's modelUsage map holds the invocation-wide
		// token totals, including subagent (Task tool) usage that the
		// main loop's assistant/message_delta events never report.
		// It is cumulative per CLI process, so when a process emits
		// several result events (background subagents), the last one
		// wins — replace wholesale rather than overlay.
		if len(event.ModelUsage) > 0 {
			total := &Usage{CostUSD: event.TotalCostUSD}
			for _, mu := range event.ModelUsage {
				total.InputTokens += mu.InputTokens
				total.OutputTokens += mu.OutputTokens
				total.CacheReadInputTokens += mu.CacheReadInputTokens
				total.CacheCreationInputTokens += mu.CacheCreationInputTokens
			}
			res.usage = total
			// modelUsage totals are cumulative per CLI process. A persistent
			// session must delta this against the prior turn; flag the source
			// so it can (one-shot processes delta from zero, unaffected).
			res.usageCumulative = true
		} else if event.TotalCostUSD > 0 && res.usage != nil {
			// Older CLIs report total_cost_usd without modelUsage;
			// keep the stream-accumulated tokens and attach the cost.
			res.usage.CostUSD = event.TotalCostUSD
		}
		if event.Result != "" {
			if fullText.Len() == 0 {
				fullText.WriteString(event.Result)
				if !send(ChatEvent{Type: "text", Delta: event.Result}) {
					// Record the cancelled send but still report the result
					// boundary: returning early here would leave the caller
					// (one-shot parseClaudeStream or the persistent session)
					// thinking the turn never terminated — stdin never gets
					// closed and the process wedges in cmd.Wait / an
					// un-completed turn.
					res.cancelled = true
				}
			}
		}
	}
	return isResult
}

// finalize attaches accumulated subagent state onto the matching top-level
// Task ToolUses and returns the completed per-turn result.
func (a *turnAccumulator) finalize() *streamParseResult {
	res := a.res
	toolUses := a.toolUses

	// Attach accumulated subagent state onto the matching top-level
	// Task ToolUse. Every key in `subagents` is either a top-level
	// tool_use ID (found directly in toolUses) or — for a subagent
	// whose parent_tool_use_id we never resolved to a known owner
	// (e.g. it arrived before its own Task tool_use closed) — left
	// dangling and dropped, matching the "no recursion" flattening
	// rule: we only ever attach one level deep onto a real top-level
	// Task call.
	if len(a.subagents) > 0 {
		byID := make(map[string]int, len(toolUses))
		for i, tu := range toolUses {
			byID[tu.ID] = i
		}
		for owner, sub := range a.subagents {
			idx, ok := byID[owner]
			if !ok {
				continue
			}
			toolUses[idx].Children = append([]ToolUse(nil), sub.children...)
		}
	}

	res.fullText = a.fullText.String()
	res.thinking = a.thinking.String()
	res.toolUses = toolUses
	return res
}

// subagentState accumulates one subagent's (Task tool call's) streamed
// output — its own tool_use/tool_result flow interleaved with narrative
// text, in arrival order — keyed by the top-level Task ToolUse ID it will
// be attached to as Children.
//
// children holds both real tool_use entries (Name/Input set) and
// synthetic text bubbles (Name/Input empty, Text set) in the order they
// arrived, so the persisted transcript preserves "said X, ran tool Y,
// said Z" ordering rather than collapsing all narrative text into a
// single trailing bubble.
type subagentState struct {
	children         []ToolUse
	toolIDToName     map[string]string
	currentToolName  string
	currentToolID    string
	currentToolInput strings.Builder
}

// appendText appends a text delta to the trailing text-bubble entry of
// sub.children, creating one if the last entry isn't already a text
// bubble (or there are no entries yet).
func (sub *subagentState) appendText(text string) {
	if n := len(sub.children); n > 0 && sub.children[n-1].Name == "" {
		sub.children[n-1].Text += text
		return
	}
	sub.children = append(sub.children, ToolUse{Text: text})
}

// Claude stream-json event types
type claudeStreamEvent struct {
	Type string `json:"type"`

	// ParentToolUseID tags events that belong to a subagent spawned by a
	// Task tool call, rather than the main assistant turn. Appears as a
	// sibling of "type" on the "stream_event" wrapper (streamed subagent
	// deltas) and directly on complete top-level "assistant"/"user"
	// events (non-streamed subagent turns) — never on the inner payload
	// once a stream_event wrapper has been unwrapped, so callers must
	// capture it before overwriting `event` with the inner value.
	ParentToolUseID string `json:"parent_tool_use_id,omitempty"`

	// "stream_event" wrapper (--include-partial-messages)
	Event json.RawMessage `json:"event,omitempty"`

	// "system" event
	Subtype string `json:"subtype,omitempty"`

	// "assistant" event
	Message struct {
		StopReason string               `json:"stop_reason,omitempty"`
		Content    []claudeContentBlock `json:"content,omitempty"`
		Usage      claudeUsage          `json:"usage,omitempty"`
	} `json:"message,omitempty"`

	// "message_delta" event carries the finalized turn usage at the top
	// level (a sibling of "delta", not nested under "message").
	Usage claudeUsage `json:"usage,omitempty"`

	// "content_block_start" event
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id,omitempty"`
		Name string `json:"name,omitempty"`
	} `json:"content_block,omitempty"`

	// "content_block_delta" event
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text,omitempty"`
		Thinking    string `json:"thinking,omitempty"`
		PartialJSON string `json:"partial_json,omitempty"`
	} `json:"delta,omitempty"`

	// "result" event
	Result       string                      `json:"result,omitempty"`
	SessionID    string                      `json:"session_id,omitempty"`
	TotalCostUSD float64                     `json:"total_cost_usd,omitempty"`
	ModelUsage   map[string]claudeModelUsage `json:"modelUsage,omitempty"`

	// "rate_limit_event" carries the CLI's usage-window telemetry,
	// emitted mid-turn when a reporting threshold is crossed.
	RateLimitInfo *RateLimitInfo `json:"rate_limit_info,omitempty"`

	// Origin marks the provenance of a "result" event. A background
	// subagent (Task run_in_background) finishing on a persistent process
	// emits an unsolicited turn whose result carries origin.kind ==
	// "task-notification"; a normal user-driven turn has no origin. Used by
	// the persistent-session loop to decide whether a completed turn was
	// solicited (deliver to the waiting caller) or unsolicited (persist +
	// broadcast as a background chat).
	Origin *struct {
		Kind string `json:"kind,omitempty"`
	} `json:"origin,omitempty"`
}

// claudeModelUsage is one entry of the "result" event's modelUsage map:
// per-model token totals for the whole CLI invocation, including subagent
// (Task tool) usage that never surfaces on the main loop's assistant /
// message_delta events. Note the camelCase keys, unlike claudeUsage.
type claudeModelUsage struct {
	InputTokens              int `json:"inputTokens"`
	OutputTokens             int `json:"outputTokens"`
	CacheReadInputTokens     int `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int `json:"cacheCreationInputTokens"`
}

// claudeUsage holds the token metrics Claude reports on both the
// per-turn "assistant" event (nested under message.usage) and the
// finalized "message_delta" event (top-level usage).
type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// claudeContentBlock represents a content block in a Claude message.
type claudeContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`    // thinking block
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result
	Content   json.RawMessage `json:"content,omitempty"`     // tool_result output (string or array)
	// tool_use block fields. These only appear on complete (non-streamed)
	// assistant messages — a subagent's tool_use, since subagent turns in
	// practice arrive as fully-materialized "assistant" events rather
	// than via content_block_start/input_json_delta/content_block_stop.
	ID    string          `json:"id,omitempty"`    // tool_use
	Name  string          `json:"name,omitempty"`  // tool_use
	Input json.RawMessage `json:"input,omitempty"` // tool_use, raw JSON object
}

// contentText extracts a plain-text representation from a claudeContentBlock's Content field.
// Content may be a JSON string or an array of content blocks with "text" entries.
func (b *claudeContentBlock) contentText() string {
	if len(b.Content) == 0 {
		return ""
	}
	// Try as plain string first
	var s string
	if json.Unmarshal(b.Content, &s) == nil {
		return s
	}
	// Try as array of content blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(b.Content, &blocks) == nil {
		var sb strings.Builder
		for _, bl := range blocks {
			if bl.Type == "text" && bl.Text != "" {
				sb.WriteString(bl.Text)
			}
		}
		return sb.String()
	}
	// Fallback: raw string
	return string(b.Content)
}

// limitedWriter wraps a bytes.Buffer and stops writing after limit bytes.
type limitedWriter struct {
	w     *bytes.Buffer
	limit int
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	remaining := lw.limit - lw.w.Len()
	if remaining <= 0 {
		return len(p), nil // discard silently
	}
	toWrite := p
	if len(toWrite) > remaining {
		toWrite = toWrite[:remaining]
	}
	lw.w.Write(toWrite)
	// Always report full len(p) to avoid io.ErrShortWrite from callers.
	return len(p), nil
}

// claudeEncodePath encodes a directory path using Claude's project
// path scheme. Replaces "/", "\", ":", ".", "_" with "-" so the
// result is portable across macOS / Linux (POSIX) and Windows
// (`C:\Users\alice\foo` → `-C--Users-alice-foo`). Using a fixed
// replacer (not filepath.Separator alone) is essential: a
// Windows path that survived an across-OS round-trip with only
// one separator translated would land at a different project
// dir on the new host and claude --continue would lose the
// transcript.
func claudeEncodePath(dir string) string {
	return strings.NewReplacer(
		"/", "-",
		"\\", "-",
		":", "-",
		".", "-",
		"_", "-",
	).Replace(dir)
}

// claudeProjectDir returns the Claude project directory for the given absolute path.
func claudeProjectDir(absDir string) string {
	return filepath.Join(claudeConfigDir(), "projects", claudeEncodePath(absDir))
}

// claudeConfigDir returns the Claude configuration root, respecting
// CLAUDE_CONFIG_DIR if set, otherwise falling back to ~/.claude.
func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".claude")
}

// hasExistingSession checks whether a Claude session JSONL file already exists
// for the given agent working directory by looking at Claude's project data.
func hasExistingSession(agentDir string) bool {
	return hasSessionFile(agentDir, "")
}

// hasSessionFile checks whether a specific session JSONL file exists.
// If sessionID is empty, it returns true if any session file exists.
func hasSessionFile(agentDir string, sessionID string) bool {
	absDir, err := filepath.Abs(agentDir)
	if err != nil {
		return false
	}
	projectDir := claudeProjectDir(absDir)
	if sessionID != "" {
		_, err := os.Stat(filepath.Join(projectDir, sessionID+".jsonl"))
		return err == nil
	}
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			return true
		}
	}
	return false
}

// sessionResetThresholdTokens is the cumulative context size (input +
// cache_read + cache_creation) at which kojo abandons --resume and starts a
// fresh claude session. Chosen below claude's 200k model limit and below the
// CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=85 threshold (~170k) so that kojo's reset
// fires first and claude's compaction stays a safety net.
//
// Why reset instead of compact: kojo persists long-term state in agent files
// (MEMORY.md, memory/, data/), so the conversation JSONL is just an ephemeral
// work log. /compact would spend output tokens to summarize history that will
// be re-derived from files on the next turn anyway — pure waste for
// interval-driven agents. Resetting is cheaper and keeps context tight.
const sessionResetThresholdTokens = 150_000

// sessionTailReadBytes caps how much of the session JSONL is read when
// measuring the last recorded context size. Chosen to fit at least one full
// assistant turn (tool_result payloads can reach several hundred KiB) so the
// latest usage record is within the window even for multi-hundred-MB session
// logs. When the last record happens to straddle the window boundary and
// gets discarded as a leading partial line, lastSessionContextTokens marks
// the read as untrusted so the reset retries on the next chat.
const sessionTailReadBytes = 1 * 1024 * 1024

// preResetSummarize is the summarization hook invoked by sessionFileUsable
// right before it deletes a session file. Defaults to PreCompactSummarize,
// which writes a diary entry from the live session JSONL. Exposed as a
// package variable so tests can substitute a deterministic fake instead of
// spawning a real claude CLI process.
var preResetSummarize = func(agentID, tool string, logger *slog.Logger) error {
	// Reset path doesn't have the PreCompact-hook stdin payload (claude
	// isn't telling us about a compaction here — kojo is initiating a
	// session wipe), so transcriptPath is left empty and the function
	// falls back to discovery.
	return PreCompactSummarize(agentID, tool, "", logger)
}

// sessionResetMinIdleDuration is the package-default idle window used when
// a caller doesn't supply a per-agent override (legacy callers and tests).
// Per-agent override comes through Agent.ResumeIdleMinutes →
// Agent.ResumeIdleDuration().
//
// Rationale for the 5-minute default: an active chat produces many short
// turns that never reach the autosummary / MEMORY.md path, so resetting
// mid-conversation would discard recent context the agent can't recover
// from files. The interval-driven agents we're actually trying to curb
// fire every 10+ minutes and always fall well outside this window, so the
// gate only protects interactive users. 5 minutes also matches Anthropic's
// default prompt cache TTL, keeping cache warm across back-to-back turns.
// If an active chat genuinely blows past the token threshold, claude's
// auto-compact (CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=85) still fires as a
// safety net.
const sessionResetMinIdleDuration = defaultResumeIdleDuration

// sessionFileUsable checks whether the deterministic session file exists and
// is minimally valid. Returns false (and removes the file) when the file is
// empty or the last recorded usage exceeds sessionResetThresholdTokens —
// forcing the caller to start a fresh session with --session-id instead of
// --resume.
//
// When the usage cannot be read reliably (e.g. the claude process is mid-write
// or the scanner tripped on an oversized line), we conservatively return true
// without touching the file: resetting on uncertainty would race with a live
// writer and could discard a healthy session. The check will run again on the
// next chat.
//
// automatedTrigger disables the interactive-chat idle guard. Pass true when
// the caller is a non-interactive trigger (cron fire, groupdm notification,
// Slack one-shot) — there is no human conversation to protect and the guard
// would otherwise prevent resets on agents whose interval is shorter than
// the idle window.
//
// idleThreshold is the per-agent active-chat protection window. Pass
// agent.ResumeIdleDuration(); pass <=0 to fall back to sessionResetMinIdleDuration.
//
// agentID and logger are used to summarize the session into the agent's
// diary (via PreCompactSummarize) just before a reset fires. This is kojo's
// system-level guarantee that recent turns aren't lost simply because the
// agent forgot to update MEMORY.md itself — production experience has shown
// that persona / system-prompt instructions to "write to MEMORY.md" are not
// reliable, so the reset path writes a summary regardless.
// sessionFileUsable reports whether the session JSONL can be resumed.
// Thin wrapper over sessionFileUsableReset for callers that don't care
// whether an over-threshold reset just happened.
func sessionFileUsable(agentDir string, sessionID string, automatedTrigger bool, agentID string, idleThreshold time.Duration, logger *slog.Logger) bool {
	usable, _ := sessionFileUsableReset(agentDir, sessionID, automatedTrigger, agentID, idleThreshold, logger)
	return usable
}

// sessionFileUsableReset additionally reports reset=true when the session
// file existed but was deliberately deleted here because its context grew
// over the reset threshold. The persistent-session pool uses that signal to
// kill a live process (whose in-memory context the reset is meant to drop);
// a merely-missing file (reset=false) must NOT kill a live session — under
// the persistent model the conversation context lives in the process, and
// the CLI may not have flushed a JSONL yet.
func sessionFileUsableReset(agentDir string, sessionID string, automatedTrigger bool, agentID string, idleThreshold time.Duration, logger *slog.Logger) (usable, reset bool) {
	if logger == nil {
		// Preserve the pre-DI behavior (logging went to the package
		// default) for callers — chiefly tests — that pass no logger.
		logger = slog.Default()
	}
	absDir, err := filepath.Abs(agentDir)
	if err != nil {
		return false, false
	}
	path := filepath.Join(claudeProjectDir(absDir), sessionID+".jsonl")
	info, err := os.Stat(path)
	if err != nil {
		return false, false
	}
	if info.Size() == 0 {
		if err := os.Remove(path); err != nil {
			// Same rationale as the threshold branch below — a file we
			// can't delete but also can't --resume (empty) is best left
			// alone; next chat will retry.
			logger.Warn("empty session remove failed, keeping as --resume fallback",
				"path", path, "err", err)
			return true, false
		}
		return false, false
	}
	ctx, ok := lastSessionContextTokens(path)
	if !ok {
		// Couldn't trust the tail read — keep the session for now and
		// re-evaluate on the next chat.
		return true, false
	}
	if ctx <= sessionResetThresholdTokens {
		return true, false
	}
	if !automatedTrigger {
		threshold := idleThreshold
		if threshold <= 0 {
			threshold = sessionResetMinIdleDuration
		}
		if idle := time.Since(info.ModTime()); idle < threshold {
			logger.Debug("claude session over threshold but recently active, keeping",
				"path", path, "contextTokens", ctx,
				"idle", idle, "idleThreshold", threshold)
			return true, false
		}
	}

	// Summarize the session into the agent's diary BEFORE we delete its
	// JSONL. PreCompactSummarize reads the live session file (which we're
	// about to remove), so ordering matters. If the summary fails we
	// abort the reset — losing context silently would be worse than
	// carrying a slightly-over-threshold session for one more turn.
	if agentID != "" {
		if err := preResetSummarize(agentID, "claude", logger); err != nil {
			logger.Warn("pre-reset summary failed, keeping session to avoid context loss",
				"path", path, "agent", agentID, "err", err)
			return true, false
		}
	}

	logger.Info("claude session context over threshold, resetting",
		"path", path, "contextTokens", ctx,
		"threshold", sessionResetThresholdTokens,
		"automatedTrigger", automatedTrigger)
	if err := os.Remove(path); err != nil {
		// Couldn't delete the session file — don't lie to the caller by
		// returning false. With our deterministic session ID, a subsequent
		// --session-id <id> invocation would either resurrect the existing
		// session or fail, neither of which is what we want. Keep using
		// --resume; the next run will retry the reset.
		logger.Warn("session reset: remove failed, keeping session",
			"path", path, "err", err)
		return true, false
	}
	return false, true
}

// lastSessionContextTokens returns (contextTokens, trusted). contextTokens is
// the approximate claude context size (input + cache_read + cache_creation)
// taken from the most recent usage record in the session JSONL. trusted is
// false when the read cannot be relied upon (missing file, seek error,
// oversized line, concurrent mid-write). Callers should treat untrusted reads
// as "skip reset this round" — never as "reset immediately" — to avoid racing
// with a live claude writer.
//
// Only the tail of the file (sessionTailReadBytes) is scanned; session logs
// for long-lived agents routinely reach tens to hundreds of MiB and the last
// usage record is always near the end, so a full-file scan on every Chat()
// call would add non-trivial latency for no benefit.
func lastSessionContextTokens(sessionPath string) (int, bool) {
	f, err := os.Open(sessionPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return 0, false
	}

	var startOffset int64
	if info.Size() > sessionTailReadBytes {
		startOffset = info.Size() - sessionTailReadBytes
	}
	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		return 0, false
	}

	scanner := bufio.NewScanner(f)
	// 1 MiB is the largest single JSONL line we've observed in practice;
	// anything larger gets reported via scanner.Err() and we mark the read
	// as untrusted.
	scanner.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)

	// If we seeked into the middle of the file, the first partial line is
	// unparseable — drop it before accumulating.
	if startOffset > 0 {
		scanner.Scan()
	}

	last := 0
	for scanner.Scan() {
		var entry struct {
			Message struct {
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		u := entry.Message.Usage
		ctx := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		if ctx > 0 {
			last = ctx
		}
	}
	if err := scanner.Err(); err != nil {
		// Oversized line or a transient read error (possibly mid-write from
		// a live claude process). Refuse to make a decision.
		return 0, false
	}
	// Tail read guarantees a conclusive result only when we either scanned
	// from the start of the file or found at least one usage record in the
	// tail window. Otherwise the single line that straddled the window start
	// may have carried the most recent usage — we can't tell, so leave the
	// decision to the next run rather than return a falsely low (0, true).
	if last == 0 && startOffset > 0 {
		return 0, false
	}
	return last, true
}

// agentIDToUUID converts an agent ID (e.g. "ag_8cf247118ad856e8") to a
// deterministic UUID v3 string that claude CLI accepts as --session-id.
func agentIDToUUID(agentID string) string {
	h := md5.Sum([]byte(agentID))
	h[6] = (h[6] & 0x0f) | 0x30 // version 3
	h[8] = (h[8] & 0x3f) | 0x80 // variant RFC4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// expectedClaudeSessionID returns the deterministic Claude session UUID that
// this invocation would resume/create, or "" when the caller has opted out of
// session persistence (oneShot mode).
//
// oneShot takes precedence: a true oneShot call ALWAYS returns "" so the
// backend skips --resume / --session-id entirely. The manager is responsible
// for setting OneShot=false whenever a non-empty SessionKey should actually
// resume — this preserves the canonical "OneShot means ephemeral" invariant
// and avoids a backend-level override that would surprise callers passing
// SessionKey for purposes other than resumption.
//
// When sessionKey is non-empty (e.g. per-Slack-thread isolation), the UUID is
// derived from that key so each thread maps to its own JSONL. Otherwise the
// agent-wide UUID is used.
//
// The recovery path uses this value as a fallback so that, if Claude exits
// before emitting the result event with streamSessionID, we still recover
// text from THIS branch's JSONL rather than whichever file was most recently
// modified (which could belong to a concurrent Slack thread).
func expectedClaudeSessionID(agentID, sessionKey string, oneShot bool) string {
	if oneShot {
		return ""
	}
	if sessionKey != "" {
		return agentIDToUUID(sessionKey)
	}
	return agentIDToUUID(agentID)
}

// resetClaudeSessionFiles best-effort deletes the deterministic session JSONL
// AND its ~/.claude/session-env cache for (agentID, sessionKey). claude marks a
// session "already in use" whenever its JSONL was recently modified but never
// cleanly closed — which is exactly the state a kill -9'd persistent process
// leaves behind, blocking a respawn on the same deterministic id until the
// heuristic's grace window (minutes) elapses. Dropping the JSONL clears that
// state so a fresh --session-id spawn succeeds immediately; the lost in-flight
// context is recovered on the next turn via recent-messages bootstrap.
func resetClaudeSessionFiles(agentID, sessionKey string, logger *slog.Logger) {
	sessionID := expectedClaudeSessionID(agentID, sessionKey, false)
	if sessionID == "" {
		return
	}
	// claude encodes its project dir from the process's RESOLVED cwd, so the
	// path must be symlink-resolved (on macOS the agent dir under /tmp resolves
	// to /private/tmp; filepath.Abs alone would target a nonexistent
	// "-tmp-..." project dir and the delete would silently no-op).
	dir := agentDir(agentID)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	_ = os.Remove(filepath.Join(claudeProjectDir(absDir), sessionID+".jsonl"))
	_ = os.RemoveAll(filepath.Join(claudeConfigDir(), "session-env", sessionID))
	// Also drop any lingering per-pid session lease that names this session.
	leaseDir := filepath.Join(claudeConfigDir(), "sessions")
	if entries, derr := os.ReadDir(leaseDir); derr == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			p := filepath.Join(leaseDir, e.Name())
			if data, rerr := os.ReadFile(p); rerr == nil && strings.Contains(string(data), sessionID) {
				_ = os.Remove(p)
			}
		}
	}
	if logger != nil {
		logger.Warn("reset claude session file to clear stale in-use state",
			"agent", agentID, "sessionID", sessionID)
	}
}

// removeClaudeSession best-effort deletes the Claude session JSONL for the
// given (agentID, sessionKey) pair. Used when a thread room is archived so the
// throwaway side-conversation's --resume artifact does not linger. Errors are
// ignored: a missing file is the expected common case (session never created).
func removeClaudeSession(agentID, sessionKey string) {
	sessionID := expectedClaudeSessionID(agentID, sessionKey, false)
	if sessionID == "" {
		return
	}
	absDir, err := filepath.Abs(agentDir(agentID))
	if err != nil {
		return
	}
	_ = os.Remove(filepath.Join(claudeProjectDir(absDir), sessionID+".jsonl"))
	// The CLI derives its project dir from the RESOLVED cwd (e.g. /tmp →
	// /private/tmp on macOS); delete that variant too or the file survives
	// under symlinked config dirs. See resetClaudeSessionFiles.
	if resolved, rerr := filepath.EvalSymlinks(absDir); rerr == nil && resolved != absDir {
		_ = os.Remove(filepath.Join(claudeProjectDir(resolved), sessionID+".jsonl"))
	}
}

// recoverFromSession reads the Claude session JSONL for the agent and
// returns the text that Claude actually generated for the last user message.
// If sessionID is non-empty, the matching session file is used; otherwise
// the most recently modified session file is selected as a fallback.
func recoverFromSession(agentID string, sessionID string, logger *slog.Logger) string {
	dir := agentDir(agentID)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}

	projectDir := claudeProjectDir(absDir)

	sessionFile := findSessionFile(projectDir, sessionID)
	if sessionFile == "" {
		return ""
	}

	f, err := os.Open(sessionFile)
	if err != nil {
		return ""
	}
	defer f.Close()

	// Walk session entries, keeping only the assistant text that appears
	// after the last real user message (tool_result entries are skipped).
	// Instead of storing all entries, we just reset on real user messages
	// and accumulate assistant text.
	var texts []string
	foundUser := false

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	for scanner.Scan() {
		var raw struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &raw) != nil {
			continue
		}

		switch raw.Type {
		case "assistant":
			var msg struct {
				Content []claudeContentBlock `json:"content"`
			}
			if json.Unmarshal(raw.Message, &msg) != nil {
				continue
			}
			var text strings.Builder
			for _, block := range msg.Content {
				if block.Type == "text" && block.Text != "" {
					text.WriteString(block.Text)
				}
			}
			if text.Len() > 0 {
				texts = append(texts, text.String())
			}

		case "user":
			if isRealUserEntry(raw.Message) {
				// New user turn — reset collected assistant text.
				texts = nil
				foundUser = true
			}
		}
	}

	// If the scanner hit an error (e.g. oversized line), discard
	// partial results to avoid returning truncated/stale text.
	if scanner.Err() != nil {
		logger.Warn("session JSONL scanner error", "err", scanner.Err())
		return ""
	}

	if !foundUser {
		return ""
	}

	return strings.Join(texts, "")
}

// findSessionFile locates the Claude session JSONL in projectDir.
// When sessionID is provided, it looks for "<sessionID>.jsonl" only.
// When sessionID is empty, falls back to the most recently modified .jsonl file.
func findSessionFile(projectDir string, sessionID string) string {
	if sessionID != "" {
		path := filepath.Join(projectDir, sessionID+".jsonl")
		if _, err := os.Stat(path); err == nil {
			return path
		}
		return ""
	}

	// Fallback for callers that don't have a session ID (e.g. loadSessionMessages).
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return ""
	}
	var best string
	var bestMod time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(bestMod) {
			bestMod = info.ModTime()
			best = filepath.Join(projectDir, e.Name())
		}
	}
	return best
}

// clearClaudeSession removes Claude session JSONL files from the global
// config store for the given agent, forcing the next chat to start fresh.
func clearClaudeSession(agentID string, logger *slog.Logger) {
	dir := agentDir(agentID)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		logger.Warn("clearClaudeSession: Abs failed", "agent", agentID, "err", err)
		return
	}
	projectDir := claudeProjectDir(absDir)
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		logger.Info("clearClaudeSession: no project dir", "agent", agentID, "dir", projectDir)
		return
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			path := filepath.Join(projectDir, e.Name())
			if err := os.Remove(path); err != nil {
				logger.Warn("clearClaudeSession: remove failed", "path", path, "err", err)
			} else {
				logger.Info("clearClaudeSession: removed", "path", path)
			}
		}
	}
}

// allowedProtectedPaths is the set of protected directory names for which
// per-agent permission bypass may be granted. Claude guards these dirs even
// under bypassPermissions, so explicit permissions.allow rules are the only
// way to suppress prompts for headless agents.
var allowedProtectedPaths = map[string]bool{
	"claude": true,
	"git":    true,
	"husky":  true,
}

// normalizeAllowProtectedPaths filters a slice to the known-valid set and
// deduplicates entries while preserving caller-provided order.
func normalizeAllowProtectedPaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if !allowedProtectedPaths[p] || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// buildProtectedPathAllowRules emits permissions.allow rule strings for the
// given protected directory names (e.g. "claude" → Edit/Write/MultiEdit for
// any **/.claude/** path). Returns a JSON array body (without the surrounding
// brackets) suitable for inlining into settings.local.json.
func buildProtectedPathAllowRules(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	var rules []string
	for _, name := range paths {
		if !allowedProtectedPaths[name] {
			continue
		}
		for _, tool := range []string{"Edit", "Write", "MultiEdit"} {
			rules = append(rules, fmt.Sprintf(`"%s(**/.%s/**)"`, tool, name))
		}
	}
	return strings.Join(rules, ",")
}

// bashCaptureHookScript is the PreToolUse hook installed for every claude
// agent session. It rewrites Bash tool_input.command so that the original
// command runs to completion in a sub-script, full stdout+stderr are
// persisted to a capture log, and only head + tail (≤8KB) are returned to
// the agent when the output is large. Token consumption is bounded without
// losing the full output for later inspection.
//
// __CAP_DIR__ is replaced at write time with the absolute path of the
// per-agent captures directory. The script is intentionally tolerant of
// missing jq / write failures: any error path emits `{}` so claude proceeds
// with the original command (degrading to BASH_MAX_OUTPUT_LENGTH default).
//
// Trust boundary: the agent runs Bash with bypassPermissions, so it could
// in principle modify the hook script or read capture files. This hook
// targets token-cost suppression, not a security boundary; with the marker
// emitting only a capture_id (not a path), an agent cannot incidentally
// inflate context by reading the capture back. A determined agent can still
// re-run a command without truncation, but that's no worse than the
// pre-existing baseline.
const bashCaptureHookScript = `#!/bin/bash
# kojo bash output capture hook (PreToolUse).
# Generated per-agent by kojo. Do not edit manually.
set -uo pipefail

JQ=$(command -v jq) || { echo '{}'; exit 0; }

input=$(cat)
tool=$(printf '%s' "$input" | "$JQ" -r '.tool_name // empty' 2>/dev/null) || { echo '{}'; exit 0; }
if [ "$tool" != "Bash" ]; then echo '{}'; exit 0; fi
orig=$(printf '%s' "$input" | "$JQ" -r '.tool_input.command // empty' 2>/dev/null) || { echo '{}'; exit 0; }
if [ -z "$orig" ]; then echo '{}'; exit 0; fi

cap_dir='__CAP_DIR__'
mkdir -p "$cap_dir" || { echo '{}'; exit 0; }

# Best-effort TTL prune (silent failures OK). Removes capture pairs older
# than 24h. find with -mmin skips files currently being written by parallel
# hook invocations because their mtime is fresh.
find "$cap_dir" -type f -mmin +1440 -delete 2>/dev/null || true

# mktemp avoids id collisions under concurrent Bash invocations. macOS BSD
# mktemp requires X's at end of template (no trailing extension allowed).
script=$(mktemp "$cap_dir/cmd-XXXXXX") || { echo '{}'; exit 0; }
id=${script##*/cmd-}
log="$cap_dir/out-$id.log"
printf '%s\n' "$orig" > "$script" || { echo '{}'; exit 0; }

# Capture the original tool_input into a variable with graceful fallback —
# if jq fails for any reason the merge falls back to a bare {command} object,
# matching the pre-fix behavior rather than crashing the hook.
orig_json=$(printf '%s' "$input" | "$JQ" -c '.tool_input // {}' 2>/dev/null) || orig_json='{}'
[ -z "$orig_json" ] && orig_json='{}'

# Shell-escape paths before embedding in the wrapper. cap_dir is controlled
# by kojo (no spaces / quotes by construction today), but printf %q is
# defensive against future config changes that might place agentDir on a
# path with whitespace.
script_q=$(printf '%q' "$script")
log_q=$(printf '%q' "$log")

# Wrapper command returned to claude as updatedInput.command.
# \$ escapes apply to the OUTER bash (this hook); the INNER bash (executed
# by claude) sees plain $ec / $sz.
wrapper="LC_ALL=C bash $script_q >$log_q 2>&1
ec=\$?
sz=\$(LC_ALL=C wc -c <$log_q | tr -d ' ')
if [ \"\$sz\" -le 8192 ]; then
  cat $log_q
else
  LC_ALL=C head -c 4096 $log_q
  printf '\\n...[KOJO_TRUNCATED %d bytes; capture_id=%s]...\\n' \"\$sz\" \"$id\"
  LC_ALL=C tail -c 4096 $log_q
fi
exit \$ec"

# updatedInput must preserve the rest of tool_input (description, timeout,
# run_in_background, …); only command is replaced. Without the merge, those
# fields would be silently dropped and claude would default them.
"$JQ" -nc --argjson orig "$orig_json" --arg cmd "$wrapper" '{
  hookSpecificOutput: {
    hookEventName: "PreToolUse",
    permissionDecision: "allow",
    permissionDecisionReason: "kojo bash capture",
    updatedInput: ($orig + { command: $cmd })
  }
}'
`

// hostOS is overridable for tests. Initialized to runtime.GOOS at startup.
var hostOS = runtime.GOOS

// writeBashCaptureHook materializes the per-agent PreToolUse hook script
// and creates the captures directory. Returns the absolute path of the
// hook script, suitable for embedding in settings.local.json.
func writeBashCaptureHook(claudeDir, captureDir string) (string, error) {
	if err := os.MkdirAll(captureDir, 0o755); err != nil {
		return "", fmt.Errorf("create capture dir: %w", err)
	}
	hooksDir := filepath.Join(claudeDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return "", fmt.Errorf("create hooks dir: %w", err)
	}
	// captureDir is embedded inside single quotes in the hook script
	// (cap_dir='__CAP_DIR__'), so any literal single quote in the path would
	// terminate the bash string and break the script. Escape using the
	// canonical '\'' close-open-close pattern. agentDir today never contains
	// quotes, but defensive escaping prevents future surprises.
	capDirSafe := strings.ReplaceAll(captureDir, `'`, `'\''`)
	script := strings.ReplaceAll(bashCaptureHookScript, "__CAP_DIR__", capDirSafe)
	hookPath := filepath.Join(hooksDir, "bash-capture.sh")
	// Atomic write: an in-flight bash hook invocation could be mid-read on
	// the existing file. os.WriteFile would truncate it and corrupt that
	// reader. Write to a sibling temp file then rename — rename is atomic
	// within a directory, so concurrent readers see either the old or new
	// version, never a half-written file.
	if err := atomicWriteFile(hookPath, []byte(script), 0o755); err != nil {
		return "", fmt.Errorf("write hook script: %w", err)
	}
	return hookPath, nil
}

// atomicWriteFile writes data to path via a temp+rename, ensuring concurrent
// readers never observe a partial write.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// PrepareClaudeSettings writes .claude/settings.local.json with persona
// override, the bash-capture PreToolUse hook (always installed), and
// (when apiBase is available) a PreCompact hook that calls kojo's API to
// save a conversation summary before Claude compacts context.
// Called from Manager.Chat before backend.Chat to ensure settings are in
// place before the Claude process reads them.
func PrepareClaudeSettings(agentID, apiBase string, allowProtectedPaths []string, logger *slog.Logger) {
	dir := agentDir(agentID)
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		logger.Warn("failed to create .claude dir", "agent", agentID, "err", err)
		return
	}

	captureDir := filepath.Join(claudeDir, "captures")
	// Captures are NOT pruned here. PrepareClaudeSettings is called from
	// prepareChat *before* the busy lock is acquired and can also fire
	// concurrently from multiple ChatOneShot invocations for the same agent
	// — a RemoveAll here would race against an in-flight claude process and
	// nuke its live capture files. Instead, captures are bounded by:
	//   1) per-call cleanup of stale entries (TTL) inside the hook itself,
	//   2) full wipe of .claude/ on agent reset (manager_lifecycle.go).
	//
	// Skip on Windows: the hook script is POSIX bash and depends on
	// mktemp / find / jq / wc / tr / head -c / tail -c / printf %q. Whether
	// Claude Code on Windows can even execute a `.sh` hook command is also
	// unverified. Falling back to BASH_MAX_OUTPUT_LENGTH default keeps the
	// agent functional; we just lose the capture-to-file behavior.
	var hookPath string
	if hostOS == "windows" {
		logger.Debug("bash capture hook skipped on windows", "agent", agentID)
	} else {
		var err error
		hookPath, err = writeBashCaptureHook(claudeDir, captureDir)
		if err != nil {
			// Non-fatal: claude can still run, just without capture.
			logger.Warn("failed to install bash capture hook", "agent", agentID, "err", err)
			hookPath = ""
		}
	}

	allowRules := buildProtectedPathAllowRules(allowProtectedPaths)
	bashHookBlock := ""
	if hookPath != "" {
		// hookPath is an absolute path under agentDir, no embedded quotes.
		bashHookBlock = fmt.Sprintf(`    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": %q
          }
        ]
      }
    ]`, hookPath)
	}

	var preCompactBlock string
	if apiBase != "" {
		curlFlags := "-s"
		if strings.HasPrefix(apiBase, "https://") {
			curlFlags = "-sk"
		}
		// Hook payload: claude pipes a JSON object to stdin describing the
		// triggering event. The fields kojo cares about are
		// `transcript_path` (so we read the exact session being compacted
		// instead of guessing) and `trigger` (manual vs auto, useful for
		// telemetry but not yet used for branching). We pass that through
		// verbatim with `--data-binary @-`. Older builds sent `-d '{}'`
		// which dropped the payload, forcing the API to probe for the
		// session file by mtime.
		// $KOJO_AGENT_TOKEN is injected into the PTY by filterEnv. The
		// auth listener requires it on every /api/v1/* call; without
		// the X-Kojo-Token header the PreCompact hook would 403 from
		// the agent perspective.
		preCompactBlock = fmt.Sprintf(`    "PreCompact": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "curl %s -X POST '%s/api/v1/agents/%s/pre-compact' -H 'Content-Type: application/json' -H \"X-Kojo-Token: $KOJO_AGENT_TOKEN\" --data-binary @- --max-time 120"
          }
        ]
      }
    ]`, curlFlags, apiBase, agentID)
	}

	hookBlocks := []string{}
	if bashHookBlock != "" {
		hookBlocks = append(hookBlocks, bashHookBlock)
	}
	if preCompactBlock != "" {
		hookBlocks = append(hookBlocks, preCompactBlock)
	}

	var settings string
	if len(hookBlocks) == 0 {
		settings = fmt.Sprintf(`{"persona":"agent-managed","permissions":{"defaultMode":"bypassPermissions","allow":[%s]}}`, allowRules) + "\n"
	} else {
		settings = fmt.Sprintf(`{
  "persona": "agent-managed",
  "permissions": {
    "defaultMode": "bypassPermissions",
    "allow": [%s]
  },
  "hooks": {
%s
  }
}
`, allowRules, strings.Join(hookBlocks, ",\n"))
	}

	settingsPath := filepath.Join(claudeDir, "settings.local.json")
	// Atomic write — claude reads settings.local.json on startup, and a
	// concurrent Chat overwriting it could surface a partial JSON to a
	// claude process that's also booting.
	if err := atomicWriteFile(settingsPath, []byte(settings), 0o644); err != nil {
		logger.Warn("failed to write claude settings", "agent", agentID, "err", err)
	}
}

// isRealUserEntry returns true if the session JSONL "user" entry
// represents a real user message (not a tool_result feedback).
func isRealUserEntry(msgRaw json.RawMessage) bool {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(msgRaw, &msg) != nil {
		return false
	}
	// Try parsing content as an array of typed blocks.
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(msg.Content, &blocks) != nil {
		// Not an array — plain string content is a real user message.
		return true
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return false
		}
	}
	return true
}

package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// persistentSessionsEnabled reports whether the Phase-3 persistent claude
// session pool is active. Enabled by default; set KOJO_PERSISTENT_SESSIONS=0
// to fall back to the per-turn spawn model.
func persistentSessionsEnabled() bool {
	return os.Getenv("KOJO_PERSISTENT_SESSIONS") != "0"
}

// sessionIdleTimeout is how long a persistent process may sit with no turn
// activity before its stdin is closed and it exits. The next turn respawns
// transparently with --resume.
const sessionIdleTimeout = 30 * time.Minute

type sessionState int

const (
	sessIdle sessionState = iota
	sessInTurn
	sessDead
)

// claudeSession owns one live `claude` CLI process for an agent's main chat,
// kept alive across turns (--input-format stream-json). A single readLoop
// goroutine parses the process stdout and demuxes turns: events during an
// active solicited turn stream to that turn's channel; events that arrive
// while idle form an unsolicited turn (a background subagent notification)
// which is persisted + broadcast via the Manager's background-turn handler.
type claudeSession struct {
	b           *ClaudeBackend
	agentID     string
	dir         string
	logger      *slog.Logger
	fingerprint string

	// procCtx governs the process lifetime and is independent of any turn's
	// context — aborting a turn must not kill the shared process.
	procCtx    context.Context
	procCancel context.CancelFunc
	cmd        *exec.Cmd
	stdinW     *claudeStdinWriter
	stderr     *bytes.Buffer
	// qstate answers interactive AskUserQuestion control_requests. Shared
	// across turns on this process (requestIDs are unique per prompt).
	qstate *claudeQuestionState

	// tailer surfaces background-subagent (Task run_in_background) output that
	// outlives the spawning turn. Owned by this session; its poll loop is tied
	// to procCtx so it stops when the process exits (no goroutine leak). nil
	// when no subagent-activity handler is registered.
	tailer *subagentTailer

	mu           sync.Mutex
	state        sessionState
	sessionID    string // authoritative, from result events
	lastActivity time.Time
	reapTimer    *time.Timer
	prevUsage    Usage // cumulative modelUsage totals seen so far (for per-turn delta)

	// current turn (guarded by mu)
	acc           *turnAccumulator
	turnSink      chan ChatEvent  // events for the active turn
	turnCtx       context.Context // solicited turn's context (Background for unsolicited)
	unsolicited   bool            // true when the active turn is a background notification
	turnCanAnswer bool            // true when a user can answer AskUserQuestion this turn
	turnAutomated bool            // true when the active turn is automated (question held with a timeout)
	turnDone    chan struct{}   // closed when the active turn's result is delivered
}

// sessionSend delivers e to the turn sink, but never blocks the shared
// readLoop indefinitely: it aborts if the turn context is cancelled or a
// generous ceiling elapses (a wedged consumer). Returns false if the send did
// not complete.
func sessionSend(ctx context.Context, sink chan<- ChatEvent, e ChatEvent) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	t := time.NewTimer(60 * time.Second)
	defer t.Stop()
	select {
	case sink <- e:
		return true
	case <-ctx.Done():
		return false
	case <-t.C:
		return false
	}
}

// fingerprintArgs derives a spawn fingerprint from the CLI args, excluding the
// session-resumption flag (--resume/--session-id) whose value/kind may flip
// between turns (a fresh session file becomes resumable after turn 1) without
// meaning the process must be respawned. A mismatch (model/effort/system
// prompt/MCP change) forces a graceful close + respawn.
func fingerprintArgs(args []string) string {
	h := sha256.New()
	skip := false
	for _, a := range args {
		if skip {
			skip = false
			continue
		}
		if a == "--resume" || a == "--session-id" {
			skip = true
			continue
		}
		h.Write([]byte(a))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// chatViaSession runs one main-chat turn on a persistent process, spawning or
// reusing one as needed. It returns the per-turn event channel with the same
// contract as ClaudeBackend.Chat (streams text/thinking/tool events, ends
// with a single "done" or "error"). ctx is the turn context: its cancellation
// interrupts the running turn (via the CLI interrupt control protocol) without
// killing the shared process.
func (b *ClaudeBackend) chatViaSession(ctx context.Context, agent *Agent, userMessage, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, error) {
	dir := agentDir(agent.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	inv := b.buildClaudeInvocation(agent, systemPrompt, dir, opts.OneShot, opts.MCPServers, opts.AutomatedTrigger, opts.SessionKey)
	args := inv.args
	fp := fingerprintArgs(args)

	b.sessMu.Lock()
	sess := b.sessions[agent.ID]
	// A live process must not be reused across a context-threshold reset —
	// it still holds the full in-memory context the reset meant to drop.
	// A merely-missing JSONL (bootstrapRecentContext without sessionWasReset)
	// is NOT a respawn reason: under the persistent model the conversation
	// context lives in the process, and the CLI may not have flushed a
	// session file yet.
	if sess != nil && (!sess.healthy(fp) || inv.sessionWasReset) {
		b.logger.Debug("claude session respawn",
			"agent", agent.ID,
			"fpMatch", sess.fingerprint == fp,
			"reset", inv.sessionWasReset,
			"state", sess.stateSnapshot())
		sess.close()
		delete(b.sessions, agent.ID)
		// Wait (bounded) for the old process to actually release the
		// session before spawning a new one with the same deterministic
		// session id — otherwise the CLI rejects the spawn with
		// "Session ID already in use".
		waitSess := sess
		wasReset := inv.sessionWasReset
		b.sessMu.Unlock()
		waitSess.awaitDead(5 * time.Second)
		// In the reset case the dying process may have flushed/regenerated its
		// session JSONL on exit. If we rebuilt now the file would exist again,
		// flipping the invocation back to --resume and silently undoing the
		// reset (the fresh process would inherit the very context the reset
		// meant to drop). Delete it again — only in the reset case — so the
		// rebuild chooses --session-id and starts genuinely fresh.
		if wasReset {
			removeClaudeSession(agent.ID, opts.SessionKey)
		}
		inv = b.buildClaudeInvocation(agent, systemPrompt, dir, opts.OneShot, opts.MCPServers, opts.AutomatedTrigger, opts.SessionKey)
		args = inv.args
		fp = fingerprintArgs(args)
		b.sessMu.Lock()
		// Re-check: another goroutine may have spawned meanwhile.
		sess = b.sessions[agent.ID]
	}
	spawned := false
	if sess == nil {
		var err error
		sess, err = b.spawnSession(agent.ID, dir, fp, args)
		if err != nil {
			b.sessMu.Unlock()
			return nil, err
		}
		b.sessions[agent.ID] = sess
		spawned = true
	}
	b.sessMu.Unlock()

	// In-use recovery — done OUTSIDE the global sessMu so the up-to-700ms
	// probe (and 3s awaitDead on the rare recovery) never stalls other agents'
	// session operations. A kill -9'd predecessor leaves its JSONL in an
	// unclean state that claude rejects as "Session ID already in use" for a
	// multi-minute grace window; detect that fast early death, drop the stale
	// session file, and respawn once with a fresh --session-id (recent-context
	// bootstrap recovers continuity). Only a process we just spawned can hit
	// this — a reused live session is already past startup.
	if spawned && b.sessionDiedInUse(sess, 700*time.Millisecond) {
		b.logger.Warn("claude session id in use on spawn; resetting session file + respawning", "agent", agent.ID)
		sess.close()
		sess.awaitDead(3 * time.Second)
		// Hold sessMu across the slot check, the session-file reset, and the
		// respawn: a concurrently registered live session must be adopted
		// BEFORE the reset (deleting its JSONL from under it would corrupt
		// its resume state), and the slot stays reserved until our new
		// process is registered. spawnSession is cmd.Start-fast, so the
		// global lock hold is short.
		b.sessMu.Lock()
		if cur := b.sessions[agent.ID]; cur != nil && cur != sess {
			// Another goroutine registered a live session meanwhile — use
			// theirs and skip the reset entirely. That session already
			// carries its own context, so this turn no longer bootstraps.
			b.sessMu.Unlock()
			sess = cur
			spawned = false
		} else {
			resetClaudeSessionFiles(agent.ID, opts.SessionKey, b.logger)
			inv = b.buildClaudeInvocation(agent, systemPrompt, dir, opts.OneShot, opts.MCPServers, opts.AutomatedTrigger, opts.SessionKey)
			args = inv.args
			fp = fingerprintArgs(args)
			newSess, err := b.spawnSession(agent.ID, dir, fp, args)
			if err != nil {
				if b.sessions[agent.ID] == sess {
					delete(b.sessions, agent.ID)
				}
				b.sessMu.Unlock()
				return nil, err
			}
			b.sessions[agent.ID] = newSess
			b.sessMu.Unlock()
			sess = newSess
		}
	}

	// Recent-context bootstrap applies only when this turn actually starts
	// a fresh session process (--session-id). A reused live session already
	// carries the conversation in memory — injecting the recap would
	// duplicate context the model just saw.
	if spawned && inv.bootstrapRecentContext && opts.RecentMessagesContext != "" {
		userMessage = injectRecentMessagesContext(userMessage, opts.RecentMessagesContext)
	}

	// A question can be surfaced whenever the caller wired an answer channel.
	// Automated turns surface it too (held with a timeout by handleControlRequest)
	// instead of auto-denying; only turns with no answer channel deny inline.
	canAnswer := opts.OnQuestionReady != nil
	ch, err := sess.startTurn(ctx, agent, userMessage, canAnswer, opts.AutomatedTrigger)
	if err == nil {
		if opts.OnSteerReady != nil {
			opts.OnSteerReady(sess.stdinW.writeUserLine)
		}
		if canAnswer {
			opts.OnQuestionReady(sess.qstate.answer)
		}
		return ch, nil
	}
	// ErrAgentBusy means the session is legitimately mid-turn (e.g. a
	// background notification turn) — surface it without killing the process.
	if errors.Is(err, ErrAgentBusy) {
		return nil, err
	}
	// A dead session or write failure: drop it and let the next turn respawn.
	b.sessMu.Lock()
	if b.sessions[agent.ID] == sess {
		sess.close()
		delete(b.sessions, agent.ID)
	}
	b.sessMu.Unlock()
	return nil, err
}

// sessionDiedInUse reports whether the just-spawned process exited within the
// window with a "session id already in use" error. Returns false as soon as
// the window elapses with the process still alive (the healthy case), so it
// only adds latency to a genuinely-colliding cold spawn.
func (b *ClaudeBackend) sessionDiedInUse(s *claudeSession, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		dead := s.state == sessDead
		s.mu.Unlock()
		if dead {
			stderr := ""
			if s.stderr != nil {
				stderr = strings.ToLower(s.stderr.String())
			}
			return strings.Contains(stderr, "already in use")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// spawnSession starts a new persistent CLI process and its readLoop. Caller
// holds sessMu.
func (b *ClaudeBackend) spawnSession(agentID, dir, fp string, args []string) (*claudeSession, error) {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return nil, err
	}
	procCtx, procCancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, claudePath, args...)
	cmd.Env = filterEnv([]string{"CLAUDE_CODE", "CLAUDECODE", "AGENT_BROWSER_SESSION", "AGENT_BROWSER_COOKIE_DIR"}, agentID, dir)
	cmd.Env = append(cmd.Env, "CLAUDE_CODE_DISABLE_1M_CONTEXT=1", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=85")
	if b.proxyURL != "" {
		cmd.Env = append(cmd.Env, "ANTHROPIC_BASE_URL="+b.proxyURL)
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			cmd.Env = append(cmd.Env, "ANTHROPIC_API_KEY=dummy")
		}
	}
	cmd.Dir = dir
	// Run the CLI in its own process group so background-subagent children
	// (Task/Bash run_in_background) can be signalled as a group. A parent-only
	// kill leaves those children orphaned, and an orphaned child keeps the
	// deterministic session id open — the next spawn then fails with
	// "Session ID already in use".
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			// Negative pid → signal the whole process group (parent + children).
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			return cmd.Process.Signal(syscall.SIGTERM)
		}
		return nil
	}
	cmd.WaitDelay = 10 * time.Second

	var stderrBuf bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderrBuf, limit: 4096}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		procCancel()
		return nil, err
	}
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		procCancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		procCancel()
		return nil, err
	}

	s := &claudeSession{
		b:            b,
		agentID:      agentID,
		dir:          dir,
		logger:       b.logger,
		fingerprint:  fp,
		procCtx:      procCtx,
		procCancel:   procCancel,
		cmd:          cmd,
		stdinW:       &claudeStdinWriter{w: stdinPipe},
		stderr:       &stderrBuf,
		state:        sessIdle,
		lastActivity: time.Now(),
	}
	s.qstate = newClaudeQuestionState(s.stdinW)
	// Background-subagent tailer: surfaces Task(run_in_background) output that
	// outlives the spawning turn. Tied to procCtx so the poll loop dies with
	// the process. Only started when a handler is registered.
	if b.onSubagentActivity != nil {
		s.tailer = newSubagentTailer(agentID, dir, b.logger, s.currentSessionID, b.onSubagentActivity)
		go s.tailer.run(procCtx)
	}
	go s.readLoop(stdout)
	b.logger.Info("claude persistent session spawned", "agent", agentID)
	return s, nil
}

// currentSessionID returns the authoritative session id (from a result event)
// when known, else the deterministic id for this agent's main chat — the only
// chat kind that uses a persistent session, so sessionKey is always empty here.
func (s *claudeSession) currentSessionID() string {
	s.mu.Lock()
	sid := s.sessionID
	s.mu.Unlock()
	if sid != "" {
		return sid
	}
	return agentIDToUUID(s.agentID)
}

// awaitDead polls (bounded by timeout) until the session process has fully
// terminated (state sessDead, set by onEOF after cmd.Wait). Used before
// respawning with the same deterministic session id — the CLI rejects a
// second process on a session that is still open ("Session ID already in
// use").
func (s *claudeSession) awaitDead(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		dead := s.state == sessDead
		s.mu.Unlock()
		if dead {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.logger.Warn("claude session did not exit within grace; spawning anyway", "agent", s.agentID)
}

// stateSnapshot returns the current session state under the lock. Used by
// callers (e.g. the respawn debug log) that must read state without holding mu.
func (s *claudeSession) stateSnapshot() sessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// isDead reports whether onEOF has finished tearing the session down.
func (s *claudeSession) isDead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == sessDead
}

// healthy reports whether the session can serve a turn with the given spawn
// fingerprint (same model/effort/system prompt/MCP config and not dead).
func (s *claudeSession) healthy(fp string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state != sessDead && s.fingerprint == fp
}

// startTurn registers a solicited turn, writes the user message onto the live
// stdin, and returns the turn's event channel. The returned channel receives
// streaming events and a terminal done/error, then closes.
func (s *claudeSession) startTurn(ctx context.Context, agent *Agent, userMessage string, canAnswer, automated bool) (<-chan ChatEvent, error) {
	s.mu.Lock()
	if s.state == sessDead {
		s.mu.Unlock()
		return nil, ErrAgentNotBusy
	}
	if s.state == sessInTurn {
		s.mu.Unlock()
		return nil, ErrAgentBusy
	}
	if s.reapTimer != nil {
		s.reapTimer.Stop()
		s.reapTimer = nil
	}
	sink := make(chan ChatEvent, 64)
	done := make(chan struct{})
	s.state = sessInTurn
	s.unsolicited = false
	s.turnCanAnswer = canAnswer
	s.turnAutomated = automated
	s.turnSink = sink
	s.turnCtx = ctx
	s.turnDone = done
	s.lastActivity = time.Now()
	s.acc = newTurnAccumulator(s.logger, func(e ChatEvent) bool {
		return sessionSend(ctx, sink, e)
	})
	s.mu.Unlock()

	if err := s.stdinW.writeUserLine(userMessage); err != nil {
		// Single-writer rule: only readLoop/onEOF may close turnSink. Closing
		// it here would race a concurrent readLoop send → panic. Instead force
		// process teardown; onEOF drives the registered turn to a terminal
		// error event and closes the sink.
		s.close()
		return nil, err
	}

	// Watch the turn context: on abort, interrupt the running turn (the
	// process stays alive). The turn ends normally via the ensuing
	// result/error_during_execution event.
	go func() {
		select {
		case <-ctx.Done():
			// The caller cancels ctx via defer AFTER a normal completion
			// too, so both select arms can be ready at once and this arm
			// can win. Re-check under the session lock that THIS turn is
			// still the running one (turnDone identity) before firing any
			// abort work — otherwise a deny/interrupt could land on an
			// idle session or, worse, on the NEXT turn.
			isStale := func() bool {
				s.mu.Lock()
				defer s.mu.Unlock()
				return s.turnDone != done || s.state != sessInTurn
			}
			if isStale() {
				return
			}
			// Start the escalation timer BEFORE the control writes and fire them
			// in their own goroutine: a wedged stdin pipe must not delay
			// escalation. Without this the session could sit in sessInTurn
			// forever and the Manager's busy/drain paths would never clear.
			// Deny any question the model is blocked on FIRST (so the CLI can
			// act on the interrupt rather than idling on an unanswered
			// control_response), then interrupt. close() EOFs stdin then cancels
			// the process group; the readLoop's onEOF fails the turn and marks
			// the session dead, so the next turn respawns via --resume.
			go func() {
				// Re-check inside the goroutine too: it may be scheduled
				// arbitrarily late, after this turn completed or the next
				// one started, and both the deny and the interrupt write
				// are process-global — neither may hit a successor turn.
				if isStale() {
					return
				}
				s.qstate.denyAllPending("turn aborted")
				s.interrupt()
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				s.logger.Warn("claude interrupt unanswered; killing session process", "agent", s.agentID)
				// Kill the process group directly — close() funnels through
				// the stdin mutex, which a wedged interrupt write may hold.
				s.forceKill()
			}
		case <-done:
		}
	}()

	return sink, nil
}

// interrupt sends the CLI interrupt control request to abort the running turn
// without killing the process.
func (s *claudeSession) interrupt() {
	line, _ := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": "kojo-interrupt-" + generateMessageID(),
		"request":    map[string]any{"subtype": "interrupt"},
	})
	s.stdinW.mu.Lock()
	if !s.stdinW.closed {
		_, _ = s.stdinW.w.Write(append(line, '\n'))
	}
	s.stdinW.mu.Unlock()
}

// handleControlRequest resolves a can_use_tool control_request against the
// active turn. Questions on an automated/unsolicited turn (or with no active
// solicited turn) are auto-denied; questions on a watched user turn are
// registered and surfaced via the turn sink; non-question tools auto-allow.
func (s *claudeSession) handleControlRequest(cr *controlRequestMsg) {
	s.mu.Lock()
	acc := s.acc
	// A question can only be surfaced/answered on a live turn that wired an
	// answer channel. Otherwise force the immediate-deny path (qstate nil).
	noChannel := !s.turnCanAnswer || s.state != sessInTurn || acc == nil
	// Unsolicited (background notification) and system-triggered turns are
	// automated: hold the question with a timeout rather than until turn end.
	automated := s.turnAutomated || s.unsolicited
	s.mu.Unlock()
	send := func(ChatEvent) bool { return true }
	if acc != nil {
		send = acc.send
	}
	qstate := s.qstate
	if noChannel {
		qstate = nil
	}
	var questionTimeout time.Duration
	if automated {
		questionTimeout = automatedQuestionTimeout
	}
	handleControlRequest(cr, s.stdinW, qstate, questionTimeout, s.logger, send)
}

// readLoop parses the process stdout for its whole lifetime, demuxing turns.
func (s *claudeSession) readLoop(stdout interface{ Read([]byte) (int, error) }) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if cr, ok := maybeControlRequest(line); ok {
			s.handleControlRequest(cr)
			continue
		}
		event, rawParent, ok := decodeClaudeStreamLine(line, s.logger)
		if !ok {
			continue
		}

		// A background-subagent notification result carries
		// origin.kind=="task-notification". Empirically (claude 2.1.201) the
		// CLI serializes turns: such a result is emitted only while the session
		// is idle, and mid-turn background completions instead surface as
		// `system` subtype=task_notification events folded into the active turn.
		// This flag drives the defensive demux for the rare interleave where a
		// notification result nonetheless arrives during a solicited turn.
		notifResult := event.Type == "result" && event.Origin != nil && event.Origin.Kind == "task-notification"

		s.mu.Lock()
		if s.state == sessInTurn && !s.unsolicited && notifResult {
			// Route the racing notification to a SEPARATE unsolicited
			// accumulator: never feed it to the solicited turn's accumulator
			// (it would pollute the user turn's content) and never let it
			// complete the user turn.
			s.lastActivity = time.Now()
			s.mu.Unlock()
			s.absorbNotification(event, rawParent)
			continue
		}
		if s.state != sessInTurn {
			// Only a meaningful event (model output) starts an unsolicited
			// turn. Stray idle events — a trailing rate_limit_event, a lone
			// system/init the CLI emits when ready for the next turn, a
			// control_response — must NOT open a turn: nothing would ever
			// close it (no result follows) and the agent would wedge busy
			// forever.
			if !isTurnOpeningEvent(event) {
				s.lastActivity = time.Now()
				s.mu.Unlock()
				continue
			}
			s.openUnsolicitedLocked()
		}
		acc := s.acc
		if event.SessionID != "" {
			s.sessionID = event.SessionID
		}
		s.lastActivity = time.Now()
		s.mu.Unlock()

		if acc == nil { // background handler unavailable; drop
			continue
		}
		isResult := acc.feed(event, rawParent)
		if isResult {
			// Demux by origin: a solicited (user) turn must terminate only on
			// its OWN result (no origin). A "task-notification" result belongs
			// to a background subagent from an EARLIER turn that happened to
			// finish mid-turn — ignore it here so it doesn't truncate the
			// active user turn. An unsolicited turn terminates on any result.
			s.mu.Lock()
			unsolicited := s.unsolicited
			s.mu.Unlock()
			if unsolicited || acc.res.origin != "task-notification" {
				s.completeTurn()
			}
		}
	}
	s.onEOF()
}

// isTurnOpeningEvent reports whether an event arriving while the session is
// idle should begin an unsolicited (background notification) turn. Only events
// that carry actual model output qualify; bookkeeping events (system,
// rate_limit_event, control_response) are ignored so a stray trailing event
// never opens a turn that will never see a result.
func isTurnOpeningEvent(e claudeStreamEvent) bool {
	switch e.Type {
	case "assistant", "content_block_start", "content_block_delta", "content_block_stop", "user":
		return true
	case "result":
		// A lone result with content/origin (e.g. a notification turn whose
		// only body is the result text) still forms a real turn.
		return e.Result != "" || e.Origin != nil
	default:
		return false
	}
}

// openUnsolicitedLocked begins a background (notification) turn. Caller holds
// mu. If no background handler is registered, acc stays nil and the events are
// dropped.
func (s *claudeSession) openUnsolicitedLocked() {
	if s.b.onBackgroundTurn == nil {
		return
	}
	sink := make(chan ChatEvent, 64)
	s.state = sessInTurn
	s.unsolicited = true
	// An unsolicited turn is automated, but a watching human can still answer an
	// AskUserQuestion it raises — the answer func is handed to the background
	// turn handler below, and turnCanAnswer lets handleControlRequest surface the
	// card (held with a timeout since unsolicited turns count as automated).
	s.turnCanAnswer = true
	s.turnSink = sink
	s.turnCtx = context.Background()
	s.turnDone = make(chan struct{})
	s.acc = newTurnAccumulator(s.logger, func(e ChatEvent) bool {
		return sessionSend(context.Background(), sink, e)
	})
	go s.b.onBackgroundTurn(s.agentID, sink, s.qstate.answer)
}

// absorbNotification handles a task-notification result that raced into an
// active solicited turn: it builds a throwaway unsolicited accumulator +
// background turn from just that result, fully isolated from the solicited
// turn's accumulator and completion. If no background handler is registered the
// notification is dropped.
func (s *claudeSession) absorbNotification(event claudeStreamEvent, rawParent string) {
	if s.b.onBackgroundTurn == nil {
		return
	}
	sink := make(chan ChatEvent, 32)
	acc := newTurnAccumulator(s.logger, func(e ChatEvent) bool {
		return sessionSend(context.Background(), sink, e)
	})
	go s.b.onBackgroundTurn(s.agentID, sink, s.qstate.answer)
	acc.feed(event, rawParent)
	res := acc.finalize()
	turnUsage := s.turnUsageDelta(res)
	finalText := mergeStreamTexts(res)
	if finalText == "" {
		close(sink)
		return
	}
	if res.fullText == "" {
		sessionSend(context.Background(), sink, ChatEvent{Type: "text", Delta: finalText})
	}
	msg := assembleAssistantMessage(finalText, res.thinking, res.toolUses, turnUsage)
	sessionSend(context.Background(), sink, ChatEvent{Type: "done", Message: msg, Usage: turnUsage})
	close(sink)
}

// completeTurn finalizes the active turn at its result boundary: assembles and
// emits the terminal done event, closes the turn sink, and returns to idle.
func (s *claudeSession) completeTurn() {
	s.mu.Lock()
	acc := s.acc
	sink := s.turnSink
	unsolicited := s.unsolicited
	done := s.turnDone
	turnCtx := s.turnCtx
	s.acc = nil
	s.turnSink = nil
	s.turnCtx = nil
	s.state = sessIdle
	s.unsolicited = false
	s.turnDone = nil
	s.armReapLocked()
	s.mu.Unlock()

	// Backfill (Option C): a turn just ended, so any background subagent it
	// spawned may have flushed transcript lines the poll loop hasn't picked up
	// yet. Kick an immediate scan off the readLoop goroutine to cut latency.
	if s.tailer != nil {
		go s.tailer.scanOnce()
	}

	// Release any question left unanswered at the turn boundary (abort path).
	s.qstate.denyAllPending("turn ended before the question was answered")

	if acc == nil || sink == nil {
		return
	}
	res := acc.finalize()

	// result.modelUsage is cumulative per CLI process; convert to this turn's
	// delta so the persisted per-turn usage isn't inflated by prior turns.
	// Done BEFORE the empty-turn drop below so a dropped (content-less)
	// unsolicited turn still advances prevUsage — otherwise its cumulative
	// tokens would be re-charged to the next visible turn's delta.
	turnUsage := s.turnUsageDelta(res)

	// Unsolicited turns with no content (stray rate-limit events etc.) must
	// not persist an empty message: close the sink without a done event so
	// the background handler drops it.
	if unsolicited && res.fullText == "" && res.lastAssistantText == "" && len(res.toolUses) == 0 {
		close(sink)
		if done != nil {
			close(done)
		}
		return
	}

	finalText := mergeStreamTexts(res)
	if finalText == "" {
		recoverID := res.streamSessionID
		if recoverID == "" {
			recoverID = s.sessionID
		}
		if recoverID != "" {
			if t := recoverFromSession(s.agentID, recoverID, s.logger); t != "" {
				finalText = t
			}
		}
	}
	if finalText != "" && res.fullText == "" {
		sessionSend(turnCtx, sink, ChatEvent{Type: "text", Delta: finalText})
	}
	msg := assembleAssistantMessage(finalText, res.thinking, res.toolUses, turnUsage)
	sessionSend(turnCtx, sink, ChatEvent{Type: "done", Message: msg, Usage: turnUsage})
	close(sink)
	if done != nil {
		close(done)
	}
}

// turnUsageDelta converts a result's cumulative-per-process modelUsage into
// this turn's delta and advances prevUsage. Non-cumulative usage (stream
// accumulation when modelUsage is absent) is already per-turn and returned
// as-is. Always advances prevUsage for cumulative usage so no turn's tokens
// are double-counted, even if the turn itself is later dropped.
func (s *claudeSession) turnUsageDelta(res *streamParseResult) *Usage {
	if res.usage == nil || !res.usageCumulative {
		return res.usage
	}
	s.mu.Lock()
	prev := s.prevUsage
	s.prevUsage = *res.usage
	s.mu.Unlock()
	delta := &Usage{
		InputTokens:              res.usage.InputTokens - prev.InputTokens,
		OutputTokens:             res.usage.OutputTokens - prev.OutputTokens,
		CacheReadInputTokens:     res.usage.CacheReadInputTokens - prev.CacheReadInputTokens,
		CacheCreationInputTokens: res.usage.CacheCreationInputTokens - prev.CacheCreationInputTokens,
		CostUSD:                  res.usage.CostUSD - prev.CostUSD,
	}
	// Guard against negatives (counters reset mid-process): fall back to raw.
	if delta.InputTokens < 0 || delta.OutputTokens < 0 || delta.CacheReadInputTokens < 0 || delta.CacheCreationInputTokens < 0 {
		return res.usage
	}
	return delta
}

// onEOF handles process exit: any in-flight turn gets an error/done so its
// consumer unblocks, and the session is marked dead so the next turn respawns.
func (s *claudeSession) onEOF() {
	if s.cmd != nil {
		_ = s.cmd.Wait()
		// Reap any orphaned background-subagent children still in the process
		// group. If the parent was kill -9'd externally, its children survive
		// and keep the deterministic session id open — a group SIGKILL here
		// releases it before the next spawn so it won't hit "already in use".
		if s.cmd.Process != nil {
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	stderr := ""
	if s.stderr != nil {
		stderr = strings.TrimSpace(s.stderr.String())
	}
	s.logger.Info("claude persistent session exited",
		"agent", s.agentID,
		"stderr", headRunes(stderr, 300))

	s.mu.Lock()
	acc := s.acc
	sink := s.turnSink
	done := s.turnDone
	turnCtx := s.turnCtx
	s.acc = nil
	s.turnSink = nil
	s.turnCtx = nil
	s.turnDone = nil
	s.state = sessDead
	if s.reapTimer != nil {
		s.reapTimer.Stop()
		s.reapTimer = nil
	}
	s.mu.Unlock()

	// Process is gone; clear any pending question (writes fail harmlessly).
	s.qstate.denyAllPending("session process exited")

	if sink != nil {
		if acc != nil {
			res := acc.finalize()
			finalText := mergeStreamTexts(res)
			if finalText != "" {
				sessionSend(turnCtx, sink, ChatEvent{Type: "done", Message: assembleAssistantMessage(finalText, res.thinking, res.toolUses, res.usage), Usage: res.usage})
			} else {
				msg := stderr
				if msg == "" {
					msg = "claude process exited"
				}
				sessionSend(turnCtx, sink, ChatEvent{Type: "error", ErrorMessage: msg})
			}
		} else {
			sessionSend(turnCtx, sink, ChatEvent{Type: "error", ErrorMessage: "claude process exited"})
		}
		close(sink)
	}
	if done != nil {
		close(done)
	}

	// Evict from the pool so a later turn spawns fresh.
	s.b.sessMu.Lock()
	if s.b.sessions[s.agentID] == s {
		delete(s.b.sessions, s.agentID)
	}
	s.b.sessMu.Unlock()
}

// armReapLocked (re)starts the idle reap timer. Caller holds mu.
func (s *claudeSession) armReapLocked() {
	if s.reapTimer != nil {
		s.reapTimer.Stop()
	}
	s.reapTimer = time.AfterFunc(sessionIdleTimeout, func() {
		// Mark dead UNDER the lock so a startTurn that races the timer either
		// wins (state becomes inTurn first → we observe not-idle and skip) or
		// loses (we set dead first → startTurn sees dead and respawns). This
		// closes the TOCTOU where reap could kill a just-started turn.
		s.mu.Lock()
		if s.state != sessIdle {
			s.mu.Unlock()
			return
		}
		s.state = sessDead
		s.reapTimer = nil
		s.mu.Unlock()

		s.logger.Info("claude persistent session idle-reaped", "agent", s.agentID)
		s.stdinW.close()
		if s.procCancel != nil {
			s.procCancel()
		}
		s.b.sessMu.Lock()
		if s.b.sessions[s.agentID] == s {
			delete(s.b.sessions, s.agentID)
		}
		s.b.sessMu.Unlock()
	})
}

// close closes stdin and terminates the process. Safe to call multiple times.
func (s *claudeSession) close() {
	s.mu.Lock()
	if s.state == sessDead {
		s.mu.Unlock()
		return
	}
	if s.reapTimer != nil {
		s.reapTimer.Stop()
		s.reapTimer = nil
	}
	s.mu.Unlock()
	s.stdinW.close()
	// Give the process a brief chance to exit on its own after stdin EOF,
	// then force-cancel. The readLoop's onEOF marks the session dead.
	if s.procCancel != nil {
		go func() {
			time.Sleep(2 * time.Second)
			s.procCancel()
		}()
	}
}

// forceKill cancels the process group immediately, bypassing stdin entirely —
// the stdin mutex may be held by a wedged write, so close() (which EOFs stdin
// first) is not safe to call from escalation paths. onEOF still runs off the
// readLoop and owns all cleanup.
func (s *claudeSession) forceKill() {
	if s.procCancel != nil {
		s.procCancel()
	}
	// procCancel is SIGTERM-based and a no-op when already cancelled — a
	// process that ignored the earlier TERM would survive it. SIGKILL the
	// group directly; ESRCH after exit is fine.
	if s.cmd != nil && s.cmd.Process != nil {
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
	}
}

// closeClaudeSession terminates any live persistent claude process for the
// agent. No-op for non-claude backends or when the pool is empty. Called from
// the restart drain (async close is fine — the process only needs to be gone
// eventually).
func (m *Manager) closeClaudeSession(agentID string) {
	if cb, ok := m.backends["claude"].(*ClaudeBackend); ok {
		cb.CloseSession(agentID)
	}
}

// closeClaudeSessionSync is the synchronous variant used by destructive
// lifecycle paths (reset, archive/delete, device-switch handoff) that
// delete/transfer the session JSONL immediately after and must not race the
// old process re-creating files during its close grace.
func (m *Manager) closeClaudeSessionSync(agentID string) {
	if cb, ok := m.backends["claude"].(*ClaudeBackend); ok {
		cb.CloseSessionSync(agentID)
	}
}

// claudeSessionInTurn reports whether the agent's persistent claude process is
// mid-turn. An idle live session is not busy.
func (m *Manager) claudeSessionInTurn(agentID string) bool {
	if cb, ok := m.backends["claude"].(*ClaudeBackend); ok {
		return cb.SessionInTurn(agentID)
	}
	return false
}

// CloseSession terminates any persistent process for the agent. Used by the
// Manager on reset, restart drain, device-switch quiesce, and archive/delete.
func (b *ClaudeBackend) CloseSession(agentID string) {
	b.sessMu.Lock()
	sess := b.sessions[agentID]
	delete(b.sessions, agentID)
	b.sessMu.Unlock()
	if sess != nil {
		sess.close()
	}
}

// CloseSessionSync terminates the agent's persistent process AND waits
// (bounded) for it to fully exit before returning. Destructive lifecycle paths
// (context-threshold reset, archive/delete, device-switch handoff) delete or
// transfer the session JSONL immediately afterward and must not race the old
// process re-creating files during its async close grace. The restart drain,
// which only needs the process gone eventually, keeps the async CloseSession.
func (b *ClaudeBackend) CloseSessionSync(agentID string) {
	b.sessMu.Lock()
	sess := b.sessions[agentID]
	delete(b.sessions, agentID)
	b.sessMu.Unlock()
	if sess != nil {
		// close() funnels through the stdin mutex, which a wedged write can
		// hold indefinitely — run it async so the grace clock starts NOW and
		// the escalation below can never be postponed by a stuck pipe.
		go sess.close()
		sess.awaitDead(5 * time.Second)
		// The caller is about to delete/transfer session files — a process
		// that outlived the grace would race those destructive writes.
		// Escalate to an immediate group SIGKILL and wait once more (bounded).
		if !sess.isDead() {
			sess.logger.Warn("claude session survived sync close grace; force-killing", "agent", sess.agentID)
			sess.forceKill()
			sess.awaitDead(3 * time.Second)
		}
	}
}

// CloseAllSessions terminates every live persistent process. Used by the
// restart drain so a device switch / restart doesn't strand a process.
func (b *ClaudeBackend) CloseAllSessions() {
	b.sessMu.Lock()
	sessions := make([]*claudeSession, 0, len(b.sessions))
	for id, s := range b.sessions {
		sessions = append(sessions, s)
		delete(b.sessions, id)
	}
	b.sessMu.Unlock()
	for _, s := range sessions {
		s.close()
	}
}

// CloseClaudeSession terminates the agent's persistent claude process, if any.
func (m *Manager) CloseClaudeSession(agentID string) { m.closeClaudeSession(agentID) }

// CloseClaudeSessionSync terminates the agent's persistent claude process and
// waits (bounded) for it to exit. Used by destructive paths that touch the
// session JSONL right after (e.g. the device-switch handoff).
func (m *Manager) CloseClaudeSessionSync(agentID string) { m.closeClaudeSessionSync(agentID) }

// CloseAllClaudeSessions terminates all persistent claude processes.
func (m *Manager) CloseAllClaudeSessions() {
	if cb, ok := m.backends["claude"].(*ClaudeBackend); ok {
		cb.CloseAllSessions()
	}
}

// HasLiveSession reports whether a persistent process currently exists for the
// agent (used by lifecycle idle checks).
func (b *ClaudeBackend) HasLiveSession(agentID string) bool {
	b.sessMu.Lock()
	defer b.sessMu.Unlock()
	_, ok := b.sessions[agentID]
	return ok
}

// SessionInTurn reports whether the agent's persistent session is actively
// processing a turn (solicited or unsolicited). An idle live session is NOT
// busy for quiesce purposes.
func (b *ClaudeBackend) SessionInTurn(agentID string) bool {
	b.sessMu.Lock()
	sess := b.sessions[agentID]
	b.sessMu.Unlock()
	if sess == nil {
		return false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.state == sessInTurn
}

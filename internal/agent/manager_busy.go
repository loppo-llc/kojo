package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// IsBusy returns true if the agent has an active chat (any source).
func (m *Manager) IsBusy(agentID string) bool {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	_, ok := m.busy[agentID]
	return ok
}

// IsBusyForStatus returns true only when the agent is busy with a user
// chat or cron job — automated notifications (group DM replies etc.) are
// excluded so that members don't all appear "busy" when responding to a
// broadcast notification.
func (m *Manager) IsBusyForStatus(agentID string) bool {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	entry, ok := m.busy[agentID]
	if !ok {
		return false
	}
	return entry.source == BusySourceUser || entry.source == BusySourceCron
}

// BusySince returns the time when the agent started its current chat.
// Returns zero time and false if the agent is not busy.
func (m *Manager) BusySince(agentID string) (time.Time, bool) {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	entry, ok := m.busy[agentID]
	if !ok {
		return time.Time{}, false
	}
	return entry.startedAt, true
}

// Subscribe returns a snapshot of all past events and a live channel for an
// agent's ongoing chat. The caller must call unsub when done to free resources.
// If the agent is not busy, busy is false and all other values are zero.
func (m *Manager) Subscribe(agentID string) (startedAt time.Time, past []ChatEvent, live <-chan ChatEvent, unsub func(), busy bool) {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	entry, ok := m.busy[agentID]
	if !ok {
		return time.Time{}, nil, nil, func() {}, false
	}
	if entry.broadcaster == nil {
		return entry.startedAt, nil, nil, func() {}, true
	}
	past, live, unsub = entry.broadcaster.Subscribe()
	return entry.startedAt, past, live, unsub, true
}

// Abort cancels any running chat for an agent — both the user
// chat (busy entry) AND any in-flight one-shot chats (Slack /
// group-DM responders). The §3.7 switch-device
// orchestrator relies on this dual cancel: WaitChatIdle's drain
// waits on both `busy` and `oneShotCancels`, so a bare Abort
// that only signaled the user chat would let one-shots run past
// the quiesce window and write transcript / JSONL after the
// snapshot.
//
// cancelOneShots leaves the oneShotCancels map entry intact so
// waitOneShotClear / WaitChatIdle can observe completion as each
// goroutine removes itself via untrackOneShot. Idempotent — a
// second Abort on a finished chat is a no-op for both halves.
func (m *Manager) Abort(agentID string) {
	// Call the cancel OUTSIDE busyMu: an unsolicited turn's cancel is
	// wrapped with the backend abort (session mutex + stdin write) — running
	// it under the global busy lock would let one wedged session stall every
	// agent in the daemon, and invites lock-order inversions against code
	// that takes the session mutex before busyMu.
	m.busyMu.Lock()
	var cancel context.CancelFunc
	if entry, ok := m.busy[agentID]; ok {
		cancel = entry.cancel
	}
	m.busyMu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.cancelOneShots(agentID)
}

// Steer injects an additional user message into the agent's currently
// running turn (claude/codex backends only — see ChatOptions.OnSteerReady /
// SteerFunc). It returns the mode used: SteerModeInjected when the text merged
// into the running turn, or SteerModeFallbackTurn when the turn had just ended
// and a normal follow-up turn was started with the text instead (so the
// message is never dropped on the turn-just-ended race — the reply arrives as
// a fresh turn). Returns ErrQuiescing during a restart drain (before any
// persistence), ErrSteerUnsupported if the backend cannot steer, and any real
// persist/backend failure otherwise. On the inject path the steered text is
// appended to the transcript as a plain user message; on the fallback path
// Chat persists it exactly once.
//
// A steer that arrives while the turn is still starting — prepareChat
// (memory search, context assembly) runs BEFORE the busy entry exists,
// and OnSteerReady fires slightly after it — waits for the handle
// instead of bouncing 409 not_busy. Without the wait, a message sent in
// that window falls back to a queued normal send and gets reordered
// behind steers sent later in the same turn.
// Steer return modes (the first result value). SteerModeInjected means the
// text was merged into the running turn; SteerModeFallbackTurn means the turn
// had just ended and a normal follow-up turn was started with the text
// instead — the caller/UI should treat it like an ordinary send whose reply
// arrives as a fresh turn over the agent WS.
const (
	SteerModeInjected     = "steer"
	SteerModeFallbackTurn = "fallback_turn"
)

func (m *Manager) Steer(ctx context.Context, agentID, text string) (string, error) {
	// Refuse a steer during a restart drain BEFORE persisting anything
	// (mirrors acquirePreparing). A steer accepted mid-quiesce would reserve a
	// row against a turn about to be aborted and never processed — surface the
	// same ErrQuiescing the Chat path does.
	m.busyMu.Lock()
	quiescing := m.quiescing
	m.busyMu.Unlock()
	if quiescing {
		return "", ErrQuiescing
	}

	// Wait for the steer handle, but do NOT hold the global lock across the
	// wait, the pipe write, or the transcript append: a wedged claude process
	// (stdin buffer full) or a slow disk would otherwise stall every agent in
	// the daemon behind busyMu. ErrAgentNotBusy here means no steerable turn
	// exists (fully idle, an unsolicited background turn, a cross-generation
	// wait, or an aborted prepare) — that's not the fire-and-forget race this
	// fix targets, so propagate it and let the client fall back to a normal
	// send. ErrSteerUnsupported / ctx errors likewise propagate unchanged.
	entry, err := m.awaitSteerHandle(ctx, agentID)
	if err != nil {
		return "", err
	}

	injectErr := m.injectSteer(agentID, entry, text)
	if injectErr == nil {
		return SteerModeInjected, nil
	}
	if !errors.Is(injectErr, ErrAgentNotBusy) {
		// A genuine failure (persist error, backend error). The reserved row
		// was already rolled back by injectSteer.
		return "", injectErr
	}

	// The turn ended between the handle check and the injection — the exact
	// fire-and-forget window this fix closes (claudeTurnSteer.markOver). The
	// row injectSteer reserved was rolled back, so no duplicate exists. Its
	// busy entry lingers until the done event drains, so a fresh Chat would
	// bounce ErrAgentBusy — wait for the slot to clear, then start a normal
	// follow-up turn with the same text (Chat re-persists it exactly once and
	// streams it as a fresh turn the agent WS picks up).
	if werr := m.waitBusyClear(agentID); werr != nil {
		// A genuinely new turn took the slot within the grace window. Report
		// not_busy so the client falls back to a normal send rather than
		// silently landing this text in an unrelated turn.
		return "", ErrAgentNotBusy
	}
	bch, cerr := m.fallbackChat(ctx, agentID, text)
	if cerr != nil {
		// Could not start the follow-up turn (e.g. a restart drain began, or
		// the agent is busy again). Surface the real error — never a silent
		// drop; the message was rolled back and never persisted.
		return "", cerr
	}
	// Discard the caller channel — the turn is delivered to WS subscribers via
	// the broadcaster. Drain so the per-sub forwarder never blocks.
	go func() {
		for range bch {
		}
	}()
	return SteerModeFallbackTurn, nil
}

// fallbackChat starts a normal follow-up turn for the fallback path. Uses the
// overridable seam (set in tests) when present, else the real Chat.
func (m *Manager) fallbackChat(ctx context.Context, agentID, text string) (<-chan ChatEvent, error) {
	if m.steerFallbackChat != nil {
		return m.steerFallbackChat(ctx, agentID, text)
	}
	// Detach from the request context: the fallback turn must survive the
	// HTTP 200 / WS disconnect that cancels ctx (the response is saved to the
	// transcript regardless), exactly like the normal message paths that start
	// Chat with a background context. Without this, returning success to the
	// client would immediately cancel the turn we just started.
	return m.Chat(context.WithoutCancel(ctx), agentID, text, "user", nil)
}

// injectSteer persists the steer row and injects the text into the running
// turn behind entry. Persist-first reserves the row so an injection failure
// can't leave the model having consumed input that never persisted (a retry
// would then double-inject). On any injection error the reserved row is rolled
// back and the error returned (notably ErrAgentNotBusy, which Steer maps to a
// fallback turn). On success it pushes a live "message" event so subscribed
// clients see the steered line inline.
func (m *Manager) injectSteer(agentID string, entry busyEntry, text string) error {
	msg := newUserMessage(text, nil)
	if appendErr := appendMessage(agentID, msg); appendErr != nil {
		m.logger.Warn("failed to save steer message", "agent", agentID, "err", appendErr)
		return fmt.Errorf("steer accepted but not persisted: %w", appendErr)
	}
	if err := entry.steer(text); err != nil {
		if delErr := deleteMessage(agentID, msg.ID, ""); delErr != nil &&
			!errors.Is(delErr, ErrMessageNotFound) {
			m.logger.Warn("failed to roll back steer message after injection failure",
				"agent", agentID, "err", delErr)
		}
		return err
	}
	// Re-acquire busyMu for the live-event push. clearBusy removes the
	// entry under busyMu BEFORE close(outCh), so an entry re-observed here
	// is guaranteed to have an open channel while we hold the lock —
	// re-checking (rather than reusing the earlier snapshot) is what makes
	// the send panic-safe.
	m.busyMu.Lock()
	if cur, ok := m.busy[agentID]; ok && cur.outCh != nil && cur.outCh == entry.outCh {
		select {
		case cur.outCh <- ChatEvent{Type: "message", Message: msg}:
		default:
		}
	}
	m.busyMu.Unlock()
	return nil
}

// AnswerQuestion resolves a pending interactive AskUserQuestion raised by the
// agent's running turn (claude backend only — see ChatOptions.OnQuestionReady).
// On allow, answers maps each question string to the chosen answer; on deny the
// tool call is refused with denyMessage. Returns ErrAgentNotBusy when no turn is
// running (or the backend can't answer) and ErrQuestionNotFound when requestID
// doesn't match a pending question. On a successful allow it appends a readable
// user message recording the answers to the transcript (mirroring Steer) so the
// Q&A survives a reload.
func (m *Manager) AnswerQuestion(ctx context.Context, agentID, requestID string, answers map[string]any, deny bool, denyMessage string) error {
	m.busyMu.Lock()
	entry, ok := m.busy[agentID]
	m.busyMu.Unlock()
	if !ok || entry.answer == nil {
		return ErrAgentNotBusy
	}
	// Deny resolves the control_request without persisting anything — nothing
	// to reserve or roll back.
	if deny {
		return entry.answer(requestID, answers, deny, denyMessage)
	}
	// Allow: persist the answer record FIRST (reserve the row), THEN deliver it
	// to the live turn. Persist-first mirrors Steer — an answer delivered before
	// persistence that then 500s could be re-delivered on retry (duplicate
	// injection into the turn). On delivery failure we roll the reserved row
	// back and propagate the error (ErrAgentNotBusy / ErrQuestionNotFound).
	msg := newUserMessage(formatAnswers(answers), nil)
	if appendErr := appendMessage(agentID, msg); appendErr != nil {
		m.logger.Warn("failed to save answer message", "agent", agentID, "err", appendErr)
		return fmt.Errorf("answer accepted but not persisted: %w", appendErr)
	}
	if err := entry.answer(requestID, answers, deny, denyMessage); err != nil {
		if delErr := deleteMessage(agentID, msg.ID, ""); delErr != nil &&
			!errors.Is(delErr, ErrMessageNotFound) {
			m.logger.Warn("failed to roll back answer message after delivery failure",
				"agent", agentID, "err", delErr)
		}
		return err
	}
	m.busyMu.Lock()
	if cur, ok := m.busy[agentID]; ok && cur.outCh != nil && cur.outCh == entry.outCh {
		select {
		case cur.outCh <- ChatEvent{Type: "message", Message: msg}:
		default:
		}
	}
	m.busyMu.Unlock()
	return nil
}

// markQuestionRaised records requestID as pending for agentID and fires
// OnQuestionRaised exactly once for THIS requestID — re-observing a
// requestID that is already tracked as pending (e.g. a duplicate
// user_question event) doesn't re-notify, but a second distinct question
// raised while an earlier one is still outstanding does, since each is its
// own raise the user hasn't been told about yet. The callback runs in its
// own goroutine (mirroring how OnChatDone is invoked) so a slow web-push
// send can't delay forwarding the user_question ChatEvent to the WS client.
func (m *Manager) markQuestionRaised(agentID, requestID string) {
	m.busyMu.Lock()
	if m.pendingQuestions == nil {
		m.pendingQuestions = make(map[string]map[string]struct{})
	}
	set, ok := m.pendingQuestions[agentID]
	if !ok {
		set = make(map[string]struct{})
		m.pendingQuestions[agentID] = set
	}
	_, alreadyPending := set[requestID]
	set[requestID] = struct{}{}
	m.busyMu.Unlock()
	if !alreadyPending && m.OnQuestionRaised != nil {
		go m.OnQuestionRaised(agentID)
	}
}

// clearQuestion removes requestID from agentID's pending set. Wired as
// ChatOptions.OnQuestionResolved so it fires whenever a question is
// answered, denied, or auto-denied on timeout — regardless of which path
// resolved it. Idempotent: safe to call for an already-cleared requestID
// (e.g. Manager.AnswerQuestion and a racing backend timeout both resolving
// the same question) or an agent with no tracked questions at all.
func (m *Manager) clearQuestion(agentID, requestID string) {
	m.busyMu.Lock()
	if set, ok := m.pendingQuestions[agentID]; ok {
		delete(set, requestID)
		if len(set) == 0 {
			delete(m.pendingQuestions, agentID)
		}
	}
	m.busyMu.Unlock()
}

// clearAllQuestionsForAgent drops every pending question tracked for
// agentID. Safety-net cleanup: processChatEvents defers this on every exit
// path (done, error, abort, or the backend process dying outright) so a
// question that never received an explicit resolution can never leave
// AwaitingAnswer stuck on past the turn that raised it.
func (m *Manager) clearAllQuestionsForAgent(agentID string) {
	m.busyMu.Lock()
	delete(m.pendingQuestions, agentID)
	m.busyMu.Unlock()
}

// HasPendingQuestion reports whether agentID currently has an unanswered
// AskUserQuestion prompt outstanding. Folded into Agent.AwaitingAnswer by
// Manager.List.
func (m *Manager) HasPendingQuestion(agentID string) bool {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	return len(m.pendingQuestions[agentID]) > 0
}

// formatAnswers renders an answers map as a readable transcript line, e.g.
// "answered: 色選択 → 青". Keys are sorted for deterministic output.
func formatAnswers(answers map[string]any) string {
	keys := make([]string, 0, len(answers))
	for k := range answers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s → %v", k, answers[k]))
	}
	return "answered: " + strings.Join(parts, ", ")
}

// steerHandleWait bounds how long Steer waits for a starting turn to
// register its steer handle. prepareChat (memory search, volatile
// context) normally finishes well inside this; the bound only guards
// against a prepare that wedges or a backend that never registers.
const steerHandleWait = 30 * time.Second

// backendSupportsSteer reports whether the tool's backend ever registers
// a steer handle (ChatOptions.OnSteerReady).
func backendSupportsSteer(tool string) bool {
	return tool == "claude" || tool == "codex"
}

// awaitSteerHandle returns the current turn's busy entry once its steer
// handle is registered. If no turn is running or preparing, it returns
// ErrAgentNotBusy immediately. If a turn is running or starting on a
// backend that can never steer, it returns ErrSteerUnsupported
// immediately. Otherwise (turn preparing, or busy entry present but
// OnSteerReady not yet fired) it polls until the handle appears, the
// turn disappears, ctx is cancelled, or steerHandleWait elapses.
//
// The wait is pinned to the FIRST busy entry it observes (by outCh
// identity): if that turn ends and a different one appears while we
// poll, the steer was aimed at a turn that no longer exists and the
// caller gets ErrAgentNotBusy (→ normal-send fallback) instead of the
// text silently landing in the wrong turn. A prepare that aborts and is
// replaced by another prepare inside one poll interval is not
// distinguishable (prepares carry no identity); steering the successor
// turn there matches the pre-wait fallback ordering anyway.
func (m *Manager) awaitSteerHandle(ctx context.Context, agentID string) (busyEntry, error) {
	var supportChecked, supported bool
	var pinnedCh chan<- ChatEvent
	deadline := time.Now().Add(steerHandleWait)
	for {
		m.busyMu.Lock()
		entry, busyOK := m.busy[agentID]
		preparing := m.preparing[agentID] > 0
		m.busyMu.Unlock()
		if busyOK {
			// A background notification turn (a subagent from an EARLIER
			// turn finishing, surfaced by the persistent claude session)
			// holds the busy slot and streams to the UI, but is not a
			// steerable user turn — it never registers a steer handle.
			// Fail-fast with ErrAgentNotBusy so the caller falls back to a
			// queued normal send (a fresh turn once the notification turn
			// ends) instead of polling steerHandleWait and then bouncing
			// ErrSteerUnsupported, which the web client surfaces as a
			// rollback that silently drops the user's message.
			if entry.unsolicited {
				return busyEntry{}, ErrAgentNotBusy
			}
			if pinnedCh == nil {
				pinnedCh = entry.outCh
			} else if entry.outCh != pinnedCh {
				return busyEntry{}, ErrAgentNotBusy
			}
			if entry.steer != nil {
				return entry, nil
			}
		} else if pinnedCh != nil {
			// The turn we were waiting on ended without ever
			// registering a handle.
			return busyEntry{}, ErrAgentNotBusy
		}
		if !busyOK && !preparing {
			return busyEntry{}, ErrAgentNotBusy
		}
		// A turn is starting (preparing) or running without a handle
		// yet. Waiting is only useful when the backend will eventually
		// register one; look the tool up once, lazily.
		if !supportChecked {
			supportChecked = true
			if ag, ok := m.Get(agentID); ok {
				supported = backendSupportsSteer(ag.Tool)
			}
		}
		if !supported {
			return busyEntry{}, ErrSteerUnsupported
		}
		if time.Now().After(deadline) {
			return busyEntry{}, ErrSteerUnsupported
		}
		select {
		case <-ctx.Done():
			return busyEntry{}, ctx.Err()
		case <-time.After(75 * time.Millisecond):
		}
	}
}

// WaitChatIdle polls busyMu until every concurrent write path
// has drained for the agent OR ctx is cancelled. Returns nil on
// idle, ctx.Err() on timeout. Caller is responsible for issuing
// Abort first (WaitChatIdle just observes flags; without an
// abort it would block until the chat finishes naturally).
//
// Drains:
//   - busy:       in-flight Chat / ChatOneShot
//   - preparing:  a Chat between prepareChat entry and busy
//     entry insert (disk side effects still landing)
//   - editing:    Regenerate / transcript edit holding the
//     acquireTranscriptEdit guard
//   - resetting:  ResetData / Fork / Archive / ResetSession
//     holding the acquireResetGuard
//   - mutating:   non-chat state writers (persona / settings /
//     task / credential / avatar / slack tokens)
//     holding AcquireMutation
//   - oneShotCancels: Slack / group-DM one-shot chats
//     cancelled by switch_device_handler's Abort
//
// Without all six checks the §3.7 quiesce window would race a
// Slack / cron / persona-edit write that landed mid-handoff.
//
// Used by the §3.7 device-switch orchestrator: after Abort the
// chat goroutine still needs a few hundred ms to flush its final
// claude session JSONL append before we snapshot the file. The
// 1.5 s caller default is generous; in practice typical aborts
// drain in well under 100 ms.
func (m *Manager) WaitChatIdle(ctx context.Context, agentID string) error {
	return m.waitChatIdle(ctx, agentID, false)
}

// WaitChatIdleSelfCall is the §3.7 device-switch variant used
// when the HTTP request is the agent's own chat tool — typically
// the kojo-switch-device skill's curl. That curl is driven by
// the busy entry it would otherwise wait on, so the busy check
// would deadlock until the orchestration context timed out.
// Skipping busy lets every OTHER concurrent writer (preparing,
// Slack one-shots, editing, resetting, mutating,
// profileGen) still drain before the snapshot.
//
// preparing is intentionally NOT skipped: prepareChat exits
// before busy is set, so the self chat itself no longer counts;
// a non-zero preparing counter means a DIFFERENT chat is in
// prepareChat and must be drained.
//
// Pair with CancelOneShotsForAgent (not Abort) on entry so we
// don't cancel the busy entry making the call.
func (m *Manager) WaitChatIdleSelfCall(ctx context.Context, agentID string) error {
	return m.waitChatIdle(ctx, agentID, true)
}

func (m *Manager) waitChatIdle(ctx context.Context, agentID string, skipBusy bool) error {
	for {
		m.busyMu.Lock()
		_, busyOK := m.busy[agentID]
		preparing := m.preparing[agentID] > 0
		editingOK := m.editing[agentID]
		resettingOK := m.resetting[agentID]
		mutating := m.mutating[agentID] > 0
		notifying := m.notifying[agentID] > 0
		m.busyMu.Unlock()
		m.oneShotCancelsMu.Lock()
		oneShotN := len(m.oneShotCancels[agentID])
		m.oneShotCancelsMu.Unlock()
		// profileGen tracks in-flight regeneratePublicProfile
		// goroutines. The entry-gate refuses new regens during
		// switching, but a regen that started BEFORE SetSwitching
		// can still be mid-LLM-roundtrip and would write
		// PublicProfile after the snapshot if we don't wait.
		// LLM round-trips can exceed the 3s quiesce window; in
		// that case the orchestrator times out → 409 fail-closed.
		m.mu.Lock()
		profileGen := m.profileGen[agentID]
		m.mu.Unlock()
		if skipBusy {
			busyOK = false
		}
		if !busyOK && !preparing && !editingOK && !resettingOK && !mutating && !notifying && oneShotN == 0 && !profileGen {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// SetQuiescing flips the daemon-wide chat gate. When on,
// acquirePreparing refuses every new Chat / ChatOneShot with
// ErrAgentBusy ("server restart in progress") for ALL agents.
// In-flight turns are unaffected — the restart path waits for them
// via WaitAllChatsIdle. Idempotent; guard-off on a non-quiescing
// manager is a no-op.
func (m *Manager) SetQuiescing(on bool) {
	m.busyMu.Lock()
	m.quiescing = on
	m.busyMu.Unlock()
}

// blockerAge renders a coarse human-readable age for a drain blocker
// ("32m", "8s"). Minute resolution above a minute, second resolution
// below, so DrainBlockers strings stay short and stable.
func blockerAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if mins := int(d.Minutes()); mins > 0 {
		return fmt.Sprintf("%dm", mins)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// DrainBlockers returns a sorted, human-readable list of everything
// that currently prevents the daemon from reaching the fully-idle
// state WaitAllChatsIdle waits for — one entry per blocking item with
// its kind and agentID. Empty slice means idle. Used by the restart
// drain logging, the timeout error, and the GET /api/v1/system/restart
// status endpoint so a stuck drain is diagnosable instead of silent.
//
// Format examples:
//
//	busy:ag_x(source=notification,age=4m)
//	preparing:ag_y(n=2)
//	editing:ag_y
//	resetting:ag_y
//	mutating:ag_y(n=1)
//	switching:ag_y
//	notifying:ag_y(n=1)
//	summarizing(n=1)
//	oneShot:ag_z(id=7,key=groupdm:g_1,age=32m)
//	profileGen:ag_w
func (m *Manager) DrainBlockers() []string {
	if m == nil {
		return nil
	}
	now := time.Now()
	var out []string

	m.busyMu.Lock()
	for id, e := range m.busy {
		out = append(out, fmt.Sprintf("busy:%s(source=%s,age=%s)", id, e.source, blockerAge(now.Sub(e.startedAt))))
	}
	for id, n := range m.preparing {
		out = append(out, fmt.Sprintf("preparing:%s(n=%d)", id, n))
	}
	for id := range m.editing {
		out = append(out, "editing:"+id)
	}
	for id := range m.resetting {
		out = append(out, "resetting:"+id)
	}
	for id, n := range m.mutating {
		out = append(out, fmt.Sprintf("mutating:%s(n=%d)", id, n))
	}
	for id := range m.switching {
		out = append(out, "switching:"+id)
	}
	for id, n := range m.notifying {
		out = append(out, fmt.Sprintf("notifying:%s(n=%d)", id, n))
	}
	if m.summarizing > 0 {
		out = append(out, fmt.Sprintf("summarizing(n=%d)", m.summarizing))
	}
	m.busyMu.Unlock()

	m.oneShotCancelsMu.Lock()
	for agentID, set := range m.oneShotCancels {
		for id := range set {
			parts := fmt.Sprintf("id=%d", id)
			if key := m.oneShotSessions[agentID][id]; key != "" {
				parts += ",key=" + key
			}
			if t, ok := m.oneShotArmed[agentID][id]; ok {
				parts += ",age=" + blockerAge(now.Sub(t))
			}
			out = append(out, fmt.Sprintf("oneShot:%s(%s)", agentID, parts))
		}
	}
	m.oneShotCancelsMu.Unlock()

	m.mu.Lock()
	for id, on := range m.profileGen {
		if on {
			out = append(out, "profileGen:"+id)
		}
	}
	m.mu.Unlock()

	sort.Strings(out)
	return out
}

// WaitAllChatsIdle polls until every concurrent write path across
// ALL agents has drained, or ctx is cancelled. The daemon-wide
// analogue of WaitChatIdle — same six checks, plus `switching`
// (a §3.7 device switch must not be cut in half by a re-exec) and
// the post-turn summarize counter (turnSummarizeAsync runs after
// busy clears, and killing it mid-write would corrupt recent.md /
// the cursor file).
//
// Callers should SetQuiescing(true) first; otherwise a fresh turn
// can start between the idle observation and whatever the caller
// does next (the restart path re-execs the process).
func (m *Manager) WaitAllChatsIdle(ctx context.Context) error {
	lastLog := time.Now()
	for {
		m.busyMu.Lock()
		idle := len(m.busy) == 0 && len(m.preparing) == 0 &&
			len(m.editing) == 0 && len(m.resetting) == 0 &&
			len(m.mutating) == 0 && len(m.switching) == 0 &&
			len(m.notifying) == 0 &&
			m.summarizing == 0
		m.busyMu.Unlock()
		if idle {
			m.oneShotCancelsMu.Lock()
			for _, set := range m.oneShotCancels {
				if len(set) > 0 {
					idle = false
					break
				}
			}
			m.oneShotCancelsMu.Unlock()
		}
		if idle {
			m.mu.Lock()
			for _, on := range m.profileGen {
				if on {
					idle = false
					break
				}
			}
			m.mu.Unlock()
		}
		if idle {
			return nil
		}
		// Surface WHAT is still blocking the drain every 30s. Two 15-min
		// restarts aborted silently because WaitAllChatsIdle reported
		// nothing while some counter/map had leaked; the blocker list
		// makes the culprit visible in the logs long before the timeout.
		if m.logger != nil && time.Since(lastLog) >= 30*time.Second {
			m.logger.Warn("restart drain still waiting for chats to idle",
				"blockers", strings.Join(m.DrainBlockers(), ", "))
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			// Wrap ctx.Err() with the final blocker list so the aborted
			// restart is diagnosable from the returned error alone.
			return fmt.Errorf("restart drain timed out; still blocked by [%s]: %w",
				strings.Join(m.DrainBlockers(), ", "), ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// CancelOneShotsForAgent cancels every in-flight one-shot chat
// (Slack / group-DM responder) for the agent
// WITHOUT touching the busy entry. The §3.7 device-switch
// orchestrator calls this on the agent-self-call path where
// Abort()'s busy-cancel would kill the curl that initiated the
// switch. Pairs with WaitChatIdleSelfCall.
func (m *Manager) CancelOneShotsForAgent(agentID string) {
	m.cancelOneShots(agentID)
}

// acquirePreparing marks the agent as inside prepareChat. Returns
// ErrAgentBusy when switching is set so callers refuse the chat
// before any disk side effect. Pairs with releasePreparing in
// defer / chat exit.
func (m *Manager) acquirePreparing(agentID string) error {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	if m.quiescing {
		return ErrQuiescing
	}
	if m.switching != nil && m.switching[agentID] {
		return fmt.Errorf("%w: device switch in progress", ErrAgentBusy)
	}
	// Lazy-init: not all test fixtures use NewManager, so the
	// map may be nil here. Guard so an unrelated test that
	// drives Chat through a hand-rolled Manager doesn't panic.
	if m.preparing == nil {
		m.preparing = make(map[string]int)
	}
	m.preparing[agentID]++
	return nil
}

// releasePreparing decrements the preparing counter for the
// agent. No-op when the counter is already zero (defensive
// against a double-release) or the map was never initialised
// (test fixtures that hand-roll Manager).
func (m *Manager) releasePreparing(agentID string) {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	if m.preparing == nil {
		return
	}
	if m.preparing[agentID] > 0 {
		m.preparing[agentID]--
		if m.preparing[agentID] == 0 {
			delete(m.preparing, agentID)
		}
	}
}

func (m *Manager) clearBusy(agentID string) {
	m.busyMu.Lock()
	delete(m.busy, agentID)
	m.busyMu.Unlock()
}

// waitBusyClear waits up to 5 seconds for the agent's busy entry to be removed.
// Returns ErrAgentBusy if the agent is still busy after the timeout.
func (m *Manager) waitBusyClear(agentID string) error {
	for i := 0; i < 50; i++ {
		m.busyMu.Lock()
		_, busy := m.busy[agentID]
		m.busyMu.Unlock()
		if !busy {
			return nil
		}
		if i == 49 {
			return fmt.Errorf("%w, try again later", ErrAgentBusy)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// waitOneShotClear waits up to 5 seconds for the agent's in-flight one-shot
// chats (Slack, Discord, Group DM) to drain. Call cancelOneShots first so the
// goroutines are actively winding down. Returns ErrAgentBusy if not drained.
func (m *Manager) waitOneShotClear(agentID string) error {
	for i := 0; i < 50; i++ {
		m.oneShotCancelsMu.Lock()
		n := len(m.oneShotCancels[agentID])
		m.oneShotCancelsMu.Unlock()
		if n == 0 {
			return nil
		}
		if i == 49 {
			return fmt.Errorf("%w, try again later", ErrAgentBusy)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// SetSwitching marks the agent as mid-§3.7-switch (true) or
// clears the marker (false). When set, IsSwitching returns true
// and the chat path refuses new starts with ErrAgentSwitching so
// no transcript / JSONL is written between Step -1's quiesce and
// the post-complete drain. Idempotent: setting true on an
// already-switching agent is a no-op; clearing an agent that
// wasn't switching is also a no-op.
//
// Returns ErrAgentBusy when on=true and a restart drain is
// quiescing the daemon — the quiesce check and the switching set
// share busyMu, so a switch can never start after WaitAllChatsIdle
// observed idle. Clearing (on=false) always succeeds so the
// orchestrator's deferred cleanup can't be refused.
func (m *Manager) SetSwitching(agentID string, on bool) error {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	if on && m.quiescing {
		return ErrQuiescing
	}
	if m.switching == nil {
		m.switching = make(map[string]bool)
	}
	if on {
		m.switching[agentID] = true
	} else {
		delete(m.switching, agentID)
	}
	return nil
}

// IsSwitching returns true when SetSwitching(agentID, true) is in
// effect and a §3.7 device-switch is mid-flight.
func (m *Manager) IsSwitching(agentID string) bool {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	return m.switching[agentID]
}

// AcquireMutation reserves a slot in the per-agent mutation
// counter and returns a release callback. The acquire fails
// when a §3.7 device switch is mid-flight on this peer. While
// the slot is held, WaitChatIdle observes the agent as
// non-idle — so Step -1's snapshot cannot race a mutation
// that started just before SetSwitching landed.
//
// Common entry guard for every agent-state mutation surface
// that does NOT route through prepareChat (persona / settings
// / slackbot / tasks / credentials / avatar / slack tokens).
// The release callback is idempotent and safe to defer.
//
// Threadsafe; nil-safe for hand-rolled test fixtures.
func (m *Manager) AcquireMutation(agentID string) (func(), error) {
	if m == nil {
		return func() {}, nil
	}
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	if m.quiescing {
		return nil, ErrQuiescing
	}
	if m.switching != nil && m.switching[agentID] {
		return nil, fmt.Errorf("%w: device switch in progress", ErrAgentBusy)
	}
	if m.mutating == nil {
		m.mutating = make(map[string]int)
	}
	m.mutating[agentID]++
	released := false
	return func() {
		m.busyMu.Lock()
		defer m.busyMu.Unlock()
		if released {
			return
		}
		released = true
		if m.mutating == nil {
			return
		}
		if m.mutating[agentID] > 0 {
			m.mutating[agentID]--
		}
		if m.mutating[agentID] == 0 {
			delete(m.mutating, agentID)
		}
	}, nil
}

// acquireResetGuard marks the agent as resetting, cancels any active chat,
// and returns a cleanup function that removes the resetting flag.
// Returns ErrAgentBusy if the agent is already being reset.
func (m *Manager) acquireResetGuard(agentID string) (func(), error) {
	m.busyMu.Lock()
	if m.quiescing {
		m.busyMu.Unlock()
		return nil, ErrQuiescing
	}
	if m.resetting[agentID] {
		m.busyMu.Unlock()
		return nil, fmt.Errorf("%w, try again later", ErrAgentBusy)
	}
	if m.editing[agentID] {
		m.busyMu.Unlock()
		return nil, fmt.Errorf("%w, try again later", ErrAgentBusy)
	}
	if m.switching != nil && m.switching[agentID] {
		// §3.7 device switch is mid-flight: reset / fork
		// would re-emit MEMORY.md and session JSONLs after
		// the snapshot, stranding them on source.
		m.busyMu.Unlock()
		return nil, fmt.Errorf("%w: device switch in progress", ErrAgentBusy)
	}
	m.resetting[agentID] = true
	if entry, busy := m.busy[agentID]; busy {
		entry.cancel()
	}
	m.busyMu.Unlock()

	cleanup := func() {
		m.busyMu.Lock()
		delete(m.resetting, agentID)
		m.busyMu.Unlock()
	}
	return cleanup, nil
}

// WaitChatDone blocks until the busy entry's chat goroutine emits a
// terminal `done` event for the agent's in-flight turn or ctx is
// cancelled. Returns the captured assistant Message converted to a
// store.MessageRecord (with seq=0; caller stamps the target-side
// allocation, ToolUses cleared — see below) or nil on ctx
// cancellation / missing busy entry / nil accumulator.
//
// ToolUses is intentionally CLEARED on the returned record. The
// done event carries the WHOLE turn — the agent's pre-tool-call
// text, every tool_use (including the kojo-switch-device call
// that triggered this whole flow), and every tool_result. The
// snapshot row the orchestrator shipped during /agent-sync ALREADY
// carries that tool_use; if the tail row carried it too, target's
// userMessageAddressed / planATailContent would mis-classify the
// tail as another snapshot (substring-detect of `kojo-switch-device`
// in ToolUses) and the commitment text would not surface in the
// arrival prompt. Stripping ToolUses leaves the tail as a pure
// "post-tool-result completion text" row, distinct in shape from
// the snapshot.
//
// Used by the device-switch self-call orchestrator's deferred
// finalize: after /handoff/complete moves the lock to target, the
// source's claude turn continues for a few hundred milliseconds to
// emit a post-tool-result "I'll do X on arrival" message. Without
// this wait the orchestrator would race ahead, ship /handoff/finalize
// to target empty-handed, and target's arrival prompt would fire
// against a transcript missing the agent's own commitment text. The
// caller passes a bounded ctx so a wedged source backend can't
// indefinitely defer target activation; on timeout the orchestrator
// ships finalize WITHOUT the tail (current behavior — degraded but
// not stuck).
//
// Snapshots the accumulator pointer under busyMu so a concurrent
// clearBusy(agentID) that removes the entry doesn't strand the
// caller on a dead reference. The accumulator outlives clearBusy
// (closed channel + capture stays live until the wait returns).
func (m *Manager) WaitChatDone(ctx context.Context, agentID string) *store.MessageRecord {
	if m == nil {
		return nil
	}
	m.busyMu.Lock()
	entry, ok := m.busy[agentID]
	m.busyMu.Unlock()
	if !ok || entry.accumulator == nil {
		return nil
	}
	doneMsg := entry.accumulator.WaitDone(ctx)
	if doneMsg == nil {
		return nil
	}
	rec, err := messageToRecord(agentID, doneMsg)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("WaitChatDone: messageToRecord failed", "agent", agentID, "err", err)
		}
		return nil
	}
	// Strip ToolUses so the tail's wire shape stays distinct from
	// the snapshot's (which carries kojo-switch-device in its
	// ToolUses). See doc-comment above.
	rec.ToolUses = nil
	ts := parseAgentRFC3339Millis(doneMsg.Timestamp)
	if ts == 0 {
		ts = store.NowMillis()
	}
	rec.CreatedAt = ts
	rec.UpdatedAt = ts
	return rec
}

// SnapshotAccumulatedMessageRecord reconstructs the in-flight
// assistant message from the busy entry's shared accumulator and
// returns it as a store.MessageRecord ready for inclusion in the
// §3.7 agent-sync payload.
//
// Used by the device-switch self-call path: the assistant turn
// containing the kojo-switch-device tool_use is still mid-flight
// (accumulated in processChatEvents' local variables, not yet
// persisted to the messages table). Without this snapshot the sync
// payload would miss the last assistant turn entirely, and the §3.7
// release guard would prevent the processChatEvents defer from ever
// persisting it.
//
// Reads the shared chatAccumulator (NOT the chat broadcaster's
// log) because outCh's non-blocking send drops non-terminal events
// under back-pressure — so the broadcaster's log silently misses
// streaming text / tool_use rows on long claude turns and the
// snapshot would migrate a partial message. The accumulator is fed
// inline by processChatEvents on every event, regardless of outCh
// pressure, so a Snapshot read here matches the chat goroutine's
// own view of the turn.
//
// Falls back to the broadcaster's log when the busy entry has no
// accumulator (legacy / hand-rolled test fixtures); the legacy path
// retains the original behavior so existing tests don't regress.
//
// Returns nil if the agent is not busy or no streaming data has
// accumulated. The caller appends the returned record to the sync
// payload WITHOUT persisting it to the source's DB — on abort the
// chat continues normally and the done event persists the full
// message; on success the source is released and persistence is moot.
func (m *Manager) SnapshotAccumulatedMessageRecord(agentID string) *store.MessageRecord {
	m.busyMu.Lock()
	entry, ok := m.busy[agentID]
	m.busyMu.Unlock()
	if !ok {
		return nil
	}

	var text, thinking string
	var toolUses []ToolUse
	if entry.accumulator != nil {
		text, thinking, toolUses = entry.accumulator.Snapshot()
	} else if entry.broadcaster != nil {
		// Fallback for legacy busy entries that predate the
		// accumulator. Same shape as the original implementation —
		// reads the broadcaster's past log and folds it into a
		// MessageRecord. Drops events if outCh ever back-pressured;
		// acceptable here because the only callers without an
		// accumulator are test fixtures whose chats run too short
		// to back-pressure outCh.
		past, _, unsub := entry.broadcaster.Subscribe()
		unsub()
		var tBuf, thBuf strings.Builder
		for _, ev := range past {
			if ev.ParentToolUseID != "" {
				// Subagent event — already folded into its parent
				// Task ToolUse's Children by the backend.
				continue
			}
			switch ev.Type {
			case "text":
				tBuf.WriteString(ev.Delta)
			case "thinking":
				thBuf.WriteString(ev.Delta)
			case "tool_use":
				toolUses = append(toolUses, ToolUse{
					ID:    ev.ToolUseID,
					Name:  ev.ToolName,
					Input: ev.ToolInput,
				})
			case "tool_result":
				matchToolOutput(toolUses, ev.ToolUseID, ev.ToolName, ev.ToolOutput)
			}
		}
		text = tBuf.String()
		thinking = thBuf.String()
	} else {
		return nil
	}

	if text == "" && thinking == "" && len(toolUses) == 0 {
		return nil
	}
	msg := newAssistantMessage()
	msg.Content = text
	msg.Thinking = thinking
	msg.ToolUses = toolUses
	rec, err := messageToRecord(agentID, msg)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("SnapshotAccumulatedMessageRecord: conversion failed", "agent", agentID, "err", err)
		}
		return nil
	}
	ts := parseAgentRFC3339Millis(msg.Timestamp)
	if ts == 0 {
		ts = store.NowMillis()
	}
	rec.CreatedAt = ts
	rec.UpdatedAt = ts
	return rec
}

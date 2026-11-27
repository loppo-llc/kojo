package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// Agent-driven model switch. POST /api/v1/agents/{id}/model-switch stores
// the new model right away: a turn already running keeps the process it was
// spawned with, so the change first takes effect on the next turn. When the
// calling turn ends, kojo starts that next turn by itself in the same
// conversation, the way a device switch resumes on arrival.
//
// A failed first turn on the new model reverts the switch: the agent has no
// other way out of a model it cannot run.
const (
	modelSwitchKVNamespace   = "model_switch"
	modelSwitchWakeWindow    = 2 * time.Minute
	modelSwitchRetryInterval = 250 * time.Millisecond
	modelSwitchActivateLimit = 15 * time.Second
	// modelSwitchBindLimit is how long a requested turn keeps its restart
	// marker while waiting to reach a backend.
	modelSwitchBindLimit = 10 * time.Minute
	// keyedModelSwitchReason closes an idle keyed process still on the old model.
	keyedModelSwitchReason = "モデル切替"
	modelSwitchReasonLimit = 500

	// ModelSwitchNoteLimit caps the note carried into the next turn.
	ModelSwitchNoteLimit = 4000

	ModelSwitchQueued    = "queued"
	ModelSwitchUnchanged = "unchanged"
)

var (
	ErrModelSwitchUnknownTool     = errors.New("unknown tool")
	ErrModelSwitchUnknownModel    = errors.New("model is not available on this tool")
	ErrModelSwitchInvalidEffort   = errors.New("effort is not supported by this model")
	ErrModelSwitchCallerInactive  = errors.New("the calling conversation has no turn in flight")
	ErrModelSwitchBackgroundTasks = errors.New("background tasks are running; switching would kill them")
	ErrModelSwitchGoalRunning     = errors.New("a native goal is running in this conversation")
	ErrModelSwitchPending         = errors.New("another conversation already has a model switch pending")

	// errModelSwitchSuperseded: the model changed between read and write.
	errModelSwitchSuperseded = fmt.Errorf("%w: the agent's model changed concurrently", ErrAgentBusy)
)

// ModelSwitchTarget is one model configuration.
type ModelSwitchTarget struct {
	Tool   string `json:"tool"`
	Model  string `json:"model"`
	Effort string `json:"effort,omitempty"`
}

func (t ModelSwitchTarget) String() string {
	if t.Effort == "" {
		return t.Tool + "/" + t.Model
	}
	return t.Tool + "/" + t.Model + " (effort " + t.Effort + ")"
}

// ModelSwitchRequest asks for a switch. A nil Tool keeps the backend; a nil
// Effort keeps the current effort when the new model supports it and falls
// back to the model default otherwise. SessionKey names the calling thread
// (empty for the main conversation).
type ModelSwitchRequest struct {
	Model      string
	Tool       *string
	Effort     *string
	Note       string
	SessionKey string
}

// ModelSwitchResult reports what SwitchModel did.
type ModelSwitchResult struct {
	Status        string            `json:"status"`
	From          ModelSwitchTarget `json:"from"`
	To            ModelSwitchTarget `json:"to"`
	EffortCleared bool              `json:"effortCleared,omitempty"`
}

type modelSwitchPhase int

const (
	// modelSwitchAwaitingSource: the calling turn is still running.
	modelSwitchAwaitingSource modelSwitchPhase = iota
	// modelSwitchVerifying: the next turn was requested; its outcome decides
	// whether the switch stands.
	modelSwitchVerifying
)

// modelSwitchStop is what the way the calling turn ended means for the wake.
type modelSwitchStop int

const (
	// modelSwitchProceed: the turn finished (or timed out): start the next.
	modelSwitchProceed modelSwitchStop = iota
	// modelSwitchDrop: someone stopped the agent (user stop, reset, archive,
	// device switch...): the model stays switched but no turn starts.
	modelSwitchDrop
	// modelSwitchDefer: kojo is going down: the marker wakes the next boot.
	modelSwitchDefer
)

// modelSwitchStopOf classifies a finished turn's context.
func modelSwitchStopOf(ctx context.Context) modelSwitchStop {
	cause := context.Cause(ctx)
	var sc *StopCause
	switch {
	case errors.As(cause, &sc):
		switch {
		case sc.ByUser, sc.Silent:
			return modelSwitchDrop
		case sc.Reason == stopReasonShutdown:
			return modelSwitchDefer
		case sc.Reason == stopReasonTimeout:
			return modelSwitchProceed
		}
		return modelSwitchDrop
	case errors.Is(cause, ErrStopNotedByCaller):
		return modelSwitchDrop
	}
	return modelSwitchProceed
}

// pendingModelSwitch is guarded by Manager.modelSwitchMu.
type pendingModelSwitch struct {
	agentID    string
	sessionKey string
	from, to   ModelSwitchTarget
	note       string
	phase      modelSwitchPhase

	// oneShotID identifies the calling thread turn (0 for the main chat).
	oneShotID   int64
	sourceDone  chan struct{}
	sourceEnded bool
	stop        modelSwitchStop
	// continued: the conversation's response adapter accepted the next turn
	// into the slot it reserved behind the calling turn.
	continued bool
	// verifyKey is the conversation the next turn runs in. It differs from
	// sessionKey when a thread cannot be re-driven and main takes the turn.
	verifyKey string
	// capability is the calling thread's adapter continuation, if any.
	capability string
	// token is embedded in the next turn's prompt. The turn carrying it is the
	// one whose outcome decides the switch; any other turn says nothing.
	token       string
	bound       bool
	boundTurnID int64
	// failure is set on a revert that has to wait for the next boot.
	failure string
}

type modelSwitchMarker struct {
	AgentID    string            `json:"agentId"`
	SessionKey string            `json:"sessionKey,omitempty"`
	From       ModelSwitchTarget `json:"from"`
	To         ModelSwitchTarget `json:"to"`
	Note       string            `json:"note,omitempty"`
	// Failure marks a failed switch whose revert is still owed.
	Failure string `json:"failure,omitempty"`
}

func resolveModelSwitchTarget(cur ModelSwitchTarget, req ModelSwitchRequest) (ModelSwitchTarget, bool, error) {
	to := cur
	if req.Tool != nil {
		to.Tool = NormalizeToolName(strings.TrimSpace(*req.Tool))
		if !KnownTool(to.Tool) {
			return to, false, fmt.Errorf("%w: %q", ErrModelSwitchUnknownTool, to.Tool)
		}
	}
	to.Model = normalizeRetiredGrokModel(to.Tool, strings.TrimSpace(req.Model))
	if !ToolModelAllowed(to.Tool, to.Model) {
		return to, false, fmt.Errorf("%w: %q on %s", ErrModelSwitchUnknownModel, to.Model, to.Tool)
	}
	if req.Effort != nil {
		to.Effort = strings.TrimSpace(*req.Effort)
		if !ValidToolModelEffort(to.Tool, to.Model, to.Effort) {
			return to, false, fmt.Errorf("%w: %q on %s", ErrModelSwitchInvalidEffort, to.Effort, to.Model)
		}
		return to, false, nil
	}
	if !ValidToolModelEffort(to.Tool, to.Model, to.Effort) {
		to.Effort = ""
		return to, true, nil
	}
	return to, false, nil
}

func modelSwitchTargetOf(a *Agent) ModelSwitchTarget {
	return ModelSwitchTarget{Tool: NormalizeToolName(a.Tool), Model: a.Model, Effort: a.Effort}
}

// SwitchModel stores the new model and schedules the next turn. See the
// comment at the top of this file.
func (m *Manager) SwitchModel(agentID string, req ModelSwitchRequest) (*ModelSwitchResult, error) {
	m.modelSwitchMu.Lock()
	defer m.modelSwitchMu.Unlock()

	a, ok := m.Get(agentID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, agentID)
	}
	if a.Archived {
		return nil, fmt.Errorf("%w: %s", ErrAgentArchived, agentID)
	}
	current := modelSwitchTargetOf(a)
	to, cleared, err := resolveModelSwitchTarget(current, req)
	if err != nil {
		return nil, err
	}

	prior := m.modelSwitches[agentID]
	if prior != nil && prior.phase != modelSwitchAwaitingSource {
		prior = nil // its outcome no longer matters: this request replaces it
	}
	if prior != nil && (prior.sessionKey != req.SessionKey || prior.sourceEnded) {
		return nil, ErrModelSwitchPending
	}
	if to == current {
		res := &ModelSwitchResult{Status: ModelSwitchUnchanged, From: current, To: to}
		if prior != nil {
			prior.note = req.Note
			m.putModelSwitchMarker(prior)
			res.Status, res.From = ModelSwitchQueued, prior.from
		}
		return res, nil
	}

	var oneShotID int64
	var capability string
	var sourceCtx context.Context
	if req.SessionKey != "" {
		origin, ok := m.InFlightOneShotOrigin(agentID, req.SessionKey)
		if !ok {
			return nil, ErrModelSwitchCallerInactive
		}
		oneShotID = origin.ID
		capability = origin.HandoffCapability
	} else {
		m.busyMu.Lock()
		if entry, busy := m.busy[agentID]; busy {
			sourceCtx = entry.ctx
		}
		m.busyMu.Unlock()
	}
	if NativeGoalRunning(agentID, req.SessionKey) {
		return nil, ErrModelSwitchGoalRunning
	}
	if m.modelSwitchPendingTasks(agentID, req.SessionKey) > 0 {
		return nil, ErrModelSwitchBackgroundTasks
	}

	updated, err := m.Update(agentID, modelSwitchUpdate(current, to))
	if err != nil {
		return nil, err
	}
	to = modelSwitchTargetOf(updated)

	if prior != nil {
		res := &ModelSwitchResult{Status: ModelSwitchQueued, From: prior.from, To: to, EffortCleared: cleared}
		if to == prior.from {
			// Switched back before the turn ended: nothing left to do.
			delete(m.modelSwitches, agentID)
			m.deleteModelSwitchMarker(agentID)
			close(prior.sourceDone) // no turn end will reach it now
			res.Status = ModelSwitchUnchanged
			return res, nil
		}
		prior.to, prior.note = to, req.Note
		m.putModelSwitchMarker(prior)
		return res, nil
	}

	p := &pendingModelSwitch{
		agentID: agentID, sessionKey: req.SessionKey,
		from: current, to: to, note: req.Note,
		oneShotID: oneShotID, capability: capability,
		sourceDone: make(chan struct{}), token: newModelSwitchToken(),
	}
	if m.modelSwitches == nil {
		m.modelSwitches = make(map[string]*pendingModelSwitch)
	}
	m.modelSwitches[agentID] = p
	m.putModelSwitchMarker(p)
	go m.runModelSwitch(p, sourceCtx)
	return &ModelSwitchResult{Status: ModelSwitchQueued, From: current, To: to, EffortCleared: cleared}, nil
}

func modelSwitchUpdate(from, to ModelSwitchTarget) AgentUpdateConfig {
	expected := from
	cfg := AgentUpdateConfig{Model: &to.Model, Effort: &to.Effort, expectModel: &expected}
	if to.Tool != from.Tool {
		cfg.Tool = &to.Tool
	}
	return cfg
}

// modelSwitchPendingTasks counts the background tasks a switch would kill:
// the main process respawns on its next turn whichever conversation asked,
// and the calling thread's process is replaced for its next turn.
func (m *Manager) modelSwitchPendingTasks(agentID, sessionKey string) int {
	cb := m.claudeBackend()
	if cb == nil {
		return 0
	}
	cb.sessMu.Lock()
	sessions := []*claudeSession{cb.sessions[agentID]}
	if sessionKey != "" {
		sessions = append(sessions, cb.sessions[keyedPoolKey(agentID, sessionKey)])
	}
	cb.sessMu.Unlock()
	total := 0
	for _, s := range sessions {
		if s == nil {
			continue
		}
		s.mu.Lock()
		total += s.pendingTasks
		s.mu.Unlock()
	}
	return total
}

// runModelSwitch waits for the calling turn to end, then starts the next one.
func (m *Manager) runModelSwitch(p *pendingModelSwitch, sourceCtx context.Context) {
	switch {
	case p.oneShotID != 0:
		<-p.sourceDone
	case sourceCtx != nil:
		<-sourceCtx.Done()
		for m.busyEntryLive(p.agentID, sourceCtx) {
			time.Sleep(modelSwitchRetryInterval / 5)
		}
		m.modelSwitchMu.Lock()
		p.stop = modelSwitchStopOf(sourceCtx)
		m.modelSwitchMu.Unlock()
	}
	m.deliverModelSwitch(p)
}

func (m *Manager) busyEntryLive(agentID string, ctx context.Context) bool {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	entry, ok := m.busy[agentID]
	return ok && entry.ctx == ctx
}

// deliverModelSwitch starts the turn that runs on the new model, unless the
// calling turn was stopped (see modelSwitchStop).
func (m *Manager) deliverModelSwitch(p *pendingModelSwitch) {
	m.modelSwitchMu.Lock()
	if m.modelSwitches[p.agentID] != p {
		m.modelSwitchMu.Unlock()
		return
	}
	switch p.stop {
	case modelSwitchDrop:
		delete(m.modelSwitches, p.agentID)
		m.deleteModelSwitchMarker(p.agentID)
		m.modelSwitchMu.Unlock()
		return
	case modelSwitchDefer:
		delete(m.modelSwitches, p.agentID)
		m.modelSwitchMu.Unlock()
		return
	}
	p.phase = modelSwitchVerifying
	if !p.bound {
		p.verifyKey = p.sessionKey
	}
	continued := p.continued
	msg := m.modelSwitchPrompt(p)
	// The marker stays until the turn reaches its backend (bindModelSwitchTurn),
	// so a restart before that point still resumes it.
	m.modelSwitchMu.Unlock()

	if continued {
		m.expireUnboundModelSwitch(p)
		return
	}
	deadline := time.Now().Add(modelSwitchWakeWindow)
	for {
		err := m.fireModelSwitchWake(p, msg)
		switch {
		case err == nil:
			m.expireUnboundModelSwitch(p)
			return
		case errors.Is(err, ErrQuiescing):
			m.deferModelSwitchWake(p)
			return
		case errors.Is(err, ErrAgentBusy) && time.Now().Before(deadline):
			time.Sleep(modelSwitchRetryInterval)
			m.modelSwitchMu.Lock()
			current := m.modelSwitches[p.agentID] == p
			m.modelSwitchMu.Unlock()
			if !current {
				return
			}
		case modelSwitchStartRefused(err):
			m.logger.Warn("model switch: next turn not started", "agent", p.agentID, "err", err)
			m.discardModelSwitch(p)
			return
		default:
			if m.discardModelSwitch(p) {
				m.rollbackModelSwitch(p, err.Error())
			}
			return
		}
	}
}

// closeStaleKeyedProcess ends the calling thread's idle lingering process: a
// keyed process is reused without comparing its args, so it would answer the
// next turn on the old model. A process with tasks is left alone.
// It reports false when such a process stays and would serve the key.
func (m *Manager) closeStaleKeyedProcess(agentID, sessionKey string) bool {
	cb := m.claudeBackend()
	if cb == nil {
		return true
	}
	return cb.CloseKeyedSessionIfIdle(agentID, sessionKey, keyedModelSwitchReason)
}

// expireUnboundModelSwitch drops the marker of a requested turn that has not
// reached a backend, so it cannot start a phantom turn at some later boot. The
// pending switch stays: a turn still queued is verified when it does start.
func (m *Manager) expireUnboundModelSwitch(p *pendingModelSwitch) {
	time.AfterFunc(modelSwitchBindLimit, func() { m.expireModelSwitchMarker(p) })
}

func (m *Manager) expireModelSwitchMarker(p *pendingModelSwitch) {
	m.busyMu.Lock()
	quiescing := m.quiescing
	m.busyMu.Unlock()
	if quiescing {
		return // the marker is what resumes the turn after this restart
	}
	m.modelSwitchMu.Lock()
	defer m.modelSwitchMu.Unlock()
	if m.modelSwitches[p.agentID] == p && !p.bound {
		m.deleteModelSwitchMarker(p.agentID)
	}
}

// deferModelSwitchWake hands an unstarted wake to the next boot.
func (m *Manager) deferModelSwitchWake(p *pendingModelSwitch) {
	m.modelSwitchMu.Lock()
	defer m.modelSwitchMu.Unlock()
	if m.modelSwitches[p.agentID] != p {
		return
	}
	delete(m.modelSwitches, p.agentID)
	if !p.bound {
		m.putModelSwitchMarker(p)
		m.resumeIfQuiesceLifted()
	}
}

// resumeIfQuiesceLifted covers a restart aborted between a refusal and the
// marker being saved: the abort's own re-read could not have seen it.
func (m *Manager) resumeIfQuiesceLifted() {
	m.busyMu.Lock()
	quiescing := m.quiescing
	m.busyMu.Unlock()
	if !quiescing {
		go m.ConsumeModelSwitchWakes(time.Now())
	}
}

// modelSwitchStartRefused: the turn did not start for a reason that says
// nothing against the model, so the switch stands without a turn.
func modelSwitchStartRefused(err error) bool {
	return errors.Is(err, ErrAgentBusy) || errors.Is(err, ErrAgentResetting) ||
		errors.Is(err, ErrAgentNotFound) || errors.Is(err, ErrAgentArchived) ||
		errors.Is(err, ErrTurnStopped) || errors.Is(err, context.Canceled)
}

func newModelSwitchToken() string {
	raw := make([]byte, 6)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

// bindModelSwitchTurn runs right before a turn reaches its backend and
// recognizes the auto-started turn by the token in its prompt.
func (m *Manager) bindModelSwitchTurn(agentID, sessionKey string, turnID int64, message string) {
	m.modelSwitchMu.Lock()
	p := m.modelSwitches[agentID]
	if p == nil || p.bound || !strings.Contains(message, modelSwitchTokenText(p.token)) {
		m.modelSwitchMu.Unlock()
		return
	}
	p.bound, p.boundTurnID, p.verifyKey = true, turnID, sessionKey
	// Consumed as the turn starts: a crash from here on cannot run it twice.
	m.deleteModelSwitchMarker(agentID)
	m.modelSwitchMu.Unlock()

	// Checked here, not when the wake is requested: another turn of the
	// thread may have held the process until now.
	if !m.closeStaleKeyedProcess(agentID, sessionKey) {
		// Tasks keep the old process serving this turn, so its outcome says
		// nothing about the new model.
		m.logger.Warn("model switch: thread stays on its old process until its tasks end", "agent", agentID, "thread", sessionKey)
		m.forgetModelSwitch(p)
	}
}

// unbindModelSwitchTurn undoes the bind when the backend refused to start the
// turn: it never ran, so a retry or the next boot may still run it.
func (m *Manager) unbindModelSwitchTurn(agentID, message string) {
	m.modelSwitchMu.Lock()
	defer m.modelSwitchMu.Unlock()
	p := m.modelSwitches[agentID]
	if p == nil || !p.bound || !strings.Contains(message, modelSwitchTokenText(p.token)) {
		return
	}
	p.bound, p.boundTurnID = false, 0
	m.putModelSwitchMarker(p)
}

// noteModelSwitchStartFailure handles the auto-started turn being refused
// before it reached a backend.
func (m *Manager) noteModelSwitchStartFailure(agentID, message string, err error) {
	m.modelSwitchMu.Lock()
	p := m.modelSwitches[agentID]
	if p == nil || !strings.Contains(message, modelSwitchTokenText(p.token)) {
		m.modelSwitchMu.Unlock()
		return
	}
	if errors.Is(err, ErrQuiescing) {
		m.modelSwitchMu.Unlock()
		m.deferModelSwitchWake(p)
		return
	}
	delete(m.modelSwitches, agentID)
	m.deleteModelSwitchMarker(agentID)
	m.modelSwitchMu.Unlock()
	if modelSwitchStartRefused(err) {
		m.logger.Warn("model switch: next turn not started", "agent", agentID, "err", err)
		return
	}
	go m.rollbackModelSwitch(p, err.Error())
}

func modelSwitchTokenText(token string) string { return "(switch " + token + ")" }

// fireModelSwitchWake delivers msg to the calling thread, or to the main
// conversation when the thread cannot be re-driven from here.
func (m *Manager) fireModelSwitchWake(p *pendingModelSwitch, msg string) error {
	if p.sessionKey != "" {
		err := m.WakeThread(p.agentID, p.sessionKey, msg)
		if !errors.Is(err, errWakeThreadGone) {
			return err
		}
		m.modelSwitchMu.Lock()
		p.verifyKey = ""
		m.modelSwitchMu.Unlock()
		msg += "\n\nThe switch was requested from another conversation (" + p.sessionKey +
			") that kojo cannot resume from here, so this turn runs in your main conversation."
	}
	return m.WakeChat(p.agentID, msg)
}

func (m *Manager) forgetModelSwitch(p *pendingModelSwitch) bool {
	m.modelSwitchMu.Lock()
	defer m.modelSwitchMu.Unlock()
	if m.modelSwitches[p.agentID] != p {
		return false
	}
	delete(m.modelSwitches, p.agentID)
	return true
}

// discardModelSwitch is forgetModelSwitch for a wake that will never run.
func (m *Manager) discardModelSwitch(p *pendingModelSwitch) bool {
	m.modelSwitchMu.Lock()
	defer m.modelSwitchMu.Unlock()
	if m.modelSwitches[p.agentID] != p {
		return false
	}
	delete(m.modelSwitches, p.agentID)
	m.deleteModelSwitchMarker(p.agentID)
	return true
}

// modelSwitchPrompt is the wake turn after a self switch. The switch token
// must stay in it (it binds the verifying turn); the model names and effort
// are dropped when the agent disables InjectionModelSwitchInfo. Callers hold
// modelSwitchMu, which already nests m.mu (see the Update call in
// RequestModelSwitch), so the injection lookup is safe here.
func (m *Manager) modelSwitchPrompt(p *pendingModelSwitch) string {
	var msg string
	if m.injectionDisabled(p.agentID, InjectionModelSwitchInfo) {
		msg = fmt.Sprintf(
			"[System] Your model switch is complete %s. This turn was auto-triggered and runs on the new model. Continue your pending work.",
			modelSwitchTokenText(p.token))
	} else {
		msg = fmt.Sprintf(
			"[System] Your model switch is complete: %s -> %s %s. This turn was auto-triggered and runs on the new model. Continue your pending work.",
			p.from, p.to, modelSwitchTokenText(p.token))
	}
	if p.note != "" {
		msg += "\n\nNote you left for this turn:\n" + p.note
	}
	return msg
}

// modelSwitchRollbackPrompt tells the agent its switch was reverted. With
// hideInfo the model names and effort are left out; the failure reason stays.
func modelSwitchRollbackPrompt(p *pendingModelSwitch, reason string, hideInfo bool) string {
	if hideInfo {
		return fmt.Sprintf("[System] Your model switch failed (%s), so kojo reverted you to the previous model. This turn was auto-triggered on the previous model.", reason)
	}
	return fmt.Sprintf("[System] Your model switch to %s failed (%s), so kojo reverted you to %s. This turn was auto-triggered on the previous model.",
		p.to, reason, p.from)
}

// noteModelSwitchTurnEnd observes every finished turn. oneShotID is 0 for
// the main chat. reservation is the slot the response adapter holds behind a
// thread turn: it can only be activated while that turn is still streaming,
// so the continuation is requested here rather than from runModelSwitch.
func (m *Manager) noteModelSwitchTurnEnd(agentID, sessionKey string, oneShotID int64, ctx context.Context, end turnEnd, reservation HandoffArrivalReservation) {
	m.modelSwitchMu.Lock()
	p := m.modelSwitches[agentID]
	if p == nil {
		m.modelSwitchMu.Unlock()
		return
	}
	if p.bound && p.boundTurnID == oneShotID && p.verifyKey == sessionKey {
		delete(m.modelSwitches, agentID)
		m.modelSwitchMu.Unlock()
		if failure := modelSwitchTurnFailure(ctx, end); failure != "" {
			go m.rollbackModelSwitch(p, failure)
		}
		return
	}
	if p.phase != modelSwitchAwaitingSource || oneShotID == 0 || p.oneShotID != oneShotID || p.sourceEnded {
		m.modelSwitchMu.Unlock()
		return
	}
	p.sourceEnded = true
	p.stop = modelSwitchStopOf(ctx)
	if _, stopped := turnStopCause(ctx, end); stopped && p.stop == modelSwitchProceed && end.terminal {
		p.stop = modelSwitchDrop
	}
	// A WebUI thread resumes through WakeThread, which keeps its native
	// session; an adapter slot starts the arrival from a fresh one.
	activate := p.stop == modelSwitchProceed && !strings.HasPrefix(sessionKey, webUIThreadKeyPrefix)
	capability, prompt := p.capability, m.modelSwitchPrompt(p)
	continueThread := m.modelSwitchContinue
	m.modelSwitchMu.Unlock()

	if activate {
		var err error
		actCtx, cancel := context.WithTimeout(context.Background(), modelSwitchActivateLimit)
		switch {
		case reservation != nil:
			err = reservation.Activate(actCtx, prompt, "")
		case capability != "" && continueThread != nil:
			err = continueThread(actCtx, agentID, sessionKey, capability, prompt)
		default:
			err = errWakeThreadGone
		}
		cancel()
		if err != nil && !errors.Is(err, errWakeThreadGone) {
			m.logger.Warn("model switch: thread continuation refused", "agent", agentID, "thread", sessionKey, "err", err)
		}
		m.modelSwitchMu.Lock()
		p.continued = err == nil
		m.modelSwitchMu.Unlock()
	}
	close(p.sourceDone)
}

// SetModelSwitchContinuer installs how a thread turn's adapter continuation
// is requested from its capability (the server owns that ledger).
func (m *Manager) SetModelSwitchContinuer(fn func(ctx context.Context, agentID, sessionKey, capability, prompt string) error) {
	m.modelSwitchMu.Lock()
	defer m.modelSwitchMu.Unlock()
	m.modelSwitchContinue = fn
}

// modelSwitchTurnFailure returns why a turn failed, or "" when it says
// nothing against the model. A stop or a timeout is not the model's fault.
func modelSwitchTurnFailure(ctx context.Context, end turnEnd) string {
	if _, stopped := turnStopCause(ctx, end); stopped {
		return ""
	}
	if end.terminal {
		if end.errMsg == ErrMsgTimeout || end.errMsg == ErrMsgCancelled {
			return ""
		}
		return end.errMsg
	}
	if ctx.Err() == nil {
		return "event stream closed without a result"
	}
	return ""
}

// rollbackModelSwitch restores the previous model unless someone already
// changed it again, and tells the agent in one turn. That turn is not
// verified, so a backend that is down altogether cannot start a loop.
func (m *Manager) rollbackModelSwitch(p *pendingModelSwitch, reason string) {
	a, ok := m.Get(p.agentID)
	if !ok {
		return
	}
	current := modelSwitchTargetOf(a)
	if current != p.to {
		m.logger.Info("model switch: failed but config moved on; not reverting", "agent", p.agentID)
		return
	}
	if _, err := m.Update(p.agentID, modelSwitchUpdate(current, p.from)); err != nil {
		if errors.Is(err, ErrQuiescing) {
			m.deferModelSwitchRollback(p, reason)
			return
		}
		m.logger.Warn("model switch: revert failed", "agent", p.agentID, "err", err)
		return
	}
	if len(reason) > modelSwitchReasonLimit {
		reason = strings.ToValidUTF8(reason[:modelSwitchReasonLimit], "") + "..."
	}
	m.logger.Warn("model switch: reverted", "agent", p.agentID, "from", p.to.String(), "to", p.from.String(), "reason", reason)
	msg := modelSwitchRollbackPrompt(p, reason, m.injectionDisabled(p.agentID, InjectionModelSwitchInfo))
	if p.note != "" {
		msg += "\n\nNote you left for the switched turn:\n" + p.note
	}
	// A process kept alive by its tasks still runs the failed model: the
	// notice then goes to main, which respawns on the restored one.
	noticeKey := p.verifyKey
	if !m.closeStaleKeyedProcess(p.agentID, noticeKey) {
		noticeKey = ""
	}
	deadline := time.Now().Add(modelSwitchWakeWindow)
	for {
		var err error
		if noticeKey != "" {
			err = m.WakeThread(p.agentID, noticeKey, msg)
		} else {
			err = errWakeThreadGone
		}
		if errors.Is(err, errWakeThreadGone) {
			err = m.WakeChat(p.agentID, msg)
		}
		if err == nil {
			return
		}
		if errors.Is(err, ErrQuiescing) || !errors.Is(err, ErrAgentBusy) || time.Now().After(deadline) {
			m.logger.Warn("model switch: revert notice not delivered", "agent", p.agentID, "err", err)
			return
		}
		time.Sleep(modelSwitchRetryInterval)
	}
}

// deferModelSwitchRollback records a revert kojo's restart refused, so a
// model that cannot run does not survive into the next boot. A newer switch
// owns the marker and supersedes it, even one that has already finished.
func (m *Manager) deferModelSwitchRollback(p *pendingModelSwitch, reason string) {
	m.modelSwitchMu.Lock()
	defer m.modelSwitchMu.Unlock()
	if m.modelSwitches[p.agentID] != nil {
		return
	}
	if a, ok := m.Get(p.agentID); !ok || modelSwitchTargetOf(a) != p.to {
		return
	}
	p.failure = reason
	m.putModelSwitchMarker(p)
	m.resumeIfQuiesceLifted()
}

func (m *Manager) putModelSwitchMarker(p *pendingModelSwitch) {
	st := m.Store()
	if st == nil {
		return
	}
	sessionKey := p.sessionKey
	if p.failure != "" {
		sessionKey = p.verifyKey // the conversation that failed gets the notice
	}
	val, err := json.Marshal(modelSwitchMarker{
		AgentID: p.agentID, SessionKey: sessionKey, From: p.from, To: p.to, Note: p.note,
		Failure: p.failure,
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := st.PutKV(ctx, &store.KVRecord{
		Namespace: modelSwitchKVNamespace,
		Key:       p.agentID,
		Value:     string(val),
		Type:      store.KVTypeString,
		Scope:     store.KVScopeMachine,
	}, store.KVPutOptions{}); err != nil {
		m.logger.Warn("model switch: marker write failed", "agent", p.agentID, "err", err)
	}
}

func (m *Manager) deleteModelSwitchMarker(agentID string) {
	st := m.Store()
	if st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := st.DeleteKV(ctx, modelSwitchKVNamespace, agentID, ""); err != nil && !errors.Is(err, store.ErrNotFound) {
		m.logger.Warn("model switch: marker clear failed", "agent", agentID, "err", err)
	}
}

// ConsumeModelSwitchWakes starts the turns whose switch was pending when the
// previous process exited. It is at-most-once per started turn: the marker is
// removed just before the turn reaches its backend.
func (m *Manager) ConsumeModelSwitchWakes(bootTime time.Time) {
	st := m.Store()
	if st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	recs, err := st.ListKV(ctx, modelSwitchKVNamespace)
	if err != nil {
		m.logger.Warn("model switch: marker list failed", "err", err)
		return
	}
	for _, rec := range recs {
		if rec.UpdatedAt > bootTime.UnixMilli() {
			continue
		}
		p := m.restoreModelSwitch(ctx, rec)
		switch {
		case p == nil:
		case p.failure != "":
			go m.rollbackModelSwitch(p, p.failure)
		default:
			go m.deliverModelSwitch(p)
		}
	}
}

// restoreModelSwitch consumes one marker. It returns nil when nothing should
// run: a switch of this process owns the marker, or the model moved on since.
func (m *Manager) restoreModelSwitch(ctx context.Context, rec *store.KVRecord) *pendingModelSwitch {
	m.modelSwitchMu.Lock()
	defer m.modelSwitchMu.Unlock()
	if _, live := m.modelSwitches[rec.Key]; live {
		return nil
	}
	// The listing may predate a wake that has since run and removed this marker.
	cur, err := m.Store().GetKV(ctx, modelSwitchKVNamespace, rec.Key)
	if err != nil || cur.ETag != rec.ETag {
		return nil
	}
	drop := func() bool {
		err := m.Store().DeleteKV(ctx, modelSwitchKVNamespace, rec.Key, rec.ETag)
		if err != nil {
			m.logger.Warn("model switch: marker clear failed; skipping", "agent", rec.Key, "err", err)
		}
		return err == nil
	}
	var mk modelSwitchMarker
	if err := json.Unmarshal([]byte(rec.Value), &mk); err != nil || mk.AgentID != rec.Key {
		m.logger.Warn("model switch: dropping malformed marker", "agent", rec.Key, "err", err)
		drop()
		return nil
	}
	a, ok := m.Get(mk.AgentID)
	if !ok || modelSwitchTargetOf(a) != mk.To {
		m.logger.Info("model switch: dropping stale marker", "agent", rec.Key)
		drop()
		return nil
	}
	p := &pendingModelSwitch{
		agentID: mk.AgentID, sessionKey: mk.SessionKey,
		from: mk.From, to: mk.To, note: mk.Note,
		sourceEnded: true, token: newModelSwitchToken(),
	}
	if mk.Failure != "" {
		if !drop() {
			return nil
		}
		p.failure, p.verifyKey = mk.Failure, mk.SessionKey
		return p // a revert, not a pending switch
	}
	// The marker stays until the turn reaches its backend, so another restart
	// before that point still resumes it.
	if m.modelSwitches == nil {
		m.modelSwitches = make(map[string]*pendingModelSwitch)
	}
	m.modelSwitches[p.agentID] = p
	return p
}

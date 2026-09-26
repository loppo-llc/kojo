package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Agent-facing view/control of keyed lingering sessions: the thread
// conversations (Slack threads, WebUI threads) whose CLI process is kept alive
// after a turn because run_in_background tasks are still running.
//
// Served by GET/DELETE /api/v1/agents/{id}/background-sessions[...] (see the
// background-sessions guide).

// KeyedStopRequestedReason is the close reason of a session stopped through
// the background-sessions API. Surfaces render it as an explicit stop.
const KeyedStopRequestedReason = "エージェントの依頼で停止"

// KeyedUserStopReason is the close reason of a session whose background tasks
// a person in the thread stopped (Slack `!stop all`). Unlike an agent-requested
// stop, the agent gets a one-time note on its next turn in the thread.
const KeyedUserStopReason = "ユーザーの依頼で停止"

// keyedUserStopNotedReason is KeyedUserStopReason for a close whose agent note
// the stop already stored synchronously: the exit notice then only reaches the
// thread (as KeyedUserStopReason) and does not store the note again.
const keyedUserStopNotedReason = KeyedUserStopReason + " (note stored)"

// IsKeyedStopReason reports whether reason is an explicit stop (as opposed to
// a crash, cap or lifecycle close), so surfaces can word the notice as such.
func IsKeyedStopReason(reason string) bool {
	return reason == KeyedStopRequestedReason || reason == KeyedUserStopReason
}

var (
	// ErrBackgroundSessionNotFound: no lingering/background session for the key.
	ErrBackgroundSessionNotFound = errors.New("background session not found")
	// ErrBackgroundTaskNotFound: the session has no such pending task.
	ErrBackgroundTaskNotFound = errors.New("background task not found")
)

// BackgroundTaskInfo describes one still-running background task.
type BackgroundTaskInfo struct {
	ID          string `json:"id"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	// StartedAt is when the task started: the arrival of the CLI's
	// task_started event (the CLI reports no start timestamp; for a
	// foreground task backgrounded later this predates its first
	// background_tasks_changed listing). ElapsedSeconds is measured from it.
	StartedAt      string `json:"startedAt"`
	ElapsedSeconds int64  `json:"elapsedSeconds"`
}

// BackgroundSessionInfo describes one keyed session with background tasks.
type BackgroundSessionInfo struct {
	SessionKey string `json:"sessionKey"`
	// Surface is "webui_thread", "slack", or "other".
	Surface    string `json:"surface"`
	ThreadID   string `json:"threadId,omitempty"`
	ThreadName string `json:"threadName,omitempty"`
	// State: "lingering" (idle, waiting for tasks), "turn" (a turn is running
	// on the process), "closing".
	State          string               `json:"state"`
	LingeringSince string               `json:"lingeringSince,omitempty"`
	PendingCount   int                  `json:"pendingCount"`
	Tasks          []BackgroundTaskInfo `json:"tasks"`
}

// BackgroundSessionsSnapshot is the list response.
type BackgroundSessionsSnapshot struct {
	Sessions []BackgroundSessionInfo `json:"sessions"`
	// Lingering is how many sessions occupy a linger slot; Cap is the
	// per-agent maximum. A turn that ends with tasks pending while
	// Lingering == Cap cannot keep them running.
	Lingering int `json:"lingering"`
	Cap       int `json:"cap"`
}

// keyedSessionSnapshot is the backend-level view of one keyed session.
type keyedSessionSnapshot struct {
	sessionKey  string
	state       string
	lingerSince time.Time
	pending     int
	tasks       []claudeBackgroundTask
	taskSeen    map[string]time.Time
}

// keyedSessionsWithTasks snapshots the agent's keyed sessions that have
// background tasks pending or hold a linger slot.
func (b *ClaudeBackend) keyedSessionsWithTasks(agentID string) []keyedSessionSnapshot {
	b.sessMu.Lock()
	var sessions []*claudeSession
	for _, s := range b.sessions {
		if s.keyed && s.agentID == agentID {
			sessions = append(sessions, s)
		}
	}
	b.sessMu.Unlock()
	var out []keyedSessionSnapshot
	for _, s := range sessions {
		s.mu.Lock()
		if s.pendingTasks == 0 && !s.lingerSlot {
			s.mu.Unlock()
			continue
		}
		snap := keyedSessionSnapshot{
			sessionKey:  s.sessionKey,
			lingerSince: s.lingerSince,
			pending:     s.pendingTasks,
			tasks:       append([]claudeBackgroundTask(nil), s.tasks...),
			taskSeen:    make(map[string]time.Time, len(s.taskSeen)),
		}
		for k, v := range s.taskSeen {
			snap.taskSeen[k] = v
		}
		switch {
		case s.state == sessDead || s.closing:
			snap.state = "closing"
		case s.state == sessInTurn:
			snap.state = "turn"
		default:
			snap.state = "lingering"
		}
		s.mu.Unlock()
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].sessionKey < out[j].sessionKey })
	return out
}

func (b *ClaudeBackend) keyedSession(agentID, sessionKey string) *claudeSession {
	b.sessMu.Lock()
	defer b.sessMu.Unlock()
	return b.sessions[keyedPoolKey(agentID, sessionKey)]
}

// stopBackgroundSession stops every background task of the keyed session.
// An idle (lingering) session is closed, which kills its tasks; the abandoned
// notice (reason KeyedStopRequestedReason) informs the thread. A session in
// the middle of a turn — possibly the very turn calling this API — is not
// killed: its tasks are stopped via stop_task control requests and
// notify(pending, surface) is called so the caller can post the stop notice
// itself. surface is the session's bound surface with a reference the notify
// callee must Release (nil when none is bound).
// allow (optional) vets the looked-up session instance itself, so an
// authorization check and the stop cannot hit two different sessions.
// noteLocked (optional) runs under the session lock with the number of tasks
// being stopped (idle: the session is already claimed closing), so the
// caller can store the agent's one-time note before any follow-up turn on the
// key can be admitted and peek it. It may only take leaf locks (lock order
// s.mu -> Manager.keyedBgMu). For an idle close a non-empty return replaces
// the close reason reported at exit (published in the same critical section,
// so an exit can never observe the old one); in-turn its return is ignored.
func (b *ClaudeBackend) stopBackgroundSession(agentID, sessionKey, reason string, allow func(*claudeSession) error, notify func(pending int, surface KeyedSessionSurface), noteLocked func(pending int, idle bool) (closeReason string)) error {
	s := b.keyedSession(agentID, sessionKey)
	if s == nil {
		return ErrBackgroundSessionNotFound
	}
	if allow != nil {
		if err := allow(s); err != nil {
			return err
		}
	}
	s.mu.Lock()
	if s.state == sessDead || s.closing {
		s.mu.Unlock()
		return ErrBackgroundSessionNotFound
	}
	pending := s.pendingTasks
	if s.state == sessInTurn {
		if reason == KeyedUserStopReason {
			// `!stop all` also interrupts this turn: whatever it spawns
			// after this snapshot is stopped at its result.
			s.stopAllAtTurnEnd = true
		}
		ids := make([]string, 0, len(s.tasks))
		for _, t := range s.tasks {
			if _, stopping := s.stopRequested[t.TaskID]; !stopping {
				ids = append(ids, t.TaskID)
			}
		}
		covered := len(s.tasks) > 0
		s.markStopRequestedLocked(ids...)
		var surface KeyedSessionSurface
		if len(ids) > 0 {
			if noteLocked != nil {
				noteLocked(len(ids), false)
			}
			// Retained while the tasks leave the exit's abandoned count
			// (see stopBackgroundTask): exactly this stop reports them.
			surface = s.retainSurface()
		}
		s.mu.Unlock()
		if len(ids) == 0 {
			if covered {
				// A concurrent stop already covers every task (and posts
				// the notice): coalesce instead of notifying twice.
				return nil
			}
			return ErrBackgroundSessionNotFound
		}
		for _, id := range ids {
			s.stopTask(id)
		}
		if notify != nil {
			notify(len(ids), surface)
		} else if surface != nil {
			surface.Release()
		}
		return nil
	}
	if pending == 0 && !s.lingerSlot {
		s.mu.Unlock()
		return ErrBackgroundSessionNotFound
	}
	s.closeReason = reason
	s.setClosingLocked()
	if noteLocked != nil {
		if r := noteLocked(s.pendingAtClose, true); r != "" {
			s.closeReason = r
		}
	}
	s.mu.Unlock()
	s.closeKeyed("")
	return nil
}

// stopBackgroundTask stops one pending task of the keyed session via the CLI's
// stop_task control request. The process keeps running; once nothing is
// pending the normal linger grace closes it. notify(surface) is called after
// the stop so the caller can post the stop notice (surface: the session's bound
// surface with a reference the callee must Release, or nil).
func (b *ClaudeBackend) stopBackgroundTask(agentID, sessionKey, taskID string, notify func(surface KeyedSessionSurface)) error {
	s := b.keyedSession(agentID, sessionKey)
	if s == nil {
		return ErrBackgroundSessionNotFound
	}
	s.mu.Lock()
	if s.state == sessDead || s.closing {
		s.mu.Unlock()
		return ErrBackgroundSessionNotFound
	}
	found := false
	for _, t := range s.tasks {
		if t.TaskID == taskID {
			found = true
			break
		}
	}
	_, already := s.stopRequested[taskID]
	var surface KeyedSessionSurface
	if found && !already {
		s.markStopRequestedLocked(taskID)
		// Retained in the same critical section that excludes the task from
		// the exit's abandoned count (s.mu -> surfaceMu, a leaf lock): an
		// exit racing the stop cannot take the surface first, so exactly
		// this stop reports the task.
		surface = s.retainSurface()
	}
	s.mu.Unlock()
	// The CLI acknowledges unknown task ids with success too, so validate
	// against the latest snapshot to give the caller a real 404.
	if !found {
		return ErrBackgroundTaskNotFound
	}
	if already {
		return nil // already stopping; its notice was posted
	}
	s.stopTask(taskID)
	if notify != nil {
		notify(surface)
	} else if surface != nil {
		surface.Release()
	}
	return nil
}

// markStopRequestedLocked records task ids sent a stop_task. Caller holds mu.
func (s *claudeSession) markStopRequestedLocked(ids ...string) {
	if len(ids) == 0 {
		return
	}
	if s.stopRequested == nil {
		s.stopRequested = make(map[string]struct{}, len(ids))
	}
	for _, id := range ids {
		s.stopRequested[id] = struct{}{}
	}
}

// stopTask writes a stop_task control request (verified against claude
// 2.1.280: the CLI kills the task, emits background_tasks_changed without it
// and a task_notification with status "stopped"; no auto-turn follows).
func (s *claudeSession) stopTask(taskID string) {
	line, _ := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": "kojo-stop-task-" + generateMessageID(),
		"request":    map[string]any{"subtype": "stop_task", "task_id": taskID},
	})
	s.stdinW.mu.Lock()
	if !s.stdinW.closed {
		_, _ = s.stdinW.w.Write(append(line, '\n'))
	}
	s.stdinW.mu.Unlock()
}

func (m *Manager) claudeBackend() *ClaudeBackend {
	cb, _ := m.backends["claude"].(*ClaudeBackend)
	return cb
}

// keyedSessionSurface classifies a session key for the API.
func (m *Manager) keyedSessionSurface(agentID, sessionKey string) (surface, threadID, threadName string) {
	if id, ok := strings.CutPrefix(sessionKey, webUIThreadKeyPrefix); ok {
		name := ""
		if m.groupdms != nil {
			if g, ok := m.groupdms.Get(id); ok && g != nil {
				name = g.Name
			}
		}
		return "webui_thread", id, name
	}
	if rest, ok := strings.CutPrefix(sessionKey, agentID+":slack:"); ok {
		return "slack", rest, ""
	}
	return "other", "", ""
}

// keyedBackgroundPending is the number of background tasks still running on
// the Hub-local keyed session (0 when there is none or it is closing).
func (m *Manager) keyedBackgroundPending(agentID, sessionKey string) int {
	cb := m.claudeBackend()
	if cb == nil {
		return 0
	}
	s := cb.keyedSession(agentID, sessionKey)
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == sessDead || s.closing {
		return 0
	}
	return s.pendingTasks
}

// ListBackgroundSessions returns the agent's thread sessions that are still
// running background tasks.
func (m *Manager) ListBackgroundSessions(agentID string) (BackgroundSessionsSnapshot, error) {
	if _, ok := m.Get(agentID); !ok {
		return BackgroundSessionsSnapshot{}, ErrAgentNotFound
	}
	out := BackgroundSessionsSnapshot{Sessions: []BackgroundSessionInfo{}, Cap: maxLingeringSessionsPerAgent}
	cb := m.claudeBackend()
	if cb == nil {
		return out, nil
	}
	now := time.Now()
	for _, snap := range cb.keyedSessionsWithTasks(agentID) {
		info := BackgroundSessionInfo{
			SessionKey:   snap.sessionKey,
			State:        snap.state,
			PendingCount: snap.pending,
			Tasks:        []BackgroundTaskInfo{},
		}
		info.Surface, info.ThreadID, info.ThreadName = m.keyedSessionSurface(agentID, snap.sessionKey)
		if !snap.lingerSince.IsZero() {
			info.LingeringSince = snap.lingerSince.Format(time.RFC3339)
		}
		for _, t := range snap.tasks {
			ti := BackgroundTaskInfo{ID: t.TaskID, Type: t.TaskType, Description: t.Description}
			if at, ok := snap.taskSeen[t.TaskID]; ok {
				ti.StartedAt = at.Format(time.RFC3339)
				ti.ElapsedSeconds = int64(now.Sub(at).Seconds())
			}
			info.Tasks = append(info.Tasks, ti)
		}
		out.Sessions = append(out.Sessions, info)
	}
	out.Lingering = cb.lingeringCount(agentID, "")
	return out, nil
}

// StopBackgroundSession stops all background tasks of one thread session and
// posts a stop notice into that thread (agent-requested, via the API).
func (m *Manager) StopBackgroundSession(agentID, sessionKey string) error {
	return m.stopBackgroundSessionReason(agentID, sessionKey, KeyedStopRequestedReason, nil)
}

// StopThreadBackgroundTasks is StopBackgroundSession on behalf of a person in
// the thread (Slack `!stop all`): same stop, with KeyedUserStopReason so the
// agent is told on its next turn that its tasks were stopped.
func (m *Manager) StopThreadBackgroundTasks(agentID, sessionKey string) error {
	return m.stopBackgroundSessionReason(agentID, sessionKey, KeyedUserStopReason, nil)
}

// StopThreadBackgroundTasksFromOrigin is the holder side of a Hub-relayed
// `!stop all`: a peer may stop only a session reporting to that peer's surface
// (like SteerOneShotFromOrigin, a session with no remote origin — a thread
// served by this holder itself — is never a peer's to stop). originPeerID ""
// is the unsafe local mode and always allowed.
func (m *Manager) StopThreadBackgroundTasksFromOrigin(agentID, sessionKey, originPeerID string) error {
	if originPeerID == "" {
		return m.StopThreadBackgroundTasks(agentID, sessionKey)
	}
	return m.stopBackgroundSessionReason(agentID, sessionKey, KeyedUserStopReason, func(s *claudeSession) error {
		if s.closingOrDead() {
			return ErrBackgroundSessionNotFound
		}
		owner := ""
		if sf := s.retainSurface(); sf != nil {
			owner = sf.OriginPeerID()
			sf.Release()
		}
		if owner != originPeerID {
			return ErrSteerOriginForbidden
		}
		return nil
	})
}

func (m *Manager) stopBackgroundSessionReason(agentID, sessionKey, reason string, allow func(*claudeSession) error) error {
	if _, ok := m.Get(agentID); !ok {
		return ErrAgentNotFound
	}
	cb := m.claudeBackend()
	if cb == nil {
		return ErrBackgroundSessionNotFound
	}
	return cb.stopBackgroundSession(agentID, sessionKey, reason, allow, func(pending int, surface KeyedSessionSurface) {
		// The agent's note was stored under the session lock (noteLocked);
		// only the thread notice is posted asynchronously.
		go func() {
			m.deliverKeyedTasksAbandoned(agentID, sessionKey, pending, reason, surface)
			if surface != nil {
				surface.Release()
			}
		}()
	}, func(pending int, idle bool) string {
		// Stored under the session lock, so a follow-up turn on the key
		// (admitted only after this critical section: in-turn at the
		// running turn's result, idle after the claimed-closing session's
		// exit) always peeks it. The agent was live at entry; a deletion
		// racing this drops the note again (dropKeyedNotesAfterExitNotices).
		if keyedAbandonedNote(pending, reason) == "" {
			return ""
		}
		m.storeKeyedNoteLocked(agentID, sessionKey, pending, reason)
		if !idle {
			return ""
		}
		// Idle close: keep the asynchronous exit path from storing it again.
		return keyedUserStopNotedReason
	})
}

// StopBackgroundTask stops a single background task of a thread session.
func (m *Manager) StopBackgroundTask(agentID, sessionKey, taskID string) error {
	if _, ok := m.Get(agentID); !ok {
		return ErrAgentNotFound
	}
	cb := m.claudeBackend()
	if cb == nil {
		return ErrBackgroundSessionNotFound
	}
	return cb.stopBackgroundTask(agentID, sessionKey, taskID, func(surface KeyedSessionSurface) {
		// Same thread notice as a whole-session stop (one task), so a stop
		// never happens silently from the thread's point of view.
		go func() {
			m.handleKeyedTasksAbandoned(agentID, sessionKey, 1, KeyedStopRequestedReason, surface)
			if surface != nil {
				surface.Release()
			}
		}()
	})
}

// keyedTurnNote builds the short context note injected at the start of a
// lingering-capable keyed turn: a pending one-time note for this key, plus how
// many OTHER threads of the agent are still running background tasks (and a
// warning when the linger cap is already full).
//
// The one-time note is only peeked: the caller passes the returned oneTime
// value to commitKeyedNote once the backend accepted the turn, so a turn that
// fails to start does not lose it.
func (m *Manager) keyedTurnNote(agentID, sessionKey string, backend ChatBackend) (text, oneTime string) {
	var parts []string
	if note := m.peekKeyedNote(agentID, sessionKey); note != "" {
		oneTime = note
		parts = append(parts, note)
	}
	cb, ok := backend.(*ClaudeBackend)
	if !ok || cb == nil {
		return strings.Join(parts, "\n"), oneTime
	}
	if n := cb.lingeringCount(agentID, sessionKey); n > 0 {
		apiBase := ""
		if m.groupdms != nil {
			apiBase = m.groupdms.APIBase()
		}
		line := fmt.Sprintf("[kojo] 他に裏でバックグラウンドタスク実行中のスレッドが%d件あります。一覧: GET %s/api/v1/agents/%s/background-sessions", n, apiBase, agentID)
		if n >= maxLingeringSessionsPerAgent {
			line += fmt.Sprintf("（待機上限%d件に到達: このターンで run_in_background を使うと、ターン終了時に継続できず停止されます）", maxLingeringSessionsPerAgent)
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "\n"), oneTime
}

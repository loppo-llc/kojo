package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// KeyedBackgroundHandler is implemented by a response surface (the Slack bot,
// the GroupDMManager for WebUI threads) that owns keyed conversations of an
// agent. It receives unsolicited turns
// produced by a keyed lingering claude session — the completion notification
// of a run_in_background task started by an earlier thread turn.
type KeyedBackgroundHandler interface {
	// HandleKeyedBackgroundTurn must consume events until the channel closes.
	// The stream has the same contract as a ChatOneShot result (deltas, then
	// a single terminal done/error); a done carrying BackgroundTasksPending>0
	// means more tasks are still running on the lingering process.
	// cancel aborts the background turn (e.g. a user !stop).
	HandleKeyedBackgroundTurn(agentID, sessionKey string, events <-chan ChatEvent, cancel func())
	// KeyedBackgroundTasksAbandoned reports that the keyed session exited
	// while `pending` background tasks were still running (best-effort
	// notice; their completion will never arrive).
	KeyedBackgroundTasksAbandoned(agentID, sessionKey string, pending int, reason string)
}

// KeyedSessionSurface is a response surface bound to one keyed lingering
// session for its lifetime, taking precedence over the agent-wide handler
// resolution. A remote holder binds one per Hub dispatch so the session's
// background turns and abandoned notices go back to that Hub. Retain/Release
// refcount the surface's credentials (e.g. the Hub file relay): the session
// holds a reference while bound and releases it after its exit notice.
type KeyedSessionSurface interface {
	// HandleKeyedSessionTurn is HandleKeyedBackgroundTurn with the turn's
	// lifecycle context (cancelled by reset / delete / shutdown / handoff
	// quiesce), so the surface can stop waiting on remote I/O promptly.
	HandleKeyedSessionTurn(ctx context.Context, agentID, sessionKey string, events <-chan ChatEvent, cancel func())
	KeyedBackgroundTasksAbandoned(agentID, sessionKey string, pending int, reason string)
	// OriginPeerID is the peer allowed to steer the surface's background
	// turns ("" for none / unsafe local mode).
	OriginPeerID() string
	Retain()
	Release()
}

// keyedBackgroundCtxHandler is implemented by in-package handlers (the
// GroupDMManager) that also want the turn's lifecycle context: it is cancelled
// when the tracked one-shot is cancelled (reset / delete / shutdown), so the
// handler can discard instead of posting a partial reply.
type keyedBackgroundCtxHandler interface {
	handleKeyedBackgroundTurnCtx(ctx context.Context, agentID, sessionKey string, events <-chan ChatEvent, cancel func())
}

type keyedBgRegistration struct {
	id int64
	h  KeyedBackgroundHandler
}

// RegisterKeyedBackgroundHandler installs h as the agent's keyed background
// handler (replacing any previous one) and returns an unregister func that
// only removes this registration.
func (m *Manager) RegisterKeyedBackgroundHandler(agentID string, h KeyedBackgroundHandler) func() {
	m.keyedBgMu.Lock()
	if m.keyedBgHandlers == nil {
		m.keyedBgHandlers = make(map[string]keyedBgRegistration)
	}
	m.keyedBgSeq++
	id := m.keyedBgSeq
	m.keyedBgHandlers[agentID] = keyedBgRegistration{id: id, h: h}
	m.keyedBgMu.Unlock()
	return func() {
		m.keyedBgMu.Lock()
		if cur, ok := m.keyedBgHandlers[agentID]; ok && cur.id == id {
			delete(m.keyedBgHandlers, agentID)
		}
		m.keyedBgMu.Unlock()
	}
}

// isWebUIThreadKey reports whether sessionKey belongs to a WebUI thread room
// (GroupDMManager's "groupdm:<id>" keys).
func isWebUIThreadKey(sessionKey string) bool {
	return strings.HasPrefix(sessionKey, webUIThreadKeyPrefix)
}

const webUIThreadKeyPrefix = "groupdm:"

// keyedBackgroundHandler resolves the response surface owning sessionKey:
// WebUI thread keys belong to the GroupDMManager, everything else to the
// per-agent registered handler (the Slack bot).
func (m *Manager) keyedBackgroundHandler(agentID, sessionKey string) KeyedBackgroundHandler {
	if isWebUIThreadKey(sessionKey) {
		if m.groupdms != nil {
			return m.groupdms
		}
		return nil
	}
	m.keyedBgMu.Lock()
	defer m.keyedBgMu.Unlock()
	return m.keyedBgHandlers[agentID].h
}

func (m *Manager) hasKeyedBackgroundHandler(agentID, sessionKey string) bool {
	return m.keyedBackgroundHandler(agentID, sessionKey) != nil
}

type keyedBgSteer struct {
	id     int64
	fn     SteerFunc
	origin string // the dispatching Hub of a remote thread's turn ("" local)
}

// registerKeyedBgSteer exposes a running keyed background turn to SteerOneShot
// (a WebUI thread steers while ThreadLive is active, a Slack thread while its
// active turn is registered). origin is the turn's dispatching Hub, checked by
// the inter-peer fallback. Returns an identity-guarded unregister.
func (m *Manager) registerKeyedBgSteer(sessionKey, origin string, fn SteerFunc) func() {
	m.keyedBgMu.Lock()
	if m.keyedBgSteers == nil {
		m.keyedBgSteers = make(map[string]keyedBgSteer)
	}
	m.keyedBgSeq++
	id := m.keyedBgSeq
	m.keyedBgSteers[sessionKey] = keyedBgSteer{id: id, fn: fn, origin: origin}
	m.keyedBgMu.Unlock()
	return func() {
		m.keyedBgMu.Lock()
		if cur, ok := m.keyedBgSteers[sessionKey]; ok && cur.id == id {
			delete(m.keyedBgSteers, sessionKey)
		}
		m.keyedBgMu.Unlock()
	}
}

func (m *Manager) keyedBgSteerFor(sessionKey string) SteerFunc {
	m.keyedBgMu.Lock()
	defer m.keyedBgMu.Unlock()
	return m.keyedBgSteers[sessionKey].fn
}

// keyedBgSteerFromOrigin is keyedBgSteerFor for an inter-peer steer: the
// registered turn must have been dispatched by originPeerID, checked against
// the very registration whose closure is returned (a turn from another Hub
// registered after the caller validated a different turn is refused).
func (m *Manager) keyedBgSteerFromOrigin(sessionKey, originPeerID string) SteerFunc {
	m.keyedBgMu.Lock()
	defer m.keyedBgMu.Unlock()
	cur := m.keyedBgSteers[sessionKey]
	if originPeerID == "" || cur.origin != originPeerID {
		return nil
	}
	return cur.fn
}

// handleKeyedBackgroundTurn routes an unsolicited turn from a keyed lingering
// session to the response surface owning the key. It deliberately never touches
// the main transcript, the busy slot, or the broadcaster: the turn belongs to
// the thread. It is tracked as a one-shot so reset/delete/shutdown
// drains (cancelOneShots/waitOneShotClear) see and can cancel it.
func (m *Manager) handleKeyedBackgroundTurn(agentID, sessionKey string, events <-chan ChatEvent, _ AnswerFunc, abort func(), steer SteerFunc, surface KeyedSessionSurface) {
	drain := func() {
		for range events {
		}
	}
	var h KeyedBackgroundHandler
	origin := ""
	if surface != nil {
		origin = surface.OriginPeerID()
	} else {
		h = m.keyedBackgroundHandler(agentID, sessionKey)
	}
	if h == nil && surface == nil {
		m.logger.Info("keyed background turn discarded: no handler", "agent", agentID, "sessionKey", sessionKey)
		drain()
		return
	}
	// lifeCtx is cancelled only by the tracked one-shot's cancel (reset /
	// delete / shutdown / explicit abort), never by normal completion, so the
	// handler can tell a lifecycle cancel from a finished stream.
	lifeCtx, lifeCancel := context.WithCancel(context.Background())
	defer lifeCancel()
	ctx, cancel := context.WithCancel(lifeCtx)
	entryCancel := func() {
		lifeCancel()
		if abort != nil {
			// Async: cancelOneShots callers may hold locks; abort writes to
			// the CLI stdin and is identity-guarded by the session.
			go abort()
		}
	}
	m.busyMu.Lock()
	if m.quiescing || (m.switching != nil && m.switching[agentID]) || m.resetting[agentID] {
		m.busyMu.Unlock()
		cancel()
		m.logger.Info("keyed background turn discarded: agent gated (quiescing/switching/resetting)", "agent", agentID, "sessionKey", sessionKey)
		drain()
		return
	}
	osID := m.trackOneShot(agentID, entryCancel, sessionKey, origin, "")
	m.busyMu.Unlock()
	// Untracked only after the handler has finished posting, so a reset /
	// delete drain (waitOneShotClear) cannot complete while a reply is still
	// being written into the thread.
	defer m.untrackOneShot(agentID, osID)
	if steer != nil {
		// A thread message (WebUI or Slack, local or via the dispatching Hub)
		// sent while the notification turn streams is steered into it: the
		// reply is merged into this turn's post, with no separate reply.
		defer m.registerKeyedBgSteer(sessionKey, origin, steer)()
	}

	var backendCh <-chan ChatEvent = events
	if isSlackConversationKey(agentID, sessionKey) {
		backendCh = filterSlackNoReplyEvents(backendCh)
	}
	out := make(chan ChatEvent, 64)
	go func() {
		defer close(out)
		defer cancel()
		m.processOneShotEvents(ctx, agentID, backendCh, out, true)
	}()
	if surface != nil {
		surface.HandleKeyedSessionTurn(lifeCtx, agentID, sessionKey, out, entryCancel)
	} else if hc, ok := h.(keyedBackgroundCtxHandler); ok {
		hc.handleKeyedBackgroundTurnCtx(lifeCtx, agentID, sessionKey, out, entryCancel)
	} else {
		h.HandleKeyedBackgroundTurn(agentID, sessionKey, out, entryCancel)
	}
	// The handler contract is to consume until close; drain defensively so
	// the relay goroutine (and the session's readLoop behind it) never wedge.
	for range out {
	}
}

// handleKeyedTasksAbandoned forwards a keyed session's abandoned-tasks notice
// to the owning surface and leaves a one-time note for the agent's next turn
// on that key, so the agent learns its tasks never completed (an explicit
// stop requested through the background-sessions API needs no note).
func (m *Manager) handleKeyedTasksAbandoned(agentID, sessionKey string, pending int, reason string, surface KeyedSessionSurface) {
	if reason == keyedUserStopNotedReason {
		// The stop already stored the note (see stopBackgroundSessionReason).
		m.deliverKeyedTasksAbandoned(agentID, sessionKey, pending, KeyedUserStopReason, surface)
		return
	}
	m.noteKeyedTasksAbandoned(agentID, sessionKey, pending, reason)
	m.deliverKeyedTasksAbandoned(agentID, sessionKey, pending, reason, surface)
}

// noteKeyedTasksAbandoned leaves the agent's one-time note for an abandoned
// notice (see handleKeyedTasksAbandoned).
func (m *Manager) noteKeyedTasksAbandoned(agentID, sessionKey string, pending int, reason string) {
	if _, agentLive := m.Get(agentID); !agentLive {
		// No later turn can ever read it; a note here would only leak.
		return
	}
	if note := keyedAbandonedNote(pending, reason); note != "" {
		m.setKeyedNote(agentID, sessionKey, note)
	}
}

// storeKeyedNoteLocked is the backend's under-session-lock note hook: only the
// leaf keyedBgMu is taken (no liveness lookup; a racing deletion re-drops).
func (m *Manager) storeKeyedNoteLocked(agentID, sessionKey string, pending int, reason string) {
	if note := keyedAbandonedNote(pending, reason); note != "" {
		m.setKeyedNote(agentID, sessionKey, note)
	}
}

// keyedAbandonedNote is the agent's one-time note for pending tasks that ended
// for reason, or "" when there is nothing to tell it.
func keyedAbandonedNote(pending int, reason string) string {
	switch {
	case pending <= 0 || reason == KeyedStopRequestedReason:
		// The agent asked for the stop itself: nothing to tell it.
		return ""
	case reason == keyedThreadDeletedReason:
		// No later turn can ever read it (thread room deleted).
		return ""
	case reason == KeyedUserStopReason:
		return fmt.Sprintf(
			"[kojo] このスレッドで実行中だったバックグラウンドタスク%d件は、ユーザーの依頼（!stop all）で停止されました。その結果は届きません。",
			pending)
	default:
		return fmt.Sprintf(
			"[kojo] 前回このスレッドのターン終了後も実行中だったバックグラウンドタスク%d件は、完了前に終了しました（%s）。その結果は届きません。必要なら再実行してください。",
			pending, reason)
	}
}

// deliverKeyedTasksAbandoned posts the abandoned notice to the owning surface
// (or the agent-wide handler).
func (m *Manager) deliverKeyedTasksAbandoned(agentID, sessionKey string, pending int, reason string, surface KeyedSessionSurface) {
	if surface != nil {
		surface.KeyedBackgroundTasksAbandoned(agentID, sessionKey, pending, reason)
		return
	}
	if h := m.keyedBackgroundHandler(agentID, sessionKey); h != nil {
		h.KeyedBackgroundTasksAbandoned(agentID, sessionKey, pending, reason)
	}
}

// SetKeyedBackgroundNote leaves a one-time note for the agent's next turn on
// the key (e.g. a background turn whose result could not be delivered).
func (m *Manager) SetKeyedBackgroundNote(agentID, sessionKey, note string) {
	m.setKeyedNote(agentID, sessionKey, note)
}

// PeekKeyedBackgroundNote returns the pending one-time note for the key
// without consuming it.
func (m *Manager) PeekKeyedBackgroundNote(agentID, sessionKey string) string {
	return m.peekKeyedNote(agentID, sessionKey)
}

// keyedThreadDeletedReason closes a WebUI thread's keyed session when its room
// is deleted.
const keyedThreadDeletedReason = "スレッドが削除されました"

func keyedNoteKey(agentID, sessionKey string) string {
	return agentID + "\x00" + sessionKey
}

// keyedNote is a pending one-time note and when it was left.
type keyedNote struct {
	text string
	at   time.Time
}

// Notes of threads that never get another turn (a Slack thread nobody posts
// in again) would otherwise live forever: they expire, and the map is capped.
const (
	keyedNoteTTL  = 7 * 24 * time.Hour
	maxKeyedNotes = 1024
	// maxKeyedNoteBytes bounds one key's combined pending notes; beyond it
	// the newest note replaces the older ones.
	maxKeyedNoteBytes = 4096
)

func (m *Manager) setKeyedNote(agentID, sessionKey, note string) {
	m.keyedBgMu.Lock()
	defer m.keyedBgMu.Unlock()
	if m.keyedNotes == nil {
		m.keyedNotes = make(map[string]keyedNote)
	}
	now := time.Now()
	k := keyedNoteKey(agentID, sessionKey)
	if prev, ok := m.keyedNotes[k]; ok && prev.text != "" && now.Sub(prev.at) <= keyedNoteTTL &&
		len(prev.text)+1+len(note) <= maxKeyedNoteBytes {
		// Not delivered yet: keep it and add the new one (e.g. an in-turn
		// `!stop all` note followed by the turn-end kill of later tasks).
		note = prev.text + "\n" + note
	}
	m.keyedNotes[k] = keyedNote{text: note, at: now}
	if len(m.keyedNotes) <= maxKeyedNotes/2 {
		return
	}
	oldestK, oldestAt := "", now
	for k, n := range m.keyedNotes {
		if now.Sub(n.at) > keyedNoteTTL {
			delete(m.keyedNotes, k)
		} else if n.at.Before(oldestAt) {
			oldestK, oldestAt = k, n.at
		}
	}
	if len(m.keyedNotes) > maxKeyedNotes && oldestK != "" {
		delete(m.keyedNotes, oldestK)
	}
}

func (m *Manager) peekKeyedNote(agentID, sessionKey string) string {
	m.keyedBgMu.Lock()
	defer m.keyedBgMu.Unlock()
	k := keyedNoteKey(agentID, sessionKey)
	n, ok := m.keyedNotes[k]
	if ok && time.Since(n.at) > keyedNoteTTL {
		delete(m.keyedNotes, k)
		return ""
	}
	return n.text
}

// commitKeyedNote consumes the one-time note delivered to a started turn. A
// note replaced in the meantime (a newer abandoned notice) is kept.
func (m *Manager) commitKeyedNote(agentID, sessionKey, delivered string) {
	if delivered == "" {
		return
	}
	m.keyedBgMu.Lock()
	defer m.keyedBgMu.Unlock()
	k := keyedNoteKey(agentID, sessionKey)
	n := m.keyedNotes[k]
	switch {
	case n.text == delivered:
		delete(m.keyedNotes, k)
	case strings.HasPrefix(n.text, delivered+"\n"):
		// A newer note was appended meanwhile: keep only that part.
		n.text = strings.TrimPrefix(n.text, delivered+"\n")
		m.keyedNotes[k] = n
	}
}

// keyedNoteRedropWait bounds how long a deletion waits for the agent's
// in-flight keyed exit notices before dropping the notes a second time.
var keyedNoteRedropWait = 5 * time.Minute

// dropKeyedNotesAfterExitNotices drops the notes now and once more after the
// agent's in-flight keyed exit notices have run (bounded, in the background):
// an exit notice that captured its close reason before the deletion may still
// store a note after the first drop.
func (m *Manager) dropKeyedNotesAfterExitNotices(agentID, sessionKey string) {
	m.dropKeyedNotes(agentID, sessionKey)
	go func() {
		m.WaitKeyedExitNotices(agentID, keyedNoteRedropWait)
		m.dropKeyedNotes(agentID, sessionKey)
	}()
}

// dropKeyedNotes forgets the one-time notes of an agent (sessionKey "") or of
// one of its keys: their thread (or the whole agent) is gone, so no later
// turn could ever deliver them.
func (m *Manager) dropKeyedNotes(agentID, sessionKey string) {
	m.keyedBgMu.Lock()
	defer m.keyedBgMu.Unlock()
	if sessionKey != "" {
		delete(m.keyedNotes, keyedNoteKey(agentID, sessionKey))
		return
	}
	prefix := agentID + "\x00"
	for k := range m.keyedNotes {
		if strings.HasPrefix(k, prefix) {
			delete(m.keyedNotes, k)
		}
	}
}

// ErrNoKeyedBackgroundSurface: this node has no response surface for the key.
var ErrNoKeyedBackgroundSurface = errors.New("no keyed background surface for the session key")

// RemoteKeyedTurnOpener attaches to a remote holder's buffered keyed
// background turn. responseGroupID/responseMessageID are set for WebUI
// threads so the holder captures response attachments for the reply the Hub
// posts; cancelling ctx aborts the holder's turn.
type RemoteKeyedTurnOpener func(ctx context.Context, responseGroupID, responseMessageID string) (<-chan ChatEvent, error)

// HasKeyedBackgroundSurface reports whether this node (the Hub) owns a
// response surface for sessionKey: a live WebUI thread room of the agent or
// the agent's registered keyed handler (the Slack bot).
func (m *Manager) HasKeyedBackgroundSurface(agentID, sessionKey string) bool {
	if groupID, ok := strings.CutPrefix(sessionKey, webUIThreadKeyPrefix); ok {
		return groupID != "" && m.groupdms != nil && m.groupdms.liveAgentThread(groupID, agentID)
	}
	return m.hasKeyedBackgroundHandler(agentID, sessionKey)
}

// DeliverRemoteKeyedBackgroundTurn runs a remote holder's keyed background
// turn through this node's response surface (the same surfaces and FIFOs a
// Hub-local lingering session uses). It blocks until the turn was posted.
func (m *Manager) DeliverRemoteKeyedBackgroundTurn(agentID, sessionKey string, open RemoteKeyedTurnOpener) error {
	if isWebUIThreadKey(sessionKey) {
		if m.groupdms == nil {
			return ErrNoKeyedBackgroundSurface
		}
		return m.groupdms.deliverRemoteKeyedBackgroundTurn(agentID, sessionKey, open)
	}
	h := m.keyedBackgroundHandler(agentID, sessionKey)
	if h == nil {
		return ErrNoKeyedBackgroundSurface
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := open(ctx, "", "")
	if err != nil {
		return err
	}
	h.HandleKeyedBackgroundTurn(agentID, sessionKey, events, cancel)
	for range events {
	}
	return nil
}

// DeliverRemoteKeyedTasksAbandoned posts a remote holder's abandoned / stop
// notice into this node's response surface for the key.
func (m *Manager) DeliverRemoteKeyedTasksAbandoned(agentID, sessionKey string, pending int, reason string) {
	if h := m.keyedBackgroundHandler(agentID, sessionKey); h != nil {
		h.KeyedBackgroundTasksAbandoned(agentID, sessionKey, pending, reason)
	}
}

// CaptureKeyedBackgroundAttachments wraps a keyed background turn stream
// (holder side of a remote WebUI thread) with holder-local capture of the
// files staged for the thread reply messageID, like a remote thread turn's
// ResponseAttachmentGroupID/MessageID capture.
func (m *Manager) CaptureKeyedBackgroundAttachments(ctx context.Context, agentID, groupID, messageID string, events <-chan ChatEvent) <-chan ChatEvent {
	stageDir := threadAttachmentStageDir(agentID, groupID)
	watcher := m.watchAndStreamAttachmentsFromDir(ctx, agentID, messageID, stageDir)
	return m.captureOneShotResponseAttachments(ctx, agentID, messageID, stageDir, events, watcher)
}

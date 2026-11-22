package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// StopCause says who stopped a turn before it finished on its own. It is
// passed as the cancel cause of the turn context (context.WithCancelCause /
// WithTimeoutCause) so the turn can tell the agent on its next turn whether
// the user or kojo stopped it — the backend transcript says "interrupted by
// user" either way.
type StopCause struct {
	// ByUser is true only for an explicit human stop (WebUI stop button,
	// Slack `!stop`). A user stop leaves no note.
	ByUser bool
	// Reason is a short Japanese reason shown to the agent for kojo stops.
	Reason string
	// Silent suppresses the note: the agent asked for the stop itself.
	Silent bool
}

func (c *StopCause) Error() string {
	switch {
	case c.ByUser:
		return "turn stopped by the user"
	case c.Silent:
		return "turn stopped on the agent's own request"
	default:
		return "turn stopped by kojo: " + c.Reason
	}
}

// UserStop is the cancel cause of an explicit human stop.
func UserStop() error { return &StopCause{ByUser: true} }

// KojoStop is the cancel cause of a stop kojo made for reason.
func KojoStop(reason string) error { return &StopCause{Reason: reason} }

// AgentRequestedStop is the cancel cause of a stop the agent asked for
// itself; it leaves no note.
func AgentRequestedStop() error { return &StopCause{Silent: true} }

// ErrStopNotedByCaller cancels a turn whose caller (the dispatching Hub)
// records the stop note itself.
var ErrStopNotedByCaller = errors.New("turn stop is recorded by the caller")

// ErrTurnStopped wraps ChatOneShot's error for a StopNotedByCaller turn
// stopped on this side before its stream started, so the caller notes it.
var ErrTurnStopped = errors.New("turn stopped before its stream started")

// Default reasons for stops that carry no typed cause.
const (
	stopReasonTimeout  = "制限時間超過"
	stopReasonInternal = "kojo 内部のキャンセル"
	stopReasonCodex    = "codex 側でターンが中断"
	stopReasonRemote   = "実行中のプロセスを kojo が終了"
	stopReasonShutdown = "kojo の停止/再起動"
	stopReasonReset    = "エージェントのリセット"
	stopReasonArchive  = "エージェントのアーカイブ"
	stopReasonUnarch   = "エージェントのアーカイブ解除"
	stopReasonDelete   = "エージェントの削除"
	stopReasonFork     = "エージェントのフォーク"
	stopReasonTruncate = "メモリの切り詰め"
	stopReasonSwitch   = "デバイス移行"
)

// Exported reasons for stops made outside this package.
const (
	StopReasonSwitch   = stopReasonSwitch
	StopReasonShutdown = stopReasonShutdown
)

// stopCauseOf classifies a cancelled context: a typed StopCause wins, a
// deadline is a kojo timeout, anything else defaults to kojo. ok=false means
// no note is wanted (agent-requested, or noted by the caller).
func stopCauseOf(ctx context.Context) (*StopCause, bool) {
	cause := context.Cause(ctx)
	var sc *StopCause
	switch {
	case errors.As(cause, &sc):
		if sc.Silent {
			return nil, false
		}
		if !sc.ByUser && sc.Reason == "" {
			return &StopCause{Reason: stopReasonInternal}, true
		}
		return sc, true
	case errors.Is(cause, ErrStopNotedByCaller):
		return nil, false
	case errors.Is(cause, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &StopCause{Reason: stopReasonTimeout}, true
	default:
		return &StopCause{Reason: stopReasonInternal}, true
	}
}

// turnEnd summarises how a turn's event stream ended.
type turnEnd struct {
	// terminal: a done/error event arrived while the turn context was alive.
	terminal bool
	// errMsg is that terminal's ErrorMessage.
	errMsg string
	// sent (one-shot only): some terminal reached the caller, possibly
	// after a stop (see markStoppedTerminals).
	sent bool
}

// turnStopCause decides whether the turn was stopped before a normal end and
// by whom. ok=false: it ended on its own (or the stop needs no note).
func turnStopCause(ctx context.Context, end turnEnd) (*StopCause, bool) {
	if end.terminal {
		switch end.errMsg {
		case ErrMsgCancelled:
			// Only a peer relay delivers this with our context alive: the
			// holder cancelled the turn on its side.
			return &StopCause{Reason: stopReasonRemote}, true
		case "codex turn interrupted":
			return &StopCause{Reason: stopReasonCodex}, true
		}
		return nil, false
	}
	if ctx.Err() == nil {
		// Stream ended without a terminal while nobody cancelled: a backend
		// failure, not a stop.
		return nil, false
	}
	return stopCauseOf(ctx)
}

// stopNotePrefix starts every stop note (used to keep only the latest one).
const stopNotePrefix = "[kojo] 直前のターンは"

// stopNoteText is the agent's one-time note for a stopped turn.
func stopNoteText(c *StopCause) string {
	if c == nil {
		return ""
	}
	if c.ByUser {
		// The user knows they stopped it: no note. Recording "" still
		// clears an undelivered older kojo note (only the latest stop
		// matters).
		return ""
	}
	reason := c.Reason
	if reason == "" {
		reason = stopReasonInternal
	}
	return stopNotePrefix + "ユーザーではなく kojo が停止しました（理由: " + reason + "）。会話ログに \"interrupted by user\" 等とあってもユーザーの操作ではありません。"
}

// recordKeyedTurnStop leaves the stop note for the next turn on sessionKey
// when the turn ended by a stop.
func (m *Manager) recordKeyedTurnStop(agentID, sessionKey string, ctx context.Context, end turnEnd) {
	m.recordKeyedTurnStopAs(agentID, sessionKey, ctx, end, false)
}

func (m *Manager) recordKeyedTurnStopAs(agentID, sessionKey string, ctx context.Context, end turnEnd, relayed bool) {
	if sessionKey == "" {
		return
	}
	if c, ok := turnStopCause(ctx, end); ok {
		if keyedStopNoteReadable(c) {
			m.setKeyedStopNote(agentID, sessionKey, stopNoteText(c), relayed)
		}
	}
}

// GoalControlOnly: a goal request that runs no model turn (status, pause,
// clear, budget...). Stopping it stops no turn, so it leaves no stop note
// (and must not replace a pending one).
func GoalControlOnly(g *GoalRequest) bool {
	return g != nil && g.Action != "start" && g.Action != "resume"
}

// keyedStopNoteReadable: a deleted thread room has no later turn to read
// its note (same rule as keyedAbandonedNote).
func keyedStopNoteReadable(c *StopCause) bool {
	return c.ByUser || c.Reason != keyedThreadDeletedReason
}

// RecordKeyedStop leaves the stop note for the next turn on sessionKey from
// a cancelled turn context (the Hub side of a relayed turn).
func (m *Manager) RecordKeyedStop(agentID, sessionKey string, ctx context.Context, terminal bool, errMsg string) {
	m.recordKeyedTurnStopAs(agentID, sessionKey, ctx, turnEnd{terminal: terminal, errMsg: errMsg}, true)
}

// RecordKeyedStopCause leaves the stop note for cause (a StopCause; nil or
// an untyped error counts as a kojo stop) for the next turn on sessionKey.
func (m *Manager) RecordKeyedStopCause(agentID, sessionKey string, cause error) {
	if sessionKey == "" {
		return
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	if c, ok := stopCauseOf(ctx); ok {
		if keyedStopNoteReadable(c) {
			m.setKeyedStopNote(agentID, sessionKey, stopNoteText(c), true)
		}
	}
}

// setKeyedStopNote stores a stop note, replacing an undelivered older one:
// only the latest stop matters to the agent.
//
// relayed: recorded by the dispatching Hub, whose agent may run only on
// the holder (not in this Manager): no liveness check.
//
// An empty note (a user stop) only clears the older stop note.
func (m *Manager) setKeyedStopNote(agentID, sessionKey, note string, relayed bool) {
	if _, live := m.Get(agentID); !live && !relayed {
		return
	}
	m.keyedBgMu.Lock()
	defer m.keyedBgMu.Unlock()
	k := keyedNoteKey(agentID, sessionKey)
	if n, ok := m.keyedNotes[k]; ok {
		if kept := withoutStopNotes(n.text); kept != n.text {
			if kept == "" {
				delete(m.keyedNotes, k)
			} else {
				n.text = kept
				m.keyedNotes[k] = n
			}
		}
	}
	if note != "" {
		m.setKeyedNoteLocked(agentID, sessionKey, note)
	}
}

func withoutStopNotes(text string) string {
	if !strings.Contains(text, stopNotePrefix) {
		return text
	}
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if !strings.HasPrefix(l, stopNotePrefix) {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

// Main-chat stop notes live in kv so a kojo restart (the most common kojo
// stop) does not lose them.
const (
	mainStopNoteKVNamespace = "stopnote"
	mainStopNoteKVTimeout   = 2 * time.Second
)

type mainStopNote struct {
	Text string `json:"text"`
	At   int64  `json:"at"`
}

// recordMainTurnStop persists the main chat's stop note when the turn ended
// by a stop.
func (m *Manager) recordMainTurnStop(agentID string, ctx context.Context, end turnEnd) {
	m.noteModelSwitchTurnEnd(agentID, "", 0, ctx, end, nil)
	if c, ok := turnStopCause(ctx, end); ok {
		m.putMainStopNote(agentID, stopNoteText(c))
	}
}

// An empty text (a user stop) drops the undelivered older note instead.
func (m *Manager) putMainStopNote(agentID, text string) {
	st := m.Store()
	if st == nil {
		return
	}
	if _, live := m.Get(agentID); !live {
		return
	}
	if text == "" {
		m.dropMainStopNote(agentID)
		return
	}
	val, err := json.Marshal(mainStopNote{Text: text, At: time.Now().Unix()})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), mainStopNoteKVTimeout)
	defer cancel()
	// Unconditional upsert: the latest stop replaces an undelivered one.
	if _, err := st.PutKV(ctx, &store.KVRecord{
		Namespace: mainStopNoteKVNamespace,
		Key:       agentID,
		Value:     string(val),
		Type:      store.KVTypeJSON,
		Scope:     store.KVScopeMachine,
	}, store.KVPutOptions{}); err != nil {
		m.logger.Debug("persist main-chat stop note", "agent", agentID, "err", err)
	}
}

// peekMainStopNote returns the pending main-chat stop note and its etag
// (for commitMainStopNote), or "" when there is none.
func (m *Manager) peekMainStopNote(agentID string) (text, etag string) {
	st := m.Store()
	if st == nil {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), mainStopNoteKVTimeout)
	defer cancel()
	rec, err := st.GetKV(ctx, mainStopNoteKVNamespace, agentID)
	if err != nil {
		return "", ""
	}
	var n mainStopNote
	if json.Unmarshal([]byte(rec.Value), &n) != nil || n.Text == "" {
		_ = st.DeleteKV(ctx, mainStopNoteKVNamespace, agentID, rec.ETag)
		return "", ""
	}
	if time.Since(time.Unix(n.At, 0)) > keyedNoteTTL {
		_ = st.DeleteKV(ctx, mainStopNoteKVNamespace, agentID, rec.ETag)
		return "", ""
	}
	return n.Text, rec.ETag
}

// commitMainStopNote consumes the delivered note. The etag guard keeps a
// newer note written meanwhile.
func (m *Manager) commitMainStopNote(agentID, etag string) {
	st := m.Store()
	if st == nil || etag == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), mainStopNoteKVTimeout)
	defer cancel()
	if err := st.DeleteKV(ctx, mainStopNoteKVNamespace, agentID, etag); err != nil &&
		!errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrETagMismatch) {
		m.logger.Debug("consume main-chat stop note", "agent", agentID, "err", err)
	}
}

// dropMainStopNote forgets the main chat's pending note (reset / delete /
// release: no later turn here should read it).
func (m *Manager) dropMainStopNote(agentID string) {
	st := m.Store()
	if st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), mainStopNoteKVTimeout)
	defer cancel()
	if err := st.DeleteKV(ctx, mainStopNoteKVNamespace, agentID, ""); err != nil && !errors.Is(err, store.ErrNotFound) {
		m.logger.Debug("drop main-chat stop note", "agent", agentID, "err", err)
	}
}

// PendingMainStopNote returns the main chat's pending stop note (tests and
// diagnostics).
func (m *Manager) PendingMainStopNote(agentID string) string {
	text, _ := m.peekMainStopNote(agentID)
	return text
}

// markMainTurnEnded flags the main chat's busy entry once its event stream
// is over, so a Shutdown during post-turn work leaves no stop note.
func (m *Manager) markMainTurnEnded(agentID string) {
	m.busyMu.Lock()
	if e, ok := m.busy[agentID]; ok {
		e.turnEnded = true
		m.busy[agentID] = e
	}
	m.busyMu.Unlock()
}

// endMainTurnIfLive marks the main chat's turn ended on its terminal event
// unless it was already stopped. Shutdown cancels under the same busyMu, so
// a turn is either stopped (and noted) or ended (and not), never both.
func (m *Manager) endMainTurnIfLive(agentID string, ctx context.Context) bool {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	if ctx.Err() != nil {
		return false
	}
	if e, ok := m.busy[agentID]; ok {
		e.turnEnded = true
		m.busy[agentID] = e
	}
	return true
}

// stop cancels the busy turn with cause (plain cancel for entries without a
// cause-aware cancel).
func (e busyEntry) stop(cause error) {
	if e.cancelCause != nil {
		e.cancelCause(cause)
		return
	}
	if e.cancel != nil {
		e.cancel()
	}
}

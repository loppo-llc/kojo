package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// ErrNotThreadRoom is returned by thread-only operations on a group room.
var ErrNotThreadRoom = errors.New("not a thread room")

// ErrThreadGoalBound is returned by RewindThread while a Codex goal is bound
// to the thread's native session.
var ErrThreadGoalBound = errors.New("a Codex goal is bound to this thread")

// threadFreshSessionNamespace holds one kv row per thread room whose native
// backend session must be discarded before its next turn (value unused).
// Persisted so a daemon restart between a rewind and the next turn cannot
// resume a session that still contains the rewound turns.
const threadFreshSessionNamespace = "groupdm.thread_fresh_session"

// keyedThreadRewoundReason is the close reason for a thread's lingering
// keyed process when the thread is rewound.
const keyedThreadRewoundReason = "スレッドが巻き戻されました"

// threadFreshState is the in-memory mirror of threadFreshSessionNamespace.
// It is authoritative when no store is configured (tests).
type threadFreshState struct {
	mu      sync.Mutex
	pending map[string]bool
}

// ThreadRewindResult reports what RewindThread removed.
type ThreadRewindResult struct {
	MessagesRemoved int64 `json:"messagesRemoved"`
}

// RewindThread rolls a thread room back to just before msgID: the message
// and everything after it leave the room transcript, and the thread's native
// backend session is discarded before the next turn, which starts fresh with
// the (now shorter) transcript injected as history — so the agent no longer
// sees the removed turns, on whichever peer currently holds it.
//
// Refuses with ErrAgentBusy while a thread turn is running or queued, and with
// ErrThreadGoalBound while a Codex goal is bound to the thread's native
// session (a fresh session would be suppressed to keep the goal). No redo.
func (m *GroupDMManager) RewindThread(groupID, msgID string) (*ThreadRewindResult, error) {
	if msgID == "" {
		return nil, fmt.Errorf("%w: message id required", ErrMessageNotFound)
	}
	// m.mu first: user posts append AND reserve the room FIFO under m.mu, so
	// taking the ticket here can never slip between a post's append and its
	// reservation (which would truncate a post that already succeeded).
	m.mu.Lock()
	g, err := m.liveGroupLocked(groupID)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if !isThreadRoom(g) || len(g.Members) != 1 {
		m.mu.Unlock()
		return nil, ErrNotThreadRoom
	}
	// Hold the room's FIFO head for the whole rewind: no thread turn (human
	// or background) can start against the half-rewound state, and the next
	// one observes the fresh-session marker set below.
	ticket, ok := m.threadTurns.TryReserve(groupID)
	if !ok {
		m.mu.Unlock()
		return nil, ErrAgentBusy
	}
	defer ticket.Release()
	agentID := g.Members[0].AgentID
	db := getGlobalStore()
	if db == nil {
		m.mu.Unlock()
		return nil, errStoreNotReady
	}
	// Manager keeps a goal-bound Codex session even under ForceFreshSession,
	// so the rewound turns would survive there. Only visible when this peer
	// holds the session files.
	binding, err := goalBindingFor(agentID, webUIThreadKeyPrefix+groupID)
	if err != nil {
		// The next turn's session reset reads the same ref and would fail.
		m.mu.Unlock()
		return nil, fmt.Errorf("read thread session ref: %w", err)
	}
	if binding != nil && binding.State != nil {
		m.mu.Unlock()
		return nil, ErrThreadGoalBound
	}
	ctx, cancel := transcriptCtx()
	defer cancel()
	// Marker first: if the truncate then fails, the worst case is one
	// needlessly fresh session; the reverse order could leave the removed
	// turns in the native session for good.
	if err := m.setThreadFreshSession(groupID); err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("mark thread session for reset: %w", err)
	}
	res, err := db.TruncateGroupDMMessagesFrom(ctx, groupID, msgID)
	if err != nil {
		m.mu.Unlock()
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrMessageNotFound, msgID)
		}
		return nil, err
	}
	if res.HeadID == "" {
		delete(m.latestMsgID, groupID)
	} else {
		m.latestMsgID[groupID] = res.HeadID
	}
	m.resetThreadAutoTitleLocked(ctx, g, res)
	m.mu.Unlock()

	// A lingering keyed process (pending background tasks) still holds the
	// removed turns and would post their completions into the rewound room.
	// (Remote holders' stale completions are dropped by the marker check in
	// runKeyedBackgroundTurn.)
	if m.agentMgr != nil {
		if cb := m.agentMgr.claudeBackend(); cb != nil {
			cb.CloseKeyedSessionSync(agentID, webUIThreadKeyPrefix+groupID, keyedThreadRewoundReason)
		}
	}
	m.logger.Info("thread rewound", "group", groupID, "agent", agentID, "fromMsg", msgID, "messages", res.Removed)
	return &ThreadRewindResult{MessagesRemoved: res.Removed}, nil
}

// resetThreadAutoTitleLocked restores the default name when the rewind
// removed the whole transcript and the current name is the auto-title derived
// from the removed first message, so the next first message re-titles the
// room. A name the user chose is kept. Caller holds m.mu.
func (m *GroupDMManager) resetThreadAutoTitleLocked(ctx context.Context, g *GroupDM, res *store.GroupDMTruncateResult) {
	if res.HeadID != "" || g.Kind != GroupDMKindThread || g.Name == DefaultThreadName {
		return
	}
	if title := deriveThreadTitle(res.BoundaryContent); title == "" || title != g.Name {
		return
	}
	if db := getGlobalStore(); db != nil {
		if _, err := db.UpdateGroupDM(ctx, g.ID, "", func(r *store.GroupDMRecord) error {
			r.Name = DefaultThreadName
			return nil
		}); err != nil {
			m.logger.Warn("failed to reset thread auto-title after rewind", "group", g.ID, "err", err)
			return
		}
	}
	g.Name = DefaultThreadName
	g.UpdatedAt = time.Now().Format(time.RFC3339)
}

func (m *GroupDMManager) setThreadFreshSession(groupID string) error {
	if db := getGlobalStore(); db != nil {
		ctx, cancel := transcriptCtx()
		defer cancel()
		if _, err := db.PutKV(ctx, &store.KVRecord{
			Namespace: threadFreshSessionNamespace,
			Key:       groupID,
			Value:     "1",
			Type:      store.KVTypeString,
			Scope:     store.KVScopeLocal,
		}, store.KVPutOptions{}); err != nil {
			return err
		}
	}
	m.threadFresh.mu.Lock()
	if m.threadFresh.pending == nil {
		m.threadFresh.pending = make(map[string]bool)
	}
	m.threadFresh.pending[groupID] = true
	m.threadFresh.mu.Unlock()
	return nil
}

// threadFreshSessionPending reports whether groupID's next turn must discard
// its native session (set by RewindThread).
func (m *GroupDMManager) threadFreshSessionPending(groupID string) bool {
	m.threadFresh.mu.Lock()
	pending := m.threadFresh.pending[groupID]
	m.threadFresh.mu.Unlock()
	if pending {
		return true
	}
	db := getGlobalStore()
	if db == nil {
		return false
	}
	ctx, cancel := transcriptCtx()
	defer cancel()
	_, err := db.GetKV(ctx, threadFreshSessionNamespace, groupID)
	if err == nil {
		return true
	}
	if !errors.Is(err, store.ErrNotFound) {
		// Unknown: a needless fresh session is the safe side.
		m.logger.Warn("thread fresh-session marker lookup failed", "group", groupID, "err", err)
		return true
	}
	return false
}

// clearThreadFreshSession drops groupID's marker once a turn has started on
// a fresh native session (or the room is gone).
func (m *GroupDMManager) clearThreadFreshSession(groupID string) {
	if db := getGlobalStore(); db != nil {
		ctx, cancel := transcriptCtx()
		defer cancel()
		if err := db.DeleteKV(ctx, threadFreshSessionNamespace, groupID, ""); err != nil && !errors.Is(err, store.ErrNotFound) {
			// Keep the in-memory marker too: dropping only one copy
			// would make the next turn's answer depend on a restart.
			m.logger.Warn("thread fresh-session marker clear failed", "group", groupID, "err", err)
			return
		}
	}
	m.threadFresh.mu.Lock()
	delete(m.threadFresh.pending, groupID)
	m.threadFresh.mu.Unlock()
}

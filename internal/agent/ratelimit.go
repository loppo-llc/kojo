package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// RateLimitInfo mirrors the "rate_limit_info" payload of the Claude CLI's
// stream-json "rate_limit_event". The CLI emits it mid-turn (observed on
// 2.1.201) whenever usage crosses a reporting threshold.
//
//	status        allowed | allowed_warning | rejected
//	rateLimitType seven_day | five_hour (window the utilization is measured over)
//	resetsAt      unix seconds at which the window resets
//	utilization   0..1 fraction of the window consumed
type RateLimitInfo struct {
	Status             string  `json:"status"`
	RateLimitType      string  `json:"rateLimitType,omitempty"`
	ResetsAt           int64   `json:"resetsAt,omitempty"`
	Utilization        float64 `json:"utilization"`
	IsUsingOverage     bool    `json:"isUsingOverage,omitempty"`
	SurpassedThreshold float64 `json:"surpassedThreshold,omitempty"`
}

// RateLimitSnapshot is a RateLimitInfo stamped with the wall-clock time it
// was observed, so a stale snapshot loaded after a restart can be aged out
// or displayed with an "as of" note.
type RateLimitSnapshot struct {
	RateLimitInfo
	ObservedAt int64 `json:"observedAt"` // unix seconds
}

const (
	rateLimitKVNamespace = "ratelimit"
	rateLimitKVKeyPrefix = "snapshot/"
	rateLimitKVTimeout   = 2 * time.Second
)

func rateLimitKVKey(agentID string) string {
	return rateLimitKVKeyPrefix + agentID
}

// recordRateLimit stores the latest rate-limit snapshot for an agent both
// in memory (fast path for API / volatile-context reads) and in the kv
// table (scope=machine, so it survives a daemon restart on this host). The
// kv write is best-effort: a failure only means the snapshot won't survive
// a restart, which is acceptable for a display-only indicator.
func (m *Manager) recordRateLimit(agentID string, info RateLimitInfo) {
	snap := RateLimitSnapshot{RateLimitInfo: info, ObservedAt: time.Now().Unix()}

	m.rateLimitsMu.Lock()
	if m.rateLimits == nil {
		m.rateLimits = make(map[string]RateLimitSnapshot)
	}
	m.rateLimits[agentID] = snap
	m.rateLimitsMu.Unlock()

	m.persistRateLimit(agentID, snap)
}

func (m *Manager) persistRateLimit(agentID string, snap RateLimitSnapshot) {
	st := m.Store()
	if st == nil {
		return
	}
	val, err := json.Marshal(snap)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), rateLimitKVTimeout)
	defer cancel()
	// Read the existing row (if any) to reuse its etag; the snapshot is
	// single-writer per host (the chat goroutine), so a plain last-wins
	// upsert is fine — no CAS contention to defend against.
	var ifMatch string
	if rec, gerr := st.GetKV(ctx, rateLimitKVNamespace, rateLimitKVKey(agentID)); gerr == nil {
		ifMatch = rec.ETag
	} else if errors.Is(gerr, store.ErrNotFound) {
		ifMatch = store.IfMatchAny
	} else {
		return
	}
	upd := &store.KVRecord{
		Namespace: rateLimitKVNamespace,
		Key:       rateLimitKVKey(agentID),
		Value:     string(val),
		Type:      store.KVTypeJSON,
		Scope:     store.KVScopeMachine,
	}
	if _, err := st.PutKV(ctx, upd, store.KVPutOptions{IfMatchETag: ifMatch}); err != nil {
		m.logger.Debug("persist rate-limit snapshot", "agent", agentID, "err", err)
	}
}

// RateLimit returns the latest rate-limit snapshot for an agent. It checks
// the in-memory cache first, then lazily loads (and caches) the persisted
// snapshot from the kv table so a badge renders correctly after a restart
// before any new turn runs. ok is false when nothing has ever been recorded.
func (m *Manager) RateLimit(agentID string) (RateLimitSnapshot, bool) {
	m.rateLimitsMu.Lock()
	if snap, ok := m.rateLimits[agentID]; ok {
		m.rateLimitsMu.Unlock()
		return snap, true
	}
	m.rateLimitsMu.Unlock()

	snap, ok := m.loadRateLimit(agentID)
	if !ok {
		return RateLimitSnapshot{}, false
	}
	m.rateLimitsMu.Lock()
	if m.rateLimits == nil {
		m.rateLimits = make(map[string]RateLimitSnapshot)
	}
	// Don't clobber a fresher in-memory value that landed while we read kv.
	if existing, present := m.rateLimits[agentID]; present {
		snap = existing
	} else {
		m.rateLimits[agentID] = snap
	}
	m.rateLimitsMu.Unlock()
	return snap, true
}

func (m *Manager) loadRateLimit(agentID string) (RateLimitSnapshot, bool) {
	st := m.Store()
	if st == nil {
		return RateLimitSnapshot{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), rateLimitKVTimeout)
	defer cancel()
	rec, err := st.GetKV(ctx, rateLimitKVNamespace, rateLimitKVKey(agentID))
	if err != nil {
		return RateLimitSnapshot{}, false
	}
	if rec.Type != store.KVTypeJSON || rec.Scope != store.KVScopeMachine {
		return RateLimitSnapshot{}, false
	}
	var snap RateLimitSnapshot
	if err := json.Unmarshal([]byte(rec.Value), &snap); err != nil {
		return RateLimitSnapshot{}, false
	}
	return snap, true
}

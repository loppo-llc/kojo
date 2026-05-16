package peer

// AgentLockGuard is the v1 single-Hub binding of agent_locks (3.5 /
// 3.7 in the design doc): every agent the local binary knows about
// must hold a live lock under this peer's DeviceID for as long as
// the binary is up.
//
// Why this exists right now: the agent_locks table + store API have
// been in place for several slices, but the runtime never wrote a
// row — so a multi-peer cluster would happily have two binaries
// both treating themselves as the holder of agent X. v1 is still
// single-Hub, but landing the lock writes now closes the gap so a
// future inter-peer slice can rely on the row being present.
//
// Scope: this guard takes / refreshes / releases the LOCK rows; it
// does NOT yet thread the fencing token through individual
// agent-runtime writes (transcript append, MEMORY.md update, tool
// side-effects). That requires plumbing the per-agent token into
// every write call site and is the explicit next slice. Until then
// the runtime continues to write without a fencing check; the lock
// row exists so the next slice's CheckFencingTx-gated writes have
// something to validate against.
//
// Lock-loss policy: if a refresh fails (lease was stolen — another
// peer steals after this binary's lease expired), the guard logs a
// loud Error. Tearing down the running agent CLI on lock loss is a
// separate concern handled when fencing-check write paths land.
// Until then a stolen lock is observable via the registry but does
// not stop ongoing writes — which is consistent with v1's "best-
// effort" mode noted in the design.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// AgentLockLeaseDuration is how long each Acquire / Refresh grants
// the lock for. The refresh loop runs at half this cadence so a
// single missed refresh still leaves headroom before lease expiry.
// 5 minutes is enough that brief DB hiccups don't surrender locks
// without holding a stale claim too long after a crash: in the
// worst-case (binary crashes silently without Stop), another peer
// has to wait up to the full lease (5min) before it can steal,
// which is longer than the OfflineSweeper's 150 s
// (5×HeartbeatInterval) "mark offline" threshold but is intentional
// — the offline mark only updates registry liveness; lock ownership
// has stronger correctness needs and warrants a longer holdoff so a
// flaky network doesn't churn locks across peers. A future slice
// can fold the failover speed into a tunable.
const AgentLockLeaseDuration = 5 * time.Minute

// AgentLockRefreshInterval is how often the guard's refresh loop
// runs Refresh on every held lock. Half the lease so a single
// missed refresh still has lease remaining.
const AgentLockRefreshInterval = AgentLockLeaseDuration / 2

// agentLockOpTimeout bounds individual store calls in the guard's
// hot paths so a wedged DB doesn't block the loop indefinitely.
const agentLockOpTimeout = 10 * time.Second

// AgentLockGuard maintains live lock rows for a set of agent IDs.
//
// Concurrency: the guard owns one goroutine for the refresh loop.
// AddAgent / RemoveAgent are safe to call from other goroutines;
// they take the in-memory mutex and either schedule the next Acquire
// or stop refreshing the dropped id.
//
// State:
//   - `desired`: the set of agents we WANT a lock on. AddAgent
//     extends it; RemoveAgent / explicit shutdown shrinks it.
//   - `tokens`: subset of `desired` whose Acquire has succeeded
//     and whose row we are currently refreshing. Entries here come
//     and go as the loop notices loss/regains the lock.
//
// Each refresh tick first refreshes everything in `tokens`, then
// retries Acquire on the difference (desired \ tokens). That covers
// the "first Acquire returned ErrLockHeld" case (another peer was
// holding it; we re-poll until their lease expires and we steal)
// and the "refresh failed and the row vanished" case (next tick
// reseeds).
type AgentLockGuard struct {
	st     *store.Store
	id     *Identity
	logger *slog.Logger

	mu       sync.Mutex
	desired  map[string]struct{} // agent_ids we want a lock for
	tokens   map[string]int64    // agent_id -> fencing_token of the row we hold
	stopCh   chan struct{}
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// NewAgentLockGuard wires the deps. The caller is responsible for
// invoking Start once and Stop at shutdown.
func NewAgentLockGuard(st *store.Store, id *Identity, logger *slog.Logger) *AgentLockGuard {
	return &AgentLockGuard{
		st:      st,
		id:      id,
		logger:  logger,
		desired: make(map[string]struct{}),
		tokens:  make(map[string]int64),
		stopCh:  make(chan struct{}),
	}
}

// Start launches the refresh goroutine and acquires the initial
// batch of locks listed in `agentIDs`. Individual Acquire failures
// are logged and skipped — the binary keeps running without a lock
// for the affected agent (matches the "best-effort" v1 stance).
//
// AcquireFail-class errors:
//   - ErrLockHeld: another live peer holds the lock. Logged Warn;
//     the agent runs locally without owning the row, and the next
//     refresh tick will retry (in case the other peer's lease
//     expires).
//   - any other store error: logged Error.
func (g *AgentLockGuard) Start(ctx context.Context, agentIDs []string) {
	if g == nil || g.st == nil || g.id == nil {
		return
	}
	g.mu.Lock()
	for _, id := range agentIDs {
		if id != "" {
			g.desired[id] = struct{}{}
		}
	}
	g.mu.Unlock()
	for _, id := range agentIDs {
		if id != "" {
			g.acquire(ctx, id)
		}
	}
	g.wg.Add(1)
	go g.refreshLoop()
}

// AddAgent acquires the lock for an agent that was created after
// Start. Idempotent — calling on an already-held id is a no-op.
func (g *AgentLockGuard) AddAgent(ctx context.Context, agentID string) {
	if g == nil || agentID == "" {
		return
	}
	g.mu.Lock()
	g.desired[agentID] = struct{}{}
	_, already := g.tokens[agentID]
	g.mu.Unlock()
	if already {
		return
	}
	g.acquire(ctx, agentID)
}

// RemoveAgent releases the lock for an agent that was deleted or
// archived. Idempotent.
func (g *AgentLockGuard) RemoveAgent(ctx context.Context, agentID string) {
	if g == nil || agentID == "" {
		return
	}
	g.mu.Lock()
	delete(g.desired, agentID)
	token, held := g.tokens[agentID]
	if held {
		delete(g.tokens, agentID)
	}
	g.mu.Unlock()
	if !held {
		return
	}
	relCtx, cancel := context.WithTimeout(context.Background(), agentLockOpTimeout)
	defer cancel()
	if err := g.st.ReleaseAgentLock(relCtx, agentID, g.id.DeviceID, token); err != nil &&
		!errors.Is(err, store.ErrNotFound) &&
		!errors.Is(err, store.ErrFencingMismatch) {
		if g.logger != nil {
			g.logger.Warn("agent_lock: release on RemoveAgent failed",
				"agent", agentID, "err", err)
		}
	}
}

// Token returns the fencing token currently held for agentID, or
// (0, false) when the guard does not hold a lock for it. Will be
// used by the write-fencing slice to thread CheckFencingTx into
// agent-runtime write paths.
func (g *AgentLockGuard) Token(agentID string) (int64, bool) {
	if g == nil {
		return 0, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	tok, ok := g.tokens[agentID]
	return tok, ok
}

// Stop signals the refresh loop to exit and releases every held
// lock via ReleaseAgentLockByPeer. Bounded so shutdown can't block.
// Idempotent.
func (g *AgentLockGuard) Stop() {
	if g == nil {
		return
	}
	g.stopOnce.Do(func() {
		close(g.stopCh)
		g.wg.Wait()
		// Drop every row this peer holds in one swing — saves a
		// per-agent round-trip on shutdown.
		ctx, cancel := context.WithTimeout(context.Background(), agentLockOpTimeout)
		defer cancel()
		if _, err := g.st.ReleaseAgentLockByPeer(ctx, g.id.DeviceID); err != nil {
			if g.logger != nil {
				g.logger.Warn("agent_lock: ReleaseByPeer on shutdown failed",
					"peer", g.id.DeviceID, "err", err)
			}
		}
	})
}

// acquire performs one Acquire call and records the resulting
// fencing token. The caller decides whether to log + continue or
// retry; this helper just centralises the bookkeeping.
func (g *AgentLockGuard) acquire(ctx context.Context, agentID string) {
	opCtx, cancel := context.WithTimeout(ctx, agentLockOpTimeout)
	defer cancel()
	rec, err := g.st.AcquireAgentLock(opCtx, agentID, g.id.DeviceID,
		store.NowMillis(), AgentLockLeaseDuration.Milliseconds())
	if err != nil {
		if errors.Is(err, store.ErrLockHeld) {
			if g.logger != nil && rec != nil {
				g.logger.Warn("agent_lock: held by another peer; retrying on refresh",
					"agent", agentID, "holder", rec.HolderPeer,
					"lease_expires_at", rec.LeaseExpiresAt)
			}
			return
		}
		if g.logger != nil {
			g.logger.Error("agent_lock: acquire failed",
				"agent", agentID, "err", err)
		}
		return
	}
	g.mu.Lock()
	_, stillDesired := g.desired[agentID]
	if stillDesired {
		g.tokens[agentID] = rec.FencingToken
	}
	g.mu.Unlock()
	if !stillDesired {
		// RemoveAgent dropped the desired entry while we were
		// inside the Acquire call. Release the row we just took so
		// another peer (or the next RemoveAgent retry on a future
		// guard) can claim it without waiting for the lease to
		// expire.
		relCtx, relCancel := context.WithTimeout(context.Background(), agentLockOpTimeout)
		defer relCancel()
		if err := g.st.ReleaseAgentLock(relCtx, agentID, g.id.DeviceID, rec.FencingToken); err != nil &&
			!errors.Is(err, store.ErrNotFound) &&
			!errors.Is(err, store.ErrFencingMismatch) && g.logger != nil {
			g.logger.Warn("agent_lock: release-after-stale-acquire failed",
				"agent", agentID, "err", err)
		}
		return
	}
	if g.logger != nil {
		g.logger.Info("agent_lock: acquired",
			"agent", agentID, "token", rec.FencingToken,
			"lease_expires_at", rec.LeaseExpiresAt)
	}
}

func (g *AgentLockGuard) refreshLoop() {
	defer g.wg.Done()
	t := time.NewTicker(AgentLockRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-g.stopCh:
			return
		case <-t.C:
			g.refreshAll()
		}
	}
}

// refreshAll snapshots the held set + their tokens (under mu so
// AddAgent / RemoveAgent can't race the iteration), then issues
// RefreshAgentLock for each. Failed refreshes log loudly and the
// row is dropped from the in-memory set so the next tick treats
// it as fresh (re-Acquire). After refreshing the held set, retries
// Acquire on every desired agent that is NOT currently held — that
// covers the "first Acquire returned ErrLockHeld" case (the prior
// holder's lease eventually expires and we steal) plus the "row
// vanished out from under us" case (drop happened, this tick
// reseeds).
func (g *AgentLockGuard) refreshAll() {
	g.mu.Lock()
	heldSnap := make(map[string]int64, len(g.tokens))
	for id, tok := range g.tokens {
		heldSnap[id] = tok
	}
	missing := make([]string, 0)
	for id := range g.desired {
		if _, ok := g.tokens[id]; !ok {
			missing = append(missing, id)
		}
	}
	g.mu.Unlock()

	for agentID, token := range heldSnap {
		g.refreshOne(agentID, token)
	}
	// Retry Acquire on every desired-but-not-held id. acquire is
	// best-effort and logs Warn on ErrLockHeld; the next tick
	// retries again until the prior holder's lease expires.
	for _, agentID := range missing {
		g.acquire(context.Background(), agentID)
	}
}

func (g *AgentLockGuard) refreshOne(agentID string, token int64) {
	ctx, cancel := context.WithTimeout(context.Background(), agentLockOpTimeout)
	defer cancel()
	if _, err := g.st.RefreshAgentLock(ctx, agentID, g.id.DeviceID, token,
		store.NowMillis(), AgentLockLeaseDuration.Milliseconds()); err != nil {
		// Lock loss — drop from in-memory state so the next
		// AddAgent / external re-acquire can re-claim.
		g.mu.Lock()
		delete(g.tokens, agentID)
		g.mu.Unlock()
		if g.logger != nil {
			level := slog.LevelError
			if errors.Is(err, store.ErrNotFound) {
				// Row was deleted out from under us — common during
				// agent delete; not an error.
				level = slog.LevelInfo
			}
			g.logger.Log(context.Background(), level,
				"agent_lock: refresh failed; dropping from held set",
				"agent", agentID, "token", token, "err", err)
		}
	}
}

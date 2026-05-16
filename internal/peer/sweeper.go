package peer

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// OfflineThreshold is the silence window after which a peer's
// `peer_registry.status` is flipped from 'online' to 'offline' by the
// sweeper. The convention from docs/multi-device-storage.md §3.10 is
// "5 missed heartbeats = expire". 5 × HeartbeatInterval (30s default)
// = 150s.
//
// The comment on Registrar.HeartbeatInterval calls out 3× as the
// "degraded" threshold; that intermediate state is not implemented in
// v1 (peer_registry CHECK accepts 'degraded' but no producer emits
// it). When a "reachable but flaky" detector lands, this constant
// will become the second of two transitions, not the only one.
const OfflineThreshold = 5 * HeartbeatInterval

// SweepInterval is how often the OfflineSweeper checks for stale
// peers. It runs at the heartbeat cadence so a freshly-stale peer is
// detected within one heartbeat tick of crossing the threshold; any
// faster would hammer the DB without value (heartbeats only update
// last_seen at HeartbeatInterval anyway), any slower would let
// "online" rows linger past the documented threshold.
const SweepInterval = HeartbeatInterval

// sweepOpTimeout bounds the UPDATE the sweeper issues. Local SQLite
// is fast; the timeout exists to keep the loop from wedging on a
// pathological lock contention.
const sweepOpTimeout = 10 * time.Second

// OfflineSweeper is the v1 stand-in for the inter-peer WS subscriber
// described in docs/multi-device-storage.md §3.10. Until peers
// cross-subscribe over mTLS-secured WS connections (a follow-up
// slice that requires a peer-trust bootstrap path that v1 doesn't
// yet have), the Hub leans on heartbeat freshness alone:
//
//   - Each peer's Registrar refreshes its self-row's last_seen on a
//     heartbeat tick.
//   - On the Hub, this sweeper periodically scans peer_registry and
//     flips rows whose last_seen is older than OfflineThreshold from
//     'online' to 'offline'.
//
// That gives the cluster eventual consistency on liveness without
// needing peer-to-peer connectivity beyond their connection to the
// Hub. Cross-WS subscription would only narrow the detection window
// from "<=2× HeartbeatInterval" to "near-realtime", which v1 does
// not need.
//
// The sweeper excludes its own peer's row so a stuck sweeper goroutine
// never flips the local self-row offline while the registrar's
// heartbeat is still firing.
//
// Concurrency: identical model to Registrar — one goroutine, sync.Once
// shutdown guard, sync.WaitGroup for graceful drain.
type OfflineSweeper struct {
	st       *store.Store
	id       *Identity
	logger   *slog.Logger
	events   *EventBus // optional pub/sub for status push
	stopCh   chan struct{}
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// NewOfflineSweeper wires the deps. Returns nil-safe sentinels so a
// caller can always Start/Stop without nil-checks.
func NewOfflineSweeper(st *store.Store, id *Identity, logger *slog.Logger) *OfflineSweeper {
	return &OfflineSweeper{
		st:     st,
		id:     id,
		logger: logger,
		stopCh: make(chan struct{}),
	}
}

// SetEventBus wires the optional cross-peer status push channel.
// When non-nil, sweepOnce republishes one "expire" StatusEvent per
// flipped row so WS subscribers learn of the offline transition
// without waiting for their own heartbeat. Safe to call once
// before Start.
func (s *OfflineSweeper) SetEventBus(bus *EventBus) {
	if s == nil {
		return
	}
	s.events = bus
}

// Start launches the sweep goroutine. Returns nil even if the deps
// are nil — the goroutine exits immediately on its first tick — so
// the caller can wire it unconditionally and let the route-guard
// pattern decide whether the Server actually exposes peer endpoints.
func (s *OfflineSweeper) Start() {
	if s == nil {
		return
	}
	s.wg.Add(1)
	go s.loop()
}

// Stop signals the loop to exit and waits for it. Idempotent.
func (s *OfflineSweeper) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
		s.wg.Wait()
	})
}

func (s *OfflineSweeper) loop() {
	defer s.wg.Done()
	t := time.NewTicker(SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			s.sweepOnce()
		}
	}
}

// sweepOnce executes a single MarkStalePeersOffline. Errors are
// logged at Warn (not Error) so a transient DB lock during a
// heartbeat doesn't surface as alerting noise — the next tick will
// retry, and the cluster can tolerate a brief delay in offline
// detection.
func (s *OfflineSweeper) sweepOnce() {
	if s == nil || s.st == nil || s.id == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sweepOpTimeout)
	defer cancel()
	cutoff := store.NowMillis() - OfflineThreshold.Milliseconds()
	// Use the detail variant when an events bus is attached so we
	// can publish one StatusEvent per flipped row; otherwise stick
	// to the cheaper count-only path.
	if s.events == nil {
		n, err := s.st.MarkStalePeersOffline(ctx, cutoff, s.id.DeviceID)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("peer.OfflineSweeper: sweep failed", "err", err)
			}
			return
		}
		if n > 0 && s.logger != nil {
			s.logger.Info("peer.OfflineSweeper: marked stale peers offline", "count", n)
		}
		return
	}
	flipped, err := s.st.MarkStalePeersOfflineDetail(ctx, cutoff, s.id.DeviceID)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("peer.OfflineSweeper: sweep failed", "err", err)
		}
		return
	}
	if len(flipped) == 0 {
		return
	}
	nowMs := store.NowMillis()
	for _, id := range flipped {
		s.events.Publish(StatusEvent{
			DeviceID: id,
			Status:   store.PeerStatusOffline,
			LastSeen: nowMs,
			Op:       StatusOpExpire,
		})
	}
	if s.logger != nil {
		s.logger.Info("peer.OfflineSweeper: marked stale peers offline",
			"count", len(flipped))
	}
}

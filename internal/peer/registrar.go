package peer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// HeartbeatInterval is how often Registrar.heartbeatLoop calls
// TouchPeer to refresh last_seen. The Hub's offline detection (3.7)
// uses 3× this as the "missed heartbeats → degraded" threshold by
// convention; tuning is operator-side via a future env var if
// needed.
const HeartbeatInterval = 30 * time.Second

// shutdownTouchTimeout bounds the synchronous TouchPeer that
// Registrar.Stop emits to mark this peer offline. Kept short so a
// stuck DB doesn't block kojo shutdown — the next peer that lists
// the registry will see the stale last_seen and degrade us anyway.
const shutdownTouchTimeout = 2 * time.Second

// Registrar maintains this peer's row in peer_registry: registers it
// at startup, refreshes last_seen on a heartbeat tick, and marks the
// peer offline at shutdown.
//
// Concurrency: one goroutine owns the heartbeat loop; Stop signals
// shutdown via stopCh and waits on wg. Multiple Stop calls are
// idempotent (sync.Once guard).
type Registrar struct {
	st     *store.Store
	id     *Identity
	logger *slog.Logger
	events *EventBus // optional pub/sub for cross-peer status push

	// publicName overrides id.Name on the peer_registry self-row.
	// Writers: SetPublicName (called from cmd/kojo after tsnet
	// reports its FQDN). Readers: selfName called from
	// heartbeatLoop / Stop / RefreshPublicName. Mutex-protected
	// because the Status() call that drives SetPublicName runs
	// concurrently with the heartbeat goroutine. The `stopped`
	// flag closes the race where a slow retry goroutine still
	// calls RefreshPublicName after Stop has emitted the final
	// offline-touch — without it the post-Stop refresh would
	// flip the row back to online.
	publicMu   sync.RWMutex
	publicName string
	stopped    bool

	stopCh   chan struct{}
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// NewRegistrar wires the deps. The caller MUST call Start before
// Stop; calling Stop on a never-Started Registrar is a no-op.
func NewRegistrar(st *store.Store, id *Identity, logger *slog.Logger) *Registrar {
	return &Registrar{
		st:     st,
		id:     id,
		logger: logger,
		stopCh: make(chan struct{}),
	}
}

// SetEventBus attaches the cross-peer status push bus. Optional;
// when non-nil, each register / heartbeat / shutdown publishes a
// StatusEvent so subscribers receive near-realtime status changes
// without waiting for the OfflineSweeper. Safe to call once
// before Start.
func (r *Registrar) SetEventBus(bus *EventBus) {
	if r == nil {
		return
	}
	r.events = bus
}

// SetPublicName overrides the name written to the peer_registry
// self-row. The default (id.Name = os.Hostname()) is wrong for a
// multi-peer cluster: other peers' Subscriber dials
// `https://<row.Name>` and expects DNS-resolvable. cmd/kojo passes
// the Tailscale FQDN + port (e.g. "bravo.<tailnet>.ts.net:8080")
// here once tsnet has reported its assigned name. Safe to call
// once before Start; empty name falls back to id.Name.
func (r *Registrar) SetPublicName(name string) {
	if r == nil {
		return
	}
	r.publicMu.Lock()
	r.publicName = name
	r.publicMu.Unlock()
}

// RefreshPublicName re-upserts the self-row with the current
// publicName. Called by cmd/kojo AFTER tsnet has reported its
// assigned FQDN — Start runs before tsnet is up, so the initial
// upsert lands with id.Name (the OS hostname) and this method
// corrects it. Idempotent; safe to call repeatedly.
//
// Refuses to run after Stop has been called — without that
// gate, a slow retry goroutine could fire the refresh AFTER
// Stop emitted its final offline-touch and end up flipping
// the row back to online, contradicting the shutdown signal
// other peers already received.
func (r *Registrar) RefreshPublicName(ctx context.Context) error {
	if r == nil || r.st == nil || r.id == nil {
		return errors.New("peer.Registrar.RefreshPublicName: nil deps")
	}
	// Hold publicMu across the DB write so Stop's offline-touch
	// (which takes the same lock) can't interleave between our
	// stopped-check and our UpsertPeer. Without this the
	// online-marking write could land AFTER Stop's
	// offline-marking write, contradicting the shutdown signal.
	r.publicMu.Lock()
	defer r.publicMu.Unlock()
	if r.stopped {
		return errors.New("peer.Registrar.RefreshPublicName: registrar stopped")
	}
	name := r.selfNameLocked()
	nowMs := store.NowMillis()
	rec, err := r.st.UpsertPeer(ctx, &store.PeerRecord{
		DeviceID:  r.id.DeviceID,
		Name:      name,
		PublicKey: r.id.PublicKeyBase64(),
		LastSeen:  nowMs,
		Status:    store.PeerStatusOnline,
	})
	if err != nil {
		return fmt.Errorf("peer.Registrar.RefreshPublicName: upsert: %w", err)
	}
	r.publishStatus(StatusEvent{
		DeviceID: r.id.DeviceID, Name: name,
		Status: store.PeerStatusOnline, LastSeen: nowMs,
		Op: StatusOpUpsert,
	})
	if r.logger != nil {
		r.logger.Info("peer self-row refreshed with public name",
			"device_id", r.id.DeviceID, "name", rec.Name)
	}
	return nil
}

// selfNameLocked returns the row Name to write. Caller MUST
// hold r.publicMu (read or write). Used by RefreshPublicName +
// Stop which hold the write lock for the full DB-write scope.
func (r *Registrar) selfNameLocked() string {
	if r.publicName != "" {
		return r.publicName
	}
	return r.id.Name
}

// selfName returns the row Name to write — publicName when set,
// id.Name otherwise.
func (r *Registrar) selfName() string {
	if r == nil {
		return ""
	}
	r.publicMu.RLock()
	pn := r.publicName
	r.publicMu.RUnlock()
	if pn != "" {
		return pn
	}
	return r.id.Name
}

// Start performs the initial peer_registry upsert (online + last_seen
// = now) and launches the heartbeat goroutine. Returns the upsert
// error verbatim if the row write fails — callers SHOULD log and
// proceed (the binary still works, just won't appear in cross-peer
// listings until the next start).
func (r *Registrar) Start(ctx context.Context) error {
	if r == nil || r.st == nil || r.id == nil {
		return errors.New("peer.Registrar.Start: nil deps")
	}
	nowMs := store.NowMillis()
	if _, err := r.st.UpsertPeer(ctx, &store.PeerRecord{
		DeviceID:  r.id.DeviceID,
		Name:      r.selfName(),
		PublicKey: r.id.PublicKeyBase64(),
		LastSeen:  nowMs,
		Status:    store.PeerStatusOnline,
	}); err != nil {
		return fmt.Errorf("peer.Registrar.Start: upsert: %w", err)
	}
	r.publishStatus(StatusEvent{
		DeviceID: r.id.DeviceID, Name: r.selfName(),
		Status: store.PeerStatusOnline, LastSeen: nowMs,
		Op: StatusOpUpsert,
	})
	if r.logger != nil {
		r.logger.Info("peer registered", "device_id", r.id.DeviceID, "name", r.id.Name)
	}
	r.wg.Add(1)
	go r.heartbeatLoop()
	return nil
}

// Stop signals the heartbeat loop to exit, waits for it, then
// best-effort-marks the peer offline so cross-peer listings reflect
// the shutdown.
//
// Safe to call multiple times (sync.Once); safe to call from a
// signal handler (no blocking I/O on the calling goroutine beyond
// the bounded shutdownTouchTimeout TouchPeer).
func (r *Registrar) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		// Set stopped first (under lock) so any RefreshPublicName
		// that grabs publicMu after this point sees stopped=true
		// and bails before its DB write. Then RELEASE the lock
		// before wg.Wait — heartbeatLoop's tickOnce calls
		// selfName() which takes the same lock as a reader, so
		// holding through wg.Wait would deadlock.
		r.publicMu.Lock()
		r.stopped = true
		r.publicMu.Unlock()
		close(r.stopCh)
		r.wg.Wait()
		// Re-acquire under lock for the offline touch. Any
		// in-flight RefreshPublicName goroutine that took the
		// lock between our Unlock and here will either:
		//   - have started before stopped=true was set → its
		//     UpsertPeer commits, our Lock here waits for it,
		//     then our offline touch lands AFTER. The row ends
		//     up offline as intended.
		//   - have started after stopped=true → hit the early
		//     return without touching the DB.
		r.publicMu.Lock()
		defer r.publicMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTouchTimeout)
		defer cancel()
		nowMs := store.NowMillis()
		if err := r.st.TouchPeer(ctx, r.id.DeviceID, store.PeerStatusOffline, nowMs); err != nil {
			if r.logger != nil {
				r.logger.Warn("peer.Registrar.Stop: touch offline failed",
					"device_id", r.id.DeviceID, "err", err)
			}
			return
		}
		r.publishStatus(StatusEvent{
			DeviceID: r.id.DeviceID, Name: r.selfNameLocked(),
			Status: store.PeerStatusOffline, LastSeen: nowMs,
			Op: StatusOpTouch,
		})
	})
}

// publishStatus is a nil-safe shortcut for r.events.Publish so
// every call site stays a one-liner regardless of whether the
// EventBus is wired.
func (r *Registrar) publishStatus(evt StatusEvent) {
	if r == nil || r.events == nil {
		return
	}
	r.events.Publish(evt)
}

func (r *Registrar) heartbeatLoop() {
	defer r.wg.Done()
	t := time.NewTicker(HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-t.C:
			r.tickOnce()
		}
	}
}

// tickOnce performs a single heartbeat touch and, if the row is
// missing (ErrNotFound), reseeds the self-row via UpsertPeer.
//
// The self-row can disappear between Start and a heartbeat if an
// operator manually deletes it (sqlite3 CLI), or if a peer's
// `DELETE /api/v1/peers/{id}` cross-targets us before the policy
// guard catches it. Without recovery the heartbeat loop would
// silently log Warn forever and the binary would never re-appear in
// cross-peer listings until the next restart.
//
// Reseed uses the same "online + last_seen=now" pattern as Start so
// the row reappears already authoritative for the rest of the
// cluster, not as a fresh offline=0 row that has to be promoted by
// the next heartbeat.
func (r *Registrar) tickOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), kvOpTimeout)
	defer cancel()
	nowMs := store.NowMillis()
	err := r.st.TouchPeer(ctx, r.id.DeviceID, store.PeerStatusOnline, nowMs)
	if err == nil {
		r.publishStatus(StatusEvent{
			DeviceID: r.id.DeviceID, Name: r.selfName(),
			Status: store.PeerStatusOnline, LastSeen: nowMs,
			Op: StatusOpTouch,
		})
		return
	}
	if !errors.Is(err, store.ErrNotFound) {
		// Transient DB lock or similar — log and retry on the next
		// tick. The cluster can tolerate brief stale last_seen.
		if r.logger != nil {
			r.logger.Warn("peer.Registrar: heartbeat touch failed",
				"device_id", r.id.DeviceID, "err", err)
		}
		return
	}
	// Row vanished. Reseed via the same path Start uses so the row
	// reappears at full identity (public_key + name + caps) instead
	// of a header-only stub.
	if _, upErr := r.st.UpsertPeer(ctx, &store.PeerRecord{
		DeviceID:  r.id.DeviceID,
		Name:      r.selfName(),
		PublicKey: r.id.PublicKeyBase64(),
		LastSeen:  nowMs,
		Status:    store.PeerStatusOnline,
	}); upErr != nil {
		if r.logger != nil {
			r.logger.Warn("peer.Registrar: heartbeat reseed failed",
				"device_id", r.id.DeviceID, "err", upErr)
		}
		return
	}
	r.publishStatus(StatusEvent{
		DeviceID: r.id.DeviceID, Name: r.selfName(),
		Status: store.PeerStatusOnline, LastSeen: nowMs,
		Op: StatusOpUpsert,
	})
	if r.logger != nil {
		r.logger.Info("peer.Registrar: self-row reseeded after disappearance",
			"device_id", r.id.DeviceID)
	}
}

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
// TouchPeer to refresh last_seen.
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

	// publicURL is the dial address other peers reach this row on.
	// Writers: SetPublicURL (called from cmd/kojo after tsnet reports
	// its FQDN, or after peer-mode binds its Tailscale IPv4).
	// Readers: selfURL / selfURLLocked, called from heartbeatLoop /
	// Stop / RefreshPublicName. Mutex-protected because the
	// Status() poll that drives SetPublicURL runs concurrently with
	// the heartbeat goroutine. The `stopped` flag closes the race
	// where a slow retry goroutine still calls RefreshPublicName
	// after Stop has emitted the final offline-touch — without it
	// the post-Stop refresh would flip the row back to online.
	publicMu    sync.RWMutex
	publicURL   string
	selfNodeKey string
	stopped     bool

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

// SetPublicURL stamps the dial address other peers reach this row
// on. Other peers' Subscriber dials `https://<row.url>` and expects
// DNS-resolvable. cmd/kojo passes the Tailscale FQDN + port (e.g.
// "bravo.<tailnet>.ts.net:8080") here once tsnet has reported its
// assigned name; --peer hosts pass "http://<ts-ipv4>:<port>". Safe
// to call once before Start; empty leaves the column blank until a
// later call lands.
func (r *Registrar) SetPublicURL(url string) {
	if r == nil {
		return
	}
	r.publicMu.Lock()
	r.publicURL = url
	r.publicMu.Unlock()
}

// ClearSelfNodeKey wipes the self-row's node_key column AND the
// in-memory cache. Used at Hub-mode boot to drop a stale value left
// by a previous binary that stamped tsnet.Server's NodeKey here —
// the post-clear value is then refilled with the host tailscaled
// NodeKey via cmd/kojo's captureOSSelfNodeKeyForRegistrar. Without
// this, hub-info would keep advertising the stale tsnet NodeKey
// (which peers stamp into their local Hub-row but never observe on
// the inbound side, because Hub's outbound HTTP runs through
// http.DefaultTransport not tsnet.Server.Dial), and the §3.7
// inter-peer surface would keep 403-ing forbidden.
func (r *Registrar) ClearSelfNodeKey(ctx context.Context) error {
	if r == nil || r.st == nil || r.id == nil {
		return errors.New("peer.Registrar.ClearSelfNodeKey: nil deps")
	}
	r.publicMu.Lock()
	r.selfNodeKey = ""
	r.publicMu.Unlock()
	if err := r.st.ClearPeerNodeKey(ctx, r.id.DeviceID); err != nil {
		// ErrNotFound is expected when Registrar.Start has not run
		// yet (or the migration just wiped the row); the next Start
		// / refresh will (re)create the row with node_key=NULL.
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("peer.Registrar.ClearSelfNodeKey: %w", err)
	}
	return nil
}

// SetSelfNodeKey records the local Tailscale NodeKey to stamp on the
// self-row's node_key column. cmd/kojo wires this from
// tsnet.LocalClient.Status (Hub) or localTailscale.Status (--peer).
// Empty leaves the column unchanged.
func (r *Registrar) SetSelfNodeKey(nk string) {
	if r == nil {
		return
	}
	r.publicMu.Lock()
	r.selfNodeKey = nk
	r.publicMu.Unlock()
}

// RefreshPublicName re-upserts the self-row with the current
// publicURL. Called by cmd/kojo AFTER tsnet has reported its
// assigned FQDN — Start runs before tsnet is up, so the initial
// upsert lands with an empty URL and this method fills it in.
// Idempotent; safe to call repeatedly.
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
	url := r.selfURLLocked()
	nowMs := store.NowMillis()
	rec, err := r.st.UpsertPeer(ctx, &store.PeerRecord{
		DeviceID: r.id.DeviceID,
		Name:     r.id.Name,
		URL:      url,
		NodeKey:  r.selfNodeKey,
		LastSeen: nowMs,
		Status:   store.PeerStatusOnline,
	})
	if err != nil {
		return fmt.Errorf("peer.Registrar.RefreshPublicName: upsert: %w", err)
	}
	r.publishStatus(r.selfStatusEvent(url, store.PeerStatusOnline, nowMs, StatusOpUpsert))
	if r.logger != nil {
		r.logger.Info("peer self-row refreshed with public URL",
			"device_id", r.id.DeviceID, "name", rec.Name, "url", rec.URL)
	}
	return nil
}

// selfURLLocked returns the URL column value to write. Caller
// MUST hold r.publicMu (read or write). Empty when SetPublicURL
// has not been called — the row's url column lands blank until
// the listener finishes binding.
func (r *Registrar) selfURLLocked() string {
	return r.publicURL
}

// selfURL returns the URL column value to write.
func (r *Registrar) selfURL() string {
	if r == nil {
		return ""
	}
	r.publicMu.RLock()
	defer r.publicMu.RUnlock()
	return r.publicURL
}

// selfIdentity returns the URL + NodeKey columns to write, read under
// a single RLock so the two stay consistent. Start and tickOnce's
// reseed path both stamp NodeKey via this helper so their UpsertPeer
// records carry the same identity columns RefreshPublicName writes —
// otherwise a reseed after the row vanished would drop node_key and
// re-quarantine the self-row until the next RefreshPublicName lands.
//
// NodeKey is legitimately empty at Start (tsnet/tailscaled hasn't
// reported its NodeKey yet; SetSelfNodeKey runs later once Status
// resolves). UpsertPeer treats an empty node_key as "no change", so
// stamping the empty value at Start is a safe no-op on the column,
// while a later reseed — after SetSelfNodeKey has populated it —
// correctly restamps the real NodeKey.
func (r *Registrar) selfIdentity() (url, nodeKey string) {
	if r == nil {
		return "", ""
	}
	r.publicMu.RLock()
	defer r.publicMu.RUnlock()
	return r.publicURL, r.selfNodeKey
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
	url, nodeKey := r.selfIdentity()
	if _, err := r.st.UpsertPeer(ctx, &store.PeerRecord{
		DeviceID: r.id.DeviceID,
		Name:     r.id.Name,
		URL:      url,
		NodeKey:  nodeKey,
		LastSeen: nowMs,
		Status:   store.PeerStatusOnline,
	}); err != nil {
		return fmt.Errorf("peer.Registrar.Start: upsert: %w", err)
	}
	r.publishStatus(r.selfStatusEvent(url, store.PeerStatusOnline, nowMs, StatusOpUpsert))
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
		// selfURL() which takes the same lock as a reader, so
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
		r.publishStatus(r.selfStatusEvent(r.selfURLLocked(), store.PeerStatusOffline, nowMs, StatusOpTouch))
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

// selfStatusEvent builds the StatusEvent this Registrar publishes
// for register / heartbeat / shutdown transitions. url is passed in
// by the caller rather than resolved here because callers differ in
// which accessor is safe to use at their call site: Stop already
// holds publicMu for writing when it builds its event, so it must
// pass the pre-locked selfURLLocked() value; other callers pass the
// RLock-taking selfURL().
func (r *Registrar) selfStatusEvent(url, status string, lastSeen int64, op string) StatusEvent {
	return StatusEvent{
		DeviceID: r.id.DeviceID, Name: r.id.Name, URL: url,
		Status: status, LastSeen: lastSeen,
		Op: op,
	}
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
		r.publishStatus(r.selfStatusEvent(r.selfURL(), store.PeerStatusOnline, nowMs, StatusOpTouch))
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
	// reappears at full identity (name + url + node_key) instead of a
	// header-only stub. Stamping node_key here matters: the reseed
	// fires precisely because the row was deleted, so unlike Start the
	// self-row's node_key is gone and a NodeKey-less reseed would leave
	// the row quarantined (inbound peer requests 403) until the next
	// RefreshPublicName. By this point SetSelfNodeKey has usually run,
	// so selfIdentity carries the real NodeKey.
	url, nodeKey := r.selfIdentity()
	if _, upErr := r.st.UpsertPeer(ctx, &store.PeerRecord{
		DeviceID: r.id.DeviceID,
		Name:     r.id.Name,
		URL:      url,
		NodeKey:  nodeKey,
		LastSeen: nowMs,
		Status:   store.PeerStatusOnline,
	}); upErr != nil {
		if r.logger != nil {
			r.logger.Warn("peer.Registrar: heartbeat reseed failed",
				"device_id", r.id.DeviceID, "err", upErr)
		}
		return
	}
	r.publishStatus(r.selfStatusEvent(url, store.PeerStatusOnline, nowMs, StatusOpUpsert))
	if r.logger != nil {
		r.logger.Info("peer.Registrar: self-row reseeded after disappearance",
			"device_id", r.id.DeviceID)
	}
}

package peer

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// docs/multi-device-storage.md §3.10 calls for cross-peer
// subscription to other peers' status feeds so a node observes a
// remote peer's disappearance via TCP disconnect rather than
// waiting on heartbeat aging. The Subscriber type maintains one
// reconnecting WS connection per remote peer; on receipt of a
// StatusEvent it updates its local cache so a query against
// LiveStatus returns the freshest view available.
//
// v1 deployment shape: in a single-peer cluster the Subscriber has
// no remote targets and the run loop sits idle. In a multi-peer
// cluster the operator's peer_registry rows drive the target list;
// the Subscriber polls peer_registry periodically and reconciles.
//
// Conflict with OfflineSweeper: the Subscriber's LiveStatus
// reports what the WS feed last said. OfflineSweeper writes
// peer_registry.status based on Hub-side last_seen. The two are
// independent views — a future slice may have the Hub consult
// LiveStatus as a "we just observed this peer over WS within the
// last N seconds, don't flip it offline yet" hint.

// SubscriberTarget identifies one peer the Subscriber should
// connect to. Address is the base URL (e.g.
// "https://peer-b.tail-net.ts.net:8443") and DeviceID is the
// remote peer's identity (used to verify the peer_registry row's
// public_key matches what the Hub knows).
type SubscriberTarget struct {
	DeviceID string
	Address  string
}

// Subscriber maintains one connect-and-reconnect loop per target
// peer. Local Identity (DeviceID + PrivateKey) signs every
// outbound WS upgrade so the remote peer's AuthMiddleware
// authenticates us as RolePeer.
type Subscriber struct {
	id     *Identity
	logger *slog.Logger
	// bus is the LOCAL pub/sub the Subscriber would forward
	// remote events into. In v1 we keep it wired but NEVER
	// re-publish remote events here — A↔B mutual subscriptions
	// would otherwise reflect the same status_event back and
	// forth forever (no origin/hop field to suppress the loop).
	// In-process consumers consult Subscriber.LiveStatus
	// instead; the bus is retained on the struct for future
	// origin-tagging.
	bus *EventBus

	mu      sync.RWMutex
	live    map[string]map[string]StatusEvent // target → (deviceID → event)
	targets map[string]*subTarget
	stopped bool

	stopCh   chan struct{}
	wg       sync.WaitGroup
	stopOnce sync.Once

	// httpClient is used for the WS upgrade. Production
	// NewSubscriber wires NoKeepAliveHTTPClient(10s) so each
	// signed upgrade GET runs over a fresh TCP/TLS handshake —
	// see NewSubscriber for the stale-conn-retry-replays-nonce
	// rationale. Tests can overwrite the field with their own
	// transport-injecting client.
	httpClient *http.Client
}

// subTarget tracks one running per-target connect loop. We keep
// the current Address alongside the cancel so SetTargets can
// detect an address change for the SAME DeviceID and tear down +
// restart instead of silently sticking with the stale URL.
type subTarget struct {
	address string
	cancel  context.CancelFunc
}

// NewSubscriber wires the deps. The local EventBus (optional) lets
// the in-process Hub-side handler re-broadcast events the
// Subscriber observes — useful in multi-peer setups where a peer
// learning of a status change should propagate it back through
// its own /api/v1/peers/events WS to other subscribers.
func NewSubscriber(id *Identity, bus *EventBus, logger *slog.Logger) *Subscriber {
	return &Subscriber{
		id:      id,
		logger:  logger,
		bus:     bus,
		live:    make(map[string]map[string]StatusEvent),
		targets: make(map[string]*subTarget),
		stopCh:  make(chan struct{}),
		// Disable keep-alives on the WS upgrade GET. The
		// upgrade carries an Ed25519-signed Authorization
		// header with a single-use nonce; Go's http.Transport
		// will silently retry an idempotent GET when a reused
		// idle connection EOFs before any response bytes,
		// resending the SAME nonce → the recipient's peer auth
		// middleware rejects the retry with HTTP 401 "replayed
		// nonce". Each WS dial gets a fresh TCP/TLS handshake;
		// the WS connection itself is then long-lived as
		// usual.
		//
		// 10s upgrade ceiling — generous enough for tsnet's
		// LetsEncrypt cert fetch on first contact, tight
		// enough that a dead target fails fast and the outer
		// connect loop retries.
		httpClient: NoKeepAliveHTTPClient(10 * time.Second),
	}
}

// SetTargets reconciles the connect set: any target in `targets`
// that doesn't have a goroutine yet gets one started; any
// running goroutine whose target is no longer in `targets` is
// cancelled. Concurrent-safe. No-op after Stop has been called.
func (s *Subscriber) SetTargets(targets []SubscriberTarget) {
	if s == nil || s.id == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	wanted := make(map[string]SubscriberTarget, len(targets))
	for _, t := range targets {
		if t.DeviceID == "" || t.Address == "" {
			continue
		}
		if t.DeviceID == s.id.DeviceID {
			continue // never self-subscribe
		}
		wanted[t.DeviceID] = t
	}
	// Cancel goroutines for targets that are gone OR whose
	// address has changed. Address-change is the same operation
	// as removal: we tear down the old connection and start a
	// new one against the fresh URL.
	for id, st := range s.targets {
		want, keep := wanted[id]
		if !keep || want.Address != st.address {
			st.cancel()
			delete(s.targets, id)
		}
	}
	// Drop live entries for every target NOT in the wanted set,
	// regardless of whether we had a goroutine for it. Without
	// this, a target that was reconciled away (or pushed events
	// before its goroutine was started, e.g. in tests) leaves
	// stale online entries in LiveStatus forever.
	for tgt := range s.live {
		if _, keep := wanted[tgt]; !keep {
			delete(s.live, tgt)
		}
	}
	// Start goroutines for new targets.
	for id, t := range wanted {
		if _, running := s.targets[id]; running {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		s.targets[id] = &subTarget{address: t.Address, cancel: cancel}
		s.wg.Add(1)
		go s.runOne(ctx, t)
	}
}

// LiveStatus returns the most recent StatusEvent received for
// deviceID across any target's feed. When the same deviceID
// appears in multiple targets the freshest LastSeen wins.
// Returns (zero-value, false) when never observed.
func (s *Subscriber) LiveStatus(deviceID string) (StatusEvent, bool) {
	if s == nil {
		return StatusEvent{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var (
		best     StatusEvent
		bestSeen bool
	)
	for _, perTarget := range s.live {
		evt, ok := perTarget[deviceID]
		if !ok {
			continue
		}
		if !bestSeen || evt.LastSeen > best.LastSeen {
			best = evt
			bestSeen = true
		}
	}
	return best, bestSeen
}

// Stop signals every per-target loop to exit and waits for them.
// Idempotent.
func (s *Subscriber) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		for _, st := range s.targets {
			st.cancel()
		}
		// Clear the map but keep the field non-nil so a
		// concurrent SetTargets (which the stopped flag now
		// short-circuits) doesn't crash on a nil map write
		// during the race window between setting stopped and
		// re-checking.
		s.targets = make(map[string]*subTarget)
		s.live = make(map[string]map[string]StatusEvent)
		s.mu.Unlock()
		close(s.stopCh)
		s.wg.Wait()
	})
}

// runOne is the per-target reconnect loop. Backoff is bounded and
// jittered so a cluster-wide flap doesn't cause a synchronised
// reconnect storm.
func (s *Subscriber) runOne(ctx context.Context, t SubscriberTarget) {
	defer s.wg.Done()
	const minBackoff = 1 * time.Second
	const maxBackoff = 60 * time.Second
	backoff := minBackoff
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		default:
		}
		err := s.connectOnce(ctx, t)
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil && s.logger != nil {
			s.logger.Warn("peer.Subscriber: connection dropped",
				"target", t.DeviceID, "addr", t.Address,
				"backoff", backoff.String(), "err", err)
		}
		// Backoff with jitter (±25%).
		jittered := jitter(backoff)
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-time.After(jittered):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// connectOnce performs a single WS dial + read loop. Returns when
// the connection drops (or the ctx fires); the caller wraps the
// call in the reconnect loop.
func (s *Subscriber) connectOnce(ctx context.Context, t SubscriberTarget) error {
	target, err := url.Parse(t.Address)
	if err != nil {
		return fmt.Errorf("parse target url: %w", err)
	}
	// Convert https → wss / http → ws so the dial chooses TLS
	// correctly. The peer-auth headers are added below.
	switch target.Scheme {
	case "http":
		target.Scheme = "ws"
	case "https":
		target.Scheme = "wss"
	}
	target.Path = "/api/v1/peers/events"

	// Build the upgrade request to attach peer-auth headers.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	nonce, err := newNonce()
	if err != nil {
		return fmt.Errorf("nonce: %w", err)
	}
	if err := SignRequest(req, s.id.DeviceID, s.id.PrivateKey, nonce, t.DeviceID); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	dialOpts := &websocket.DialOptions{
		HTTPClient: s.httpClient, // nil OK; websocket uses http.DefaultClient
		HTTPHeader: req.Header,
	}
	conn, _, err := websocket.Dial(ctx, target.String(), dialOpts)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Read frames forever. Each frame is either {type: "snapshot",
	// peers: [...]} or {type: "event", event: {...}}.
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		s.handleFrame(t.DeviceID, data)
	}
}

// handleFrame decodes one WS frame and updates the live cache
// keyed by the target whose feed delivered it. Unknown frame
// types are ignored (forward-compat).
func (s *Subscriber) handleFrame(target string, data []byte) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		if s.logger != nil {
			s.logger.Warn("peer.Subscriber: malformed frame",
				"err", err, "preview", previewBytes(data, 80))
		}
		return
	}
	switch probe.Type {
	case "snapshot":
		var f struct {
			Peers []StatusEvent `json:"peers"`
		}
		if err := json.Unmarshal(data, &f); err != nil {
			return
		}
		s.mu.Lock()
		// Snapshot REPLACES the target's contribution to the
		// live view — any device the remote no longer lists is
		// dropped. This is what makes LiveStatus reflect "what
		// this peer believes right now" rather than the union
		// of every event we've ever seen.
		fresh := make(map[string]StatusEvent, len(f.Peers))
		for _, evt := range f.Peers {
			fresh[evt.DeviceID] = evt
		}
		s.live[target] = fresh
		s.mu.Unlock()
		for _, evt := range f.Peers {
			s.republish(evt)
		}
	case "event":
		var f struct {
			Event StatusEvent `json:"event"`
		}
		if err := json.Unmarshal(data, &f); err != nil {
			return
		}
		s.mu.Lock()
		perTarget, ok := s.live[target]
		if !ok {
			perTarget = make(map[string]StatusEvent)
			s.live[target] = perTarget
		}
		perTarget[f.Event.DeviceID] = f.Event
		s.mu.Unlock()
		s.republish(f.Event)
	}
}

// republish is intentionally a NO-OP in v1.
//
// An earlier iteration forwarded observed events into s.bus so
// in-process consumers (e.g. a deconflict layer with
// OfflineSweeper) could see remote-observed status changes
// alongside locally-generated ones. With mutual subscriptions
// (A subscribed to B, B subscribed to A) that creates a
// reflection loop: A's bus emits → B's subscriber picks it up
// → B's bus emits → A's subscriber picks it up → A's bus emits
// the same event again, ad infinitum. The status_event payload
// has no origin / event_id / hop field to break the loop.
//
// In-process consumers can query Subscriber.LiveStatus instead;
// the bus is retained on the struct (and the function kept as
// a deliberate no-op) for a future slice that adds origin
// tagging.
func (s *Subscriber) republish(evt StatusEvent) {
	_ = evt
}

// newNonce returns a fresh 32-byte base64 nonce for use in
// AuthHeaderNonce. The dedicated helper lives here so the
// subscriber doesn't have to import crypto/rand at every call
// site.
func newNonce() (string, error) {
	var b [AuthNonceLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b[:]), nil
}

// jitter applies ±25% to d. Helps avoid synchronised reconnect
// storms when many peers come back at once.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return d
	}
	r := int64(b[0])<<56 | int64(b[1])<<48 | int64(b[2])<<40 | int64(b[3])<<32 |
		int64(b[4])<<24 | int64(b[5])<<16 | int64(b[6])<<8 | int64(b[7])
	if r < 0 {
		r = -r
	}
	pct := r%50 - 25 // -25..+24
	delta := d * time.Duration(pct) / 100
	return d + delta
}

func previewBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(" + fmt.Sprint(len(b)) + " bytes total)"
}


package peer

import "sync"

// Presence tracks which remote peers currently hold an active
// peer-events WebSocket against this daemon. The peer_registry
// last_seen / status columns reflect a 5-missed-heartbeat sampling
// view — fine in steady state, brittle on a flaky mobile uplink
// where a single missed 15s touch can push last_seen past the
// 150s OfflineThreshold and cause the OfflineSweeper to flip a row
// offline even though the WS connection is alive.
//
// Presence is the second source of truth: if at least one WS for
// a deviceID is currently held by this process, the peer is
// observably reachable RIGHT NOW regardless of what the sampled
// last_seen says. OfflineSweeper consults Presence.IsActive and
// skips any peer with a live connection, so a sweep tick can
// never demote an actively-connected peer.
//
// Concurrency: a multi-set semantics on top of a single mutex.
// AddConn returns a release closure so handlers can `defer` the
// removal without tracking a per-call key. The same deviceID may
// hold multiple simultaneous connections (mobile reconnect race,
// dual-stack DNS, retries) — the counter only drops to 0 when
// every connection has been released.
type Presence struct {
	mu    sync.RWMutex
	conns map[string]int
}

// NewPresence returns an empty presence map ready for use.
func NewPresence() *Presence {
	return &Presence{conns: make(map[string]int)}
}

// AddConn marks deviceID as actively connected. Returns a release
// closure the caller MUST defer to drop the count when the
// connection ends. Empty deviceID is a no-op so call sites don't
// have to special-case anonymous peers. The release closure is
// idempotent (sync.Once) so a caller can wire it both into a defer
// AND an explicit early-cleanup path without double-decrementing
// the count from a parallel goroutine.
func (p *Presence) AddConn(deviceID string) func() {
	if p == nil || deviceID == "" {
		return func() {}
	}
	p.mu.Lock()
	p.conns[deviceID]++
	p.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			n := p.conns[deviceID] - 1
			if n <= 0 {
				delete(p.conns, deviceID)
			} else {
				p.conns[deviceID] = n
			}
			p.mu.Unlock()
		})
	}
}

// IsActive reports whether at least one connection is currently
// held for deviceID. Nil-safe: a nil Presence returns false so
// the OfflineSweeper degrades gracefully to last_seen-only when
// the binary wasn't wired with a presence set (e.g. unit tests).
func (p *Presence) IsActive(deviceID string) bool {
	if p == nil || deviceID == "" {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.conns[deviceID] > 0
}

// ActiveDeviceIDs returns a snapshot of every deviceID with at
// least one active connection. Intended for diagnostics /
// /api/v1/peers debug endpoints; the OfflineSweeper hot path uses
// IsActive instead to avoid allocating a slice per sweep tick.
func (p *Presence) ActiveDeviceIDs() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.conns) == 0 {
		return nil
	}
	out := make([]string, 0, len(p.conns))
	for id := range p.conns {
		out = append(out, id)
	}
	return out
}

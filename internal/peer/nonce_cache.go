package peer

import (
	"sync"
	"time"
)

// NonceCache is a fixed-window de-duplicator for peer-auth nonces.
// docs/multi-device-storage.md §3.10's two cross-peer flows are
// both low-volume (status WS connect once, blob handoff fires once
// per device-switch), so a simple map + lazy-expiry sweep
// outperforms a TTL-bucketed ring buffer at this scale.
//
// Concurrency: a single sync.Mutex serializes Seen / sweep. The
// hot path is one lookup-and-insert per request; the sweep walks
// every entry but only runs every sweepInterval and only on
// goroutines that observe the interval has elapsed (no separate
// timer goroutine — fewer moving parts).
//
// Cache key: device_id || ":" || nonce. Globally unique within
// the window because nonces are 256 random bits per (device_id,
// time-bucket).
//
// Retention: AuthMaxClockSkew + sweepGracePeriod. The grace
// period avoids a TOCTOU where a request whose timestamp is at
// the very edge of the skew window arrives, gets accepted, and
// the sweep purges the nonce before a second-instance replay
// would also reach Seen — leaving the duplicate undetected.
type NonceCache struct {
	mu             sync.Mutex
	entries        map[string]int64 // key → expires_at (unix ms)
	now            func() time.Time
	maxAge         time.Duration
	lastSweep      time.Time
	sweepInterval  time.Duration
}

// NewNonceCache returns a fresh cache. maxAge is how long a nonce
// stays remembered after Seen accepts it; pass AuthMaxClockSkew in
// production. Zero / negative maxAge defaults to AuthMaxClockSkew.
func NewNonceCache(maxAge time.Duration) *NonceCache {
	if maxAge <= 0 {
		maxAge = AuthMaxClockSkew
	}
	return &NonceCache{
		entries:       make(map[string]int64),
		now:           time.Now,
		maxAge:        maxAge,
		sweepInterval: maxAge, // sweep on the same cadence as the TTL
	}
}

// Seen records the (deviceID, nonce) pair and returns true if it
// was already present (i.e. this is a replay). The check + insert
// is atomic under the mutex.
//
// DEPRECATED for the AuthMiddleware hot path — use Probe + Commit
// instead so a bogus signature presented BEFORE the genuine
// signer can't consume the nonce and DoS the real request out.
// Retained for callers that need the simpler "check + remember
// in one step" shape (tests, future non-auth uses).
func (c *NonceCache) Seen(deviceID, nonce string) bool {
	if c == nil || deviceID == "" || nonce == "" {
		return false
	}
	key := deviceID + ":" + nonce
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()
	if _, dup := c.entries[key]; dup {
		return true
	}
	expiresAt := c.now().Add(c.maxAge).UnixMilli()
	c.entries[key] = expiresAt
	return false
}

// Probe returns true if the (deviceID, nonce) pair is already
// recorded WITHOUT inserting it. Used by AuthMiddleware as the
// "is this a replay?" check; the matching Commit happens only
// after signature verification succeeds. The split keeps an
// attacker who can guess a victim's nonce in flight (but not
// the signature) from causing the genuine request to be
// rejected — the bogus request fails Verify and never reaches
// Commit, so the victim's nonce stays available.
func (c *NonceCache) Probe(deviceID, nonce string) bool {
	if c == nil || deviceID == "" || nonce == "" {
		return false
	}
	key := deviceID + ":" + nonce
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()
	_, dup := c.entries[key]
	return dup
}

// Commit records the (deviceID, nonce) pair after signature
// verification has succeeded. tsMs is the request's timestamp
// (Unix ms); the entry expires at tsMs + 2×maxAge — covering
// both clock-skew directions. Returns true if a concurrent
// Commit landed first (an authenticated replay slipped through
// the Probe window) — the caller should treat that as the
// replay it is and refuse the request.
//
// Why expire at ts + 2×maxAge instead of accept_time + maxAge:
// a sender whose clock leads the receiver's by maxAge would
// have its nonce expire from the cache well before the
// request's timestamp falls out of CheckTimestamp's accept
// window. The 2× window guarantees an accepted request's
// nonce stays remembered for the full duration the timestamp
// gate could re-admit it.
func (c *NonceCache) Commit(deviceID, nonce string, tsMs int64) bool {
	if c == nil || deviceID == "" || nonce == "" {
		return false
	}
	key := deviceID + ":" + nonce
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()
	if _, dup := c.entries[key]; dup {
		return true
	}
	// Expiry is the later of {ts + 2×maxAge, now + maxAge} so a
	// caller passing tsMs=0 still gets the legacy retention.
	expiry := tsMs + 2*c.maxAge.Milliseconds()
	if floor := c.now().Add(c.maxAge).UnixMilli(); expiry < floor {
		expiry = floor
	}
	c.entries[key] = expiry
	return false
}

// Size returns the current entry count. Exported for tests +
// telemetry; production code shouldn't need it.
func (c *NonceCache) Size() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()
	return len(c.entries)
}

// sweepLocked drops expired entries. Caller must hold c.mu.
//
// Skips entirely if less than sweepInterval has elapsed since the
// last sweep — keeps the hot path's overhead at a single map
// lookup for the common case (no entries to drop).
func (c *NonceCache) sweepLocked() {
	now := c.now()
	if !c.lastSweep.IsZero() && now.Sub(c.lastSweep) < c.sweepInterval {
		return
	}
	c.lastSweep = now
	nowMs := now.UnixMilli()
	for k, exp := range c.entries {
		if exp <= nowMs {
			delete(c.entries, k)
		}
	}
}

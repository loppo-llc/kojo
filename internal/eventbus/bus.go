// Package eventbus implements the in-memory invalidation broadcast bus
// described in docs/multi-device-storage.md §3.5 / §4.1.
//
// The Hub publishes one Event per successful write (insert/update/delete
// on a domain table) so subscribed peers can drop the matching cache
// row and re-fetch. Events are best-effort: a slow subscriber whose
// buffer overflows is dropped, and the peer is expected to reconcile
// via the resync cursor (`GET /api/v1/changes?since=<seq>`, future
// slice). This is the design call-out in §3.5: "再同期用 cursor:
// invalidation broadcast の event drop に備え".
//
// The bus is process-local. Cross-peer fan-out is the WebSocket layer's
// job (see internal/server/events_ws.go), which subscribes once per
// connected peer and forwards events frame-by-frame.
//
// Concurrency model:
//
//   - Publish is non-blocking. It walks the subscriber list under a
//     read lock and tries to enqueue into each subscriber's buffered
//     channel; on a full channel the subscriber is terminated with
//     ReasonOverflow and removed.
//   - Subscribe returns a *Subscription. The subscription's event
//     channel is closed exactly once: when terminated for any reason
//     (caller Cancel, ctx Done, overflow, or bus Close). The
//     subscription's Context is cancelled at the same moment so any
//     in-flight write derived from it unblocks immediately.
//   - Reason reports why a subscription ended: caller / ctx / overflow
//     / closed. WebSocket handlers map this onto a close code so a
//     peer can tell "drop, please resync" apart from "server going
//     away" apart from "you cancelled".
//   - Close drains all subscribers and rejects further Publish/
//     Subscribe calls.
//
// Buffer sizing: §3.5 promises invalidations are "best effort"; the
// server pairs this with an HTTP cursor for replay. A small buffer
// (default 64) is enough to ride out a brief WebSocket write stall;
// anything bigger just delays the inevitable cursor catch-up.
package eventbus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// Event is the on-the-wire invalidation record. Fields mirror §4.1's
// `{table, id, etag, op}` plus `seq` for the resync cursor (peers ack
// up to a known seq) and `ts` (unix millis) for staleness debugging.
//
// Seq is the global monotonic seq from store.NextGlobalSeq() — the
// SAME value the row was written with, so a peer can compare against
// its local cache's last-seen seq for that table without ambiguity.
//
// ETag intentionally lacks `omitempty`: a delete is wire-distinguished
// from an update only by Op, and dropping the field would force
// peers to special-case missing-vs-empty. Always emit a string,
// always emit it.
type Event struct {
	Table string `json:"table"`
	ID    string `json:"id"`
	ETag  string `json:"etag"`
	Op    string `json:"op"` // insert | update | delete
	Seq   int64  `json:"seq"`
	TS    int64  `json:"ts"` // unix millis
}

// Reason for ending a subscription. Stable strings — the WebSocket
// handler maps these onto wire close codes, and changing them would
// break peer behavior expectations.
const (
	// ReasonCaller fires when the holder calls Subscription.Cancel().
	// The peer initiated the close; no resync needed.
	ReasonCaller = "caller"
	// ReasonContext fires when the ctx passed to Subscribe is cancelled
	// (typically the HTTP request context — client disconnected).
	ReasonContext = "context"
	// ReasonOverflow fires when the per-subscription buffer filled and
	// Publish had to drop the subscriber. The peer MUST resync via
	// `GET /api/v1/changes?since=<seq>` to recover lost events.
	ReasonOverflow = "overflow"
	// ReasonClosed fires when the bus itself was Closed (server
	// shutdown). The peer should reconnect after the server returns.
	ReasonClosed = "closed"
)

// ErrClosed is returned by Publish/Subscribe after Close.
var ErrClosed = errors.New("eventbus: closed")

// DefaultBuffer is the per-subscriber channel capacity. A subscriber
// that falls more than this many events behind is dropped.
const DefaultBuffer = 64

// Subscription is the live handle returned by Bus.Subscribe.
//
// Lifecycle:
//
//   - C() returns the event channel; closes once on termination.
//   - Context() returns a context that is cancelled at the same
//     moment as C() closes. Use it as the parent of any write
//     operation derived from the channel so the write unblocks
//     immediately on overflow / shutdown.
//   - Reason() reports why the subscription ended; meaningful only
//     after C() has closed (until then it returns "").
//   - Cancel() ends the subscription with ReasonCaller. Idempotent.
type Subscription struct {
	bus    *Bus
	ch     chan Event
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
	reason atomic.Value // string
}

// C returns the event delivery channel. The channel is closed exactly
// once when the subscription ends — read until !ok, then call Reason()
// to learn why.
func (s *Subscription) C() <-chan Event { return s.ch }

// Context returns a context that is cancelled when the subscription
// ends. Callers SHOULD use it as the parent of any blocking I/O that
// dispatches received events so the I/O unblocks promptly on overflow.
func (s *Subscription) Context() context.Context { return s.ctx }

// Reason returns the reason the subscription ended, or "" while live.
// One of the Reason* constants once terminated.
func (s *Subscription) Reason() string {
	v := s.reason.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

// Cancel ends the subscription with ReasonCaller. Idempotent.
//
// Cancel routes through the bus's eviction path so the bus mutex
// serializes channel-close against an in-flight Publish: without that
// hop, Cancel could close the channel concurrently with Publish's
// send and trip "send on closed channel".
func (s *Subscription) Cancel() {
	if s.bus != nil {
		s.bus.evict(s, ReasonCaller)
		return
	}
	// bus == nil means the subscription was constructed outside Subscribe
	// (only happens in test fakes); terminate directly.
	s.terminate(ReasonCaller)
}

// terminate is the single shutdown path: set reason, cancel ctx, close
// channel — in that order so a reader who sees the channel close can
// read a meaningful reason. once guarantees idempotence even when
// multiple termination triggers fire simultaneously (e.g. ctx cancel
// + bus close).
func (s *Subscription) terminate(reason string) {
	s.once.Do(func() {
		s.reason.Store(reason)
		s.cancel()
		close(s.ch)
	})
}

// Bus is the in-memory broadcast hub. The zero value is NOT usable —
// call New.
type Bus struct {
	mu      sync.RWMutex
	subs    map[*Subscription]struct{}
	closed  bool
	bufSize int
	dropped atomic.Int64 // total subscribers dropped for overflow
}

// New returns an empty Bus. bufSize <= 0 picks DefaultBuffer.
func New(bufSize int) *Bus {
	if bufSize <= 0 {
		bufSize = DefaultBuffer
	}
	return &Bus{
		subs:    make(map[*Subscription]struct{}),
		bufSize: bufSize,
	}
}

// Subscribe registers a new subscription. ctx, when not nil, is wired
// to the subscription's lifetime: ctx.Done causes the subscription to
// terminate with ReasonContext. The watcher goroutine exits as soon
// as the subscription terminates for any reason (no leak even if ctx
// outlives the subscription).
func (b *Bus) Subscribe(ctx context.Context) (*Subscription, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrClosed
	}
	subCtx, cancel := context.WithCancel(context.Background())
	sub := &Subscription{
		bus:    b,
		ch:     make(chan Event, b.bufSize),
		ctx:    subCtx,
		cancel: cancel,
	}
	b.subs[sub] = struct{}{}
	b.mu.Unlock()

	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				b.evict(sub, ReasonContext)
			case <-subCtx.Done():
				// Already terminated by another path (Cancel,
				// overflow, Close). Just exit.
			}
		}()
	}
	return sub, nil
}

// Publish broadcasts ev to every live subscriber. A full subscriber
// buffer means the subscriber is too slow — it is terminated with
// ReasonOverflow (the peer must resync via the cursor). Publish itself
// never blocks.
//
// Publish on a closed bus returns ErrClosed; otherwise nil.
func (b *Bus) Publish(ev Event) error {
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return ErrClosed
	}
	// Snapshot the subscriber set so we can release the read lock
	// before doing slow drops; otherwise an evict during fan-out
	// would deadlock with evict (which takes the write lock).
	victims := make([]*Subscription, 0)
	for sub := range b.subs {
		select {
		case sub.ch <- ev:
		default:
			victims = append(victims, sub)
		}
	}
	b.mu.RUnlock()

	for _, sub := range victims {
		if b.evict(sub, ReasonOverflow) {
			// Only count subscribers we actually removed. A sub that
			// was already terminated (e.g. ctx-cancelled or victim of
			// a concurrent Close) might still be in our snapshot but
			// must not double-count toward Dropped.
			b.dropped.Add(1)
		}
	}
	return nil
}

// Close drains all subscribers (terminating each with ReasonClosed)
// and marks the bus closed. Subsequent Subscribe / Publish return
// ErrClosed. Idempotent.
func (b *Bus) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	subs := b.subs
	b.subs = nil
	b.mu.Unlock()

	for sub := range subs {
		sub.terminate(ReasonClosed)
	}
}

// Dropped is the total number of subscribers evicted for buffer
// overflow since the bus was created. Useful for ops dashboards.
func (b *Bus) Dropped() int64 { return b.dropped.Load() }

// Subscribers returns the current live subscriber count.
func (b *Bus) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// evict removes sub from the live set and terminates it with the given
// reason. Returns true if this call was the one that actually removed
// the sub from the live set (i.e. it was the first termination path
// to win); false if it was already gone (Bus.Close, double cancel,
// concurrent overflow). Callers use the return value to gate side
// effects like the Dropped counter.
//
// delete and terminate happen under the same lock so a Bus.Close
// racing with this evict cannot snapshot a half-removed state. The
// channel close is cheap (non-blocking) so holding the bus mutex
// across it is harmless.
func (b *Bus) evict(sub *Subscription, reason string) bool {
	b.mu.Lock()
	_, ok := b.subs[sub]
	if ok {
		delete(b.subs, sub)
		// Only the path that actually owned the sub gets to label
		// the reason. If ok is false, either:
		//   - another evict already won (terminate is sync.Once,
		//     so a redundant call would be a no-op anyway)
		//   - Bus.Close swapped the map and is about to terminate
		//     this sub with ReasonClosed
		// In both cases, leaving terminate to the responsible path
		// preserves the correct user-visible reason.
		sub.terminate(reason)
	}
	b.mu.Unlock()
	return ok
}

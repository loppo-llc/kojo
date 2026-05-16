package peer

import (
	"context"
	"errors"
	"sync"
)

// StatusEvent is one peer_registry mutation visible to subscribers
// of the cross-peer status feed (docs §3.10 "両方向 heartbeat"
// narrowed to push semantics). The fields are intentionally a
// minimal subset of PeerRecord — subscribers re-fetch the full row
// via the registry API if they need capabilities or public_key.
type StatusEvent struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name,omitempty"`
	Status   string `json:"status"`             // "online" | "offline" | "degraded"
	LastSeen int64  `json:"last_seen,omitempty"` // unix millis
	// Op is "upsert" for register / first-touch, "touch" for
	// heartbeat status refreshes, "expire" for sweeper-driven
	// stale flips, "delete" for explicit deregistration. Lets a
	// subscriber gate on which mutations matter without re-deriving
	// from before/after rows.
	Op string `json:"op"`
}

// StatusEventOp enumerates the Op string values the publisher uses.
// Subscribers compare against these constants rather than the bare
// strings so a typo on either side surfaces at compile time.
const (
	StatusOpUpsert = "upsert"
	StatusOpTouch  = "touch"
	StatusOpExpire = "expire"
	StatusOpDelete = "delete"
)

// EventBus is the in-memory pub/sub for peer_registry status
// mutations. Publishers (the Registrar's heartbeat loop, the
// OfflineSweeper, the HTTP handlers that UpsertPeer / DeletePeer)
// call Publish; subscribers (the cross-peer WS handler, the
// in-process peer subscriber) call Subscribe and read from the
// returned channel.
//
// Why an in-memory bus instead of the existing internal/eventbus
// (which fans out events table rows): peer_registry mutations
// aren't currently routed through RecordEvent, and adding events
// table entries for every heartbeat would add a row every 30 s
// per peer — significant noise on the events stream that the
// /api/v1/changes cursor walks. A dedicated channel keeps the
// peer-status feed isolated from the domain-events feed.
//
// Concurrency: Publish is non-blocking — if a subscriber's channel
// is full, the event is DROPPED for that subscriber and a counter
// (Dropped) is bumped. A subscriber that drops too many events is
// expected to drop its subscription and re-fetch the full
// peer_registry. This trades best-effort delivery for guaranteed
// publisher progress — a stalled HTTP client can't pin the
// heartbeat loop.
type EventBus struct {
	mu     sync.Mutex
	subs   map[int]*statusSub
	nextID int
}

type statusSub struct {
	ch      chan StatusEvent
	ctx     context.Context
	cancel  context.CancelFunc
	dropped int
}

// NewEventBus returns a fresh bus with no subscribers.
func NewEventBus() *EventBus {
	return &EventBus{subs: make(map[int]*statusSub)}
}

// Publish fans out evt to every live subscriber. Non-blocking: a
// subscriber whose channel is full has its subscription cancelled
// — the WS handler observes the channel close and emits a
// PolicyViolation close code that the peer's subscriber
// reconnects through (and re-receives the snapshot frame). This
// trades a noisy log line for guaranteed publisher progress
// AND correctness: previously the publisher bumped a counter
// and dropped the event silently, leaving the subscriber with
// a stale view it had no way to detect.
func (b *EventBus) Publish(evt StatusEvent) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, s := range b.subs {
		select {
		case s.ch <- evt:
		default:
			// Buffer full → cancel the sub. The watcher
			// goroutine in Subscribe removes the entry from
			// b.subs and closes the channel, so the receiver's
			// next read returns !ok and the handler emits its
			// close-code. We bump dropped for telemetry; the
			// counter is observable via tests.
			s.dropped++
			s.cancel()
			delete(b.subs, id)
		}
	}
}

// Subscribe returns a channel that will receive every StatusEvent
// the bus publishes from now on. The channel has a small buffer
// (subBufferSize) — a subscriber that doesn't drain it within that
// many events starts dropping. The returned cancel function
// removes the subscription and closes the channel; subscribers
// MUST call it on exit to avoid leaks.
//
// The parent ctx also tears down the subscription: when it
// cancels, the bus's internal context derived from it fires the
// cancel goroutine that removes the subscriber.
func (b *EventBus) Subscribe(ctx context.Context) (<-chan StatusEvent, func(), error) {
	if b == nil {
		return nil, nil, errors.New("peer.EventBus: nil bus")
	}
	if ctx == nil {
		return nil, nil, errors.New("peer.EventBus.Subscribe: nil ctx")
	}
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	subCtx, cancel := context.WithCancel(ctx)
	sub := &statusSub{
		ch:     make(chan StatusEvent, subBufferSize),
		ctx:    subCtx,
		cancel: cancel,
	}
	b.subs[id] = sub
	b.mu.Unlock()

	// ctx watcher: when the caller cancels (or the parent ctx
	// fires), drop the subscription and close the channel so the
	// reader's range loop exits cleanly.
	go func() {
		<-subCtx.Done()
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
		close(sub.ch)
	}()
	return sub.ch, cancel, nil
}

// Subscribers returns the current subscriber count. Exposed for
// tests + telemetry; production code shouldn't care.
func (b *EventBus) Subscribers() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// subBufferSize bounds the per-subscriber channel buffer. Status
// events are a few per minute even in a chatty cluster (heartbeats
// every 30 s × N peers); 32 absorbs a transient stall on the
// subscriber side without immediately starting to drop. A
// subscriber stuck longer than that is broken and dropping is
// the correct response.
const subBufferSize = 32

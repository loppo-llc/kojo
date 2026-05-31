package agent

import "sync"

// Per-subscriber backlog caps. A turn may produce thousands of small
// text/thinking deltas, or a handful of large tool_result events; both must
// be bounded to keep memory finite if a consumer stalls (e.g. WebSocket TCP
// buffer full and the client never recovers). On overflow the subscriber is
// abandoned — its consumer will see the channel close and can resync from
// the transcript via synthesizeTerminal.
const (
	maxSubBacklog      = 8192
	maxSubBacklogBytes = 64 * 1024 * 1024 // 64 MiB
)

// eventSize returns a rough byte estimate of an event for backlog accounting.
// Only the variable-length string fields are counted; the fixed struct
// overhead is ignored. The final "done" event's Message often carries the
// full assistant content plus tool input/output, which is by far the
// largest event type — so we account for those too.
func eventSize(ev ChatEvent) int {
	n := len(ev.Type) + len(ev.Status) + len(ev.Delta) +
		len(ev.ToolUseID) + len(ev.ToolName) + len(ev.ToolInput) +
		len(ev.ToolOutput) + len(ev.ErrorMessage)
	for i := range ev.Attachments {
		a := &ev.Attachments[i]
		n += len(a.Path) + len(a.Name) + len(a.Mime)
	}
	if ev.Message != nil {
		m := ev.Message
		n += len(m.ID) + len(m.Role) + len(m.Content) + len(m.Thinking) + len(m.Timestamp)
		for i := range m.ToolUses {
			t := &m.ToolUses[i]
			n += len(t.ID) + len(t.Name) + len(t.Input) + len(t.Output)
		}
		for i := range m.Attachments {
			a := &m.Attachments[i]
			n += len(a.Path) + len(a.Name) + len(a.Mime)
		}
	}
	return n
}

// chatBroadcaster fans out events from a single source channel to multiple
// subscribers. It keeps a log of all events so that late-joining subscribers
// can replay the full history.
//
// Each subscriber owns an independent backlog and a forwarder goroutine, so a
// slow consumer (e.g. a WebSocket whose TCP send buffer is full) cannot drop
// events nor stall the broadcaster or other subscribers. The backlog is
// bounded by maxSubBacklog; a runaway consumer is abandoned rather than
// allowed to consume unbounded memory.
type chatBroadcaster struct {
	mu   sync.Mutex
	log  []ChatEvent
	subs map[*chatSub]struct{}
	done bool
}

type chatSub struct {
	ch chan ChatEvent // delivered to the consumer; closed by forward()

	mu       sync.Mutex
	cond     *sync.Cond
	buf      []ChatEvent
	bufBytes int           // approximate total payload bytes in buf
	closed   bool          // no more events will be pushed; forward drains remaining
	stop     chan struct{} // signals forward() to exit immediately (drop pending)
}

func newChatSub() *chatSub {
	s := &chatSub{
		ch:   make(chan ChatEvent, 1),
		stop: make(chan struct{}),
	}
	s.cond = sync.NewCond(&s.mu)
	go s.forward()
	return s
}

// push appends an event to the subscriber's backlog. Non-blocking.
// Returns false if the sub has already been closed, or if the backlog overflowed
// (count or byte cap) — in the overflow case the subscriber is force-abandoned.
// Callers should treat false as a soft drop and remove this sub from the
// active set so further events skip it; the consumer will resync from the
// transcript on next connect.
func (s *chatSub) push(ev ChatEvent) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	sz := eventSize(ev)
	if len(s.buf) >= maxSubBacklog || s.bufBytes+sz > maxSubBacklogBytes {
		// Slow/stalled consumer; abandon to bound memory.
		s.closed = true
		select {
		case <-s.stop:
		default:
			close(s.stop)
		}
		s.cond.Broadcast()
		s.mu.Unlock()
		return false
	}
	s.buf = append(s.buf, ev)
	s.bufBytes += sz
	s.cond.Signal()
	s.mu.Unlock()
	return true
}

// closeDrain marks the subscriber closed. The forwarder drains any remaining
// events to the consumer before closing the channel. Use this for normal
// termination (terminal event delivered, source closed).
func (s *chatSub) closeDrain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.cond.Broadcast()
}

// abandon forces the forwarder to exit immediately, discarding any pending
// events. Use this when the consumer has stopped reading (e.g. Unsubscribe by
// a disconnected WebSocket client) — without it, forward() could block forever
// on `s.ch <- ev`.
func (s *chatSub) abandon() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		// Already closing via closeDrain; also signal stop so forward exits
		// even if the consumer never reads the remaining backlog.
		select {
		case <-s.stop:
		default:
			close(s.stop)
		}
		return
	}
	s.closed = true
	close(s.stop)
	s.cond.Broadcast()
}

func (s *chatSub) forward() {
	defer close(s.ch)
	for {
		s.mu.Lock()
		for len(s.buf) == 0 && !s.closed {
			s.cond.Wait()
		}
		if len(s.buf) == 0 {
			// closed && empty → done
			s.mu.Unlock()
			return
		}
		ev := s.buf[0]
		s.buf[0] = ChatEvent{} // release for GC
		s.buf = s.buf[1:]
		s.bufBytes -= eventSize(ev)
		if s.bufBytes < 0 {
			s.bufBytes = 0
		}
		s.mu.Unlock()

		select {
		case s.ch <- ev:
		case <-s.stop:
			return
		}
	}
}

func newChatBroadcaster(src <-chan ChatEvent) *chatBroadcaster {
	b := &chatBroadcaster{subs: make(map[*chatSub]struct{})}
	go b.run(src)
	return b
}

func (b *chatBroadcaster) run(src <-chan ChatEvent) {
	for ev := range src {
		terminal := ev.Type == "done" || ev.Type == "error"

		b.mu.Lock()
		b.log = append(b.log, ev)

		if terminal {
			// Push terminal event to every subscriber, then close them so the
			// forwarder drains the backlog (including this terminal) before
			// closing the consumer channel. Mark done so any Subscribe() that
			// races between this push and source close returns a pre-closed
			// channel rather than registering a live sub that will never see
			// another event.
			subs := make([]*chatSub, 0, len(b.subs))
			for sub := range b.subs {
				subs = append(subs, sub)
				delete(b.subs, sub)
			}
			b.done = true
			b.mu.Unlock()
			for _, sub := range subs {
				sub.push(ev)
				sub.closeDrain()
			}
		} else {
			subs := make([]*chatSub, 0, len(b.subs))
			for sub := range b.subs {
				subs = append(subs, sub)
			}
			b.mu.Unlock()
			var dead []*chatSub
			for _, sub := range subs {
				if !sub.push(ev) {
					// Sub abandoned (overflow or already closed). Drop it
					// from the active set so subsequent events don't keep
					// calling push on a dead sub.
					dead = append(dead, sub)
				}
			}
			if len(dead) > 0 {
				b.mu.Lock()
				for _, sub := range dead {
					delete(b.subs, sub)
				}
				b.mu.Unlock()
			}
		}
	}

	// Source closed without a terminal event — drain & close remaining subs.
	b.mu.Lock()
	b.done = true
	subs := make([]*chatSub, 0, len(b.subs))
	for sub := range b.subs {
		subs = append(subs, sub)
	}
	b.subs = make(map[*chatSub]struct{})
	b.mu.Unlock()
	for _, sub := range subs {
		sub.closeDrain()
	}
}

// Subscribe returns all past events and a channel for future events.
// Call the returned unsub function when done to avoid leaking the channel.
// If the source is already closed, the returned channel is pre-closed.
func (b *chatBroadcaster) Subscribe() (past []ChatEvent, live <-chan ChatEvent, unsub func()) {
	sub := newChatSub()

	b.mu.Lock()
	past = make([]ChatEvent, len(b.log))
	copy(past, b.log)

	if b.done {
		b.mu.Unlock()
		sub.closeDrain() // forwarder will exit (buf empty, closed=true)
		return past, sub.ch, func() {}
	}

	b.subs[sub] = struct{}{}
	b.mu.Unlock()

	unsub = func() {
		b.mu.Lock()
		_, stillRegistered := b.subs[sub]
		if stillRegistered {
			delete(b.subs, sub)
		}
		b.mu.Unlock()
		// abandon() is safe to call regardless of state; it forces the
		// forwarder to exit even if the consumer has stopped reading.
		sub.abandon()
	}
	return past, sub.ch, unsub
}

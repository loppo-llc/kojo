package agent

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestBroadcaster_SlowSubscriberNoDrop verifies that a slow consumer does not
// cause non-terminal events to be dropped (the bug that caused agent chat UI
// to "stop updating" mid-stream). The producer pushes many text deltas quickly
// while the consumer reads slowly; every event must arrive.
func TestBroadcaster_SlowSubscriberNoDrop(t *testing.T) {
	src := make(chan ChatEvent)
	bc := newChatBroadcaster(src)

	_, live, unsub := bc.Subscribe()
	defer unsub()

	const N = 500
	go func() {
		for i := 0; i < N; i++ {
			src <- ChatEvent{Type: "text", Delta: strconv.Itoa(i)}
		}
		src <- ChatEvent{Type: "done"}
		close(src)
	}()

	var got []string
	for ev := range live {
		// Simulate a slow WebSocket consumer.
		time.Sleep(200 * time.Microsecond)
		if ev.Type == "text" {
			got = append(got, ev.Delta)
		}
		if ev.Type == "done" {
			break
		}
	}

	if len(got) != N {
		t.Fatalf("expected %d text events, got %d", N, len(got))
	}
	for i, s := range got {
		if s != strconv.Itoa(i) {
			t.Fatalf("event %d out of order: got %q", i, s)
		}
	}
}

// TestBroadcaster_MultipleSubscribers verifies that a slow subscriber does not
// affect the delivery rate of a fast one.
func TestBroadcaster_MultipleSubscribers(t *testing.T) {
	src := make(chan ChatEvent)
	bc := newChatBroadcaster(src)

	_, fast, unsubFast := bc.Subscribe()
	defer unsubFast()
	_, slow, unsubSlow := bc.Subscribe()
	defer unsubSlow()

	const N = 200
	go func() {
		for i := 0; i < N; i++ {
			src <- ChatEvent{Type: "text", Delta: strconv.Itoa(i)}
		}
		src <- ChatEvent{Type: "done"}
		close(src)
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	var fastGot, slowGot int
	go func() {
		defer wg.Done()
		for ev := range fast {
			if ev.Type == "text" {
				fastGot++
			}
		}
	}()
	go func() {
		defer wg.Done()
		for ev := range slow {
			time.Sleep(500 * time.Microsecond)
			if ev.Type == "text" {
				slowGot++
			}
		}
	}()

	wg.Wait()
	if fastGot != N {
		t.Errorf("fast: expected %d, got %d", N, fastGot)
	}
	if slowGot != N {
		t.Errorf("slow: expected %d, got %d", N, slowGot)
	}
}

// TestBroadcaster_UnsubscribeReleases verifies that calling unsub on a
// subscriber that has stopped reading does not deadlock the forwarder.
func TestBroadcaster_UnsubscribeReleases(t *testing.T) {
	src := make(chan ChatEvent)
	bc := newChatBroadcaster(src)

	_, live, unsub := bc.Subscribe()

	// Producer keeps pushing; consumer never reads.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			select {
			case src <- ChatEvent{Type: "text", Delta: "x"}:
			case <-time.After(time.Second):
				t.Error("producer stalled")
				return
			}
		}
	}()

	<-done
	unsub()

	// After unsub, the channel must drain (close) within a reasonable time.
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-live:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("live channel did not close after unsub")
		}
	}
}

// TestBroadcaster_TerminalEventDelivered verifies that the terminal event is
// always delivered, even to a temporarily-slow subscriber, before close.
func TestBroadcaster_TerminalEventDelivered(t *testing.T) {
	src := make(chan ChatEvent)
	bc := newChatBroadcaster(src)

	_, live, unsub := bc.Subscribe()
	defer unsub()

	go func() {
		for i := 0; i < 100; i++ {
			src <- ChatEvent{Type: "text", Delta: "d"}
		}
		src <- ChatEvent{Type: "done"}
		close(src)
	}()

	var sawDone bool
	for ev := range live {
		// Slow consumer.
		time.Sleep(100 * time.Microsecond)
		if ev.Type == "done" {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("terminal 'done' event was not delivered")
	}
}

// TestBroadcaster_StalledConsumerNoLeak simulates the production failure:
// a consumer that stops reading (e.g. dead WebSocket) without calling unsub.
// Without the backlog cap, the broadcaster would keep pushing events into the
// unread channel forever; the cap must force-abandon the sub so the forwarder
// goroutine exits.
func TestBroadcaster_StalledConsumerNoLeak(t *testing.T) {
	src := make(chan ChatEvent)
	bc := newChatBroadcaster(src)

	// Subscribe but never read from `live`. Don't call unsub() either —
	// this is the "WebSocket dropped silently" scenario.
	_, live, _ := bc.Subscribe()

	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		// Push enough events to exceed maxSubBacklog. With the cap the
		// broadcaster.run loop continues normally; without it, the test would
		// rely on goroutine leak detection.
		for i := 0; i < maxSubBacklog+1000; i++ {
			src <- ChatEvent{Type: "text", Delta: "x"}
		}
		src <- ChatEvent{Type: "done"}
		close(src)
	}()

	// Wait for producer to finish — proves push() returned quickly and the
	// broadcaster.run loop wasn't stalled by a stuck subscriber. If we ever
	// regress to "non-blocking drop" the producer would still finish too,
	// but events would be lost silently rather than the sub being abandoned.
	select {
	case <-producerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("producer stalled — broadcaster.run blocked on push")
	}

	// Forwarder must have exited; ch is closed. The consumer never read
	// anything, but it observes the closure now.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-live:
			if !ok {
				return // closed — forwarder exited cleanly
			}
			// We read an event we never wanted; allowed but not expected to
			// continue forever. Loop until close.
		case <-deadline:
			t.Fatal("forwarder for stalled consumer did not exit")
		}
	}
}

// TestBroadcaster_LateSubscribeAfterTerminal verifies that a subscriber
// joining after the terminal event was sent (but before src is closed) does
// not register as a live sub — it gets the terminal via `past` and a
// pre-closed channel.
func TestBroadcaster_LateSubscribeAfterTerminal(t *testing.T) {
	src := make(chan ChatEvent)
	bc := newChatBroadcaster(src)

	// Push a regular event and a terminal, but do NOT close src.
	src <- ChatEvent{Type: "text", Delta: "hi"}
	src <- ChatEvent{Type: "done"}
	// Allow run() to process them.
	time.Sleep(20 * time.Millisecond)

	past, live, unsub := bc.Subscribe()
	defer unsub()

	if len(past) != 2 || past[1].Type != "done" {
		t.Fatalf("late subscriber missing terminal in past: %+v", past)
	}

	// live must be pre-closed.
	select {
	case _, ok := <-live:
		if ok {
			t.Fatal("expected live channel to be closed for late subscriber")
		}
	case <-time.After(time.Second):
		t.Fatal("live channel did not close for late subscriber")
	}

	close(src)
}

// TestBroadcaster_LateSubscriberReplay verifies that a subscriber joining
// after some events have been broadcast receives the full history via `past`.
func TestBroadcaster_LateSubscriberReplay(t *testing.T) {
	src := make(chan ChatEvent)
	bc := newChatBroadcaster(src)

	// Push 3 events synchronously through the broadcaster.
	for i := 0; i < 3; i++ {
		src <- ChatEvent{Type: "text", Delta: strconv.Itoa(i)}
	}
	// Allow run() to drain.
	time.Sleep(20 * time.Millisecond)

	past, live, unsub := bc.Subscribe()
	defer unsub()

	if len(past) != 3 {
		t.Fatalf("expected 3 past events, got %d", len(past))
	}

	go func() {
		src <- ChatEvent{Type: "done"}
		close(src)
	}()

	var sawDone bool
	for ev := range live {
		if ev.Type == "done" {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("late subscriber missed done event")
	}
}

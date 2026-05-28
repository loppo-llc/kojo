package eventbus

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// drainReason waits for sub.C() to close, then returns its Reason.
// All termination paths set Reason BEFORE closing the channel, so this
// is race-free against any concurrent termination.
func drainReason(t *testing.T, sub *Subscription) string {
	t.Helper()
	for {
		select {
		case _, ok := <-sub.C():
			if !ok {
				return sub.Reason()
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("subscription did not close")
			return ""
		}
	}
}

func TestPublishFanOut(t *testing.T) {
	b := New(8)
	defer b.Close()

	subA, err := b.Subscribe(nil)
	if err != nil {
		t.Fatalf("Subscribe A: %v", err)
	}
	defer subA.Cancel()
	subB, err := b.Subscribe(nil)
	if err != nil {
		t.Fatalf("Subscribe B: %v", err)
	}
	defer subB.Cancel()

	ev := Event{Table: "agents", ID: "ag_1", ETag: "1-x", Op: "insert", Seq: 42, TS: 1000}
	if err := b.Publish(ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	for i, sub := range []*Subscription{subA, subB} {
		select {
		case got, ok := <-sub.C():
			if !ok {
				t.Fatalf("sub %d closed unexpectedly", i)
			}
			if got != ev {
				t.Errorf("sub %d: got %+v want %+v", i, got, ev)
			}
		case <-time.After(time.Second):
			t.Fatalf("sub %d: timeout waiting for event", i)
		}
	}
}

func TestSubscribeCancelClosesChannel(t *testing.T) {
	b := New(4)
	defer b.Close()

	sub, err := b.Subscribe(nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	sub.Cancel()

	if r := drainReason(t, sub); r != ReasonCaller {
		t.Errorf("Reason = %q, want %q", r, ReasonCaller)
	}
	// double cancel is a no-op
	sub.Cancel()
	if sub.Reason() != ReasonCaller {
		t.Error("double Cancel changed Reason")
	}
	// context must be cancelled too
	select {
	case <-sub.Context().Done():
	default:
		t.Error("Subscription.Context() was not cancelled")
	}
}

func TestContextCancelClosesChannel(t *testing.T) {
	b := New(4)
	defer b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	sub, err := b.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancel()

	if r := drainReason(t, sub); r != ReasonContext {
		t.Errorf("Reason = %q, want %q", r, ReasonContext)
	}
}

func TestSlowSubscriberDropped(t *testing.T) {
	b := New(2) // tiny buffer to force overflow
	defer b.Close()

	sub, err := b.Subscribe(nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Publish 5 events without draining → after 2, the subscriber
	// fills its buffer; the 3rd publish evicts.
	for i := 0; i < 5; i++ {
		if err := b.Publish(Event{Seq: int64(i)}); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}

	// First 2 events were buffered; remaining publishes evict.
	for i := 0; i < 2; i++ {
		select {
		case ev, ok := <-sub.C():
			if !ok {
				t.Fatalf("channel closed too early at i=%d", i)
			}
			if ev.Seq != int64(i) {
				t.Errorf("i=%d: got seq %d want %d", i, ev.Seq, i)
			}
		case <-time.After(time.Second):
			t.Fatalf("i=%d: timeout", i)
		}
	}
	// Then channel must close with overflow reason.
	if r := drainReason(t, sub); r != ReasonOverflow {
		t.Errorf("Reason = %q, want %q", r, ReasonOverflow)
	}

	if b.Dropped() == 0 {
		t.Error("Dropped counter did not advance")
	}
}

func TestCloseDrainsAllSubscribers(t *testing.T) {
	b := New(4)

	const n = 5
	subs := make([]*Subscription, n)
	for i := 0; i < n; i++ {
		sub, err := b.Subscribe(nil)
		if err != nil {
			t.Fatalf("Subscribe %d: %v", i, err)
		}
		subs[i] = sub
	}

	b.Close()

	for i, sub := range subs {
		if r := drainReason(t, sub); r != ReasonClosed {
			t.Errorf("sub %d Reason = %q, want %q", i, r, ReasonClosed)
		}
	}

	if err := b.Publish(Event{}); !errors.Is(err, ErrClosed) {
		t.Errorf("Publish after Close: got %v want ErrClosed", err)
	}
	if _, err := b.Subscribe(nil); !errors.Is(err, ErrClosed) {
		t.Errorf("Subscribe after Close: got %v want ErrClosed", err)
	}

	// double Close is a no-op
	b.Close()

	// Cancel still safe after close
	for _, sub := range subs {
		sub.Cancel()
	}
}

func TestContextWatcherDoesNotLeakAfterCancel(t *testing.T) {
	// If the ctx watcher only selects on ctx.Done, calling Cancel()
	// before ctx fires would leak the goroutine until ctx eventually
	// completes. This test asserts the watcher exits via the
	// subscription context too.
	b := New(4)
	defer b.Close()

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub, err := b.Subscribe(parent)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	sub.Cancel()
	// ctx watcher must observe sub termination and exit, even though
	// parent is still alive. We can't observe goroutine exit directly
	// in a portable way, but a healthy sub.Context().Done() is the
	// signal the watcher uses internally — make sure it's closed.
	select {
	case <-sub.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("Subscription.Context() did not cancel")
	}
}

func TestSubscriberCount(t *testing.T) {
	b := New(4)
	defer b.Close()

	if got := b.Subscribers(); got != 0 {
		t.Errorf("initial Subscribers = %d, want 0", got)
	}
	s1, _ := b.Subscribe(nil)
	s2, _ := b.Subscribe(nil)
	if got := b.Subscribers(); got != 2 {
		t.Errorf("after 2 Subscribe = %d, want 2", got)
	}
	s1.Cancel()
	if got := b.Subscribers(); got != 1 {
		t.Errorf("after 1 cancel = %d, want 1", got)
	}
	s2.Cancel()
	if got := b.Subscribers(); got != 0 {
		t.Errorf("after 2 cancel = %d, want 0", got)
	}
}

func TestConcurrentPublishSubscribeAndClose(t *testing.T) {
	// Soak: publishers + churning subscribers + a Close racing the
	// whole thing must not deadlock or trip -race. The test trusts
	// the race detector + timeout; assertions are negative (no panic,
	// no hang).
	b := New(32)

	var wg sync.WaitGroup

	// Publishers
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(start int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = b.Publish(Event{Seq: int64(start*1000 + j)})
			}
		}(i)
	}

	// Churning subscribers
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				sub, err := b.Subscribe(nil)
				if err != nil {
					return
				}
				// Drain a few then cancel.
				for k := 0; k < 3; k++ {
					select {
					case <-sub.C():
					case <-time.After(50 * time.Millisecond):
						k = 3
					}
				}
				sub.Cancel()
			}
		}()
	}

	// Close racer — fires once partway through.
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		b.Close()
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("workers did not finish — possible deadlock")
	}
}

func TestEventJSONIncludesEmptyETag(t *testing.T) {
	// §4.1 wire format requires the etag field on every event,
	// including delete (which has no etag). The Event struct uses
	// `json:"etag"` (no omitempty) — assert that's still true.
	ev := Event{Table: "agents", ID: "ag_1", Op: "delete", Seq: 1, TS: 2}
	const want = `"etag":""`
	got, err := jsonMarshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !contains(got, want) {
		t.Errorf("expected %q in %s", want, got)
	}
}

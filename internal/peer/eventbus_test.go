package peer

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestEventBus_PublishToSingleSubscriber(t *testing.T) {
	b := NewEventBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, unsub, err := b.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()
	evt := StatusEvent{DeviceID: "peer-a", Status: "online", Op: StatusOpUpsert}
	b.Publish(evt)
	select {
	case got := <-ch:
		if got != evt {
			t.Errorf("got %+v, want %+v", got, evt)
		}
	case <-time.After(time.Second):
		t.Fatalf("did not receive event")
	}
}

func TestEventBus_PublishToMultipleSubscribers(t *testing.T) {
	b := NewEventBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var chs []<-chan StatusEvent
	for i := 0; i < 3; i++ {
		ch, _, err := b.Subscribe(ctx)
		if err != nil {
			t.Fatalf("Subscribe %d: %v", i, err)
		}
		chs = append(chs, ch)
	}
	evt := StatusEvent{DeviceID: "peer-a", Status: "online", Op: StatusOpUpsert}
	b.Publish(evt)
	for i, ch := range chs {
		select {
		case got := <-ch:
			if got != evt {
				t.Errorf("sub %d got %+v, want %+v", i, got, evt)
			}
		case <-time.After(time.Second):
			t.Errorf("sub %d did not receive", i)
		}
	}
}

func TestEventBus_CancelUnsubscribes(t *testing.T) {
	b := NewEventBus()
	ctx, cancel := context.WithCancel(context.Background())
	ch, _, err := b.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if b.Subscribers() != 1 {
		t.Errorf("want 1 sub, got %d", b.Subscribers())
	}
	cancel()
	// Wait for the watcher goroutine to clean up.
	for i := 0; i < 100 && b.Subscribers() != 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if b.Subscribers() != 0 {
		t.Errorf("after cancel: want 0 subs, got %d", b.Subscribers())
	}
	// Channel must close so a reader's range loop exits.
	if _, ok := <-ch; ok {
		t.Errorf("channel did not close after cancel")
	}
}

func TestEventBus_PublishNonBlockingOnFullSubscriber(t *testing.T) {
	b := NewEventBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, err := b.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Don't read — fill the buffer + overflow. Publish must return
	// immediately for each call.
	done := make(chan struct{})
	go func() {
		for i := 0; i < subBufferSize+10; i++ {
			b.Publish(StatusEvent{DeviceID: "peer", Op: StatusOpTouch})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a full subscriber")
	}
}

func TestEventBus_NilSafe(t *testing.T) {
	var b *EventBus
	b.Publish(StatusEvent{}) // no panic
	if b.Subscribers() != 0 {
		t.Errorf("nil bus: subs = %d", b.Subscribers())
	}
}

func TestEventBus_ConcurrentPublishAndSubscribe(t *testing.T) {
	b := NewEventBus()
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 4 subscribers reading, 4 publishers writing — proves the
	// mutex serializes correctly without panics or races.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			subCtx, subCancel := context.WithCancel(ctx)
			defer subCancel()
			ch, _, err := b.Subscribe(subCtx)
			if err != nil {
				t.Errorf("Subscribe: %v", err)
				return
			}
			for j := 0; j < 50; j++ {
				select {
				case <-ch:
				case <-time.After(time.Second):
					return
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				b.Publish(StatusEvent{DeviceID: "peer", Op: StatusOpTouch})
			}
		}(i)
	}
	wg.Wait()
}

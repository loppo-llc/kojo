package agent

import (
	"context"
	"testing"
	"time"
)

func TestProcessOneShotEventsDeliversTerminalAfterCancelUnderBackpressure(t *testing.T) {
	m := &Manager{}
	ctx, cancel := context.WithCancel(context.Background())
	outCh := make(chan ChatEvent, 1)
	backendCh := make(chan ChatEvent, 1)

	outCh <- ChatEvent{Type: "status", Status: "buffer-full"}
	backendCh <- ChatEvent{Type: "error", ErrorMessage: "terminal after cancel"}
	close(backendCh)
	cancel()

	done := make(chan struct{})
	go func() {
		m.processOneShotEvents(ctx, "test-agent", backendCh, outCh)
		close(done)
	}()

	// Keep outCh full long enough for processOneShotEvents to observe the
	// cancelled context, then free capacity. The bounded terminal send should
	// still deliver the event instead of dropping it as soon as ctx.Done is ready.
	time.Sleep(20 * time.Millisecond)
	<-outCh

	select {
	case got := <-outCh:
		if got.Type != "error" || got.ErrorMessage != "terminal after cancel" {
			t.Fatalf("terminal event = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal event was dropped after cancellation under backpressure")
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("processOneShotEvents did not return")
	}
}

//go:build heavy_test

// Same heavy_test gating as agent_handlers_test.go / blob_handlers_test.go —
// New() pulls the full server graph (modernc.org/sqlite via store, agent
// manager, etc) and the bundled binary is too heavy for the default
// `go test ./...` run. Run with `go test -tags heavy_test ./internal/server/...`.

package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/loppo-llc/kojo/internal/eventbus"
)

// newEventsTestServer stands up a Server whose only configured
// subsystem is an event bus. Tests hit the public mux directly via
// httptest so they don't need to negotiate auth — the route under test
// is independent of the auth middleware.
func newEventsTestServer(t *testing.T, bus *eventbus.Bus) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(Config{
		Addr:     ":0",
		Logger:   logger,
		Version:  "test",
		EventBus: bus,
	})
	ts := httptest.NewServer(srv.mux)
	t.Cleanup(ts.Close)
	return ts
}

func dialEventsWS(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	url := strings.Replace(ts.URL, "http://", "ws://", 1) + "/api/v1/events"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	return conn
}

func TestEventsWSReceivesPublishedEvent(t *testing.T) {
	bus := eventbus.New(8)
	defer bus.Close()
	ts := newEventsTestServer(t, bus)

	conn := dialEventsWS(t, ts)

	// Wait until the server-side subscriber has registered. Without this
	// the Publish below races the Subscribe inside the handler — the
	// event fires into an empty bus and the read times out.
	deadline := time.Now().Add(2 * time.Second)
	for bus.Subscribers() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("server never subscribed")
		}
		time.Sleep(10 * time.Millisecond)
	}

	want := eventbus.Event{
		Table: "agents",
		ID:    "ag_42",
		ETag:  "5-deadbeef",
		Op:    "update",
		Seq:   100,
	}
	if err := bus.Publish(want); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var got eventbus.Event
	if err := wsjson.Read(ctx, conn, &got); err != nil {
		t.Fatalf("wsjson.Read: %v", err)
	}
	// TS is server-stamped on Publish path going through Server.PublishEvent;
	// the bus.Publish path passes it through verbatim, so we expect 0.
	got.TS = 0
	if got != want {
		t.Errorf("event mismatch:\n got: %+v\nwant: %+v", got, want)
	}
}

func TestEventsWSDisabledReturns503(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(Config{
		Addr:    ":0",
		Logger:  logger,
		Version: "test",
		// EventBus left nil
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	srv.mux.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		// When EventBus is nil the route is not registered at all, so
		// the mux's default 404 stands. This documents the contract:
		// callers that need the endpoint must wire a bus at startup.
		t.Errorf("expected 404 for nil bus, got %d body=%q", w.Code, w.Body.String())
	}
}

func TestServerPublishEventStampsTimestamp(t *testing.T) {
	bus := eventbus.New(8)
	defer bus.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(Config{
		Addr:     ":0",
		Logger:   logger,
		Version:  "test",
		EventBus: bus,
	})

	sub, err := bus.Subscribe(nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	before := time.Now().UnixMilli()
	srv.PublishEvent(eventbus.Event{Table: "x", ID: "1", Op: "insert"})

	select {
	case ev := <-sub.C():
		if ev.TS < before {
			t.Errorf("TS = %d, want >= %d", ev.TS, before)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
}

// readWSCloseCode performs one wsjson.Read against conn until it sees
// a close frame, then returns the close code. Times out via ctx so a
// connection that never closes fails the test instead of hanging.
func readWSCloseCode(t *testing.T, conn *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := conn.Read(ctx)
		if err == nil {
			continue // discard any pre-close frames
		}
		return websocket.CloseStatus(err)
	}
}

func TestEventsWSOverflowSendsPolicyViolation(t *testing.T) {
	// Tiny bus buffer so a couple of publishes fill it before the
	// handler can drain. Eviction must close the connection with
	// 1008 PolicyViolation so the peer knows to resync.
	bus := eventbus.New(1)
	defer bus.Close()
	ts := newEventsTestServer(t, bus)

	conn := dialEventsWS(t, ts)

	deadline := time.Now().Add(2 * time.Second)
	for bus.Subscribers() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("server never subscribed")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Hammer the bus until at least one subscriber is dropped for
	// overflow. Bound by a deadline rather than a fixed iteration
	// count so the test stays deterministic regardless of scheduling
	// jitter. With buffer=1, the very first publish that finds the
	// channel full evicts.
	deadline = time.Now().Add(3 * time.Second)
	for bus.Dropped() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("subscriber never overflowed")
		}
		_ = bus.Publish(eventbus.Event{Table: "agents", ID: "ag", Op: "update"})
	}

	if got := readWSCloseCode(t, conn); got != websocket.StatusPolicyViolation {
		t.Errorf("close code = %d, want %d (PolicyViolation)", got, websocket.StatusPolicyViolation)
	}
}

func TestEventsWSBusCloseSendsGoingAway(t *testing.T) {
	bus := eventbus.New(8)
	ts := newEventsTestServer(t, bus)

	conn := dialEventsWS(t, ts)

	deadline := time.Now().Add(2 * time.Second)
	for bus.Subscribers() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("server never subscribed")
		}
		time.Sleep(10 * time.Millisecond)
	}

	bus.Close()

	if got := readWSCloseCode(t, conn); got != websocket.StatusGoingAway {
		t.Errorf("close code = %d, want %d (GoingAway)", got, websocket.StatusGoingAway)
	}
}

func TestServerPublishEventNilBus(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(Config{
		Addr:    ":0",
		Logger:  logger,
		Version: "test",
	})
	// Must not panic.
	srv.PublishEvent(eventbus.Event{Table: "x", ID: "1", Op: "insert"})
}

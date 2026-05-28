package server

import (
	"context"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/loppo-llc/kojo/internal/eventbus"
)

// handleEventsWS streams invalidation events to a connected peer.
//
// Wire format: each frame is a JSON object matching eventbus.Event
// (`{table, id, etag, op, seq, ts}`). The client SHOULD treat events
// as advisory: drop the matching cache row and re-fetch on next read.
// Event delivery is best-effort — a stalled client whose buffer
// overflows is dropped with close code 1008 (PolicyViolation) and the
// peer must then resync via `GET /api/v1/changes?since=<seq>` (future
// slice).
//
// Lifecycle ordering is deliberate:
//
//  1. Subscribe BEFORE Accept. Otherwise the brief gap between
//     Accept-returns and Subscribe-completes drops events that fire
//     in that window — the peer would miss invalidations it has no
//     way to recover.
//  2. If Accept fails, the deferred Cancel still tears down the
//     subscription (no leak in the bus).
//  3. The write context is derived from the subscription's Context,
//     so an overflow eviction (which cancels the sub ctx) unblocks an
//     in-flight wsjson.Write immediately. Without this the handler
//     would sit on a stale write until its 10 s timeout, then exit
//     without sending the close code that tells the peer to resync.
//
// Auth: the public listener already gates this with the Owner
// middleware, so the handler does not re-check. On the agent-facing
// listener the handler shares the same mux but EnforceMiddleware
// rejects non-Owner principals (the route is not on the AllowNonOwner
// allowlist) before the handler ever runs — only the kojo user (Owner)
// can actually reach this code.
func (s *Server) handleEventsWS(w http.ResponseWriter, r *http.Request) {
	if s.events == nil {
		// Disabled — return a clean 503 so the client knows to back off
		// rather than tight-loop a 404 retry.
		writeError(w, http.StatusServiceUnavailable, "unavailable", "event bus not configured")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Subscribe BEFORE Accept (see comment block above). ctx ties the
	// subscription to the HTTP request so a client disconnect tears
	// down the subscription via the bus's ctx watcher.
	sub, err := s.events.Subscribe(ctx)
	if err != nil {
		s.logger.Warn("events subscribe failed", "err", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "event bus not accepting subscriptions")
		return
	}
	defer sub.Cancel()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: wsOriginPatterns,
	})
	if err != nil {
		s.logger.Error("events websocket accept failed", "err", err)
		return
	}
	// Default close on any return; the success paths upgrade to a
	// graceful close with a meaningful code BEFORE returning, so
	// CloseNow only fires on truly-abrupt exits (panic, rare error).
	defer conn.CloseNow()
	// Caps client → server traffic. We do not expect any client frames
	// (it's a pure server-push channel) but a tiny limit defends against
	// a buggy client streaming garbage.
	conn.SetReadLimit(4 * 1024)

	// Drain any client frames in the background so the read pump notices
	// disconnects promptly and the websocket library can deliver Pongs.
	// Anything received from the client is discarded — events is push-only.
	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				cancel()
				return
			}
		}
	}()

	// Keepalive ping — same cadence as agent_ws.go for consistency.
	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "client gone")
			return
		case <-pingTicker.C:
			// joinedCtx for ping cancels on EITHER request ctx or
			// subscription ctx. Sub.Context() alone would mean the
			// request-cancel path has to flow through the ctx watcher
			// goroutine + bus mutex before reaching the Ping, so a
			// scheduling stall could keep the connection alive up to
			// the full 10 s timeout. context.AfterFunc fires inline.
			pingCtx, pingCancel := joinedTimeout(ctx, sub.Context(), 10*time.Second)
			err := conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				if reason := sub.Reason(); reason != "" {
					closeForReason(conn, reason)
				}
				return
			}
		case ev, ok := <-sub.C():
			if !ok {
				closeForReason(conn, sub.Reason())
				return
			}
			// Parent the write context on BOTH request ctx and
			// subscription ctx so a client disconnect or an overflow
			// eviction unblocks the write immediately, instead of
			// waiting for the 10 s timeout.
			writeCtx, writeCancel := joinedTimeout(ctx, sub.Context(), 10*time.Second)
			err := wsjson.Write(writeCtx, conn, ev)
			writeCancel()
			if err != nil {
				// If the subscription terminated mid-write, prefer
				// reporting the actual reason so the peer can pick
				// the right recovery path.
				if reason := sub.Reason(); reason != "" {
					closeForReason(conn, reason)
				}
				return
			}
		}
	}
}

// closeForReason maps an eventbus.Reason* onto a WebSocket close code
// so peers can distinguish recoverable drops (resync via cursor) from
// server-side shutdown (reconnect when server returns) from a clean
// caller close.
func closeForReason(conn *websocket.Conn, reason string) {
	switch reason {
	case eventbus.ReasonOverflow:
		conn.Close(websocket.StatusPolicyViolation,
			"subscriber dropped; resync via /api/v1/changes?since=<seq>")
	case eventbus.ReasonClosed:
		conn.Close(websocket.StatusGoingAway, "server shutting down")
	case eventbus.ReasonContext, eventbus.ReasonCaller, "":
		conn.Close(websocket.StatusNormalClosure, "subscription ended")
	default:
		// Unknown reason — be conservative, treat as policy violation
		// so the peer resyncs rather than assuming clean state.
		conn.Close(websocket.StatusPolicyViolation, "subscription ended: "+reason)
	}
}

// joinedTimeout returns a context that is cancelled when EITHER a or b
// fires (or after timeout). The returned cancel func MUST be called
// to release the AfterFunc registration even if a or b fires.
//
// Synchronization note: context.AfterFunc(b, cancel) registers cancel
// to run on its own goroutine when b fires; if b is already done at
// the time of the call, the goroutine still scheduled before
// returning leaves a tiny window where the returned ctx is live even
// though b was already cancelled. We close that window by checking
// b.Err() synchronously after registration and cancelling inline.
func joinedTimeout(a, b context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	// Parent on a so the timeout starts from now and inherits a's
	// values (request-scoped logging etc).
	ctx, cancel := context.WithTimeout(a, timeout)
	stopAfter := context.AfterFunc(b, cancel)
	if b.Err() != nil {
		cancel()
	}
	return ctx, func() {
		stopAfter()
		cancel()
	}
}

// PublishEvent is the Server-side hook for write handlers. Calls into
// the bus when configured, no-op otherwise. Always returns nil to keep
// call sites trivial — the bus reports its own internal errors via
// metrics (Bus.Dropped).
func (s *Server) PublishEvent(ev eventbus.Event) {
	if s.events == nil {
		return
	}
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	_ = s.events.Publish(ev)
}

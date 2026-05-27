package server

import (
	"context"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// peerEventsWriteTimeout bounds a single WebSocket write so a
// stalled subscriber doesn't pin the bus's broadcast goroutine.
// The handler-side ctx fires on close so this is mostly a belt-
// and-braces failsafe.
const peerEventsWriteTimeout = 10 * time.Second

// peerEventsTouchInterval is how often the handler refreshes the
// connected peer's last_seen while the WS stays open. Set to half
// the heartbeat cadence so a flaky mobile uplink that loses one
// touch still has the next touch land well inside the
// OfflineThreshold (5×HeartbeatInterval). Without this redundancy
// a single dropped touch on a long-lived idle WS would push the
// row toward stale.
const peerEventsTouchInterval = peer.HeartbeatInterval / 2

// peerEventsPingInterval is how often the handler sends a WS ping
// to the subscriber so a TCP half-open (mobile NAT timeout, carrier
// rebind) is detected without waiting for the next bus event. Ping
// is the only liveness probe that actually traverses the wire —
// the touch ticker writes to the local DB and never touches the
// socket — so a flap during the gap between two ticker fires is
// caught exclusively here. 20s balances mobile-radio wake cost
// (each ping wakes the device's modem) against detection latency;
// dropping below ~10s on cellular has measurable battery impact.
const peerEventsPingInterval = 20 * time.Second

// peerEventsPingTimeout bounds one Ping round-trip. coder/websocket
// returns an error if the deadline fires before the pong lands,
// which exits the streaming loop and lets Subscriber.runOne dial
// a fresh connection.
const peerEventsPingTimeout = 10 * time.Second

// handlePeerEventsWS streams peer_registry status mutations to a
// connected peer (docs §3.10 — "両方向 heartbeat" narrowed to push
// semantics). Auth: RolePeer (Ed25519-signed) OR RoleOwner. The
// route is opt-in via PeerEvents in Config — nil disables the
// endpoint with 503 so a misconfigured cluster fails loudly
// rather than silently going polling-only.
//
// On connect: first frame is the current peer_registry snapshot
// so the subscriber has a known starting state without needing a
// separate GET. After that the channel multiplexes Publish-driven
// events (registrar heartbeat, OfflineSweeper status flip,
// operator-initiated UpsertPeer / DeletePeer).
//
// Wire format mirrors peer.StatusEvent (snake_case JSON).
//
// Lifecycle ordering mirrors handleEventsWS:
//
//  1. Subscribe BEFORE Accept (no events dropped in the
//     handshake gap).
//  2. Snapshot AFTER Subscribe so events that fire between
//     the snapshot read and the first published event aren't
//     missed — the subscriber's channel already holds them.
//  3. ctx threads through both Subscribe and Accept so a peer
//     disconnect tears down both immediately.
func (s *Server) handlePeerEventsWS(w http.ResponseWriter, r *http.Request) {
	if s.peerEvents == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"peer events bus not configured")
		return
	}
	p := auth.FromContext(r.Context())
	if !p.IsPeer() && !p.IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden",
			"peer or owner principal required")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Fill PeerID if the Hub-public TailnetIdentityMiddleware's
	// 500ms lookup raced a DB-lock spike and left it empty. Without
	// this fallback an Owner-stamped peer-events connection would
	// silently lose the pre-snapshot touch + presence + periodic
	// touch + ping, defeating the entire redundancy effort against
	// a flaky mobile uplink. The lookup here is allowed a longer
	// timeout (2s) because peer-events is a single-connection
	// upgrade, not an every-request hot path, so the cost is
	// amortized over the lifetime of the WS.
	if p.PeerID == "" && p.IsOwner() && s.agents != nil && s.agents.Store() != nil {
		if filled, ok := s.fillOwnerPeerIDForEvents(ctx, r); ok {
			p.PeerID = filled
		}
	}

	// Subscribe before Accept so any status flip that fires during
	// the WS handshake is buffered into our channel.
	ch, unsub, err := s.peerEvents.Subscribe(ctx)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"peer events bus rejected subscription: "+err.Error())
		return
	}
	defer unsub()

	// Bump this peer's last_seen BEFORE we snapshot the registry
	// so a peer that's reconnecting after a brief disconnect — and
	// whose row may have been flipped offline by a sweep tick in
	// the gap — appears authoritatively online in the very first
	// frame the subscriber reads. Without the pre-snapshot touch
	// the freshly-reconnected peer would render offline in its
	// own UI until the next 15s tick lands.
	if p.PeerID != "" {
		// Read the row's previous status BEFORE touching so we can
		// publish a one-shot online event ONLY on a genuine
		// offline→online transition. Periodic 15s touches DON'T
		// publish (would spam the bus with no-op events that every
		// subscriber would have to filter); the reconnect-recovery
		// event lets in-process subscribers in a multi-peer cluster
		// learn of the recovery without waiting for their own next
		// snapshot.
		nowMs := store.NowMillis()
		prevStatus := ""
		readCtx, readCancel := context.WithTimeout(ctx, 1*time.Second)
		if prev, err := s.agents.Store().GetPeer(readCtx, p.PeerID); err == nil && prev != nil {
			prevStatus = prev.Status
		}
		readCancel()
		touchCtx, touchCancel := context.WithTimeout(ctx, 2*time.Second)
		touchErr := s.agents.Store().TouchPeer(touchCtx, p.PeerID, store.PeerStatusOnline, nowMs)
		touchCancel()
		// Publish the recovery event ONLY when (a) the touch
		// actually landed (no DB lock / ErrNotFound) AND (b) the
		// previous status was specifically 'offline'. The
		// touch-failure check makes sure subscribers never see an
		// online claim that we couldn't actually commit.
		if touchErr == nil && prevStatus == store.PeerStatusOffline {
			s.peerEvents.Publish(peer.StatusEvent{
				DeviceID: p.PeerID,
				Status:   store.PeerStatusOnline,
				LastSeen: nowMs,
				Op:       peer.StatusOpTouch,
			})
		}
	}

	// Snapshot the current peer_registry rows BEFORE accepting so
	// the subscriber's first read sees a complete starting state.
	// Any concurrent mutation is captured by ch and delivered
	// after the snapshot frame.
	rows, err := s.agents.Store().ListPeers(ctx, store.ListPeersOptions{})
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"peer_registry read: "+err.Error())
		return
	}

	// Wrap the presence release in a deferred closure registered
	// BEFORE conn.Close. defer LIFO then runs conn.Close first,
	// release second — so the socket is fully closed before the
	// next sweep tick sees presence drop the row. release stays
	// nil until AddConn lands (after Accept succeeds).
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: wsOriginPatterns,
	})
	if err != nil {
		// Accept already wrote a response on failure.
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Snapshot frame.
	snapshotFrame := struct {
		Type  string             `json:"type"`
		Peers []peer.StatusEvent `json:"peers"`
	}{
		Type:  "snapshot",
		Peers: peerRecordsToStatusEvents(rows),
	}
	writeCtx, writeCancel := context.WithTimeout(ctx, peerEventsWriteTimeout)
	if err := wsjson.Write(writeCtx, conn, snapshotFrame); err != nil {
		writeCancel()
		s.logger.Warn("peer events: write snapshot failed", "err", err)
		return
	}
	writeCancel()

	// Streaming loop. AuthMiddleware did one TouchPeer on
	// upgrade; without a periodic refresh OfflineSweeper would
	// flip this peer's row offline after ~5×HeartbeatInterval
	// (150s) even while the WS stays open. Gate the periodic
	// path on p.PeerID (not p.IsPeer) so Hub-mode connections —
	// where TailnetIdentityMiddleware stamps RoleOwner+PeerID
	// because the public listener trusts every tailnet caller —
	// also benefit. Touch immediately on connect (don't wait
	// for the first tick) so a peer whose row aged out before
	// reconnect comes back online without a 15s lag, register
	// the connection in the in-memory presence set so the
	// OfflineSweeper can skip it entirely on flaky uplinks, and
	// run a WS ping ticker so a half-open TCP (mobile NAT rebind,
	// LTE→Wi-Fi handover) drops fast and the subscriber redials.
	var touchTick <-chan time.Time
	var pingTick <-chan time.Time
	if p.PeerID != "" {
		if s.peerPresence != nil {
			release = s.peerPresence.AddConn(p.PeerID)
		}
		t := time.NewTicker(peerEventsTouchInterval)
		defer t.Stop()
		touchTick = t.C
		pt := time.NewTicker(peerEventsPingInterval)
		defer pt.Stop()
		pingTick = pt.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-touchTick:
			touchCtx, touchCancel := context.WithTimeout(ctx, 2*time.Second)
			_ = s.agents.Store().TouchPeer(touchCtx, p.PeerID, store.PeerStatusOnline, store.NowMillis())
			touchCancel()
		case <-pingTick:
			pingCtx, pingCancel := context.WithTimeout(ctx, peerEventsPingTimeout)
			err := conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				// Half-open detected — exit so Subscriber.runOne
				// dials a fresh connection. The fresh upgrade
				// re-touches last_seen + re-Adds presence.
				return
			}
		case evt, ok := <-ch:
			if !ok {
				// Bus closed our subscription (overflow or shutdown).
				conn.Close(websocket.StatusPolicyViolation,
					"event channel closed; resync via /api/v1/peers")
				return
			}
			frame := struct {
				Type  string           `json:"type"`
				Event peer.StatusEvent `json:"event"`
			}{
				Type:  "event",
				Event: evt,
			}
			writeCtx, writeCancel := context.WithTimeout(ctx, peerEventsWriteTimeout)
			err := wsjson.Write(writeCtx, conn, frame)
			writeCancel()
			if err != nil {
				// Client gone / timeout — exit cleanly.
				return
			}
		}
	}
}

// fillOwnerPeerIDForEvents resolves the WhoIs lookup for the
// current request when the Hub-public middleware's 500ms attempt
// returned an empty PeerID. Single-connection upgrade so a 2s
// lookup window is acceptable. Returns ("", false) on resolver
// miss, DB miss, or DB blip — caller proceeds as plain Owner
// (events still flow, the redundancy paths are disabled).
func (s *Server) fillOwnerPeerIDForEvents(ctx context.Context, r *http.Request) (string, bool) {
	if s == nil || s.agents == nil {
		return "", false
	}
	st := s.agents.Store()
	if st == nil {
		return "", false
	}
	resolveCtx, resolveCancel := context.WithTimeout(ctx, 2*time.Second)
	nodeKey, err := s.resolveNodeKey(resolveCtx, r.RemoteAddr)
	resolveCancel()
	if err != nil || nodeKey == "" {
		return "", false
	}
	lookupCtx, lookupCancel := context.WithTimeout(ctx, 2*time.Second)
	rec, err := st.GetPeerByNodeKey(lookupCtx, nodeKey)
	lookupCancel()
	if err != nil || rec == nil || rec.DeviceID == "" {
		return "", false
	}
	if s.peerID != nil && rec.DeviceID == s.peerID.DeviceID {
		// Self-loop guard: never treat a loopback request as a
		// remote peer connection.
		return "", false
	}
	return rec.DeviceID, true
}

// peerRecordsToStatusEvents materializes the snapshot frame's
// payload from the live peer_registry rows. The "op" field is set
// to "upsert" for every row because the snapshot represents the
// current state, not a delta — subscribers MUST treat these as
// authoritative-on-arrival rather than as additive change events.
func peerRecordsToStatusEvents(rows []*store.PeerRecord) []peer.StatusEvent {
	out := make([]peer.StatusEvent, 0, len(rows))
	for _, r := range rows {
		out = append(out, peer.StatusEvent{
			DeviceID: r.DeviceID,
			Name:     r.Name,
			Status:   r.Status,
			LastSeen: r.LastSeen,
			Op:       peer.StatusOpUpsert,
		})
	}
	return out
}

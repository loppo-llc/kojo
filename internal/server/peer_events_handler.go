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

	// Subscribe before Accept so any status flip that fires during
	// the WS handshake is buffered into our channel.
	ch, unsub, err := s.peerEvents.Subscribe(ctx)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"peer events bus rejected subscription: "+err.Error())
		return
	}
	defer unsub()

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
		Type  string              `json:"type"`
		Peers []peer.StatusEvent  `json:"peers"`
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

	// Streaming loop. AuthMiddleware did a single TouchPeer on
	// the upgrade — without periodic refresh, OfflineSweeper
	// would flip the remote peer's row offline after
	// ~5×HeartbeatInterval (150s) even while the WS is open.
	// Fire one TouchPeer per HeartbeatInterval as long as the
	// connection stays up.
	var touchTick <-chan time.Time
	if p.IsPeer() {
		t := time.NewTicker(peer.HeartbeatInterval)
		defer t.Stop()
		touchTick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-touchTick:
			touchCtx, touchCancel := context.WithTimeout(ctx, 2*time.Second)
			_ = s.agents.Store().TouchPeer(touchCtx, p.PeerID, store.PeerStatusOnline, store.NowMillis())
			touchCancel()
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

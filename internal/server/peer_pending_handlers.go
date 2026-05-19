package server

// Peer onboarding HTTP surface (docs/peer-tsnet-identity.md).
//
// Auto-pairing flow:
//
//   1. peer mode boots, learns Hub URL (--hub / KOJO_HUB_URL /
//      MagicDNS default).
//   2. peer GETs /api/v1/peers/hub-info to learn Hub's
//      {deviceId, name, url}, writes that into local peer_registry.
//   3. peer POSTs /api/v1/peers/join-request with {deviceId, name,
//      url}. The peer does NOT send its NodeKey — the Hub reads it
//      from the inbound HTTP request via tsnet.LocalClient.WhoIs.
//      Hub answers one of:
//        - state="approved" + hub spec   (already in peer_registry,
//                                         WhoIs NodeKey matches)
//        - state="pending"               (parked in peer_pending,
//                                         awaiting Owner Approve)
//        - 409 conflict                  (NodeKey mismatch — operator
//                                         must delete the stale row)
//   4. peer polls GET /api/v1/peers/join-request/{deviceId} every 60s
//      until approved. STOPS the loop once approved.
//   5. Owner clicks Approve in Settings → peer_pending row is
//      promoted into peer_registry (carrying NodeKey); next poll
//      returns approved.
//
// hub-info + join-request are unauthenticated at the HTTP layer
// (the requesting peer has no credential on the Hub yet). Identity
// is verified at the network layer via tsnet WhoIs; --unsafe mode
// skips WhoIs and admits every caller (LAN / docker / CI escape
// hatch).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// isNodeKeyUniqueViolation matches SQLite's UNIQUE-constraint error
// strings for the partial indexes on peer_registry.node_key and
// peer_pending.node_key. Both modernc.org/sqlite and mattn/go-sqlite3
// emit the substring "UNIQUE constraint failed" verbatim, so a
// strings.Contains is safe across drivers without pulling in a
// driver-specific error type.
func isNodeKeyUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") &&
		strings.Contains(msg, "node_key")
}

// hubInfoResponse is the wire shape of GET /api/v1/peers/hub-info.
//
// NodeKey is the Hub's Tailscale stable NodeKey. The peer writes it
// into its local peer_registry so when the Hub later dials the peer
// (Subscriber WS, blob push, agent-sync), the peer's tsnet identity
// middleware can resolve Hub's NodeKey → peer_registry row →
// RolePeer and admit the request. Empty until tsnet finishes its
// login handshake; the peer's discovery loop tolerates that and
// re-fetches on the next tick.
//
// ProtocolVersion advertises the pairing protocol the Hub speaks
// (see peer.PairingProtocolVersion). A peer whose own version
// differs MUST refuse to write the Hub row / send a join-request;
// the Hub also re-validates this on /join-request so the gap
// surfaces explicitly rather than silently re-pairing under a
// stale contract.
type hubInfoResponse struct {
	DeviceID        string `json:"deviceId"`
	Name            string `json:"name"`
	URL             string `json:"url"`
	NodeKey         string `json:"nodeKey,omitempty"`
	Version         string `json:"version"`
	ProtocolVersion int    `json:"protocolVersion"`
}

// handleHubInfo returns the Hub's identity row + dial URL so a
// peer can populate its peer_registry before sending its first
// join-request. Unauthenticated by design.
func (s *Server) handleHubInfo(w http.ResponseWriter, r *http.Request) {
	if s.peerID == nil || s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"peer registry not initialized")
		return
	}
	rec, err := s.agents.Store().GetPeer(r.Context(), s.peerID.DeviceID)
	if err != nil {
		writeJSONResponse(w, http.StatusOK, hubInfoResponse{
			DeviceID:        s.peerID.DeviceID,
			Name:            s.peerID.Name,
			Version:         s.version,
			ProtocolVersion: peer.PairingProtocolVersion,
		})
		return
	}
	writeJSONResponse(w, http.StatusOK, hubInfoResponse{
		DeviceID:        rec.DeviceID,
		Name:            rec.Name,
		URL:             rec.URL,
		NodeKey:         rec.NodeKey,
		Version:         s.version,
		ProtocolVersion: peer.PairingProtocolVersion,
	})
}

// joinRequestBody is the wire shape a peer POSTs to
// /api/v1/peers/join-request.
//
// ProtocolVersion advertises the pairing protocol the caller speaks
// (see peer.PairingProtocolVersion). The Hub rejects any value that
// does not equal its own constant so a v1 peer (Bearer-era) and a
// v2 Hub (NodeKey-only) cannot accidentally re-pair under a stale
// auth contract. Zero / missing is treated as legacy and rejected.
type joinRequestBody struct {
	DeviceID        string `json:"deviceId"`
	Name            string `json:"name"`
	URL             string `json:"url"`
	ProtocolVersion int    `json:"protocolVersion"`
}

// joinRequestResponse is what the Hub answers.
type joinRequestResponse struct {
	State string           `json:"state"` // "approved" | "pending"
	Hub   *hubInfoResponse `json:"hub,omitempty"`
}

// joinRequestBodyCap bounds the join-request body. 16 KiB is a
// generous wire ceiling against a buggy / hostile peer.
const joinRequestBodyCap = 16 * 1024

// callerNodeKey resolves the inbound request's caller to a
// Tailscale NodeKey. Returns "" when the listener is not tsnet
// (--local + --unsafe) or when the resolver fails; the handler
// uses (nodeKey, unsafe) together to decide whether "" is allowed
// (unsafe accepts, secure rejects).
//
// Under --unsafe we deliberately return "" rather than a synthetic
// sentinel: a single sentinel would collide on the partial UNIQUE
// index the moment a second --unsafe peer joined. The empty value
// is excluded from that index, so multiple --unsafe rows coexist.
func (s *Server) callerNodeKey(ctx context.Context, r *http.Request) string {
	if s.unsafePeer {
		return ""
	}
	s.identityMu.RLock()
	fn := s.nodeKeyResolver
	s.identityMu.RUnlock()
	if fn == nil {
		return ""
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	nk, err := fn(rctx, r.RemoteAddr)
	if err != nil {
		return ""
	}
	return nk
}

// handleJoinRequest is the auto-pairing entry point. Peer side
// posts {deviceId, name, url}; Hub reads NodeKey from WhoIs and
// answers approved/pending/409. Unauthenticated at the HTTP layer.
func (s *Server) handleJoinRequest(w http.ResponseWriter, r *http.Request) {
	if s.peerID == nil || s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"peer registry not initialized")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, joinRequestBodyCap)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge,
				"payload_too_large", "request body exceeds 16 KiB cap")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request",
			"invalid request body")
		return
	}
	var req joinRequestBody
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"invalid JSON")
		return
	}
	if err := peer.ValidateDeviceID(req.DeviceID); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := peer.ValidateName(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !peer.IsDialAddress(req.URL) {
		writeError(w, http.StatusBadRequest, "bad_request",
			"url must be host:port or http(s)://host:port")
		return
	}
	// Pairing-protocol version gate. A mismatch means the caller was
	// built against a different auth contract (e.g. v1 Bearer-era
	// vs. v2 NodeKey-only); silently accepting the row would leave
	// the registry in a state where the §3.7 inter-peer surface
	// later 403s under conditions the operator can't diagnose.
	// Reject explicitly so the upgrade gap surfaces at pairing time.
	if req.ProtocolVersion != peer.PairingProtocolVersion {
		writeError(w, http.StatusBadRequest, "protocol_version_mismatch",
			fmt.Sprintf("caller speaks pairing protocol v%d; this Hub speaks v%d — upgrade both ends to the same kojo release",
				req.ProtocolVersion, peer.PairingProtocolVersion))
		return
	}
	if req.DeviceID == s.peerID.DeviceID {
		writeError(w, http.StatusConflict, "conflict",
			"deviceId collides with Hub self-row")
		return
	}
	nodeKey := s.callerNodeKey(r.Context(), r)
	if nodeKey == "" && !s.unsafePeer {
		// No identity resolvable. With --unsafe disabled this means
		// the Hub cannot bind the caller to a Tailscale node; we
		// REFUSE rather than admit an anonymous claim that would
		// poison the registry.
		writeError(w, http.StatusUnauthorized, "no_tailnet_identity",
			"caller has no Tailscale NodeKey; bind kojo on a tsnet listener or pass --unsafe")
		return
	}
	s.processJoinRequest(w, r, req, nodeKey)
}

// handleJoinRequestPoll is the GET companion to POST
// /api/v1/peers/join-request. Peer side uses it to poll for Approve
// without re-shipping its full identity body.
//
// The handler validates the WhoIs-resolved NodeKey against the
// stored binding (peer_registry.node_key when approved,
// peer_pending.node_key when pending) so an attacker who learns the
// device_id but lives on a different tailnet node cannot
// impersonate the legitimate peer.
func (s *Server) handleJoinRequestPoll(w http.ResponseWriter, r *http.Request) {
	if s.peerID == nil || s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"peer registry not initialized")
		return
	}
	id := r.PathValue("deviceId")
	if err := peer.ValidateDeviceID(id); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	callerNodeKey := s.callerNodeKey(r.Context(), r)
	if callerNodeKey == "" && !s.unsafePeer {
		writeError(w, http.StatusUnauthorized, "no_tailnet_identity",
			"caller has no Tailscale NodeKey")
		return
	}
	st := s.agents.Store()
	if rec, err := st.GetPeer(r.Context(), id); err == nil {
		// Skip the NodeKey gate under --unsafe (every caller has
		// an empty NodeKey there; the operator opted into trusting
		// the listener).
		if !s.unsafePeer && rec.NodeKey != "" && rec.NodeKey != callerNodeKey {
			writeError(w, http.StatusConflict, "node_key_mismatch",
				"deviceId is bound to a different Tailscale node; operator must delete the stale registry row")
			return
		}
		hub := s.buildHubInfoResponse(r.Context())
		writeJSONResponse(w, http.StatusOK, joinRequestResponse{State: "approved", Hub: hub})
		return
	}
	if rec, err := st.GetPeerPending(r.Context(), id); err == nil {
		if !s.unsafePeer && rec.NodeKey != "" && rec.NodeKey != callerNodeKey {
			writeError(w, http.StatusConflict, "node_key_mismatch",
				"deviceId is bound to a different Tailscale node; operator must reject the stale pending row")
			return
		}
		writeJSONResponse(w, http.StatusOK, joinRequestResponse{
			State: "pending",
		})
		return
	}
	writeError(w, http.StatusNotFound, "not_found",
		"no join request on file for this deviceId")
}

// processJoinRequest is the POST decision tree.
func (s *Server) processJoinRequest(w http.ResponseWriter, r *http.Request, req joinRequestBody, nodeKey string) {
	st := s.agents.Store()

	// NodeKey collision early-reject. Empty NodeKey skips the
	// check (unsafe mode); a real WhoIs-resolved value must not
	// already be bound to a different device_id. Race-proof
	// backstop: peer_registry.node_key + peer_pending.node_key
	// carry partial UNIQUE indexes (migrations 0013/0014) so a
	// concurrent insert that sneaks past this check still fails
	// at the SQL layer with a constraint violation rather than
	// silently double-binding the NodeKey.
	if nodeKey != "" {
		if existing, err := st.GetPeerByNodeKey(r.Context(), nodeKey); err == nil && existing.DeviceID != req.DeviceID {
			writeError(w, http.StatusConflict, "node_key_collision",
				"this Tailscale node is already paired as a different deviceId")
			return
		}
		if existing, err := st.GetPeerPendingByNodeKey(r.Context(), nodeKey); err == nil && existing.DeviceID != req.DeviceID {
			writeError(w, http.StatusConflict, "node_key_collision",
				"this Tailscale node already has a pending join request under a different deviceId")
			return
		}
	}

	rec, err := st.GetPeer(r.Context(), req.DeviceID)
	switch {
	case err == nil:
		// Existing registry row. Reject if the NodeKey differs.
		// Empty incoming NodeKey (unsafe mode) skips the check.
		if nodeKey != "" && rec.NodeKey != "" && rec.NodeKey != nodeKey {
			writeError(w, http.StatusConflict, "node_key_mismatch",
				"deviceId is bound to a different Tailscale node; operator must delete the stale registry row")
			return
		}
		// Refresh name/url + carry NodeKey (back-fill empty column).
		// A UNIQUE-constraint violation here means another row in
		// peer_registry already holds the same NodeKey under a
		// different device_id — the early collision check raced
		// and lost. Surface a clean 409 so the operator can
		// reconcile.
		if _, err := st.UpsertPeer(r.Context(), &store.PeerRecord{
			DeviceID: req.DeviceID,
			Name:     req.Name,
			URL:      req.URL,
			NodeKey:  nodeKey,
			LastSeen: time.Now().UnixMilli(),
			Status:   store.PeerStatusOnline,
		}); err != nil {
			if isNodeKeyUniqueViolation(err) {
				writeError(w, http.StatusConflict, "node_key_collision",
					"this Tailscale node is already paired as a different deviceId")
				return
			}
			s.logger.Error("join-request: refresh registry", "device_id", req.DeviceID, "err", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
		hub := s.buildHubInfoResponse(r.Context())
		writeJSONResponse(w, http.StatusOK, joinRequestResponse{State: "approved", Hub: hub})
		return
	case errors.Is(err, store.ErrNotFound):
		// Fall through to pending.
	default:
		s.logger.Error("join-request: registry lookup", "device_id", req.DeviceID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"internal server error")
		return
	}

	// Pending path. Check whether a pending row already exists with
	// the same device_id; if so, verify NodeKey match before
	// touching name/url.
	if existing, err := st.GetPeerPending(r.Context(), req.DeviceID); err == nil {
		if nodeKey != "" && existing.NodeKey != "" && existing.NodeKey != nodeKey {
			writeError(w, http.StatusConflict, "node_key_mismatch",
				"deviceId already has a pending request from a different Tailscale node")
			return
		}
		if _, err := st.UpsertPeerPending(r.Context(), &store.PeerPendingRecord{
			DeviceID: req.DeviceID,
			Name:     req.Name,
			URL:      req.URL,
			NodeKey:  nodeKey,
		}); err != nil {
			if isNodeKeyUniqueViolation(err) {
				writeError(w, http.StatusConflict, "node_key_collision",
					"this Tailscale node already has a pending join request under a different deviceId")
				return
			}
			s.logger.Error("join-request: upsert pending", "device_id", req.DeviceID, "err", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
		writeJSONResponse(w, http.StatusOK, joinRequestResponse{State: "pending"})
		return
	}

	// Fresh pending row. UNIQUE-constraint violation on node_key
	// means a concurrent join from the same NodeKey under a
	// different device_id beat us to the insert — surface 409.
	out, inserted, err := st.InsertPeerPendingIfAbsent(r.Context(), &store.PeerPendingRecord{
		DeviceID: req.DeviceID,
		Name:     req.Name,
		URL:      req.URL,
		NodeKey:  nodeKey,
	})
	if err != nil {
		if isNodeKeyUniqueViolation(err) {
			writeError(w, http.StatusConflict, "node_key_collision",
				"this Tailscale node already has a pending join request under a different deviceId")
			return
		}
		s.logger.Error("join-request: insert pending", "device_id", req.DeviceID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if !inserted && nodeKey != "" && out != nil && out.NodeKey != "" && out.NodeKey != nodeKey {
		// A pending row already exists for this device_id but
		// under a different NodeKey. The InsertIfAbsent treated
		// our call as a no-op; we MUST NOT echo 200 because the
		// caller's identity does not match the row.
		writeError(w, http.StatusConflict, "node_key_mismatch",
			"deviceId already has a pending request from a different Tailscale node")
		return
	}
	writeJSONResponse(w, http.StatusOK, joinRequestResponse{State: "pending"})
}

// buildHubInfoResponse mirrors handleHubInfo without HTTP plumbing.
func (s *Server) buildHubInfoResponse(ctx context.Context) *hubInfoResponse {
	if s.peerID == nil || s.agents == nil || s.agents.Store() == nil {
		return nil
	}
	rec, err := s.agents.Store().GetPeer(ctx, s.peerID.DeviceID)
	if err != nil {
		return &hubInfoResponse{
			DeviceID:        s.peerID.DeviceID,
			Name:            s.peerID.Name,
			Version:         s.version,
			ProtocolVersion: peer.PairingProtocolVersion,
		}
	}
	return &hubInfoResponse{
		DeviceID:        rec.DeviceID,
		Name:            rec.Name,
		URL:             rec.URL,
		NodeKey:         rec.NodeKey,
		Version:         s.version,
		ProtocolVersion: peer.PairingProtocolVersion,
	}
}

// peerPendingResponse mirrors PeerPendingRecord on the wire.
type peerPendingResponse struct {
	DeviceID  string `json:"deviceId"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	FirstSeen int64  `json:"firstSeen"`
	LastSeen  int64  `json:"lastSeen"`
}

type peerPendingListResponse struct {
	Items []peerPendingResponse `json:"items"`
}

// handleListPeerPending returns every pending row. Owner-only.
func (s *Server) handleListPeerPending(w http.ResponseWriter, r *http.Request) {
	if !auth.FromContext(r.Context()).IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden",
			"pending peer requests are owner-only")
		return
	}
	if s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"peer registry not initialized")
		return
	}
	rows, err := s.agents.Store().ListPeerPending(r.Context())
	if err != nil {
		s.logger.Error("pending: list failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"internal server error")
		return
	}
	out := peerPendingListResponse{Items: make([]peerPendingResponse, 0, len(rows))}
	for _, rec := range rows {
		out.Items = append(out.Items, peerPendingResponse{
			DeviceID:  rec.DeviceID,
			Name:      rec.Name,
			URL:       rec.URL,
			FirstSeen: rec.FirstSeen,
			LastSeen:  rec.LastSeen,
		})
	}
	writeJSONResponse(w, http.StatusOK, out)
}

// handleApprovePeerPending promotes a pending row into peer_registry.
// Owner-only. With Bearer issuance retired (docs/peer-tsnet-identity.md),
// approve is now a pure metadata promotion: no token mint, no roll-back
// path. The peer authenticates on its next inter-peer request via
// tsnet WhoIs.
func (s *Server) handleApprovePeerPending(w http.ResponseWriter, r *http.Request) {
	if !auth.FromContext(r.Context()).IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden",
			"approve is owner-only")
		return
	}
	if s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"peer registry not initialized")
		return
	}
	id := r.PathValue("deviceId")
	if err := peer.ValidateDeviceID(id); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	rec, err := s.agents.Store().ApprovePeerPending(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found",
				"no pending join request for this deviceId")
			return
		}
		s.logger.Error("approve: failed", "device_id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"internal server error")
		return
	}
	writeJSONResponse(w, http.StatusOK, s.toPeerResponse(rec))
}

// handleRejectPeerPending drops a pending row without promoting.
// Owner-only. Idempotent.
func (s *Server) handleRejectPeerPending(w http.ResponseWriter, r *http.Request) {
	if !auth.FromContext(r.Context()).IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden",
			"reject is owner-only")
		return
	}
	if s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"peer registry not initialized")
		return
	}
	id := r.PathValue("deviceId")
	if err := peer.ValidateDeviceID(id); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := s.agents.Store().DeletePeerPending(r.Context(), id); err != nil {
		s.logger.Error("reject: failed", "device_id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

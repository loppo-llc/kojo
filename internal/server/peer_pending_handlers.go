package server

// Peer onboarding HTTP surface (docs/peer-onboarding-plan.md).
//
// Auto-pairing flow:
//
//   1. peer mode boots, learns Hub URL (--hub / KOJO_HUB_URL /
//      MagicDNS default).
//   2. peer GETs /api/v1/peers/hub-info to learn Hub's
//      {deviceId, name, publicKey, url}, writes that into local
//      peer_registry (trusted=true).
//   3. peer POSTs /api/v1/peers/join-request with its own identity.
//      Hub answers one of:
//        - state="approved" + hub spec   (already in registry)
//        - state="pending"               (parked in peer_pending,
//                                         awaiting Owner Approve)
//   4. peer polls join-request every 60s until approved.
//   5. Owner clicks Approve in Settings → peer_pending row is
//      promoted into peer_registry (trusted=true), pending row
//      deleted; next poll returns approved.
//
// hub-info + join-request are UNAUTHENTICATED (the requesting peer
// has no credential on the Hub yet). They live on the public
// listener which `OwnerOnlyMiddleware` already promotes to Owner
// for any Tailscale-reachable caller; the handlers themselves treat
// the body as the source of truth and never read the Principal.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// hubInfoResponse is the wire shape of GET /api/v1/peers/hub-info.
// The peer writes this into its local peer_registry (trusted=true)
// before sending the first join-request — that's how Hub becomes a
// signing-key the peer's PeerAuth middleware trusts.
//
// `version` carries the Hub binary's version string so a peer can
// log a useful mismatch warning if it ever needs to.
type hubInfoResponse struct {
	DeviceID  string `json:"deviceId"`
	Name      string `json:"name"`
	PublicKey string `json:"publicKey"`
	URL       string `json:"url"`
	Version   string `json:"version"`
}

// handleHubInfo returns the Hub's identity row + dial URL so a
// peer can populate its peer_registry before signing its first
// join-request. Unauthenticated by design (the peer has no
// credential yet); the response carries only the public identity
// material that any tailnet member could otherwise learn by
// inspecting a signed inter-peer request.
func (s *Server) handleHubInfo(w http.ResponseWriter, r *http.Request) {
	if s.peerID == nil || s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"peer registry not initialized")
		return
	}
	rec, err := s.agents.Store().GetPeer(r.Context(), s.peerID.DeviceID)
	if err != nil {
		// Pre-heartbeat boot — fall back to the in-memory Identity
		// so the peer can still proceed. URL ends up empty; the
		// peer's NormalizeAddress(...) call will refuse and the
		// peer retries hub-info on its next discovery tick.
		writeJSONResponse(w, http.StatusOK, hubInfoResponse{
			DeviceID:  s.peerID.DeviceID,
			Name:      s.peerID.Name,
			PublicKey: s.peerID.PublicKeyBase64(),
			Version:   s.version,
		})
		return
	}
	writeJSONResponse(w, http.StatusOK, hubInfoResponse{
		DeviceID:  rec.DeviceID,
		Name:      rec.Name,
		PublicKey: rec.PublicKey,
		URL:       rec.URL,
		Version:   s.version,
	})
}

// joinRequestBody is the wire shape a peer POSTs to
// /api/v1/peers/join-request.
type joinRequestBody struct {
	DeviceID  string `json:"deviceId"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	PublicKey string `json:"publicKey"`
}

// joinRequestResponse is what the Hub answers. state="approved"
// means the peer is already in peer_registry and its public_key
// matches; state="pending" means a peer_pending row was upserted
// and the operator must Approve.
//
// When state="approved", Hub carries its own pairing spec in `hub`
// so the peer can populate / refresh its local registry without a
// second round-trip to /hub-info.
//
// PeerBearer / HubBearer are the two halves of the Bearer-over-TLS
// pair docs/peer-simplify-plan.md introduces. They are delivered
// EXACTLY ONCE per approve event: the first /join-request poll
// returning state="approved" after the operator approves carries
// the raw values; subsequent polls return state="approved" with
// the bearer fields empty. The peer side MUST persist both on
// first receipt:
//   - PeerBearer (the peer→Hub credential) → peer kv outbound
//   - HubBearer (the Hub→peer credential) → peer_tokens hash row
//
// A peer that crashes between receiving and persisting must
// trigger operator re-approve to mint a fresh pair; the same raw
// tokens never come back over the wire.
type joinRequestResponse struct {
	State      string           `json:"state"` // "approved" | "pending"
	Hub        *hubInfoResponse `json:"hub,omitempty"`
	PeerBearer string           `json:"peerBearer,omitempty"`
	HubBearer  string           `json:"hubBearer,omitempty"`
}

// joinRequestBodyCap bounds the join-request body. A few hundred
// bytes is realistic; 16 KiB is a generous wire ceiling against a
// buggy / hostile peer.
const joinRequestBodyCap = 16 * 1024

// handleJoinRequest is the auto-pairing entry point. Peer side
// posts {deviceId, name, url, publicKey}; Hub answers approved or
// pending. Unauthenticated by design (see file-header comment).
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
	if err := peer.ValidatePublicKey(req.PublicKey); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.DeviceID == s.peerID.DeviceID {
		writeError(w, http.StatusConflict, "conflict",
			"deviceId collides with Hub self-row")
		return
	}
	s.processJoinRequest(w, r, req)
}

// handleJoinRequestPoll is the GET companion to POST
// /api/v1/peers/join-request. peer side uses it to poll for
// Approve without re-shipping its full identity body. The path
// param carries deviceId; the response shape matches the POST.
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
	st := s.agents.Store()
	// approved branch: peer_registry row exists AND is trusted.
	// Untrusted rows are still in the "operator must approve"
	// state from the peer's point of view — answer pending below.
	if rec, err := st.GetPeer(r.Context(), id); err == nil && rec.Trusted {
		hub := s.buildHubInfoResponse(r.Context())
		resp := joinRequestResponse{State: "approved", Hub: hub}
		s.attachPairingBearers(r.Context(), id, &resp)
		writeJSONResponse(w, http.StatusOK, resp)
		return
	}
	// pending branch: still in peer_pending.
	if _, err := st.GetPeerPending(r.Context(), id); err == nil {
		writeJSONResponse(w, http.StatusOK, joinRequestResponse{
			State: "pending",
		})
		return
	}
	// Neither — the peer must POST a fresh join-request to start.
	writeError(w, http.StatusNotFound, "not_found",
		"no join request on file for this deviceId")
}

// processJoinRequest is the POST decision tree. Centralised so a
// future channel (e.g. signed re-pair) reuses the same logic.
func (s *Server) processJoinRequest(w http.ResponseWriter, r *http.Request, req joinRequestBody) {
	st := s.agents.Store()
	existing, err := st.GetPeer(r.Context(), req.DeviceID)
	switch {
	case err == nil:
		// Existing registry row. public_key immutability: a
		// peer that has rotated keys cannot silently re-pair —
		// Owner must `kojo --peer-remove` first.
		if existing.PublicKey != req.PublicKey {
			writeError(w, http.StatusConflict, "conflict",
				"public_key disagrees with existing registry row; ask Owner to --peer-remove and re-pair")
			return
		}
		// Trust gate. The auto-onboarding flow's contract is
		// "Approve → trusted=true"; an existing untrusted row
		// means the peer was paired via `--peer-add` (no
		// --trusted) or had its trust revoked. Either way,
		// it must NOT be auto-promoted just because the peer
		// retransmitted its join-request. Fall through to
		// pending so the Owner sees it in Settings → Approve.
		if !existing.Trusted {
			break
		}
		// Refresh name/url + last_seen so the operator-visible
		// row tracks the peer's current advertised address.
		_ = st.UpdatePeerMetadata(r.Context(), req.DeviceID, req.Name, req.URL)
		_ = st.TouchPeer(r.Context(), req.DeviceID, store.PeerStatusOnline, 0)
		hub := s.buildHubInfoResponse(r.Context())
		resp := joinRequestResponse{State: "approved", Hub: hub}
		s.attachPairingBearers(r.Context(), req.DeviceID, &resp)
		writeJSONResponse(w, http.StatusOK, resp)
		return
	case errors.Is(err, store.ErrNotFound):
		// Fall through to pending.
	default:
		s.logger.Error("join-request: registry lookup", "device_id", req.DeviceID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"internal server error")
		return
	}

	pending := &store.PeerPendingRecord{
		DeviceID:  req.DeviceID,
		Name:      req.Name,
		URL:       req.URL,
		PublicKey: req.PublicKey,
	}
	if _, err := st.UpsertPeerPending(r.Context(), pending); err != nil {
		s.logger.Error("join-request: upsert pending", "device_id", req.DeviceID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"internal server error")
		return
	}
	writeJSONResponse(w, http.StatusOK, joinRequestResponse{
		State: "pending",
	})
}

// buildHubInfoResponse mirrors handleHubInfo without HTTP plumbing.
// Used by the approved-branch response so the peer learns the Hub
// spec in the same round-trip.
func (s *Server) buildHubInfoResponse(ctx context.Context) *hubInfoResponse {
	if s.peerID == nil || s.agents == nil || s.agents.Store() == nil {
		return nil
	}
	rec, err := s.agents.Store().GetPeer(ctx, s.peerID.DeviceID)
	if err != nil {
		return &hubInfoResponse{
			DeviceID:  s.peerID.DeviceID,
			Name:      s.peerID.Name,
			PublicKey: s.peerID.PublicKeyBase64(),
			Version:   s.version,
		}
	}
	return &hubInfoResponse{
		DeviceID:  rec.DeviceID,
		Name:      rec.Name,
		PublicKey: rec.PublicKey,
		URL:       rec.URL,
		Version:   s.version,
	}
}

// peerPendingResponse mirrors PeerPendingRecord on the wire. JSON
// keys follow the camelCase convention used elsewhere in the peer
// HTTP surface.
type peerPendingResponse struct {
	DeviceID  string `json:"deviceId"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	PublicKey string `json:"publicKey"`
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
			PublicKey: rec.PublicKey,
			FirstSeen: rec.FirstSeen,
			LastSeen:  rec.LastSeen,
		})
	}
	writeJSONResponse(w, http.StatusOK, out)
}

// handleApprovePeerPending promotes a pending row into
// peer_registry (trusted=true). Owner-only.
//
// Echoes the resulting peer_registry row + fans out the new row
// to other paired peers (same path RegisterPeerMetadata uses) so
// the cluster converges without manual re-pairing.
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
		if errors.Is(err, store.ErrPeerPendingPubkeyMismatch) {
			writeError(w, http.StatusConflict, "conflict",
				"existing peer_registry row has a different public_key; --peer-remove first")
			return
		}
		s.logger.Error("approve: failed", "device_id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"internal server error")
		return
	}
	// Mint the Bearer pair (docs/peer-simplify-plan.md step 4) and
	// stash both raw values for the next /join-request poll to
	// consume. A mint failure must NOT block approval: the legacy
	// Ed25519 path is still in place during the dual-stack window,
	// so a peer that never receives Bearers keeps functioning on
	// signing until the operator re-approves manually. Log + carry
	// on rather than rolling back the registry promotion.
	if err := s.mintAndStashPairingBearers(r.Context(), id); err != nil {
		s.logger.Warn("approve: bearer mint failed; peer will fall back to signing",
			"device_id", id, "err", err)
	}
	// No fan-out broadcast — sibling peers learn about this row when
	// they next GET /api/v1/peers from the Hub.
	writeJSONResponse(w, http.StatusOK, s.toPeerResponse(rec))
}

// handleRejectPeerPending drops a pending row without promoting.
// Owner-only. Idempotent — DeletePeerPending returns nil on a
// missing row, so a stale browser tab can Reject the same row
// twice without surfacing a 404.
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

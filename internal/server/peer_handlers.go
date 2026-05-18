package server

// Peer registry HTTP surface (Phase G slice 2).
//
// These handlers expose `peer_registry` rows so the Web UI (and a
// future inter-peer subscriber) can list cluster members, identify the
// local device, register a remote peer the operator just paired with,
// and decommission a peer that has been retired.
//
// Auth: every route is gated by OwnerOnlyMiddleware on the public
// listener. The agent-facing listener does not include /api/v1/peers
// in its allowlist — agent Bearer principals cannot reach these
// endpoints.
//
// "Self" semantics: the server holds a *peer.Identity reference at
// boot. Responses tag the row whose device_id matches that identity
// with `isSelf=true`, and DELETE on the self device_id is rejected
// (decommissioning the local peer must be done from a different peer
// to avoid a peer killing its own registry row mid-write).
//
// Wire format follows the existing kv / blob handler conventions:
// JSON objects, snake_case-free camelCase fields, `{error:{code,
// message}}` for non-2xx, no ETag yet (peer rows mutate on every
// heartbeat — etag would churn without giving editing UIs anything
// useful). When a future slice adds inter-peer mTLS the public_key
// and capabilities fields here are the wire surface peers will
// validate against.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// peerResponse is the wire shape for one peer_registry row.
type peerResponse struct {
	DeviceID string `json:"deviceId"`
	// Name is the human-friendly device label (OS hostname by
	// default). Operator-overridable.
	Name string `json:"name"`
	// URL is the dial address other peers reach this row on
	// (`host:port` or `http(s)://host:port`). Empty until the
	// daemon has bound a listener at least once.
	URL      string `json:"url,omitempty"`
	LastSeen int64  `json:"lastSeen,omitempty"`
	Status   string `json:"status"`
	IsSelf       bool   `json:"isSelf"`
	// Trusted mirrors peer_registry.trusted. When true the peer's
	// signed requests are admitted on the privileged surface
	// (sessions, ws, info, dirs, files, git, upload). UI flips
	// this via PATCH /api/v1/peers/{id} or the register form's
	// "trust this peer" checkbox.
	Trusted bool `json:"trusted"`
}

type peerListResponse struct {
	Items []peerResponse `json:"items"`
	// SelfDeviceID is echoed alongside the list so the UI doesn't
	// have to do a separate /self round-trip just to know which row
	// to highlight. Empty when the server was wired without a peer
	// identity (shouldn't happen because the route would not be
	// registered, but defensive).
	SelfDeviceID string `json:"selfDeviceId,omitempty"`
}

type peerRegisterRequest struct {
	DeviceID string `json:"deviceId"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	// Trusted opts the peer into the privileged cross-peer surface
	// at registration time. Defaults to false; the UI checkbox
	// flips it to true and operators who paste an unmodified
	// pairing spec keep the safe default.
	Trusted bool `json:"trusted,omitempty"`
}

// peerRequestCap caps the JSON request size for POST /api/v1/peers.
// A peer registration record is metadata-only; a few hundred bytes
// is realistic. 16 KiB is a generous wire ceiling that still defends
// against a buggy / hostile client.
const peerRequestCap = 16 * 1024

// nameMaxBytes bounds the human-readable peer name. Matches the
// hostname-class limit (RFC 1035 says 255; we cap shorter so a
// pasted-from-elsewhere blob can't smuggle a multi-MB body through
// the JSON-decoded struct).
const peerNameMaxBytes = 255

// peerCapabilitiesMaxBytes bounds the opaque capabilities JSON. Peers
// are expected to advertise a small fixed set of feature flags here;
// 8 KiB leaves room for growth without inviting abuse. Stored verbatim
// (not parsed) so the limit is a byte-length not a structure depth.
const peerCapabilitiesMaxBytes = 8 * 1024

// requireOwnerForPeers gates every /api/v1/peers route. Returns
// false (after writing a 403) when the request is not from the Owner
// principal — never reached on the public listener (Owner-only
// middleware) but the agent listener routes through the same mux
// and the allowlist is the only barrier.
func (s *Server) requireOwnerForPeers(w http.ResponseWriter, r *http.Request) bool {
	if !auth.FromContext(r.Context()).IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden", "peers API is owner-only")
		return false
	}
	return true
}

// toPeerResponse converts a store row to wire form, stamping IsSelf
// against the local identity. Tolerates a nil Server.peerID for
// unit-test isolation, in which case no row is ever flagged self.
func (s *Server) toPeerResponse(rec *store.PeerRecord) peerResponse {
	out := peerResponse{
		DeviceID: rec.DeviceID,
		Name:     rec.Name,
		URL:      rec.URL,
		LastSeen: rec.LastSeen,
		Status:   rec.Status,
		Trusted:  rec.Trusted,
	}
	if s.peerID != nil && rec.DeviceID == s.peerID.DeviceID {
		out.IsSelf = true
	}
	return out
}

// validateDeviceID enforces the peer-identity convention that
// device_id is a UUID in canonical lowercase 8-4-4-4-12 form. The
// store layer accepts any non-empty string, but the wire contract is
// stricter so cross-peer joins can't smuggle path-traversal /
// shell-metacharacter payloads via the id.
//
// uuid.Parse on its own is too permissive: it accepts URN form
// ("urn:uuid:..."), braced form ("{...}"), un-hyphenated raw bytes,
// and uppercase. Letting any of those through would let the same
// logical UUID land twice in peer_registry under different keys, and
// would let a caller bypass self-detection by submitting an
// alternate spelling of the local device id. Reject anything whose
// String() round-trip differs from the input.
//
// Empty input is its own error so the handler can produce a clearer
// message ("required" vs "invalid format").
func validateDeviceID(id string) error {
	return peer.ValidateDeviceID(id)
}

// validatePeerName checks the human-readable name. Trimmed length
// > 0 and ≤ peerNameMaxBytes; all Unicode control characters rejected
// so a UI rendering the value can't be tricked into ANSI escape /
// null / DEL / TAB injection. unicode.IsControl covers the C0 range
// (incl. NUL, TAB, LF, CR, ESC), DEL (U+007F), and the C1 range
// (U+0080..U+009F) — the entire control-class surface, not just the
// three sequence-breaking ones.
func validatePeerName(name string) error {
	return peer.ValidateName(name)
}

// validatePeerURL ensures the dial-URL value the operator (or a
// peering broadcast) submitted parses as `host:port` or
// `http(s)://host:port`. peer.NormalizeAddress is the runtime
// authority for this shape; reusing it here keeps the validation
// and the dial path agreeing.
//
// Empty is treated as "not yet known" and accepted — a Hub may
// register a peer before that peer has been started for the first
// time, in which case the URL lands blank and the row will be
// refreshed once a heartbeat arrives.
func validatePeerURL(rawURL string) error {
	if rawURL == "" {
		return nil
	}
	// NormalizeAddress accepts the dial-form ("host:port" or
	// "http(s)://host:port") but historically discards any
	// trailing path / query / fragment without raising — that
	// would let an operator silently store a junk URL whose
	// dial shape only emerges after the silent strip. Use the
	// CLI's strict shape gate so HTTP and CLI agree on what a
	// valid URL looks like.
	if !peer.IsDialAddress(rawURL) {
		return errors.New("url must be host:port or http(s)://host:port with no path/query/fragment")
	}
	return nil
}


// handleListPeers returns every row in peer_registry. The local row
// is included — UIs are expected to render it differently using the
// `isSelf` flag, but a full listing makes "this device's identity
// drifted" cases visible.
//
// Auth: Owner sees every field. RoleAgent / RolePrivAgent see a
// reduced view (device_id, name, status, isSelf) so an agent
// driving a handoff/switch can discover the target's device_id by
// name without learning every peer's Ed25519 public key. Other
// non-Owner principals (Guest, Peer, WebDAV) fall through the
// policy gate and never reach the handler.
func (s *Server) handleListPeers(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if !p.IsOwner() && !p.IsAgent() {
		writeError(w, http.StatusForbidden, "forbidden",
			"peers API requires owner or agent principal")
		return
	}
	rows, err := s.agents.Store().ListPeers(r.Context(), store.ListPeersOptions{})
	if err != nil {
		s.logger.Error("peer handler: list failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	out := peerListResponse{
		Items: make([]peerResponse, 0, len(rows)),
	}
	if s.peerID != nil {
		out.SelfDeviceID = s.peerID.DeviceID
	}
	for _, rec := range rows {
		row := s.toPeerResponse(rec)
		out.Items = append(out.Items, row)
	}
	_ = p // (peerResponse no longer carries identity-sensitive
	// fields, so the previous Owner-vs-Agent reduced view is
	// unnecessary. The principal handle is retained for any
	// future surface that wants role-based gating.)
	writeJSONResponse(w, http.StatusOK, out)
}

// handleGetSelfPeer returns the local peer's identity row. Used by
// the UI to render a stable "this device" badge and by tooling that
// pairs a remote peer (the remote needs the local public key to
// register us). The handler resolves the row through the store — it
// does NOT serialize the in-memory Identity directly — so a stale
// registry row (e.g. heartbeat hasn't fired yet on this boot) shows
// up here too rather than silently being papered over.
func (s *Server) handleGetSelfPeer(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwnerForPeers(w, r) {
		return
	}
	// Defensive: the route-registration guard already ensures peerID
	// and the store handle are non-nil before this handler is wired
	// up (see registerRoutes). Re-check here so a misconfigured test
	// that bolted the handler onto a stripped Server doesn't panic
	// under load.
	if s.peerID == nil || s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "peer registry not initialized")
		return
	}
	rec, err := s.agents.Store().GetPeer(r.Context(), s.peerID.DeviceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Unexpected — the registrar should have upserted at
			// boot. Surface as 503 (not 500) so a UI knows to
			// retry; the registrar may simply not have run yet.
			writeError(w, http.StatusServiceUnavailable, "unavailable", "peer registry not initialized")
			return
		}
		s.logger.Error("peer handler: get self failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	writeJSONResponse(w, http.StatusOK, s.toPeerResponse(rec))
}

// handleRegisterPeer adds (or updates) a remote peer the operator
// has just paired with. The body MUST carry the remote's
// {deviceId, name, publicKey}; capabilities is optional.
//
// Identity-key rotation is intentionally NOT exposed here — the
// store's UpsertPeer preserves public_key on conflict. A registered
// peer that wants to rotate its key has to go through a future
// /api/v1/peers/{id}/rotate-key path that captures the audit trail.
// Re-POSTing a different publicKey for an existing deviceId silently
// keeps the original key (UpsertPeer contract); status / name /
// capabilities update normally.
//
// Self-registration is rejected — the local peer's row is owned by
// the registrar's heartbeat loop. Letting a Web UI overwrite it
// would create a race between heartbeat (status=online) and the UI
// write.
func (s *Server) handleRegisterPeer(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwnerForPeers(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, peerRequestCap)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 16 KiB cap")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	var req peerRegisterRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	if err := validateDeviceID(req.DeviceID); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := validatePeerName(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := validatePeerURL(req.URL); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if s.peerID != nil && req.DeviceID == s.peerID.DeviceID {
		writeError(w, http.StatusConflict, "conflict",
			"cannot register self via this endpoint; the registrar manages the local peer row")
		return
	}
	// RegisterPeerMetadata is a single SQL statement (INSERT ... ON
	// CONFLICT DO UPDATE), so a heartbeat that fires concurrently
	// cannot race the metadata edit: the conflict-update branch only
	// touches name / capabilities, leaving last_seen / status (and
	// public_key) alone. A read-modify-write would, by contrast,
	// roll back the heartbeat's status flip if it landed between
	// the GetPeer and the UpsertPeer.
	rec := &store.PeerRecord{
		DeviceID: req.DeviceID,
		Name:     req.Name,
		URL:      req.URL,
		Trusted:  req.Trusted,
	}
	out, err := s.agents.Store().RegisterPeerMetadata(r.Context(), rec)
	if err != nil {
		s.logger.Error("peer handler: register failed", "device_id", req.DeviceID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	// RegisterPeerMetadata preserves `trusted` on conflict (an
	// untrusted re-push can't self-promote). Apply the operator's
	// explicit value so re-registering with a flipped checkbox
	// updates the trust state in the same round-trip.
	if out != nil && out.Trusted != req.Trusted {
		if err := s.agents.Store().UpdatePeerTrust(r.Context(), req.DeviceID, req.Trusted); err != nil {
			s.logger.Error("peer handler: trust apply failed",
				"device_id", req.DeviceID, "err", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
		out.Trusted = req.Trusted
	}
	// Cluster convergence is no longer push-replicated: peers learn
	// each other lazily via the Hub-mediated Bearer pairing flow
	// (docs/peer-simplify-plan.md). Other peers see this row through
	// their next /api/v1/peers GET against the Hub.
	writeJSONResponse(w, http.StatusOK, s.toPeerResponse(out))
}

// peerKeyFingerprint returns the first 16 chars of a base64-std
// public key for log-line use. The full key is identity surface and
// 64 chars, which clutters logs without adding distinguishing power
// at the audit-line scan level. Truncation here is OK because (a)
// 16 base64 chars = 96 bits, well above collision-class for the few
// peers a single cluster will ever carry, and (b) the response body
// always includes the full key for verifiers.
func peerKeyFingerprint(b64 string) string {
	if len(b64) <= 16 {
		return b64
	}
	return b64[:16] + "…"
}

// peerMetadataPatchRequest is the wire shape for PATCH
// /api/v1/peers/{id}. Only the operator-editable name + url
// are accepted here; identity rotation flows through
// /rotate-key, trust flip through /trust, and capabilities
// are owned by the peer's own self-report (so the GUI must
// not clobber them). Limiting the patch shape keeps a stale
// browser tab from accidentally rolling back fields another
// surface just changed.
type peerMetadataPatchRequest struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// handlePatchPeerMetadata updates a peer's name + url in place.
// Owner-only. Refuses self because the local registrar owns the
// self-row's url/name (operator must use the daemon's hostname /
// pairing spec to rename this host).
//
// Successful edits fan out via broadcastPeerRegistration so other
// paired peers learn the new url/name without waiting for the
// next register-push roundtrip. fan-out is best-effort; the row
// is already committed locally so an unreachable peer just keeps
// the stale view until its next inbound heartbeat / re-broadcast.
func (s *Server) handlePatchPeerMetadata(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwnerForPeers(w, r) {
		return
	}
	id := r.PathValue("id")
	if err := validateDeviceID(id); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if s.peerID != nil && id == s.peerID.DeviceID {
		writeError(w, http.StatusConflict, "conflict",
			"cannot edit the local peer's row; rename via the daemon's hostname / pairing spec instead")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, peerRequestCap)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	var req peerMetadataPatchRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	if err := validatePeerName(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.URL == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "url required")
		return
	}
	if err := validatePeerURL(req.URL); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := s.agents.Store().UpdatePeerMetadata(r.Context(), id, req.Name, req.URL); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "peer not registered")
			return
		}
		s.logger.Error("peer handler: metadata update failed", "device_id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	rec, err := s.agents.Store().GetPeer(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "post-update lookup failed")
		return
	}
	// Edit propagation is no longer push-replicated. Other peers see
	// the new url/name through their next /api/v1/peers GET against
	// the Hub (docs/peer-simplify-plan.md).
	writeJSONResponse(w, http.StatusOK, s.toPeerResponse(rec))
}

// peerTrustRequest is the wire shape for PATCH
// /api/v1/peers/{id}/trust.
type peerTrustRequest struct {
	Trusted bool `json:"trusted"`
}

// handlePatchPeerTrust flips the trusted bit on a paired peer
// row. Owner-only. Refuses self for symmetry with the CLI
// (trusting yourself is a no-op — auth never checks the local
// row's trusted column).
func (s *Server) handlePatchPeerTrust(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwnerForPeers(w, r) {
		return
	}
	id := r.PathValue("id")
	if err := validateDeviceID(id); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if s.peerID != nil && id == s.peerID.DeviceID {
		writeError(w, http.StatusConflict, "conflict",
			"cannot flip trust on the local peer; the trust bit only gates inbound requests")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, peerRequestCap)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	var req peerTrustRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	if err := s.agents.Store().UpdatePeerTrust(r.Context(), id, req.Trusted); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "peer not registered")
			return
		}
		s.logger.Error("peer handler: trust update failed", "device_id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	rec, err := s.agents.Store().GetPeer(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "post-update lookup failed")
		return
	}
	writeJSONResponse(w, http.StatusOK, s.toPeerResponse(rec))
}

// handleDeletePeer removes a row by device_id. Refuses self —
// decommissioning the local peer must be done from a different peer
// (or by deleting kojo.db). Idempotent: deleting a missing row is a
// 204 (the store layer returns nil on missing rows).
func (s *Server) handleDeletePeer(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwnerForPeers(w, r) {
		return
	}
	id := r.PathValue("id")
	if err := validateDeviceID(id); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if s.peerID != nil && id == s.peerID.DeviceID {
		writeError(w, http.StatusConflict, "conflict",
			"cannot delete the local peer; remove this device's kojo.db instead")
		return
	}
	if err := s.agents.Store().DeletePeer(r.Context(), id); err != nil {
		s.logger.Error("peer handler: delete failed", "device_id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

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
//
// `publicKey` is the base64-standard encoding of the raw 32-byte
// Ed25519 public key — same wire form as `peer/public_key` in kv.
// `capabilities` is opaque JSON (forwarded through as a string so
// the client can json.parse only when it cares); empty means the
// peer never reported any.
type peerResponse struct {
	DeviceID string `json:"deviceId"`
	Name     string `json:"name"`
	// PublicKey / Capabilities are omitempty so the reduced-view
	// path for RoleAgent (which blanks both) drops the JSON keys
	// entirely instead of emitting `"publicKey":""` — that would
	// leak the field's existence to a caller the handler
	// intentionally hides it from.
	PublicKey    string `json:"publicKey,omitempty"`
	Capabilities string `json:"capabilities,omitempty"`
	LastSeen     int64  `json:"lastSeen,omitempty"`
	Status       string `json:"status"`
	IsSelf       bool   `json:"isSelf"`
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
	DeviceID     string `json:"deviceId"`
	Name         string `json:"name"`
	PublicKey    string `json:"publicKey"`
	Capabilities string `json:"capabilities,omitempty"`
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
		DeviceID:     rec.DeviceID,
		Name:         rec.Name,
		PublicKey:    rec.PublicKey,
		Capabilities: rec.Capabilities,
		LastSeen:     rec.LastSeen,
		Status:       rec.Status,
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

// validatePeerPublicKey decodes the wire-form (base64 std strict,
// raw 32-byte Ed25519) and rejects anything else. The strict decoder
// is required so embedded whitespace / line breaks / non-canonical
// padding don't get smuggled into peer_registry — two clients
// submitting the "same" public key with different whitespace would
// otherwise land as distinct rows, defeating the dedup the device_id
// PK is meant to give us. The round-trip check (re-encode == input)
// also guards against alternate-but-decoder-accepting forms.
func validatePeerPublicKey(b64 string) error {
	return peer.ValidatePublicKey(b64)
}

// validatePeerCapabilities accepts an empty string or a single JSON
// object of bounded size. Object specifically (not scalar / array /
// null) — the design doc (docs/multi-device-storage.md §3.3) types
// this column as `JSON: {os, arch, gpu, ...}` and every consumer is
// written to peek keys. Letting a scalar through would force every
// caller to second-guess the shape.
//
// The schema beyond "object" is intentionally not enforced — the
// field is extensible — but the JSON has to parse so a buggy client
// can't poison the registry with an unparseable blob that every UI
// would then bounce off.
func validatePeerCapabilities(s string) error {
	if s == "" {
		return nil
	}
	if len(s) > peerCapabilitiesMaxBytes {
		return errors.New("capabilities exceeds maximum length")
	}
	// Decode into map[string]any so non-objects (scalar/array/null)
	// fail at the JSON layer with a "cannot unmarshal X into Y of
	// type map" error rather than silently passing through.
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return errors.New("capabilities must be a JSON object")
	}
	if v == nil {
		// `null` decodes successfully into a nil map — reject it
		// explicitly so the registry never stores a literal "null".
		return errors.New("capabilities must be a JSON object, not null")
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
		if !p.IsOwner() {
			// Reduced view: drop the identity-sensitive +
			// capabilities fields. Name + status are enough
			// for the agent to pick a handoff target.
			row.PublicKey = ""
			row.Capabilities = ""
		}
		out.Items = append(out.Items, row)
	}
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
	if err := validatePeerPublicKey(req.PublicKey); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := validatePeerCapabilities(req.Capabilities); err != nil {
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
		DeviceID:     req.DeviceID,
		Name:         req.Name,
		PublicKey:    req.PublicKey,
		Capabilities: req.Capabilities,
	}
	out, err := s.agents.Store().RegisterPeerMetadata(r.Context(), rec)
	if err != nil {
		s.logger.Error("peer handler: register failed", "device_id", req.DeviceID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	writeJSONResponse(w, http.StatusOK, s.toPeerResponse(out))
}

// peerRotateKeyRequest is the wire shape for POST
// /api/v1/peers/{id}/rotate-key.
type peerRotateKeyRequest struct {
	PublicKey string `json:"publicKey"`
}

// peerRotateKeyResponse echoes the updated row plus the prior key
// fingerprint so the operator UI can display "<old> → <new>" in an
// audit-trail panel without doing its own pre-rotation read.
type peerRotateKeyResponse struct {
	Peer              peerResponse `json:"peer"`
	PreviousPublicKey string       `json:"previousPublicKey"`
}

// handleRotatePeerKey is the explicit, audited swap of a peer's
// long-lived Ed25519 identity key. The store's UpsertPeer /
// RegisterPeerMetadata contracts intentionally preserve public_key on
// conflict — re-registering a paired peer cannot rotate its identity
// silently — so this endpoint exists as the only path that mutates
// the column.
//
// Self-rotation is intentionally refused with 409. The local peer's
// public key is one half of a kv-stored identity (peer/public_key +
// peer/private_key, see internal/peer/identity.go); rotating only the
// registry copy would leave the binary signing with the old private
// key while the cluster advertises the new public key. A future slice
// will deliver a coordinated "rotate local identity" path that also
// re-seals the private key envelope.
//
// The handler logs an Info-level audit line with old/new key
// fingerprints (first 16 base64 chars each — full keys appear in the
// response body, but the log is the durable audit surface). slog is
// the audit channel for now; a dedicated audit_log table is a v2
// concern (no formal facility exists yet, see store/peer_registry.go
// comments).
func (s *Server) handleRotatePeerKey(w http.ResponseWriter, r *http.Request) {
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
			"cannot rotate the local peer's key via this endpoint; local-identity rotation requires re-sealing the private key envelope")
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
	var req peerRotateKeyRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	if err := validatePeerPublicKey(req.PublicKey); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	oldKey, rec, err := s.agents.Store().RotatePeerKey(r.Context(), id, req.PublicKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "peer not registered")
			return
		}
		// store.RotatePeerKey returns ErrPeerKeyUnchanged when the
		// supplied key equals the existing one; surface it as 409 so
		// the UI knows the no-op was refused intentionally.
		if errors.Is(err, store.ErrPeerKeyUnchanged) {
			writeError(w, http.StatusConflict, "conflict",
				"new publicKey matches the current key; nothing to rotate")
			return
		}
		s.logger.Error("peer handler: rotate-key failed", "device_id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	// Audit. fingerprint() trims to 16 chars so the log line stays
	// readable; the response body carries the full new key for any
	// downstream verifier (e.g. the operator's UI showing a QR code
	// to the paired peer).
	s.logger.Info("peer key rotated",
		"device_id", id,
		"old_key_fp", peerKeyFingerprint(oldKey),
		"new_key_fp", peerKeyFingerprint(req.PublicKey),
	)
	writeJSONResponse(w, http.StatusOK, peerRotateKeyResponse{
		Peer:              s.toPeerResponse(rec),
		PreviousPublicKey: oldKey,
	})
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

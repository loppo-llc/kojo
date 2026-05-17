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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

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
	// Name is the human-friendly device label (OS hostname by
	// default). Operator-overridable.
	Name string `json:"name"`
	// URL is the dial address other peers reach this row on
	// (`host:port` or `http(s)://host:port`). Empty until the
	// daemon has bound a listener at least once.
	URL string `json:"url,omitempty"`
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
	DeviceID     string `json:"deviceId"`
	Name         string `json:"name"`
	URL          string `json:"url"`
	PublicKey    string `json:"publicKey"`
	Capabilities string `json:"capabilities,omitempty"`
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
		DeviceID:     rec.DeviceID,
		Name:         rec.Name,
		URL:          rec.URL,
		PublicKey:    rec.PublicKey,
		Capabilities: rec.Capabilities,
		LastSeen:     rec.LastSeen,
		Status:       rec.Status,
		Trusted:      rec.Trusted,
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
	if err := validatePeerURL(req.URL); err != nil {
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
		URL:          req.URL,
		PublicKey:    req.PublicKey,
		Capabilities: req.Capabilities,
		Trusted:      req.Trusted,
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
	// Best-effort fan-out: tell every other online peer about the new
	// row so the cluster's registries converge without waiting on each
	// peer's manual `--peer-add`. Runs in the background — a slow or
	// offline peer must not block the operator's HTTP response.
	s.broadcastPeerRegistration(out)
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

// peerRegisterPushBroadcastTimeout bounds a single peer fan-out
// dial. The HTTP request that triggers broadcast already returned
// to the operator by the time this runs (the broadcast goroutine
// detaches), so we trade prompt error surfacing for predictable
// upper-bounded wall time per peer.
const peerRegisterPushBroadcastTimeout = 10 * time.Second

// broadcastPeerRegistration sends THIS host's self-row to the
// freshly-registered peer (and to every other known peer) so the
// cluster's registries converge without the operator having to
// run `--peer-add` on every host. Detached goroutine — the
// operator's POST /api/v1/peers response has already been
// written by the time this runs.
//
// Scope: the broadcast carries only the LOCAL self-row. Pushing
// a third-party peer's row would let any cluster member with a
// peer signing key rewrite that row's url/name (a Hub→peer
// routing hijack). The receiver-side handler enforces
// signer-equals-row so the scoped self-row push refuses any
// identity change it shouldn't be allowed to make.
//
// Bootstrap chicken-and-egg: a brand-new peer doesn't have this
// host's public_key in its registry yet, so the very first push
// fails at the receiver's PeerAuth middleware with 401 — that's
// expected. Operator pairing is bidirectional: run `--peer-add`
// on BOTH hosts (Hub stores the peer; peer stores the Hub).
// Once paired, subsequent broadcasts land cleanly and keep the
// peer's view of the Hub's url/name fresh through topology
// changes.
// peerRegisterPushFanoutConcurrency bounds the number of in-flight
// register-push dials. A serial loop would multiply the total
// fan-out wall time by N peers × 10s timeout when several targets
// are unreachable; a 4-way bound keeps small clusters fast while
// preventing a 100-peer registry from triggering 100 simultaneous
// TCP/TLS handshakes against an offline tailnet.
const peerRegisterPushFanoutConcurrency = 4

// broadcastPeerRegistration kicks off a fully detached fan-out
// goroutine. The DB snapshot (ListPeers + self lookup) runs INSIDE
// the goroutine so the operator's POST /api/v1/peers response
// returns the moment the inserted row is committed — no extra
// SQLite round-trip in the request path.
//
// newRow is captured by value (not pointer) so a concurrent
// mutation of the caller's struct can't race the goroutine.
func (s *Server) broadcastPeerRegistration(newRow *store.PeerRecord) {
	if s == nil || newRow == nil || s.peerID == nil || s.agents == nil || s.agents.Store() == nil {
		return
	}
	captured := *newRow
	go s.runBroadcastPeerRegistration(&captured)
}

func (s *Server) runBroadcastPeerRegistration(newRow *store.PeerRecord) {
	st := s.agents.Store()
	ctx, cancel := context.WithTimeout(context.Background(), peerRegisterPushBroadcastTimeout*2)
	defer cancel()
	// Include offline peers too: a freshly-paired peer's row lands
	// with status=offline because the heartbeat hasn't fired yet.
	rows, err := st.ListPeers(ctx, store.ListPeersOptions{})
	if err != nil {
		s.logger.Warn("broadcastPeerRegistration: list peers", "err", err)
		return
	}
	if !containsPeer(rows, newRow.DeviceID) {
		rows = append(rows, newRow)
	}
	selfRec, err := st.GetPeer(ctx, s.peerID.DeviceID)
	if err != nil {
		s.logger.Warn("broadcastPeerRegistration: self lookup", "err", err)
		return
	}
	// Push payload: this host's self-row + the freshly-paired
	// newRow. Receivers admit the self-row (signer == row) and
	// the third-party newRow (signer is trusted at the receiver,
	// see handlePeerRegisterPush's trust gate). Each row reaches
	// every paired peer once.
	payload := []*store.PeerRecord{selfRec}
	if newRow.DeviceID != selfRec.DeviceID {
		payload = append(payload, newRow)
	}
	s.fanOutPeerRegistrations(rows, payload)
}

func containsPeer(rows []*store.PeerRecord, id string) bool {
	for _, r := range rows {
		if r != nil && r.DeviceID == id {
			return true
		}
	}
	return false
}

func (s *Server) fanOutPeerRegistrations(targets []*store.PeerRecord, rows []*store.PeerRecord) {
	if len(rows) == 0 {
		return
	}
	sem := make(chan struct{}, peerRegisterPushFanoutConcurrency)
	var wg sync.WaitGroup
	for _, t := range targets {
		if t == nil || t.DeviceID == s.peerID.DeviceID || t.DeviceID == "" {
			continue
		}
		if t.URL == "" {
			continue
		}
		addr, err := peer.NormalizeAddress(t.URL)
		if err != nil {
			s.logger.Warn("broadcastPeerRegistration: skip target",
				"device_id", t.DeviceID, "err", err)
			continue
		}
		for _, row := range rows {
			if row == nil || row.DeviceID == t.DeviceID {
				// Skip pushing a peer its own row — the receiver
				// short-circuits self-rows anyway, but skipping
				// here saves a wasted round-trip.
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(addr, target string, row *store.PeerRecord) {
				defer wg.Done()
				defer func() { <-sem }()
				if err := s.sendPeerRegisterPush(addr, target, row); err != nil {
					// 401 is the expected first-time state when the
					// target hasn't been paired against us yet, or
					// when this host isn't trusted on the receiver.
					// Log at Debug so real failures aren't buried.
					s.logger.Debug("broadcastPeerRegistration: push failed",
						"target", target, "row", row.DeviceID, "err", err)
				}
			}(addr, t.DeviceID, row)
		}
	}
	wg.Wait()
}

// sendPeerRegisterPush dials `addr/api/v1/peers/register-push`
// with an Ed25519-signed body containing the row to register on
// the receiver. Receiver-side handler validates the body's shape
// and calls RegisterPeerMetadata.
func (s *Server) sendPeerRegisterPush(addr, targetDeviceID string, row *store.PeerRecord) error {
	if row == nil {
		return errors.New("nil row")
	}
	body, err := json.Marshal(peerRegisterRequest{
		DeviceID:     row.DeviceID,
		Name:         row.Name,
		URL:          row.URL,
		PublicKey:    row.PublicKey,
		Capabilities: row.Capabilities,
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), peerRegisterPushBroadcastTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addr+"/api/v1/peers/register-push", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	nonce, err := peer.MakeNonce()
	if err != nil {
		return fmt.Errorf("nonce: %w", err)
	}
	if err := peer.SignRequest(req, s.peerID.DeviceID, s.peerID.PrivateKey, nonce, targetDeviceID); err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	client := peer.NoKeepAliveHTTPClient(peerRegisterPushBroadcastTimeout)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// handlePeerRegisterPush is the receiver-side endpoint for the
// Hub's fan-out broadcast. Auth: RolePeer only (Ed25519 signed
// inter-peer request). Refuses self-rows so the local registrar's
// heartbeat keeps authority over its own row.
func (s *Server) handlePeerRegisterPush(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if !p.IsPeer() {
		writeError(w, http.StatusForbidden, "forbidden",
			"register-push requires an Ed25519-signed inter-peer request")
		return
	}
	if s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"agent store not available")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, peerRequestCap)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				"request body exceeds 16 KiB cap")
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
	if err := validatePeerPublicKey(req.PublicKey); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := validatePeerCapabilities(req.Capabilities); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if s.peerID != nil && req.DeviceID == s.peerID.DeviceID {
		// Local row — refuse silently with 200 so the broadcaster's
		// fan-out loop doesn't error-log. The local registrar already
		// owns this row.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Trust gate. A peer may push:
	//   - its OWN row (signer == req.DeviceID) — kept as an
	//     audited code path even though the PeerAuth middleware
	//     above this handler refuses an unknown signer with 401
	//     long before we reach this gate. The branch survives so
	//     a future bootstrap path (a signed self-introduction
	//     against an explicit operator-bound nonce, say) doesn't
	//     have to re-litigate the rule. v1 pairing is operator-
	//     driven via `kojo --peer-add` on BOTH hosts.
	//   - a THIRD-PARTY row only when the signer carries the
	//     trusted bit on this host. That's the cluster-bootstrap
	//     path: the Hub (whom the operator explicitly trusted via
	//     `--peer-add --trusted`) fans out every paired peer's
	//     row to every other paired peer so peer A → peer B
	//     handoff can resolve B's URL without manual all-pairs
	//     `--peer-add` on every host.
	// An untrusted signer attempting a third-party push gets 403.
	// public_key immutability (below) blocks even a trusted signer
	// from silently rotating another peer's identity key, so the
	// privilege exposed here is name/url/capabilities only.
	signerIsRow := p.PeerID != "" && p.PeerID == req.DeviceID
	if !signerIsRow && !p.PeerTrusted {
		writeError(w, http.StatusForbidden, "forbidden",
			"register-push: third-party row pushes require a trusted signer; signer="+p.PeerID+", row="+req.DeviceID)
		return
	}
	// Identity immutability: an existing row's public_key cannot be
	// rewritten via register-push. The handler-level guard mirrors
	// RegisterPeerMetadata's ON CONFLICT (which preserves
	// public_key) but surfaces 409 instead of a silent no-op so a
	// buggy peer detects the rejection.
	if existing, err := s.agents.Store().GetPeer(r.Context(), req.DeviceID); err == nil && existing != nil {
		if existing.PublicKey != "" && existing.PublicKey != req.PublicKey {
			s.logger.Warn("register-push: refusing pubkey change",
				"device_id", req.DeviceID,
				"existing_fp", peerKeyFingerprint(existing.PublicKey),
				"incoming_fp", peerKeyFingerprint(req.PublicKey),
			)
			writeError(w, http.StatusConflict, "conflict",
				"register-push: public_key disagrees with existing row; use the rotate-key path for audited rotations")
			return
		}
	}
	if _, err := s.agents.Store().RegisterPeerMetadata(r.Context(), &store.PeerRecord{
		DeviceID:     req.DeviceID,
		Name:         req.Name,
		URL:          req.URL,
		PublicKey:    req.PublicKey,
		Capabilities: req.Capabilities,
	}); err != nil {
		s.logger.Error("register-push: store write", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

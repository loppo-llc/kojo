package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

// holderPeerOnline reports whether the holder peer row exists and is
// marked online. Empty holder / missing store / lookup failure all
// count as "not online" — exactly the cases where proxyToHolderPeer
// would fail and the hub-local fallback is worth attempting.
func (s *Server) holderPeerOnline(ctx context.Context, holderDeviceID string) bool {
	if holderDeviceID == "" {
		return false
	}
	st := s.agents.Store()
	if st == nil {
		return false
	}
	lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	rec, err := st.GetPeer(lctx, holderDeviceID)
	if err != nil || rec == nil {
		return false
	}
	return rec.Status == store.PeerStatusOnline
}

// tryPatchRemoteAgentHubLocal handles PATCH /api/v1/agents/{id} for a
// remote-held agent whose holder is offline/unknown, applying the
// change to the hub's own agents row when the payload touches ONLY
// hub-local-safe fields (classification in
// internal/agent/hub_local_update.go and in
// remoteAgentProxyMiddleware).
//
// Returns true when it wrote a response (success or error) and the
// middleware must stop; false when the payload is not hub-local-safe
// — the body is restored and the caller falls through to the normal
// proxy path, preserving the existing peer_offline / agent_remote
// error surface for holder-only mutations.
func (s *Server) tryPatchRemoteAgentHubLocal(w http.ResponseWriter, r *http.Request, id string) bool {
	// Read one byte past the same 1 MiB cap handleUpdateAgent uses so
	// an oversized body is detected rather than silently truncated.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return true
	}
	if len(body) > 1<<20 {
		// Too big to classify here. Restore the full stream (read
		// prefix + unread remainder) and let the proxy path handle it
		// so Content-Length stays truthful.
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
		return false
	}
	// Restore the body for the fall-through proxy path.
	r.Body = io.NopCloser(bytes.NewReader(body))

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		// Malformed JSON: fall through — the proxy path reports the
		// holder-offline condition, same as before this fallback.
		return false
	}
	// Fail closed: any key outside the hub-local-safe allowlist keeps
	// the historic proxy behaviour (→ peer_offline while the holder is
	// down). This also covers "privileged" and any unknown/smuggled key.
	for k := range raw {
		if !agent.IsHubLocalSafePatchKey(strings.ToLower(k)) {
			return false
		}
	}

	p := auth.FromContext(r.Context())
	if !p.CanMutateSelf(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agents may only PATCH themselves")
		return true
	}
	// Same owner-only rule as handleUpdateAgent (peers never reach this
	// path — the middleware passes RolePeer through before proxying).
	for k := range raw {
		if strings.EqualFold(k, "disabledInjections") && !p.IsOwner() {
			writeError(w, http.StatusForbidden, "forbidden", "disabledInjections is owner-only")
			return true
		}
	}

	ifMatch, ifMatchPresent, err := extractDomainIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid If-Match header")
		return true
	}
	if !s.enforceIfMatchPresence(w, r, ifMatchPresent) {
		return true
	}

	var cfg agent.AgentUpdateConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return true
	}

	// Per-agent serialization across precheck → row write → ETag echo,
	// same contract as handleUpdateAgent.
	release := s.agents.LockPatch(id)
	defer release()

	// Re-check holder liveness under the patch lock: if the holder
	// flipped online while we were parsing, defer to the proxy so the
	// holder's in-memory agent stays the write authority.
	if ra := s.agents.GetRemote(id); ra != nil && s.holderPeerOnline(r.Context(), ra.HolderPeer) {
		return false
	}

	if !s.agentIfMatchPrecheck(w, r, id, ifMatch, ifMatchPresent) {
		return true
	}
	// Thread the explicit etag into the store-level CAS so the
	// precondition is atomic with the UPDATE (the precheck above keeps
	// the error-shape parity with handleUpdateAgent; the CAS closes
	// its precheck→write race).
	casIfMatch := ""
	if ifMatchPresent && ifMatch != "*" {
		casIfMatch = stripAgentDisplayETagSuffix(ifMatch)
	}

	a, err := s.agents.UpdateRemoteHubRow(id, cfg, casIfMatch)
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrAgentNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, agent.ErrAgentBusy):
			writeError(w, http.StatusConflict, "agent_busy", err.Error())
		case errors.Is(err, store.ErrETagMismatch):
			writeError(w, http.StatusPreconditionFailed, "precondition_failed",
				"If-Match: etag mismatch")
		case errors.Is(err, agent.ErrOverrideRecordFailed),
			errors.Is(err, agent.ErrHubStorageFailure):
			// Storage-side failure (override bookkeeping or row read),
			// not a client error; 500 so the client retries (safe:
			// phase-1 failure wrote nothing, phase-3 failure is lazily
			// healed by the retry's stale-pending resolution).
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		default:
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return true
	}
	snap := s.snapshotAgent(r, id)
	if displayETag := snap.displayETag(); displayETag != "" {
		w.Header().Set("ETag", quoteETag(displayETag))
	}
	writeJSONResponse(w, http.StatusOK, s.buildAgentResponse(a, snap))
	return true
}

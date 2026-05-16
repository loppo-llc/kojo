package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/loppo-llc/kojo/internal/auth"
)

// docs/multi-device-storage.md §3.7 — agent-sync finalize.
//
// The agent-sync push (`POST /api/v1/peers/agent-sync`) lands
// the source's row state on target BEFORE the blob pull and the
// complete commit. If anything between (pull or complete) fails,
// the switch aborts and target's runtime state should NOT
// activate the agent — otherwise target ends up with a valid
// token + lock-guard entry for an agent whose blobs never
// arrived, which would let an unsuspecting Bearer holder write
// to half-migrated state.
//
// The orchestrator dispatches this finalize POST only after a
// successful complete. The handler:
//
//   - consumes the raw $KOJO_AGENT_TOKEN that agent-sync
//     stashed in pendingAgentTokens and adopts it via the local
//     TokenStore (so $KOJO_AGENT_TOKEN injection on the next
//     PTY spawn authenticates),
//   - signals the wired-in onAgentSyncFinalized hook (cmd/kojo
//     hooks AgentLockGuard.AddAgent here).
//
// Route: POST /api/v1/peers/agent-sync/finalize
// Auth: RolePeer (signer.PeerID must equal source_device_id) or
// RoleOwner. Same trust model as agent-sync itself.
//
// Body:
//
//	{ "source_device_id": "...", "agent_id": "..." }
//
// Idempotent: a second finalize for the same agent_id is a no-op
// because the pending token is already consumed.

type peerAgentSyncFinalizeRequest struct {
	SourceDeviceID string `json:"source_device_id"`
	AgentID        string `json:"agent_id"`
	// OpID matches the op_id stamped on the original
	// /peers/agent-sync request. Required for the two-phase
	// protocol; a finalize without a matching pending entry
	// fails 404 not_found rather than activating runtime state
	// blindly.
	OpID string `json:"op_id"`
}

type peerAgentSyncFinalizeResponse struct {
	AgentID string `json:"agent_id"`
}

func (s *Server) handlePeerAgentSyncFinalize(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if !p.IsPeer() && !p.IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden",
			"peer or owner principal required")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"read body: "+err.Error())
		return
	}
	var req peerAgentSyncFinalizeRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"invalid json: "+err.Error())
		return
	}
	if req.SourceDeviceID == "" || req.AgentID == "" || req.OpID == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"source_device_id, agent_id, and op_id required")
		return
	}
	if p.IsPeer() && p.PeerID != req.SourceDeviceID {
		writeError(w, http.StatusForbidden, "forbidden",
			"signer peer device_id does not match source_device_id")
		return
	}

	entry, ok, err := s.consumePendingAgentSync(r.Context(), req.AgentID, req.OpID)
	if err != nil {
		s.logger.Error("peer agent-sync finalize: pending lookup failed",
			"agent", req.AgentID, "op_id", req.OpID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal",
			"pending lookup: "+err.Error())
		return
	}
	if !ok {
		// No matching sync stash. Two cases:
		//   - the orchestrator never sent the agent-sync for
		//     this op_id (forged or wrong-host finalize)
		//   - a prior finalize already committed and removed
		//     the entry (idempotent re-call)
		// Both are surfaced as 404; the orchestrator treats
		// 404 as "nothing to do" so a retry after a successful
		// finalize is a no-op.
		writeError(w, http.StatusNotFound, "not_found",
			"no pending agent-sync for the given (agent_id, op_id); finalize already committed or sync never landed")
		return
	}
	if s.onAgentSyncFinalized != nil {
		if err := s.onAgentSyncFinalized(r.Context(), req.AgentID, entry.RawToken, req.SourceDeviceID); err != nil {
			// Hook failed (e.g. transient kv write error on
			// AdoptAgentTokenFromPeer). Leave the pending
			// entry in place so a retry can pick it up; the
			// caller surfaces this as 500.
			s.logger.Error("peer agent-sync finalize: hook failed; pending entry retained for retry",
				"agent", req.AgentID, "op_id", req.OpID, "err", err)
			writeError(w, http.StatusInternalServerError, "internal",
				"finalize hook: "+err.Error())
			return
		}
	}
	// Hook succeeded: NOW remove the pending entry so a
	// subsequent retry surfaces as the 404 idempotent path.
	// A kv delete failure surfaces as 500 — leaving a sealed
	// token row in place while telling the orchestrator the
	// op completed would let a later boot consume the same
	// (agent_id, op_id) and replay every hook side effect
	// (token adopt / arrived marker / lock guard add). All
	// three are idempotent on retry, so the orchestrator's
	// next finalize call can re-fire the hook and re-attempt
	// the delete until kv is reachable again. The memory-cache
	// entry stays present too (commit clears memory only
	// after a successful kv delete), so the local retry path
	// stays consistent with the kv state.
	if err := s.commitPendingAgentSync(r.Context(), req.AgentID, req.OpID); err != nil {
		s.logger.Error("peer agent-sync finalize: commit kv delete failed; surface 500 for retry",
			"agent", req.AgentID, "op_id", req.OpID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal",
			"commit kv delete: "+err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK,
		peerAgentSyncFinalizeResponse{AgentID: req.AgentID})
}

// Drop endpoint for the orchestrator's abort path — clears any
// pending token without activating runtime state. Same auth as
// finalize.
type peerAgentSyncDropRequest struct {
	SourceDeviceID string `json:"source_device_id"`
	AgentID        string `json:"agent_id"`
	OpID           string `json:"op_id"`
}

func (s *Server) handlePeerAgentSyncDrop(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if !p.IsPeer() && !p.IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden",
			"peer or owner principal required")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"read body: "+err.Error())
		return
	}
	var req peerAgentSyncDropRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"invalid json: "+err.Error())
		return
	}
	if req.AgentID == "" || req.OpID == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"agent_id and op_id required")
		return
	}
	if p.IsPeer() && p.PeerID != req.SourceDeviceID {
		writeError(w, http.StatusForbidden, "forbidden",
			"signer peer device_id does not match source_device_id")
		return
	}
	if err := s.dropPendingAgentSync(r.Context(), req.AgentID, req.OpID); err != nil {
		s.logger.Error("peer agent-sync drop: kv delete failed",
			"agent", req.AgentID, "op_id", req.OpID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal",
			"drop kv: "+err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]string{"agent_id": req.AgentID})
}

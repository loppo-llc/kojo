package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/store"
)

// docs/multi-device-storage.md §3.7 incremental device-switch.
//
// POST /api/v1/peers/agent-sync/state — target replies with the
// snapshot of what it already has for the agent so the source
// orchestrator can skip retransmitting rows the target's kojo.db
// already holds. Without this endpoint, every switch ships the
// full transcript even when target last hosted the agent ten
// minutes ago — a 4k-message agent burns ~60 MiB of JSON each
// time. With it, only the delta since target's last sync
// crosses the wire.
//
// Auth: signed peer (RolePeer) OR Owner. The handler does not
// reveal the body content of any row, only the max seq / etag
// per surface — knowing target has "max_message_seq=1234" is
// not sensitive (it leaks turn count, nothing else) but we still
// require peer-auth so a stranger can't probe arbitrary device
// peer registries for agent enumeration. Owner is permitted for
// drill / debug use.
//
// Body:
//
//	{ "source_device_id": "...", "agent_id": "ag_..." }
//
// Response (200):
//
//	store.AgentSyncState (Known + Max*Seq + *ETag)
//
// Notes:
//   - source_device_id is asserted against the signer's PeerID
//     so a registered peer A can't ask us for B's view.
//   - Owner principals may pass source_device_id="" since they
//     are out-of-band; the field is then unchecked.

type peerAgentSyncStateRequest struct {
	SourceDeviceID string `json:"source_device_id"`
	AgentID        string `json:"agent_id"`
}

const peerAgentSyncStateMaxBody = 4 << 10 // 4 KiB; body is two short strings.

func (s *Server) handlePeerAgentSyncState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Kojo-Native-Goal", "v1")
	w.Header().Set("X-Kojo-Goal-Handoff", "v1")
	p, ok := requirePeerOrOwner(w, r)
	if !ok {
		return
	}
	if _, ok := s.requireAgentStore(w, "agent store not configured"); !ok {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, peerAgentSyncStateMaxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"read body: "+err.Error())
		return
	}
	var req peerAgentSyncStateRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"invalid json: "+err.Error())
		return
	}
	if !agent.IsPathSafeAgentID(req.AgentID) {
		writeError(w, http.StatusBadRequest, "bad_request", "agent_id required")
		return
	}
	if p.IsPeer() || p.PeerID != "" {
		// Peer principals must declare the source they're
		// orchestrating from; we enforce signer-equals-source so
		// a stray peer can't probe arbitrary device sync states.
		if req.SourceDeviceID == "" {
			writeError(w, http.StatusBadRequest, "bad_request",
				"source_device_id required for peer principal")
			return
		}
		if !verifySignerIsSource(p, req.SourceDeviceID) {
			writeError(w, http.StatusForbidden, "forbidden",
				"signer peer device_id does not match source_device_id")
			return
		}
	}
	if req.SourceDeviceID != "" {
		_, delegatedBy, err := s.authorizeIncomingSource(r.Context(), req.AgentID, req.SourceDeviceID)
		if err != nil {
			writeError(w, http.StatusConflict, "wrong_holder", err.Error())
			return
		}
		if delegatedBy != "" {
			// Multi-hop return: preserve the shadow row/fence and request a full
			// snapshot rather than purging the local agent to bypass authorization.
			writeJSONResponse(w, http.StatusOK, store.AgentSyncState{})
			return
		}
	}

	// Another device force-reclaimed this agent while we still ran it:
	// our rows diverged, so ask for a full snapshot (Known=false) rather
	// than advertising high-water marks the source's history doesn't share.
	if marked, err := s.agents.Store().HasAgentFullSyncMarker(r.Context(), req.AgentID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal",
			"full-sync marker lookup: "+err.Error())
		return
	} else if marked {
		writeJSONResponse(w, http.StatusOK, store.AgentSyncState{})
		return
	}

	state, err := s.agents.Store().GetAgentSyncState(r.Context(), req.AgentID)
	if err != nil {
		// ErrNotFound bubbles up as state.Known=false from the
		// store helper itself, so any error reaching us here is
		// genuinely unexpected.
		if errors.Is(err, store.ErrNotFound) {
			// Defense in depth — should not occur given
			// GetAgentSyncState's sql.ErrNoRows handling.
			writeJSONResponse(w, http.StatusOK, store.AgentSyncState{})
			return
		}
		writeError(w, http.StatusInternalServerError, "internal",
			"sync state lookup: "+err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, state)
}

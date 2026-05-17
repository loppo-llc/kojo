package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/loppo-llc/kojo/internal/auth"
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
	p := auth.FromContext(r.Context())
	if !p.IsPeer() && !p.IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden",
			"peer or owner principal required")
		return
	}
	if s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"agent store not configured")
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
	if req.AgentID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "agent_id required")
		return
	}
	if p.IsPeer() {
		// Peer principals must declare the source they're
		// orchestrating from; we enforce signer-equals-source so
		// a stray peer can't probe arbitrary device sync states.
		if req.SourceDeviceID == "" {
			writeError(w, http.StatusBadRequest, "bad_request",
				"source_device_id required for peer principal")
			return
		}
		if p.PeerID != req.SourceDeviceID {
			writeError(w, http.StatusForbidden, "forbidden",
				"signer peer device_id does not match source_device_id")
			return
		}
		// Holder check (defence in depth, matches handlePeerAgentSync's
		// guard at peer_agent_sync_handler.go's existing-lock branch):
		// when target already has an agent_locks row for this agent,
		// the signer MUST be the current holder. Without this, any
		// registered peer could enumerate target's sync state for an
		// agent it never owned — leaking turn counts and etags. Lock
		// absence is OK (first-time / freshly-released agent).
		lock, lerr := s.agents.Store().GetAgentLock(r.Context(), req.AgentID)
		if lerr != nil && !errors.Is(lerr, store.ErrNotFound) {
			writeError(w, http.StatusInternalServerError, "internal",
				"agent_lock lookup: "+lerr.Error())
			return
		}
		if lock != nil && lock.HolderPeer != "" && lock.HolderPeer != req.SourceDeviceID {
			// Stale-row self-heal: a holder ≠ source row blocks
			// every retry of the orchestrator's switch (the
			// agent-sync handler's existingLock guard 409s) and
			// can only be opened by tearing the row down. We do
			// that here so the operator's "re-run device-switch"
			// from the Hub UI converges without manual SQL.
			//
			// Trust model: pairing IS the trust anchor for this
			// admit. Any peer that signed a valid RolePeer
			// request has a row in peer_registry — operator
			// approval at pairing time. PurgeAgentRuntimeStateFor
			// Retry is NON-destructive in the operator's data
			// sense: it only forgets local lock + handoff
			// markers; agent rows / messages / personas survive,
			// and the orchestrator's follow-up agent-sync
			// reseeds everything from the source. Worst-case
			// abuse is a transient DoS where a paired peer
			// flushes this host's view of the agent until the
			// next switch — far below the bar an untrusted
			// agents/* admit (credentials / persona leak) would
			// cross. Trading the per-row PeerTrusted gate for
			// the pairing gate is what stops the operator from
			// having to flip trust on every paired peer just to
			// run device-switch.
			//
			// PurgeAgentRuntimeStateForRetry DELETEs the lock
			// row (no upsert) — the subsequent agent-sync's
			// existingLock check sees row=nil and admits the
			// sync, AgentLockGuard.AddAgent mints a clean
			// (holder=self, allowed=self) row, finalize's
			// UpdateAllowedProxy stamps source as
			// allowed_proxy_peer.
			s.logger.Warn("state probe: purging stale agent runtime state on target",
				"agent", req.AgentID, "source", req.SourceDeviceID,
				"stale_holder", lock.HolderPeer,
				"lease_expires_at", lock.LeaseExpiresAt)
			if err := s.agents.Store().PurgeAgentRuntimeStateForRetry(
				r.Context(), req.AgentID,
			); err != nil {
				s.logger.Error("state probe: stale state purge failed",
					"agent", req.AgentID, "err", err)
				writeError(w, http.StatusInternalServerError, "internal",
					"stale state purge: "+err.Error())
				return
			}
			// Tear down in-memory runtime side channels so the
			// guard's refresh loop doesn't immediately re-Acquire
			// the lock we just deleted, and cron / notify / slack
			// stop driving the now-purged agent until the
			// orchestrator's agent-sync re-adopts it.
			if s.onAgentRuntimePurged != nil {
				s.onAgentRuntimePurged(r.Context(), req.AgentID)
			}
			// Force full sync. Without this the orchestrator
			// would consult GetAgentSyncState below, find stale
			// max(seq) on messages / memory_entries left over
			// from the prior switch, and ship only the delta —
			// any rows the source generated AFTER the prior
			// sync but BEFORE the purge would be missing from
			// the response and never replayed on target.
			// Empty state ≡ Known=false, which triggers full
			// sync on the source.
			writeJSONResponse(w, http.StatusOK, store.AgentSyncState{})
			return
		}
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

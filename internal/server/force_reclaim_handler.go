package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

// forceReclaimLeaseDuration mirrors AgentLockGuard's default lease
// window so the reclaimed lock matches what the in-memory guard
// would have minted on the next periodic refresh. Kept local to
// avoid an import cycle with internal/peer.
const forceReclaimLeaseDuration = 5 * time.Minute

// handleAgentHandoffForceReclaim is the operator-driven recovery
// path for an agent whose device-switch left the lock pointing at
// an unreachable / dead peer. The regular Acquire / Transfer paths
// refuse a live lease, and a peer that's offline can't co-operate
// on a Release — so the agent is effectively unreachable from the
// Hub UI until the operator can revive the holder.
//
// Owner-only. POST /api/v1/agents/{id}/handoff/force-reclaim
//
// Steps:
//
//  1. ForceReclaimAgentLock — rewrite the agent_locks row to point
//     at this peer with a fresh fencing token, regardless of the
//     current holder or lease state.
//  2. Fire onAgentForceReclaimed (cmd/kojo wires it to
//     AgentLockGuard.AddAgent + Manager.ActivateAgentRuntime) so
//     the local chat surface reattaches without a daemon restart.
//
// Trade-off: any in-flight write from the previous holder racing
// the reclaim WILL fail CheckFencing on the new token. That's the
// point — the previous holder's view of the world is now stale.
func (s *Server) handleAgentHandoffForceReclaim(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwnerForAgents(w, r) {
		return
	}
	if s.peerID == nil || s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"force-reclaim not available on this host (peer identity / agent store not wired)")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing agent id")
		return
	}
	// Confirm the agent exists in the store so an operator typo
	// surfaces as 404 instead of silently creating an orphan lock
	// row for a nonexistent agent.
	if _, err := s.agents.Store().GetAgent(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "agent not registered")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "agent lookup: "+err.Error())
		return
	}
	// One-shot atomic restore of every device-switch artefact
	// (agent_locks, blob_refs, kv handoff markers) back to "local
	// owns the runtime". Without this all-in-one tx the operator
	// has to chase down state row-by-row whenever a switch
	// half-fails — see the recurring force-reclaim breakage where
	// agent_locks was rewritten but blob_refs.home_peer kept
	// pointing at the dead peer and the next device-switch
	// surfaced wrong_source.
	rec, err := s.agents.Store().ForceReclaimAgentToLocal(
		r.Context(), id, s.peerID.DeviceID,
		store.NowMillis(), forceReclaimLeaseDuration.Milliseconds(),
	)
	if err != nil {
		s.logger.Error("force-reclaim: atomic state restore failed", "agent", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal",
			"force-reclaim: "+err.Error())
		return
	}
	if s.onAgentForceReclaimed != nil {
		s.onAgentForceReclaimed(r.Context(), id)
	}
	// Queue-and-forward: holdership just moved to the local peer —
	// deliver anything queued while the previous holder was away.
	s.kickHandoffQueueDrain()
	s.logger.Info("force-reclaim: agent runtime reclaimed",
		"agent", id, "fencing_token", rec.FencingToken,
		"lease_expires_at", rec.LeaseExpiresAt)
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"agentId":        id,
		"holderPeer":     rec.HolderPeer,
		"fencingToken":   rec.FencingToken,
		"leaseExpiresAt": rec.LeaseExpiresAt,
	})
}

// requireOwnerForAgents mirrors requireOwnerForPeers — the
// force-reclaim path is dangerous (it stomps the previous
// holder's fencing token) so it must run only as Owner.
func (s *Server) requireOwnerForAgents(w http.ResponseWriter, r *http.Request) bool {
	if !auth.FromContext(r.Context()).IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden", "owner-only operation")
		return false
	}
	return true
}

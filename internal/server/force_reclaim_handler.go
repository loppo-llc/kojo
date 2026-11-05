package server

import (
	"context"
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
//  3. Notify every other peer (reclaim_notice.go) so the previous
//     holder releases its runtime instead of running a second copy;
//     unreachable peers keep a pending notice that is retried.
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
	// Exclude the whole device-switch transaction, not just its finalize
	// step: a reclaim landing between complete and finalize would leave the
	// source and target runtimes both active. Same lock order as
	// handleSwitchDevice (switch-transaction, then the finalize key).
	unlockSwitch := s.lockPendingFinalize(pendingSyncKey{AgentID: id, OpID: "switch-transaction"})
	defer unlockSwitch()
	unlock := s.lockPendingFinalize(pendingSyncKey{AgentID: id})
	defer unlock()
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
	// Outbox first: persist the notice for the previous holder BEFORE the
	// reclaim commits, so a crash or hang-up right after the commit can't
	// leave the old runtime running with nothing queued to tell it. If
	// the reclaim below fails, the notice is left in place: it addresses
	// every peer (a superset of any notice it replaced) and the delivery
	// pass discards it unless this device holds the agent. Detached from
	// the request and bounded so the switch lock is not held indefinitely.
	noticeCtx, cancelNotice := context.WithTimeout(context.WithoutCancel(r.Context()), 2*reclaimNoticeTimeout)
	defer cancelNotice()
	// One timestamp orders this reclaim against a concurrent one elsewhere:
	// it rides on the notice and is stamped (bound to the new fencing
	// token) inside the reclaim tx below.
	reclaimedAt := store.NowMillis()
	if err := s.recordReclaimNotice(noticeCtx, id, reclaimedAt); err != nil {
		s.logger.Error("force-reclaim: could not record notice for the previous holder", "agent", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal",
			"force-reclaim: record notice for the previous holder: "+err.Error())
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
	rec, err := s.agents.Store().ForceReclaimAgentToLocalStamped(
		r.Context(), id, s.peerID.DeviceID,
		reclaimedAt, forceReclaimLeaseDuration.Milliseconds(), reclaimedAt,
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
	// Tell the previous holder (and any other peer that still claims the
	// agent) to let go. Best-effort: peers we can't reach now keep the
	// pending notice, which the retry loop delivers once they come back.
	// Still under the switch lock, so no switch can move the agent away
	// mid-delivery.
	notice, err := s.deliverReclaimNotice(noticeCtx, id, "")
	if err != nil {
		s.logger.Warn("force-reclaim: notice delivery failed; will retry", "agent", id, "err", err)
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"agentId":        id,
		"holderPeer":     rec.HolderPeer,
		"fencingToken":   rec.FencingToken,
		"leaseExpiresAt": rec.LeaseExpiresAt,
		"reclaimNotice":  notice,
	})
}

// requireOwnerForAgents mirrors requireOwnerForPeers — the
// force-reclaim path is dangerous (it stomps the previous
// holder's fencing token) so it must run only as Owner.
func (s *Server) requireOwnerForAgents(w http.ResponseWriter, r *http.Request) bool {
	if !auth.FromContext(r.Context()).HasOwnerAuthority() {
		writeError(w, http.StatusForbidden, "forbidden", "owner-only operation")
		return false
	}
	return true
}

package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// docs/multi-device-storage.md §3.7 — orchestrated device switch.
//
// Invariants the slice closes (across this file + adjacent
// helpers):
//
//   - **Atomic complete**: lock transfer + every blob_refs.
//     home_peer flip run in ONE transaction via
//     store.CompleteHandoff. A crash between rolls back to the
//     pre-call state — no half-migrated agent can survive a
//     daemon restart.
//
//   - **Fencing on agent-runtime mutations**: the
//     auth.AgentFencingMiddleware refuses POST / PATCH / PUT /
//     DELETE requests from RoleAgent / RolePrivAgent principals
//     when agent_locks.holder_peer ≠ the local peer's
//     device_id. Once complete moves the lock to target, this
//     peer's agent runtime stops being a write authority for
//     the agent's tables.
//
// The owner-only begin/complete/abort triplet leaves the actual
// cross-peer body copy to the operator (or to higher-level
// tooling). This handler closes that gap so an agent — running in
// a PTY on the Hub — can self-migrate to another peer with a
// single POST. The flow on the Hub:
//
//   1. begin                                    (local DB write)
//   2. POST <target>/api/v1/peers/pull          (signed RolePeer)
//      target then loops GETs against
//      <hub>/api/v1/peers/blobs/{uri} per blob, verifying each
//      body's sha256 against the digest the Hub stamped in the
//      pull request (which the Hub read from its own blob_refs)
//   3. complete on success, abort on failure
//
// Route: POST /api/v1/agents/{id}/handoff/switch
//
// Auth: Owner OR the agent itself (`p.IsAgent() && p.AgentID == id`).
// Anything else 403s. The agent-self path is the whole point of
// this endpoint — the existing begin/complete/abort handlers stay
// owner-only because they expose individual state transitions an
// agent has no business driving directly.
//
// Body:
//
//	{ "target_peer_id": "<device_id>" }
//
// Response (200):
//
//	{
//	  "agent_id": "...",
//	  "outcome":  "completed" | "completed_with_lock_failure"
//	              | "aborted" | "abort_failed" | "complete_failed",
//	  "target_peer_id": "...",
//	  "begin":  { ... handoffResponse ... },
//	  "pull":   { "results": [ {uri, status, sha256, size, error}, ... ] },
//	  "complete": { ... handoffResponse ... },   // present on success
//	  "abort":    { ... handoffResponse ... }    // present on failure
//	}
//
// Non-200 surfaces only catastrophic local errors (target not in
// registry, source not local, request build failed before begin).
// Once begin has been recorded the handler always tries to finish
// either with complete or abort and returns 200 so the caller can
// inspect the per-step detail.

// switchDeviceOpTimeout bounds the whole begin→pull→complete
// chain. The pull leg is the slow one; 5 minutes accommodates a
// few hundred megabytes of transcript/memory blobs over a
// Tailscale link. Override via context if you need shorter.
const switchDeviceOpTimeout = 5 * time.Minute

type switchDeviceRequest struct {
	TargetPeerID string `json:"target_peer_id"`
}

type switchDeviceResponse struct {
	AgentID      string            `json:"agent_id"`
	TargetPeerID string            `json:"target_peer_id"`
	Outcome      string            `json:"outcome"`
	// OpID is the orchestrator-minted UUID stamped on the
	// agent-sync request, replayed on finalize/drop. Always
	// surfaced (even on early failures BEFORE sync dispatch)
	// so the operator can correlate target-side pending-sync
	// state, drive manual finalize/drop retries, or grep
	// per-attempt log lines. Empty only when the request was
	// rejected before op_id was minted (bad input).
	OpID string `json:"op_id,omitempty"`
	// FinalizeError captures the dispatchPeerAgentSyncFinalize
	// failure detail when outcome=="completed_finalize_failed"
	// so the operator does not need to grep the server log to
	// see why target's runtime activation didn't fire.
	FinalizeError string            `json:"finalize_error,omitempty"`
	Begin         *handoffResponse  `json:"begin,omitempty"`
	Pull          *peerPullResponse `json:"pull,omitempty"`
	Complete      *handoffResponse  `json:"complete,omitempty"`
	Abort         *handoffResponse  `json:"abort,omitempty"`
	// AbortFailureReason is set when outcome=="abort_failed";
	// surfaces the underlying message so the operator knows
	// handoff_pending may still be set on some rows and needs
	// manual cleanup.
	AbortFailureReason string `json:"abort_failure_reason,omitempty"`
	// Reason carries the per-step failure detail for non-success
	// outcomes (aborted / abort_failed / complete_failed /
	// source_drain_failed / complete_errored_lock_at_target /
	// completed_with_lock_failure). Without this the caller —
	// typically the agent driving the kojo-switch-device skill —
	// sees only "outcome=aborted" and has no diagnostic to report
	// to the user. Best-effort prose, not a stable code.
	Reason string `json:"reason,omitempty"`
	// PullSkipped is true when the agent has no blob_refs rows —
	// the pull step is a no-op and we proceed straight to complete
	// (which still transfers the agent_lock).
	PullSkipped bool `json:"pull_skipped,omitempty"`
	// AgentSynced reports whether the §3.7 agent-sync step
	// landed the agent row + transcript + persona + memory +
	// claude session JSONLs on target. False means the switch
	// aborted before sync had a chance to fire.
	AgentSynced bool `json:"agent_synced,omitempty"`
}

func (s *Server) handleAgentHandoffSwitch(w http.ResponseWriter, r *http.Request) {
	if s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"handoff requires agent store")
		return
	}
	if s.peerID == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"local peer identity not configured")
		return
	}
	if s.blob == nil {
		// Without a local blob store the source has nothing to
		// serve from — refusing here also keeps the request-build
		// step honest (the orchestrator-supplied sha256s would be
		// derived from blob_refs rows the source can't actually
		// fulfil at the body-fetch step).
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"local blob store not configured")
		return
	}
	agentID := r.PathValue("id")
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "agent id required")
		return
	}

	p := auth.FromContext(r.Context())
	if !p.IsOwner() && !(p.IsAgent() && p.AgentID == agentID) {
		writeError(w, http.StatusForbidden, "forbidden",
			"owner or self-agent only")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"read body: "+err.Error())
		return
	}
	var req switchDeviceRequest
	if len(body) > 0 {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request",
				"invalid json: "+err.Error())
			return
		}
	}
	if req.TargetPeerID == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"target_peer_id required")
		return
	}

	// Orchestration ctx is DETACHED from r.Context(): once the
	// agent's own /handoff/switch call goes out, the agent CLI
	// may exit (or be Aborted by Step -1 below) and tear down
	// the originating connection — but the orchestration itself
	// is multi-step and irreversible past complete. A
	// client-cancel must not interrupt begin / pull / complete /
	// finalize. We still cap the run with switchDeviceOpTimeout
	// so a wedged step doesn't hang the goroutine forever.
	ctx, cancel := context.WithTimeout(context.Background(), switchDeviceOpTimeout)
	defer cancel()

	// Resolve target. Accept either a canonical UUID device_id
	// or a Tailscale machine name (peer_registry.name) so a
	// human-typed "bravo" or "bravo.tailnet.ts.net" works the
	// same as the device_id.
	targetRec, err := peer.ResolvePeerTarget(ctx, s.agents.Store(), req.TargetPeerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "bad_request",
				"target peer not in peer_registry: "+req.TargetPeerID)
			return
		}
		if errors.Is(err, peer.ErrAmbiguousPeerName) {
			writeError(w, http.StatusBadRequest, "bad_request",
				err.Error()+" — pass the device_id directly to disambiguate")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal",
			"peer_registry lookup: "+err.Error())
		return
	}
	if targetRec.DeviceID == s.peerID.DeviceID {
		writeError(w, http.StatusBadRequest, "bad_request",
			"target must not equal the local peer")
		return
	}
	// Freshness guard. peerCountLookup (cmd/kojo/main.go)
	// already filters online + last_seen-fresh peers for the
	// skill install gate, but the handler must enforce the same
	// rule server-side: a stale online row that survived a
	// daemon restart but hasn't been touched since
	// peer.OfflineThreshold ago is almost certainly unreachable.
	// Failing fast here saves a 5-minute switch attempt that
	// would time out in the pull leg and surface a confusing
	// abort_failed outcome.
	if targetRec.Status != store.PeerStatusOnline {
		writeError(w, http.StatusConflict, "target_offline",
			"target peer is not online: status="+targetRec.Status)
		return
	}
	cutoffMillis := time.Now().Add(-peer.OfflineThreshold).UnixMilli()
	if targetRec.LastSeen <= 0 || targetRec.LastSeen < cutoffMillis {
		writeError(w, http.StatusConflict, "target_stale",
			"target peer has not been seen recently; refusing switch")
		return
	}
	// Canonicalise the device_id for the rest of the request
	// (runHandoffOp, dispatchPeerPull). The user-typed string
	// only got us this far.
	req.TargetPeerID = targetRec.DeviceID
	targetAddr, err := peer.NormalizeAddress(targetRec.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"target peer has no usable dial name in peer_registry: "+err.Error())
		return
	}

	// Sanity-check the source. Two pre-conditions:
	//
	//   (a) Every blob_refs row for the agent must currently
	//       live on the local peer. Orchestrating from a peer
	//       that isn't the home_peer would have the target dial
	//       us for blobs we don't have.
	//
	//   (b) The agent_lock (if it exists) must be held by the
	//       local peer. Without this check, an agent with zero
	//       blobs could trigger a lock migration even when the
	//       lock currently sits on a third peer. The lock-first
	//       complete reorder relies on the orchestrator owning
	//       the lock to begin with.
	prefix := "kojo://global/agents/" + agentID + "/"
	refs, err := s.agents.Store().ListBlobRefs(ctx, store.ListBlobRefsOptions{
		URIPrefix: prefix,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal",
			"blob_refs list: "+err.Error())
		return
	}
	for _, ref := range refs {
		if ref.HomePeer != s.peerID.DeviceID {
			writeError(w, http.StatusConflict, "wrong_source",
				fmt.Sprintf("blob %s lives on peer %s, not the local peer; orchestrate the switch from that peer",
					ref.URI, ref.HomePeer))
			return
		}
	}
	if cur, lerr := s.agents.Store().GetAgentLock(ctx, agentID); lerr == nil {
		if cur.HolderPeer != "" && cur.HolderPeer != s.peerID.DeviceID {
			writeError(w, http.StatusConflict, "wrong_source",
				fmt.Sprintf("agent_lock holder is %s, not the local peer; orchestrate the switch from that peer",
					cur.HolderPeer))
			return
		}
	} else if !errors.Is(lerr, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "internal",
			"agent_lock read: "+lerr.Error())
		return
	}

	// Self row gives us the address we tell the target to send
	// back during the blob fetch.
	selfRec, err := s.agents.Store().GetPeer(ctx, s.peerID.DeviceID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"self peer_registry row missing: "+err.Error())
		return
	}
	if _, err := peer.NormalizeAddress(selfRec.Name); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"self peer_registry row has no usable name: "+err.Error())
		return
	}

	resp := switchDeviceResponse{AgentID: agentID, TargetPeerID: req.TargetPeerID}

	// selfCall: the request was signed by the agent's own
	// $KOJO_AGENT_TOKEN — i.e. the kojo-switch-device skill is
	// driving us from inside the agent's chat. In that case the
	// busy entry IS the curl we're handling; calling Abort would
	// cancel the curl mid-response, killing the very thing we
	// need to reply to (user-visible symptom: "agent immediately
	// returns processing"). Branch the quiesce + drain steps to
	// skip the busy-cancel while still cancelling one-shots and
	// draining all other concurrent writers.
	//
	// Trust model (v1): selfCall is decided by the bearer
	// identity alone — there is no per-chat run-id binding the
	// HTTP call to a specific busy entry. The agent token lives
	// in kv under namespace=auth and is readable only by the
	// owner-trusted process, so possessing it is already owner-
	// equivalent.
	//
	// Caveat (claude session JSONL): on selfCall the snapshot
	// captures the JSONL while a tool_use (this curl) is mid-
	// flight; target's `claude --continue` may see a torn final
	// turn. Pre-existing limitation — the non-selfCall path also
	// produces a torn turn at the SIGTERM point.
	selfCall := p.IsAgent() && p.AgentID == agentID

	// Step -1: quiesce the local PTY AND set the switching
	// flag so no NEW Chat starts on this peer for the duration
	// of the switch. Without the flag a cron tick or WS frame
	// after the snapshot but before complete would land
	// transcript / JSONL on source that target never receives.
	//
	// Order matters: set the flag FIRST so any race between
	// "switching is true" and the in-flight chat's Abort is
	// resolved in our favour (the chat finishes / is aborted;
	// new chats refuse). Cleared at every exit path below via
	// `defer s.agents.SetSwitching(agentID, false)`.
	//
	// Fail closed: if the chat goroutine doesn't drain inside
	// the quiesce window, refuse the switch with 409 so the
	// operator retries instead of letting a torn-turn JSONL
	// migrate. 3s is generous — typical aborts drain in well
	// under 100ms; a longer hang is a runtime defect worth
	// surfacing.
	if s.agents != nil {
		s.agents.SetSwitching(agentID, true)
		defer s.agents.SetSwitching(agentID, false)
		if selfCall {
			s.agents.CancelOneShotsForAgent(agentID)
		} else {
			s.agents.Abort(agentID)
		}
		quiesceCtx, quiesceCancel := context.WithTimeout(ctx, 3*time.Second)
		var err error
		if selfCall {
			err = s.agents.WaitChatIdleSelfCall(quiesceCtx, agentID)
		} else {
			err = s.agents.WaitChatIdle(quiesceCtx, agentID)
		}
		quiesceCancel()
		if err != nil {
			writeError(w, http.StatusConflict, "agent_busy",
				"source chat did not drain within quiesce window; retry switch-device: "+err.Error())
			return
		}
	}

	// Step 0a: probe target's existing state for this agent so
	// we ship only the delta. Error handling distinguishes two
	// cases:
	//
	//   - agentSyncStateLegacyTargetErr (404 on the route): older
	//     target binary without the /state endpoint. Log + fall
	//     back to full-sync. This is the ONLY graceful-downgrade
	//     path — protocol is backward compatible.
	//
	//   - any other error (401/403/409/5xx, network, decode):
	//     hard-fail BEFORE begin so an auth/holder mismatch
	//     surfaces here instead of getting re-rejected at
	//     /agent-sync after handoff_pending is already set.
	var targetState *store.AgentSyncState
	probed, perr := s.dispatchPeerAgentSyncState(ctx, targetAddr, req.TargetPeerID, agentID)
	switch {
	case perr == nil:
		targetState = probed
		if targetState != nil && targetState.Known {
			s.logger.Info("switch-device: incremental agent-sync",
				"agent", agentID, "target", req.TargetPeerID,
				"since_message_seq", targetState.MaxMessageSeq,
				"since_memory_entry_updated_at", targetState.MaxMemoryEntryUpdatedAt)
		}
	case errors.Is(perr, agentSyncStateLegacyTargetErr):
		s.logger.Info("switch-device: target lacks /agent-sync/state endpoint; falling back to full sync",
			"agent", agentID, "target", req.TargetPeerID)
	default:
		writeError(w, http.StatusBadGateway, "state_probe_failed",
			"agent-sync state probe failed; refusing to switch: "+perr.Error())
		return
	}

	// Step 0: build the agent-sync payload BEFORE begin. When
	// targetState is non-nil + Known, the builder filters
	// messages / memory_entries by seq so only the delta target
	// is missing rides the wire. Other surfaces (agent / persona
	// / memory / tasks / claude_sessions / token) ship as
	// before — they're small enough that incremental gates
	// aren't worth the complexity.
	//
	// Failure here is a precondition error (source missing data
	// we'd need to migrate); we bail BEFORE marking
	// handoff_pending so no rollback is needed.
	syncReq, serr := s.buildAgentSyncRequest(ctx, agentID, targetState)
	if serr != nil {
		writeError(w, http.StatusInternalServerError, "internal",
			"build agent-sync payload: "+serr.Error())
		return
	}

	// §3.7 self-call: the assistant turn containing the
	// kojo-switch-device tool_use is still mid-flight — accumulated
	// in processChatEvents' local variables, not yet persisted to
	// the messages table. Snapshot the in-flight message from the
	// broadcaster's event log and append it to the sync payload so
	// the target side receives the full conversation. The message
	// is NOT persisted to the source's DB: on abort the chat
	// continues normally and the done event handles persistence; on
	// success the source is released and persistence is moot.
	if selfCall && s.agents != nil {
		if inflight := s.agents.SnapshotAccumulatedMessageRecord(agentID); inflight != nil {
			// Allocate a seq higher than any already-loaded
			// message so the target's syncMessagesTx accepts it
			// (seq <= 0 is rejected). Version=1 is the initial
			// value AppendMessage uses for new rows.
			var maxSeq int64
			for _, m := range syncReq.Messages {
				if m.Seq > maxSeq {
					maxSeq = m.Seq
				}
			}
			inflight.Seq = maxSeq + 1
			inflight.Version = 1
			syncReq.Messages = append(syncReq.Messages, inflight)
		}
	}

	// Mint a per-switch op_id and stamp it on the sync request.
	// finalize / drop replay the same id so a stale drop from
	// a prior attempt can't collide with a fresh retry's
	// pending-sync entry on target.
	syncReq.OpID = uuid.NewString()
	// Surface op_id in the response immediately so even early
	// failures (begin / sync / pull / complete) carry the
	// identifier the operator needs to correlate target-side
	// pending state or drive a manual drop.
	resp.OpID = syncReq.OpID

	// Step 0.5: serialize + gzip the agent-sync wire body BEFORE
	// begin. If the gzipped size exceeds the peer auth
	// middleware's wire cap, target will reject with HTTP 413
	// after begin has already marked handoff_pending — costing a
	// round-trip and forcing an orchestrateAbort cycle. Doing
	// the marshal/gzip here lets us fail FAST with a clean
	// "agent state too large" error before any state change. The
	// resulting bytes are reused by dispatchPeerAgentSync (no
	// double work).
	syncWireBody, syncRawLen, werr := encodeAgentSyncWire(syncReq)
	if werr != nil {
		writeError(w, http.StatusInternalServerError, "internal",
			"encode agent-sync wire: "+werr.Error())
		return
	}
	if int64(len(syncWireBody)) > int64(peer.AuthMaxBodyBytes) {
		writeError(w, http.StatusRequestEntityTooLarge, "agent_too_large",
			fmt.Sprintf("agent state too large for single agent-sync (gzipped %d bytes > peer auth cap %d); chunked sync not yet supported in v1",
				len(syncWireBody), peer.AuthMaxBodyBytes))
		return
	}
	if int64(syncRawLen) > int64(peerAgentSyncMaxBody) {
		// Defends against a small-gzip / huge-decompressed
		// payload that would pass the wire preflight only to
		// be rejected by the receiver's decompressed-size cap
		// (and the gzip-bomb LimitReader inside the handler).
		// Bailing before begin keeps handoff_pending clean.
		writeError(w, http.StatusRequestEntityTooLarge, "agent_too_large",
			fmt.Sprintf("agent state too large for single agent-sync (raw JSON %d bytes > decompressed cap %d); chunked sync not yet supported in v1",
				syncRawLen, peerAgentSyncMaxBody))
		return
	}

	// Step 1: begin.
	beginResp, err := s.runHandoffOp(ctx, agentID, "begin", req.TargetPeerID)
	if err != nil {
		var hoe *handoffOpError
		if errors.As(err, &hoe) {
			writeError(w, hoe.Status, hoe.Code, hoe.Message)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	resp.Begin = beginResp

	// Step 1.5: dispatch the agent-sync to target so target's
	// kojo.db has the agent row + state by the time the blob
	// pull lands. Any failure here aborts the switch (target
	// can't host the agent without metadata).
	//
	// agentSyncAttempted is set BEFORE the dispatch so a
	// network failure that left target with a committed
	// pending entry still triggers drop on abort — without
	// the flag, a lost response would strand the pending
	// entry on target until the next sync overwrites it.
	agentSyncAttempted := true
	_ = agentSyncAttempted // referenced below in orchestrateAbort path
	if err := s.dispatchPeerAgentSync(ctx, targetAddr, req.TargetPeerID, syncWireBody); err != nil {
		// Mark AgentSynced=true unconditionally for the
		// abort path so orchestrateAbort sends drop — the
		// network error means target may have committed
		// before its response was lost.
		resp.AgentSynced = true
		s.orchestrateAbort(ctx, agentID, targetAddr, req.TargetPeerID, syncReq.OpID, &resp,
			"agent-sync dispatch: "+err.Error())
		writeJSONResponse(w, http.StatusOK, resp)
		return
	}
	resp.AgentSynced = true

	// Collect (URI, sha256) items from successful begin rows.
	// A per-blob error inside begin shouldn't silently propagate
	// into the pull leg — refuse to proceed if any row failed,
	// since the target's GET would 409 against the rows whose
	// flag stayed false.
	items := make([]peerPullItem, 0, len(beginResp.Blobs))
	for _, b := range beginResp.Blobs {
		if b.Status != "ok" {
			s.orchestrateAbort(ctx, agentID, targetAddr, req.TargetPeerID, syncReq.OpID, &resp,
				fmt.Sprintf("begin marked blob %s as %s: %s", b.URI, b.Status, b.Error))
			writeJSONResponse(w, http.StatusOK, resp)
			return
		}
		// Look up the canonical sha256 the Hub has on file so
		// the target can verify the pulled body against it
		// (defends against a compromised source returning a
		// matching X-Kojo-Blob-SHA256 header on a substituted
		// body — the orchestrator's blob_refs row is the
		// authoritative anchor).
		ref, gerr := s.agents.Store().GetBlobRef(ctx, b.URI)
		if gerr != nil {
			s.orchestrateAbort(ctx, agentID, targetAddr, req.TargetPeerID, syncReq.OpID, &resp,
				fmt.Sprintf("blob_refs lookup for %s: %s", b.URI, gerr.Error()))
			writeJSONResponse(w, http.StatusOK, resp)
			return
		}
		items = append(items, peerPullItem{URI: b.URI, ExpectedSHA256: ref.SHA256})
	}

	// Step 2: pull. Skip the network round-trip when the agent
	// owns no blobs (a freshly forked agent with no memory yet,
	// etc.); the lock transfer in `complete` is still meaningful.
	if len(items) == 0 {
		resp.PullSkipped = true
	} else {
		pullResp, perr := s.dispatchPeerPull(ctx, targetAddr, req.TargetPeerID, items)
		resp.Pull = pullResp
		if perr != nil {
			s.orchestrateAbort(ctx, agentID, targetAddr, req.TargetPeerID, syncReq.OpID, &resp,
				"peer pull dispatch: "+perr.Error())
			writeJSONResponse(w, http.StatusOK, resp)
			return
		}
		// Defense in depth: the pull handler is supposed to
		// return one result per item, but if a future version
		// deviates we'd silently miss URIs the target never
		// fetched.
		if len(pullResp.Results) != len(items) {
			s.orchestrateAbort(ctx, agentID, targetAddr, req.TargetPeerID, syncReq.OpID, &resp,
				fmt.Sprintf("pull returned %d results for %d items",
					len(pullResp.Results), len(items)))
			writeJSONResponse(w, http.StatusOK, resp)
			return
		}
		for i, r := range pullResp.Results {
			if r.Status != "ok" {
				s.orchestrateAbort(ctx, agentID, targetAddr, req.TargetPeerID, syncReq.OpID, &resp,
					fmt.Sprintf("pull %s: %s: %s", r.URI, r.Status, r.Error))
				writeJSONResponse(w, http.StatusOK, resp)
				return
			}
			// Result/item URI mismatch would mean the target
			// returned a different shape — same defense in depth.
			if r.URI != items[i].URI {
				s.orchestrateAbort(ctx, agentID, targetAddr, req.TargetPeerID, syncReq.OpID, &resp,
					fmt.Sprintf("pull result[%d] uri=%q does not match requested %q",
						i, r.URI, items[i].URI))
				writeJSONResponse(w, http.StatusOK, resp)
				return
			}
		}
	}

	// Step 3: complete. Two failure shapes we care about:
	//
	//   - lock_transfer_failed: the §3.7 reorder runs lock
	//     transfer FIRST; on failure, blob_refs is untouched
	//     and handoff_pending is still set from begin. We must
	//     drive abort to clear the flag, otherwise future
	//     writes against the agent's blobs would 409 forever.
	//
	//   - any other complete error (DB contention etc.) AFTER
	//     lock transferred: blob_refs may have partially
	//     switched home. We can't auto-reverse those rows
	//     (the lock is already at target so a retry by target
	//     converges); record complete_failed and leave the
	//     state for the operator to inspect.
	completeResp, cerr := s.runHandoffOp(ctx, agentID, "complete", req.TargetPeerID)
	if cerr != nil {
		var hoe *handoffOpError
		isLockFail := errors.As(cerr, &hoe) && hoe.Code == "lock_transfer_failed"
		if isLockFail {
			s.logger.Warn("switch-device: lock transfer failed; aborting to clear handoff_pending",
				"agent", agentID, "target", req.TargetPeerID,
				"op_id", syncReq.OpID, "msg", hoe.Message)
			s.orchestrateAbort(ctx, agentID, targetAddr, req.TargetPeerID, syncReq.OpID, &resp,
				"complete (lock transfer): "+hoe.Message)
			writeJSONResponse(w, http.StatusOK, resp)
			return
		}
		resp.Outcome = "complete_failed"
		if errors.As(cerr, &hoe) {
			resp.Reason = "complete: " + hoe.Code + ": " + hoe.Message
			s.logger.Warn("switch-device complete failed",
				"agent", agentID, "target", req.TargetPeerID,
				"op_id", syncReq.OpID, "code", hoe.Code, "msg", hoe.Message)
		} else {
			resp.Reason = "complete: " + cerr.Error()
			s.logger.Warn("switch-device complete failed",
				"agent", agentID, "target", req.TargetPeerID,
				"op_id", syncReq.OpID, "err", cerr)
		}
		// Reconcile: re-read agent_locks. Two cases:
		//   (a) holder == target: complete partially succeeded;
		//       release source so AgentLockGuard doesn't re-Acquire
		//       on lease expiry.
		//   (b) holder != target / no row: complete rolled back
		//       atomically; begin's handoff_pending=1 still set →
		//       drive orchestrateAbort to clear it, otherwise
		//       future blob writes 409 forever.
		if s.agents != nil && s.agents.Store() != nil {
			lookupCtx, lookupCancel := context.WithTimeout(context.Background(), handoffOpTimeout)
			lock, lerr := s.agents.Store().GetAgentLock(lookupCtx, agentID)
			lookupCancel()
			switch {
			case lerr == nil && lock != nil && lock.HolderPeer == req.TargetPeerID:
				s.logger.Warn("switch-device: complete errored but lock observed at target; releasing source",
					"agent", agentID, "target", req.TargetPeerID, "op_id", syncReq.OpID)
				if s.onAgentReleasedAsSource != nil {
					releaseCtx, releaseCancel := context.WithTimeout(context.Background(), handoffOpTimeout)
					s.onAgentReleasedAsSource(releaseCtx, agentID)
					releaseCancel()
				}
				resp.Outcome = "complete_errored_lock_at_target"
			case lerr == nil || errors.Is(lerr, store.ErrNotFound):
				s.logger.Warn("switch-device: complete errored and lock not at target; aborting to clear handoff_pending",
					"agent", agentID, "target", req.TargetPeerID,
					"op_id", syncReq.OpID, "lock_lookup_err", lerr)
				originalReason := resp.Reason
				s.orchestrateAbort(ctx, agentID, targetAddr, req.TargetPeerID, syncReq.OpID, &resp,
					originalReason+" (lock not at target; aborted to clear handoff_pending)")
			}
		}
		writeJSONResponse(w, http.StatusOK, resp)
		return
	}
	resp.Complete = completeResp
	// Per-blob failures inside a "successful" complete (lock
	// moved, but some SwitchBlobRefHome rows errored): surface
	// as complete_failed so the operator knows blobs are
	// inconsistent even though the lock migrated.
	//
	// IMPORTANT: even on this failure path, the lock has
	// already transferred (we got past the lock_transfer_failed
	// branch above). Source's AgentLockGuard.desired still
	// contains this agent, so the refresh loop would re-Acquire
	// under a fresh fencing token once target's lease expires —
	// effectively stealing the agent back with no claude
	// session JSONL / blob bodies to back it up. Run source
	// release before returning so RemoveAgent + SlackHub.Stop
	// fire just like on the success path.
	for _, b := range completeResp.Blobs {
		if b.Status != "ok" {
			s.logger.Warn("switch-device: complete left blob row in non-ok state",
				"agent", agentID, "op_id", syncReq.OpID,
				"uri", b.URI, "status", b.Status, "err", b.Error)
			resp.Outcome = "complete_failed"
			resp.Reason = fmt.Sprintf("complete: blob %s left in %s state: %s",
				b.URI, b.Status, b.Error)
			if s.onAgentReleasedAsSource != nil {
				releaseCtx, releaseCancel := context.WithTimeout(context.Background(), handoffOpTimeout)
				s.onAgentReleasedAsSource(releaseCtx, agentID)
				releaseCancel()
			}
			writeJSONResponse(w, http.StatusOK, resp)
			return
		}
	}
	// Post-complete source drain MUST happen BEFORE we activate
	// the target runtime. The switching flag refused new chat
	// starts during begin/sync/pull/complete, but an in-flight
	// chat that survived the Step-1 abort (e.g. a long claude
	// turn that ignored SIGTERM until just now) could still be
	// holding the JSONL open. Re-Abort + drain catches any
	// stragglers so target's freshly-spawned CLI isn't racing
	// the source's dying one.
	//
	// Surface drain failures: if the chat won't release after a
	// generous timeout, refuse to finalize. Target keeps the
	// pending entry (drop hasn't fired) and the operator can
	// either kill the source process or call finalize manually
	// later. The switch itself is still recorded as completed
	// at the row level — only runtime activation defers.
	drainErr := error(nil)
	if s.agents != nil {
		// Same selfCall guard as Step -1: aborting the busy
		// entry that still holds the open response would kill
		// the curl before writeJSONResponse reaches it.
		if selfCall {
			s.agents.CancelOneShotsForAgent(agentID)
		} else {
			s.agents.Abort(agentID)
		}
		drainCtx, drainCancel := context.WithTimeout(ctx, 5*time.Second)
		if selfCall {
			drainErr = s.agents.WaitChatIdleSelfCall(drainCtx, agentID)
		} else {
			drainErr = s.agents.WaitChatIdle(drainCtx, agentID)
		}
		drainCancel()
	}

	if drainErr != nil {
		s.logger.Error("switch-device: source chat did not drain after complete; deferring finalize",
			"agent", agentID, "target", req.TargetPeerID,
			"op_id", syncReq.OpID, "err", drainErr)
		resp.Outcome = "source_drain_failed"
		resp.Reason = "source chat did not drain after complete: " + drainErr.Error()
		// Even though we never finalize on target, the lock +
		// blob_refs have already moved. Source MUST release —
		// otherwise the defer below clears the switching flag
		// and AgentLockGuard / cron / poller would resume on
		// an agent target now owns. The downside (target never
		// activates) is the same as the finalize-failed path;
		// operator drives a manual finalize retry.
		if s.onAgentReleasedAsSource != nil {
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), handoffOpTimeout)
			s.onAgentReleasedAsSource(releaseCtx, agentID)
			releaseCancel()
		}
		writeJSONResponse(w, http.StatusOK, resp)
		return
	}

	// Two-phase agent-sync finalize: target activates token +
	// AgentLockGuard now that complete + source drain have
	// succeeded. A failure here doesn't roll back the switch
	// (blobs + lock already moved); we log + record so the
	// operator can drive a manual finalize retry. The drop
	// counterpart fires only on the abort paths above.
	//
	// Uses a fresh background ctx so a wedged target doesn't
	// stall past switchDeviceOpTimeout — the switch is already
	// irreversible at this point, finalize is best-effort
	// runtime activation.
	finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), handoffOpTimeout)
	finalizeErr := s.dispatchPeerAgentSyncFinalize(finalizeCtx, targetAddr, req.TargetPeerID, agentID, syncReq.OpID)
	finalizeCancel()
	if finalizeErr != nil {
		s.logger.Warn("switch-device: agent-sync finalize failed; operator may need to retry",
			"agent", agentID, "target", req.TargetPeerID,
			"op_id", syncReq.OpID, "err", finalizeErr)
		// Surface error detail on the response so the operator
		// does not have to grep the log; the op_id field already
		// carries the recovery identifier.
		resp.FinalizeError = finalizeErr.Error()
	}
	// Source-side guard drop runs REGARDLESS of finalize
	// outcome. complete has already moved agent_locks +
	// blob_refs to target; this peer is no longer the
	// authoritative home. Leaving the agent in our
	// AgentLockGuard.desired set would let our refresh loop
	// re-Acquire under a fresh fencing token after target's
	// lease expires — effectively stealing the agent back
	// with no claude session JSONL / blob bodies to back it
	// up. A future handoff back to this peer goes through the
	// same agent-sync → AgentLockGuard.AddAgent path, so the
	// drop is safe to make unconditional.
	if s.onAgentReleasedAsSource != nil {
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), handoffOpTimeout)
		s.onAgentReleasedAsSource(releaseCtx, agentID)
		releaseCancel()
	}

	switch {
	case completeResp.LockTransferred && finalizeErr != nil:
		// Lock + blobs moved successfully but target's runtime
		// activation (token adopt + AgentLockGuard register)
		// failed. The agent is half-installed on target; the
		// operator must drive a manual finalize retry. Surface
		// distinct outcome so this doesn't masquerade as a
		// clean switch in dashboards / logs.
		resp.Outcome = "completed_finalize_failed"
	case completeResp.LockTransferred:
		resp.Outcome = "completed"
	default:
		// LockTransferred=false in handoff_handler.go is collapsed
		// to true when Lock.HolderPeer already equals target, so
		// reaching this default means there was NO agent_lock row
		// to migrate. blob_refs flipped to target on this complete
		// but no fencing authority follows.
		resp.Outcome = "completed_with_lock_failure"
		resp.Reason = "complete: blob_refs flipped to target but no agent_lock row existed to migrate. Inspect agent_locks on target — issue a manual Acquire if the agent should be locked."
	}

	// (Source drain + finalize already happened above before
	// the LockTransferred branch.)
	writeJSONResponse(w, http.StatusOK, resp)
}

// dispatchPeerAgentSyncFinalize tells target to commit the
// runtime side effects (TokenStore adopt, AgentLockGuard
// AddAgent) that agent-sync deferred. Best-effort: a failure
// here is logged + ignored by the caller — target still holds
// the agent rows and the operator can manually re-call this
// endpoint to recover.
func (s *Server) dispatchPeerAgentSyncFinalize(ctx context.Context, targetAddr, targetDeviceID, agentID, opID string) error {
	return s.dispatchPeerAgentSyncPhase2(ctx, targetAddr, targetDeviceID, agentID, opID,
		"/api/v1/peers/agent-sync/finalize")
}

// dispatchPeerAgentSyncDrop tells target to discard any pending
// agent-sync state (raw $KOJO_AGENT_TOKEN it stashed before
// finalize). Used on the orchestrator's abort path so an
// aborted switch doesn't leak the raw into target's memory map.
func (s *Server) dispatchPeerAgentSyncDrop(ctx context.Context, targetAddr, targetDeviceID, agentID, opID string) error {
	return s.dispatchPeerAgentSyncPhase2(ctx, targetAddr, targetDeviceID, agentID, opID,
		"/api/v1/peers/agent-sync/drop")
}

func (s *Server) dispatchPeerAgentSyncPhase2(ctx context.Context, targetAddr, targetDeviceID, agentID, opID, path string) error {
	body, err := json.Marshal(map[string]string{
		"source_device_id": s.peerID.DeviceID,
		"agent_id":         agentID,
		"op_id":            opID,
	})
	if err != nil {
		return fmt.Errorf("marshal phase-2 body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		targetAddr+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build phase-2 request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	nonce, err := peer.MakeNonce()
	if err != nil {
		return fmt.Errorf("nonce: %w", err)
	}
	if err := peer.SignRequest(req, s.peerID.DeviceID, s.peerID.PrivateKey, nonce, targetDeviceID); err != nil {
		return fmt.Errorf("sign phase-2 request: %w", err)
	}
	client := peer.NoKeepAliveHTTPClient(switchDeviceOpTimeout)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("dispatch phase-2: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// 404 on phase-2 is treated as a failure, NOT idempotent
	// success. The target's pendingAgentSyncs map is in-memory
	// only; a target restart between agent-sync and finalize
	// drops the pending entry. Without the finalize hook
	// firing, target's TokenStore never adopts the raw
	// $KOJO_AGENT_TOKEN and AgentLockGuard.AddAgent never
	// registers — agent rows are present but the runtime is
	// inert. We can't distinguish "lost finalize response /
	// already committed" from "pending entry never landed" at
	// this layer, so the conservative call is to surface 404
	// as failure and let the operator inspect / retry. The
	// downside (a lost response masquerading as
	// completed_finalize_failed) is cosmetic and recoverable;
	// the alternative would silently strand an agent in a
	// non-runnable state on target.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("phase-2 HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

// orchestrateAbort runs handoff/abort on the local store AND
// asks target to drop any pending agent-sync state. Wraps
// abortAfterFailure so every failure path in
// handleAgentHandoffSwitch fans out the cleanup uniformly. The
// op_id matches the one stamped on the agent-sync dispatch so
// target's pendingAgentSyncs map can identify the exact entry
// to remove.
func (s *Server) orchestrateAbort(ctx context.Context, agentID, targetAddr, targetDeviceID, opID string, resp *switchDeviceResponse, reason string) {
	// Stamp the upstream reason BEFORE abortAfterFailure runs so
	// it survives even when abort itself succeeds (the success
	// branch in abortAfterFailure only sets Outcome; without this
	// a successful abort would erase any record of why we
	// aborted). Latest cause wins by design — callers that
	// already populated resp.Reason fold that detail into the
	// `reason` argument they pass.
	resp.Reason = reason
	s.abortAfterFailure(ctx, agentID, resp, reason)
	if resp.AgentSynced && targetAddr != "" && targetDeviceID != "" && opID != "" {
		dropCtx, cancel := context.WithTimeout(context.Background(), handoffOpTimeout)
		defer cancel()
		if derr := s.dispatchPeerAgentSyncDrop(dropCtx, targetAddr, targetDeviceID, agentID, opID); derr != nil {
			s.logger.Warn("switch-device: agent-sync drop failed (target may hold stale pending entry until next sync)",
				"agent", agentID, "target", targetDeviceID, "err", derr)
		}
	}
}

// abortAfterFailure drives `handoff/abort` for a switch that
// failed mid-flight and stamps the outcome + abort response onto
// resp. The function distinguishes two failure modes:
//
//   - "aborted": abort itself succeeded; every blob_refs row had
//     handoff_pending cleared cleanly. Operator can re-drive the
//     switch later.
//   - "abort_failed": abort hit at least one error (DB write
//     contention, store-level failure, or a per-blob row that
//     refused the update). handoff_pending may still be set on
//     some rows and a future write against the agent's blobs
//     will 409. The reason is logged AND surfaced in
//     resp.AbortFailureReason so the operator doesn't have to
//     dig into stderr to know cleanup is needed.
func (s *Server) abortAfterFailure(ctx context.Context, agentID string, resp *switchDeviceResponse, reason string) {
	s.logger.Warn("switch-device: aborting after failure",
		"agent", agentID, "target", resp.TargetPeerID, "reason", reason)
	// Abort uses a fresh context bounded to the handoff timeout so
	// a caller-cancelled ctx (e.g. client hung up) still lets us
	// clear handoff_pending — leaving the flag set would block
	// future writes against the agent's blobs.
	abortCtx, cancel := context.WithTimeout(context.Background(), handoffOpTimeout)
	defer cancel()
	abortResp, aerr := s.runHandoffOp(abortCtx, agentID, "abort", "")
	resp.Abort = abortResp
	if aerr != nil {
		resp.Outcome = "abort_failed"
		resp.AbortFailureReason = aerr.Error()
		s.logger.Error("switch-device: abort itself failed; handoff_pending may persist",
			"agent", agentID, "err", aerr)
		return
	}
	// Even a successful runHandoffOp call can have per-blob
	// errors in its Blobs list. Surface those so the operator
	// knows not every row converged.
	if abortResp != nil {
		for _, b := range abortResp.Blobs {
			if b.Status != "ok" {
				resp.Outcome = "abort_failed"
				resp.AbortFailureReason = fmt.Sprintf("abort %s: %s: %s",
					b.URI, b.Status, b.Error)
				s.logger.Error("switch-device: abort left handoff_pending on row",
					"agent", agentID, "uri", b.URI, "err", b.Error)
				return
			}
		}
	}
	resp.Outcome = "aborted"
}

// dispatchPeerPull POSTs the URI list to the target peer's
// /api/v1/peers/pull endpoint with an Ed25519-signed request and
// decodes the per-URI result list. Network and decode errors
// surface to the caller so the orchestrator can roll back via
// abort.
func (s *Server) dispatchPeerPull(ctx context.Context, targetAddr, targetDeviceID string, items []peerPullItem) (*peerPullResponse, error) {
	body, err := json.Marshal(peerPullRequest{
		SourceDeviceID: s.peerID.DeviceID,
		Items:          items,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal pull body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		targetAddr+"/api/v1/peers/pull", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build pull request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	nonce, err := peer.MakeNonce()
	if err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	if err := peer.SignRequest(req, s.peerID.DeviceID, s.peerID.PrivateKey, nonce, targetDeviceID); err != nil {
		return nil, fmt.Errorf("sign pull request: %w", err)
	}

	// One-shot HTTP client with a generous timeout — the body is
	// just the result manifest, but the target loops blob GETs
	// against us before responding, so the round-trip can be
	// long. Re-use of connections doesn't matter (single call).
	client := peer.NoKeepAliveHTTPClient(switchDeviceOpTimeout)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dispatch pull: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, peerPullMaxBody))
	if err != nil {
		return nil, fmt.Errorf("read pull response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pull HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var out peerPullResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode pull response: %w", err)
	}
	return &out, nil
}

// buildAgentSyncRequest captures every row on this peer that
// target needs to spawn the agent CLI + render its prior state:
//
//   - agents row + persona + memory_md
//   - transcript (agent_messages) — full when targetState is nil
//     or Known=false, otherwise only rows with seq > targetState.
//     MaxMessageSeq (incremental device-switch path)
//   - memory_entries (analogous incremental filter)
//   - claude session JSONL files (~/.claude/projects/...)
//   - raw $KOJO_AGENT_TOKEN so target's TokenStore can adopt
//
// Source-of-truth reads only — no mutations. Returns an error
// when the agent row itself is missing; absent persona / memory /
// JSONLs are tolerated (those agents simply migrate with less
// state).
//
// targetState=nil means "no preflight performed, ship everything"
// (legacy / first-time). targetState.Known=false has the same
// effect — target has nothing for this agent. Both Max*Seq=0 is
// also full-ship territory.
func (s *Server) buildAgentSyncRequest(ctx context.Context, agentID string, targetState *store.AgentSyncState) (*peerAgentSyncRequest, error) {
	st := s.agents.Store()
	rec, err := st.GetAgent(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("get agent: %w", err)
	}
	req := &peerAgentSyncRequest{
		SourceDeviceID: s.peerID.DeviceID,
		Agent:          rec,
	}
	if persona, perr := st.GetAgentPersona(ctx, agentID); perr == nil {
		req.Persona = persona
	} else if !errors.Is(perr, store.ErrNotFound) {
		return nil, fmt.Errorf("get persona: %w", perr)
	}
	if mem, merr := st.GetAgentMemory(ctx, agentID); merr == nil {
		req.Memory = mem
	} else if !errors.Is(merr, store.ErrNotFound) {
		return nil, fmt.Errorf("get memory: %w", merr)
	}

	// Incremental filter for messages only. memory_entries do
	// NOT use seq-cursor delta — that table allows body updates,
	// soft-deletes, and recreations on the SAME seq, so a delta
	// keyed by `seq > cursor` would silently miss every
	// in-place mutation. v1 keeps memory_entries on the
	// full-replace path; it's much smaller than the transcript
	// anyway (400 KB vs 40 MB observed on ag_f71bf5..).
	//
	// Messages can have in-place mutations on the SAME seq —
	// either soft-delete (TruncateMessagesFromCreatedAt /
	// individual delete) or edit (UpdateMessage / Regenerate
	// bumps version + etag). A seq-cursor delta skips both
	// kinds, so target would keep a stale or resurrected
	// transcript view. Probe the source for any
	// non-append-only row; if found, downgrade messages to
	// full-replace. Bandwidth cost falls back to pre-incremental
	// baseline only for agents that have actually been edited /
	// truncated — append-only transcripts (the common case)
	// keep the delta path.
	var sinceMsgSeq int64
	if targetState != nil && targetState.Known {
		hasMutation, terr := st.HasNonAppendOnlyMessages(ctx, agentID)
		if terr != nil {
			return nil, fmt.Errorf("check non-append-only messages: %w", terr)
		}
		if !hasMutation {
			sinceMsgSeq = targetState.MaxMessageSeq
		}
	}
	msgs, merr := st.ListMessages(ctx, agentID, store.MessageListOptions{
		SinceSeq: sinceMsgSeq,
	})
	if merr != nil {
		return nil, fmt.Errorf("list messages: %w", merr)
	}
	req.Messages = msgs
	req.SinceMessageSeq = sinceMsgSeq

	// memory_entries: incremental keyed off updated_at (NOT seq —
	// the same seq is reused across body update / soft-delete /
	// recreation, so a seq cursor would silently miss in-place
	// mutations). IncludeDeleted=true ships tombstones; the
	// orchestrator orders by updated_at ASC so a tombstone
	// arrives BEFORE any recreation that reused its (kind,name)
	// slot under the alive UNIQUE index. Handler upserts by id,
	// leaving target's rows outside the delta untouched.
	var sinceMemUpdatedAt int64
	if targetState != nil && targetState.Known {
		sinceMemUpdatedAt = targetState.MaxMemoryEntryUpdatedAt
	}
	memOpts := store.MemoryEntryListOptions{}
	if sinceMemUpdatedAt > 0 {
		memOpts.UpdatedAtSince = sinceMemUpdatedAt
		memOpts.IncludeDeleted = true
	}
	mentries, eerr := st.ListMemoryEntries(ctx, agentID, memOpts)
	if eerr != nil {
		return nil, fmt.Errorf("list memory_entries: %w", eerr)
	}
	req.MemoryEntries = mentries
	req.SinceMemoryEntryUpdatedAt = sinceMemUpdatedAt

	tasks, terr := st.ListAgentTasks(ctx, agentID, store.AgentTaskListOptions{})
	if terr != nil {
		return nil, fmt.Errorf("list tasks: %w", terr)
	}
	req.Tasks = tasks

	// claude session JSONLs — claude's cwd is AgentDir(agentID),
	// NOT Settings.workDir (workDir is the user's project files
	// surface, unrelated to claude's --resume session
	// placement). Read from source's AgentDir, target writes
	// into its own AgentDir.
	files, skipped, ferr := agent.ReadClaudeSessionFiles(agentID)
	if ferr != nil {
		return nil, fmt.Errorf("read claude sessions: %w", ferr)
	}
	if len(skipped) > 0 {
		s.logger.Warn("agent-sync: skipped oversized claude session files",
			"agent", agentID, "files", skipped)
	}
	if len(files) > 0 {
		req.ClaudeSessions = make([]claudeSessionWire, 0, len(files))
		for _, f := range files {
			req.ClaudeSessions = append(req.ClaudeSessions, claudeSessionWire{
				SessionID:  f.SessionID,
				ContentB64: base64.StdEncoding.EncodeToString(f.Content),
			})
		}
	}

	// Raw agent token (best-effort — post-restart peers only
	// have the kv hash, so the callback may return false; that's
	// acceptable in v1, the target will require a re-issue
	// after a stranded sync but the rest of the state still
	// migrates).
	if tok, ok := agent.LookupAgentToken(agentID); ok {
		req.AgentToken = tok
	}
	return req, nil
}

// agentSyncStateLegacyTargetErr is returned by
// dispatchPeerAgentSyncState when target's binary predates the
// /agent-sync/state route. Sentinel so the orchestrator can
// downgrade to full-sync only for THIS specific case and
// hard-fail on auth/holder rejections (which signal a config
// problem the operator must see before begin runs).
var agentSyncStateLegacyTargetErr = errors.New("peer /agent-sync/state not available on target (legacy binary)")

// dispatchPeerAgentSyncState POSTs a signed peer envelope to
// target's POST /api/v1/peers/agent-sync/state and returns the
// store.AgentSyncState the target reports for the agent. Used
// by the orchestrator BEFORE buildAgentSyncRequest so the
// transcript payload only contains the delta target doesn't
// already have.
//
// Error policy:
//
//   - 404: returns (nil, agentSyncStateLegacyTargetErr). Caller
//     downgrades to full-sync transparently.
//   - 401 / 403 / 409 / 5xx: returns (nil, err) — caller treats
//     as a hard pre-begin failure. An auth or holder mismatch
//     means the switch is misconfigured; silently full-syncing
//     would only postpone the same rejection to /agent-sync.
//   - Network / unmarshal: same — hard fail.
func (s *Server) dispatchPeerAgentSyncState(ctx context.Context, targetAddr, targetDeviceID, agentID string) (*store.AgentSyncState, error) {
	body, err := json.Marshal(map[string]string{
		"source_device_id": s.peerID.DeviceID,
		"agent_id":         agentID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal state body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		targetAddr+"/api/v1/peers/agent-sync/state", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build state request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	nonce, err := peer.MakeNonce()
	if err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	if err := peer.SignRequest(req, s.peerID.DeviceID, s.peerID.PrivateKey, nonce, targetDeviceID); err != nil {
		return nil, fmt.Errorf("sign state request: %w", err)
	}
	client := peer.NoKeepAliveHTTPClient(handoffOpTimeout)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dispatch state: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusNotFound {
		// Backward compat: older target binary without the
		// /state endpoint. Sentinel lets the caller downgrade
		// to full-sync.
		return nil, agentSyncStateLegacyTargetErr
	}
	if resp.StatusCode != http.StatusOK {
		// 401/403/409/5xx — surface verbatim. Caller will
		// hard-fail before begin rather than silently full-sync
		// past an auth/holder rejection.
		return nil, fmt.Errorf("state HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var out store.AgentSyncState
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode state response: %w", err)
	}
	return &out, nil
}

// encodeAgentSyncWire marshals the payload as JSON, gzip-
// compresses it, and returns BOTH the raw JSON length and the
// compressed bytes — the exact shape dispatchPeerAgentSync
// will POST. Exposed as a separate step so the orchestrator
// can preflight both caps (raw vs wire) against the receiver's
// limits BEFORE begin (handoff_pending is set), avoiding a
// round-trip + abort cycle on oversize payloads.
//
// rawLen is returned alongside body so the caller can compare
// against peerAgentSyncMaxBody (the decompressed-size cap on
// the receiver) — a payload that gzips small but expands huge
// would otherwise pass the wire-size preflight only to hit
// target's decompressed cap after begin.
func encodeAgentSyncWire(payload *peerAgentSyncRequest) (body []byte, rawLen int, err error) {
	raw, merr := json.Marshal(payload)
	if merr != nil {
		return nil, 0, fmt.Errorf("marshal sync body: %w", merr)
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, werr := gz.Write(raw); werr != nil {
		return nil, 0, fmt.Errorf("gzip sync body: %w", werr)
	}
	if cerr := gz.Close(); cerr != nil {
		return nil, 0, fmt.Errorf("gzip flush sync body: %w", cerr)
	}
	return compressed.Bytes(), len(raw), nil
}

// dispatchPeerAgentSync POSTs the precomputed gzipped wire body
// to target's /api/v1/peers/agent-sync with an Ed25519-signed
// envelope. The body's gzip framing fits inside the peer auth
// middleware's wire cap (the orchestrator preflighted that);
// target's handler honours the Content-Encoding header to
// decompress. Network and decode errors propagate to the
// caller so the orchestrator can roll back via abort.
func (s *Server) dispatchPeerAgentSync(ctx context.Context, targetAddr, targetDeviceID string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		targetAddr+"/api/v1/peers/agent-sync", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build sync request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")

	nonce, err := peer.MakeNonce()
	if err != nil {
		return fmt.Errorf("nonce: %w", err)
	}
	if err := peer.SignRequest(req, s.peerID.DeviceID, s.peerID.PrivateKey, nonce, targetDeviceID); err != nil {
		return fmt.Errorf("sign sync request: %w", err)
	}

	client := peer.NoKeepAliveHTTPClient(switchDeviceOpTimeout)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("dispatch sync: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sync HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

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
	"strings"
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
//	  "outcome":  "completed" | "completed_finalize_failed"
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

// syncAgentMemoryFromDiskFn / syncAgentPersonaFromDiskFn are the
// pre-sync flush steps, indirected through package vars so the
// degraded-mode tests can inject a deterministic failure (the real
// functions only fail on genuine FS / DB errors that are awkward to
// stage in a unit test).
var (
	syncAgentMemoryFromDiskFn  = agent.SyncAgentMemoryFromDisk
	syncAgentPersonaFromDiskFn = agent.SyncAgentPersonaFromDisk
)

type switchDeviceRequest struct {
	TargetPeerID string `json:"target_peer_id"`
	// Degraded opts in to transcript-only transfer when the
	// pre-sync disk→DB memory / persona flush fails. Default
	// (false) keeps the hard-fail contract: a failed flush
	// aborts the switch with memory_flush_failed /
	// persona_flush_failed. With the flag set, the switch
	// proceeds and the skipped flushes are recorded in the
	// response (degraded_flushes), the logs, and the target's
	// arrival prompt so the agent knows memory may be stale.
	Degraded bool `json:"degraded,omitempty"`
}

type switchDeviceResponse struct {
	AgentID      string `json:"agent_id"`
	TargetPeerID string `json:"target_peer_id"`
	Outcome      string `json:"outcome"`
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
	// source_drain_failed / complete_errored_lock_at_target).
	// Without this the caller —
	// typically the agent driving the kojo-switch-device skill —
	// sees only "outcome=aborted" and has no diagnostic to report
	// to the user. Best-effort prose, not a stable code.
	Reason string `json:"reason,omitempty"`
	// PullSkipped is true when the agent has no portable blob_refs
	// rows (currently avatar.*) — the pull step is a no-op and we
	// proceed straight to complete (which still transfers the
	// agent_lock).
	PullSkipped bool `json:"pull_skipped,omitempty"`
	// AgentSynced reports whether the §3.7 agent-sync step
	// landed the agent row + transcript + persona + memory +
	// claude session JSONLs on target. False means the switch
	// aborted before sync had a chance to fire.
	AgentSynced bool `json:"agent_synced,omitempty"`
	// Degraded / DegradedFlushes report that the switch ran in
	// opt-in degraded mode AND at least one pre-sync flush was
	// actually skipped ("memory_flush", "persona_flush"). The
	// same list rides the sync payload so target's arrival
	// prompt warns the agent about potentially stale memory.
	Degraded        bool     `json:"degraded,omitempty"`
	DegradedFlushes []string `json:"degraded_flushes,omitempty"`
	// TransferSkips lists every session file the sync payload
	// left behind (oversized claude JSONL, unreadable codex ref,
	// …). Also shipped to target for the arrival prompt and the
	// owner-facing UI notice.
	TransferSkips []agent.SkippedSessionFile `json:"transfer_skips,omitempty"`
	// TokenAutoReissue is true when source could not include the
	// raw $KOJO_AGENT_TOKEN (post-restart peers only hold the kv
	// hash). Target auto-re-issues at finalize IF it also lacks
	// the raw (a target that still caches a matching raw needs no
	// repair) — either way no manual re-issue step remains.
	TokenAutoReissue bool `json:"token_auto_reissue,omitempty"`
}

func (s *Server) handleAgentHandoffSwitch(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAgentStore(w, "handoff requires agent store"); !ok {
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
	if !p.HasOwnerAuthority() && !(p.IsAgent() && p.AgentID == agentID) {
		writeError(w, http.StatusForbidden, "forbidden",
			"owner or self-agent only")
		return
	}

	// Serialize switch transactions, including deferred Goal execution. This
	// lock is not retained while the requesting native turn finishes.
	unlockSwitch := s.lockPendingFinalize(pendingSyncKey{AgentID: agentID, OpID: "switch-transaction"})
	defer unlockSwitch()
	pending, pendingErr := agent.PendingGoalHandoff(agentID)
	executionIdentity, _ := r.Context().Value(goalHandoffExecutionKey{}).(*goalHandoffOperation)
	if pendingErr != nil {
		writeError(w, 409, "checkpoint_unreadable", pendingErr.Error())
		return
	}
	if pending != nil && (executionIdentity == nil || pending.Handoff.ID != executionIdentity.ID) {
		writeError(w, 409, "goal_handoff_pending", "A Goal move is already reserved. Finish that turn, or pause the Goal to cancel. Do not force-reclaim.")
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
	// Freshness guard. peerCountLookup (cmd/kojo/main.go) only
	// checks for the existence of a non-self peer_registry row —
	// it intentionally does NOT filter on online status so the
	// skill stays installed across transient peer offlines.
	// Online/freshness enforcement lives HERE, server-side: a row
	// that's not currently online (or that survived a daemon
	// restart with a stale last_seen past peer.OfflineThreshold)
	// is almost certainly unreachable. Failing fast saves a
	// 5-minute switch attempt that would time out in the pull leg
	// and surface a confusing abort_failed outcome.
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
	targetAddr, err := peer.NormalizeAddress(targetRec.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"target peer has no usable dial name in peer_registry: "+err.Error())
		return
	}

	// Sanity-check the source. Two pre-conditions:
	//
	//   (a) Every portable blob_refs row for the agent must
	//       currently live on the local peer. Orchestrating from a
	//       peer that isn't the home_peer would have the target dial
	//       us for blobs we don't have. Historical attach/ blobs are
	//       not part of the switch payload and intentionally do not
	//       gate the handoff.
	//
	//   (b) The agent_lock must exist and be held by the local
	//       peer. Without this check, an agent with zero blobs
	//       could trigger a lock migration even when the lock
	//       currently sits on a third peer — or, worse, complete
	//       could switch blob_refs with no fencing authority to
	//       transfer. The lock-first complete reorder relies on
	//       the orchestrator owning the lock to begin with.
	refs, err := s.listHandoffBlobRefs(ctx, agentID)
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
		ctx = context.WithValue(ctx, sourceHandoffVersionKey{}, store.AgentLockVersion{Token: cur.FencingToken, Holder: cur.HolderPeer})
		if cur.HolderPeer != s.peerID.DeviceID {
			writeError(w, http.StatusConflict, "wrong_source",
				fmt.Sprintf("agent_lock holder is %s, not the local peer; orchestrate the switch from that peer",
					cur.HolderPeer))
			return
		}
	} else if errors.Is(lerr, store.ErrNotFound) {
		writeError(w, http.StatusConflict, "lock_missing",
			"agent_lock row is missing; local runtime is not fenced, so device-switch cannot safely transfer ownership")
		return
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
	if _, err := peer.NormalizeAddress(selfRec.URL); err != nil {
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
	// External/thread self-calls additionally carry KOJO_SESSION_KEY, which
	// resolves the exact tracked one-shot so only that caller survives the
	// drain. Keyless main-WebUI self-calls retain the bearer-only legacy path.
	//
	// Caveat (claude session JSONL): on selfCall the snapshot
	// captures the JSONL while a tool_use (this curl) is mid-
	// flight; target's `claude --continue` may see a torn final
	// turn. Pre-existing limitation — the non-selfCall path also
	// produces a torn turn at the SIGTERM point.
	execution, _ := r.Context().Value(goalHandoffExecutionKey{}).(*goalHandoffOperation)
	selfCall := p.IsAgent() && p.AgentID == agentID && execution == nil
	if execution != nil {
		if req.Degraded || execution.AgentID != agentID || execution.Target != req.TargetPeerID || execution.Source != s.peerID.DeviceID {
			writeError(w, 409, "goal_changed", "handoff identity mismatch")
			return
		}
		if err := s.agents.Store().CheckFencing(ctx, agentID, execution.Source, execution.FencingToken); err != nil {
			writeError(w, 409, "wrong_source", err.Error())
			return
		}
		if _, err := s.agents.GoalHandoffCheckpoint(agentID, execution.SessionKey, execution.ID); err != nil {
			writeError(w, 409, "goal_changed", err.Error())
			return
		}
		if !s.targetSupportsGoalHandoff(ctx, targetAddr, agentID) {
			writeError(w, 409, "goal_handoff_unsupported", "target no longer supports goal handoff")
			return
		}
	}
	if selfCall && agent.NativeGoalRunning(agentID, strings.TrimSpace(r.Header.Get("X-Kojo-Session-Key"))) {
		if req.Degraded {
			writeError(w, 409, "degraded_goal_handoff", "Goal handoff requires a complete checkpoint")
			return
		}
		s.queueGoalHandoff(w, r, agentID, req.TargetPeerID, targetAddr)
		return
	}
	var callerOneShot agent.OneShotOrigin
	var continuation *handoffContinuation
	if execution != nil {
		binding, err := s.agents.GoalHandoffCheckpoint(agentID, execution.SessionKey, execution.ID)
		if err != nil {
			writeError(w, 409, "goal_changed", err.Error())
			return
		}
		origin := binding.OriginPeerID
		if origin == "" {
			origin = execution.Source
		}
		continuation = &handoffContinuation{SessionKey: execution.SessionKey, OriginPeerID: origin, GoalHandoffID: execution.ID}
	}
	if selfCall {
		sessionKey := strings.TrimSpace(r.Header.Get("X-Kojo-Session-Key"))
		if len(sessionKey) > 1024 {
			writeError(w, http.StatusBadRequest, "bad_request", "X-Kojo-Session-Key is too long")
			return
		}
		if sessionKey != "" {
			var ok bool
			callerOneShot, ok = s.agents.InFlightOneShotOrigin(agentID, sessionKey)
			if !ok {
				writeError(w, http.StatusConflict, "caller_not_active",
					"the conversation that requested this switch is no longer the active one-shot turn")
				return
			}
			if callerOneShot.OriginPeerID != "" && callerOneShot.HandoffCapability != "" {
				continuation = &handoffContinuation{
					SessionKey: callerOneShot.SessionKey, OriginPeerID: callerOneShot.OriginPeerID,
					Capability: callerOneShot.HandoffCapability,
				}
			}
		}
	}
	if continuation != nil && continuation.GoalHandoffID == "" && !s.targetSupportsOriginAwareArrival(r.Context(), targetAddr, agentID) {
		s.logger.Warn("switch-device: target lacks origin-aware arrival capability; using legacy main WebUI arrival",
			"agent", agentID, "target", req.TargetPeerID)
		continuation = nil
	}

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
		if err := s.agents.SetSwitching(agentID, true); err != nil {
			// Restart drain is quiescing the daemon — refuse the
			// switch so the re-exec can't cut it in half.
			writeError(w, http.StatusConflict, "agent_busy",
				"cannot start device switch: "+err.Error())
			return
		}
		defer s.agents.SetSwitching(agentID, false)
		if selfCall {
			if callerOneShot.ID != 0 {
				s.agents.AbortExceptOneShot(agentID, callerOneShot.ID)
			} else {
				s.agents.CancelOneShotsForAgent(agentID)
			}
		} else {
			s.agents.Abort(agentID)
		}
		quiesceCtx, quiesceCancel := context.WithTimeout(ctx, 3*time.Second)
		var err error
		if selfCall {
			if callerOneShot.ID != 0 {
				err = s.agents.WaitChatIdleExceptOneShot(quiesceCtx, agentID, callerOneShot.ID)
			} else {
				err = s.agents.WaitChatIdleSelfCall(quiesceCtx, agentID)
			}
		} else {
			err = s.agents.WaitChatIdle(quiesceCtx, agentID)
		}
		quiesceCancel()
		if err != nil {
			writeError(w, http.StatusConflict, "agent_busy",
				"source chat did not drain within quiesce window; retry switch-device: "+err.Error())
			return
		}
		// Drained: close the persistent claude process so its session file is
		// released before the switch transfers session state to the peer.
		s.agents.CloseClaudeSessionSync(agentID)
		// Keyed sessions closed above report abandoned background tasks
		// asynchronously (to a remote Hub for a holder). Deliver them while
		// this peer still holds the lock: after the transfer the Hub rejects
		// this now-stale holder's notices. Bounded; never blocks the switch.
		if !s.agents.WaitKeyedExitNotices(agentID, keyedBgHandoffNoticeWait) {
			s.logger.Warn("keyed background exit notices still pending at handoff", "agent", agentID)
		}
	}

	// Step 0-pre: flush source's disk-side memory writes into the
	// DB BEFORE building the sync payload. The agent's own chat
	// turn (which is what called this handler on the self-call
	// path) may have just used the Write tool to create
	// memory/daily/<date>.md or any other memory_entries body
	// that hasn't been picked up by the next prepareChat's
	// SyncAgentMemoryFromDiskBestEffort yet. Without this flush
	// buildAgentSyncRequest's ListMemoryEntries reads stale rows
	// and target ships back missing the freshly-written diary —
	// visible to the user as "日記が反映されない" on return.
	//
	// Same fix applies to MEMORY.md (syncAgentMemoryToDB inside)
	// so an edit during the closing turn doesn't get dropped.
	//
	// Hard fail: a failed flush leaves source's DB stale, and the
	// switch would silently ship a sync payload missing the newest
	// diary entry / MEMORY.md edit. The source would also be
	// released afterwards, so the operator has no path back to
	// recover the missing rows. Bail BEFORE begin so handoff_pending
	// stays clear and the operator can retry the switch.
	//
	// NOTE: SyncAgentMemoryFromDisk does NOT respect the caller's
	// ctx — both the per-agent memorySyncMu acquisition and the
	// internal DB ctx are rooted at context.Background() (see
	// dbContextWithCancel). switchDeviceOpTimeout above ONLY caps
	// the parent ctx; the sync itself can outlast it. In practice
	// the call returns in milliseconds because Step -1's
	// WaitChatIdle just drained every concurrent writer that could
	// be holding memorySyncMu; the DB sync runs against a quiet
	// agent. The honest bound is "as long as a single memory
	// sync takes," which is dominated by FS scan size and SQLite
	// write throughput. Adding a ctx-aware lock acquire is a
	// separate refactor.
	var degradedFlushes []string
	if s.agents != nil && s.agents.Store() != nil {
		if err := syncAgentMemoryFromDiskFn(ctx, s.agents.Store(), agentID, s.logger); err != nil {
			if req.Degraded {
				// Opt-in degraded mode: proceed without the flush.
				// The sync payload still ships source's DB rows —
				// deliberately. Those rows hold the LAST SUCCESSFUL
				// flush, and under the fencing model they are always
				// ≥ target's copy (only the lock holder mutates
				// agent state, and the lock is at source; target's
				// rows froze when the agent last left it). Omitting
				// memory/persona instead would WIPE target: the
				// wire treats nil persona/memory as "source has
				// none → DELETE" and non-incremental memory_entries
				// as DELETE-then-INSERT. The only loss is the
				// un-flushed disk edits, which is exactly what
				// degraded_flushes records on the response, the
				// sync payload (→ target's arrival prompt), and
				// the log line here.
				s.logger.Warn("switch-device: degraded mode: disk→DB memory flush failed; proceeding transcript-only",
					"agent", agentID, "err", err)
				degradedFlushes = append(degradedFlushes, "memory_flush")
			} else {
				s.logger.Error("switch-device: pre-sync disk→DB flush failed; refusing switch to avoid shipping stale memory state",
					"agent", agentID, "err", err)
				s.noteSwitchFailure(agentID, "memory_flush_failed: "+err.Error())
				writeError(w, http.StatusInternalServerError, "memory_flush_failed",
					"disk→DB memory flush failed; switch aborted to avoid shipping stale state (retry with \"degraded\":true to proceed transcript-only): "+err.Error())
				return
			}
		}
		// persona.md rides its own personaSyncMu, NOT the memorySyncMu
		// path above, so SyncAgentMemoryFromDisk does not flush it.
		// Manager.syncPersona would, but it bails once SetSwitching is
		// set (Step -1). Flush it directly here so a persona edit made
		// during the closing turn isn't dropped from the sync payload.
		// Same hard-fail contract as the memory flush.
		if err := syncAgentPersonaFromDiskFn(ctx, s.agents.Store(), agentID, s.logger); err != nil {
			if req.Degraded {
				s.logger.Warn("switch-device: degraded mode: persona disk→DB flush failed; proceeding transcript-only",
					"agent", agentID, "err", err)
				degradedFlushes = append(degradedFlushes, "persona_flush")
			} else {
				s.logger.Error("switch-device: pre-sync persona disk→DB flush failed; refusing switch to avoid shipping stale persona state",
					"agent", agentID, "err", err)
				s.noteSwitchFailure(agentID, "persona_flush_failed: "+err.Error())
				writeError(w, http.StatusInternalServerError, "persona_flush_failed",
					"disk→DB persona flush failed; switch aborted to avoid shipping stale state (retry with \"degraded\":true to proceed transcript-only): "+err.Error())
				return
			}
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
		s.noteSwitchFailure(agentID, "state_probe_failed: "+perr.Error())
		writeError(w, http.StatusBadGateway, "state_probe_failed",
			"agent-sync state probe failed; refusing to switch: "+perr.Error())
		return
	}

	// Step 0: build the agent-sync payload BEFORE begin. When
	// targetState is non-nil + Known, the builder filters
	// messages / memory_entries by seq so only the canonical-history
	// delta missing on the target rides the wire. Native backend sessions
	// are ordered newest-first and fitted to the transfer envelope below;
	// the remaining agent state is sent as a complete snapshot.
	//
	// Failure here is a precondition error (source missing data
	// we'd need to migrate); we bail BEFORE marking
	// handoff_pending so no rollback is needed.
	syncReq, serr := s.buildAgentSyncRequest(ctx, agentID, targetState)
	if serr != nil {
		s.noteSwitchFailure(agentID, "build agent-sync payload: "+serr.Error())
		writeError(w, http.StatusInternalServerError, "internal",
			"build agent-sync payload: "+serr.Error())
		return
	}
	// Stamp degraded-mode + skip metadata on both the wire payload
	// (target persists it into the pending entry and the agent row
	// so the arrival prompt / owner UI can surface it) and the
	// switch response (operator + skill see it immediately).
	syncReq.DegradedFlushes = degradedFlushes
	resp.DegradedFlushes = degradedFlushes
	resp.Degraded = len(degradedFlushes) > 0
	// Raw token unavailable on source (hash-only after a restart):
	// target auto-repairs at finalize if it also lacks the raw.
	// Surface the fact so the operator knows no raw rode the wire.
	resp.TokenAutoReissue = syncReq.AgentToken == ""

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
			//
			// Seed maxSeq with targetState.MaxMessageSeq when
			// known: incremental sync can ship an EMPTY
			// syncReq.Messages slice (no rows past target's
			// max), and a maxSeq=0 would push inflight.Seq=1
			// straight into a UNIQUE(agent_id, seq) collision
			// with target's existing seq=1 row.
			var maxSeq int64
			if targetState != nil && targetState.Known {
				maxSeq = targetState.MaxMessageSeq
			}
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
	if execution != nil {
		syncReq.OpID = execution.ID
	}
	if continuation != nil && continuation.GoalHandoffID == "" {
		bindCtx, bindCancel := context.WithTimeout(ctx, 5*time.Second)
		bindErr := s.bindHandoffArrivalAtOrigin(bindCtx, continuation.OriginPeerID, handoffArrivalBindRequest{
			SourceDeviceID: s.peerID.DeviceID,
			TargetDeviceID: req.TargetPeerID,
			AgentID:        agentID,
			OpID:           syncReq.OpID,
			SessionKey:     continuation.SessionKey,
			Capability:     continuation.Capability,
		})
		bindCancel()
		if bindErr != nil {
			s.logger.Warn("switch-device: origin conversation could not bind handoff; using legacy main arrival",
				"agent", agentID, "target", req.TargetPeerID, "op_id", syncReq.OpID, "err", bindErr)
			continuation = nil
		}
	}
	keptSessions, capacitySkips, budgetErr := fitAgentSyncSessions(syncReq, int64(peerAgentSyncMaxBody))
	if budgetErr != nil {
		s.noteSwitchFailure(agentID, "fit session transfer budget: "+budgetErr.Error())
		writeError(w, http.StatusInternalServerError, "internal",
			"fit session transfer budget: "+budgetErr.Error())
		return
	}
	if capacitySkips > 0 {
		s.logger.Info("switch-device: session artifacts trimmed to transfer capacity",
			"agent", agentID, "kept", keptSessions, "skipped", capacitySkips,
			"single_shot_cap", peerAgentSyncMaxBody)
	}
	if execution != nil {
		checkpoint, err := s.agents.GoalHandoffCheckpoint(agentID, execution.SessionKey, execution.ID)
		if err != nil {
			writeError(w, 409, "goal_changed", err.Error())
			return
		}
		found := false
		if syncReq.CodexSession != nil {
			for _, t := range syncReq.CodexSession.Threads {
				if t.Goal != nil && t.Goal.SessionKey == execution.SessionKey && t.Goal.Generation == checkpoint.Generation && t.ThreadID == checkpoint.State.ThreadID && t.Goal.State != nil && t.Goal.State.Status == "paused" && t.Goal.DesiredPaused && t.Goal.Handoff != nil && t.Goal.Handoff.ID == execution.ID && t.NativeGoal != nil && t.NativeGoal.Row != nil && t.ThreadRow != nil && t.RolloutContentB64 != "" {
					found = true
				}
			}
		}
		if !found {
			writeError(w, 409, "checkpoint_missing", "required native goal session was skipped; no transfer performed")
			return
		}
		execution.Phase = "transferring"
		if err := s.saveGoalHandoff(ctx, execution); err != nil {
			writeError(w, 500, "internal", err.Error())
			return
		}
	}
	resp.TransferSkips = syncReq.TransferSkips
	// Surface op_id in the response immediately so even early
	// failures (begin / sync / pull / complete) carry the
	// identifier the operator needs to correlate target-side
	// pending state or drive a manual drop.
	resp.OpID = syncReq.OpID

	// Step 0.5: decide single-shot vs chunked BEFORE the heavy
	// encode. estimateAgentSyncRawSize walks each entity in turn
	// (peak memory = one largest row), so an oversized payload is
	// detected without pinning ~2× its size on source. Only when
	// the estimate fits the single-shot cap do we run the full
	// encodeAgentSyncWire (which itself buffers raw + gzip).
	//
	// When the payload busts the cap the orchestrator falls back
	// to the chunked protocol
	// (/api/v1/peers/agent-sync/chunked/{begin,chunk,commit}) which
	// splits the row arrays across multiple POSTs so neither side
	// holds the full payload in memory at once.
	estimateRaw, estErr := estimateAgentSyncRawSize(syncReq)
	if estErr != nil {
		s.noteSwitchFailure(agentID, "estimate agent-sync size: "+estErr.Error())
		writeError(w, http.StatusInternalServerError, "internal",
			"estimate agent-sync size: "+estErr.Error())
		return
	}
	useChunked := estimateRaw > int64(peerAgentSyncMaxBody)
	var syncWireBody []byte
	var syncRawLen int
	if useChunked {
		s.logger.Info("switch-device: agent state exceeds single-shot caps; routing through chunked agent-sync",
			"agent", agentID, "target", req.TargetPeerID,
			"estimated_raw_bytes", estimateRaw, "single_shot_cap", peerAgentSyncMaxBody,
			"chunk_budget", chunkedSyncBudgetBytes)
	} else {
		// Within cap: encode now so dispatchPeerAgentSync gets
		// reusable bytes. If the actual encoded raw size somehow
		// busts the cap that the estimator missed (e.g. JSON
		// escape blow-up on certain UTF-8 sequences), fall back
		// to chunked mode rather than 413 against target.
		var werr error
		syncWireBody, syncRawLen, werr = encodeAgentSyncWire(syncReq)
		if werr != nil {
			s.noteSwitchFailure(agentID, "encode agent-sync wire: "+werr.Error())
			writeError(w, http.StatusInternalServerError, "internal",
				"encode agent-sync wire: "+werr.Error())
			return
		}
		if int64(syncRawLen) > int64(peerAgentSyncMaxBody) ||
			int64(len(syncWireBody)) > int64(peerAgentSyncMaxWireBody) {
			s.logger.Warn("switch-device: post-encode size busted single-shot cap despite estimator; switching to chunked",
				"agent", agentID, "estimate", estimateRaw,
				"actual_raw", syncRawLen, "actual_wire", len(syncWireBody))
			useChunked = true
			syncWireBody = nil
		}
	}

	// Step 1: begin.
	beginResp, err := s.runHandoffOp(ctx, agentID, "begin", req.TargetPeerID)
	if err != nil {
		s.noteSwitchFailure(agentID, "handoff begin: "+err.Error())
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
	var syncErr error
	if useChunked {
		syncErr = s.dispatchPeerAgentSyncChunked(ctx, targetAddr, syncReq)
	} else {
		syncErr = s.dispatchPeerAgentSync(ctx, targetAddr, req.TargetPeerID, syncWireBody)
	}
	if syncErr != nil {
		// Mark AgentSynced=true unconditionally for the
		// abort path so orchestrateAbort sends drop — the
		// network error means target may have committed
		// before its response was lost.
		resp.AgentSynced = true
		s.orchestrateAbort(ctx, agentID, targetAddr, req.TargetPeerID, syncReq.OpID, &resp,
			"agent-sync dispatch: "+syncErr.Error())
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

	// Step 2: pull. Skip the network round-trip when the agent owns
	// no portable blobs (for example, no custom avatar); the lock
	// transfer in `complete` is still meaningful.
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
		// No direct note here: whether the agent is still local
		// is unknown until the lock reconciliation below. Case
		// (b) (lock stayed local) notes via orchestrateAbort;
		// case (a) (lock at target) intentionally gets NO note —
		// source has released the agent and a post-snapshot
		// local row would diverge from the transcript target
		// committed. Same reasoning for the released paths
		// further down (blob row non-ok / post-complete drain
		// failure).
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
					if selfCall {
						s.agents.DetachOneShot(agentID, callerOneShot.ID)
					}
					releaseCtx, releaseCancel := context.WithTimeout(context.Background(), handoffOpTimeout)
					s.releaseHandoffSource(releaseCtx, agentID, req.TargetPeerID, lock.FencingToken)
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
	if !completeResp.LockTransferred {
		resp.Outcome = "complete_failed"
		resp.Reason = "complete: agent_lock did not transfer; restored source ownership to avoid a blob-only migration"
		if s.peerID != nil && s.agents != nil && s.agents.Store() != nil {
			reclaimCtx, reclaimCancel := context.WithTimeout(context.Background(), handoffOpTimeout)
			unlockRestore := s.lockPendingFinalize(pendingSyncKey{AgentID: agentID})
			rec, rerr := s.agents.Store().ForceReclaimAgentToLocal(
				reclaimCtx, agentID, s.peerID.DeviceID,
				store.NowMillis(), forceReclaimLeaseDuration.Milliseconds(),
			)
			if rerr != nil {
				resp.Reason += "; automatic source restore failed: " + rerr.Error()
				s.logger.Error("switch-device: complete returned no lock transfer and source restore failed",
					"agent", agentID, "target", req.TargetPeerID,
					"op_id", syncReq.OpID, "err", rerr)
			} else {
				if s.onAgentForceReclaimed != nil {
					s.onAgentForceReclaimed(reclaimCtx, agentID)
				}
				s.logger.Error("switch-device: complete returned no lock transfer; source ownership restored",
					"agent", agentID, "target", req.TargetPeerID,
					"op_id", syncReq.OpID, "fencing_token", rec.FencingToken)
			}
			unlockRestore()
			reclaimCancel()
		}
		if resp.AgentSynced && targetAddr != "" && req.TargetPeerID != "" && syncReq.OpID != "" {
			dropCtx, dropCancel := context.WithTimeout(context.Background(), handoffOpTimeout)
			if derr := s.dispatchPeerAgentSyncDrop(dropCtx, targetAddr, req.TargetPeerID, agentID, syncReq.OpID); derr != nil {
				s.logger.Warn("switch-device: no-lock repair could not drop target pending sync",
					"agent", agentID, "target", req.TargetPeerID,
					"op_id", syncReq.OpID, "err", derr)
				resp.Reason += "; target pending-sync drop failed: " + derr.Error()
			}
			dropCancel()
		}
		writeJSONResponse(w, http.StatusOK, resp)
		return
	}
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
				if selfCall {
					s.agents.DetachOneShot(agentID, callerOneShot.ID)
				}
				releaseCtx, releaseCancel := context.WithTimeout(context.Background(), handoffOpTimeout)
				s.releaseHandoffSource(releaseCtx, agentID, req.TargetPeerID, completeResp.LockFencing)
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
			if callerOneShot.ID != 0 {
				s.agents.AbortExceptOneShot(agentID, callerOneShot.ID)
			} else {
				s.agents.CancelOneShotsForAgent(agentID)
			}
		} else {
			s.agents.Abort(agentID)
		}
		drainCtx, drainCancel := context.WithTimeout(ctx, 5*time.Second)
		if selfCall {
			if callerOneShot.ID != 0 {
				drainErr = s.agents.WaitChatIdleExceptOneShot(drainCtx, agentID, callerOneShot.ID)
			} else {
				drainErr = s.agents.WaitChatIdleSelfCall(drainCtx, agentID)
			}
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
			if selfCall {
				s.agents.DetachOneShot(agentID, callerOneShot.ID)
			}
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), handoffOpTimeout)
			s.releaseHandoffSource(releaseCtx, agentID, req.TargetPeerID, completeResp.LockFencing)
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
	// A legacy/main selfCall defers finalize into a background goroutine that
	// first waits for source's claude to emit its
	// post-tool-result `done` event. The captured assistant
	// Message rides as the TailMessage payload of finalize so
	// target appends it BEFORE the arrival continuation fires —
	// the LLM on target then sees its own commitment text in
	// transcript (e.g. "到着したらセキュリティチェックを実施する"),
	// which the pre-A architecture silently dropped because the
	// §3.7 release guard skipped persistDoneEvent on source.
	//
	// Origin-aware external selfCalls finalize synchronously while the adapter's
	// FIFO reservation is still alive; non-selfCalls are synchronous as before.
	var finalizeErr error
	if selfCall && continuation == nil && callerOneShot.SessionKey == "" {
		// Defer finalize: response returns to claude immediately
		// (curl unblocks → claude continues turn → done event
		// fires → goroutine ships finalize with tail). Outcome is
		// forced to "completed" because the orchestrator no
		// longer synchronously knows the finalize result; the
		// SKILL.md tells claude to stay silent on completed so a
		// missing finalize_error surface here doesn't leak to the
		// user.
		go s.runDeferredFinalize(targetAddr, req.TargetPeerID, agentID, syncReq.OpID, completeResp.LockFencing)
	} else if selfCall && continuation == nil && callerOneShot.ID != 0 {
		// The response adapter identified this as a one-shot, but the target
		// could not negotiate origin-aware continuation (old target or failed
		// capability bind). Preserve legacy main-chat arrival without letting it
		// overlap the source turn that is still blocked inside this curl.
		go s.runDeferredOneShotFinalize(targetAddr, req.TargetPeerID, agentID, syncReq.OpID, callerOneShot.ID, completeResp.LockFencing)
	} else {
		// Uses a fresh background ctx so a wedged target doesn't
		// stall past switchDeviceOpTimeout — the switch is already
		// irreversible at this point, finalize is best-effort
		// runtime activation.
		// Bounded retry: finalize's `lock_not_self` (target's
		// AcquireAgentLock hit a still-live source row) is the
		// recoverable race — the AgentLockGuard refresh loop will
		// re-acquire as the lease expires, then a retry of finalize
		// succeeds. Three short attempts cover the typical drain
		// window without blocking the operator's UI indefinitely.
		// Non-recoverable errors (network, 4xx other than
		// lock_not_self) bail on the first attempt.
		const finalizeAttempts = 3
		const finalizeRetryBackoff = 500 * time.Millisecond
		for attempt := 0; attempt < finalizeAttempts; attempt++ {
			finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), handoffOpTimeout)
			finalizeErr = s.dispatchPeerAgentSyncFinalize(finalizeCtx, targetAddr, req.TargetPeerID, agentID, syncReq.OpID, nil, continuation, completeResp.LockFencing)
			finalizeCancel()
			if finalizeErr == nil {
				break
			}
			if errors.Is(finalizeErr, errFinalizeContinuationDowngrade) {
				// The target rolled back after its capability probe. For a
				// one-shot self-call, legacy main arrival must remain behind the
				// source turn that is still waiting for this HTTP response.
				continuation = nil
				if selfCall && callerOneShot.ID != 0 {
					go s.runDeferredOneShotFinalize(targetAddr, req.TargetPeerID, agentID, syncReq.OpID, callerOneShot.ID, completeResp.LockFencing)
					finalizeErr = nil
					break
				}
				// No live response-surface turn needs a barrier. Send the legacy
				// finalize immediately rather than consuming another loop attempt:
				// the capability downgrade can be discovered on the final attempt.
				legacyCtx, legacyCancel := context.WithTimeout(context.Background(), handoffOpTimeout)
				finalizeErr = s.dispatchPeerAgentSyncFinalize(legacyCtx, targetAddr, req.TargetPeerID, agentID, syncReq.OpID, nil, nil, completeResp.LockFencing)
				legacyCancel()
				if finalizeErr == nil {
					break
				}
			}
			// Only retry the lock-not-self race; everything else is
			// either fatal (4xx that won't fix itself) or fits the
			// "operator manual retry / force-reclaim" path.
			if !retryableFinalizeError(finalizeErr) {
				break
			}
			if attempt+1 < finalizeAttempts {
				time.Sleep(finalizeRetryBackoff)
			}
		}
		if finalizeErr != nil {
			s.logger.Warn("switch-device: agent-sync finalize failed; operator may need to retry",
				"agent", agentID, "target", req.TargetPeerID,
				"op_id", syncReq.OpID, "err", finalizeErr)
			// Surface error detail on the response so the operator
			// does not have to grep the log; the op_id field already
			// carries the recovery identifier.
			resp.FinalizeError = finalizeErr.Error()
		}
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
	//
	// SELFCALL ORDERING: release MUST happen AFTER the deferred-
	// finalize goroutine is spawned (above) but the goroutine
	// itself may need the caller to finish unwinding. Source release
	// normally cancels one-shots, so DetachOneShot removes only this
	// identified caller from lifecycle cancellation first. The caller
	// then exits via its own defer chain once backendCh closes.
	if s.onAgentReleasedAsSource != nil {
		if selfCall {
			s.agents.DetachOneShot(agentID, callerOneShot.ID)
		}
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), handoffOpTimeout)
		s.releaseHandoffSource(releaseCtx, agentID, req.TargetPeerID, completeResp.LockFencing)
		releaseCancel()
	}

	switch {
	case selfCall && continuation == nil && completeResp.LockTransferred:
		// Deferred finalize: we don't synchronously know whether
		// finalize succeeded, so report "completed" optimistically.
		// A failure surfaces in the server log; the SKILL.md tells
		// claude not to act on finalize_error on the self-call
		// path, so this doesn't regress the user-visible flow.
		resp.Outcome = "completed"
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
		// Defensive fallback: the post-complete guard above should
		// have already caught LockTransferred=false, restored source
		// ownership, and returned before source release / finalize.
		resp.Outcome = "complete_failed"
		resp.Reason = "complete: agent_lock did not transfer"
	}

	// (Source drain + finalize already happened above before
	// the LockTransferred branch.)
	writeJSONResponse(w, http.StatusOK, resp)
}

// targetSupportsOriginAwareArrival negotiates the optional continuation wire
// extension before begin transfers any ownership. A missing route/field is an
// expected old-target downgrade, not a switch failure.
func (s *Server) targetSupportsOriginAwareArrival(ctx context.Context, targetAddr, agentID string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, defaultExternalChatProbe)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet,
		targetAddr+"/api/v1/agents/"+agentID+"/external-chat/ready", nil)
	if err != nil {
		return false
	}
	resp, err := peer.NoKeepAliveHTTPClient(defaultExternalChatProbe).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var ready externalChatReadyResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&ready); err != nil {
		return false
	}
	return ready.OriginAwareArrivalV1
}

// deferredFinalizeTailWaitBudget bounds how long the selfCall finalize
// goroutine waits for source's claude to emit a `done` event before
// shipping finalize without the tail. Set generously enough that a
// typical claude turn (a few seconds of post-tool-result text +
// thinking) completes within budget, but capped so a hung backend
// can't strand target's runtime activation indefinitely.
//
// 30s mirrors claude's interactive-turn p95; longer-running tails
// degrade to the no-tail finalize (target's arrival prompt then has
// to rely on the user-instruction quoting added by buildArrivalPrompt
// — strictly worse than the tail path but strictly better than
// silently dropping finalize altogether).
const deferredFinalizeTailWaitBudget = 30 * time.Second

// runDeferredFinalize is the goroutine the legacy/main selfCall switch path
// spawns after /handoff/complete. It waits for the post-tool-result done event
// and ships it as TailMessage. Origin-aware Slack/thread callers finalize
// synchronously instead, while their adapter reservation is still alive.
//
// Failure modes (all surfaced as Warn-level logs, never propagated
// back to the agent — the agent's claude has already disconnected
// from the curl tool call by the time we get here):
//
//   - WaitChatDone returns nil within budget: finalize fires with
//     nil tail (degraded behavior — no commitment text, but still
//     activates target's runtime).
//   - finalize POST fails on all attempts: target stays without
//     runtime activation. Operator must manually retry via
//     /api/v1/peers/agent-sync/finalize.
//
// Runs under context.Background so a parent-handler cancellation
// (e.g. switchDeviceOpTimeout firing right after writeJSONResponse)
// doesn't cut us off mid-wait.
func (s *Server) runDeferredFinalize(targetAddr, targetDeviceID, agentID, opID string, expectedToken ...int64) {
	defer func() {
		// Goroutine-level recover: a panic here would have no
		// supervisor to catch it (the parent handler already
		// returned), and the daemon's net/http panic handler
		// covers HTTP handlers, not detached goroutines.
		if r := recover(); r != nil {
			s.logger.Error("runDeferredFinalize: panic recovered",
				"agent", agentID, "op_id", opID, "panic", r)
		}
	}()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), deferredFinalizeTailWaitBudget)
	var tail *store.MessageRecord
	if s.agents != nil {
		tail = s.agents.WaitChatDone(waitCtx, agentID)
	}
	waitCancel()
	if tail != nil {
		s.logger.Info("switch-device: captured post-tool-result tail; shipping with finalize",
			"agent", agentID, "op_id", opID, "tail_id", tail.ID,
			"tail_content_len", len(tail.Content))
	} else {
		s.logger.Warn("switch-device: source done event not observed within budget; finalizing without tail",
			"agent", agentID, "op_id", opID, "budget", deferredFinalizeTailWaitBudget)
	}

	const finalizeAttempts = 3
	const finalizeRetryBackoff = 500 * time.Millisecond
	var finalizeErr error
	for attempt := 0; attempt < finalizeAttempts; attempt++ {
		finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), handoffOpTimeout)
		finalizeErr = s.dispatchPeerAgentSyncFinalize(finalizeCtx, targetAddr, targetDeviceID, agentID, opID, tail, nil, expectedToken...)
		finalizeCancel()
		if finalizeErr == nil {
			break
		}
		if !retryableFinalizeError(finalizeErr) {
			break
		}
		if attempt+1 < finalizeAttempts {
			time.Sleep(finalizeRetryBackoff)
		}
	}
	if finalizeErr != nil {
		s.logger.Warn("switch-device: deferred finalize failed; target arrival may not fire (operator can manually retry)",
			"agent", agentID, "op_id", opID, "err", finalizeErr)
	}
}

func (s *Server) runDeferredOneShotFinalize(targetAddr, targetDeviceID, agentID, opID string, oneShotID int64, expectedToken ...int64) {
	waitCtx, cancel := context.WithTimeout(context.Background(), deferredFinalizeTailWaitBudget)
	err := s.agents.WaitOneShotDone(waitCtx, agentID, oneShotID)
	cancel()
	if err != nil {
		s.logger.Warn("switch-device: downgraded source one-shot did not exit; leaving target finalize pending",
			"agent", agentID, "op_id", opID, "err", err)
		return
	}
	const attempts = 3
	for attempt := 0; attempt < attempts; attempt++ {
		finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), handoffOpTimeout)
		finalizeErr := s.dispatchPeerAgentSyncFinalize(finalizeCtx, targetAddr, targetDeviceID, agentID, opID, nil, nil, expectedToken...)
		finalizeCancel()
		if finalizeErr == nil {
			return
		}
		if !retryableFinalizeError(finalizeErr) || attempt+1 == attempts {
			s.logger.Warn("switch-device: downgraded one-shot finalize failed",
				"agent", agentID, "op_id", opID, "err", finalizeErr)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func retryableFinalizeError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "lock_not_self") || strings.Contains(msg, "arrival_not_admitted")
}

// finalizeWireBodyCap mirrors the MaxBytesReader limit on target's
// finalize handler. Source pre-checks the marshalled body against
// this cap; an oversized tail payload (huge assistant output, deep
// ToolUses arrays) is dropped + retransmitted as a nil-tail finalize
// so target's runtime activation still happens. The cap MUST stay
// in sync with the receiver-side 16<<20 in
// peer_agent_sync_finalize_handler.go.
const finalizeWireBodyCap = 16 << 20

var errFinalizeContinuationDowngrade = errors.New("target rejected origin-aware finalize continuation")

// dispatchPeerAgentSyncFinalize tells target to commit the
// runtime side effects (TokenStore adopt, AgentLockGuard
// AddAgent) that agent-sync deferred. Best-effort: a failure
// here is logged + ignored by the caller — target still holds
// the agent rows and the operator can manually re-call this
// endpoint to recover.
//
// tail is the optional post-tool-result assistant Message captured
// from source's claude turn AFTER /handoff/complete (self-call defer
// path). When non-nil, target upserts the row before firing the
// arrival hook so the LLM sees its own commitment text in the
// transcript. Nil on every non-self-call path and on self-call paths
// where the source done-event never landed within the wait budget
// OR where the marshalled wire body would bust target's MaxBytesReader
// cap (oversized tail is silently dropped + finalize still proceeds,
// preferring "target runtime activates without the commitment text"
// over "target never activates because finalize itself 413'd").
func (s *Server) dispatchPeerAgentSyncFinalize(ctx context.Context, targetAddr, targetDeviceID, agentID, opID string, tail *store.MessageRecord, continuation *handoffContinuation, expectedToken ...int64) error {
	if len(expectedToken) > 0 {
		current, err := s.agents.Store().GetAgentLockVersion(ctx, agentID)
		if err != nil {
			return err
		}
		if current != (store.AgentLockVersion{Holder: targetDeviceID, Token: expectedToken[0]}) {
			return store.ErrStaleHandoff
		}
	}

	body := peerAgentSyncFinalizeRequest{
		SourceDeviceID: s.peerID.DeviceID,
		AgentID:        agentID,
		OpID:           opID,
		TailMessage:    tail,
		Continuation:   continuation,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal finalize body: %w", err)
	}
	if int64(len(raw)) > finalizeWireBodyCap && tail != nil {
		// Drop the tail and re-marshal. Log so the loss is
		// operator-visible — target's arrival prompt will fall back
		// to the no-tail path (current behavior + the user-msg
		// quoting in buildArrivalPrompt).
		s.logger.Warn("switch-device: finalize body with tail exceeds wire cap; dropping tail",
			"agent", agentID, "op_id", opID,
			"body_bytes", len(raw), "cap", finalizeWireBodyCap)
		body.TailMessage = nil
		raw, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("re-marshal finalize body (tail dropped): %w", err)
		}
	}
	err = s.dispatchPeerAgentSyncPhase2Body(ctx, targetAddr, targetDeviceID, agentID, opID,
		"/api/v1/peers/agent-sync/finalize", raw)
	if err == nil || continuation == nil || continuation.GoalHandoffID != "" ||
		!strings.Contains(err.Error(), "phase-2 HTTP 400") || !strings.Contains(err.Error(), "continuation") {
		return err
	}
	// The target may have restarted or rolled back after the capability probe.
	// Return a distinct downgrade signal so the orchestrator can preserve the
	// source one-shot barrier before it retries the still-pending legacy finalize.
	s.logger.Warn("switch-device: target rejected continuation field; requesting legacy downgrade",
		"agent", agentID, "op_id", opID, "target", targetDeviceID)
	return fmt.Errorf("%w: %v", errFinalizeContinuationDowngrade, err)
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
	return s.dispatchPeerAgentSyncPhase2Body(ctx, targetAddr, targetDeviceID, agentID, opID, path, body)
}

// dispatchPeerAgentSyncPhase2Body POSTs an already-marshalled body to
// target's phase-2 endpoint (finalize or drop). targetDeviceID /
// agentID / opID are unused at the HTTP layer (signing identity
// carries auth, target re-derives the path) but kept for caller-site
// symmetry and future per-request logging.
func (s *Server) dispatchPeerAgentSyncPhase2Body(ctx context.Context, targetAddr, _, _, _, path string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		targetAddr+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build phase-2 request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
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
// noteSwitchFailure leaves a system message with the failure reason
// in the agent's transcript. Every post-quiesce failure exit calls
// this: the quiesce/drain of Step -1 tears the calling turn on the
// self-call path (the backend process dies mid-response), so the
// turn ends with no visible output and this note is the only place
// the outcome surfaces to the user. Harmless on owner-driven
// switches too — the operator gets the reason in the chat instead
// of only in stderr. Best-effort: an append failure is logged and
// never masks the switch result.
func (s *Server) noteSwitchFailure(agentID, reason string) {
	if s.agents == nil {
		return
	}
	if err := s.agents.AppendSystemNote(agentID, agent.NoticeSwitchAbortedPrefix+reason); err != nil {
		s.logger.Warn("switch-device: failed to append failure note to transcript",
			"agent", agentID, "err", err)
	}
}

func (s *Server) orchestrateAbort(ctx context.Context, agentID, targetAddr, targetDeviceID, opID string, resp *switchDeviceResponse, reason string) {
	// Stamp the upstream reason BEFORE abortAfterFailure runs so
	// it survives even when abort itself succeeds (the success
	// branch in abortAfterFailure only sets Outcome; without this
	// a successful abort would erase any record of why we
	// aborted). Latest cause wins by design — callers that
	// already populated resp.Reason fold that detail into the
	// `reason` argument they pass.
	resp.Reason = reason
	// Note FIRST: the reason is already final, and abort + the
	// target drop below can take up to two handoff timeouts when
	// the target is unreachable — the user shouldn't wait on
	// those to learn the switch died.
	s.noteSwitchFailure(agentID, reason)
	s.abortAfterFailure(ctx, agentID, resp, reason)
	// A failed/timed-out sync may still have committed at the target. Send
	// cancellation even without a success response; it can precede phase-1.
	if targetAddr != "" && targetDeviceID != "" && opID != "" {
		dropCtx, cancel := context.WithTimeout(context.Background(), handoffOpTimeout)
		defer cancel()
		if derr := s.dispatchPeerAgentSyncDrop(dropCtx, targetAddr, targetDeviceID, agentID, opID); derr != nil {
			s.logger.Warn("switch-device: agent-sync drop failed (target may require explicit cancellation before another sync)",
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
	// Abort ignores cancellation but retains the source ownership fence so
	// a caller-cancelled ctx (e.g. client hung up) still lets us
	// clear this generation's handoff_pending, never a later operation's.
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), handoffOpTimeout)
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
// /api/v1/peers/pull endpoint and decodes the per-URI result list.
// Authentication is by Tailnet identity: the request travels over
// tsnet and the target's ServeAuthTsnet listener stamps RolePeer
// from the WhoIs-resolved peer_registry row. Network and decode
// errors surface to the caller so the orchestrator can roll back
// via abort.
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

	// agent_workspace_files: ALWAYS full-ship. Per-agent these are
	// tiny singletons (≤ 2 rows: user.md, checkin.md) so the delta
	// has no meaningful perf benefit, but an incremental cursor
	// risks SILENT data loss across peer clock skew (source's
	// updated_at lagging target's cursor would skip a real edit).
	// IncludeDeleted=true keeps tombstones in the payload so a
	// "cleared workspace file" still propagates to target; the
	// DELETE-then-INSERT in syncWorkspaceFilesTx replaces target's
	// state wholesale.
	wf, werr := st.ListAgentWorkspaceFiles(ctx, agentID,
		store.WorkspaceFileListOptions{IncludeDeleted: true})
	if werr != nil && !errors.Is(werr, store.ErrNotFound) {
		return nil, fmt.Errorf("list workspace_files: %w", werr)
	}
	req.WorkspaceFiles = wf

	tasks, terr := st.ListAgentTasks(ctx, agentID, store.AgentTaskListOptions{})
	if terr != nil {
		return nil, fmt.Errorf("list tasks: %w", terr)
	}
	req.Tasks = tasks

	// Credentials: decrypt with source's credentials.key and ship in
	// plaintext (the receiver re-encrypts with its own). credentials.db
	// is per-peer so the encrypted bytes can't cross peer boundaries.
	//
	// Ship the field as a NON-NIL pointer only when source actually
	// has the credential store — that signals "source is authoritative,
	// here is the full set (possibly empty)". A nil pointer (field
	// omitted on the wire) tells target "source is not authoritative,
	// don't touch your rows" so a broken credentials.key / older binary
	// can't silently wipe target's credentials. Mirrors how the
	// best-effort LookupAgentToken below behaves on missing state.
	tool := agentRecordTool(rec)
	if s.agents != nil && s.agents.HasCredentials() {
		creds, cerr := s.agents.Credentials().ExportCredentials(agentID)
		if cerr != nil {
			return nil, fmt.Errorf("export credentials: %w", cerr)
		}
		req.Credentials = &creds
		customKey, kerr := agent.LoadCustomAPIKey(s.agents.Credentials(), agentID, agentRecordCustomBaseURL(rec))
		if kerr != nil {
			return nil, fmt.Errorf("export custom API key: %w", kerr)
		}
		// Non-nil even when empty: once the source credential store is
		// authoritative, the target must clear any stale key retained from an
		// earlier stay. Omitting the field for a keyless non-custom agent could
		// resurrect a revoked key when that agent later selects custom again.
		req.CustomAPIKey = &customKey
	}

	// Transfer only the configured backend's active native sessions. Stale
	// artifacts left by a previous backend are not part of the running
	// agent state and previously accounted for tens of MiB of duplicate
	// history on codex agents.
	if tool == agent.ToolClaude || tool == agent.ToolCustomClaude {
		// Claude's cwd is AgentDir(agentID), NOT Settings.workDir. Read from
		// source's AgentDir; target writes into its own AgentDir.
		files, skipped, ferr := agent.ReadClaudeSessionFiles(agentID)
		if ferr != nil {
			return nil, fmt.Errorf("read claude sessions: %w", ferr)
		}
		if len(skipped) > 0 {
			s.logger.Warn("agent-sync: skipped oversized claude session files",
				"agent", agentID, "files", skipped)
			req.TransferSkips = append(req.TransferSkips, skipped...)
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
	}

	// grok session: independent surface. Gate by source's Tool so
	// a claude/custom agent whose agentDir happens to contain a
	// stale `.grok/session_id` (e.g. from a previous era when the
	// operator briefly switched to grok and back) does NOT
	// accidentally ship grok state target would then resume from.
	// The receiver treats an absent Grok snapshot as authoritative
	// cleanup, so stale target state cannot survive a tool change.
	//
	// CAVEAT (torn-turn read): when the agent triggered the switch
	// via the kojo-switch-device skill, this read happens WHILE
	// the agent's grok turn is still in flight (the LLM is parked
	// on the curl POST that delivered us here). The JSONL
	// surfaces (events.jsonl, chat_history.jsonl, updates.jsonl)
	// tolerate torn writes — a partial trailing line is dropped
	// on parse — and grok writes summary.json / system_prompt.txt
	// atomically (rename-into-place), so a mid-turn read either
	// sees the previous turn's bytes or the new turn's bytes,
	// never a partial. If a required core file is somehow absent
	// (a half-deleted session left behind by a crashed `grok
	// sessions delete --partial`, etc.), ReadGrokSessionFiles
	// returns an error and this optional native artifact is skipped;
	// target starts fresh from canonical Kojo history. The in-flight
	// message itself is captured separately via
	// SnapshotAccumulatedMessageRecord (the same path claude
	// uses), so target's UI transcript stays complete even when
	// the file-level session lags a turn. The transcript is the
	// authoritative carried-over state; ResetSession remains available
	// for operators who want to discard a stale backend-local session.
	if tool == "grok" {
		grokTransfer, grokSkipped, gerr := agent.ReadGrokSessionFiles(agentID)
		if gerr != nil {
			// Native session artifacts are an optimisation: canonical Kojo
			// history remains authoritative and fresh-session bootstrap can
			// recover continuity. A corrupt/oversized Grok directory must not
			// block that non-droppable state from moving.
			s.logger.Warn("agent-sync: grok session unavailable; using history fallback",
				"agent", agentID, "err", gerr)
			req.TransferSkips = append(req.TransferSkips, agent.SkippedSessionFile{
				Path: "grok-session", Reason: "unreadable",
			})
			grokTransfer = nil
		}
		if len(grokSkipped) > 0 {
			s.logger.Warn("agent-sync: skipped oversized grok session files",
				"agent", agentID, "files", grokSkipped)
			req.TransferSkips = append(req.TransferSkips, grokSkipped...)
		}
		if grokTransfer != nil && len(grokTransfer.Files) > 0 {
			gw := &grokSessionWire{
				SessionID: grokTransfer.SessionID,
				Files:     make([]grokSessionFileWire, 0, len(grokTransfer.Files)),
			}
			for _, f := range grokTransfer.Files {
				gw.Files = append(gw.Files, grokSessionFileWire{
					RelPath:    f.RelPath,
					ContentB64: base64.StdEncoding.EncodeToString(f.Content),
				})
			}
			req.GrokSession = gw
		}
	}

	if agentRecordUsesCodex(rec) {
		codexTransfer, codexSkipped, cerr := agent.ReadCodexSessionFiles(agentID)
		if cerr != nil {
			return nil, fmt.Errorf("read codex session: %w", cerr)
		}
		if len(codexSkipped) > 0 {
			s.logger.Warn("agent-sync: skipped codex session files",
				"agent", agentID, "files", codexSkipped)
			req.TransferSkips = append(req.TransferSkips, codexSkipped...)
		}
		if codexTransfer != nil && len(codexTransfer.Threads) > 0 {
			cw := &codexSessionWire{
				Threads: make([]codexThreadWire, 0, len(codexTransfer.Threads)),
			}
			for _, th := range codexTransfer.Threads {
				cw.Threads = append(cw.Threads, codexThreadToWire(th))
			}
			req.CodexSession = cw
		}
	}

	// Raw agent token (best-effort — post-restart peers only
	// have the kv hash, so the callback may return false). An
	// empty AgentToken on the wire tells target's finalize hook
	// to auto-re-issue a fresh token locally, so no manual
	// re-issue step is needed anymore.
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
	client := peer.NoKeepAliveHTTPClient(handoffOpTimeout)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dispatch state: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if agent.HasCodexGoals(agentID) && resp.Header.Get("X-Kojo-Native-Goal") != "v1" {
		return nil, errors.New("target peer does not support native goal transfer; upgrade it before moving this agent")
	}
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

// dispatchPeerAgentSync POSTs the precomputed gzipped wire body to
// target's /api/v1/peers/agent-sync. Authentication is by Tailnet
// identity (ServeAuthTsnet → TailnetIdentityMiddleware → RolePeer
// when WhoIs resolves to a peer_registry row). The body's gzip
// framing fits inside the listener's MaxHeaderBytes / read cap (the
// orchestrator preflighted that); target's handler honours the
// Content-Encoding header to decompress. Network and decode errors
// propagate to the caller so the orchestrator can roll back via
// abort.
func (s *Server) dispatchPeerAgentSync(ctx context.Context, targetAddr, targetDeviceID string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		targetAddr+"/api/v1/peers/agent-sync", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build sync request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")

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

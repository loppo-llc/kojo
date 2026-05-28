package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

// docs/multi-device-storage.md §3.7 — device switch state machine
// orchestration. The owner drives the three transitions:
//
//   begin    → blob_refs.handoff_pending = true for every blob
//              owned by the agent. Both source and target peers
//              refuse new agent-runtime writes against these
//              rows (409) for the duration.
//   complete → blob_refs.home_peer = target AND
//              handoff_pending = false for every blob.
//              agent_locks.holder_peer = target AND
//              fencing_token bumped so the source peer's
//              delayed writes can't slip through. Caller MUST
//              have ensured the target peer pulled every blob
//              (between begin and complete) — the Hub does not
//              verify the pull happened.
//   abort    → blob_refs.handoff_pending = false on every blob,
//              no home_peer / lock changes. Used by the operator
//              when the target peer's pull failed or timed out.
//
// In v1 monolith there's only one peer, so target ≠ source is a
// precondition that fails locally. The endpoint is wired anyway
// because the state machine itself is the testable surface; v2
// peer-to-peer deployments use the same Hub-side orchestration.
//
// Auth: Owner-only.

const handoffOpTimeout = 30 * time.Second

type handoffRequest struct {
	TargetPeerID string `json:"target_peer_id"`
}

type handoffBlobResult struct {
	URI    string `json:"uri"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type handoffResponse struct {
	AgentID string              `json:"agent_id"`
	Op      string              `json:"op"`
	Blobs   []handoffBlobResult `json:"blobs"`
	// Lock holds the per-agent lock state after the transition.
	// Empty on `abort` (no lock change attempted).
	LockHolderPeer  string `json:"lock_holder_peer,omitempty"`
	LockFencing     int64  `json:"lock_fencing_token,omitempty"`
	LockTransferred bool   `json:"lock_transferred,omitempty"`
}

// handleAgentHandoffBegin marks every blob for the agent as
// handoff_pending and returns the per-blob list so the operator
// can drive the target-side pull.
func (s *Server) handleAgentHandoffBegin(w http.ResponseWriter, r *http.Request) {
	s.handleAgentHandoffOp(w, r, "begin")
}

// handleAgentHandoffComplete switches home_peer + transfers the
// lock. Caller MUST have pulled every blob on the target peer
// first; the Hub doesn't verify.
func (s *Server) handleAgentHandoffComplete(w http.ResponseWriter, r *http.Request) {
	s.handleAgentHandoffOp(w, r, "complete")
}

// handleAgentHandoffAbort clears handoff_pending without
// switching home or transferring the lock. Used on pull failure.
func (s *Server) handleAgentHandoffAbort(w http.ResponseWriter, r *http.Request) {
	s.handleAgentHandoffOp(w, r, "abort")
}

// handoffOpError wraps an HTTP status / error code / message
// triple as a regular Go error so runHandoffOp can return a
// single (resp, error) pair (Go-idiomatic) rather than a
// four-value tuple. Callers that need the HTTP shape
// `errors.As(err, &*handoffOpError{})` it; the orchestrator
// that just wants to know "did it work" can errors.Is /
// propagate normally.
type handoffOpError struct {
	Status  int
	Code    string
	Message string
}

func (e *handoffOpError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("handoff %s: %s (http %d)", e.Code, e.Message, e.Status)
}

func newHandoffOpError(status int, code, msg string) *handoffOpError {
	return &handoffOpError{Status: status, Code: code, Message: msg}
}

func (s *Server) handleAgentHandoffOp(w http.ResponseWriter, r *http.Request, op string) {
	if s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"handoff requires agent store")
		return
	}
	if !auth.FromContext(r.Context()).IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden", "owner-only")
		return
	}
	agentID := r.PathValue("id")
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "agent id required")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "read body: "+err.Error())
		return
	}
	var req handoffRequest
	if len(body) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(body)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid json: "+err.Error())
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), handoffOpTimeout)
	defer cancel()

	resp, err := s.runHandoffOp(ctx, agentID, op, req.TargetPeerID)
	if err != nil {
		var hoe *handoffOpError
		if errors.As(err, &hoe) {
			writeError(w, hoe.Status, hoe.Code, hoe.Message)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, resp)
}

// runHandoffOp drives one transition of the §3.7 device-switch
// state machine. It is the side-effecting core extracted out of
// handleAgentHandoffOp so the switch-device orchestrator can
// reuse it without re-parsing an HTTP request / re-checking auth
// (the orchestrator does its own caller authz before driving
// begin → pull → complete).
//
// Returns (response, error). A *handoffOpError carries the
// intended HTTP status + error code + message so HTTP callers
// can `errors.As` it; an internal Go error from the DB layer
// propagates verbatim and the HTTP wrapper maps it to 500.
//
// op ∈ {"begin", "complete", "abort"}. begin/complete require a
// non-empty targetPeerID that lives in peer_registry and is not
// the local peer. abort ignores targetPeerID.
//
// docs §3.7 deviation: the spec lists steps 5 (blob_refs.home_peer
// = target) then 6 (lock transfer); the begin path stays a
// separate INSERT-per-row loop (intentionally per-row so a
// partial-failure surfaces per-URI), but the complete path uses
// store.CompleteHandoff which folds the lock transfer + every
// blob_refs flip into ONE transaction — a crash mid-call rolls
// back to the pre-call state instead of leaving a half-migrated
// agent (the previous TransferAgentLock + per-row SwitchBlobRefHome
// loop split this across multiple statements).
func (s *Server) runHandoffOp(ctx context.Context, agentID, op, targetPeerID string) (*handoffResponse, error) {
	if s.agents == nil || s.agents.Store() == nil {
		return nil, newHandoffOpError(http.StatusServiceUnavailable,
			"unavailable", "handoff requires agent store")
	}
	if agentID == "" {
		return nil, newHandoffOpError(http.StatusBadRequest,
			"bad_request", "agent id required")
	}
	switch op {
	case "begin", "complete":
		if targetPeerID == "" {
			return nil, newHandoffOpError(http.StatusBadRequest,
				"bad_request", "target_peer_id required for begin/complete")
		}
		if s.peerID != nil && targetPeerID == s.peerID.DeviceID {
			return nil, newHandoffOpError(http.StatusBadRequest,
				"bad_request",
				"target_peer_id must not equal the local peer (single-peer cluster cannot device-switch)")
		}
	case "abort":
		// targetPeerID is ignored for abort.
	default:
		return nil, newHandoffOpError(http.StatusBadRequest,
			"bad_request", fmt.Sprintf("unknown handoff op %q", op))
	}

	// For begin/complete we need to know the target peer exists
	// in peer_registry. Refuse a handoff to a peer the cluster
	// has never seen.
	if targetPeerID != "" {
		if _, err := s.agents.Store().GetPeer(ctx, targetPeerID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, newHandoffOpError(http.StatusBadRequest,
					"bad_request",
					"target_peer_id not in peer_registry: "+targetPeerID)
			}
			return nil, fmt.Errorf("peer_registry lookup: %w", err)
		}
	}

	// Find every blob the agent owns. The §2.3 layout puts
	// agent-scoped global blobs under `agents/<id>/*`.
	prefix := "kojo://global/agents/" + agentID + "/"
	refs, err := s.agents.Store().ListBlobRefs(ctx, store.ListBlobRefsOptions{
		URIPrefix: prefix,
	})
	if err != nil {
		return nil, fmt.Errorf("blob_refs list: %w", err)
	}

	resp := &handoffResponse{AgentID: agentID, Op: op, Blobs: make([]handoffBlobResult, 0, len(refs))}

	if op == "complete" {
		// Atomic lock+blob_refs transition. CompleteHandoff
		// returns the lock state, the URIs it just switched,
		// the URIs already at target (idempotent re-call),
		// and any leftover rows whose state didn't converge.
		// The whole thing is one DB transaction — a crash
		// rolls back cleanly.
		out, cerr := s.agents.Store().CompleteHandoff(ctx,
			agentID, targetPeerID, prefix,
			5*60*1000, // 5 min lease
		)
		if cerr != nil {
			if errors.Is(cerr, store.ErrNotFound) {
				// No lock AND no blob rows. Nothing to do —
				// return an empty success rather than erroring;
				// re-invoking complete after a successful run
				// would hit this branch and the operator should
				// see it as "already done".
				return resp, nil
			}
			if errors.Is(cerr, store.ErrFencingMismatch) {
				return nil, newHandoffOpError(http.StatusConflict,
					"lock_transfer_failed",
					"agent_lock transfer to target failed: "+cerr.Error())
			}
			return nil, fmt.Errorf("complete handoff: %w", cerr)
		}
		if out.Lock != nil {
			resp.LockHolderPeer = out.Lock.HolderPeer
			resp.LockFencing = out.Lock.FencingToken
			resp.LockTransferred = out.LockTransferred ||
				out.Lock.HolderPeer == targetPeerID
		}
		// Project per-URI results from CompleteHandoff buckets.
		// Switched + AlreadyAtTarget → status="ok"; Leftover →
		// status="error" so the orchestrator surfaces the row
		// state instead of silently claiming success.
		for _, u := range out.SwitchedURIs {
			resp.Blobs = append(resp.Blobs, handoffBlobResult{URI: u, Status: "ok"})
		}
		for _, u := range out.AlreadyAtTargetURIs {
			resp.Blobs = append(resp.Blobs, handoffBlobResult{URI: u, Status: "ok"})
		}
		for _, u := range out.LeftoverURIs {
			resp.Blobs = append(resp.Blobs, handoffBlobResult{
				URI:    u,
				Status: "error",
				Error:  "row did not converge to target (state mismatch)",
			})
		}
		return resp, nil
	}

	// begin / abort: per-row updates remain non-atomic across
	// the loop because partial failure here is meaningful (the
	// orchestrator's per-URI report lets the operator see WHICH
	// rows failed). A future slice can fold these into a single
	// UPDATE … RETURNING when the partial-error surface is no
	// longer needed.
	for _, ref := range refs {
		res := handoffBlobResult{URI: ref.URI, Status: "ok"}
		switch op {
		case "begin":
			if err := s.agents.Store().SetBlobRefHandoffPending(ctx, ref.URI, true); err != nil {
				res.Status = "error"
				res.Error = err.Error()
			}
		case "abort":
			if err := s.agents.Store().SetBlobRefHandoffPending(ctx, ref.URI, false); err != nil {
				res.Status = "error"
				res.Error = err.Error()
			}
		}
		resp.Blobs = append(resp.Blobs, res)
	}

	return resp, nil
}

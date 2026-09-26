package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

type goalRecoveryRequest struct {
	HandoffID  string `json:"handoffId,omitempty"`
	UserID     string `json:"userId,omitempty"`
	RunID      string `json:"runId,omitempty"`
	AgentID    string `json:"agentId"`
	SessionKey string `json:"sessionKey"`
	ThreadID   string `json:"threadId"`
	Generation int64  `json:"generation"`
	HolderID   string `json:"holderId"`
}

// RecoverNativeGoals is called once after listeners and ownership pruning are
// ready. A lost request is NOT replayed: query/explicit resume remains possible.
func (s *Server) RecoverNativeGoals() { s.recoverNativeGoals("", "", false) }

// Retry detached response surfaces, not arbitrary failed tool executions.
func (s *Server) RunNativeGoalRecovery(ctx context.Context) {
	s.RecoverNativeGoals()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.recoverNativeGoals("", "", true)
			blocked := s.reconcileResolvedGoalHandoffs()
			s.retryStalledGoalHandoffResumes(blocked)
		}
	}
}

// goalHandoffResumeGrace is how long a destination waits after dispatching
// `!goal resume-if` before treating a still-resume_pending handoff as lost.
const goalHandoffResumeGrace = 2 * time.Minute

// retryStalledGoalHandoffResumes re-dispatches the exact accepted identity when
// finalize's asynchronous resume never reached admission (surface dropped the
// command, or the daemon restarted in between). Bounded per handoff; the
// origin stop check is repeated on every attempt so a stop issued after
// finalize still fences the resume.
func (s *Server) retryStalledGoalHandoffResumes(blocked map[pendingSyncKey]struct{}) {
	if s.agents == nil || s.peerID == nil || s.agents.NativeGoalsShuttingDown() {
		return
	}
	self := s.peerID.DeviceID
	for id, bindings := range s.agents.StalledGoalHandoffResumes(self, goalHandoffResumeGrace) {
		for _, b := range bindings {
			if b.Handoff == nil || b.State == nil {
				continue
			}
			if _, unresolved := blocked[pendingSyncKey{AgentID: id, OpID: b.Handoff.ID}]; unresolved {
				continue
			}
			// A temporary source outage must not consume the bounded resume
			// budget: no resume has been dispatched yet. Authorize the exact
			// enumerated handoff first, then atomically claim its next attempt;
			// ClaimGoalHandoffResume rechecks the operation identity and phase
			// so a stale enumeration cannot be dispatched after this check.
			origin := b.OriginPeerID
			if origin == "" {
				origin = b.Handoff.SourcePeerID
			}
			checkCtx, checkCancel := context.WithTimeout(context.Background(), 20*time.Second)
			err := s.callGoalHandoffOrigin(checkCtx, origin, goalHandoffOriginRequest{Action: "check", OpID: b.Handoff.ID, AgentID: id})
			checkCancel()
			if err != nil {
				s.logger.Warn("goal handoff resume authorization unavailable; retry not charged", "agent", id, "sessionKey", b.SessionKey, "handoff", b.Handoff.ID, "err", err)
				continue
			}
			claimed, ok := s.agents.ClaimGoalHandoffResume(id, b.SessionKey, b.Handoff.ID)
			if !ok || claimed.Handoff == nil || claimed.State == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			req := goalRecoveryRequest{AgentID: id, SessionKey: claimed.SessionKey, ThreadID: claimed.State.ThreadID, Generation: claimed.Generation, UserID: claimed.UserID, RunID: claimed.RunID, HolderID: self, HandoffID: claimed.Handoff.ID}
			// Same routing as finalize: main WebUI follows the agent, Slack
			// retains the original Hub.
			if claimed.SessionKey != "" && claimed.OriginPeerID != "" && claimed.OriginPeerID != self {
				err = s.requestGoalRecovery(ctx, claimed.OriginPeerID, req)
			} else {
				err = s.resumeGoalSurface(ctx, req)
			}
			cancel()
			if err != nil {
				s.logger.Warn("goal handoff resume retry not admitted; explicit resume available", "agent", id, "sessionKey", claimed.SessionKey, "handoff", claimed.Handoff.ID, "attempt", claimed.Handoff.ResumeAttempts, "err", err)
			} else {
				s.logger.Info("goal handoff resume re-dispatched", "agent", id, "sessionKey", claimed.SessionKey, "handoff", claimed.Handoff.ID, "attempt", claimed.Handoff.ResumeAttempts)
			}
		}
	}
}

// reconcileResolvedGoalHandoffs retires a pending finalize row only after the
// exact GoalBinding proves that the asynchronous resume reached backend
// admission (resuming/resumed) or reached a terminal no-resume decision. It
// shares finalize's per-agent lock so reconciliation cannot race a retry.
func (s *Server) reconcileResolvedGoalHandoffs() map[pendingSyncKey]struct{} {
	blocked := map[pendingSyncKey]struct{}{}
	if s.agents == nil || s.peerID == nil {
		return blocked
	}
	for id, bindings := range s.agents.ResolvedGoalHandoffs(s.peerID.DeviceID) {
		for _, b := range bindings {
			if b.Handoff == nil {
				continue
			}
			op := b.Handoff.ID
			key := pendingSyncKey{AgentID: id, OpID: op}
			err := s.reconcileResolvedGoalHandoff(id, b)
			if err != nil {
				blocked[key] = struct{}{}
				s.logger.Warn("goal handoff pending finalize reconciliation failed", "agent", id, "handoff", op, "err", err)
			}
		}
	}
	return blocked
}

func (s *Server) reconcileResolvedGoalHandoff(id string, b agent.GoalBinding) error {
	if b.Handoff == nil {
		return nil
	}
	op := b.Handoff.ID
	unlock := s.lockPendingFinalize(pendingSyncKey{AgentID: id})
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entry, ok, err := s.consumePendingAgentSync(ctx, id, op)
	if err != nil || !ok {
		return err
	}
	if !entry.IncomingFenced || entry.SourceDeviceID == "" || entry.SourceDeviceID != b.Handoff.SourcePeerID {
		return errors.New("pending finalize identity does not match resolved goal handoff")
	}
	if !entry.ArrivalHandled || entry.ArrivalUncertain {
		entry.ArrivalUncertain = false
		entry.ArrivalHandled = true
		if err := s.updatePendingAgentSyncAfterSideEffect(ctx, id, op, entry); err != nil {
			return err
		}
	}
	if err := s.agents.Store().FinishIncomingHandoff(ctx, id, op); err != nil {
		return err
	}
	return s.commitPendingAgentSync(ctx, id, op)
}
func (s *Server) recoverNativeGoals(onlyID, excludeKey string, pendingOnly bool) {
	if s.agents == nil || s.agents.NativeGoalsShuttingDown() {
		return
	}
	for id, bindings := range s.agents.RecoverableGoals() {
		if onlyID != "" && id != onlyID {
			continue
		}
		for _, b := range bindings {
			if onlyID != "" && b.SessionKey == excludeKey {
				continue
			}
			if pendingOnly && !b.RecoveryPending {
				continue
			}
			if !s.agents.ClaimGoalRecovery(id, b.SessionKey, b.Generation) {
				continue
			}
			req := goalRecoveryRequest{UserID: b.UserID, RunID: b.RunID, AgentID: id, SessionKey: b.SessionKey, ThreadID: b.State.ThreadID, Generation: b.Generation}
			if s.peerID != nil {
				req.HolderID = s.peerID.DeviceID
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			var err error
			if b.OriginPeerID != "" && b.OriginPeerID != req.HolderID {
				err = s.requestGoalRecovery(ctx, b.OriginPeerID, req)
			} else {
				err = s.resumeGoalSurface(ctx, req)
			}
			cancel()
			if err != nil {
				s.logger.Warn("native goal recovery not admitted; explicit resume available", "agent", id, "sessionKey", b.SessionKey, "err", err)
			}
		}
	}
}
func (s *Server) requestGoalRecovery(ctx context.Context, origin string, req goalRecoveryRequest) error {
	rec, err := s.agents.Store().GetPeer(ctx, origin)
	if err != nil {
		return err
	}
	addr, err := peer.NormalizeAddress(rec.URL)
	if err != nil {
		return err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, addr+"/api/v1/peers/goals/resume", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := peer.NoKeepAliveHTTPClient(20 * time.Second).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("goal recovery HTTP %d", response.StatusCode)
	}
	return nil
}
func (s *Server) handlePeerGoalResume(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if !p.IsOwner() && !p.IsPeer() {
		writeError(w, 403, "forbidden", "peer or owner required")
		return
	}
	var req goalRecoveryRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	// Paired peers reaching the Hub-public listener are stamped RoleOwner with
	// PeerID. Bind either peer-shaped principal to the claimed holder; only a
	// local Owner without a peer identity may act without this comparison.
	if p.PeerID != "" && p.PeerID != req.HolderID {
		writeError(w, 403, "forbidden", "holder identity mismatch")
		return
	}
	if s.externalChat == nil {
		writeError(w, 503, "unavailable", "no external chat router")
		return
	}
	lock, err := s.agents.Store().GetAgentLock(r.Context(), req.AgentID)
	if err != nil || s.peerID == nil || lock.HolderPeer == s.peerID.DeviceID || lock.HolderPeer != req.HolderID {
		writeError(w, 409, "wrong_holder", "goal recovery must come from the current remote holder")
		return
	}
	routeCtx := context.WithValue(r.Context(), externalChatRouteVersionKey{}, externalChatRouteVersion{AgentID: req.AgentID, Version: store.AgentLockVersion{Token: lock.FencingToken, Holder: lock.HolderPeer}})
	s.externalChat.rememberRouteFrom(routeCtx, req.AgentID, lock.HolderPeer)
	if err = s.resumeGoalSurface(r.Context(), req); err != nil {
		writeError(w, 409, "recovery_unavailable", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusAccepted, map[string]bool{"accepted": true})
}
func (s *Server) resumeGoalSurface(ctx context.Context, req goalRecoveryRequest) error {
	if len(req.UserID) > 64 || strings.Trim(req.UserID, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
		return errors.New("invalid recovery user identity")
	}
	if err := s.checkGoalStop(ctx, req.AgentID, req.HandoffID); err != nil {
		return err
	}
	if err := s.checkGoalStop(ctx, req.AgentID, req.RunID); err != nil {
		return err
	}
	command := fmt.Sprintf("!goal resume-if %s %d", req.ThreadID, req.Generation)
	if req.RunID != "" {
		command += " " + req.RunID
	}
	if req.HandoffID != "" {
		if req.RunID == "" {
			command += " -"
		}
		command += " " + req.HandoffID
	}
	q, err := agent.ParseGoalCommand(command)
	if err != nil || q == nil {
		return errors.New("invalid native goal recovery identity")
	}
	if strings.HasPrefix(req.SessionKey, "groupdm:") {
		return s.agents.WakeThread(req.AgentID, req.SessionKey, command)
	}
	if strings.HasPrefix(req.SessionKey, req.AgentID+":slack:") {
		if s.slackHub == nil || !s.slackHub.ResumeGoal(req.AgentID, req.SessionKey+"\n"+req.UserID+"\n"+command) {
			return errors.New("Slack bot unavailable")
		}
		return nil
	}
	if req.SessionKey != "" {
		return errors.New("unsupported goal response surface")
	}
	events, err := s.agents.Chat(context.Background(), req.AgentID, command, "user", nil)
	if err != nil {
		return err
	}
	go func() {
		for range events {
		}
	}()
	return nil
}

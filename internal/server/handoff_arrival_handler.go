package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

const (
	handoffArrivalBodyLimit       = 1 << 20
	maxHandoffArrivalCapabilities = 4096
)

var (
	errHandoffCapabilityInvalid  = errors.New("arrival capability is missing, expired, or bound to another conversation")
	errHandoffCapabilityMismatch = errors.New("arrival capability is not bound to this handoff")
	errHandoffArrivalUncertain   = errors.New("origin conversation arrival delivery is uncertain")
)

type handoffArrivalRequest struct {
	HolderDeviceID string             `json:"holder_device_id"`
	AgentID        string             `json:"agent_id"`
	OpID           string             `json:"op_id"`
	SessionKey     string             `json:"session_key"`
	SourceDeviceID string             `json:"source_device_id"`
	Notes          agent.ArrivalNotes `json:"notes"`
	Capability     string             `json:"capability"`
}

type handoffArrivalCapability struct {
	AgentID, SessionKey string
	ExpiresAt           time.Time
	OpID, HolderID      string
	Accepted, Admitting bool
	TurnDone            bool
	AdmissionDone       chan struct{}
	Reservation         agent.HandoffArrivalReservation
}

type handoffArrivalBindRequest struct {
	SourceDeviceID string `json:"source_device_id"`
	TargetDeviceID string `json:"target_device_id"`
	AgentID        string `json:"agent_id"`
	OpID           string `json:"op_id"`
	SessionKey     string `json:"session_key"`
	Capability     string `json:"capability"`
}

// Adapters use pointer receivers. A nil pointer inside the interface is not an
// executable reservation and must never be advertised as a capability.
// continueThreadAfterModelSwitch queues the next turn into the arrival slot
// held behind the calling turn. Only a capability minted on this peer and
// not bound to a device handoff qualifies; the caller falls back otherwise.
func (s *Server) continueThreadAfterModelSwitch(ctx context.Context, agentID, sessionKey, capability, prompt string) error {
	s.handoffArrivalMu.Lock()
	entry := s.handoffArrivalCaps[capability]
	if entry == nil || entry.AgentID != agentID || entry.SessionKey != sessionKey ||
		entry.OpID != "" || entry.Accepted || entry.Admitting ||
		time.Now().After(entry.ExpiresAt) || missingHandoffArrivalReservation(entry.Reservation) {
		s.handoffArrivalMu.Unlock()
		return errHandoffCapabilityInvalid
	}
	entry.Admitting = true
	entry.AdmissionDone = make(chan struct{})
	reservation := entry.Reservation
	s.handoffArrivalMu.Unlock()

	err := reservation.Activate(ctx, prompt, "")
	s.completeHandoffArrivalAdmission(capability, entry, err == nil)
	return err
}

func missingHandoffArrivalReservation(reservation agent.HandoffArrivalReservation) bool {
	return reservation == nil || (reflect.ValueOf(reservation).Kind() == reflect.Pointer && reflect.ValueOf(reservation).IsNil())
}

func (s *Server) mintHandoffArrivalCapability(agentID, sessionKey string, reservation agent.HandoffArrivalReservation) string {
	if s == nil || agentID == "" || sessionKey == "" || missingHandoffArrivalReservation(reservation) {
		return ""
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	capability := hex.EncodeToString(raw)
	now := time.Now()
	s.handoffArrivalMu.Lock()
	if s.handoffArrivalCaps == nil {
		s.handoffArrivalCaps = make(map[string]*handoffArrivalCapability)
	}
	for key, entry := range s.handoffArrivalCaps {
		// Accepted entries are compact idempotency tombstones. Keep them while
		// the process has room because finalize is durably retryable and can
		// legitimately replay the callback long after the admission window.
		// Never reap an admission while Reservation.Activate is in flight.
		if entry == nil || (!entry.Accepted && !entry.Admitting && now.After(entry.ExpiresAt)) {
			delete(s.handoffArrivalCaps, key)
		}
	}
	if len(s.handoffArrivalCaps) >= maxHandoffArrivalCapabilities {
		// Never evict an accepted tombstone: target finalize may still be
		// pending after its arrival was admitted, and forgetting it could replay
		// the arrival. Degrade new turns to legacy main-WebUI continuation
		// instead. A daemon restart already clears this in-memory ledger.
		s.handoffArrivalMu.Unlock()
		return ""
	}
	s.handoffArrivalCaps[capability] = &handoffArrivalCapability{
		AgentID: agentID, SessionKey: sessionKey, ExpiresAt: now.Add(time.Hour), Reservation: reservation,
	}
	s.handoffArrivalMu.Unlock()
	return capability
}

// finishHandoffArrivalTurn releases the capability when its ordinary source
// turn ends without starting a handoff. If a callback is concurrently being
// admitted, that callback owns the final cleanup decision.
func (s *Server) finishHandoffArrivalTurn(capability string) {
	if s == nil || capability == "" {
		return
	}
	s.handoffArrivalMu.Lock()
	defer s.handoffArrivalMu.Unlock()
	entry := s.handoffArrivalCaps[capability]
	if entry == nil {
		return
	}
	entry.TurnDone = true
	if !entry.Accepted && !entry.Admitting {
		delete(s.handoffArrivalCaps, capability)
	}
}

func (s *Server) completeHandoffArrivalAdmission(capability string, expected *handoffArrivalCapability, accepted bool) {
	s.handoffArrivalMu.Lock()
	defer s.handoffArrivalMu.Unlock()
	entry := s.handoffArrivalCaps[capability]
	if entry != expected {
		return
	}
	entry.Admitting = false
	done := entry.AdmissionDone
	entry.AdmissionDone = nil
	if accepted {
		entry.Accepted = true
		// The adapter owns the reservation lifecycle. Once activation succeeds,
		// retain only the small dedup identity rather than the full history
		// snapshot captured by the reservation.
		entry.Reservation = nil
		entry.ExpiresAt = time.Time{}
	} else if entry.TurnDone {
		delete(s.handoffArrivalCaps, capability)
	}
	if done != nil {
		close(done)
	}
}

func (s *Server) activateHandoffArrivalCapability(ctx context.Context, req handoffArrivalRequest, prompt string) error {
	for {
		s.handoffArrivalMu.Lock()
		entry := s.handoffArrivalCaps[req.Capability]
		if entry == nil || entry.AgentID != req.AgentID || entry.SessionKey != req.SessionKey {
			s.handoffArrivalMu.Unlock()
			return errHandoffCapabilityInvalid
		}
		if entry.OpID != req.OpID || entry.HolderID != req.HolderDeviceID {
			s.handoffArrivalMu.Unlock()
			return errHandoffCapabilityMismatch
		}
		if entry.Accepted {
			s.handoffArrivalMu.Unlock()
			return nil
		}
		if time.Now().After(entry.ExpiresAt) {
			delete(s.handoffArrivalCaps, req.Capability)
			s.handoffArrivalMu.Unlock()
			return errHandoffCapabilityInvalid
		}
		if entry.Admitting {
			done := entry.AdmissionDone
			s.handoffArrivalMu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-done:
				continue
			}
		}
		if missingHandoffArrivalReservation(entry.Reservation) {
			s.handoffArrivalMu.Unlock()
			return errHandoffCapabilityInvalid
		}
		entry.Admitting = true
		entry.AdmissionDone = make(chan struct{})
		reservation := entry.Reservation
		s.handoffArrivalMu.Unlock()

		err := reservation.Activate(ctx, prompt, req.HolderDeviceID)
		s.completeHandoffArrivalAdmission(req.Capability, entry, err == nil)
		return err
	}
}

type handoffArrivalResponse struct {
	Accepted bool `json:"accepted"`
}

// handleHandoffArrivalBind lets the authenticated source bind the opaque Hub
// capability to the exact operation and target before ownership moves. The
// later target callback is therefore a full equality check, not TOFU.
func (s *Server) handleHandoffArrivalBind(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePeerOrOwner(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, handoffArrivalBodyLimit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req handoffArrivalBindRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid arrival bind: "+err.Error())
		return
	}
	if req.SourceDeviceID == "" || req.TargetDeviceID == "" || req.AgentID == "" ||
		req.OpID == "" || req.SessionKey == "" || req.Capability == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "all arrival bind fields are required")
		return
	}
	// On a peer daemon the caller is RolePeer. On the Hub's normal tsnet
	// listener the same paired device is intentionally promoted to RoleOwner,
	// but PeerID is still stamped. Treat either form as a peer identity and
	// require an exact source match. Only unsafe local test mode may use an
	// owner principal without a peer identity.
	if p.PeerID != "" && p.PeerID != req.SourceDeviceID {
		writeError(w, http.StatusForbidden, "forbidden", "signer peer does not match source_device_id")
		return
	}
	if p.PeerID == "" && !(s.unsafePeer && p.IsOwner()) {
		writeError(w, http.StatusForbidden, "forbidden", "arrival bind is peer-only")
		return
	}
	if s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "agent lock store is unavailable")
		return
	}
	lock, err := s.agents.Store().GetAgentLock(r.Context(), req.AgentID)
	if err != nil || lock.HolderPeer != req.SourceDeviceID {
		writeError(w, http.StatusConflict, "source_not_current", "signer is not the current agent holder")
		return
	}
	if err := s.bindHandoffArrivalCapability(req); err != nil {
		writeError(w, http.StatusConflict, "capability_unavailable", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, handoffArrivalResponse{Accepted: true})
}

func (s *Server) bindHandoffArrivalCapability(req handoffArrivalBindRequest) error {
	s.handoffArrivalMu.Lock()
	defer s.handoffArrivalMu.Unlock()
	entry := s.handoffArrivalCaps[req.Capability]
	if entry == nil || time.Now().After(entry.ExpiresAt) || entry.AgentID != req.AgentID || entry.SessionKey != req.SessionKey {
		return errors.New("arrival capability is missing, expired, or bound to another conversation")
	}
	if entry.Accepted {
		return errors.New("arrival capability was already consumed")
	}
	if entry.OpID != "" && (entry.OpID != req.OpID || entry.HolderID != req.TargetDeviceID) {
		return errors.New("arrival capability is bound to another handoff")
	}
	entry.OpID, entry.HolderID = req.OpID, req.TargetDeviceID
	return nil
}

func (s *Server) bindHandoffArrivalAtOrigin(ctx context.Context, originPeerID string, payload handoffArrivalBindRequest) error {
	if s.peerID != nil && originPeerID == s.peerID.DeviceID {
		return s.bindHandoffArrivalCapability(payload)
	}
	if s.agents == nil || s.agents.Store() == nil {
		return errors.New("peer registry is unavailable")
	}
	rec, err := s.agents.Store().GetPeer(ctx, originPeerID)
	if err != nil {
		return fmt.Errorf("resolve origin peer: %w", err)
	}
	addr, err := peer.NormalizeAddress(rec.URL)
	if err != nil {
		return fmt.Errorf("resolve origin address: %w", err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		addr+"/api/v1/peers/handoff/arrival/bind", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := peer.NoKeepAliveHTTPClient(5 * time.Second).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("origin Hub bind returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// handleHandoffArrivalContinuation is the target-holder -> origin-Hub callback
// used after finalize. It launches an internal turn through the existing
// response adapter; no synthetic Slack/user message is posted or re-consumed.
func (s *Server) handleHandoffArrivalContinuation(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePeerOrOwner(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, handoffArrivalBodyLimit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req handoffArrivalRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid arrival continuation: "+err.Error())
		return
	}
	if req.HolderDeviceID == "" || req.AgentID == "" || req.OpID == "" ||
		req.SessionKey == "" || req.SourceDeviceID == "" || req.Capability == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"holder_device_id, agent_id, op_id, session_key, source_device_id, and capability are required")
		return
	}
	// See bind above: paired callers arrive as RolePeer on peer daemons and as
	// RoleOwner+PeerID on the Hub listener. PeerID, not the role label, is the
	// device-authentication fence for this callback.
	if p.PeerID != "" && p.PeerID != req.HolderDeviceID {
		writeError(w, http.StatusForbidden, "forbidden", "signer peer does not match holder_device_id")
		return
	}
	if p.PeerID == "" && !(s.unsafePeer && p.IsOwner()) {
		writeError(w, http.StatusForbidden, "forbidden", "arrival continuation is peer-only")
		return
	}
	// A paired peer is not automatically allowed to inject an arrival turn.
	// It must be the agent's current fenced holder. This rejects stale former
	// holders and unrelated peers before they can reset a native session.
	if s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "agent lock store is unavailable")
		return
	}
	lock, err := s.agents.Store().GetAgentLock(r.Context(), req.AgentID)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusConflict
		}
		writeError(w, status, "holder_not_current", "cannot verify the current agent holder")
		return
	}
	if lock.HolderPeer != req.HolderDeviceID {
		writeError(w, http.StatusConflict, "holder_not_current", "signer is not the current agent holder")
		return
	}
	prompt := s.agents.BuildExternalDeviceSwitchArrivalPrompt(s.peerDisplayName(r.Context(), req.SourceDeviceID), req.Notes)
	if err := s.activateHandoffArrivalCapability(r.Context(), req, prompt); err != nil {
		if errors.Is(err, errHandoffCapabilityInvalid) {
			writeError(w, http.StatusForbidden, "invalid_capability", err.Error())
			return
		}
		if errors.Is(err, errHandoffCapabilityMismatch) {
			writeError(w, http.StatusForbidden, "capability_mismatch", err.Error())
			return
		}
		writeError(w, http.StatusConflict, "conversation_unavailable", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, handoffArrivalResponse{Accepted: true})
}

// dispatchHandoffArrivalContinuation retries only until the origin Hub
// accepts the turn. A dedup key on the Hub makes lost-response retries safe.
// If the adapter remains unavailable, the target falls back to the legacy
// main-WebUI arrival so the agent is never silently stranded.
func (s *Server) dispatchHandoffArrivalContinuation(ctx context.Context, originPeerID string, req handoffArrivalRequest, fallback func()) error {
	const attempts = 3
	const retryBackoff = 500 * time.Millisecond
	var lastErr error
	deliveryUncertain := false
	for attempt := 0; attempt < attempts; attempt++ {
		current, err := s.isCurrentHolder(ctx, req.AgentID, req.HolderDeviceID)
		if err != nil {
			lastErr = fmt.Errorf("verify current holder: %w", err)
		} else if !current {
			s.logger.Warn("device-switch: dropping stale origin-conversation arrival",
				"agent", req.AgentID, "op_id", req.OpID, "holder", req.HolderDeviceID)
			if deliveryUncertain {
				// An earlier attempt may already have reached the Hub; the caller
				// must keep the durable uncertain intent rather than replay.
				return fmt.Errorf("%w; holder is no longer current", errHandoffArrivalUncertain)
			}
			return errors.New("handoff arrival holder is no longer current")
		} else {
			attemptCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			var uncertain bool
			uncertain, lastErr = s.postHandoffArrivalContinuation(attemptCtx, originPeerID, req)
			deliveryUncertain = deliveryUncertain || uncertain
			cancel()
			if lastErr == nil {
				s.logger.Info("device-switch arrival routed to origin conversation",
					"agent", req.AgentID, "op_id", req.OpID, "session_key", req.SessionKey,
					"origin_peer", originPeerID)
				return nil
			}
		}
		if attempt+1 < attempts {
			select {
			case <-ctx.Done():
				if deliveryUncertain {
					return fmt.Errorf("%w; fallback suppressed: %v", errHandoffArrivalUncertain, ctx.Err())
				}
				return ctx.Err()
			case <-time.After(retryBackoff):
			}
		}
	}
	// A transport error after Do began cannot tell us whether the Hub admitted
	// the capability and merely lost the response. Its capability ledger makes
	// retries safe, but launching the legacy fallback here would create a second
	// arrival on a different surface. Prefer a visible degraded error to a
	// duplicate agent turn.
	if deliveryUncertain {
		return fmt.Errorf("%w; fallback suppressed: %v", errHandoffArrivalUncertain, lastErr)
	}
	current, holderErr := s.isCurrentHolder(ctx, req.AgentID, req.HolderDeviceID)
	if holderErr != nil {
		return fmt.Errorf("verify holder before arrival fallback: %w", holderErr)
	}
	if !current {
		return errors.New("handoff arrival holder changed before fallback")
	}
	s.logger.Warn("device-switch origin conversation unavailable; recording main-transcript error instead of an arrival turn",
		"agent", req.AgentID, "op_id", req.OpID, "session_key", req.SessionKey,
		"origin_peer", originPeerID, "err", lastErr)
	if fallback != nil {
		fallback()
		return nil
	}
	return fmt.Errorf("origin conversation unavailable and no fallback: %w", lastErr)
}

func (s *Server) isCurrentHolder(ctx context.Context, agentID, holderID string) (bool, error) {
	if s.agents == nil || s.agents.Store() == nil {
		return false, errors.New("agent lock store is unavailable")
	}
	lock, err := s.agents.Store().GetAgentLock(ctx, agentID)
	if err != nil {
		return false, err
	}
	return lock.HolderPeer == holderID, nil
}

func (s *Server) peerDisplayName(ctx context.Context, peerID string) string {
	if s.agents != nil && s.agents.Store() != nil {
		if rec, err := s.agents.Store().GetPeer(ctx, peerID); err == nil && rec.Name != "" {
			return rec.Name
		}
	}
	return peerID
}

func (s *Server) postHandoffArrivalContinuation(ctx context.Context, originPeerID string, payload handoffArrivalRequest) (bool, error) {
	if s.peerID != nil && originPeerID == s.peerID.DeviceID {
		err := s.activateLocalHandoffCapability(ctx, payload)
		// Missing capability means this process did not admit this attempt.
		// Finalize persists intent before dispatch, so a crash retry cannot
		// replay an admission from a previous process as a fresh fallback.
		return false, err
	}
	if s.agents == nil || s.agents.Store() == nil {
		return false, errors.New("peer registry is unavailable")
	}
	rec, err := s.agents.Store().GetPeer(ctx, originPeerID)
	if err != nil {
		return false, fmt.Errorf("resolve origin peer: %w", err)
	}
	addr, err := peer.NormalizeAddress(rec.URL)
	if err != nil {
		return false, fmt.Errorf("resolve origin address: %w", err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		addr+"/api/v1/peers/handoff/arrival", bytes.NewReader(raw))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	var wroteRequest atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { wroteRequest.Store(true) },
	}))
	resp, err := peer.NoKeepAliveHTTPClient(10 * time.Second).Do(req)
	if err != nil {
		return wroteRequest.Load(), err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		// An explicit rejection did not admit this attempt. Earlier transport
		// uncertainty remains sticky in dispatch; durable finalize intent
		// prevents fallback after a target crash loses that in-memory state.
		return false, fmt.Errorf("origin Hub returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return false, nil
}

func (s *Server) activateLocalHandoffCapability(ctx context.Context, req handoffArrivalRequest) error {
	prompt := s.agents.BuildExternalDeviceSwitchArrivalPrompt(s.peerDisplayName(ctx, req.SourceDeviceID), req.Notes)
	return s.activateHandoffArrivalCapability(ctx, req, prompt)
}

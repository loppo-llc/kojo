package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// Force-reclaim notices (docs/handoff-lock-recovery.md "強制復帰と旧holder").
//
// ForceReclaimAgentToLocal rewrites only the reclaiming device's kojo.db.
// The device that previously ran the agent keeps holder==self (or its
// arrived/<id> marker), so it keeps a second runtime alive — duplicate
// cron / checkins and a diverging transcript — and every later switch
// back to it fails the state probe with wrong_holder, because
// authorizeIncomingSource deliberately never lets a source erase local
// ownership.
//
// The reclaimer therefore persists a notice and tells every other
// registered peer "I force-reclaimed <agent>". A receiver releases only
// if it still claims the agent itself (store.ReleaseReclaimedAgentHere),
// so broadcasting is safe and also covers multi-hop chains where the
// reclaimer's row named an intermediate peer. Offline peers keep the
// notice pending; it is retried periodically, on a peer's offline→online
// transition, and right before a switch to that peer.
//
// Staleness: a notice is only sent while the reclaimer is still the
// holder in its own DB. If the agent has since been switched away, the
// notice is dropped — the new holder legitimately owns the agent, and the
// receiver-side claim check is the second line of defence.

const (
	reclaimNoticeNamespace = "handoff_reclaim_notice"
	reclaimedPeerPath      = "/api/v1/peers/agent-sync/reclaimed"
	reclaimNoticeTTL       = 7 * 24 * time.Hour
	reclaimNoticeInterval  = 1 * time.Minute
	reclaimNoticeTimeout   = 10 * time.Second
)

type reclaimNotice struct {
	// Peers still to be told. Removed once a peer answers 200.
	Peers     []string `json:"peers"`
	CreatedAt int64    `json:"createdAt"`
}

// reclaimNoticeResult is surfaced on the force-reclaim response so the
// operator can see whether the old holder actually let go.
type reclaimNoticeResult struct {
	ReleasedOn   []string `json:"releasedOn"`
	NotHolderOn  []string `json:"notHolderOn"`
	PendingPeers []string `json:"pendingPeers"`
	// SupersededOn lists peers that refused because they force-reclaimed
	// the agent after this device (409 concurrent_reclaim); they keep it.
	SupersededOn []string `json:"supersededOn,omitempty"`
}

// reclaimNoticeOutcome is one peer's answer to a notice.
type reclaimNoticeOutcome int

const (
	reclaimNoticePending    reclaimNoticeOutcome = iota // retry later
	reclaimNoticeReleased                               // old holder let go
	reclaimNoticeNotHolder                              // peer did not run it (or is gone)
	reclaimNoticeSuperseded                             // peer's own later reclaim wins
)

// recordReclaimNotice persists a notice, stamped with reclaimedAt, addressed
// to every registered peer except self. Called by the force-reclaim handler
// BEFORE the local state restore commits (outbox).
func (s *Server) recordReclaimNotice(ctx context.Context, agentID string, reclaimedAt int64) error {
	st := s.agents.Store()
	peers, err := st.ListPeers(ctx, store.ListPeersOptions{})
	if err != nil {
		return fmt.Errorf("list peers: %w", err)
	}
	n := reclaimNotice{CreatedAt: reclaimedAt}

	for _, p := range peers {
		if p == nil || p.DeviceID == "" || p.DeviceID == s.peerID.DeviceID {
			continue
		}
		n.Peers = append(n.Peers, p.DeviceID)
	}
	if len(n.Peers) == 0 {
		return s.deleteReclaimNotice(ctx, agentID)
	}
	return s.putReclaimNotice(ctx, agentID, &n)
}

func (s *Server) putReclaimNotice(ctx context.Context, agentID string, n *reclaimNotice) error {
	raw, err := json.Marshal(n)
	if err != nil {
		return err
	}
	_, err = s.agents.Store().PutKV(ctx, &store.KVRecord{
		Namespace: reclaimNoticeNamespace,
		Key:       agentID,
		Value:     string(raw),
		Type:      store.KVTypeJSON,
		Scope:     store.KVScopeMachine,
	}, store.KVPutOptions{})
	return err
}

func (s *Server) deleteReclaimNotice(ctx context.Context, agentID string) error {
	err := s.agents.Store().DeleteKV(ctx, reclaimNoticeNamespace, agentID, "")
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// deliverReclaimNotice sends the pending notice for agentID. onlyPeer, when
// non-empty, limits delivery to that peer (the switch-device preflight).
// The caller MUST hold the agent's switch-transaction lock so the local
// holder cannot change between the staleness check and the sends.
func (s *Server) deliverReclaimNotice(ctx context.Context, agentID, onlyPeer string) (reclaimNoticeResult, error) {
	var res reclaimNoticeResult
	if s.peerID == nil || s.agents == nil || s.agents.Store() == nil {
		return res, nil
	}
	st := s.agents.Store()
	rec, err := st.GetKV(ctx, reclaimNoticeNamespace, agentID)
	if errors.Is(err, store.ErrNotFound) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	var n reclaimNotice
	if err := json.Unmarshal([]byte(rec.Value), &n); err != nil {
		s.logger.Warn("force-reclaim notice: dropping unreadable notice", "agent", agentID, "err", err)
		return res, s.deleteReclaimNotice(ctx, agentID)
	}
	version, err := st.GetAgentLockVersion(ctx, agentID)
	if err != nil {
		return res, err
	}
	if version.Holder != s.peerID.DeviceID {
		s.logger.Info("force-reclaim notice: agent no longer held here; dropping notice",
			"agent", agentID, "holder", version.Holder)
		return res, s.deleteReclaimNotice(ctx, agentID)
	}
	// The notice is written before the reclaim tx (outbox), so it may
	// belong to a reclaim that never committed. Only the stamp written by
	// a committed reclaim is announced: no stamp means there is nothing
	// to announce, and an older stamp (the latest attempt failed) is the
	// reclaim this device actually performed.
	committedAt, err := s.committedReclaimAt(ctx, agentID)
	if err != nil {
		return res, err
	}
	if committedAt <= 0 {
		s.logger.Info("force-reclaim notice: no committed reclaim; dropping notice", "agent", agentID)
		return res, s.deleteReclaimNotice(ctx, agentID)
	}
	if committedAt != n.CreatedAt {
		n.CreatedAt = committedAt
		if err := s.putReclaimNotice(ctx, agentID, &n); err != nil {
			return res, err
		}
	}
	if store.NowMillis()-n.CreatedAt > reclaimNoticeTTL.Milliseconds() {
		s.logger.Warn("force-reclaim notice: expired with peers never reached",
			"agent", agentID, "pending", n.Peers)
		return res, s.deleteReclaimNotice(ctx, agentID)
	}

	type outcome struct {
		peer   string
		result reclaimNoticeOutcome
	}
	var targets []string
	for _, id := range n.Peers {
		if onlyPeer == "" || id == onlyPeer {
			targets = append(targets, id)
		}
	}
	outcomes := make([]outcome, len(targets))
	var wg sync.WaitGroup
	for i, id := range targets {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			outcomes[i] = outcome{peer: id, result: s.sendReclaimNotice(ctx, agentID, id, n.CreatedAt)}
		}(i, id)
	}
	wg.Wait()

	delivered := map[string]bool{}
	for _, o := range outcomes {
		switch o.result {
		case reclaimNoticeReleased:
			res.ReleasedOn = append(res.ReleasedOn, o.peer)
		case reclaimNoticeNotHolder:
			res.NotHolderOn = append(res.NotHolderOn, o.peer)
		case reclaimNoticeSuperseded:
			res.SupersededOn = append(res.SupersededOn, o.peer)
		default:
			continue
		}
		delivered[o.peer] = true
	}
	remaining := slices.DeleteFunc(slices.Clone(n.Peers), func(id string) bool { return delivered[id] })
	res.PendingPeers = remaining
	if len(remaining) == 0 {
		return res, s.deleteReclaimNotice(ctx, agentID)
	}
	if len(remaining) != len(n.Peers) {
		n.Peers = remaining
		return res, s.putReclaimNotice(ctx, agentID, &n)
	}
	return res, nil
}

// sendReclaimNotice POSTs one notice. Every outcome except
// reclaimNoticePending ends delivery to this peer.
func (s *Server) sendReclaimNotice(ctx context.Context, agentID, deviceID string, reclaimedAt int64) reclaimNoticeOutcome {
	rec, err := s.agents.Store().GetPeer(ctx, deviceID)
	if errors.Is(err, store.ErrNotFound) {
		return reclaimNoticeNotHolder
	}
	if err != nil || rec == nil || rec.Status != store.PeerStatusOnline || rec.URL == "" {
		return reclaimNoticePending
	}
	addr, err := peer.NormalizeAddress(rec.URL)
	if err != nil {
		return reclaimNoticePending
	}
	body, _ := json.Marshal(peerAgentReclaimedRequest{
		ReclaimerDeviceID: s.peerID.DeviceID,
		AgentID:           agentID,
		ReclaimedAt:       reclaimedAt,
	})
	reqCtx, cancel := context.WithTimeout(ctx, reclaimNoticeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, addr+reclaimedPeerPath, bytes.NewReader(body))
	if err != nil {
		return reclaimNoticePending
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := peer.NoKeepAliveHTTPClient(reclaimNoticeTimeout).Do(req)
	if err != nil {
		s.logger.Info("force-reclaim notice: peer unreachable; will retry",
			"agent", agentID, "peer", deviceID, "err", err)
		return reclaimNoticePending
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusConflict && peerErrorCode(raw) == "concurrent_reclaim" {
		// Retrying cannot change the (reclaimed_at, device ID) order.
		s.logger.Warn("force-reclaim notice: peer reclaimed the agent after this device and keeps it",
			"agent", agentID, "peer", deviceID)
		return reclaimNoticeSuperseded
	}
	if resp.StatusCode != http.StatusOK {
		// 403/404 from a peer that predates this endpoint stays pending
		// until it is upgraded (or the notice expires).
		s.logger.Info("force-reclaim notice: peer refused; will retry",
			"agent", agentID, "peer", deviceID, "status", resp.StatusCode,
			"body", strings.TrimSpace(string(raw)))
		return reclaimNoticePending
	}
	var out peerAgentReclaimedResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return reclaimNoticePending
	}
	if !out.Released {
		return reclaimNoticeNotHolder
	}
	s.logger.Warn("force-reclaim notice: old holder released its runtime",
		"agent", agentID, "peer", deviceID)
	return reclaimNoticeReleased
}

// peerErrorCode extracts error.code from a writeError response body.
func peerErrorCode(raw []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return ""
	}
	return e.Error.Code
}

// deliverAllReclaimNotices is one retry pass over every pending notice.
func (s *Server) deliverAllReclaimNotices(ctx context.Context) {
	if s.peerID == nil || s.agents == nil || s.agents.Store() == nil {
		return
	}
	recs, err := s.agents.Store().ListKV(ctx, reclaimNoticeNamespace)
	if err != nil {
		s.logger.Warn("force-reclaim notice: list failed", "err", err)
		return
	}
	for _, rec := range recs {
		if ctx.Err() != nil {
			return
		}
		if rec == nil || !agent.IsPathSafeAgentID(rec.Key) {
			continue
		}
		func() {
			unlock := s.lockPendingFinalize(pendingSyncKey{AgentID: rec.Key, OpID: "switch-transaction"})
			defer unlock()
			if _, err := s.deliverReclaimNotice(ctx, rec.Key, ""); err != nil {
				s.logger.Warn("force-reclaim notice: delivery pass failed", "agent", rec.Key, "err", err)
			}
		}()
	}
}

// StartReclaimNoticeLoop retries pending force-reclaim notices until ctx is
// cancelled. Safe to call more than once.
func (s *Server) StartReclaimNoticeLoop(ctx context.Context) {
	if s == nil {
		return
	}
	kick := s.reclaimNoticeKickCh()
	s.reclaimNoticeOnce.Do(func() {
		go func() {
			t := time.NewTicker(reclaimNoticeInterval)
			defer t.Stop()
			for {
				s.deliverAllReclaimNotices(ctx)
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				case <-kick:
				}
			}
		}()
	})
}

func (s *Server) reclaimNoticeKickCh() chan struct{} {
	s.reclaimNoticeChOnce.Do(func() { s.reclaimNoticeKick = make(chan struct{}, 1) })
	return s.reclaimNoticeKick
}

// kickReclaimNotices schedules an immediate retry pass (e.g. a peer just
// came back online). Buffered; coalesces with a pending kick.
func (s *Server) kickReclaimNotices() {
	if s == nil {
		return
	}
	select {
	case s.reclaimNoticeKickCh() <- struct{}{}:
	default:
	}
}

// localReclaimedAt returns when this device force-reclaimed agentID in
// the CURRENT ownership epoch: the stamp written with the reclaim
// (store.ReclaimedAtNamespace, "<at>:<token>") counts only while this
// device still holds the lock row with that fencing token. Once the agent
// leaves, is re-acquired after a restart, or the stamp is unreadable, it
// returns 0 — erring toward yielding to a competing reclaim.
func (s *Server) localReclaimedAt(ctx context.Context, agentID string) (int64, error) {
	at, token, err := s.reclaimStamp(ctx, agentID)
	if err != nil || at <= 0 {
		return 0, err
	}
	v, err := s.agents.Store().GetAgentLockVersion(ctx, agentID)
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if v.Holder != s.peerID.DeviceID || v.Token != token {
		return 0, nil
	}
	return at, nil
}

// committedReclaimAt returns the time of this device's latest COMMITTED
// force-reclaim of agentID regardless of epoch (0 when none). Unlike
// localReclaimedAt it survives a restart's lock re-acquire, so a pending
// notice keeps being retried; it is used only for delivery, where the
// caller has already checked that this device holds the agent.
func (s *Server) committedReclaimAt(ctx context.Context, agentID string) (int64, error) {
	at, _, err := s.reclaimStamp(ctx, agentID)
	return at, err
}

// reclaimStamp parses store.ReclaimedAtNamespace's "<at>:<token>" (zeros
// when absent or unreadable).
func (s *Server) reclaimStamp(ctx context.Context, agentID string) (at, token int64, err error) {
	rec, err := s.agents.Store().GetKV(ctx, store.ReclaimedAtNamespace, agentID)
	if errors.Is(err, store.ErrNotFound) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	atStr, tokStr, ok := strings.Cut(rec.Value, ":")
	if !ok {
		return 0, 0, nil
	}
	at, err1 := strconv.ParseInt(atStr, 10, 64)
	token, err2 := strconv.ParseInt(tokStr, 10, 64)
	if err1 != nil || err2 != nil || at <= 0 {
		return 0, 0, nil
	}
	return at, token, nil
}

// reclaimSupersedes reports whether reclaim (atA, idA) is ordered after
// (atB, idB). Both peers compare the same two pairs, so exactly one side
// of a concurrent reclaim yields regardless of delivery order.
func reclaimSupersedes(atA int64, idA string, atB int64, idB string) bool {
	if atA != atB {
		return atA > atB
	}
	return idA > idB
}

type peerAgentReclaimedRequest struct {
	ReclaimerDeviceID string `json:"reclaimer_device_id"`
	AgentID           string `json:"agent_id"`
	// ReclaimedAt is the reclaimer's wall-clock millis at reclaim time.
	// Required from peers; orders concurrent reclaims.
	ReclaimedAt int64 `json:"reclaimed_at,omitempty"`
}

type peerAgentReclaimedResponse struct {
	AgentID  string `json:"agentId"`
	Released bool   `json:"released"`
}

// handlePeerAgentReclaimed is the old-holder side: another device
// force-reclaimed agent_id. If this peer still claims the agent, release
// it the way a switch source releases after complete (lock row and
// arrival markers gone, released/<id> written, cron / notify / one-shots
// torn down) and mark the agent so the next switch back here is a full
// snapshot.
//
// Auth: signed peer whose device ID equals reclaimer_device_id, or a local
// owner (manual recovery when the reclaimer can't reach this peer).
func (s *Server) handlePeerAgentReclaimed(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePeerOrOwner(w, r)
	if !ok {
		return
	}
	if s.peerID == nil || s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"peer identity / agent store not wired")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "read body: "+err.Error())
		return
	}
	var req peerAgentReclaimedRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid json: "+err.Error())
		return
	}
	if !agent.IsPathSafeAgentID(req.AgentID) {
		writeError(w, http.StatusBadRequest, "bad_request", "agent_id required")
		return
	}
	isPeer := p.IsPeer() || p.PeerID != ""
	if isPeer {
		if req.ReclaimedAt <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "reclaimed_at required for peer principal")
			return
		}
		if req.ReclaimerDeviceID == "" || !verifySignerIsSource(p, req.ReclaimerDeviceID) {
			writeError(w, http.StatusForbidden, "forbidden",
				"signer peer device_id does not match reclaimer_device_id")
			return
		}
	}
	if req.ReclaimerDeviceID == s.peerID.DeviceID {
		writeError(w, http.StatusBadRequest, "bad_request", "reclaimer is this device")
		return
	}
	// Same lock order as force-reclaim / switch-device so a release can't
	// interleave with an outgoing switch or an incoming finalize here.
	unlockSwitch := s.lockPendingFinalize(pendingSyncKey{AgentID: req.AgentID, OpID: "switch-transaction"})
	defer unlockSwitch()
	unlock := s.lockPendingFinalize(pendingSyncKey{AgentID: req.AgentID})
	defer unlock()

	st := s.agents.Store()
	ctx := r.Context()
	claims, err := st.LocalPeerClaimsAgent(ctx, req.AgentID, s.peerID.DeviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !claims {
		writeJSONResponse(w, http.StatusOK, peerAgentReclaimedResponse{AgentID: req.AgentID})
		return
	}
	// Concurrent reclaims: both devices may hold the agent and notify each
	// other. Releasing on both sides would leave nobody holding it, so the
	// reclaims are totally ordered by (reclaimed_at, device ID) and only an
	// older local reclaim yields. The order is static, so a delayed or
	// retried notice cannot flip the outcome. An owner (manual recovery) is
	// never refused.
	if isPeer {
		ownAt, err := s.localReclaimedAt(ctx, req.AgentID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if ownAt > 0 && reclaimSupersedes(ownAt, s.peerID.DeviceID, req.ReclaimedAt, req.ReclaimerDeviceID) {
			writeError(w, http.StatusConflict, "concurrent_reclaim",
				"this device force-reclaimed the agent after the notifying device")
			return
		}
	}
	released, err := st.ReleaseReclaimedAgentHere(ctx, req.AgentID, s.peerID.DeviceID, req.ReclaimerDeviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if released {
		// Writes released/<id>, stops cron / notify / one-shots and drops
		// the agent from AgentLockGuard.desired. If the guard re-acquired
		// between our commit and this call, RemoveAgent releases that row.
		if s.onAgentReleasedAsSource != nil {
			s.onAgentReleasedAsSource(ctx, req.AgentID)
		}
		s.logger.Warn("force-reclaim notice: released local runtime of an agent reclaimed elsewhere",
			"agent", req.AgentID, "reclaimer", req.ReclaimerDeviceID)
	}
	writeJSONResponse(w, http.StatusOK, peerAgentReclaimedResponse{AgentID: req.AgentID, Released: released})
}

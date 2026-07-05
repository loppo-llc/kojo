package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// Queue-and-forward for the §3.7 device-transfer/handoff subsystem.
//
// When the holder peer of a remote agent is offline (or unreachable
// on dial), a POST /api/v1/agents/{id}/messages is not rejected with
// 502 peer_offline like other mutations — it is persisted into
// handoff_queued_messages (migration 0023) and delivered later, in
// order, when either:
//
//   - the holder peer transitions back to online (peer-events
//     reconnect touch, pairing join), or
//   - holdership moves to the local peer (force-reclaim, handoff
//     complete, switch-device).
//
// Only the message-send route gets this treatment; every other
// mutation keeps failing exactly as before.

// handoffQueueMaxBody caps a single queued message body. Matches the
// 1 MiB JSON-route convention used across agent_handlers.go.
const handoffQueueMaxBody = 1 << 20

// postAgentMessageRequest is the wire shape of POST /agents/{id}/messages.
type postAgentMessageRequest struct {
	Content string `json:"content"`
}

// handlePostAgentMessage is the local (holder-side) HTTP send path:
// inject a user message into the agent's chat loop, exactly like a
// WebSocket "message" frame, and let the response stream into the
// transcript in the background. Reached when the agent is local, or
// when a hub forwarded the request to this peer (RolePeer caller —
// fencing middleware has already confirmed this peer holds the lock).
func (s *Server) handlePostAgentMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing agent id")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, handoffQueueMaxBody)
	var req postAgentMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	if req.Content == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "content required")
		return
	}
	if _, local := s.agents.Get(id); !local {
		writeError(w, http.StatusNotFound, "not_found", "agent not found on this peer")
		return
	}
	if s.agents.IsBusy(id) {
		w.Header().Set("X-Kojo-No-Idempotency-Cache", "1")
		writeError(w, http.StatusConflict, "busy", "agent is busy")
		return
	}
	// Background context: the chat outlives this request, same as
	// the WS path — the transcript records the response even if
	// the caller goes away.
	events, err := s.agents.Chat(context.Background(), id, req.Content, "user", nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chat_failed", err.Error())
		return
	}
	drainEventsAsync(events)
	writeJSONResponse(w, http.StatusAccepted, map[string]any{
		"accepted": true,
		"queued":   false,
	})
}

// proxyOrQueueAgentMessage handles POST /agents/{id}/messages for a
// remote agent. Holder online → forward and stream the response
// back; holder offline OR dial failure → enqueue and answer 202
// queued. Called from remoteAgentProxyMiddleware.
func (s *Server) proxyOrQueueAgentMessage(w http.ResponseWriter, r *http.Request, agentID, holderDeviceID string) {
	st := s.agents.Store()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "store not available")
		return
	}
	// Buffer + validate the body up-front: it is needed both for
	// the forward attempt and for the queue fallback.
	r.Body = http.MaxBytesReader(w, r.Body, handoffQueueMaxBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large",
			"message body exceeds limit: "+err.Error())
		return
	}
	var req postAgentMessageRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	if req.Content == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "content required")
		return
	}

	peerRec, err := st.GetPeer(r.Context(), holderDeviceID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "peer_lookup_failed",
			"cannot resolve holder peer: "+err.Error())
		return
	}

	// Pre-generate the queue id so the synchronous forward and any
	// later drain redelivery share ONE deterministic idempotency
	// key: a forward the holder processed but whose response was
	// lost falls through to enqueue, and the drain's redelivery
	// then replays the holder's cached response instead of
	// injecting the message twice.
	queueID, err := store.NewHandoffQueuedMessageID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "queue_failed", err.Error())
		return
	}

	if peerRec.Status == store.PeerStatusOnline {
		if addr, aerr := peer.NormalizeAddress(peerRec.URL); aerr == nil {
			ok := s.tryForwardQueuedSend(w, r.Context(), addr, agentID, body,
				idempotencyKeyForQueueID(queueID))
			if ok {
				return
			}
			// Dial failed — holder is marked online but
			// unreachable. Fall through to enqueue.
		}
	}

	s.enqueueAgentMessage(w, r.Context(), st, queueID, agentID, peerRec, req.Content)
}

// tryForwardQueuedSend POSTs the buffered body to the holder's
// /messages endpoint. Returns true when a response (any status) was
// relayed to the caller; false when the dial itself failed and the
// caller should queue instead.
func (s *Server) tryForwardQueuedSend(w http.ResponseWriter, ctx context.Context, addr, agentID string, body []byte, idempotencyKey string) bool {
	url := addr + "/api/v1/agents/" + agentID + "/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	// Same key a drain redelivery of the fallback-enqueued row would
	// carry — dedup across "forwarded but response lost, then queued".
	req.Header.Set("Idempotency-Key", idempotencyKey)
	client := peer.NoKeepAliveHTTPClient(30 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		if s.logger != nil {
			s.logger.Debug("queued-send forward: holder dial failed; queueing",
				"agent", agentID, "err", err)
		}
		return false
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return true
}

// enqueueAgentMessage persists one message into the hub-side queue
// and answers 202 with the queued envelope the UI keys off.
func (s *Server) enqueueAgentMessage(w http.ResponseWriter, ctx context.Context, st *store.Store, queueID, agentID string, holder *store.PeerRecord, content string) {
	rec, err := st.EnqueueHandoffQueuedMessageWithID(ctx, queueID, agentID, holder.DeviceID, content)
	if err != nil {
		if errors.Is(err, store.ErrHandoffQueueFull) {
			writeError(w, http.StatusTooManyRequests, "queue_full",
				"message queue for this agent is full; cancel queued messages or wait for the holder device to reconnect")
			return
		}
		writeError(w, http.StatusInternalServerError, "queue_failed", err.Error())
		return
	}
	holderName := holder.Name
	if holderName == "" {
		holderName = holder.DeviceID
	}
	// Kick a drain right away: a holder that is reachable but
	// stale-marked offline (or that came back without a peer event)
	// gets its first delivery attempt now, and the pass's backoff
	// re-arm keeps retrying afterwards — no external trigger needed.
	s.kickHandoffQueueDrain()
	writeJSONResponse(w, http.StatusAccepted, map[string]any{
		"queued":         true,
		"id":             rec.ID,
		"agentId":        agentID,
		"holderPeer":     holder.DeviceID,
		"holderPeerName": holderName,
		"createdAt":      rec.CreatedAt,
		"message":        "queued, will deliver when device " + holderName + " reconnects",
	})
}

// --- Owner endpoints -------------------------------------------------

// handleListQueuedAgentMessages — GET /api/v1/agents/{id}/queued-messages.
// Owner-only: queued content is operator-visible chat input.
func (s *Server) handleListQueuedAgentMessages(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwnerForAgents(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing agent id")
		return
	}
	st := s.agents.Store()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "store not available")
		return
	}
	msgs, err := st.ListHandoffQueuedMessages(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, map[string]any{
			"id":         m.ID,
			"agentId":    m.AgentID,
			"holderPeer": m.HolderPeer,
			"content":    m.Content,
			"createdAt":  m.CreatedAt,
			"status":     m.Status,
		})
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"messages": out})
}

// handleCancelQueuedAgentMessage — DELETE /api/v1/agents/{id}/queued-messages/{qid}.
// Owner-only cancel of a not-yet-delivered queued message.
func (s *Server) handleCancelQueuedAgentMessage(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwnerForAgents(w, r) {
		return
	}
	id := r.PathValue("id")
	qid := r.PathValue("qid")
	if id == "" || qid == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing agent or message id")
		return
	}
	st := s.agents.Store()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "store not available")
		return
	}
	// Deterministic cancel-vs-deliver: the claim-map check and the
	// row DELETE run under the SAME mutex the drain uses to insert
	// its claim, so exactly one of two orders exists — (a) cancel
	// wins: the row is gone before the drain's post-claim existence
	// re-check, delivery is skipped; (b) the drain wins: the id is
	// claimed and the cancel gets 409 delivery_in_progress instead
	// of a hollow success on a message already on the wire. The
	// delete is a single-row statement, cheap enough to hold the
	// mutex across.
	s.handoffDrainMu.Lock()
	_, inFlight := s.handoffDelivering[qid]
	var delErr error
	if !inFlight {
		delErr = st.DeleteHandoffQueuedMessage(r.Context(), id, qid)
	}
	s.handoffDrainMu.Unlock()
	if inFlight {
		w.Header().Set("X-Kojo-No-Idempotency-Cache", "1")
		writeError(w, http.StatusConflict, "delivery_in_progress",
			"queued message is being delivered right now and can no longer be cancelled")
		return
	}
	if delErr != nil {
		if errors.Is(delErr, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "queued message not found (already delivered or cancelled)")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", delErr.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"cancelled": true, "id": qid})
}

// --- Drain -----------------------------------------------------------

// Self-scheduled retry bounds: when a drain pass leaves rows queued
// (holder offline, holder busy, handoff finalize not landed yet, hub
// restarted while the holder was already online), the scheduler
// re-arms itself with exponential backoff instead of waiting for the
// next external trigger — the queue can never sit forever.
const (
	handoffDrainRetryMin = 15 * time.Second
	handoffDrainRetryMax = 5 * time.Minute
)

// kickHandoffQueueDrain schedules an asynchronous drain pass. Cheap
// and idempotent: callers fire it liberally on every trigger (peer
// online transition, pairing join, force-reclaim, handoff complete,
// switch-device, startup). A pass already in flight sets a rerun
// flag instead of stacking goroutines, so triggers that land
// mid-drain are not lost. An external kick resets the retry backoff:
// fresh evidence (a peer just came online) deserves an immediate
// attempt.
func (s *Server) kickHandoffQueueDrain() {
	s.scheduleHandoffQueueDrain(true)
}

func (s *Server) scheduleHandoffQueueDrain(external bool) {
	if s.agents == nil || s.agents.Store() == nil {
		return
	}
	s.handoffDrainMu.Lock()
	if s.handoffDrainStopping {
		s.handoffDrainMu.Unlock()
		return
	}
	if external {
		s.handoffDrainBackoff = 0
		if s.handoffDrainTimer != nil {
			s.handoffDrainTimer.Stop()
			s.handoffDrainTimer = nil
		}
	}
	if s.handoffDrainRunning {
		s.handoffDrainAgain = true
		s.handoffDrainMu.Unlock()
		return
	}
	s.handoffDrainRunning = true
	s.handoffDrainMu.Unlock()

	go func() {
		for {
			// Per-pass cancelable context: Shutdown cancels it so an
			// in-flight holder dial / store read aborts promptly
			// instead of racing the store close.
			passCtx, passCancel := context.WithCancel(context.Background())
			s.handoffDrainMu.Lock()
			if s.handoffDrainStopping {
				// Shutdown landed between passes — don't start a
				// new one with a cancel func Shutdown can't see.
				s.handoffDrainRunning = false
				if s.handoffDrainStopped != nil {
					close(s.handoffDrainStopped)
					s.handoffDrainStopped = nil
				}
				s.handoffDrainMu.Unlock()
				passCancel()
				return
			}
			s.handoffDrainCancel = passCancel
			s.handoffDrainMu.Unlock()
			s.drainHandoffQueueOnce(passCtx)
			passCancel()
			s.handoffDrainMu.Lock()
			s.handoffDrainCancel = nil
			if s.handoffDrainAgain && !s.handoffDrainStopping {
				s.handoffDrainAgain = false
				s.handoffDrainMu.Unlock()
				continue
			}
			s.handoffDrainAgain = false
			s.handoffDrainRunning = false
			if s.handoffDrainStopping {
				if s.handoffDrainStopped != nil {
					close(s.handoffDrainStopped)
					s.handoffDrainStopped = nil
				}
				s.handoffDrainMu.Unlock()
				return
			}
			// Rows still queued → re-arm with backoff so delivery
			// never depends solely on the next external trigger.
			remaining, err := s.agents.Store().ListHandoffQueuedAgentIDs(context.Background())
			if err == nil && len(remaining) > 0 {
				switch {
				case s.handoffDrainBackoff == 0:
					s.handoffDrainBackoff = handoffDrainRetryMin
				case s.handoffDrainBackoff < handoffDrainRetryMax:
					s.handoffDrainBackoff *= 2
					if s.handoffDrainBackoff > handoffDrainRetryMax {
						s.handoffDrainBackoff = handoffDrainRetryMax
					}
				}
				s.handoffDrainTimer = time.AfterFunc(s.handoffDrainBackoff, func() {
					s.scheduleHandoffQueueDrain(false)
				})
			}
			s.handoffDrainMu.Unlock()
			return
		}
	}()
}

// drainHandoffQueueOnce walks every agent with queued messages,
// re-resolves the agent's CURRENT holder from agent_locks, and
// attempts in-order delivery. Per-message failure stops that agent's
// drain (order preserved; retried on the next trigger) but other
// agents still get their pass.
func (s *Server) drainHandoffQueueOnce(ctx context.Context) {
	st := s.agents.Store()
	if st == nil {
		return
	}
	agentIDs, err := st.ListHandoffQueuedAgentIDs(ctx)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("handoff queue drain: list agents failed", "err", err)
		}
		return
	}
	for _, agentID := range agentIDs {
		s.drainHandoffQueueForAgent(ctx, agentID)
	}
}

// drainHandoffQueueForAgent delivers one agent's queue in order.
func (s *Server) drainHandoffQueueForAgent(ctx context.Context, agentID string) {
	st := s.agents.Store()
	msgs, err := st.ListHandoffQueuedMessages(ctx, agentID)
	if err != nil || len(msgs) == 0 {
		return
	}

	// Resolve the CURRENT holder. Missing lock row or empty holder
	// means no one claimed the agent — treat as local (the local
	// runtime will Acquire on first write, same semantic as the
	// fencing middleware's ErrNotFound pass-through; delivery still
	// requires the runtime to actually be attached locally).
	holderPeer, ok := s.currentHolderForDrain(ctx, agentID)
	if !ok {
		return // transient lock read failure — retry next trigger
	}

	local := holderPeer == "" || s.peerID == nil || holderPeer == s.peerID.DeviceID

	if local {
		s.drainQueueLocally(ctx, agentID, msgs)
		return
	}

	// Remote holder: must be online with a dialable address.
	peerRec, err := st.GetPeer(ctx, holderPeer)
	if err != nil || peerRec.Status != store.PeerStatusOnline {
		return
	}
	addr, err := peer.NormalizeAddress(peerRec.URL)
	if err != nil {
		return
	}
	client := peer.NoKeepAliveHTTPClient(30 * time.Second)
	for _, m := range msgs {
		if ctx.Err() != nil {
			return // shutdown / caller cancelled
		}
		// Re-verify holdership immediately before EVERY delivery
		// (including the first — force-reclaim can land between the
		// resolve above and this send) so a device-switch mid-drain
		// stops the pass instead of racing the new holder. The
		// receiving peer's fencing middleware remains the
		// authoritative gate — it 409s when it lost the lock.
		if cur, ok := s.currentHolderForDrain(ctx, agentID); !ok || cur != holderPeer {
			return
		}
		// Claim + row existence re-check: an owner cancel that
		// landed after the list snapshot deletes the row → skip.
		// While claimed, the cancel endpoint 409s instead of
		// "successfully" cancelling a message already on the wire.
		if !s.claimQueuedForDelivery(ctx, agentID, m.ID) {
			continue
		}
		delivered := s.deliverQueuedToPeer(ctx, client, addr, agentID, holderPeer, m)
		s.unclaimQueued(m.ID)
		if !delivered {
			return // keep queued, retry next trigger (order preserved)
		}
	}
}

// deliverQueuedToPeer performs one claimed delivery: POST to the
// holder, then delete the row. Returns false when the message must
// stay queued (dial failure, non-2xx, delete failure).
//
// KNOWN LIMIT (accepted): the idempotency receipt lives on the
// receiving device, so dedup does not survive a holdership move
// between a successful delivery and its redelivery. Hitting that
// requires the conjunction of (a) POST succeeded at holder A,
// (b) the hub crashed / failed the row DELETE before removal —
// otherwise there IS no redelivery, and a plain retry goes back to
// A where the receipt dedupes — AND (c) holdership moved A→B before
// the next drain pass. The blast radius is one visibly duplicated
// user message in the transcript (B's copy was also synced over from
// A), operator-deletable. Replicating idempotency receipts through
// the agent-sync payload to close this would grow the device-switch
// wire format for a crash-window × switch-window coincidence; not
// worth it at this layer.
func (s *Server) deliverQueuedToPeer(ctx context.Context, client *http.Client, addr, agentID, holderPeer string, m *store.HandoffQueuedMessage) bool {
	st := s.agents.Store()
	payload, _ := json.Marshal(postAgentMessageRequest{Content: m.Content})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		addr+"/api/v1/agents/"+agentID+"/messages", bytes.NewReader(payload))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	// Deterministic UUID derived from the queue id: if the row's
	// DELETE fails after a successful delivery, the redelivery on
	// the next pass replays the holder's cached idempotency response
	// instead of injecting the message twice.
	req.Header.Set("Idempotency-Key", idempotencyKeyForQueueID(m.ID))
	resp, err := client.Do(req)
	if err != nil {
		if s.logger != nil {
			s.logger.Debug("handoff queue drain: holder dial failed",
				"agent", agentID, "peer", holderPeer, "err", err)
		}
		return false
	}
	status := resp.StatusCode
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if status < 200 || status >= 300 {
		if s.logger != nil {
			s.logger.Debug("handoff queue drain: holder rejected message",
				"agent", agentID, "peer", holderPeer, "status", status)
		}
		return false // e.g. 409 busy — keep queued, order preserved
	}
	if err := st.DeleteHandoffQueuedMessage(ctx, agentID, m.ID); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		return false
	}
	if s.logger != nil {
		s.logger.Info("handoff queue drain: delivered queued message",
			"agent", agentID, "peer", holderPeer, "queue_id", m.ID)
	}
	return true
}

// idempotencyKeyForQueueID derives a deterministic RFC-4122-shaped
// UUID (name-based, version 5 bits) from the queue row id. The
// holder's idempotency middleware validates the header with
// uuid.Parse, so the raw "hq_<hex>" row id would be rejected with
// 400; the derived UUID is stable per row, which is exactly what
// dedup-on-redelivery needs.
func idempotencyKeyForQueueID(id string) string {
	sum := sha256.Sum256([]byte("kojo-handoff-queue:" + id))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50 // version 5 (name-based)
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// claimQueuedForDelivery marks a queue id as in-flight and then
// re-checks the row still exists (an owner cancel that landed after
// the drain's list snapshot must win). Returns deliver=false when
// the row is gone (cancelled) or the store read failed; the claim is
// released in that case. On deliver=true the caller MUST call
// unclaimQueued when done.
func (s *Server) claimQueuedForDelivery(ctx context.Context, agentID, id string) (deliver bool) {
	s.handoffDrainMu.Lock()
	if s.handoffDelivering == nil {
		s.handoffDelivering = make(map[string]struct{})
	}
	s.handoffDelivering[id] = struct{}{}
	s.handoffDrainMu.Unlock()
	if _, err := s.agents.Store().GetHandoffQueuedMessage(ctx, agentID, id); err != nil {
		s.unclaimQueued(id)
		return false
	}
	return true
}

func (s *Server) unclaimQueued(id string) {
	s.handoffDrainMu.Lock()
	delete(s.handoffDelivering, id)
	s.handoffDrainMu.Unlock()
}

func (s *Server) queuedDeliveryInFlight(id string) bool {
	s.handoffDrainMu.Lock()
	_, ok := s.handoffDelivering[id]
	s.handoffDrainMu.Unlock()
	return ok
}

// currentHolderForDrain reads the agent's current lock holder.
// ok=false only on a transient store read failure; a missing lock
// row returns ("", true) — unclaimed, same semantic as the fencing
// middleware's ErrNotFound pass-through.
func (s *Server) currentHolderForDrain(ctx context.Context, agentID string) (string, bool) {
	lock, err := s.agents.Store().GetAgentLock(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", true
		}
		return "", false
	}
	return lock.HolderPeer, true
}

// drainQueueLocally injects queued messages into the local agent
// runtime, one at a time, waiting for each chat to finish so the
// transcript keeps the enqueue order.
func (s *Server) drainQueueLocally(ctx context.Context, agentID string, msgs []*store.HandoffQueuedMessage) {
	st := s.agents.Store()
	if _, local := s.agents.Get(agentID); !local {
		return // runtime not (yet) attached — retry next trigger
	}
	selfID := ""
	if s.peerID != nil {
		selfID = s.peerID.DeviceID
	}
	for _, m := range msgs {
		if ctx.Err() != nil {
			return // shutdown / caller cancelled
		}
		// Re-verify per message, INCLUDING the first — a
		// force-reclaim / device-switch landing between the caller's
		// resolve and this injection must stop immediately.
		cur, ok := s.currentHolderForDrain(ctx, agentID)
		if !ok || (cur != "" && cur != selfID) {
			return
		}
		if s.agents.IsBusy(agentID) {
			return
		}
		// Claim + row existence re-check (see deliverQueuedToPeer):
		// a cancel that landed after the list snapshot wins; while
		// the chat runs, cancel 409s.
		if !s.claimQueuedForDelivery(ctx, agentID, m.ID) {
			continue
		}
		events, err := s.agents.Chat(context.Background(), agentID, m.Content, "user", nil)
		if err != nil {
			s.unclaimQueued(m.ID)
			if s.logger != nil {
				s.logger.Debug("handoff queue drain: local chat failed",
					"agent", agentID, "err", err)
			}
			return
		}
		// Consume synchronously so the next queued message doesn't
		// hit the busy gate while this one is still streaming.
		// The Chat itself deliberately runs on context.Background
		// (matches the WS path — the transcript should record the
		// response even across disconnects), but the WAIT is
		// shutdown-aware: on ctx cancellation we abort the agent's
		// turn instead of blocking Shutdown behind a long LLM
		// stream. Chat persisted the user message synchronously
		// before returning (appendMessage runs pre-stream), so the
		// row is deleted below either way — the aborted turn looks
		// exactly like a user-pressed abort, not a lost message.
		aborted := false
	consume:
		for {
			select {
			case _, open := <-events:
				if !open {
					break consume
				}
			case <-ctx.Done():
				s.agents.Abort(agentID)
				aborted = true
				// Bounded wait for the chat goroutine to finish:
				// Manager.Shutdown only CANCELS chat contexts (no
				// wait), so returning immediately would let the
				// aborted chat's final transcript writes race the
				// store close that follows the drain-stopped
				// signal. Channel close means the chat goroutine
				// emitted its terminal event (persistence done).
				abortWait := time.NewTimer(5 * time.Second)
			abortDrain:
				for {
					select {
					case _, open := <-events:
						if !open {
							break abortDrain
						}
					case <-abortWait.C:
						if s.logger != nil {
							s.logger.Warn("handoff queue drain: aborted chat did not finish within 5s",
								"agent", agentID, "queue_id", m.ID)
						}
						break abortDrain
					}
				}
				abortWait.Stop()
				break consume
			}
		}
		if aborted {
			// Best-effort row delete with a fresh context (ctx is
			// already cancelled); tolerate failure — worst case the
			// already-persisted user message is re-injected after
			// restart, which the operator can see and delete.
			delCtx, delCancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := st.DeleteHandoffQueuedMessage(delCtx, agentID, m.ID); err != nil &&
				!errors.Is(err, store.ErrNotFound) && s.logger != nil {
				s.logger.Warn("handoff queue drain: shutdown abort; row delete failed",
					"agent", agentID, "queue_id", m.ID, "err", err)
			}
			delCancel()
			s.unclaimQueued(m.ID)
			return
		}
		// The chat has been injected; retry the delete once before
		// giving up so a transient SQLITE_BUSY doesn't leave a
		// delivered message queued (local injection has no
		// idempotency layer to absorb a redelivery).
		derr := st.DeleteHandoffQueuedMessage(ctx, agentID, m.ID)
		if derr != nil && !errors.Is(derr, store.ErrNotFound) {
			derr = st.DeleteHandoffQueuedMessage(ctx, agentID, m.ID)
		}
		s.unclaimQueued(m.ID)
		if derr != nil && !errors.Is(derr, store.ErrNotFound) {
			if s.logger != nil {
				s.logger.Error("handoff queue drain: delivered locally but row delete failed; possible duplicate on next pass",
					"agent", agentID, "queue_id", m.ID, "err", derr)
			}
			return
		}
		if s.logger != nil {
			s.logger.Info("handoff queue drain: delivered queued message locally",
				"agent", agentID, "queue_id", m.ID)
		}
	}
}

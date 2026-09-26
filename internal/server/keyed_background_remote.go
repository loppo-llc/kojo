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
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// Remote keyed background continuation (holder -> Hub pull, the goal-recovery
// shape):
//
//  1. A Hub dispatch with lingerBackgroundTasks binds a remoteKeyedSurface to
//     the holder's keyed claude session. The surface holds its own Hub file
//     relay reference, so relay callbacks stay valid while the session
//     lingers; it is released when the session exits.
//  2. A run_in_background completion opens an unsolicited turn. The surface
//     buffers it under a one-time token and POSTs keyed-background/notify
//     (kind "turn") to the Hub over the authenticated peer transport.
//  3. The Hub fences the caller to the current lock holder, opens its
//     synthetic admission in the surface FIFO (Slack thread / WebUI thread)
//     and dispatches a one-shot to exactly that holder carrying the token.
//  4. The holder's external-chat handler claims the token and streams the
//     buffered turn back as NDJSON (attachment ACKs included).
//
// Abandoned / stop notices use the same notify route (kind "abandoned").
// Failure policy: while the turn waits for the claim, the surface drains it
// into its own capped buffer (bufferKeyedBgEvents), so the session's readLoop
// never stalls on a slow Hub FIFO. The claim wait is bounded by the session's
// linger lifetime (keyedBgAttachMax); every keyedBgAttachWait the holder
// re-notifies with the same token (the Hub keeps one delivery in flight per
// token), so a Hub restart or a lost delivery is requeued rather than lost. A
// turn the Hub rejects or never takes within that bound is dropped: it runs
// to completion unobserved, is logged, and leaves a note for the agent's next
// turn on that key. An abandoned notice is retried within its budget, then
// logged and dropped.

const keyedBgNotifyPath = "/api/v1/peers/keyed-background/notify"

const (
	keyedBgNotifyKindTurn      = "turn"
	keyedBgNotifyKindAbandoned = "abandoned"
	keyedBgPlaceholderMessage  = "[background task notification]"
	maxKeyedBgAttaches         = 256
	maxKeyedBgSeen             = 4096
	maxKeyedBgInflight         = 4096
	keyedBgSeenTTL             = 10 * time.Minute
	// Holder-side buffer for a turn waiting on the Hub's claim. Text deltas
	// coalesce into the queued tail, so only non-text events count against
	// maxKeyedBgBufferedEvents; beyond it they are dropped (terminal and
	// attachment events are always kept; message events are allowed up to
	// twice the cap). maxKeyedBgBufferedBytes bounds the queued text.
	maxKeyedBgBufferedEvents = 2048
	maxKeyedBgBufferedBytes  = 16 << 20
)

// Test seams.
var (
	// keyedBgAttachWait is the re-notify interval while the Hub has not
	// claimed; keyedBgAttachMax bounds the whole wait (the keyed session's
	// max linger: a turn the Hub still has not taken by then is dropped).
	keyedBgAttachWait          = 2 * time.Minute
	keyedBgAttachMax           = 2 * time.Hour
	keyedBgTurnNotifyBudget    = 30 * time.Second
	keyedBgAbandonNotifyBudget = 2 * time.Minute
	keyedBgNotifyBackoff       = 250 * time.Millisecond
	keyedBgNotifyMaxBackoff    = 5 * time.Second
	keyedBgNotifyAttemptLimit  = 10 * time.Second
	// keyedBgHandoffNoticeWait covers the whole abandoned-notice retry
	// budget: after the lock transfers the Hub fences this peer (409), so
	// the switch must not proceed while a notice is still retrying. It only
	// takes this long when the Hub is unreachable (and is still well inside
	// switchDeviceOpTimeout).
	keyedBgHandoffNoticeWait = keyedBgAbandonNotifyBudget + 5*time.Second
)

type keyedBgNotifyRequest struct {
	HolderID   string `json:"holderId"`
	AgentID    string `json:"agentId"`
	SessionKey string `json:"sessionKey"`
	Kind       string `json:"kind"`
	Token      string `json:"token,omitempty"`
	Pending    int    `json:"pending,omitempty"`
	Reason     string `json:"reason,omitempty"`
	NotifyID   string `json:"notifyId"`
}

// keyedBgRegistry: holder-side pending attaches, Hub-side notify dedup.
type keyedBgRegistry struct {
	mu       sync.Mutex
	attaches map[string]*keyedBgAttachEntry
	seen     map[string]time.Time
	// inflight (Hub side) holds the attach tokens with a delivery running,
	// so a holder's periodic re-notify never starts a second one.
	inflight map[string]struct{}
}

type keyedBgAttachEntry struct {
	agentID    string
	sessionKey string
	hubPeerID  string
	events     <-chan agent.ChatEvent
	cancel     func()
	claimed    chan struct{}
	finished   chan struct{}
	finishOnce sync.Once
}

func randomKeyedBgToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func (rr *keyedBgRegistry) register(e *keyedBgAttachEntry) (string, error) {
	token, err := randomKeyedBgToken()
	if err != nil {
		return "", err
	}
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if rr.attaches == nil {
		rr.attaches = make(map[string]*keyedBgAttachEntry)
	}
	if len(rr.attaches) >= maxKeyedBgAttaches {
		return "", errors.New("keyed background attach registry is full")
	}
	rr.attaches[token] = e
	return token, nil
}

// claim removes and returns the entry when it matches the caller exactly.
func (rr *keyedBgRegistry) claim(token, agentID, sessionKey, hubPeerID string) *keyedBgAttachEntry {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	e := rr.attaches[token]
	if e == nil || e.agentID != agentID || e.sessionKey != sessionKey || e.hubPeerID != hubPeerID {
		return nil
	}
	delete(rr.attaches, token)
	close(e.claimed)
	return e
}

// unregister withdraws an unclaimed entry; false means it was claimed.
func (rr *keyedBgRegistry) unregister(token string, e *keyedBgAttachEntry) bool {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if rr.attaches[token] != e {
		return false
	}
	delete(rr.attaches, token)
	return true
}

// firstNotify records notifyID and reports whether it is new (idempotent
// holder retries after a lost 202).
func (rr *keyedBgRegistry) firstNotify(id string) bool {
	now := time.Now()
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if rr.seen == nil {
		rr.seen = make(map[string]time.Time)
	}
	for k, at := range rr.seen {
		if now.Sub(at) > keyedBgSeenTTL {
			delete(rr.seen, k)
		}
	}
	if _, ok := rr.seen[id]; ok {
		return false
	}
	if len(rr.seen) >= maxKeyedBgSeen {
		// Bounded: dedup is best-effort beyond the cap (a duplicate turn
		// notify fails its one-time claim anyway).
		for k := range rr.seen {
			delete(rr.seen, k)
			break
		}
	}
	rr.seen[id] = now
	return true
}

// forgetNotify undoes firstNotify for a notify the Hub did not admit, so the
// holder's retry of the same NotifyID is processed instead of deduped.
func (rr *keyedBgRegistry) forgetNotify(id string) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	delete(rr.seen, id)
}

// beginDelivery (Hub) reserves the one delivery for token. ok=false with
// full=false means one is already running (a re-notify: nothing to do).
func (rr *keyedBgRegistry) beginDelivery(token string) (ok, full bool) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if rr.inflight == nil {
		rr.inflight = make(map[string]struct{})
	}
	if _, running := rr.inflight[token]; running {
		return false, false
	}
	if len(rr.inflight) >= maxKeyedBgInflight {
		return false, true
	}
	rr.inflight[token] = struct{}{}
	return true, false
}

func (rr *keyedBgRegistry) endDelivery(token string) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	delete(rr.inflight, token)
}

// bufferKeyedBgEvents drains in eagerly into a capped queue and replays it on
// the returned channel, so a turn waiting for the Hub's claim never
// backpressures the session. discard drops the queue and drains in to its
// close (the turn then finishes unobserved). The returned channel closes once
// in is closed and the queue is replayed, or right after discard.
func bufferKeyedBgEvents(in <-chan agent.ChatEvent) (<-chan agent.ChatEvent, func()) {
	out := make(chan agent.ChatEvent)
	discardCh := make(chan struct{})
	var discardOnce sync.Once
	discard := func() { discardOnce.Do(func() { close(discardCh) }) }
	go func() {
		defer close(out)
		var queue []agent.ChatEvent
		textBytes := 0
		nonText := 0
		discarding := false
		push := func(e agent.ChatEvent) {
			if e.Type == "text" && e.Message == nil && len(e.Attachments) == 0 {
				if textBytes+len(e.Delta) > maxKeyedBgBufferedBytes {
					return
				}
				textBytes += len(e.Delta)
				if n := len(queue); n > 0 {
					tail := &queue[n-1]
					if tail.Type == "text" && tail.Message == nil && len(tail.Attachments) == 0 && tail.ParentToolUseID == e.ParentToolUseID {
						tail.Delta += e.Delta
						return
					}
				}
				queue = append(queue, e)
				return
			}
			switch e.Type {
			case "done", "error", "attachment":
				// Terminal events and the turn's (single, post-scan)
				// attachment event are always kept.
			case "message":
				// Transcript messages (steered user lines) get headroom
				// beyond the tool-event cap but are still bounded.
				if nonText >= 2*maxKeyedBgBufferedEvents {
					return
				}
			default:
				if nonText >= maxKeyedBgBufferedEvents {
					return
				}
			}
			nonText++
			queue = append(queue, e)
		}
		for in != nil || (len(queue) > 0 && !discarding) {
			var send chan<- agent.ChatEvent
			var head agent.ChatEvent
			if len(queue) > 0 && !discarding {
				send, head = out, queue[0]
			}
			select {
			case e, ok := <-in:
				if !ok {
					in = nil
					continue
				}
				if !discarding {
					push(e)
				}
			case send <- head:
				if head.Type == "text" && head.Message == nil && len(head.Attachments) == 0 {
					textBytes -= len(head.Delta)
				} else {
					nonText--
				}
				queue[0] = agent.ChatEvent{}
				queue = queue[1:]
			case <-discardCh:
				discarding, queue, discardCh = true, nil, nil
			}
		}
	}()
	return out, discard
}

// finish settles a claimed entry once the attach stream ends: without a
// terminal event the Hub is gone, so the turn is aborted.
func (e *keyedBgAttachEntry) finish(terminal bool) {
	e.finishOnce.Do(func() {
		if !terminal && e.cancel != nil {
			e.cancel()
		}
		close(e.finished)
	})
}

// remoteKeyedSurface is the holder-side KeyedSessionSurface bound to a keyed
// session dispatched by a Hub. It is refcounted: the dispatching request and
// the session each hold a reference; the Hub file relay reference is dropped
// when the last one is released (session exit, at most the 2h max linger).
type remoteKeyedSurface struct {
	s          *Server
	agentID    string
	sessionKey string
	hubPeerID  string
	hubAddr    string

	mu      sync.Mutex
	refs    int
	release func()
}

func (s *Server) newRemoteKeyedSurface(agentID, sessionKey, hubPeerID, hubAddr string) *remoteKeyedSurface {
	sf := &remoteKeyedSurface{s: s, agentID: agentID, sessionKey: sessionKey, hubPeerID: hubPeerID, hubAddr: hubAddr}
	sf.Retain()
	return sf
}

func (sf *remoteKeyedSurface) Retain() {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	sf.refs++
	if sf.refs == 1 && sf.release == nil && sf.s.externalChatRelays != nil {
		sf.release = sf.s.externalChatRelays.acquire(sf.agentID, sf.hubPeerID)
	}
}

func (sf *remoteKeyedSurface) Release() {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	if sf.refs <= 0 {
		return
	}
	sf.refs--
	if sf.refs == 0 && sf.release != nil {
		sf.release()
		sf.release = nil
	}
}

func (sf *remoteKeyedSurface) OriginPeerID() string { return sf.hubPeerID }

func (sf *remoteKeyedSurface) holderID() string {
	if sf.s.peerID == nil {
		return ""
	}
	return sf.s.peerID.DeviceID
}

// HandleKeyedSessionTurn buffers the unsolicited turn for the Hub's attach.
// Returning leaves any unconsumed events to the caller's drain (the turn then
// finishes unobserved; it is not aborted).
func (sf *remoteKeyedSurface) HandleKeyedSessionTurn(ctx context.Context, agentID, sessionKey string, events <-chan agent.ChatEvent, cancel func()) {
	buffered, discard := bufferKeyedBgEvents(events)
	// However this returns, the rest of the turn is drained (never replayed).
	defer discard()
	e := &keyedBgAttachEntry{
		agentID: agentID, sessionKey: sessionKey, hubPeerID: sf.hubPeerID,
		events: buffered, cancel: cancel,
		claimed: make(chan struct{}), finished: make(chan struct{}),
	}
	token, err := sf.s.keyedBg.register(e)
	if err != nil {
		sf.undelivered(err)
		return
	}
	deadline := time.Now().Add(keyedBgAttachMax)
	for {
		notifyID, err := randomKeyedBgToken()
		permanent := err != nil
		if err == nil {
			err = sf.notify(ctx, keyedBgNotifyRequest{Kind: keyedBgNotifyKindTurn, Token: token, NotifyID: notifyID}, keyedBgTurnNotifyBudget)
			var perm keyedBgPermanentError
			permanent = errors.As(err, &perm)
		}
		// Only a permanent rejection or a lifecycle cancel gives up early. A
		// transient failure (Hub unreachable past the notify budget) keeps
		// the token registered and retries at the next re-notify interval,
		// so an outage shorter than keyedBgAttachMax does not lose the turn.
		if err != nil && (permanent || ctx.Err() != nil) && sf.s.keyedBg.unregister(token, e) {
			if ctx.Err() == nil {
				sf.undelivered(err)
			}
			return
		}
		if err != nil && !permanent && ctx.Err() == nil {
			sf.s.logger.Warn("keyed background turn notify failed; will re-notify",
				"agent", sf.agentID, "sessionKey", sf.sessionKey, "hub", sf.hubPeerID, "err", err)
		}
		// Notified (or claimed although the 202 was lost): wait for the
		// claim, re-notifying (same token) each keyedBgAttachWait so a lost
		// Hub delivery is requeued, up to the linger lifetime.
		wait := keyedBgAttachWait
		if left := time.Until(deadline); left < wait {
			wait = left
		}
		timer := time.NewTimer(wait)
		select {
		case <-e.claimed:
			timer.Stop()
			<-e.finished
			return
		case <-ctx.Done():
			timer.Stop()
			// Lifecycle cancel (reset / delete / shutdown / handoff quiesce).
			if sf.s.keyedBg.unregister(token, e) {
				return
			}
			<-e.finished
			return
		case <-timer.C:
		}
		if !time.Now().Before(deadline) {
			if sf.s.keyedBg.unregister(token, e) {
				sf.undelivered(errors.New("Hub did not attach within " + keyedBgAttachMax.String()))
				return
			}
			<-e.finished
			return
		}
	}
}

func (sf *remoteKeyedSurface) HandleKeyedBackgroundTurn(agentID, sessionKey string, events <-chan agent.ChatEvent, cancel func()) {
	sf.HandleKeyedSessionTurn(context.Background(), agentID, sessionKey, events, cancel)
}

func (sf *remoteKeyedSurface) undelivered(err error) {
	sf.s.logger.Warn("keyed background turn not delivered to Hub; dropped",
		"agent", sf.agentID, "sessionKey", sf.sessionKey, "hub", sf.hubPeerID, "err", err)
	if sf.s.agents != nil {
		sf.s.agents.SetKeyedBackgroundNote(sf.agentID, sf.sessionKey,
			"[kojo] 前回このスレッドのバックグラウンドタスク完了後のターンは、Hubへ届けられずユーザーに表示されていません。必要なら結果を改めて伝えてください。")
	}
}

// KeyedBackgroundTasksAbandoned reports the abandoned / stop notice to the Hub
// (bounded retry, then logged and dropped). Called off the session's locks.
func (sf *remoteKeyedSurface) KeyedBackgroundTasksAbandoned(agentID, sessionKey string, pending int, reason string) {
	if pending <= 0 {
		return
	}
	notifyID, err := randomKeyedBgToken()
	if err == nil {
		err = sf.notify(context.Background(), keyedBgNotifyRequest{
			Kind: keyedBgNotifyKindAbandoned, Pending: pending, Reason: reason, NotifyID: notifyID,
		}, keyedBgAbandonNotifyBudget)
	}
	if err != nil {
		sf.s.logger.Warn("keyed background abandoned notice not delivered to Hub; dropped",
			"agent", agentID, "sessionKey", sessionKey, "pending", pending, "reason", reason, "err", err)
	}
}

type keyedBgPermanentError struct{ status int }

func (e keyedBgPermanentError) Error() string {
	return fmt.Sprintf("Hub rejected keyed background notify: HTTP %d", e.status)
}

func (sf *remoteKeyedSurface) notify(ctx context.Context, req keyedBgNotifyRequest, budget time.Duration) error {
	req.HolderID = sf.holderID()
	req.AgentID = sf.agentID
	req.SessionKey = sf.sessionKey
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(budget)
	// The budget bounds in-flight attempts too, so keyedBgHandoffNoticeWait
	// strictly covers a whole notify.
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	backoff := keyedBgNotifyBackoff
	for {
		err = sf.postNotify(ctx, body)
		if err == nil {
			return nil
		}
		var perm keyedBgPermanentError
		if errors.As(err, &perm) || ctx.Err() != nil || time.Now().Add(backoff).After(deadline) {
			return err
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		backoff *= 2
		if backoff > keyedBgNotifyMaxBackoff {
			backoff = keyedBgNotifyMaxBackoff
		}
	}
}

func (sf *remoteKeyedSurface) postNotify(ctx context.Context, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, keyedBgNotifyAttemptLimit)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, sf.hubAddr+keyedBgNotifyPath, bytes.NewReader(body))
	if err != nil {
		return keyedBgPermanentError{}
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := peer.NoKeepAliveHTTPClient(keyedBgNotifyAttemptLimit).Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusAccepted:
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests:
		return keyedBgPermanentError{status: resp.StatusCode}
	default:
		return fmt.Errorf("keyed background notify HTTP %d", resp.StatusCode)
	}
}

func validKeyedBgToken(v string) bool {
	if len(v) != 64 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

// handlePeerKeyedBackgroundNotify (Hub) accepts a remote holder's keyed
// background turn / abandoned notice. It answers 202 before any FIFO wait or
// holder I/O; delivery runs in the background.
func (s *Server) handlePeerKeyedBackgroundNotify(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if !p.IsOwner() && !p.IsPeer() {
		writeError(w, http.StatusForbidden, "forbidden", "peer required")
		return
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	dec.DisallowUnknownFields()
	var req keyedBgNotifyRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid keyed background notify: "+err.Error())
		return
	}
	// Bind the claimed holder to the authenticated peer. Only the --unsafe
	// owner escape hatch may act without a peer identity.
	if req.HolderID == "" || (p.PeerID != "" && p.PeerID != req.HolderID) || (p.PeerID == "" && !(s.unsafePeer && p.IsOwner())) {
		writeError(w, http.StatusForbidden, "forbidden", "holder identity mismatch")
		return
	}
	validKey := req.AgentID != "" && (strings.HasPrefix(req.SessionKey, "groupdm:") && len(req.SessionKey) > len("groupdm:") ||
		strings.HasPrefix(req.SessionKey, req.AgentID+":slack:"))
	switch {
	case !validKey || len(req.SessionKey) > 512 || !validKeyedBgToken(req.NotifyID):
		validKey = false
	case req.Kind == keyedBgNotifyKindTurn:
		validKey = validKeyedBgToken(req.Token) && req.Pending == 0 && req.Reason == ""
	case req.Kind == keyedBgNotifyKindAbandoned:
		validKey = req.Token == "" && req.Pending > 0 && len(req.Reason) <= 512
	default:
		validKey = false
	}
	if !validKey {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid keyed background notify")
		return
	}
	if s.externalChat == nil || s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no external chat router")
		return
	}
	// Fence to the current remote holder: a stale holder (lock moved by a
	// handoff) can never post into the conversation.
	lock, err := s.agents.Store().GetAgentLock(r.Context(), req.AgentID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		// Transient: the holder retries 5xx (a 409 would end its retries).
		writeError(w, http.StatusServiceUnavailable, "lock_unavailable", "agent lock lookup failed")
		return
	}
	if err != nil || lock == nil || s.peerID == nil || lock.HolderPeer == s.peerID.DeviceID || lock.HolderPeer != req.HolderID {
		writeError(w, http.StatusConflict, "holder_not_current", "keyed background notify must come from the current remote holder")
		return
	}
	if !s.agents.HasKeyedBackgroundSurface(req.AgentID, req.SessionKey) {
		writeError(w, http.StatusNotFound, "no_surface", "no response surface for this session key")
		return
	}
	if !s.keyedBg.firstNotify(req.NotifyID) {
		writeJSONResponse(w, http.StatusAccepted, map[string]bool{"accepted": true})
		return
	}
	agentID, key, holder := req.AgentID, req.SessionKey, lock.HolderPeer
	switch req.Kind {
	case keyedBgNotifyKindTurn:
		routeCtx := context.WithValue(r.Context(), externalChatRouteVersionKey{}, externalChatRouteVersion{AgentID: agentID, Version: store.AgentLockVersion{Token: lock.FencingToken, Holder: lock.HolderPeer}})
		s.externalChat.rememberRouteFrom(routeCtx, agentID, holder)
		token := req.Token
		ok, full := s.keyedBg.beginDelivery(token)
		if full {
			s.keyedBg.forgetNotify(req.NotifyID)
			writeError(w, http.StatusServiceUnavailable, "busy", "too many keyed background deliveries in flight")
			return
		}
		if !ok {
			// A holder re-notify while the delivery still waits in the
			// surface FIFO: it is already queued.
			break
		}
		go func() {
			defer s.keyedBg.endDelivery(token)
			err := s.agents.DeliverRemoteKeyedBackgroundTurn(agentID, key, func(ctx context.Context, groupID, messageID string) (<-chan agent.ChatEvent, error) {
				return s.externalChat.ChatOneShot(ctx, agentID, keyedBgPlaceholderMessage, agent.OneShotOpts{
					SessionKey:                  key,
					ExpectedHolderPeer:          holder,
					AttachBackgroundToken:       token,
					ResponseAttachmentGroupID:   groupID,
					ResponseAttachmentMessageID: messageID,
				})
			})
			if err != nil {
				s.logger.Warn("remote keyed background turn not delivered", "agent", agentID, "sessionKey", key, "holder", holder, "err", err)
			}
		}()
	case keyedBgNotifyKindAbandoned:
		pending, reason := req.Pending, req.Reason
		go s.agents.DeliverRemoteKeyedTasksAbandoned(agentID, key, pending, reason)
	}
	writeJSONResponse(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

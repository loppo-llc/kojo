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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

const (
	externalChatTextContentType     = "application/x-ndjson"
	externalChatTextBodyLimit       = 4 << 20
	externalChatHeartbeat           = 15 * time.Second
	externalChatAttachmentAckWait   = 2 * time.Minute
	externalChatAttachmentAckTTL    = 5 * time.Minute
	maxExternalChatAttachmentAcks   = 4096
	externalChatAttachmentAckV1     = "v1"
	externalChatAttachmentAckHeader = "X-Kojo-Attachment-Ack"

	defaultExternalChatHandoffWait = 30 * time.Second
	defaultExternalChatPoll        = 250 * time.Millisecond
	defaultExternalChatProbe       = 2 * time.Second
	maxConcurrentLocalSteers       = 8
	localSteerAdmissionWait        = 5 * time.Second
)

var fallbackLocalSteerSem = make(chan struct{}, maxConcurrentLocalSteers)

// externalChatRouter keeps Slack transport ownership on the Hub while
// dispatching only the agent turn to the machine that currently owns the
// runtime. Routing hints are memory-only: correctness comes from the holder's
// existing agent_lock fence, and a stale hint is repaired by probing peers.
type externalChatRouter struct {
	server *Server

	mu     sync.RWMutex
	routes map[string]externalChatRouteHint // agent ID -> holder within a local ownership generation

	handoffWait   time.Duration
	pollInterval  time.Duration
	probeTimeout  time.Duration
	localSteerSem chan struct{}
}

type externalChatTextRequest struct {
	GoalUserID                  string                    `json:"goalUserId,omitempty"`
	GoalRunID                   string                    `json:"goalRunId,omitempty"`
	Goal                        *agent.GoalRequest        `json:"goal,omitempty"`
	InteractiveQuestions        bool                      `json:"-"`
	Message                     string                    `json:"message"`
	SessionKey                  string                    `json:"sessionKey,omitempty"`
	FreshSessionContext         string                    `json:"freshSessionContext,omitempty"`
	ResumeSessionContext        string                    `json:"resumeSessionContext,omitempty"`
	SystemPromptExtra           string                    `json:"systemPromptExtra,omitempty"`
	DisableAttachments          bool                      `json:"disableAttachments,omitempty"`
	Attachments                 []agent.MessageAttachment `json:"attachments,omitempty"`
	HubMCPBaseURL               string                    `json:"hubMcpBaseUrl,omitempty"`
	ForceFreshSession           bool                      `json:"forceFreshSession,omitempty"`
	ResponseAttachmentGroupID   string                    `json:"responseAttachmentGroupId,omitempty"`
	ResponseAttachmentMessageID string                    `json:"responseAttachmentMessageId,omitempty"`
	HandoffCapability           string                    `json:"-"`
	PreserveTerminalOnCancel    bool                      `json:"-"`
	// LingerBackgroundTasks keeps the holder's keyed claude process alive
	// while run_in_background tasks are pending. It is relayed only to a
	// holder advertising keyedBackgroundV1, and a holder honors it only from
	// the agent's allowed Hub proxy (externalChatHubAddress), binding a
	// Hub-bound keyed surface so completions come back to this Hub.
	LingerBackgroundTasks bool `json:"lingerBackgroundTasks,omitempty"`
	// AttachBackgroundToken (Hub -> holder) attaches this request's stream to
	// the holder's buffered keyed background turn instead of starting a turn.
	AttachBackgroundToken string `json:"attachBackgroundToken,omitempty"`
}

type externalChatSteerRequest struct {
	GoalUserID string                `json:"goalUserId,omitempty"`
	SessionKey string                `json:"sessionKey"`
	Content    string                `json:"content,omitempty"`
	Question   *agent.QuestionAnswer `json:"question,omitempty"`
	// StopBackground relays a thread's `!stop all`: stop every background
	// task of the keyed session instead of steering text into a turn.
	StopBackground bool `json:"stopBackground,omitempty"`
}

type externalChatTextEnvelope struct {
	Kind               string           `json:"kind"`
	Event              *agent.ChatEvent `json:"event,omitempty"`
	AttachmentAckToken string           `json:"attachmentAckToken,omitempty"`
}

type externalChatAttachmentAckRequest struct {
	Token string `json:"token"`
}

type externalChatAttachmentAckRegistry struct {
	mu      sync.Mutex
	entries map[string]*externalChatAttachmentAckEntry
}

type externalChatAttachmentAckEntry struct {
	agentID   string
	peerID    string
	done      chan struct{}
	acked     bool
	finished  bool
	expiresAt time.Time
}

func (rr *externalChatAttachmentAckRegistry) register(agentID, peerID string) (string, *externalChatAttachmentAckEntry, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	token := hex.EncodeToString(raw)
	now := time.Now()
	entry := &externalChatAttachmentAckEntry{
		agentID: agentID, peerID: peerID, done: make(chan struct{}),
		expiresAt: now.Add(externalChatAttachmentAckTTL),
	}
	rr.mu.Lock()
	if rr.entries == nil {
		rr.entries = make(map[string]*externalChatAttachmentAckEntry)
	}
	for key, existing := range rr.entries {
		if existing == nil || now.After(existing.expiresAt) {
			delete(rr.entries, key)
		}
	}
	if len(rr.entries) >= maxExternalChatAttachmentAcks {
		rr.mu.Unlock()
		return "", nil, errors.New("attachment acknowledgement registry is full")
	}
	rr.entries[token] = entry
	rr.mu.Unlock()
	return token, entry, nil
}

func (rr *externalChatAttachmentAckRegistry) acknowledge(token, agentID, peerID string) bool {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	entry := rr.entries[token]
	if entry == nil || entry.agentID != agentID || entry.peerID != peerID {
		return false
	}
	if entry.finished {
		return entry.acked
	}
	if !entry.acked {
		entry.acked = true
		close(entry.done)
	}
	return true
}

// finish resolves the holder-side timeout/ACK race under the same lock as the
// HTTP acknowledgement. A successful entry becomes a short-lived tombstone so
// an ACK whose 200 response was lost remains idempotently retryable.
func (rr *externalChatAttachmentAckRegistry) finish(token string, expected *externalChatAttachmentAckEntry) bool {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	entry := rr.entries[token]
	if entry == nil || entry != expected {
		return false
	}
	entry.finished = true
	if !entry.acked {
		delete(rr.entries, token)
		return false
	}
	entry.expiresAt = time.Now().Add(externalChatAttachmentAckTTL)
	return true
}

type externalChatReadyResponse struct {
	Ready                bool   `json:"ready"`
	Switching            bool   `json:"switching,omitempty"`
	HolderPeer           string `json:"holderPeer,omitempty"`
	Unavailable          string `json:"unavailable,omitempty"`
	OriginAwareArrivalV1 bool   `json:"originAwareArrivalV1,omitempty"`
	// KeyedBackgroundV1: the holder accepts lingerBackgroundTasks /
	// attachBackgroundToken and reports keyed background turns to the Hub.
	KeyedBackgroundV1 bool `json:"keyedBackgroundV1,omitempty"`
}

type externalChatDispatchState int

const (
	externalChatDispatchDone externalChatDispatchState = iota
	externalChatDispatchStale
	externalChatDispatchSwitching
)

type externalChatDispatchResult struct {
	events     <-chan agent.ChatEvent
	state      externalChatDispatchState
	nextHolder string
	err        error
}

func newExternalChatRouter(s *Server) *externalChatRouter {
	return &externalChatRouter{
		server:        s,
		routes:        make(map[string]externalChatRouteHint),
		handoffWait:   defaultExternalChatHandoffWait,
		pollInterval:  defaultExternalChatPoll,
		probeTimeout:  defaultExternalChatProbe,
		localSteerSem: make(chan struct{}, maxConcurrentLocalSteers),
	}
}

func (r *externalChatRouter) routeHint(agentID string) string {
	v, err := r.currentRouteVersion(context.Background(), agentID)
	if err != nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	hint := r.routes[agentID]
	if hint.Version != v {
		return ""
	}
	return hint.Holder
}

// rememberRoute is for synchronous authoritative hints. Asynchronous routes
// must use rememberRouteFrom with the generation captured BEFORE their I/O.
func (r *externalChatRouter) rememberRoute(agentID, holder string) {
	ctx, err := r.withRouteVersion(context.Background(), agentID)
	if err == nil {
		r.rememberRouteFrom(ctx, agentID, holder)
	}
}

// ChatOneShot implements slackbot.ChatManager. A remote transport request is
// never replayed after its POST may have reached a holder. Only explicit
// pre-admission responses (switching / wrong holder / runtime not ready) are
// safe to route again.
func (r *externalChatRouter) ChatOneShot(ctx context.Context, agentID, message string, opts agent.OneShotOpts) (events <-chan agent.ChatEvent, err error) {
	if r == nil || r.server == nil || r.server.agents == nil {
		return nil, errors.New("external chat router is unavailable")
	}
	if opts.Goal != nil && (opts.Goal.Action == "pause" || opts.Goal.Action == "clear") {
		if handled, stopErr := r.StopIdleGoal(ctx, agentID, opts.SessionKey, opts.GoalUserID); handled && stopErr != nil {
			r.server.logger.Warn("handoff stop not confirmed at origin; continuing normal authorized goal control", "agent", agentID, "err", stopErr)
		}
	}
	ctx, err = r.withRouteVersion(ctx, agentID)
	if err != nil {
		return nil, err
	}
	freshContext, resumeContext := opts.FreshSessionContext, opts.ResumeSessionContext
	if freshContext == "" && resumeContext == "" {
		freshContext, resumeContext = agent.FormatOneShotHistoryContexts(opts.History, opts.HistorySelfUserID)
	}
	req := externalChatTextRequest{
		Goal:                        opts.Goal,
		GoalUserID:                  opts.GoalUserID,
		Message:                     message,
		InteractiveQuestions:        opts.InteractiveQuestions,
		SessionKey:                  opts.SessionKey,
		FreshSessionContext:         freshContext,
		ResumeSessionContext:        resumeContext,
		SystemPromptExtra:           opts.SystemPromptExtra,
		DisableAttachments:          opts.DisableKojoAttachmentInstructions,
		Attachments:                 append([]agent.MessageAttachment(nil), opts.Attachments...),
		ForceFreshSession:           opts.ForceFreshSession,
		ResponseAttachmentGroupID:   opts.ResponseAttachmentGroupID,
		ResponseAttachmentMessageID: opts.ResponseAttachmentMessageID,
		PreserveTerminalOnCancel:    opts.PreserveTerminalOnCancel,
		LingerBackgroundTasks:       opts.LingerBackgroundTasks,
		AttachBackgroundToken:       opts.AttachBackgroundToken,
	}
	if opts.SessionKey != "" && opts.HandoffArrivalReservation != nil {
		req.HandoffCapability = r.server.mintHandoffArrivalCapability(agentID, opts.SessionKey, opts.HandoffArrivalReservation)
	}
	defer func() {
		if req.HandoffCapability == "" {
			return
		}
		if events == nil {
			r.server.finishHandoffArrivalTurn(req.HandoffCapability)
			return
		}
		events = r.trackHandoffCapabilityStream(ctx, req.HandoffCapability, events, req.PreserveTerminalOnCancel)
	}()

	// A handoff arrival is fenced to the holder that finalized that exact
	// operation. Do not consult the ordinary in-memory route hint here: it can
	// still name the source while the target arrival is waiting behind the
	// initiating turn's FIFO reservation. Likewise, never discover/replay this
	// synthetic turn on a newer holder; that newer handoff owns its own arrival.
	if opts.ExpectedHolderPeer != "" {
		holder := opts.ExpectedHolderPeer
		result := r.dispatch(ctx, ctx, agentID, holder, holder == r.selfPeerID(), req)
		if result.events != nil {
			return result.events, result.err
		}
		if result.err != nil {
			return nil, fmt.Errorf("stale handoff arrival for holder %q: %w", holder, result.err)
		}
		return nil, fmt.Errorf("stale handoff arrival: holder %q did not admit the turn", holder)
	}

	holder, local, err := r.initialRoute(ctx, agentID)
	if err != nil {
		return nil, err
	}
	result := r.dispatch(ctx, ctx, agentID, holder, local, req)
	if result.events != nil || (result.err != nil && result.state == externalChatDispatchDone) {
		return result.events, result.err
	}

	if result.nextHolder != "" {
		r.rememberRouteFrom(ctx, agentID, result.nextHolder)
		result = r.dispatch(ctx, ctx, agentID, result.nextHolder,
			result.nextHolder == r.selfPeerID(), req)
		if result.events != nil || (result.err != nil && result.state == externalChatDispatchDone) {
			return result.events, result.err
		}
	}

	// A wrong-holder response is not necessarily a handoff: an in-memory hint
	// may simply be stale after a Hub restart. Bound both the initial all-peer
	// discovery and any subsequent handoff wait by one deadline.
	wait := r.handoffWait
	if wait <= 0 {
		wait = defaultExternalChatHandoffWait
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	waitCtx, err = r.refreshRouteVersion(waitCtx, agentID)
	if err != nil {
		return nil, err
	}
	found, switching, discoverErr := r.discoverReadyHolder(waitCtx, agentID)
	if discoverErr == nil && found != "" {
		result = r.dispatch(waitCtx, ctx, agentID, found, found == r.selfPeerID(), req)
		if result.events != nil || (result.err != nil && result.state == externalChatDispatchDone) {
			cancel()
			return result.events, result.err
		}
		// Discovery proved a holder was ready, but it moved or entered a
		// switch before admission. No turn started; keep it in the bounded
		// wait rather than surfacing a transient boundary race.
		switching = true
	}
	if result.state != externalChatDispatchSwitching && !switching {
		if result.err != nil {
			return nil, result.err
		}
		if discoverErr != nil {
			return nil, discoverErr
		}
		return nil, errors.New("agent holder is unavailable")
	}

	poll := r.pollInterval
	if poll <= 0 {
		poll = defaultExternalChatPoll
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("agent device switch did not finish within %s", wait)
			}
			return nil, waitCtx.Err()
		case <-ticker.C:
			waitCtx, err = r.refreshRouteVersion(waitCtx, agentID)
			if err != nil {
				return nil, err
			}
			found, _, err := r.discoverReadyHolder(waitCtx, agentID)
			if err != nil || found == "" {
				continue
			}
			result := r.dispatch(waitCtx, ctx, agentID, found, found == r.selfPeerID(), req)
			if result.events == nil && !(result.err != nil && result.state == externalChatDispatchDone) {
				// All non-done states are pre-admission and therefore safe to
				// retry on the next poll.
				continue
			}
			// The bounded context governs only the in-memory handoff wait.
			// The accepted turn must retain the caller's full lifetime; returning
			// a stream bound to waitCtx would cancel it as this function returns.
			cancel()
			return result.events, result.err
		}
	}
}

// trackHandoffCapabilityStream ties the capability's potentially large
// reservation snapshot to the source turn lifetime. Successful handoff
// admission compacts it to a small idempotency tombstone; an ordinary turn
// removes it when its event stream ends.
func (r *externalChatRouter) trackHandoffCapabilityStream(ctx context.Context, capability string, in <-chan agent.ChatEvent, preserveTerminalOnCancel bool) <-chan agent.ChatEvent {
	out := make(chan agent.ChatEvent, 64)
	go func() {
		defer close(out)
		defer r.server.finishHandoffArrivalTurn(capability)
		for {
			select {
			case event, ok := <-in:
				if !ok {
					return
				}
				select {
				case out <- event:
				case <-ctx.Done():
					forwardTrackedTerminalAfterCancel(in, out, event, preserveTerminalOnCancel)
					return
				}
			case <-ctx.Done():
				forwardTrackedTerminalAfterCancel(in, out, agent.ChatEvent{}, preserveTerminalOnCancel)
				return
			}
		}
	}()
	return out
}

func forwardTrackedTerminalAfterCancel(in <-chan agent.ChatEvent, out chan<- agent.ChatEvent, current agent.ChatEvent, preserve bool) {
	if !preserve {
		return
	}
	for {
		if current.Type == "done" || current.Type == "error" {
			out <- current
			return
		}
		if current.Type == "attachment" && current.BeginAttachmentOwnership() {
			current.FinishAttachmentOwnership(false)
		}
		event, ok := <-in
		if !ok {
			return
		}
		current = event
	}
}

// SteerOneShot follows a WebUI thread turn to its current holder and injects
// text into that exact response-surface session. Unlike a normal turn, steer
// is not replayable after POST: the holder may have accepted the text before
// a transport error becomes visible to the Hub.
func (r *externalChatRouter) SteerOneShot(ctx context.Context, agentID, sessionKey, content string) error {
	return r.sendOneShotInput(ctx, agentID, externalChatSteerRequest{SessionKey: sessionKey, Content: content})
}

func (r *externalChatRouter) SteerOneShotAsUser(ctx context.Context, agentID, sessionKey, content, userID string) error {
	return r.sendOneShotInput(ctx, agentID, externalChatSteerRequest{SessionKey: sessionKey, Content: content, GoalUserID: userID})
}

// StopThreadBackgroundTasks follows the thread's holder like a steer and stops
// every background task of its keyed session (Slack `!stop all`). It returns
// agent.ErrBackgroundSessionNotFound when the thread has none.
func (r *externalChatRouter) StopThreadBackgroundTasks(ctx context.Context, agentID, sessionKey string) error {
	return r.sendOneShotInput(ctx, agentID, externalChatSteerRequest{SessionKey: sessionKey, StopBackground: true})
}

func (r *externalChatRouter) AnswerOneShotQuestion(ctx context.Context, agentID, sessionKey string, answer agent.QuestionAnswer) error {
	return r.sendOneShotInput(ctx, agentID, externalChatSteerRequest{SessionKey: sessionKey, Question: &answer})
}

// sendOneShotInput shares the holder fence and no-replay transport policy for
// steering and question answers. Questions are never converted to steering.
func (r *externalChatRouter) sendOneShotInput(ctx context.Context, agentID string, input externalChatSteerRequest) error {
	if r == nil || r.server == nil || r.server.agents == nil {
		return errors.New("external chat router is unavailable")
	}
	var versionErr error
	ctx, versionErr = r.withRouteVersion(ctx, agentID)
	if versionErr != nil {
		return versionErr
	}
	holder, local, err := r.initialRoute(ctx, agentID)
	if err != nil {
		return err
	}
	// Readiness is also a pre-admission holder fence. A cached route may still
	// name the source immediately after handoff; follow the holder advertised by
	// that source before issuing any POST. Once a POST is attempted we never
	// replay the steer, because the target may have accepted it before a response
	// was lost.
	for redirects := 0; redirects < 4; redirects++ {
		if local || holder == "" {
			ready := r.server.externalChatReadiness(ctx, agentID)
			if ready.HolderPeer != "" && ready.HolderPeer != r.selfPeerID() {
				r.forgetRouteFrom(ctx, agentID, holder)
				holder = ready.HolderPeer
				local = false
				continue
			}
			if !ready.Ready {
				return fmt.Errorf("agent thread holder is unavailable: %s", ready.Unavailable)
			}
			return steerOneShotWithContext(ctx, r.localSteerSem, localSteerAdmissionWait, func() error {
				if input.StopBackground {
					return r.server.agents.StopThreadBackgroundTasks(agentID, input.SessionKey)
				}
				if input.Question != nil {
					q := input.Question
					return r.server.agents.AnswerOneShotQuestion(agentID, input.SessionKey, r.selfPeerID(), q.RequestID, q.Answers, q.Deny, q.DenyMessage)
				}
				return r.server.agents.SteerOneShotAsUser(agentID, input.SessionKey, "", input.Content, input.GoalUserID)
			})
		}

		ready, err := r.probeHolder(ctx, agentID, holder)
		if err != nil {
			return fmt.Errorf("probe thread holder before steer: %w", err)
		}
		if ready.HolderPeer != "" && ready.HolderPeer != holder {
			r.forgetRouteFrom(ctx, agentID, holder)
			holder = ready.HolderPeer
			local = holder == r.selfPeerID()
			continue
		}
		if !ready.Ready {
			r.forgetRouteFrom(ctx, agentID, holder)
			return fmt.Errorf("thread holder %s is not ready: %s", holder, ready.Unavailable)
		}
		if err := r.checkRouteVersion(ctx, agentID); err != nil {
			return err
		}
		resp, attempted, err := r.postRemoteSteer(ctx, agentID, holder, input)
		if err != nil {
			if attempted {
				return fmt.Errorf("%w: %v", agent.ErrSteerDeliveryUncertain, err)
			}
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			r.rememberRouteFrom(ctx, agentID, holder)
			return nil
		}
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body)
		msg := strings.TrimSpace(body.Error.Message)
		if msg == "" {
			msg = resp.Status
		}
		switch body.Error.Code {
		case "goal_owner_forbidden":
			return agent.ErrGoalOwnerForbidden
		case "question_not_found":
			return agent.ErrQuestionNotFound
		case "invalid_answers":
			return agent.ErrInvalidQuestionAnswer
		case "not_busy":
			return fmt.Errorf("%w: %s", agent.ErrAgentNotBusy, msg)
		case "background_session_not_found":
			return fmt.Errorf("%w: %s", agent.ErrBackgroundSessionNotFound, msg)
		case "unsupported":
			return fmt.Errorf("%w: %s", agent.ErrSteerUnsupported, msg)
		case "delivery_uncertain":
			return fmt.Errorf("%w: %s", agent.ErrSteerDeliveryUncertain, msg)
		case "wrong_holder", "runtime_not_ready", "switching":
			r.forgetRouteFrom(ctx, agentID, holder)
			return fmt.Errorf("thread holder changed before steer admission: %s", msg)
		default:
			return fmt.Errorf("holder rejected thread steer (%s): %s", resp.Status, msg)
		}
	}
	return errors.New("thread holder changed too many times before steer admission")
}

// Manager's holder-local SteerFunc predates context-aware transports. Run it
// behind a buffered result so Slack stop/timeout can release its ordered
// admission even if a backend write wedges. Once the call has started,
// cancellation is necessarily delivery-uncertain: the backend may accept the
// input after the caller stops waiting, so the transport must not retry it as a
// normal turn.
func steerOneShotWithContext(ctx context.Context, sem chan struct{}, admissionWait time.Duration, steer func() error) error {
	if sem == nil {
		sem = fallbackLocalSteerSem
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	admissionCtx, cancelAdmission := context.WithTimeout(ctx, admissionWait)
	defer cancelAdmission()
	select {
	case sem <- struct{}{}:
	case <-admissionCtx.Done():
		// No backend call started, so the caller may safely fall back to an
		// ordinary turn rather than treating delivery as uncertain.
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("local steer admission timed out before backend call")
	}
	if err := admissionCtx.Err(); err != nil {
		<-sem
		if parentErr := ctx.Err(); parentErr != nil {
			return parentErr
		}
		return errors.New("local steer admission timed out before backend call")
	}
	done := make(chan error, 1)
	go func() {
		err := steer()
		<-sem
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("%w: local steer did not finish before cancellation: %v", agent.ErrSteerDeliveryUncertain, ctx.Err())
	}
}

func (r *externalChatRouter) initialRoute(ctx context.Context, agentID string) (holder string, local bool, err error) {
	s := r.server
	self := r.selfPeerID()
	if self == "" || s.agents.Store() == nil {
		return self, true, nil
	}
	if hint := r.routeHint(agentID); hint != "" {
		return hint, hint == self, nil
	}
	lock, lockErr := s.agents.Store().GetAgentLock(ctx, agentID)
	if lockErr == nil && lock != nil && lock.HolderPeer != "" {
		return lock.HolderPeer, lock.HolderPeer == self, nil
	}
	if lockErr != nil && !errors.Is(lockErr, store.ErrNotFound) {
		return "", false, fmt.Errorf("read agent holder: %w", lockErr)
	}
	if _, ok := s.agents.Get(agentID); ok {
		return self, true, nil
	}
	return "", false, fmt.Errorf("%w: %s", agent.ErrAgentNotFound, agentID)
}

func (r *externalChatRouter) selfPeerID() string {
	if r == nil || r.server == nil || r.server.peerID == nil {
		return ""
	}
	return r.server.peerID.DeviceID
}

func (r *externalChatRouter) dispatch(routeCtx, turnCtx context.Context, agentID, holder string, local bool, req externalChatTextRequest) externalChatDispatchResult {
	s := r.server
	var versionErr error
	routeCtx, versionErr = r.withRouteVersion(routeCtx, agentID)
	if versionErr != nil {
		return externalChatVersionFailure(versionErr)
	}
	if versionErr = r.checkRouteVersion(routeCtx, agentID); versionErr != nil {
		return externalChatVersionFailure(versionErr)
	}
	if req.Goal != nil && req.Goal.ExpectedHandoffID != "" {
		if err := s.checkGoalStop(routeCtx, agentID, req.Goal.ExpectedHandoffID); err != nil {
			return externalChatDispatchResult{state: externalChatDispatchDone, err: err}
		}
	}
	if req.Goal != nil && req.Goal.ExpectedRunID != "" {
		if err := s.checkGoalStop(routeCtx, agentID, req.Goal.ExpectedRunID); err != nil {
			return externalChatDispatchResult{state: externalChatDispatchDone, err: err}
		}
	}
	if local || holder == "" {
		if req.AttachBackgroundToken != "" {
			// Attach targets a remote holder's buffered turn; a local keyed
			// session delivers its background turns directly.
			return externalChatDispatchResult{state: externalChatDispatchDone,
				err: errors.New("keyed background attach requires a remote holder")}
		}
		ready := s.externalChatReadiness(routeCtx, agentID)
		if !ready.Ready {
			state := externalChatDispatchStale
			if ready.Switching || ready.HolderPeer == "" || ready.HolderPeer == r.selfPeerID() {
				// A lock can move to this host just before finalize activates the
				// runtime. Treat that short gap like a handoff, not a terminal 404.
				state = externalChatDispatchSwitching
			}
			return externalChatDispatchResult{state: state, nextHolder: ready.HolderPeer,
				err: errors.New(ready.Unavailable)}
		}
		events, err := s.agents.ChatOneShot(turnCtx, agentID, req.Message, agent.OneShotOpts{
			Goal:                              req.Goal,
			GoalUserID:                        req.GoalUserID,
			GoalRunID:                         req.GoalRunID,
			SessionKey:                        req.SessionKey,
			InteractiveQuestions:              req.InteractiveQuestions,
			FreshSessionContext:               req.FreshSessionContext,
			ResumeSessionContext:              req.ResumeSessionContext,
			SystemPromptExtra:                 req.SystemPromptExtra,
			DisableKojoAttachmentInstructions: req.DisableAttachments,
			Attachments:                       req.Attachments,
			OriginPeerID:                      r.selfPeerID(),
			ForceFreshSession:                 req.ForceFreshSession,
			HandoffCapability:                 req.HandoffCapability,
			ResponseAttachmentGroupID:         req.ResponseAttachmentGroupID,
			ResponseAttachmentMessageID:       req.ResponseAttachmentMessageID,
			PreserveTerminalOnCancel:          req.PreserveTerminalOnCancel,
			LingerBackgroundTasks:             req.LingerBackgroundTasks,
		})
		if err != nil {
			if errors.Is(err, agent.ErrAgentBusy) && s.agents.IsSwitching(agentID) {
				return externalChatDispatchResult{state: externalChatDispatchSwitching}
			}
			return externalChatDispatchResult{state: externalChatDispatchDone, err: err}
		}
		r.rememberRouteFrom(routeCtx, agentID, r.selfPeerID())
		return externalChatDispatchResult{events: events, state: externalChatDispatchDone}
	}

	ready, probeErr := r.probeHolder(routeCtx, agentID, holder)
	if probeErr != nil {
		r.forgetRouteFrom(routeCtx, agentID, holder)
		return externalChatDispatchResult{state: externalChatDispatchStale, err: probeErr}
	}
	if !ready.Ready {
		r.forgetRouteFrom(routeCtx, agentID, holder)
		state := externalChatDispatchStale
		if ready.Switching || ready.HolderPeer == holder {
			// The lock can reach the target just before finalize activates
			// its runtime. That is a handoff gap, not a stale terminal route.
			state = externalChatDispatchSwitching
		}
		return externalChatDispatchResult{state: state, nextHolder: ready.HolderPeer,
			err: fmt.Errorf("holder %s is not ready: %s", holder, ready.Unavailable)}
	}
	if !ready.KeyedBackgroundV1 {
		// An old holder rejects unknown fields; keep its close-at-result
		// behaviour instead of failing the turn.
		req.LingerBackgroundTasks = false
		if req.AttachBackgroundToken != "" {
			return externalChatDispatchResult{state: externalChatDispatchDone,
				err: fmt.Errorf("holder %s does not support keyed background attach", holder)}
		}
	}

	if req.Goal != nil || req.ForceFreshSession || (req.GoalUserID != "" && strings.HasPrefix(req.SessionKey, agentID+":slack:")) {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return externalChatDispatchResult{state: externalChatDispatchDone, err: err}
		}
		req.GoalRunID = hex.EncodeToString(raw)
	}
	stopGoal := func() {
		if req.GoalRunID != "" && !s.agents.NativeGoalsShuttingDown() {
			if err := s.recordGoalStop(agentID, req.GoalRunID); err != nil {
				s.logger.Error("persist native goal stop failed", "agent", agentID, "err", err)
			}
			if err := r.fenceRemoteGoalRun(agentID, holder, req.SessionKey, req.GoalRunID); err != nil {
				s.logger.Warn("native goal stop not acknowledged; origin recovery remains fenced", "agent", agentID, "err", err)
			}
		}
	}
	if err := r.checkRouteVersion(routeCtx, agentID); err != nil {
		return externalChatVersionFailure(err)
	}
	resp, attempted, err := r.postRemote(turnCtx, agentID, holder, req)
	if err != nil {
		if turnCtx.Err() != nil {
			stopGoal()
		}
		// The POST may have reached ChatOneShot before the connection failed.
		// Never retry this turn on another holder.
		state := externalChatDispatchStale
		if attempted {
			state = externalChatDispatchDone
		}
		return externalChatDispatchResult{state: state, err: err}
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body)
		msg := strings.TrimSpace(body.Error.Message)
		if msg == "" {
			msg = resp.Status
		}
		switch body.Error.Code {
		case "switching", "runtime_not_ready":
			r.forgetRouteFrom(routeCtx, agentID, holder)
			return externalChatDispatchResult{state: externalChatDispatchSwitching,
				nextHolder: resp.Header.Get("X-Kojo-Holder-Peer"), err: errors.New(msg)}
		case "wrong_holder":
			r.forgetRouteFrom(routeCtx, agentID, holder)
			return externalChatDispatchResult{state: externalChatDispatchStale,
				nextHolder: resp.Header.Get("X-Kojo-Holder-Peer"), err: errors.New(msg)}
		default:
			return externalChatDispatchResult{state: externalChatDispatchDone,
				err: fmt.Errorf("holder rejected Slack turn (%s): %s", resp.Status, msg)}
		}
	}

	r.rememberRouteFrom(routeCtx, agentID, holder)
	out := make(chan agent.ChatEvent, 64)
	go r.decodeExternalChatTextStream(turnCtx, agentID, holder, resp.Body, out, stopGoal)
	return externalChatDispatchResult{events: out, state: externalChatDispatchDone}
}

func (r *externalChatRouter) postRemote(ctx context.Context, agentID, holder string, payload externalChatTextRequest) (*http.Response, bool, error) {
	rec, err := r.server.agents.Store().GetPeer(ctx, holder)
	if err != nil {
		return nil, false, fmt.Errorf("resolve holder peer: %w", err)
	}
	if rec.Status != store.PeerStatusOnline {
		return nil, false, fmt.Errorf("holder peer is %s", rec.Status)
	}
	addr, err := peer.NormalizeAddress(rec.URL)
	if err != nil {
		return nil, false, fmt.Errorf("resolve holder address: %w", err)
	}
	remotePayload := payload
	remotePayload.Attachments = make([]agent.MessageAttachment, 0, len(payload.Attachments))
	for _, attachment := range payload.Attachments {
		materialized, err := uploadAttachmentToPeer(ctx, addr, attachment)
		if err != nil {
			return nil, false, err
		}
		remotePayload.Attachments = append(remotePayload.Attachments, materialized)
	}
	if r.server.peerID != nil {
		if hub, getErr := r.server.agents.Store().GetPeer(ctx, r.server.peerID.DeviceID); getErr == nil {
			remotePayload.HubMCPBaseURL, _ = peer.NormalizeAddress(hub.URL)
		}
	}
	body, err := json.Marshal(remotePayload)
	if err != nil {
		return nil, false, fmt.Errorf("encode Slack turn: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		addr+"/api/v1/agents/"+agentID+"/external-chat", bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if payload.InteractiveQuestions {
		req.Header.Set("X-Kojo-Interactive-Questions", "v1")
	}
	req.Header.Set(externalChatAttachmentAckHeader, externalChatAttachmentAckV1)
	if payload.HandoffCapability != "" {
		req.Header.Set("X-Kojo-Handoff-Capability", payload.HandoffCapability)
	}
	var wroteRequest atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { wroteRequest.Store(true) },
	}))
	resp, err := peer.NoKeepAliveHTTPClient(0).Do(req)
	if err != nil {
		return nil, wroteRequest.Load(), fmt.Errorf("dispatch Slack turn to holder: %w", err)
	}
	return resp, true, nil
}

func (r *externalChatRouter) postRemoteSteer(ctx context.Context, agentID, holder string, payload externalChatSteerRequest) (*http.Response, bool, error) {
	rec, err := r.server.agents.Store().GetPeer(ctx, holder)
	if err != nil {
		return nil, false, fmt.Errorf("resolve thread holder peer: %w", err)
	}
	if rec.Status != store.PeerStatusOnline {
		return nil, false, fmt.Errorf("thread holder peer is %s", rec.Status)
	}
	addr, err := peer.NormalizeAddress(rec.URL)
	if err != nil {
		return nil, false, fmt.Errorf("resolve thread holder address: %w", err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, false, fmt.Errorf("encode thread steer: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		addr+"/api/v1/agents/"+agentID+"/external-chat/steer", bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	var wroteRequest atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { wroteRequest.Store(true) },
	}))
	resp, err := peer.NoKeepAliveHTTPClient(0).Do(req)
	if err != nil {
		return nil, wroteRequest.Load(), fmt.Errorf("dispatch thread steer to holder: %w", err)
	}
	return resp, true, nil
}

func (r *externalChatRouter) probeHolder(ctx context.Context, agentID, holder string) (externalChatReadyResponse, error) {
	var result externalChatReadyResponse
	rec, err := r.server.agents.Store().GetPeer(ctx, holder)
	if err != nil {
		return result, fmt.Errorf("resolve holder peer: %w", err)
	}
	if rec.Status != store.PeerStatusOnline {
		return result, fmt.Errorf("holder peer is %s", rec.Status)
	}
	addr, err := peer.NormalizeAddress(rec.URL)
	if err != nil {
		return result, err
	}
	timeout := r.probeTimeout
	if timeout <= 0 {
		timeout = defaultExternalChatProbe
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet,
		addr+"/api/v1/agents/"+agentID+"/external-chat/ready", nil)
	if err != nil {
		return result, err
	}
	resp, err := peer.NoKeepAliveHTTPClient(timeout).Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("holder readiness probe returned %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result); err != nil {
		return result, err
	}
	return result, nil
}

func (r *externalChatRouter) discoverReadyHolder(ctx context.Context, agentID string) (holder string, switching bool, err error) {
	ctx, err = r.withRouteVersion(ctx, agentID)
	if err != nil {
		return "", false, err
	}
	if err = r.checkRouteVersion(ctx, agentID); err != nil {
		return "", false, err
	}
	if ready := r.localReadiness(ctx, agentID); ready.Ready {
		r.rememberRouteFrom(ctx, agentID, r.selfPeerID())
		return r.selfPeerID(), false, nil
	} else if ready.Switching {
		switching = true
	}
	peers, err := r.server.agents.Store().ListPeers(ctx, store.ListPeersOptions{Status: store.PeerStatusOnline})
	if err != nil {
		return "", switching, fmt.Errorf("list peers for holder discovery: %w", err)
	}
	self := r.selfPeerID()
	for _, rec := range peers {
		if rec == nil || rec.DeviceID == "" || rec.DeviceID == self {
			continue
		}
		ready, probeErr := r.probeHolder(ctx, agentID, rec.DeviceID)
		if probeErr != nil {
			continue
		}
		if ready.Ready {
			r.rememberRouteFrom(ctx, agentID, rec.DeviceID)
			return rec.DeviceID, false, nil
		}
		if ready.Switching || ready.HolderPeer == rec.DeviceID {
			// The target may already own the lock while finalize has not
			// activated its runtime yet. Keep waiting instead of treating that
			// normal handoff boundary as a terminal unavailable holder.
			switching = true
		}
	}
	return "", switching, nil
}

func (r *externalChatRouter) localReadiness(ctx context.Context, agentID string) externalChatReadyResponse {
	return r.server.externalChatReadiness(ctx, agentID)
}

func (s *Server) externalChatReadiness(ctx context.Context, agentID string) externalChatReadyResponse {
	result := externalChatReadyResponse{}
	if s == nil || s.agents == nil {
		result.Unavailable = "agent manager unavailable"
		return result
	}
	if s.peerID != nil && s.agents.Store() != nil {
		lock, err := s.agents.Store().GetAgentLock(ctx, agentID)
		if err == nil && lock != nil {
			result.HolderPeer = lock.HolderPeer
			if lock.HolderPeer != "" && lock.HolderPeer != s.peerID.DeviceID {
				result.Unavailable = "agent lock is held by another peer"
				return result
			}
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			result.Unavailable = "agent lock lookup failed"
			return result
		}
	}
	if s.agents.Store() != nil {
		incomplete, err := s.agents.Store().IsIncomingHandoffIncomplete(ctx, agentID)
		if err != nil {
			result.Unavailable = "incoming handoff fence lookup failed"
			return result
		}
		if incomplete {
			result.Switching = true
			result.Unavailable = "incoming handoff is not finalized"
			return result
		}
	}
	if s.agents.IsSwitching(agentID) {
		result.Switching = true
		result.Unavailable = "device switch in progress"
		return result
	}
	a, ok := s.agents.Get(agentID)
	if !ok {
		result.Unavailable = "agent runtime is not active on this peer"
		return result
	}
	if a.Archived {
		result.Unavailable = "agent is archived"
		return result
	}
	result.Ready = true
	if result.HolderPeer == "" && s.peerID != nil {
		result.HolderPeer = s.peerID.DeviceID
	}
	return result
}

func (s *Server) handleExternalChatReady(w http.ResponseWriter, r *http.Request) {
	// Hub authentication stamps paired devices as Owner+PeerID. Permit
	// those identities for this read-only route probe, not generic chat POSTs.
	p := auth.FromContext(r.Context())
	if !p.IsPeer() && !(p.IsOwner() && (p.PeerID != "" || s.unsafePeer)) {
		writeError(w, http.StatusForbidden, "forbidden", "external chat readiness is peer-only")
		return
	}
	ready := s.externalChatReadiness(r.Context(), r.PathValue("id"))
	// This doubles as pre-handoff capability negotiation. Old targets either
	// lack this route or omit the field, so a new source can downgrade to the
	// legacy main-WebUI arrival before it transfers the lock.
	ready.OriginAwareArrivalV1 = true
	ready.KeyedBackgroundV1 = true
	writeJSONResponse(w, http.StatusOK, ready)
}

func (s *Server) handleExternalChatText(w http.ResponseWriter, r *http.Request) {
	if !s.externalChatPeerAllowed(w, r) {
		return
	}
	agentID := r.PathValue("id")
	ready := s.externalChatReadiness(r.Context(), agentID)
	if !ready.Ready {
		if ready.HolderPeer != "" {
			w.Header().Set("X-Kojo-Holder-Peer", ready.HolderPeer)
		}
		code := "runtime_not_ready"
		if ready.Switching {
			code = "switching"
		} else if ready.HolderPeer != "" && s.peerID != nil && ready.HolderPeer != s.peerID.DeviceID {
			code = "wrong_holder"
		}
		writeError(w, http.StatusConflict, code, ready.Unavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, externalChatTextBodyLimit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req externalChatTextRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid external chat request: "+err.Error())
		return
	}
	if err := req.Goal.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Goal commands carry their intent separately from chat text. Only a
	// validated goal request may omit the message; empty ordinary turns stay
	// invalid. Do this before any backend or attachment work.
	attach := req.AttachBackgroundToken != ""
	if attach && (req.Goal != nil || strings.TrimSpace(req.SessionKey) == "" || len(req.Attachments) > 0 || req.ForceFreshSession) {
		writeError(w, http.StatusBadRequest, "bad_request", "background attach carries only its session key and token")
		return
	}
	if strings.TrimSpace(req.Message) == "" && req.Goal == nil && !attach {
		writeError(w, http.StatusBadRequest, "bad_request", "message is required")
		return
	}
	for i := range req.Attachments {
		if req.Attachments[i].PeerID != "" && s.peerID != nil && req.Attachments[i].PeerID != s.peerID.DeviceID {
			writeError(w, http.StatusConflict, "wrong_holder", "attachment belongs to a different holder")
			return
		}
		f, size, kind := openUploadPath(req.Attachments[i].Path)
		if kind != "" {
			writeError(w, http.StatusBadRequest, "invalid_attachment", uploadPathUserMessage(kind))
			return
		}
		_ = f.Close()
		req.Attachments[i].Size = size
	}
	hubPeerID, hubAddr, hubErr := s.externalChatHubAddress(r.Context(), r, req.HubMCPBaseURL)
	if hubErr != nil {
		if errors.Is(hubErr, errExternalChatHubForbidden) {
			writeError(w, http.StatusForbidden, "forbidden", hubErr.Error())
		} else {
			writeError(w, http.StatusServiceUnavailable, "hub_unavailable", hubErr.Error())
		}
		return
	}
	releaseRelay := func() {}
	if s.externalChatRelays != nil {
		releaseRelay = s.externalChatRelays.acquire(agentID, hubPeerID)
	}
	defer releaseRelay()
	var attachEntry *keyedBgAttachEntry
	var events <-chan agent.ChatEvent
	var err error
	if attach {
		attachEntry = s.keyedBg.claim(req.AttachBackgroundToken, agentID, req.SessionKey, hubPeerID)
		if attachEntry == nil {
			writeError(w, http.StatusNotFound, "background_turn_not_found", "no pending keyed background turn for this token")
			return
		}
		events = attachEntry.events
		if gid := req.ResponseAttachmentGroupID; gid != "" && req.ResponseAttachmentMessageID != "" && req.SessionKey == "groupdm:"+gid {
			events = s.agents.CaptureKeyedBackgroundAttachments(r.Context(), agentID, gid, req.ResponseAttachmentMessageID, events)
		}
	} else {
		opts := agent.OneShotOpts{
			Goal:                              req.Goal,
			GoalUserID:                        req.GoalUserID,
			GoalRunID:                         req.GoalRunID,
			SessionKey:                        req.SessionKey,
			InteractiveQuestions:              r.Header.Get("X-Kojo-Interactive-Questions") == "v1",
			FreshSessionContext:               req.FreshSessionContext,
			ResumeSessionContext:              req.ResumeSessionContext,
			SystemPromptExtra:                 req.SystemPromptExtra,
			DisableKojoAttachmentInstructions: req.DisableAttachments,
			Attachments:                       req.Attachments,
			SlackMCPBaseURL:                   hubAddr,
			OriginPeerID:                      hubPeerID,
			ForceFreshSession:                 req.ForceFreshSession,
			HandoffCapability:                 strings.TrimSpace(r.Header.Get("X-Kojo-Handoff-Capability")),
			ResponseAttachmentGroupID:         req.ResponseAttachmentGroupID,
			ResponseAttachmentMessageID:       req.ResponseAttachmentMessageID,
		}
		if req.LingerBackgroundTasks && strings.TrimSpace(req.SessionKey) != "" && s.peerID != nil && hubAddr != "" {
			// The session may outlive this request (max linger); its surface
			// holds its own relay reference and reports back to this Hub.
			surface := s.newRemoteKeyedSurface(agentID, req.SessionKey, hubPeerID, hubAddr)
			defer surface.Release()
			opts.LingerBackgroundTasks = true
			opts.KeyedSurface = surface
		}
		events, err = s.agents.ChatOneShot(r.Context(), agentID, req.Message, opts)
	}
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrGoalOwnerForbidden):
			writeError(w, http.StatusForbidden, "goal_owner_forbidden", err.Error())
		case errors.Is(err, agent.ErrAgentBusy) && s.agents.IsSwitching(agentID):
			writeError(w, http.StatusConflict, "switching", err.Error())
		case errors.Is(err, agent.ErrAgentNotFound):
			// Readiness and admission are intentionally separate. The lock can
			// move after the first check but before ChatOneShot registers; that
			// is a safe pre-admission reroute, not a terminal missing agent.
			latest := s.externalChatReadiness(r.Context(), agentID)
			if latest.HolderPeer != "" {
				w.Header().Set("X-Kojo-Holder-Peer", latest.HolderPeer)
			}
			code := "runtime_not_ready"
			if latest.Switching {
				code = "switching"
			} else if latest.HolderPeer != "" && s.peerID != nil && latest.HolderPeer != s.peerID.DeviceID {
				code = "wrong_holder"
			}
			writeError(w, http.StatusConflict, code, err.Error())
		case errors.Is(err, agent.ErrAgentArchived):
			writeError(w, http.StatusConflict, "archived", err.Error())
		default:
			writeError(w, http.StatusConflict, "chat_rejected", err.Error())
		}
		return
	}

	w.Header().Set("Content-Type", externalChatTextContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	enc := json.NewEncoder(w)
	heartbeat := time.NewTicker(externalChatHeartbeat)
	defer heartbeat.Stop()
	terminal := false
	if attachEntry != nil {
		// Settle the claimed background turn however the stream ends: one
		// that never reached its terminal event (Hub gone) is aborted, and
		// the waiting surface is released either way.
		defer func() { attachEntry.finish(terminal) }()
	}
	type pendingAttachmentAck struct {
		event agent.ChatEvent
		token string
		entry *externalChatAttachmentAckEntry
	}
	var pendingAcks []pendingAttachmentAck
	finishPending := func() bool {
		allAccepted := true
		for i := range pendingAcks {
			pending := &pendingAcks[i]
			// Once the terminal envelope was flushed, the Hub closes the
			// response stream so its adapter can persist the reply. Keep this
			// application-level commit wait independent of that socket context.
			ackCtx, cancelAck := context.WithTimeout(context.Background(), externalChatAttachmentAckWait)
			select {
			case <-pending.entry.done:
			case <-ackCtx.Done():
			}
			cancelAck()
			accepted := s.externalChatAttachmentAcks.finish(pending.token, pending.entry)
			pending.event.FinishAttachmentOwnership(accepted)
			allAccepted = allAccepted && accepted
		}
		pendingAcks = nil
		return allAccepted
	}
	defer func() {
		// Every event already flushed to the Hub gets the same application-level
		// settlement path, including socket cancellation and encode failures. The
		// Hub may still persist a partial reply after observing EOF.
		if len(pendingAcks) > 0 {
			finishPending()
		}
	}()
	ackV1 := r.Header.Get(externalChatAttachmentAckHeader) == externalChatAttachmentAckV1
	for {
		select {
		case <-r.Context().Done():
			return
		case evt, ok := <-events:
			if !ok {
				if !terminal {
					terminal = true
					evt = agent.ChatEvent{Type: "error", ErrorMessage: "remote agent chat ended unexpectedly"}
					if err := enc.Encode(externalChatTextEnvelope{Kind: "event", Event: &evt}); err == nil {
						if flusher != nil {
							flusher.Flush()
						}
						finishPending()
					}
				}
				return
			}
			if evt.Type == "done" || evt.Type == "error" {
				terminal = true
			}
			attachmentFlushed := false
			if evt.Type == "attachment" {
				if !evt.BeginAttachmentOwnership() {
					continue
				}
				var ackToken string
				var ackEntry *externalChatAttachmentAckEntry
				if ackV1 {
					var ackErr error
					ackToken, ackEntry, ackErr = s.externalChatAttachmentAcks.register(agentID, hubPeerID)
					if ackErr != nil {
						evt.FinishAttachmentOwnership(false)
						return
					}
				}
				// Once ownership enters committing state, cancellation waits for
				// the Hub response adapter's explicit acknowledgement. Merely
				// flushing bytes to the socket is not an ownership transfer: the
				// Hub may disconnect before decoding or persisting the event.
				controller := http.NewResponseController(w)
				_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
				err := enc.Encode(externalChatTextEnvelope{Kind: "event", Event: &evt, AttachmentAckToken: ackToken})
				if err == nil {
					err = controller.Flush()
					attachmentFlushed = err == nil
				}
				_ = controller.SetWriteDeadline(time.Time{})
				if err != nil {
					if ackEntry != nil {
						s.externalChatAttachmentAcks.finish(ackToken, ackEntry)
					}
					evt.FinishAttachmentOwnership(false)
					return
				}
				if ackEntry == nil {
					// Old Hub: preserve the pre-v1 flush-based transfer contract.
					evt.FinishAttachmentOwnership(true)
				} else {
					pendingAcks = append(pendingAcks, pendingAttachmentAck{
						event: evt, token: ackToken, entry: ackEntry,
					})
				}
			} else if err := enc.Encode(externalChatTextEnvelope{Kind: "event", Event: &evt}); err != nil {
				return
			}
			if flusher != nil && !attachmentFlushed {
				flusher.Flush()
			}
			if terminal {
				finishPending()
				return
			}
		case <-heartbeat.C:
			if err := enc.Encode(externalChatTextEnvelope{Kind: "heartbeat"}); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func (s *Server) handleExternalChatSteer(w http.ResponseWriter, r *http.Request) {
	if !s.externalChatPeerAllowed(w, r) {
		return
	}
	agentID := r.PathValue("id")
	ready := s.externalChatReadiness(r.Context(), agentID)
	if !ready.Ready {
		if ready.HolderPeer != "" {
			w.Header().Set("X-Kojo-Holder-Peer", ready.HolderPeer)
		}
		code := "runtime_not_ready"
		if ready.Switching {
			code = "switching"
		} else if ready.HolderPeer != "" && s.peerID != nil && ready.HolderPeer != s.peerID.DeviceID {
			code = "wrong_holder"
		}
		writeError(w, http.StatusConflict, code, ready.Unavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req externalChatSteerRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid external steer request: "+err.Error())
		return
	}
	if req.StopBackground {
		if strings.TrimSpace(req.SessionKey) == "" || req.Content != "" || req.Question != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "stopBackground takes only sessionKey")
			return
		}
		origin := auth.FromContext(r.Context()).PeerID
		if s.unsafePeer && auth.FromContext(r.Context()).IsOwner() {
			origin = ""
		}
		err := s.agents.StopThreadBackgroundTasksFromOrigin(agentID, req.SessionKey, origin)
		switch {
		case err == nil:
			writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
		case errors.Is(err, agent.ErrBackgroundSessionNotFound):
			writeError(w, http.StatusNotFound, "background_session_not_found", err.Error())
		case errors.Is(err, agent.ErrSteerOriginForbidden):
			writeError(w, http.StatusForbidden, "forbidden", "caller does not own this thread session")
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	if strings.TrimSpace(req.SessionKey) == "" || (req.Question == nil && strings.TrimSpace(req.Content) == "") || (req.Question != nil && (req.Question.RequestID == "" || req.Content != "")) {
		writeError(w, http.StatusBadRequest, "bad_request", "sessionKey and exactly one of content or question.requestId are required")
		return
	}
	p := auth.FromContext(r.Context())
	var err error
	if req.Question != nil {
		origin := p.PeerID
		if s.unsafePeer && p.IsOwner() {
			origin = ""
		} else if origin == "" {
			writeError(w, http.StatusForbidden, "forbidden", "question answer requires an origin peer")
			return
		}
		q := req.Question
		err = s.agents.AnswerOneShotQuestion(agentID, req.SessionKey, origin, q.RequestID, q.Answers, q.Deny, q.DenyMessage)
	} else if s.unsafePeer && p.IsOwner() {
		err = s.agents.SteerOneShotAsUser(agentID, req.SessionKey, "", req.Content, req.GoalUserID)
	} else {
		err = s.agents.SteerOneShotAsUser(agentID, req.SessionKey, p.PeerID, req.Content, req.GoalUserID)
	}
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrGoalOwnerForbidden):
			writeError(w, http.StatusForbidden, "goal_owner_forbidden", err.Error())
		case errors.Is(err, agent.ErrQuestionNotFound):
			writeError(w, http.StatusNotFound, "question_not_found", err.Error())
		case errors.Is(err, agent.ErrInvalidQuestionAnswer):
			writeError(w, http.StatusBadRequest, "invalid_answers", err.Error())
		case errors.Is(err, agent.ErrSteerOriginForbidden):
			writeError(w, http.StatusForbidden, "forbidden", "external steer caller did not open this turn")
		case errors.Is(err, agent.ErrSteerDeliveryUncertain):
			writeError(w, http.StatusBadGateway, "delivery_uncertain", err.Error())
		case errors.Is(err, agent.ErrAgentNotBusy):
			writeError(w, http.StatusConflict, "not_busy", "agent thread has no turn in progress")
		case errors.Is(err, agent.ErrSteerUnsupported):
			writeError(w, http.StatusConflict, "unsupported", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) externalChatPeerAllowed(w http.ResponseWriter, r *http.Request) bool {
	p := auth.FromContext(r.Context())
	if p.IsPeer() || s.unsafePeer && p.IsOwner() {
		return true
	}
	writeError(w, http.StatusForbidden, "forbidden", "external chat relay is peer-only")
	return false
}

func (r *externalChatRouter) decodeExternalChatTextStream(ctx context.Context, agentID, holder string, body io.ReadCloser, out chan<- agent.ChatEvent, onCancel ...func()) {
	defer body.Close()
	// json.Decoder can otherwise remain blocked in Read after the Slack turn
	// is cancelled. Closing the response body tears down the HTTP stream and,
	// on the holder, cancels the request context passed to ChatOneShot.
	cancelOnce := sync.OnceFunc(func() {
		for _, f := range onCancel {
			f()
		}
	})
	stopClose := context.AfterFunc(ctx, func() { cancelOnce(); _ = body.Close() })
	defer stopClose()
	dec := json.NewDecoder(body)
	terminal := false
	closed := false
	closeOut := func() {
		// Every close path, including attachment finalization, must commit
		// stop intent before the adapter can admit a queued recovery.
		if ctx.Err() != nil {
			cancelOnce()
		}
		if !closed {
			close(out)
			closed = true
		}
	}
	defer func() {
		if ctx.Err() != nil {
			cancelOnce()
		}
		closeOut()
	}()
	type pendingAttachmentAck struct {
		event agent.ChatEvent
		token string
	}
	var pendingAcks []pendingAttachmentAck
	finalizePending := func() {
		// Closing the adapter stream lets GroupDM persist a complete or partial
		// reply. Its ownership decision then drives the holder ACK below.
		closeOut()
		for i := range pendingAcks {
			if !pendingAcks[i].event.WaitAttachmentOwnership(ctx) {
				return
			}
			ackCtx, cancelAck := context.WithTimeout(context.Background(), 10*time.Second)
			err := r.postRemoteAttachmentAck(ackCtx, agentID, holder, pendingAcks[i].token)
			cancelAck()
			if err != nil {
				return
			}
		}
		pendingAcks = nil
	}
	defer finalizePending()
	for {
		var env externalChatTextEnvelope
		err := dec.Decode(&env)
		if err != nil {
			if !terminal {
				msg := "remote Slack chat stream ended unexpectedly"
				if !errors.Is(err, io.EOF) {
					msg = "decode remote Slack chat stream: " + err.Error()
				} else if ctx.Err() != nil {
					msg = "remote Slack chat was interrupted: " + ctx.Err().Error()
				}
				sendExternalChatEvent(ctx, out, agent.ChatEvent{Type: "error", ErrorMessage: msg})
			}
			return
		}
		if env.Kind == "heartbeat" || env.Event == nil {
			continue
		}
		evt := *env.Event
		if evt.Type == "done" || evt.Type == "error" {
			terminal = true
		}
		if evt.Type == "attachment" && env.AttachmentAckToken != "" {
			evt.PrepareAttachmentOwnership()
		}
		if !sendExternalChatEvent(ctx, out, evt) {
			return
		}
		if evt.Type == "attachment" && env.AttachmentAckToken != "" {
			pendingAcks = append(pendingAcks, pendingAttachmentAck{event: evt, token: env.AttachmentAckToken})
		}
		if terminal {
			return
		}
	}
}

func (r *externalChatRouter) postRemoteAttachmentAck(ctx context.Context, agentID, holder, token string) error {
	rec, err := r.server.agents.Store().GetPeer(ctx, holder)
	if err != nil {
		return fmt.Errorf("resolve attachment holder peer: %w", err)
	}
	addr, err := peer.NormalizeAddress(rec.URL)
	if err != nil {
		return fmt.Errorf("resolve attachment holder address: %w", err)
	}
	body, err := json.Marshal(externalChatAttachmentAckRequest{Token: token})
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost,
			addr+"/api/v1/agents/"+agentID+"/external-chat/attachment-ack", bytes.NewReader(body))
		if reqErr != nil {
			return reqErr
		}
		req.Header.Set("Content-Type", "application/json")
		resp, doErr := peer.NoKeepAliveHTTPClient(10 * time.Second).Do(req)
		if doErr == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			doErr = fmt.Errorf("holder returned %s", resp.Status)
		}
		lastErr = doErr
		if ctx.Err() != nil {
			break
		}
	}
	return lastErr
}

func (s *Server) handleExternalChatAttachmentAck(w http.ResponseWriter, r *http.Request) {
	if !s.externalChatPeerAllowed(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var in externalChatAttachmentAckRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Token) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid attachment acknowledgement")
		return
	}
	p := auth.FromContext(r.Context())
	peerID := p.PeerID
	if s.unsafePeer && p.IsOwner() {
		peerID = ""
	}
	if !s.externalChatAttachmentAcks.acknowledge(in.Token, r.PathValue("id"), peerID) {
		writeError(w, http.StatusConflict, "invalid_ack", "attachment acknowledgement is expired or belongs to another turn")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

func sendExternalChatEvent(ctx context.Context, out chan<- agent.ChatEvent, evt agent.ChatEvent) bool {
	select {
	case out <- evt:
		return true
	case <-ctx.Done():
		return false
	}
}

// RegisterKeyedBackgroundHandler lets the Slack bot receive keyed background
// turns from agents running on this process. Lingering only happens for
// Hub-local dispatch, so delegating to the local Manager is complete.
func (r *externalChatRouter) RegisterKeyedBackgroundHandler(agentID string, h agent.KeyedBackgroundHandler) func() {
	if r == nil || r.server == nil || r.server.agents == nil {
		return func() {}
	}
	return r.server.agents.RegisterKeyedBackgroundHandler(agentID, h)
}

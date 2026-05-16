package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/slackbot"
)

// --- Slack Bot Configuration ---

type slackBotRequest struct {
	Enabled        bool   `json:"enabled"`
	AppToken       string `json:"appToken"`
	BotToken       string `json:"botToken"`
	ThreadReplies  *bool  `json:"threadReplies"`
	RespondDM      *bool  `json:"respondDM"`
	RespondMention *bool  `json:"respondMention"`
	RespondThread  *bool  `json:"respondThread"`
}

type slackBotResponse struct {
	Enabled        bool `json:"enabled"`
	ThreadReplies  bool `json:"threadReplies"`
	RespondDM      bool `json:"respondDM"`
	RespondMention bool `json:"respondMention"`
	RespondThread  bool `json:"respondThread"`
	HasAppToken    bool `json:"hasAppToken"`
	HasBotToken    bool `json:"hasBotToken"`
	Connected      bool `json:"connected"`
}

func (s *Server) handleGetSlackBot(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	a, ok := s.agents.Get(agentID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found")
		return
	}

	cfg := a.SlackBot
	if cfg == nil {
		cfg = &agent.SlackBotConfig{ThreadReplies: true}
	}
	resp := slackBotResponse{
		Enabled:        cfg.Enabled,
		ThreadReplies:  cfg.ThreadReplies,
		RespondDM:      cfg.ReactDM(),
		RespondMention: cfg.ReactMention(),
		RespondThread:  cfg.ReactThread(),
	}

	// Check if tokens exist
	creds := s.agents.Credentials()
	if creds != nil {
		appToken, _ := creds.GetToken("slack", agentID, "", "app_token")
		botToken, _ := creds.GetToken("slack", agentID, "", "bot_token")
		resp.HasAppToken = appToken != ""
		resp.HasBotToken = botToken != ""
	}

	if s.slackHub != nil {
		resp.Connected = s.slackHub.IsRunning(agentID)
	}

	writeJSONResponse(w, http.StatusOK, resp)
}

func (s *Server) handleSetSlackBot(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	_, ok := s.agents.Get(agentID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found")
		return
	}
	// §3.7 device switch gate covers the WHOLE chain (token
	// store, UpdateSlackBot, hub StartBot) — the inner
	// UpdateSlackBot call would re-acquire if we relied on
	// that alone, but the token write before it would slip
	// past.
	releaseMut, mutErr := s.agents.AcquireMutation(agentID)
	if mutErr != nil {
		writeError(w, http.StatusConflict, "agent_busy", mutErr.Error())
		return
	}
	defer releaseMut()

	// PUT /slackbot mutates the SlackBot field of the agent record. The
	// caller's optimistic-concurrency view of that agent is checked
	// against the v1 store's agent etag — same precondition the PATCH
	// /agents/{id} path uses, since SlackBot is part of the same row.
	// `*` is rejected on PUT because the row must already exist (we
	// 404'd above on missing agent); "any current version" carries no
	// useful semantics here.
	ifMatch, ifMatchPresent, err := extractDomainIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid If-Match header")
		return
	}
	if !s.enforceIfMatchPresence(w, r, ifMatchPresent) {
		return
	}
	if ifMatchPresent && ifMatch == "*" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"If-Match: * is not supported on PUT /slackbot; send a specific etag or omit the header")
		return
	}

	var req slackBotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}

	// Per-agent serialization across precheck → UpdateSlackBot → ETag
	// echo. Mirrors handleUpdateAgent: without the lock, two concurrent
	// PUT /slackbot requests carrying the same If-Match would both pass
	// the precheck, both succeed, and the second writer would silently
	// stomp the first's effective config without observing its etag.
	release := s.agents.LockPatch(agentID)
	defer release()

	// Re-check agent existence under the lock. The Get above ran
	// before the lock acquisition, so a concurrent DELETE /agents/{id}
	// could have removed the agent between then and now. Without this
	// re-check (and with If-Match omitted, where agentIfMatchPrecheck
	// is a no-op), we'd write Slack tokens to a credential store row
	// for a freshly-deleted agent — orphaned secrets.
	if _, ok := s.agents.Get(agentID); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found")
		return
	}

	if !s.agentIfMatchPrecheck(w, r, agentID, ifMatch, ifMatchPresent) {
		return
	}

	// Validate tokens
	if req.AppToken != "" && !strings.HasPrefix(req.AppToken, "xapp-") {
		writeError(w, http.StatusBadRequest, "bad_request", "appToken must start with xapp-")
		return
	}
	if req.BotToken != "" && !strings.HasPrefix(req.BotToken, "xoxb-") {
		writeError(w, http.StatusBadRequest, "bad_request", "botToken must start with xoxb-")
		return
	}

	// Store tokens if provided
	creds := s.agents.Credentials()
	if creds == nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "credential store not available")
		return
	}

	if req.AppToken != "" || req.BotToken != "" {
		// Load existing tokens so we can do partial updates. A
		// LoadTokens failure means we cannot safely merge — falling
		// back to "" would silently overwrite the present field with
		// empty, losing data. Refuse the write so the caller can
		// retry once the underlying credential store is reachable.
		existingApp, existingBot, loadErr := slackbot.LoadTokens(creds, agentID)
		if loadErr != nil && (req.AppToken == "" || req.BotToken == "") {
			s.logger.Error("slackbot: load existing tokens for partial update failed",
				"agent", agentID, "err", loadErr)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"could not load existing Slack tokens for partial update; resend with both tokens or retry later")
			return
		}
		appToken := req.AppToken
		if appToken == "" {
			appToken = existingApp
		}
		botToken := req.BotToken
		if botToken == "" {
			botToken = existingBot
		}
		if err := slackbot.StoreTokens(creds, agentID, appToken, botToken); err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
	}

	// Build config
	threadReplies := true
	if req.ThreadReplies != nil {
		threadReplies = *req.ThreadReplies
	}
	cfg := agent.SlackBotConfig{
		Enabled:        req.Enabled,
		ThreadReplies:  threadReplies,
		RespondDM:      req.RespondDM,
		RespondMention: req.RespondMention,
		RespondThread:  req.RespondThread,
	}

	// Update agent config
	// Use the no-guard variant: we already hold AcquireMutation
	// at the top of this handler. Calling the guarded variant
	// would re-Acquire; a SetSwitching landing between the two
	// would refuse the inner Acquire after the token had
	// already been saved, leaving partial state.
	if err := s.agents.UpdateSlackBotAlreadyGuarded(agentID, &cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Reconcile running bot with new config — but only if the agent is
	// active. Reconfigure on an archived agent would start the bot we
	// intentionally stopped at archive time.
	if s.slackHub != nil {
		if a, ok := s.agents.Get(agentID); ok && !a.Archived {
			s.slackHub.Reconfigure(agentID, cfg)
		}
	}

	// Echo the new agent etag so the client can chain follow-up PATCH
	// /agents/{id} or PUT /slackbot calls without an extra GET. The
	// LockPatch above guarantees no other writer landed between
	// UpdateSlackBot and this read.
	if newETag := s.readAgentETag(r, agentID); newETag != "" {
		w.Header().Set("ETag", quoteETag(newETag))
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteSlackBot(w http.ResponseWriter, r *http.Request) {
	// Wrap the whole delete flow (token Delete + UpdateSlackBot
	// + hub StopBot) in one mutation guard so a handoff that
	// lands mid-delete can't leave source with credentials but
	// no config row.
	if agentID := r.PathValue("id"); agentID != "" {
		releaseMut, mutErr := s.agents.AcquireMutation(agentID)
		if mutErr != nil {
			writeError(w, http.StatusConflict, "agent_busy", mutErr.Error())
			return
		}
		defer releaseMut()
	}
	agentID := r.PathValue("id")
	_, ok := s.agents.Get(agentID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found")
		return
	}

	// Same If-Match contract as PUT — DELETE /slackbot mutates the
	// SlackBot field on the agent record, so the precondition checks
	// against the agent etag. `*` is rejected for the same reason.
	ifMatch, ifMatchPresent, err := extractDomainIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid If-Match header")
		return
	}
	if !s.enforceIfMatchPresence(w, r, ifMatchPresent) {
		return
	}
	if ifMatchPresent && ifMatch == "*" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"If-Match: * is not supported on DELETE /slackbot; send a specific etag or omit the header")
		return
	}

	// Per-agent serialization across precheck → UpdateSlackBot → token
	// delete. Holding the lock around the token cleanup matters here:
	// without it, a concurrent PUT could re-store tokens we're about
	// to drop, leaving a half-configured bot row.
	release := s.agents.LockPatch(agentID)
	defer release()

	// Re-check existence under the lock — see handleSetSlackBot.
	if _, ok := s.agents.Get(agentID); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found")
		return
	}

	if !s.agentIfMatchPrecheck(w, r, agentID, ifMatch, ifMatchPresent) {
		return
	}

	// Stop bot
	if s.slackHub != nil {
		s.slackHub.StopBot(agentID)
	}

	// Remove config
	// No-guard variant: we already hold AcquireMutation at the
	// top of handleDeleteSlackBot.
	if err := s.agents.UpdateSlackBotAlreadyGuarded(agentID, nil); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Delete tokens. Surface failures: leaving Slack secrets in the
	// credential store after a successful "delete" 200 response is
	// the worst kind of partial-failure — the caller assumes the
	// secrets are gone, but a future enable would re-load them and
	// silently re-pair the bot. 5xx so the caller knows to retry.
	//
	// On cleanup failure echo the *new* agent ETag in the response
	// header — without that, the same caller's If-Match would still
	// be the pre-DELETE value and a retry would 412 against the
	// just-bumped row, leaving the secrets stuck. Echoing the new
	// ETag lets the caller chain its retry without an intervening
	// GET.
	creds := s.agents.Credentials()
	if creds != nil {
		if err := slackbot.DeleteTokens(creds, agentID); err != nil {
			s.logger.Error("slackbot: token cleanup failed", "agent", agentID, "err", err)
			if newETag := s.readAgentETag(r, agentID); newETag != "" {
				w.Header().Set("ETag", quoteETag(newETag))
			}
			writeError(w, http.StatusInternalServerError, "internal_error",
				"slackbot config cleared but token cleanup failed; retry to ensure secrets are removed")
			return
		}
	}

	if newETag := s.readAgentETag(r, agentID); newETag != "" {
		w.Header().Set("ETag", quoteETag(newETag))
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleTestSlackBot(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	_, ok := s.agents.Get(agentID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found")
		return
	}

	// Accept optional tokens in request body (for testing before saving).
	var req struct {
		AppToken string `json:"appToken"`
		BotToken string `json:"botToken"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // best-effort; empty body is fine

	appToken := strings.TrimSpace(req.AppToken)
	botToken := strings.TrimSpace(req.BotToken)

	// Fall back to stored tokens for any that weren't provided.
	if appToken == "" || botToken == "" {
		creds := s.agents.Credentials()
		if creds != nil {
			storedApp, storedBot, _ := slackbot.LoadTokens(creds, agentID)
			if appToken == "" {
				appToken = storedApp
			}
			if botToken == "" {
				botToken = storedBot
			}
		}
	}

	if appToken == "" || botToken == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "slack tokens not configured")
		return
	}

	team, botUser, err := slackbot.TestConnection(r.Context(), appToken, botToken)
	if err != nil {
		writeError(w, http.StatusBadRequest, "connection_failed", err.Error())
		return
	}

	writeJSONResponse(w, http.StatusOK, map[string]any{
		"ok":      true,
		"team":    team,
		"botUser": botUser,
	})
}

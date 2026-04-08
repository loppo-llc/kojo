package server

import (
	"encoding/json"
	"net/http"
	"strings"

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

	resp := slackBotResponse{
		// Defaults for when SlackBot is nil
		RespondDM:      true,
		RespondMention: true,
		RespondThread:  true,
	}
	if a.SlackBot != nil {
		resp.Enabled = a.SlackBot.Enabled
		resp.ThreadReplies = a.SlackBot.ThreadReplies
		resp.RespondDM = a.SlackBot.ReactDM()
		resp.RespondMention = a.SlackBot.ReactMention()
		resp.RespondThread = a.SlackBot.ReactThread()
	}

	// Check if tokens exist
	creds := s.agents.Credentials()
	if creds != nil {
		appToken, _ := creds.GetToken("slack", agentID, "", "app_token")
		botToken, _ := creds.GetToken("slack", agentID, "", "bot_token")
		resp.HasAppToken = appToken != ""
		resp.HasBotToken = botToken != ""
	}

	hub := s.agents.SlackHub()
	if hub != nil {
		resp.Connected = hub.IsRunning(agentID)
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

	var req slackBotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
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
		// Load existing tokens so we can do partial updates
		existingApp, existingBot, _ := slackbot.LoadTokens(creds, agentID)
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
	cfg := slackbot.Config{
		Enabled:        req.Enabled,
		ThreadReplies:  threadReplies,
		RespondDM:      req.RespondDM,
		RespondMention: req.RespondMention,
		RespondThread:  req.RespondThread,
	}

	// Update agent
	if err := s.agents.UpdateSlackBot(agentID, &cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Start/stop bot
	hub := s.agents.SlackHub()
	if hub != nil {
		hub.Reconfigure(agentID, cfg)
	}

	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteSlackBot(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	_, ok := s.agents.Get(agentID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found")
		return
	}

	// Stop bot
	hub := s.agents.SlackHub()
	if hub != nil {
		hub.StopBot(agentID)
	}

	// Remove config
	if err := s.agents.UpdateSlackBot(agentID, nil); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Delete tokens
	creds := s.agents.Credentials()
	if creds != nil {
		_ = slackbot.DeleteTokens(creds, agentID)
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

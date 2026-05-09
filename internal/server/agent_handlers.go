package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
)

// directoryView returns a public, agent-safe view of an Agent. Used by
// list/get handlers when the caller is not the Owner and not the agent
// itself — exposes only ID/Name/PublicProfile and the avatar metadata
// already shipped via /api/v1/agents/{id}/avatar.
type directoryView struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	PublicProfile string `json:"publicProfile"`
	HasAvatar     bool   `json:"hasAvatar"`
	AvatarHash    string `json:"avatarHash,omitempty"`
	Archived      bool   `json:"archived,omitempty"`
}

func toDirectoryView(a *agent.Agent) directoryView {
	if a == nil {
		return directoryView{}
	}
	return directoryView{
		ID:            a.ID,
		Name:          a.Name,
		PublicProfile: a.PublicProfile,
		HasAvatar:     a.HasAvatar,
		AvatarHash:    a.AvatarHash,
		Archived:      a.Archived,
	}
}

// agentResponse embeds *agent.Agent and tacks on runtime-only fields the UI
// consumes. These fields aren't persisted so they live outside the Agent
// struct itself.
type agentResponse struct {
	*agent.Agent
	NextCronAt string `json:"nextCronAt,omitempty"`
}

// buildAgentResponse decorates an *agent.Agent with derived runtime state
// (next cron run, etc.) for API responses.
func (s *Server) buildAgentResponse(a *agent.Agent) agentResponse {
	resp := agentResponse{Agent: a}
	if a != nil {
		if t := s.agents.NextCronRun(a.ID); !t.IsZero() {
			resp.NextCronAt = t.Format(time.RFC3339)
		}
	}
	return resp
}

// --- Cron Pause ---

func (s *Server) handleGetCronPaused(w http.ResponseWriter, r *http.Request) {
	writeJSONResponse(w, http.StatusOK, map[string]any{"paused": s.agents.CronPaused()})
}

func (s *Server) handleSetCronPaused(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paused bool `json:"paused"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	s.agents.SetCronPaused(body.Paused)
	writeJSONResponse(w, http.StatusOK, map[string]any{"paused": body.Paused})
}

// --- Active Hour Check ---

func (s *Server) handleGetAgentActive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	active, ok := s.agents.IsAgentActive(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"active": active})
}

// --- Agent CRUD Handlers ---

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	// Default: hide archived agents from the main list. Pass
	// ?includeArchived=true to fetch archived alongside active (used by the
	// "Archived agents" section in global Settings) or ?archived=true to
	// fetch only archived. The two flags are mutually exclusive — combining
	// them is a caller bug, not something we want to silently resolve in
	// favor of one or the other.
	q := r.URL.Query()
	includeArchived := q.Get("includeArchived") == "true"
	onlyArchived := q.Get("archived") == "true"
	if includeArchived && onlyArchived {
		writeError(w, http.StatusBadRequest, "bad_request",
			"archived and includeArchived are mutually exclusive")
		return
	}

	p := auth.FromContext(r.Context())
	all := s.agents.List()
	if p.IsOwner() {
		out := make([]*agent.Agent, 0, len(all))
		for _, a := range all {
			switch {
			case onlyArchived && !a.Archived:
				continue
			case !onlyArchived && !includeArchived && a.Archived:
				continue
			}
			out = append(out, a)
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"agents": out})
		return
	}

	// Non-owner: full record for self, directory view for others.
	// Force archived agents off the list — non-owners have no business
	// enumerating tombstoned personas, so the includeArchived /
	// onlyArchived flags are silently ignored at this layer (an
	// agent's own archived self is also filtered out — they should be
	// asking the owner to revive them, not poking the API).
	out := make([]any, 0, len(all))
	for _, a := range all {
		if a.Archived {
			continue
		}
		if p.IsAgent() && p.AgentID == a.ID {
			out = append(out, a)
		} else {
			out = append(out, toDirectoryView(a))
		}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"agents": out})
}

func (s *Server) handleAgentDirectory(w http.ResponseWriter, r *http.Request) {
	entries := s.agents.Directory()
	writeJSONResponse(w, http.StatusOK, map[string]any{"agents": entries})
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	if p := auth.FromContext(r.Context()); !p.CanForkOrCreate() {
		writeError(w, http.StatusForbidden, "forbidden", "agent creation is owner-only")
		return
	}
	var cfg agent.AgentConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	a, err := s.agents.Create(cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, a)
}

func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok := s.agents.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	p := auth.FromContext(r.Context())
	if p.CanReadFull(id) {
		writeJSONResponse(w, http.StatusOK, s.buildAgentResponse(a))
		return
	}
	writeJSONResponse(w, http.StatusOK, toDirectoryView(a))
}


func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := auth.FromContext(r.Context())
	if !p.CanMutateSelf(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agents may only PATCH themselves")
		return
	}
	// Defensive: refuse a payload that smuggles a "privileged" field
	// regardless of role. The Owner has a dedicated POST /privilege
	// endpoint; anyone else trying to flip the bit through PATCH is
	// almost certainly attempting self-elevation. Match keys
	// case-insensitively so a casing trick can't sneak past either.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err == nil {
		for k := range raw {
			if strings.EqualFold(k, "privileged") {
				writeError(w, http.StatusForbidden, "forbidden",
					"privileged is owner-only; use POST /api/v1/agents/{id}/privilege")
				return
			}
		}
	}
	var cfg agent.AgentUpdateConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	a, err := s.agents.Update(id, cfg)
	if err != nil {
		if errors.Is(err, agent.ErrAgentNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, s.buildAgentResponse(a))
}

// handleCheckin fires a manual check-in for the agent. The check-in runs
// asynchronously on the server (events drained in a goroutine), so we
// return immediately with 202 Accepted; the assistant reply will appear
// in the transcript via the normal chat flow.
func (s *Server) handleCheckin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := auth.FromContext(r.Context())
	if !p.CanDeleteOrReset(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agent is not allowed to checkin others")
		return
	}
	if err := s.agents.Checkin(id); err != nil {
		switch {
		case errors.Is(err, agent.ErrAgentNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, agent.ErrAgentArchived):
			writeError(w, http.StatusConflict, "archived", err.Error())
		case errors.Is(err, agent.ErrAgentBusy), errors.Is(err, agent.ErrAgentResetting):
			writeError(w, http.StatusConflict, "busy", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (s *Server) handleResetAgentData(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := auth.FromContext(r.Context())
	if !p.CanDeleteOrReset(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agent is not allowed to reset others")
		return
	}

	// Stop Slack bot before resetting to avoid stale file references.
	if s.slackHub != nil {
		s.slackHub.StopBot(id)
	}

	if err := s.agents.ResetData(id); err != nil {
		if errors.Is(err, agent.ErrAgentNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else if errors.Is(err, agent.ErrAgentBusy) {
			writeError(w, http.StatusConflict, "conflict", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}

	// Restart Slack bot if it was enabled.
	if s.slackHub != nil {
		if a, ok := s.agents.Get(id); ok && a.SlackBot != nil && a.SlackBot.Enabled {
			s.slackHub.StartBot(id, *a.SlackBot)
		}
	}

	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleTruncateAgentMemory removes everything in the agent's memory
// recorded at-or-after the given instant: kojo transcript records, Claude
// session JSONL records (with trailing-turn trim), and daily diary bullets
// in memory/YYYY-MM-DD.md. Settings, persona, MEMORY.md, project / people /
// topic notes, archive, credentials and tasks are untouched.
//
// Two ways to specify the threshold (request body):
//   - {"since": "2026-05-09T12:00:00+09:00"}     — absolute RFC3339
//   - {"fromMessageId": "m_abc..."}              — use that message's
//     timestamp; the message itself is included in the removed set.
//
// Same auth gate as reset/delete (CanDeleteOrReset), same busy / reset
// guard semantics. Returns 404 when the agent or fromMessageId can't be
// found, 409 if a chat is in flight or another reset is racing.
func (s *Server) handleTruncateAgentMemory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := auth.FromContext(r.Context())
	if !p.CanDeleteOrReset(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agent is not allowed to truncate others' memory")
		return
	}

	var body struct {
		Since         string `json:"since"`
		FromMessageID string `json:"fromMessageId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}

	hasSince := strings.TrimSpace(body.Since) != ""
	hasMsg := strings.TrimSpace(body.FromMessageID) != ""
	if hasSince == hasMsg {
		writeError(w, http.StatusBadRequest, "bad_request",
			"exactly one of 'since' (RFC3339) or 'fromMessageId' is required")
		return
	}

	var (
		res *agent.TruncateMemoryResult
		err error
	)
	if hasSince {
		t, perr := time.Parse(time.RFC3339, body.Since)
		if perr != nil {
			writeError(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("'since' must be RFC3339 (e.g. 2026-05-09T12:00:00+09:00): %v", perr))
			return
		}
		res, err = s.agents.TruncateMemoryAt(id, t)
	} else {
		res, err = s.agents.TruncateMemoryFromMessage(id, body.FromMessageID)
	}

	if err != nil {
		switch {
		case errors.Is(err, agent.ErrAgentNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, agent.ErrMessageNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, agent.ErrAgentBusy), errors.Is(err, agent.ErrAgentResetting):
			writeError(w, http.StatusConflict, "conflict", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}

	writeJSONResponse(w, http.StatusOK, res)
}

func (s *Server) handleForkAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if p := auth.FromContext(r.Context()); !p.CanForkOrCreate() {
		writeError(w, http.StatusForbidden, "forbidden", "fork is owner-only")
		return
	}
	var body struct {
		Name              string `json:"name"`
		IncludeTranscript bool   `json:"includeTranscript"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	a, err := s.agents.Fork(id, agent.ForkOptions{Name: body.Name, IncludeTranscript: body.IncludeTranscript})
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrAgentNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, agent.ErrAgentBusy), errors.Is(err, agent.ErrAgentResetting):
			writeError(w, http.StatusConflict, "conflict", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, a)
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if p := auth.FromContext(r.Context()); !p.CanDeleteOrReset(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agent is not allowed to delete others")
		return
	}
	// ?archive=true keeps all on-disk data but stops runtime activity.
	// Restored later via POST /api/v1/agents/{id}/unarchive.
	archive := r.URL.Query().Get("archive") == "true"

	// Stop Slack bot before either operation so it doesn't keep running.
	if s.slackHub != nil {
		s.slackHub.StopBot(id)
	}

	var err error
	if archive {
		err = s.agents.Archive(id)
	} else {
		err = s.agents.Delete(id)
	}
	if err != nil {
		// Roll back the StopBot above on failure: the agent is still active
		// (Archive/Delete refused), so its Slack bot must be restored to
		// avoid a silent disconnect.
		if s.slackHub != nil {
			if a, ok := s.agents.Get(id); ok && !a.Archived && a.SlackBot != nil && a.SlackBot.Enabled {
				s.slackHub.StartBot(id, *a.SlackBot)
			}
		}
		if errors.Is(err, agent.ErrAgentNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else if errors.Is(err, agent.ErrAgentBusy) {
			writeError(w, http.StatusConflict, "conflict", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleUnarchiveAgent restores an archived agent. Re-arms cron, notify
// poller, and (if previously enabled) the Slack bot.
func (s *Server) handleUnarchiveAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if p := auth.FromContext(r.Context()); !p.CanDeleteOrReset(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agent is not allowed to unarchive others")
		return
	}
	if err := s.agents.Unarchive(id); err != nil {
		switch {
		case errors.Is(err, agent.ErrAgentNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, agent.ErrAgentBusy), errors.Is(err, agent.ErrAgentResetting):
			// Concurrent Archive/Unarchive/reset still in progress — caller
			// can retry. Map both to 409 so the client knows to back off.
			writeError(w, http.StatusConflict, "busy", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	// Re-arm Slack bot if it was configured. Manager.Unarchive can't do this
	// — slackHub lives on the server, not the manager.
	if s.slackHub != nil {
		if a, ok := s.agents.Get(id); ok && a.SlackBot != nil && a.SlackBot.Enabled {
			s.slackHub.StartBot(id, *a.SlackBot)
		}
	}
	if a, ok := s.agents.Get(id); ok {
		writeJSONResponse(w, http.StatusOK, a)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Avatar Handlers ---

func (s *Server) handleGetAvatar(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok := s.agents.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	agent.ServeAvatar(w, r, a)
}

func (s *Server) handleUploadAvatar(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 10<<20) // 10MB max
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "file too large (max 10MB)")
		} else {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid multipart form")
		}
		return
	}

	file, header, err := r.FormFile("avatar")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "missing avatar file")
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	if !agent.IsAllowedImageExt(ext) {
		writeError(w, http.StatusBadRequest, "bad_request", "unsupported image format")
		return
	}

	if err := agent.SaveAvatar(id, file, ext); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Messages Handler ---

func (s *Server) handleGetMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}

	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}

	before := r.URL.Query().Get("before")

	msgs, hasMore, err := s.agents.MessagesPaginated(id, limit, before)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if msgs == nil {
		msgs = []*agent.Message{}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"messages": msgs, "hasMore": hasMore})
}

func (s *Server) handleUpdateMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	msgID := r.PathValue("msgId")

	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}

	msg, err := s.agents.UpdateMessageContent(id, msgID, body.Content)
	if err != nil {
		writeTranscriptEditError(w, err, msgID)
		return
	}
	writeJSONResponse(w, http.StatusOK, msg)
}

func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	msgID := r.PathValue("msgId")

	if err := s.agents.DeleteMessage(id, msgID); err != nil {
		writeTranscriptEditError(w, err, msgID)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRegenerateMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	msgID := r.PathValue("msgId")
	if err := s.agents.Regenerate(id, msgID); err != nil {
		writeTranscriptEditError(w, err, msgID)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

func writeTranscriptEditError(w http.ResponseWriter, err error, msgID string) {
	switch {
	case errors.Is(err, agent.ErrAgentNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, agent.ErrMessageNotFound):
		writeError(w, http.StatusNotFound, "not_found", "message not found: "+msgID)
	case errors.Is(err, agent.ErrInvalidRegenerate):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, agent.ErrUnsupportedTool):
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, agent.ErrAgentBusy), errors.Is(err, agent.ErrAgentResetting):
		writeError(w, http.StatusConflict, "busy", err.Error())
	case errors.Is(err, agent.ErrAgentArchived):
		writeError(w, http.StatusConflict, "archived", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

// --- Generate Handlers ---

func (s *Server) handleGeneratePersona(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurrentPersona string `json:"currentPersona"`
		Prompt         string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "prompt is required")
		return
	}

	persona, err := agent.GeneratePersona(req.CurrentPersona, req.Prompt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]string{"persona": persona})
}

func (s *Server) handleGenerateName(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Persona string `json:"persona"`
		Prompt  string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if req.Persona == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "persona is required")
		return
	}

	name, err := agent.GenerateName(req.Persona, req.Prompt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]string{"name": name})
}

func (s *Server) handleGenerateAvatar(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Persona      string `json:"persona"`
		Name         string `json:"name"`
		Prompt       string `json:"prompt"`
		PreviousPath string `json:"previousPath"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}

	// Clean up previous temp avatar if provided
	if req.PreviousPath != "" {
		cleanupTempAvatar(req.PreviousPath)
	}

	avatarPath, err := agent.GenerateAvatarWithAI(r.Context(), "", req.Persona, req.Name, req.Prompt, s.logger)
	if err != nil {
		s.logger.Warn("AI avatar generation failed, using SVG fallback", "err", err)
		svgPath, svgErr := agent.GenerateSVGAvatarFile(req.Name)
		if svgErr != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", svgErr.Error())
			return
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"avatarPath": svgPath, "fallback": true})
		return
	}

	writeJSONResponse(w, http.StatusOK, map[string]string{"avatarPath": avatarPath})
}

// cleanupTempAvatar removes a previously generated temp avatar directory.
func cleanupTempAvatar(avatarPath string) {
	absPath, err := filepath.EvalSymlinks(avatarPath)
	if err != nil {
		return
	}
	tempDir, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return
	}
	if !strings.HasPrefix(absPath, tempDir+string(filepath.Separator)) {
		return
	}
	rel, _ := filepath.Rel(tempDir, absPath)
	parts := strings.SplitN(rel, string(filepath.Separator), 2)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "kojo-avatar-") {
		return
	}
	// Remove the entire kojo-avatar-* temp directory
	os.RemoveAll(filepath.Join(tempDir, parts[0]))
}

// handlePreviewAvatar serves a generated avatar from the temp directory for preview.
func (s *Server) handlePreviewAvatar(w http.ResponseWriter, r *http.Request) {
	avatarPath := r.URL.Query().Get("path")
	if avatarPath == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "path is required")
		return
	}

	absPath, err := agent.ValidateTempAvatarPath(avatarPath)
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrAvatarInternal):
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		case errors.Is(err, agent.ErrAvatarNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		default:
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}

	http.ServeFile(w, r, absPath)
}

// handleUploadGeneratedAvatar copies a generated avatar to the agent's directory.
// --- Credential Handlers ---

func (s *Server) requireCredentialStore(w http.ResponseWriter) bool {
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store is not available")
		return false
	}
	return true
}

func (s *Server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	if !s.requireCredentialStore(w) {
		return
	}
	creds, err := s.agents.Credentials().ListCredentials(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if creds == nil {
		creds = []*agent.Credential{}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"credentials": creds})
}

func (s *Server) handleAddCredential(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
	var req struct {
		Label         string `json:"label"`
		Username      string `json:"username"`
		Password      string `json:"password"`
		TOTPSecret    string `json:"totpSecret"`
		TOTPAlgorithm string `json:"totpAlgorithm"`
		TOTPDigits    int    `json:"totpDigits"`
		TOTPPeriod    int    `json:"totpPeriod"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if req.Label == "" || req.Username == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "label and username are required")
		return
	}
	if req.Password == "" && req.TOTPSecret == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "password or totpSecret is required")
		return
	}
	if !s.requireCredentialStore(w) {
		return
	}
	var totp *agent.TOTPParams
	if req.TOTPSecret != "" {
		totp = &agent.TOTPParams{
			Secret:    req.TOTPSecret,
			Algorithm: req.TOTPAlgorithm,
			Digits:    req.TOTPDigits,
			Period:    req.TOTPPeriod,
		}
	}
	cred, err := s.agents.Credentials().AddCredential(id, req.Label, req.Username, req.Password, totp)
	if err != nil {
		if errors.Is(err, agent.ErrInvalidTOTP) {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, cred)
}

func (s *Server) handleUpdateCredential(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	credID := r.PathValue("credId")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
	var req struct {
		Label         *string `json:"label"`
		Username      *string `json:"username"`
		Password      *string `json:"password"`
		TOTPSecret    *string `json:"totpSecret"`
		TOTPAlgorithm *string `json:"totpAlgorithm"`
		TOTPDigits    *int    `json:"totpDigits"`
		TOTPPeriod    *int    `json:"totpPeriod"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	var totp *agent.TOTPParams
	if req.TOTPSecret != nil {
		alg := ""
		if req.TOTPAlgorithm != nil {
			alg = *req.TOTPAlgorithm
		}
		digits := 0
		if req.TOTPDigits != nil {
			digits = *req.TOTPDigits
		}
		period := 0
		if req.TOTPPeriod != nil {
			period = *req.TOTPPeriod
		}
		totp = &agent.TOTPParams{
			Secret:    *req.TOTPSecret,
			Algorithm: alg,
			Digits:    digits,
			Period:    period,
		}
	}
	if !s.requireCredentialStore(w) {
		return
	}
	cred, err := s.agents.Credentials().UpdateCredential(id, credID, req.Label, req.Username, req.Password, totp)
	if err != nil {
		if errors.Is(err, agent.ErrCredentialNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else if errors.Is(err, agent.ErrInvalidTOTP) {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, cred)
}

func (s *Server) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	credID := r.PathValue("credId")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	if !s.requireCredentialStore(w) {
		return
	}
	if err := s.agents.Credentials().DeleteCredential(id, credID); err != nil {
		if errors.Is(err, agent.ErrCredentialNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRevealCredentialPassword(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	credID := r.PathValue("credId")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	if !s.requireCredentialStore(w) {
		return
	}
	password, err := s.agents.Credentials().RevealPassword(id, credID)
	if err != nil {
		if errors.Is(err, agent.ErrCredentialNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSONResponse(w, http.StatusOK, map[string]string{"password": password})
}

func (s *Server) handleGetTOTPCode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	credID := r.PathValue("credId")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	if !s.requireCredentialStore(w) {
		return
	}
	code, remaining, err := s.agents.Credentials().GetTOTPCode(id, credID)
	if err != nil {
		if errors.Is(err, agent.ErrCredentialNotFound) || errors.Is(err, agent.ErrNoTOTPSecret) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSONResponse(w, http.StatusOK, map[string]any{"code": code, "remaining": remaining})
}

func (s *Server) handleParseQR(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 10<<20) // 10MB max
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid multipart form")
		return
	}

	file, _, err := r.FormFile("qr")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "missing qr file")
		return
	}
	defer file.Close()

	entries, err := agent.DecodeQRImage(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleParseOTPURI(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
	var req struct {
		URI string `json:"uri"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if req.URI == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "uri is required")
		return
	}

	entries, err := agent.ParseOTPURI(req.URI)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleUploadGeneratedAvatar(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}

	var req struct {
		AvatarPath string `json:"avatarPath"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}

	if req.AvatarPath == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "avatarPath is required")
		return
	}

	absPath, err := agent.ValidateTempAvatarPath(req.AvatarPath)
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrAvatarInternal):
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		default:
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}

	ext := strings.ToLower(filepath.Ext(absPath))

	src, err := os.Open(absPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("cannot open avatar: %v", err))
		return
	}
	defer src.Close()

	if err := agent.SaveAvatar(id, src, ext); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Clean up the temp file
	os.Remove(absPath)

	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Task Handlers ---

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	tasks, err := agent.ListTasks(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"tasks": tasks})
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	var params agent.TaskCreateParams
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	task, err := agent.CreateTask(id, params)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, task)
}

func (s *Server) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	taskID := r.PathValue("taskId")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	var params agent.TaskUpdateParams
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	task, err := agent.UpdateTask(id, taskID, params)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, task)
}

func (s *Server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	taskID := r.PathValue("taskId")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	if err := agent.DeleteTask(id, taskID); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Pre-Compact Handler ---

// preCompactHookPayload mirrors the subset of Claude Code's PreCompact hook
// JSON we care about. Claude pipes the full event object to the hook
// command's stdin; with `--data-binary @-` it lands here verbatim.
// Documented at:
//
//	https://docs.anthropic.com/en/docs/claude-code/hooks#precompact
//
// We only extract `transcript_path` so PreCompactSummarize can read the
// exact session being compacted, instead of probing by mtime in the
// project directory (which races with parallel sessions).
type preCompactHookPayload struct {
	TranscriptPath string `json:"transcript_path"`
	Trigger        string `json:"trigger"`
	SessionID      string `json:"session_id"`
}

func (s *Server) handlePreCompact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok := s.agents.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	// Archived agents must not have summaries appended to their diary —
	// nothing should be writing into their persisted state while dormant.
	if a.Archived {
		writeError(w, http.StatusConflict, "conflict", "agent is archived")
		return
	}
	// Best-effort decode. An empty / malformed body is fine: the
	// summarizer falls back to project-dir discovery when transcript_path
	// is empty, and we still want to honour the hook in either case.
	// Cap the body so a misbehaving hook can't OOM the server with a
	// gigabyte JSON object — Claude's PreCompact payload is a few hundred
	// bytes in practice, 64 KiB is plenty of headroom.
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var payload preCompactHookPayload
	_ = json.NewDecoder(r.Body).Decode(&payload)

	// Run synchronously — the PreCompact hook blocks until this returns
	if err := agent.PreCompactSummarize(id, a.Tool, payload.TranscriptPath, s.logger); err != nil {
		s.logger.Warn("pre-compact summarize failed", "agent", id, "err", err)
		// Return 200 anyway — don't block compaction on summary failure
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Session Reset Handler ---

// handlePrivilegeAgent toggles the Privileged flag on an agent. Owner-only;
// the auth middleware already denies non-Owner principals on this route, but
// the handler reasserts the check defensively because the privilege flag is
// the only authority an agent has to reach delete/reset on others.
func (s *Server) handlePrivilegeAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if p := auth.FromContext(r.Context()); !p.CanSetPrivileged() {
		writeError(w, http.StatusForbidden, "forbidden", "privilege is owner-only")
		return
	}
	var body struct {
		Privileged bool `json:"privileged"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if err := s.agents.SetPrivileged(id, body.Privileged); err != nil {
		if errors.Is(err, agent.ErrAgentNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"id": id, "privileged": body.Privileged})
}

func (s *Server) handleResetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if p := auth.FromContext(r.Context()); !p.CanDeleteOrReset(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agent is not allowed to reset others")
		return
	}
	if err := s.agents.ResetSession(id); err != nil {
		if errors.Is(err, agent.ErrAgentNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else if errors.Is(err, agent.ErrAgentBusy) {
			writeError(w, http.StatusConflict, "conflict", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}


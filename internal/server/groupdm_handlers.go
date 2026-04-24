package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/loppo-llc/kojo/internal/agent"
)

// --- Group DM Handlers ---

func (s *Server) handleListGroupDMs(w http.ResponseWriter, r *http.Request) {
	groups := s.groupdms.List()
	if groups == nil {
		groups = []*agent.GroupDM{}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"groups": groups})
}

func (s *Server) handleCreateGroupDM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string   `json:"name"`
		MemberIDs []string `json:"memberIds"`
		Cooldown  int      `json:"cooldown"`
		Style     string   `json:"style"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if len(req.MemberIDs) < 2 {
		writeError(w, http.StatusBadRequest, "bad_request", "at least 2 members required")
		return
	}
	g, err := s.groupdms.Create(req.Name, req.MemberIDs, req.Cooldown, agent.GroupDMStyle(req.Style))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, g)
}

func (s *Server) handleGetGroupDM(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	g, ok := s.groupdms.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "group not found: "+id)
		return
	}
	writeJSONResponse(w, http.StatusOK, g)
}

func (s *Server) handleRenameGroupDM(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Name     string `json:"name"`
		AgentID  string `json:"agentId"`
		Cooldown *int   `json:"cooldown"`
		Style    string `json:"style"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}

	// Validate all fields before applying any changes to avoid partial writes.
	if req.Name == "" && req.Cooldown == nil && req.Style == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name, cooldown, or style is required")
		return
	}
	if req.Style != "" && !agent.ValidGroupDMStyles[agent.GroupDMStyle(req.Style)] {
		writeError(w, http.StatusBadRequest, "bad_request",
			"invalid style: must be \"efficient\" or \"expressive\"")
		return
	}
	// Rename requires agentId (membership authorization).
	if req.Name != "" && req.AgentID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "agentId is required for name changes")
		return
	}
	// Preflight: verify group exists and caller is a member (for rename).
	if req.AgentID != "" {
		if err := s.groupdms.CheckMembership(id, req.AgentID); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}

	var result *agent.GroupDM

	// Update cooldown if provided
	if req.Cooldown != nil {
		g, err := s.groupdms.SetCooldown(id, *req.Cooldown)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		result = g
	}

	// Update style if provided
	if req.Style != "" {
		g, err := s.groupdms.SetStyle(id, agent.GroupDMStyle(req.Style), req.AgentID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		result = g
	}

	// Rename if name provided
	if req.Name != "" {
		g, err := s.groupdms.Rename(id, req.Name, req.AgentID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		result = g
	}
	writeJSONResponse(w, http.StatusOK, result)
}

func (s *Server) handleDeleteGroupDM(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	notify := r.URL.Query().Get("notify") == "true"
	if err := s.groupdms.Delete(id, notify); err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleGetGroupMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	before := r.URL.Query().Get("before")

	msgs, hasMore, err := s.groupdms.Messages(id, limit, before)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if msgs == nil {
		msgs = []*agent.GroupMessage{}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"messages": msgs,
		"hasMore":  hasMore,
	})
}

func (s *Server) handlePostGroupMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		AgentID string `json:"agentId"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if req.AgentID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "agentId is required")
		return
	}
	if req.Content == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "content is required")
		return
	}
	// The reserved "user" sender is not a group member and must go through
	// the dedicated user-messages endpoint.
	if req.AgentID == agent.UserSenderID {
		writeError(w, http.StatusBadRequest, "bad_request",
			"agentId \"user\" is reserved; use POST /api/v1/groupdms/{id}/user-messages")
		return
	}

	// Always notify on API-initiated messages (user or agent-initiated).
	// Notifications trigger chats that may produce follow-up messages,
	// but the busy check in Manager.Chat naturally breaks infinite loops.
	msg, err := s.groupdms.PostMessage(r.Context(), id, req.AgentID, req.Content, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, msg)
}

// handlePostGroupUserMessage posts a message from the human user (operator)
// to a group and notifies every member. Unlike agent-authored messages this
// endpoint takes no agentId — the sender is always the reserved "user" ID.
func (s *Server) handlePostGroupUserMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if req.Content == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "content is required")
		return
	}
	msg, err := s.groupdms.PostUserMessage(r.Context(), id, req.Content, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, msg)
}

func (s *Server) handleAddGroupMember(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		AgentID       string `json:"agentId"`
		CallerAgentID string `json:"callerAgentId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if req.AgentID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "agentId is required")
		return
	}
	if req.CallerAgentID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "callerAgentId is required")
		return
	}
	g, err := s.groupdms.AddMember(id, req.AgentID, req.CallerAgentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, g)
}

func (s *Server) handleLeaveGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	agentID := r.PathValue("agentId")
	if err := s.groupdms.LeaveGroup(id, agentID); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleListAgentGroups(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	groups := s.groupdms.GroupsForAgent(agentID)
	if groups == nil {
		groups = []*agent.GroupDM{}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"groups": groups})
}

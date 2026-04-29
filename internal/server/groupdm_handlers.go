package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
)

// bindAgentIdentity reconciles body-supplied agentId-style fields with
// the request Principal:
//
//   - Owner: passes through whatever the body says (admin-style call).
//   - Agent / PrivAgent: the value must be empty or match the
//     Principal's AgentID. Mismatched / impersonating bodies get a
//     hard 403 instead of silently being rewritten — silent rewrite
//     would mask client bugs that try to post-as-someone-else.
//   - Guest / unknown: 403.
//
// On success, returns the canonical agent ID to use (empty string when
// the caller is Owner and supplied none).
func (s *Server) bindAgentIdentity(w http.ResponseWriter, r *http.Request, supplied string) (string, bool) {
	p := auth.FromContext(r.Context())
	if p.IsOwner() {
		return supplied, true
	}
	if !p.IsAgent() {
		writeError(w, http.StatusForbidden, "forbidden", "agent identity required")
		return "", false
	}
	if supplied != "" && supplied != p.AgentID {
		writeError(w, http.StatusForbidden, "forbidden",
			"agent id in body does not match authenticated principal")
		return "", false
	}
	return p.AgentID, true
}

// requireMemberOrOwner blocks reads/writes on a group when the caller
// is neither the Owner nor a current member of the group. Returns
// false after writing the error response — caller should return.
func (s *Server) requireMemberOrOwner(w http.ResponseWriter, r *http.Request, groupID string) bool {
	p := auth.FromContext(r.Context())
	if p.IsOwner() {
		return true
	}
	if !p.IsAgent() {
		writeError(w, http.StatusForbidden, "forbidden", "agent identity required")
		return false
	}
	if err := s.groupdms.CheckMembership(groupID, p.AgentID); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", "not a member of this group")
		return false
	}
	return true
}

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
		Venue     string   `json:"venue"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if len(req.MemberIDs) < 2 {
		writeError(w, http.StatusBadRequest, "bad_request", "at least 2 members required")
		return
	}
	// Agents may only create groups they themselves belong to, so a stray
	// agent can't conjure a room it has no business being in. Owners can
	// create on anyone's behalf.
	if p := auth.FromContext(r.Context()); !p.IsOwner() {
		if !p.IsAgent() {
			writeError(w, http.StatusForbidden, "forbidden", "agent identity required")
			return
		}
		found := false
		for _, m := range req.MemberIDs {
			if m == p.AgentID {
				found = true
				break
			}
		}
		if !found {
			writeError(w, http.StatusForbidden, "forbidden",
				"caller must be one of the memberIds")
			return
		}
	}
	g, err := s.groupdms.Create(req.Name, req.MemberIDs, req.Cooldown,
		agent.GroupDMStyle(req.Style), agent.GroupDMVenue(req.Venue))
	if err != nil {
		// Don't leak whether a memberId names an unknown / archived agent
		// to non-Owners — that would let an agent enumerate hidden or
		// archived peers via the create endpoint.
		if p := auth.FromContext(r.Context()); !p.IsOwner() &&
			(errors.Is(err, agent.ErrAgentNotFound) || errors.Is(err, agent.ErrAgentArchived)) {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid memberIds")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, g)
}

// groupDMWithStatus wraps a GroupDM and overlays per-member online status.
type groupDMWithStatus struct {
	*agent.GroupDM
	Members []groupMemberWithStatus `json:"members"`
}

type groupMemberWithStatus struct {
	agent.GroupMember
	Status string `json:"status"` // "online", "offline", or "unknown"
}

func (s *Server) handleGetGroupDM(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireMemberOrOwner(w, r, id) {
		return
	}
	g, ok := s.groupdms.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "group not found: "+id)
		return
	}

	isPhysical := g.Venue == agent.GroupDMVenueColocated
	members := make([]groupMemberWithStatus, len(g.Members))
	for i, m := range g.Members {
		var status string
		if isPhysical {
			status = "unknown"
		} else if avail, found := s.agents.IsAgentDMAvailable(m.AgentID); !found {
			status = "unknown"
		} else if avail && s.agents.IsBusy(m.AgentID) {
			status = "busy"
		} else if avail {
			status = "online"
		} else {
			status = "offline"
		}
		members[i] = groupMemberWithStatus{GroupMember: m, Status: status}
	}

	resp := groupDMWithStatus{GroupDM: g, Members: members}
	writeJSONResponse(w, http.StatusOK, resp)
}

func (s *Server) handleRenameGroupDM(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Name     string `json:"name"`
		AgentID  string `json:"agentId"`
		Cooldown *int   `json:"cooldown"`
		Style    string `json:"style"`
		Venue    string `json:"venue"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	// Pin the caller identity to the authenticated Principal — agents
	// must not be able to PATCH a group "as" another member.
	bound, ok := s.bindAgentIdentity(w, r, req.AgentID)
	if !ok {
		return
	}
	req.AgentID = bound

	// Validate all fields before applying any changes to avoid partial writes.
	if req.Name == "" && req.Cooldown == nil && req.Style == "" && req.Venue == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"name, cooldown, style, or venue is required")
		return
	}
	if req.Style != "" && !agent.ValidGroupDMStyles[agent.GroupDMStyle(req.Style)] {
		writeError(w, http.StatusBadRequest, "bad_request",
			"invalid style: must be \"efficient\" or \"expressive\"")
		return
	}
	if req.Venue != "" && !agent.ValidGroupDMVenues[agent.GroupDMVenue(req.Venue)] {
		writeError(w, http.StatusBadRequest, "bad_request",
			"invalid venue: must be \"chatroom\" or \"colocated\"")
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

	// Update venue if provided
	if req.Venue != "" {
		g, err := s.groupdms.SetVenue(id, agent.GroupDMVenue(req.Venue), req.AgentID)
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
	if !s.requireMemberOrOwner(w, r, id) {
		return
	}
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	before := r.URL.Query().Get("before")

	msgs, hasMore, latestID, err := s.groupdms.Messages(id, limit, before)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if msgs == nil {
		msgs = []*agent.GroupMessage{}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"messages":        msgs,
		"hasMore":         hasMore,
		"latestMessageId": latestID,
	})
}

func (s *Server) handlePostGroupMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		AgentID string `json:"agentId"`
		Content string `json:"content"`
		// ExpectedLatestMessageID is the CAS guard. When non-empty, the
		// server rejects the post with 409 Conflict if any other member
		// posted after this ID, and returns the diff so the agent can
		// decide whether to retry. Empty value skips the check (legacy
		// or admin-style callers stay supported).
		ExpectedLatestMessageID string `json:"expectedLatestMessageId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	// Bind the sender to the authenticated Principal — non-Owner
	// callers cannot post as someone else, period.
	bound, ok := s.bindAgentIdentity(w, r, req.AgentID)
	if !ok {
		return
	}
	if bound != "" {
		req.AgentID = bound
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
	msg, err := s.groupdms.PostMessage(r.Context(), id, req.AgentID, req.Content, req.ExpectedLatestMessageID, true)
	if err != nil {
		// Stale CAS cursor — return 409 with the new head and the diff so
		// the caller has everything they need to decide whether to repost.
		var staleErr *agent.StaleExpectedIDError
		if errors.As(err, &staleErr) {
			newMsgs := staleErr.NewMessages
			if newMsgs == nil {
				newMsgs = []*agent.GroupMessage{}
			}
			writeJSONResponse(w, http.StatusConflict, map[string]any{
				"error":           "stale_expected_message_id",
				"message":         staleErr.Error(),
				"latestMessageId": staleErr.Latest,
				"newMessages":     newMsgs,
				"hasMore":         staleErr.HasMore,
			})
			return
		}
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
	// Pin the caller to the authenticated Principal so agents cannot
	// invite into a group on someone else's behalf. AgentID (the
	// invitee) is left up to the body — Owner can invite anyone, an
	// Agent caller can invite anyone but is recorded as the inviter.
	bound, ok := s.bindAgentIdentity(w, r, req.CallerAgentID)
	if !ok {
		return
	}
	if bound != "" {
		req.CallerAgentID = bound
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

// handleSetGroupMemberSettings updates per-member notification preferences:
// notifyMode ("realtime" | "digest" | "muted") and digestWindow (seconds).
// Members that opt out of realtime pings cut a large chunk of the per-turn
// token cost that DM notifications otherwise impose on busy groups.
//
// Authorization mirrors PATCH /api/v1/groupdms/{id}:
//   - If callerAgentId is supplied it must be a member of the group. Any
//     member may change any other member's preference — agents negotiate
//     quiet hours among themselves the same way they negotiate rename/style.
//   - An empty callerAgentId is treated as an admin/UI call and skips the
//     membership check, matching SetStyle's convention.
func (s *Server) handleSetGroupMemberSettings(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	agentID := r.PathValue("agentId")
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "agentId is required")
		return
	}
	var req struct {
		NotifyMode    string `json:"notifyMode"`
		DigestWindow  int    `json:"digestWindow"`
		CallerAgentID string `json:"callerAgentId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	// Pin the caller to the authenticated Principal — agents may
	// negotiate notify settings for any member of a group they belong
	// to, but the request must be signed by their own identity, not
	// claimed via callerAgentId in the body.
	bound, ok := s.bindAgentIdentity(w, r, req.CallerAgentID)
	if !ok {
		return
	}
	if bound != "" {
		req.CallerAgentID = bound
	}
	if req.NotifyMode == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "notifyMode is required")
		return
	}
	// SetMemberNotifyMode does its own caller membership + active check
	// inside the lock, which closes the race window between the membership
	// check and the mutation. The handler no longer pre-checks.
	g, err := s.groupdms.SetMemberNotifyMode(id, agentID, agent.NotifyMode(req.NotifyMode), req.DigestWindow, req.CallerAgentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, g)
}

func (s *Server) handleLeaveGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	agentID := r.PathValue("agentId")
	// Path agentId must match the calling Principal for non-Owner —
	// nobody but the Owner gets to evict another member.
	p := auth.FromContext(r.Context())
	if !p.IsOwner() {
		if !p.IsAgent() || p.AgentID != agentID {
			writeError(w, http.StatusForbidden, "forbidden", "agents may only leave on behalf of themselves")
			return
		}
	}
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

package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
)

// Background sessions: the agent's thread conversations (WebUI threads, Slack
// threads) whose CLI process is kept alive after a turn because
// run_in_background tasks are still running. Self-only (plus Owner / deputy);
// see the background-sessions guide.

// backgroundSessionAccess resolves the agent and enforces self-only access.
// Returns false after writing the error response.
func (s *Server) backgroundSessionAccess(w http.ResponseWriter, r *http.Request, id string) bool {
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return false
	}
	if !auth.FromContext(r.Context()).CanMutateSelf(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agents may only manage their own background sessions")
		return false
	}
	return true
}

// handleListBackgroundSessions GET /api/v1/agents/{id}/background-sessions
func (s *Server) handleListBackgroundSessions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.backgroundSessionAccess(w, r, id) {
		return
	}
	snap, err := s.agents.ListBackgroundSessions(id)
	if err != nil {
		mapBackgroundSessionErr(w, err)
		return
	}
	writeJSONResponse(w, http.StatusOK, snap)
}

// handleStopBackgroundSession DELETE /api/v1/agents/{id}/background-sessions/{key}
//
// Stops every background task of the thread session and posts a stop notice
// into the thread. {key} is the sessionKey from the list (URL-encode it; it
// contains ':').
func (s *Server) handleStopBackgroundSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.backgroundSessionAccess(w, r, id) {
		return
	}
	key := strings.TrimSpace(r.PathValue("key"))
	if key == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "session key required")
		return
	}
	if err := s.agents.StopBackgroundSession(id, key); err != nil {
		mapBackgroundSessionErr(w, err)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"stopped": true, "sessionKey": key})
}

// handleStopBackgroundTask DELETE /api/v1/agents/{id}/background-sessions/{key}/tasks/{taskId}
func (s *Server) handleStopBackgroundTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.backgroundSessionAccess(w, r, id) {
		return
	}
	key := strings.TrimSpace(r.PathValue("key"))
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	if key == "" || taskID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "session key and task id required")
		return
	}
	if err := s.agents.StopBackgroundTask(id, key, taskID); err != nil {
		mapBackgroundSessionErr(w, err)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"stopped": true, "sessionKey": key, "taskId": taskID})
}

func mapBackgroundSessionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agent.ErrAgentNotFound),
		errors.Is(err, agent.ErrBackgroundSessionNotFound),
		errors.Is(err, agent.ErrBackgroundTaskNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

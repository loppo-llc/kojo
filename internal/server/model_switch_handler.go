package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
)

const (
	modelSwitchBodyLimit = 16 << 10
	modelSwitchKeyLimit  = 1024
	modelSwitchKeyHeader = "X-Kojo-Session-Key"
)

type modelSwitchRequest struct {
	Model  string  `json:"model"`
	Tool   *string `json:"tool,omitempty"`
	Effort *string `json:"effort,omitempty"`
	Note   string  `json:"note,omitempty"`
}

type modelSwitchInfo struct {
	Current agent.ModelSwitchTarget `json:"current"`
	Options map[string][]string     `json:"options"`
}

func modelSwitchAllowed(p auth.Principal, agentID string) bool {
	return p.IsAgent() && p.AgentID == agentID
}

// handleGetAgentModelSwitch lists the current model and the selectable ones.
func (s *Server) handleGetAgentModelSwitch(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if !modelSwitchAllowed(auth.FromContext(r.Context()), agentID) {
		writeError(w, http.StatusForbidden, "forbidden", "only the agent itself may use this route")
		return
	}
	a, ok := s.agents.Get(agentID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found")
		return
	}
	writeJSONResponse(w, http.StatusOK, modelSwitchInfo{
		Current: agent.ModelSwitchTarget{Tool: agent.NormalizeToolName(a.Tool), Model: a.Model, Effort: a.Effort},
		Options: agent.ToolModelOptions(),
	})
}

// handleAgentModelSwitch stores a new model for the agent and arranges for
// the next turn to start by itself once the calling turn ends.
func (s *Server) handleAgentModelSwitch(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if !modelSwitchAllowed(auth.FromContext(r.Context()), agentID) {
		writeError(w, http.StatusForbidden, "forbidden", "only the agent itself may use this route")
		return
	}
	if agent.AgentSealed(agentID) {
		writeError(w, http.StatusConflict, "agent_sealed", "agent is being archived or deleted")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, modelSwitchBodyLimit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req modelSwitchRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "model is required")
		return
	}
	if utf8.RuneCountInString(req.Note) > agent.ModelSwitchNoteLimit {
		writeError(w, http.StatusBadRequest, "bad_request", "note is too long")
		return
	}
	sessionKey := r.Header.Get(modelSwitchKeyHeader)
	if len(sessionKey) > modelSwitchKeyLimit {
		writeError(w, http.StatusBadRequest, "bad_request", "session key is too long")
		return
	}
	res, err := s.agents.SwitchModel(agentID, agent.ModelSwitchRequest{
		Model: req.Model, Tool: req.Tool, Effort: req.Effort, Note: req.Note, SessionKey: sessionKey,
	})
	if err != nil {
		status, code := modelSwitchErrorStatus(err)
		writeError(w, status, code, err.Error())
		return
	}
	status := http.StatusOK
	if res.Status == agent.ModelSwitchQueued {
		status = http.StatusAccepted
	}
	writeJSONResponse(w, status, res)
}

func modelSwitchErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, agent.ErrAgentNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, agent.ErrModelSwitchUnknownTool):
		return http.StatusBadRequest, "unknown_tool"
	case errors.Is(err, agent.ErrModelSwitchUnknownModel):
		return http.StatusBadRequest, "unknown_model"
	case errors.Is(err, agent.ErrModelSwitchInvalidEffort):
		return http.StatusBadRequest, "invalid_effort"
	case errors.Is(err, agent.ErrAgentArchived):
		return http.StatusConflict, "agent_archived"
	case errors.Is(err, agent.ErrModelSwitchCallerInactive):
		return http.StatusConflict, "caller_not_active"
	case errors.Is(err, agent.ErrModelSwitchBackgroundTasks):
		return http.StatusConflict, "background_tasks_running"
	case errors.Is(err, agent.ErrModelSwitchGoalRunning):
		return http.StatusConflict, "goal_running"
	case errors.Is(err, agent.ErrModelSwitchPending):
		return http.StatusConflict, "switch_pending"
	case errors.Is(err, agent.ErrAgentBusy):
		return http.StatusConflict, "busy"
	}
	return http.StatusBadRequest, "invalid_config"
}

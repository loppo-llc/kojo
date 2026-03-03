package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/session"
)

// writeAgentError maps agent errors to HTTP status codes.
func writeAgentError(w http.ResponseWriter, err error) {
	if errors.Is(err, agent.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	} else {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	}
}

// --- Agent API Handlers ---

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	agents := s.agents.List()
	if agents == nil {
		agents = make([]*agent.Agent, 0)
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"agents": agents})
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string   `json:"name"`
		Tool     string   `json:"tool"`
		WorkDir  string   `json:"workDir"`
		Args     []string `json:"args"`
		YoloMode bool     `json:"yoloMode"`
		Schedule string   `json:"schedule"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	agent, err := s.agents.Create(req.Name, req.Tool, req.WorkDir, req.Args, req.YoloMode, req.Schedule)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, agent)
}

func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	agent := s.agents.Get(id)
	if agent == nil {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	writeJSONResponse(w, http.StatusOK, agent)
}

func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var updates map[string]any
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	agent, err := s.agents.Update(id, updates)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSONResponse(w, http.StatusOK, agent)
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.agents.Delete(id); err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRunAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.agents.Run(id)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSONResponse(w, http.StatusOK, sess.Info())
}

func (s *Server) handleAgentMemory(w http.ResponseWriter, r *http.Request) {
	s.handleAgentFile(w, r, "MEMORY.md")
}

func (s *Server) handleAgentSoul(w http.ResponseWriter, r *http.Request) {
	s.handleAgentFile(w, r, "SOUL.md")
}

func (s *Server) handleAgentGoals(w http.ResponseWriter, r *http.Request) {
	s.handleAgentFile(w, r, "GOALS.md")
}

func (s *Server) handleAgentFile(w http.ResponseWriter, r *http.Request, filename string) {
	id := r.PathValue("id")

	switch r.Method {
	case http.MethodGet:
		content, err := s.agents.ReadFile(id, filename)
		if err != nil {
			writeAgentError(w, err)
			return
		}
		writeJSONResponse(w, http.StatusOK, map[string]string{"content": content})

	case http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "failed to read body")
			return
		}
		// Try JSON first, fall back to raw text
		var req struct {
			Content *string `json:"content"`
		}
		if json.Unmarshal(body, &req) == nil && req.Content != nil {
			body = []byte(*req.Content)
		}
		if err := s.agents.WriteFile(id, filename, string(body)); err != nil {
			writeAgentError(w, err)
			return
		}
		writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func (s *Server) handleAgentSessions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sessions, err := s.agents.Sessions(id)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	if sessions == nil {
		sessions = make([]session.SessionInfo, 0)
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (s *Server) handleAgentLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	names, err := s.agents.LogNames(id)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	if names == nil {
		names = []string{}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"logs": names})
}

func (s *Server) handleAgentLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("name")
	content, err := s.agents.ReadLog(id, name)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]string{"content": content})
}

func (s *Server) handleAgentSearch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	query := r.URL.Query().Get("q")
	if query == "" {
		writeJSONResponse(w, http.StatusOK, map[string]any{"results": []any{}})
		return
	}
	results, err := s.agents.Search(id, query, 10)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	if results == nil {
		results = make([]agent.MemoryResult, 0)
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"results": results})
}

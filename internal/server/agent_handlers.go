package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/loppo-llc/kojo/internal/agent"
)

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

// --- Agent CRUD Handlers ---

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	agents := s.agents.List()
	writeJSONResponse(w, http.StatusOK, map[string]any{"agents": agents})
}

func (s *Server) handleAgentDirectory(w http.ResponseWriter, r *http.Request) {
	entries := s.agents.Directory()
	writeJSONResponse(w, http.StatusOK, map[string]any{"agents": entries})
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
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
	writeJSONResponse(w, http.StatusOK, a)
}

func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var cfg agent.AgentUpdateConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
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
	writeJSONResponse(w, http.StatusOK, a)
}

func (s *Server) handleResetAgentData(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

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

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Stop Slack bot before deleting the agent so it doesn't keep running.
	if s.slackHub != nil {
		s.slackHub.StopBot(id)
	}
	if err := s.agents.Delete(id); err != nil {
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

	avatarPath, err := agent.GenerateAvatarWithAI("", req.Persona, req.Name, req.Prompt, s.logger)
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

func (s *Server) handlePreCompact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok := s.agents.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	// Run synchronously — the PreCompact hook blocks until this returns
	if err := agent.PreCompactSummarize(id, a.Tool, s.logger); err != nil {
		s.logger.Warn("pre-compact summarize failed", "agent", id, "err", err)
		// Return 200 anyway — don't block compaction on summary failure
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Session Reset Handler ---

func (s *Server) handleResetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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


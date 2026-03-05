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

// --- Agent CRUD Handlers ---

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	agents := s.agents.List()
	writeJSONResponse(w, http.StatusOK, map[string]any{"agents": agents})
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
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, a)
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.agents.Delete(id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
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
	if ext != ".png" && ext != ".jpg" && ext != ".jpeg" && ext != ".webp" && ext != ".svg" {
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

	msgs, err := s.agents.Messages(id, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if msgs == nil {
		msgs = []*agent.Message{}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"messages": msgs})
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
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
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

	absPath, err := filepath.EvalSymlinks(avatarPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid avatar path")
		return
	}
	tempDir, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "cannot resolve temp dir")
		return
	}
	if !strings.HasPrefix(absPath, tempDir+string(filepath.Separator)) {
		writeError(w, http.StatusBadRequest, "bad_request", "avatar path must be in temp directory")
		return
	}

	// Only allow files inside kojo-avatar-* directories
	rel, _ := filepath.Rel(tempDir, absPath)
	parts := strings.SplitN(rel, string(filepath.Separator), 2)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "kojo-avatar-") {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid avatar path")
		return
	}

	// Only allow image extensions
	ext := strings.ToLower(filepath.Ext(absPath))
	if ext != ".png" && ext != ".jpg" && ext != ".jpeg" && ext != ".webp" {
		writeError(w, http.StatusBadRequest, "bad_request", "unsupported file type")
		return
	}

	// Must be a regular file (not dir, symlink, FIFO, device, etc.)
	fi, err := os.Stat(absPath)
	if err != nil || !fi.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "not_found", "file not found")
		return
	}

	http.ServeFile(w, r, absPath)
}

// handleUploadGeneratedAvatar copies a generated avatar to the agent's directory.
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

	// Validate the path is in temp directory (security: EvalSymlinks + separator-aware prefix)
	absPath, err := filepath.EvalSymlinks(req.AvatarPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid avatar path")
		return
	}
	// EvalSymlinks on tempDir too, to handle macOS /var → /private/var
	tempDir, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "cannot resolve temp dir")
		return
	}
	if !strings.HasPrefix(absPath, tempDir+string(filepath.Separator)) {
		writeError(w, http.StatusBadRequest, "bad_request", "avatar path must be in temp directory")
		return
	}

	// Only allow files inside kojo-avatar-* directories
	rel, _ := filepath.Rel(tempDir, absPath)
	parts := strings.SplitN(rel, string(filepath.Separator), 2)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "kojo-avatar-") {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid avatar path")
		return
	}

	// Only allow image extensions
	ext := strings.ToLower(filepath.Ext(absPath))
	if ext != ".png" && ext != ".jpg" && ext != ".jpeg" && ext != ".webp" {
		writeError(w, http.StatusBadRequest, "bad_request", "unsupported image format")
		return
	}

	// Must be a regular file
	fi, err := os.Stat(absPath)
	if err != nil || !fi.Mode().IsRegular() {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid avatar file")
		return
	}

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


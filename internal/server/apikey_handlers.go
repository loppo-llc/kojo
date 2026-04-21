package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
)

// geminiListHTTPClient has an explicit timeout so a stuck upstream can't
// tie up server goroutines indefinitely.
var geminiListHTTPClient = &http.Client{Timeout: 30 * time.Second}

// handleGetAPIKey returns whether an API key is configured for the given provider.
// Does NOT return the actual key — only a configured/not-configured status.
func (s *Server) handleGetAPIKey(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store not available")
		return
	}

	creds := s.agents.Credentials()
	_, err := creds.GetToken(provider, "", "", "api_key")
	configured := err == nil

	// Check nanobanana fallback for gemini
	hasFallback := false
	if provider == "gemini" {
		if home, err := os.UserHomeDir(); err == nil {
			data, err := os.ReadFile(filepath.Join(home, ".config", "nanobanana", "credentials"))
			hasFallback = err == nil && strings.TrimSpace(string(data)) != ""
		}
	}

	resp := map[string]any{
		"provider":    provider,
		"configured":  configured,
		"hasFallback": hasFallback,
	}

	// Include embedding model setting for gemini
	if provider == "gemini" {
		embModel := creds.GetSetting("embedding_model")
		if embModel == "" {
			embModel = agent.DefaultEmbeddingModel
		}
		resp["embeddingModel"] = embModel
	}

	writeJSONResponse(w, http.StatusOK, resp)
}

// handleSetAPIKey stores an API key for the given provider.
func (s *Server) handleSetAPIKey(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store not available")
		return
	}

	var req struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	if req.APIKey == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "apiKey is required")
		return
	}

	creds := s.agents.Credentials()
	if err := creds.SetToken(provider, "", "", "api_key", req.APIKey, time.Time{}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to save API key: "+err.Error())
		return
	}

	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSetEmbeddingModel saves the embedding model name and clears existing embeddings
// when the model changes (since dimensions may differ).
func (s *Server) handleSetEmbeddingModel(w http.ResponseWriter, r *http.Request) {
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store not available")
		return
	}

	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "model is required")
		return
	}

	creds := s.agents.Credentials()

	// Check if the effective model changed. When no model was explicitly
	// configured, existing embeddings were generated with the default model,
	// so treat an empty oldModel as the default rather than as "no embeddings
	// exist" — otherwise the first time a user picks a different model we'd
	// leave stale embeddings with mismatched dimensions.
	oldModel := creds.GetSetting("embedding_model")
	effectiveOld := oldModel
	if effectiveOld == "" {
		effectiveOld = agent.DefaultEmbeddingModel
	}
	modelChanged := effectiveOld != req.Model

	if err := creds.SetSetting("embedding_model", req.Model); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to save: "+err.Error())
		return
	}

	// Clear all embeddings if the effective model changed (dimensions may differ)
	if modelChanged {
		s.agents.ClearAllEmbeddings()
	}

	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "model": req.Model, "embeddingsCleared": modelChanged})
}

// handleDeleteAPIKey removes an API key for the given provider.
func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store not available")
		return
	}

	creds := s.agents.Credentials()
	if err := creds.DeleteToken(provider, "", "", "api_key"); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleListEmbeddingModels fetches available embedding models from the Gemini API.
func (s *Server) handleListEmbeddingModels(w http.ResponseWriter, r *http.Request) {
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store not available")
		return
	}

	creds := s.agents.Credentials()
	apiKey, err := creds.GetToken("gemini", "", "", "api_key")
	if err != nil || apiKey == "" {
		writeError(w, http.StatusBadRequest, "no_api_key", "Gemini API key not configured")
		return
	}

	models, err := fetchGeminiEmbeddingModels(r.Context(), apiKey)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}

	writeJSONResponse(w, http.StatusOK, map[string]any{"models": models})
}

// maxListModelsErrorBody caps how much of an error response we read back to
// avoid a malicious/misbehaving upstream exhausting memory.
const maxListModelsErrorBody = 16 * 1024

// fetchGeminiEmbeddingModels calls the Gemini ListModels API and returns
// model names that support embedContent.
func fetchGeminiEmbeddingModels(ctx context.Context, apiKey string) ([]string, error) {
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models?key=%s&pageSize=100", apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := geminiListHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxListModelsErrorBody))
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Models []struct {
			Name                     string   `json:"name"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode error: %w", err)
	}

	var models []string
	for _, m := range result.Models {
		for _, method := range m.SupportedGenerationMethods {
			if method == "embedContent" {
				// Strip "models/" prefix
				name := m.Name
				if strings.HasPrefix(name, "models/") {
					name = name[len("models/"):]
				}
				models = append(models, name)
				break
			}
		}
	}

	sort.Strings(models)
	return models, nil
}

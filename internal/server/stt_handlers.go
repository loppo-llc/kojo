package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
)

// xaiAPIBase is the xAI REST API base. Overridable in tests so we can
// point the ephemeral-token mint call at an httptest stub instead of the
// live api.x.ai.
var xaiAPIBase = "https://api.x.ai"

// sttWSBaseURL is handed to the browser so it can open the streaming STT
// WebSocket directly (authenticated with the ephemeral token, never the
// long-lived API key). Kept in sync with the docs endpoint.
const sttWSBaseURL = "wss://api.x.ai/v1/stt"

// sttEphemeralSeconds is how long a minted token stays valid. Short by
// design — the browser opens the WS immediately after minting.
const sttEphemeralSeconds = 300

// sttMintHTTPClient bounds the upstream mint call so a stuck xAI endpoint
// can't tie up server goroutines.
var sttMintHTTPClient = &http.Client{Timeout: 15 * time.Second}

// STT token minting is rate-limited per-process: minting is cheap but a
// runaway client (or a mic button stuck in a retry loop) shouldn't be able
// to hammer the xAI billing endpoint. A small token bucket refilled on a
// ticker is plenty for interactive push-to-talk use.
var (
	sttRateOnce   sync.Once
	sttRateBucket chan struct{}
)

func sttAcquireRateSlot() bool {
	sttRateOnce.Do(func() {
		// Burst of 5, refilled 1/sec.
		sttRateBucket = make(chan struct{}, 5)
		for i := 0; i < cap(sttRateBucket); i++ {
			sttRateBucket <- struct{}{}
		}
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for range t.C {
				select {
				case sttRateBucket <- struct{}{}:
				default:
				}
			}
		}()
	})
	select {
	case <-sttRateBucket:
		return true
	default:
		return false
	}
}

// STT provider selection. The owner picks one global speech-to-text backend
// in Settings; the chat composer hides its mic button when the effective
// provider is off. Stored in the credential store's settings table.
const (
	sttProviderSetting = "stt_provider"

	sttProviderOff    = "off"
	sttProviderXAI    = "xai"
	sttProviderGemini = "gemini"
)

// geminiAPIBase is the Gemini REST API base. Overridable in tests so the
// auth_tokens mint call can hit an httptest stub.
var geminiAPIBase = "https://generativelanguage.googleapis.com"

// geminiSTTModel is the Live API transcription model the browser connects to.
const geminiSTTModel = "gemini-3.5-transcribe-live"

// geminiSTTWSBaseURL is the Live API WebSocket for ephemeral-token clients.
// Tokens authenticate only against the Constrained method (passed as the
// access_token query parameter); the plain BidiGenerateContent method takes
// a long-lived API key instead.
const geminiSTTWSBaseURL = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContentConstrained"

// geminiSTTSessionTTL bounds how long the minted token can carry messages.
// A Live session is capped at 10 minutes server-side, so a little more than
// that is enough; geminiSTTNewSessionTTL is the window to open the socket.
const (
	geminiSTTSessionTTL    = 11 * time.Minute
	geminiSTTNewSessionTTL = time.Minute
)

// sttLanguageRe accepts BCP-47-ish tags ("ja", "ja-JP", "zh-Hant-TW").
var sttLanguageRe = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8}){0,3}$`)

// sttAvailability reports which STT providers have a usable API key
// (credential store or the env/file fallbacks the loaders honour).
type sttAvailability struct {
	XAI    bool `json:"xai"`
	Gemini bool `json:"gemini"`
}

func (s *Server) sttAvailability() sttAvailability {
	creds := s.agents.Credentials()
	var av sttAvailability
	if k, err := agent.LoadXAIAPIKey(creds); err == nil && strings.TrimSpace(k) != "" {
		av.XAI = true
	}
	if k, err := agent.LoadGeminiAPIKey(creds); err == nil && strings.TrimSpace(k) != "" {
		av.Gemini = true
	}
	return av
}

// storedSTTProvider returns the persisted selection, or "" when unset or
// unrecognised.
func (s *Server) storedSTTProvider() string {
	creds := s.agents.Credentials()
	if creds == nil {
		return ""
	}
	switch v := creds.GetSetting(sttProviderSetting); v {
	case sttProviderOff, sttProviderXAI, sttProviderGemini:
		return v
	}
	return ""
}

// resolveSTTProvider maps the stored selection to the provider actually in
// effect. Unset keeps the historical default (xAI). A provider whose key has
// since disappeared resolves to off, so the mic never shows for a backend
// that can't mint a token.
func resolveSTTProvider(stored string, av sttAvailability) string {
	if stored == "" {
		stored = sttProviderXAI
	}
	switch stored {
	case sttProviderXAI:
		if av.XAI {
			return sttProviderXAI
		}
	case sttProviderGemini:
		if av.Gemini {
			return sttProviderGemini
		}
	}
	return sttProviderOff
}

// sttConfigResponse is the GET/PUT /api/v1/stt/config payload.
type sttConfigResponse struct {
	// Provider is the effective provider: "off", "xai" or "gemini".
	Provider  string          `json:"provider"`
	Available sttAvailability `json:"available"`
}

func (s *Server) sttConfig() sttConfigResponse {
	av := s.sttAvailability()
	return sttConfigResponse{
		Provider:  resolveSTTProvider(s.storedSTTProvider(), av),
		Available: av,
	}
}

// handleGetSTTConfig returns the effective STT provider and which providers
// have an API key. Owner-only via the default /api/v1 policy.
func (s *Server) handleGetSTTConfig(w http.ResponseWriter, r *http.Request) {
	if s.agents == nil {
		writeError(w, http.StatusNotFound, "not_found", "agents not enabled")
		return
	}
	writeJSONResponse(w, http.StatusOK, s.sttConfig())
}

// handleSetSTTConfig stores the STT provider selection. Selecting a provider
// without an API key is refused so the setting can't point at a dead backend.
func (s *Server) handleSetSTTConfig(w http.ResponseWriter, r *http.Request) {
	if s.agents == nil {
		writeError(w, http.StatusNotFound, "not_found", "agents not enabled")
		return
	}
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store not available")
		return
	}
	var req struct {
		Provider string `json:"provider"`
	}
	if !readCappedJSON(w, r, 4<<10, "request body too large", "invalid JSON", &req) {
		return
	}
	switch req.Provider {
	case sttProviderOff:
	case sttProviderXAI, sttProviderGemini:
		av := s.sttAvailability()
		if (req.Provider == sttProviderXAI && !av.XAI) || (req.Provider == sttProviderGemini && !av.Gemini) {
			writeError(w, http.StatusBadRequest, "no_api_key", req.Provider+" API key not configured")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "provider must be one of off, xai, gemini")
		return
	}
	if err := s.agents.Credentials().SetSetting(sttProviderSetting, req.Provider); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to save: "+err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, s.sttConfig())
}

// sttTokenResponse is what the frontend needs to open the streaming STT
// WebSocket: which provider it talks to, the ephemeral token, when it
// expires, and the WS base URL (plus the model for Gemini's setup message).
type sttTokenResponse struct {
	Provider  string `json:"provider"`
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expiresAt"` // unix seconds
	WSBaseURL string `json:"wsBaseUrl"`
	Model     string `json:"model,omitempty"`
	// Language is the BCP-47 code locked into a Gemini token ("" = auto).
	Language string `json:"language,omitempty"`
}

// handleSTTToken mints a short-lived ephemeral token for the effective STT
// provider so the browser can authenticate the streaming Speech-to-Text
// WebSocket without ever seeing the long-lived API key. Owner-only (same
// trust level as TTS preview).
//
// Optional body: {"language":"ja-JP"} — used for Gemini, whose token locks
// the session config (including the transcription language).
func (s *Server) handleSTTToken(w http.ResponseWriter, r *http.Request) {
	if s.agents == nil {
		writeError(w, http.StatusNotFound, "not_found", "agents not enabled")
		return
	}
	p := auth.FromContext(r.Context())
	if !p.CanForkOrCreate() {
		writeError(w, http.StatusForbidden, "forbidden", "not allowed")
		return
	}
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store not available")
		return
	}

	var req struct {
		Language string `json:"language"`
	}
	if r.Body != nil {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
			return
		}
		if len(bytes.TrimSpace(body)) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
				return
			}
		}
	}
	req.Language = strings.TrimSpace(req.Language)
	if req.Language != "" && !sttLanguageRe.MatchString(req.Language) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid language")
		return
	}

	creds := s.agents.Credentials()
	stored := s.storedSTTProvider()
	provider := resolveSTTProvider(stored, s.sttAvailability())
	if provider == sttProviderOff {
		if stored == sttProviderOff {
			writeError(w, http.StatusConflict, "stt_disabled", "voice input is disabled")
			return
		}
		// Distinct code so the frontend can point the user at Settings.
		writeError(w, http.StatusBadRequest, "no_api_key", "speech-to-text API key not configured")
		return
	}

	if !sttAcquireRateSlot() {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many token requests")
		return
	}

	switch provider {
	case sttProviderGemini:
		apiKey, err := agent.LoadGeminiAPIKey(creds)
		if err != nil || apiKey == "" {
			writeError(w, http.StatusBadRequest, "no_api_key", "Gemini API key not configured")
			return
		}
		token, expiresAt, err := mintGeminiSTTToken(r.Context(), apiKey, req.Language, time.Now())
		if err != nil {
			s.logger.Warn("gemini stt ephemeral token mint failed", "err", err)
			writeError(w, http.StatusBadGateway, "stt_mint_failed", err.Error())
			return
		}
		writeJSONResponse(w, http.StatusOK, sttTokenResponse{
			Provider:  sttProviderGemini,
			Token:     token,
			ExpiresAt: expiresAt,
			WSBaseURL: geminiSTTWSBaseURL,
			Model:     geminiSTTModel,
			Language:  req.Language,
		})
	default:
		apiKey, err := agent.LoadXAIAPIKey(creds)
		if err != nil || apiKey == "" {
			writeError(w, http.StatusBadRequest, "no_api_key", "xAI API key not configured")
			return
		}
		token, expiresAt, err := mintXAIEphemeralToken(r.Context(), apiKey)
		if err != nil {
			s.logger.Warn("stt ephemeral token mint failed", "err", err)
			writeError(w, http.StatusBadGateway, "stt_mint_failed", err.Error())
			return
		}
		writeJSONResponse(w, http.StatusOK, sttTokenResponse{
			Provider:  sttProviderXAI,
			Token:     token,
			ExpiresAt: expiresAt,
			WSBaseURL: sttWSBaseURL,
		})
	}
}

// geminiSTTSetup is the Live API session config locked into the token. The
// browser sends the same config in its setup message; locking it server-side
// means a leaked token can only be used for this transcription session shape.
// Push-to-talk-style toggle: automatic VAD stays on so long dictation is
// committed utterance by utterance, and stop sends audioStreamEnd to flush.
func geminiSTTSetup(language string) map[string]any {
	transcription := map[string]any{}
	if language != "" {
		transcription["languageCodes"] = []string{language}
	}
	return map[string]any{
		"model": "models/" + geminiSTTModel,
		"generationConfig": map[string]any{
			"responseModalities": []string{"TEXT"},
		},
		"inputAudioTranscription": transcription,
	}
}

// mintGeminiSTTToken calls the Gemini auth_tokens endpoint and returns the
// single-use ephemeral token name plus its unix expiry.
func mintGeminiSTTToken(ctx context.Context, apiKey, language string, now time.Time) (string, int64, error) {
	expire := now.Add(geminiSTTSessionTTL).UTC()
	body, _ := json.Marshal(map[string]any{
		"uses":                     1,
		"expireTime":               expire.Format(time.RFC3339),
		"newSessionExpireTime":     now.Add(geminiSTTNewSessionTTL).UTC().Format(time.RFC3339),
		"bidiGenerateContentSetup": geminiSTTSetup(language),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, geminiAPIBase+"/v1beta/auth_tokens", bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("x-goog-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := sttMintHTTPClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxSTTMintErrorBody))
		return "", 0, fmt.Errorf("Gemini API error %d: %s", resp.StatusCode, b)
	}

	// Response shape: {"name":"auth_tokens/..."}
	var out struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, fmt.Errorf("decode response: %w", err)
	}
	if out.Name == "" {
		return "", 0, fmt.Errorf("Gemini returned empty token")
	}
	return out.Name, expire.Unix(), nil
}

// maxSTTMintErrorBody caps how much of an upstream error response we read
// back so a misbehaving endpoint can't exhaust memory.
const maxSTTMintErrorBody = 8 * 1024

// mintXAIEphemeralToken calls xAI's client_secrets endpoint and returns the
// ephemeral token value plus its unix expiry.
func mintXAIEphemeralToken(ctx context.Context, apiKey string) (string, int64, error) {
	body, _ := json.Marshal(map[string]any{
		"expires_after": map[string]any{"seconds": sttEphemeralSeconds},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, xaiAPIBase+"/v1/realtime/client_secrets", bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := sttMintHTTPClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxSTTMintErrorBody))
		return "", 0, fmt.Errorf("xAI API error %d: %s", resp.StatusCode, b)
	}

	// Response shape: {"value":"xai-realtime...","expires_at":<unix seconds>}
	var out struct {
		Value     string `json:"value"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, fmt.Errorf("decode response: %w", err)
	}
	if out.Value == "" {
		return "", 0, fmt.Errorf("xAI returned empty token")
	}
	return out.Value, out.ExpiresAt, nil
}

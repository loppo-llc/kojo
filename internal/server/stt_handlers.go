package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// sttTokenResponse is what the frontend needs to open the streaming STT
// WebSocket: the ephemeral token, when it expires, and the WS base URL.
type sttTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expiresAt"` // unix seconds
	WSBaseURL string `json:"wsBaseUrl"`
}

// handleSTTToken mints a short-lived xAI ephemeral token so the browser can
// authenticate the streaming Speech-to-Text WebSocket without ever seeing
// the long-lived API key. Owner-only (same trust level as TTS preview).
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

	apiKey, err := agent.LoadXAIAPIKey(s.agents.Credentials())
	if err != nil || apiKey == "" {
		// Distinct code so the frontend can point the user at Settings.
		writeError(w, http.StatusBadRequest, "no_api_key", "xAI API key not configured")
		return
	}

	if !sttAcquireRateSlot() {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many token requests")
		return
	}

	token, expiresAt, err := mintXAIEphemeralToken(r.Context(), apiKey)
	if err != nil {
		s.logger.Warn("stt ephemeral token mint failed", "err", err)
		writeError(w, http.StatusBadGateway, "stt_mint_failed", err.Error())
		return
	}

	writeJSONResponse(w, http.StatusOK, sttTokenResponse{
		Token:     token,
		ExpiresAt: expiresAt,
		WSBaseURL: sttWSBaseURL,
	})
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

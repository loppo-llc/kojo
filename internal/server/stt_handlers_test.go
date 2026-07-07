package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
)

func newSTTTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", "")
	t.Setenv("XAI_API_KEY", "")

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	mgr, err := agent.NewManager(logger)
	if err != nil {
		t.Fatalf("agent.NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	return &Server{agents: mgr, logger: logger}
}

// TestHandleSTTToken_NoKey verifies a 400 no_api_key when no xAI key is
// configured anywhere (store empty, env empty, fallback file absent).
func TestHandleSTTToken_NoKey(t *testing.T) {
	srv := newSTTTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/stt/token", nil)
	req = authedRequest(req, auth.Principal{Role: auth.RoleOwner})
	rr := httptest.NewRecorder()
	srv.handleSTTToken(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body.Error.Code != "no_api_key" {
		t.Fatalf("code = %q, want no_api_key (body %s)", body.Error.Code, rr.Body.String())
	}
}

// TestHandleSTTToken_Forbidden verifies non-owners are refused.
func TestHandleSTTToken_Forbidden(t *testing.T) {
	srv := newSTTTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/stt/token", nil)
	req = authedRequest(req, auth.Principal{Role: auth.RoleAgent, AgentID: "x"})
	rr := httptest.NewRecorder()
	srv.handleSTTToken(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

// TestHandleSTTToken_Mint stubs api.x.ai and verifies the handler forwards
// the API key, posts the expected body, and returns the minted token.
func TestHandleSTTToken_Mint(t *testing.T) {
	srv := newSTTTestServer(t)

	// Configure a fake xAI key in the encrypted store.
	if err := srv.agents.Credentials().SetToken("xai", "", "", "api_key", "xai-fake-key", time.Time{}); err != nil {
		t.Fatalf("SetToken: %v", err)
	}

	var gotAuth string
	var gotBody map[string]any
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/realtime/client_secrets" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":"xai-realtime-ephemeral","expires_at":1783433674}`))
	}))
	defer stub.Close()

	old := xaiAPIBase
	xaiAPIBase = stub.URL
	defer func() { xaiAPIBase = old }()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/stt/token", nil)
	req = authedRequest(req, auth.Principal{Role: auth.RoleOwner})
	rr := httptest.NewRecorder()
	srv.handleSTTToken(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if gotAuth != "Bearer xai-fake-key" {
		t.Fatalf("upstream Authorization = %q", gotAuth)
	}
	if gotBody["expires_after"] == nil {
		t.Fatalf("upstream body missing expires_after: %v", gotBody)
	}
	var resp sttTokenResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp.Token != "xai-realtime-ephemeral" {
		t.Fatalf("token = %q", resp.Token)
	}
	if resp.ExpiresAt != 1783433674 {
		t.Fatalf("expiresAt = %d", resp.ExpiresAt)
	}
	if resp.WSBaseURL != sttWSBaseURL {
		t.Fatalf("wsBaseUrl = %q", resp.WSBaseURL)
	}
}

package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
	for _, k := range []string{"KOJO_GEMINI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY"} {
		t.Setenv(k, "")
	}

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
	if resp.Provider != sttProviderXAI || resp.WSBaseURL != sttWSBaseURL {
		t.Fatalf("wsBaseUrl = %q", resp.WSBaseURL)
	}
}

func sttErrCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return body.Error.Code
}

func doSTT(t *testing.T, srv *Server, h http.HandlerFunc, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/api/v1/stt/x", rd)
	req = authedRequest(req, auth.Principal{Role: auth.RoleOwner})
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func TestResolveSTTProvider(t *testing.T) {
	both := sttAvailability{XAI: true, Gemini: true}
	cases := []struct {
		stored string
		av     sttAvailability
		want   string
	}{
		{"", both, sttProviderXAI},
		{"", sttAvailability{Gemini: true}, sttProviderOff},
		{sttProviderOff, both, sttProviderOff},
		{sttProviderGemini, both, sttProviderGemini},
		{sttProviderGemini, sttAvailability{XAI: true}, sttProviderOff},
		{sttProviderXAI, sttAvailability{Gemini: true}, sttProviderOff},
	}
	for _, c := range cases {
		if got := resolveSTTProvider(c.stored, c.av); got != c.want {
			t.Errorf("resolve(%q, %+v) = %q, want %q", c.stored, c.av, got, c.want)
		}
	}
}

// TestSTTConfig covers GET/PUT: selecting a provider requires its key,
// off is always allowed, and removing a key resolves the provider to off.
func TestSTTConfig(t *testing.T) {
	srv := newSTTTestServer(t)
	creds := srv.agents.Credentials()

	rr := doSTT(t, srv, srv.handleGetSTTConfig, http.MethodGet, "")
	var cfg sttConfigResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &cfg)
	if rr.Code != http.StatusOK || cfg.Provider != sttProviderOff || cfg.Available.XAI || cfg.Available.Gemini {
		t.Fatalf("initial config = %d %s", rr.Code, rr.Body.String())
	}

	rr = doSTT(t, srv, srv.handleSetSTTConfig, http.MethodPut, `{"provider":"gemini"}`)
	if rr.Code != http.StatusBadRequest || sttErrCode(t, rr) != "no_api_key" {
		t.Fatalf("PUT gemini without key = %d %s", rr.Code, rr.Body.String())
	}
	rr = doSTT(t, srv, srv.handleSetSTTConfig, http.MethodPut, `{"provider":"bogus"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("PUT bogus = %d %s", rr.Code, rr.Body.String())
	}

	if err := creds.SetToken("gemini", "", "", "api_key", "g-key", time.Time{}); err != nil {
		t.Fatal(err)
	}
	rr = doSTT(t, srv, srv.handleSetSTTConfig, http.MethodPut, `{"provider":"gemini"}`)
	_ = json.Unmarshal(rr.Body.Bytes(), &cfg)
	if rr.Code != http.StatusOK || cfg.Provider != sttProviderGemini || !cfg.Available.Gemini {
		t.Fatalf("PUT gemini = %d %s", rr.Code, rr.Body.String())
	}

	if err := creds.DeleteToken("gemini", "", "", "api_key"); err != nil {
		t.Fatal(err)
	}
	rr = doSTT(t, srv, srv.handleGetSTTConfig, http.MethodGet, "")
	_ = json.Unmarshal(rr.Body.Bytes(), &cfg)
	if cfg.Provider != sttProviderOff {
		t.Fatalf("after key removal provider = %q", cfg.Provider)
	}

	rr = doSTT(t, srv, srv.handleSetSTTConfig, http.MethodPut, `{"provider":"off"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT off = %d %s", rr.Code, rr.Body.String())
	}
}

// TestHandleSTTToken_Disabled verifies an explicit off selection refuses to
// mint even when a key exists.
func TestHandleSTTToken_Disabled(t *testing.T) {
	srv := newSTTTestServer(t)
	creds := srv.agents.Credentials()
	if err := creds.SetToken("xai", "", "", "api_key", "xai-fake-key", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := creds.SetSetting(sttProviderSetting, sttProviderOff); err != nil {
		t.Fatal(err)
	}
	rr := doSTT(t, srv, srv.handleSTTToken, http.MethodPost, "")
	if rr.Code != http.StatusConflict || sttErrCode(t, rr) != "stt_disabled" {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

// TestHandleSTTToken_Gemini stubs the Gemini auth_tokens endpoint and checks
// the key header, the locked setup, and the returned connection info.
func TestHandleSTTToken_Gemini(t *testing.T) {
	srv := newSTTTestServer(t)
	creds := srv.agents.Credentials()
	if err := creds.SetToken("gemini", "", "", "api_key", "g-fake-key", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := creds.SetSetting(sttProviderSetting, sttProviderGemini); err != nil {
		t.Fatal(err)
	}

	var gotKey string
	var gotBody map[string]any
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/auth_tokens" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotKey = r.Header.Get("x-goog-api-key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"auth_tokens/abc"}`))
	}))
	defer stub.Close()
	old := geminiAPIBase
	geminiAPIBase = stub.URL
	defer func() { geminiAPIBase = old }()

	before := time.Now()
	rr := doSTT(t, srv, srv.handleSTTToken, http.MethodPost, `{"language":"ja-JP"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if gotKey != "g-fake-key" {
		t.Fatalf("upstream key = %q", gotKey)
	}
	if gotBody["uses"] != float64(1) || gotBody["expireTime"] == nil || gotBody["newSessionExpireTime"] == nil {
		t.Fatalf("upstream body = %v", gotBody)
	}
	setup, _ := gotBody["bidiGenerateContentSetup"].(map[string]any)
	if setup["model"] != "models/"+geminiSTTModel {
		t.Fatalf("setup = %v", setup)
	}
	tr, _ := setup["inputAudioTranscription"].(map[string]any)
	if codes, _ := tr["languageCodes"].([]any); len(codes) != 1 || codes[0] != "ja-JP" {
		t.Fatalf("inputAudioTranscription = %v", tr)
	}
	var resp sttTokenResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Provider != sttProviderGemini || resp.Token != "auth_tokens/abc" || resp.WSBaseURL != geminiSTTWSBaseURL || resp.Model != geminiSTTModel || resp.Language != "ja-JP" {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.ExpiresAt < before.Add(geminiSTTSessionTTL).Unix()-1 {
		t.Fatalf("expiresAt = %d", resp.ExpiresAt)
	}

	rr = doSTT(t, srv, srv.handleSTTToken, http.MethodPost, `{"language":"ja JP; x"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad language status = %d", rr.Code)
	}
}

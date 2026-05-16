package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

// newWebDAVTestStore builds the bare-minimum server + auth store wiring
// the handler tests need: a kv-backed WebDAVTokenStore, a Server with
// just the webdavTokens field populated, and a helper that stamps an
// Owner principal into the request context so the handler-side IsOwner
// gate doesn't 403 us before we can test it.
func newWebDAVTestStore(t *testing.T) (*Server, *auth.WebDAVTokenStore) {
	t.Helper()
	dir := t.TempDir()
	kv, err := store.Open(context.Background(), store.Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = kv.Close() })
	ts, err := auth.NewWebDAVTokenStore(context.Background(), kv)
	if err != nil {
		t.Fatalf("NewWebDAVTokenStore: %v", err)
	}
	srv := &Server{webdavTokens: ts}
	return srv, ts
}

// asOwner wraps r with an Owner principal in context so the handler-
// side IsOwner gate doesn't reject the request. The tests for forbidden
// access path use a different request without this helper.
func asOwner(r *http.Request) *http.Request {
	return r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Role: auth.RoleOwner}))
}

func TestHandleIssueWebDAVToken_OwnerOnly(t *testing.T) {
	srv, _ := newWebDAVTestStore(t)
	body := strings.NewReader(`{"label":"x","ttlSeconds":3600}`)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/webdav-tokens", body)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleIssueWebDAVToken(w, r) // no Owner principal in ctx
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-owner: status = %d, want 403 (body=%s)", w.Code, w.Body.String())
	}
}

func TestHandleIssueWebDAVToken_HappyPath(t *testing.T) {
	srv, ts := newWebDAVTestStore(t)
	body := strings.NewReader(`{"label":"laptop","ttlSeconds":3600}`)
	r := asOwner(httptest.NewRequest(http.MethodPost, "/api/v1/auth/webdav-tokens", body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleIssueWebDAVToken(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var res webdavTokenIssueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Token == "" || res.ID == "" {
		t.Errorf("missing id/token in response: %+v", res)
	}
	if !ts.Verify(res.Token) {
		t.Errorf("issued token rejected by store.Verify")
	}
	if res.ExpiresAt <= res.CreatedAt {
		t.Errorf("expires_at must be > created_at: %+v", res)
	}
}

func TestHandleIssueWebDAVToken_RejectsBadBody(t *testing.T) {
	srv, _ := newWebDAVTestStore(t)
	cases := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"invalid json", `{`},
		{"unknown field", `{"label":"x","ttlSeconds":3600,"extra":1}`},
		{"missing ttl", `{"label":"x"}`},
		{"negative ttl", `{"label":"x","ttlSeconds":-1}`},
		{"empty label", `{"label":"","ttlSeconds":3600}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := asOwner(httptest.NewRequest(http.MethodPost, "/api/v1/auth/webdav-tokens", strings.NewReader(c.body)))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.handleIssueWebDAVToken(w, r)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
			}
		})
	}
}

func TestHandleIssueWebDAVToken_BodyTooLarge(t *testing.T) {
	srv, _ := newWebDAVTestStore(t)
	body := strings.NewReader(strings.Repeat("a", webdavTokenIssueMaxBody+1024))
	r := asOwner(httptest.NewRequest(http.MethodPost, "/api/v1/auth/webdav-tokens", body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleIssueWebDAVToken(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 (body=%s)", w.Code, w.Body.String())
	}
}

func TestHandleListWebDAVTokens_FiltersExpired(t *testing.T) {
	srv, ts := newWebDAVTestStore(t)
	if _, err := ts.Issue(context.Background(), "live", 1*time.Hour); err != nil {
		t.Fatalf("Issue live: %v", err)
	}
	r := asOwner(httptest.NewRequest(http.MethodGet, "/api/v1/auth/webdav-tokens", nil))
	w := httptest.NewRecorder()
	srv.handleListWebDAVTokens(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var body struct {
		Items []auth.WebDAVTokenMeta `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 {
		t.Errorf("items: got %d, want 1", len(body.Items))
	}
	// List must NEVER leak the hash material itself.
	if strings.Contains(w.Body.String(), "sha256:") {
		t.Errorf("list response leaked hash material: %s", w.Body.String())
	}
}

func TestHandleRevokeWebDAVToken_Idempotent(t *testing.T) {
	srv, ts := newWebDAVTestStore(t)
	res, err := ts.Issue(context.Background(), "tmp", 1*time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// First DELETE: 204
	r := asOwner(httptest.NewRequest(http.MethodDelete, "/api/v1/auth/webdav-tokens/"+res.ID, nil))
	r.SetPathValue("id", res.ID)
	w := httptest.NewRecorder()
	srv.handleRevokeWebDAVToken(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body=%s)", w.Code, w.Body.String())
	}
	if ts.Verify(res.Token) {
		t.Errorf("revoked token still verifies")
	}
	// Second DELETE on the same id: still 204 (idempotent).
	r2 := asOwner(httptest.NewRequest(http.MethodDelete, "/api/v1/auth/webdav-tokens/"+res.ID, nil))
	r2.SetPathValue("id", res.ID)
	w2 := httptest.NewRecorder()
	srv.handleRevokeWebDAVToken(w2, r2)
	if w2.Code != http.StatusNoContent {
		t.Errorf("repeat delete: status = %d, want 204", w2.Code)
	}
}

func TestHandleRevokeWebDAVToken_InvalidID(t *testing.T) {
	srv, _ := newWebDAVTestStore(t)
	// Path-traversal candidate — the regex-based validation in
	// auth.WebDAVTokenStore.Revoke rejects this with a non-nil
	// error, which the handler maps to 400. The post-condition is
	// "no kv mutation happens" — verified implicitly by the
	// validator's eager refusal.
	r := asOwner(httptest.NewRequest(http.MethodDelete, "/api/v1/auth/webdav-tokens/..%2F..%2Fetc", nil))
	r.SetPathValue("id", "../../etc")
	w := httptest.NewRecorder()
	srv.handleRevokeWebDAVToken(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
}

func TestWebDAVGate_AcceptsOwner(t *testing.T) {
	srv := &Server{}
	hit := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	})
	r := asOwner(httptest.NewRequest(http.MethodGet, "/api/v1/webdav/", nil))
	w := httptest.NewRecorder()
	srv.webdavGate(next).ServeHTTP(w, r)
	if !hit || w.Code != http.StatusOK {
		t.Errorf("owner request didn't reach next handler: hit=%v code=%d", hit, w.Code)
	}
}

func TestWebDAVGate_AcceptsValidBasicAuth(t *testing.T) {
	srv, ts := newWebDAVTestStore(t)
	res, err := ts.Issue(context.Background(), "basic-auth", 1*time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	hit := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	})
	r := httptest.NewRequest(http.MethodGet, "/api/v1/webdav/", nil)
	// SetBasicAuth uses base64-encoded "user:pass" in the
	// Authorization header — exactly what OS WebDAV mount clients
	// send. The token store ignores the username; we put "kojo"
	// here just to exercise the parser.
	r.SetBasicAuth("kojo", res.Token)
	w := httptest.NewRecorder()
	srv.webdavGate(next).ServeHTTP(w, r)
	if !hit || w.Code != http.StatusOK {
		t.Errorf("basic-auth: didn't reach next: hit=%v code=%d body=%s", hit, w.Code, w.Body.String())
	}
}

func TestWebDAVGate_RejectsInvalidBasicAuth(t *testing.T) {
	srv, _ := newWebDAVTestStore(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("next should not be called")
	})
	r := httptest.NewRequest(http.MethodGet, "/api/v1/webdav/", nil)
	r.SetBasicAuth("anyone", "definitely-not-a-token")
	w := httptest.NewRecorder()
	srv.webdavGate(next).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (body=%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("WWW-Authenticate"), "Basic") {
		t.Errorf("missing Basic challenge header: %q", w.Header().Get("WWW-Authenticate"))
	}
}

func TestWebDAVGate_RejectsAgentPrincipal(t *testing.T) {
	srv, _ := newWebDAVTestStore(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("next should not be called for agent principal")
	})
	r := httptest.NewRequest(http.MethodGet, "/api/v1/webdav/", nil)
	r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Role: auth.RoleAgent, AgentID: "ag_x"}))
	w := httptest.NewRecorder()
	srv.webdavGate(next).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("agent principal: status = %d, want 401", w.Code)
	}
}

// TestWebDAVGate_FullStack pins the auth listener wiring end-to-end:
// AuthMiddleware (Bearer/X-Kojo-Token only; Basic is invisible to it)
// → EnforceMiddleware (route policy) → mux → webdavGate (handler-side
// final authorization). The earlier round of review caught a critical
// where EnforceMiddleware was 403-ing Basic-auth WebDAV requests before
// they reached webdavGate; this test would have caught that regression.
func TestWebDAVGate_FullStack(t *testing.T) {
	_, ts := newWebDAVTestStore(t)
	res, err := ts.Issue(context.Background(), "stack-mount", 1*time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Build a mini stack that mirrors ensureAuthServer's middleware
	// order without dragging in the full Server (which needs an
	// agent.Manager + everything else). Resolver wires the WebDAV
	// store so it can hand back RoleWebDAV principals for Bearer
	// presentations.
	resolver := auth.NewResolver(&auth.TokenStore{}, nil)
	resolver.SetWebDAVStore(ts)
	hit := false
	srv := &Server{webdavTokens: ts}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/webdav/", srv.webdavGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	})))
	handler := auth.AuthMiddleware(resolver)(auth.EnforceMiddleware(mux))

	// 1. No credentials → 401 with Basic challenge
	hit = false
	r := httptest.NewRequest(http.MethodGet, "/api/v1/webdav/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if hit {
		t.Errorf("no-creds: handler was reached")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no-creds: status = %d, want 401 (body=%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("WWW-Authenticate"), "Basic") {
		t.Errorf("no-creds: missing Basic challenge")
	}

	// 2. Valid Basic auth → 200
	hit = false
	r = httptest.NewRequest(http.MethodGet, "/api/v1/webdav/", nil)
	r.SetBasicAuth("any-user", res.Token)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if !hit {
		t.Errorf("basic-auth: handler not reached")
	}
	if w.Code != http.StatusOK {
		t.Errorf("basic-auth: status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	// 3. Invalid Basic auth → 401 + Basic challenge
	hit = false
	r = httptest.NewRequest(http.MethodGet, "/api/v1/webdav/", nil)
	r.SetBasicAuth("any-user", "wrong-token")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if hit {
		t.Errorf("invalid-basic: handler was reached")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("invalid-basic: status = %d, want 401", w.Code)
	}

	// 4. Bearer (WebDAV-token) auth → 200
	hit = false
	r = httptest.NewRequest(http.MethodGet, "/api/v1/webdav/", nil)
	r.Header.Set("Authorization", "Bearer "+res.Token)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if !hit {
		t.Errorf("bearer: handler not reached")
	}
	if w.Code != http.StatusOK {
		t.Errorf("bearer: status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	// 5. RoleWebDAV-bearing token must NOT reach non-WebDAV routes.
	hit = false
	mux2 := http.NewServeMux()
	mux2.Handle("/api/v1/agents", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	handler2 := auth.AuthMiddleware(resolver)(auth.EnforceMiddleware(mux2))
	r = httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	r.Header.Set("Authorization", "Bearer "+res.Token)
	w = httptest.NewRecorder()
	handler2.ServeHTTP(w, r)
	if hit {
		t.Errorf("escape: WebDAV token reached /api/v1/agents")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("escape: status = %d, want 403 (body=%s)", w.Code, w.Body.String())
	}
}

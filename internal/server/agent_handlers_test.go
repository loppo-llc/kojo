//go:build heavy_test

// These tests boot a full agent.Manager (cron scheduler, notify poller,
// SQLite credential store) per test and are gated behind the
// `heavy_test` build tag so a normal `go test ./...` does not OOM
// resource-constrained dev hosts. Run with:
//
//	go test -tags heavy_test ./internal/server/...

package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
)

// newTestServer builds a Server backed by a fresh tempdir-rooted
// configdir / agent manager. Returns the server and the agent manager
// so the caller can pre-populate agents.
//
// Isolation strategy: each test gets its own $HOME pointing to t.TempDir.
// configdir.Path() resolves via os.UserHomeDir() when no override is set,
// so HOME-redirection gives true per-test directories. Calling
// configdir.Set() would be one-shot (sync.Once) and would leak state
// across tests in the same package.
//
// The manager spawns background goroutines (cron, notify poller, slack
// hub-less here) that must be torn down via mgr.Shutdown() — registered
// via t.Cleanup so a failed assertion does not strand them.
func newTestServer(t *testing.T) (*Server, *agent.Manager) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// On macOS os.UserHomeDir prefers HOME but some platforms also read
	// USERPROFILE / XDG_CONFIG_HOME. Override the obvious ones too so a
	// future change to configdir does not silently re-share state.
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp+"/.config")

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	mgr := agent.NewManager(logger)
	t.Cleanup(func() { mgr.Shutdown() })

	srv := New(Config{
		Addr:           ":0",
		Logger:         logger,
		Version:        "test",
		AgentManager:   mgr,
		GroupDMManager: nil,
	})
	return srv, mgr
}

func mkReq(method, path string, body string, p auth.Principal) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	return r.WithContext(auth.WithPrincipal(context.Background(), p))
}

// extractFirstAgent decodes {agents: [...]} and returns the first
// element as a generic map for field inspection.
func extractFirstAgent(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var resp struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, body)
	}
	if len(resp.Agents) == 0 {
		t.Fatalf("expected at least 1 agent in response, got 0\nbody=%s", body)
	}
	return resp.Agents[0]
}

func TestListAgents_OwnerSeesPersona(t *testing.T) {
	srv, mgr := newTestServer(t)
	a, err := mgr.Create(agent.AgentConfig{Name: "alice", Persona: "very secret persona", Tool: "claude", Model: "sonnet"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_ = a

	req := mkReq(http.MethodGet, "/api/v1/agents", "", auth.Principal{Role: auth.RoleOwner})
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	got := extractFirstAgent(t, w.Body.Bytes())
	if got["persona"] != "very secret persona" {
		t.Errorf("owner should see persona, got %v", got["persona"])
	}
}

func TestListAgents_AgentSelfFullOthersDirectory(t *testing.T) {
	srv, mgr := newTestServer(t)
	alice, _ := mgr.Create(agent.AgentConfig{Name: "alice", Persona: "alice persona internal", Tool: "claude", Model: "sonnet"})
	bob, _ := mgr.Create(agent.AgentConfig{Name: "bob", Persona: "bob persona internal", Tool: "claude", Model: "sonnet"})

	// Force PublicProfile so directory view has something visible.
	if _, err := mgr.Update(bob.ID, agent.AgentUpdateConfig{
		PublicProfile:         strPtr("bob short"),
		PublicProfileOverride: boolPtr(true),
	}); err != nil {
		t.Fatalf("set bob profile: %v", err)
	}

	req := mkReq(http.MethodGet, "/api/v1/agents", "", auth.Principal{Role: auth.RoleAgent, AgentID: alice.ID})
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Agents) != 2 {
		t.Fatalf("want 2 agents, got %d", len(resp.Agents))
	}

	var aliceView, bobView map[string]any
	for _, ag := range resp.Agents {
		switch ag["id"] {
		case alice.ID:
			aliceView = ag
		case bob.ID:
			bobView = ag
		}
	}
	if aliceView["persona"] != "alice persona internal" {
		t.Errorf("alice should see her own persona, got %v", aliceView["persona"])
	}
	if _, hasPersona := bobView["persona"]; hasPersona {
		t.Errorf("alice must not see bob.persona; got %v", bobView["persona"])
	}
	if bobView["publicProfile"] != "bob short" {
		t.Errorf("alice should see bob.publicProfile, got %v", bobView["publicProfile"])
	}
}

func TestGetAgent_DirectoryViewForNonOwnerOnOthers(t *testing.T) {
	srv, mgr := newTestServer(t)
	bob, _ := mgr.Create(agent.AgentConfig{Name: "bob", Persona: "bob secret", Tool: "claude", Model: "sonnet"})

	req := mkReq(http.MethodGet, "/api/v1/agents/"+bob.ID, "",
		auth.Principal{Role: auth.RoleAgent, AgentID: "ag_other"})
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, has := got["persona"]; has {
		t.Fatalf("non-owner non-self must not see persona, got %v", got["persona"])
	}
	if got["id"] != bob.ID {
		t.Errorf("id field: %v", got["id"])
	}
}

func TestUpdateAgent_RejectsCrossSelfAndPrivilegedField(t *testing.T) {
	srv, mgr := newTestServer(t)
	alice, _ := mgr.Create(agent.AgentConfig{Name: "alice", Tool: "claude", Model: "sonnet"})
	bob, _ := mgr.Create(agent.AgentConfig{Name: "bob", Tool: "claude", Model: "sonnet"})

	// Alice trying to edit bob: forbidden.
	req := mkReq(http.MethodPatch, "/api/v1/agents/"+bob.ID, `{"name":"hacked"}`,
		auth.Principal{Role: auth.RoleAgent, AgentID: alice.ID})
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-edit: want 403, got %d (%s)", w.Code, w.Body.String())
	}

	// Alice trying to set Privileged=true on herself: forbidden.
	req = mkReq(http.MethodPatch, "/api/v1/agents/"+alice.ID, `{"name":"alice","privileged":true}`,
		auth.Principal{Role: auth.RoleAgent, AgentID: alice.ID})
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("self-privilege smuggle: want 403, got %d (%s)", w.Code, w.Body.String())
	}

	// Owner setting Privileged via dedicated endpoint: ok.
	req = mkReq(http.MethodPost, "/api/v1/agents/"+alice.ID+"/privilege", `{"privileged":true}`,
		auth.Principal{Role: auth.RoleOwner})
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("owner privilege: status %d (%s)", w.Code, w.Body.String())
	}
	if !mgr.IsPrivileged(alice.ID) {
		t.Fatalf("manager state: alice should be privileged after POST /privilege")
	}

	// Agent calling /privilege on self: forbidden.
	req = mkReq(http.MethodPost, "/api/v1/agents/"+alice.ID+"/privilege", `{"privileged":false}`,
		auth.Principal{Role: auth.RolePrivAgent, AgentID: alice.ID})
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("priv revoke self: want 403, got %d", w.Code)
	}
}

func TestDeleteAgent_PrivAgentCanDeleteOthers(t *testing.T) {
	srv, mgr := newTestServer(t)
	alice, _ := mgr.Create(agent.AgentConfig{Name: "alice", Tool: "claude", Model: "sonnet"})
	bob, _ := mgr.Create(agent.AgentConfig{Name: "bob", Tool: "claude", Model: "sonnet"})

	// Regular Agent alice -> delete bob: forbidden.
	req := mkReq(http.MethodDelete, "/api/v1/agents/"+bob.ID, "",
		auth.Principal{Role: auth.RoleAgent, AgentID: alice.ID})
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("regular agent cross-delete: want 403, got %d", w.Code)
	}
	if _, ok := mgr.Get(bob.ID); !ok {
		t.Fatal("bob should still exist after rejected delete")
	}

	// Privileged agent alice -> delete bob: ok.
	req = mkReq(http.MethodDelete, "/api/v1/agents/"+bob.ID, "",
		auth.Principal{Role: auth.RolePrivAgent, AgentID: alice.ID})
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("priv agent delete: status %d (%s)", w.Code, w.Body.String())
	}
	if _, ok := mgr.Get(bob.ID); ok {
		t.Fatal("bob should be gone after priv delete")
	}
}

func TestForkAgent_AgentForbidden(t *testing.T) {
	srv, mgr := newTestServer(t)
	alice, _ := mgr.Create(agent.AgentConfig{Name: "alice", Tool: "claude", Model: "sonnet"})

	// Even privileged agent cannot fork (would copy persona/memory).
	req := mkReq(http.MethodPost, "/api/v1/agents/"+alice.ID+"/fork", `{"name":"alice2"}`,
		auth.Principal{Role: auth.RolePrivAgent, AgentID: alice.ID})
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("priv fork: want 403, got %d", w.Code)
	}
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

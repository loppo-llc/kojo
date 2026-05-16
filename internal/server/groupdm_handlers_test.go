//go:build heavy_test

// Same heavy_test gating as agent_handlers_test.go — boots a full
// agent.Manager + GroupDMManager.

package server

import (
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

// newTestServerWithGroupDM mirrors newTestServer but also wires up a
// GroupDMManager so the /api/v1/groupdms routes are registered.
func newTestServerWithGroupDM(t *testing.T) (*Server, *agent.Manager) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp+"/.config")

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	mgr, err := agent.NewManager(logger)
	if err != nil {
		t.Fatalf("agent.NewManager: %v", err)
	}
	t.Cleanup(func() {
		mgr.Shutdown()
		_ = mgr.Close()
	})

	gdm := agent.NewGroupDMManager(mgr, logger)

	srv := New(Config{
		Addr:           ":0",
		Logger:         logger,
		Version:        "test",
		AgentManager:   mgr,
		GroupDMManager: gdm,
	})
	return srv, mgr
}

func TestCreateGroupDM_AgentMustBeMember(t *testing.T) {
	srv, mgr := newTestServerWithGroupDM(t)
	alice, _ := mgr.Create(agent.AgentConfig{Name: "alice", Tool: "claude", Model: "sonnet"})
	bob, _ := mgr.Create(agent.AgentConfig{Name: "bob", Tool: "claude", Model: "sonnet"})
	carol, _ := mgr.Create(agent.AgentConfig{Name: "carol", Tool: "claude", Model: "sonnet"})

	// alice is in memberIds → 200.
	body := `{"name":"a-b","memberIds":["` + alice.ID + `","` + bob.ID + `"]}`
	req := mkReq(http.MethodPost, "/api/v1/groupdms", body,
		auth.Principal{Role: auth.RoleAgent, AgentID: alice.ID})
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("alice creates with self in members: want 200, got %d (%s)", w.Code, w.Body.String())
	}

	// alice not in memberIds (bob+carol) → 403.
	body = `{"name":"b-c","memberIds":["` + bob.ID + `","` + carol.ID + `"]}`
	req = mkReq(http.MethodPost, "/api/v1/groupdms", body,
		auth.Principal{Role: auth.RoleAgent, AgentID: alice.ID})
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("alice not in members: want 403, got %d (%s)", w.Code, w.Body.String())
	}

	// Owner can create with any members.
	body = `{"name":"b-c","memberIds":["` + bob.ID + `","` + carol.ID + `"]}`
	req = mkReq(http.MethodPost, "/api/v1/groupdms", body,
		auth.Principal{Role: auth.RoleOwner})
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("owner cross-create: want 200, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestCreateGroupDM_NonOwnerErrorSanitized(t *testing.T) {
	srv, mgr := newTestServerWithGroupDM(t)
	alice, _ := mgr.Create(agent.AgentConfig{Name: "alice", Tool: "claude", Model: "sonnet"})

	// Caller in members but referencing a non-existent peer — manager.Create
	// will surface ErrAgentNotFound. The handler must sanitize that for
	// non-Owner callers so it can't be used to enumerate IDs.
	body := `{"name":"x","memberIds":["` + alice.ID + `","ag_does_not_exist"]}`
	req := mkReq(http.MethodPost, "/api/v1/groupdms", body,
		auth.Principal{Role: auth.RoleAgent, AgentID: alice.ID})
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("nonexistent peer: want 400, got %d", w.Code)
	}
	var resp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp.Error.Message, "invalid memberIds") {
		t.Fatalf("agent should see sanitized error, got %q", resp.Error.Message)
	}
	if strings.Contains(resp.Error.Message, "ag_does_not_exist") {
		t.Fatalf("agent error must not echo unknown id: %q", resp.Error.Message)
	}

	// Owner gets the raw error so they can debug.
	req = mkReq(http.MethodPost, "/api/v1/groupdms", body, auth.Principal{Role: auth.RoleOwner})
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("owner nonexistent peer: want 400, got %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp.Error.Message, "ag_does_not_exist") {
		t.Fatalf("owner should see raw error, got %q", resp.Error.Message)
	}
}

package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
)

func newRestartRequest(p auth.Principal) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/system/restart", nil)
	return authedRequest(r, p)
}

func TestSystemRestart_ForbiddenForRegularAgent(t *testing.T) {
	srv := &Server{logger: slog.Default()}
	srv.SetRestartTrigger(func() {})
	rr := httptest.NewRecorder()
	srv.handleSystemRestart(rr, newRestartRequest(auth.Principal{Role: auth.RoleAgent, AgentID: "ag_x"}))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestSystemRestart_UnsupportedWithoutTrigger(t *testing.T) {
	srv := &Server{logger: slog.Default()}
	rr := httptest.NewRecorder()
	srv.handleSystemRestart(rr, newRestartRequest(auth.Principal{Role: auth.RoleOwner}))
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rr.Code)
	}
}

func TestSystemRestart_PrivAgentTriggersAfterDrain(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv := newChunkedSyncTestServer(t)
	fired := make(chan struct{})
	srv.SetRestartTrigger(func() { close(fired) })

	rr := httptest.NewRecorder()
	srv.handleSystemRestart(rr, newRestartRequest(auth.Principal{Role: auth.RolePrivAgent, AgentID: "ag_x"}))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rr.Code, rr.Body.String())
	}
	var body map[string]any
	readJSONResponse(t, rr, &body)
	if body["status"] != "pending" {
		t.Fatalf("status field = %v, want pending", body["status"])
	}

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("trigger did not fire after idle drain")
	}

	// Second request while pending → already_pending, trigger not re-armed.
	rr2 := httptest.NewRecorder()
	srv.handleSystemRestart(rr2, newRestartRequest(auth.Principal{Role: auth.RoleOwner}))
	if rr2.Code != http.StatusAccepted {
		t.Fatalf("dup status = %d, want 202", rr2.Code)
	}
	var body2 map[string]any
	readJSONResponse(t, rr2, &body2)
	if body2["status"] != "already_pending" {
		t.Fatalf("dup status field = %v, want already_pending", body2["status"])
	}
}

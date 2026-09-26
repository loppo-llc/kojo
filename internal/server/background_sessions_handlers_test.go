package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
)

func backgroundSessionsMux(srv *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agents/{id}/background-sessions", srv.handleListBackgroundSessions)
	mux.HandleFunc("DELETE /api/v1/agents/{id}/background-sessions/{key}", srv.handleStopBackgroundSession)
	mux.HandleFunc("DELETE /api/v1/agents/{id}/background-sessions/{key}/tasks/{taskId}", srv.handleStopBackgroundTask)
	return mux
}

func doBackgroundSessions(t *testing.T, mux *http.ServeMux, method, path string, p auth.Principal) *httptest.ResponseRecorder {
	t.Helper()
	r := authedRequest(httptest.NewRequest(method, path, nil), p)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, r)
	return rr
}

func TestBackgroundSessionsHandlers(t *testing.T) {
	srv, self, other, _ := newAttentionTestServer(t)
	mux := backgroundSessionsMux(srv)
	selfP := auth.Principal{Role: auth.RoleAgent, AgentID: self}
	owner := auth.Principal{Role: auth.RoleOwner}

	for _, p := range []auth.Principal{selfP, owner} {
		rr := doBackgroundSessions(t, mux, http.MethodGet, "/api/v1/agents/"+self+"/background-sessions", p)
		if rr.Code != http.StatusOK {
			t.Fatalf("list as %v: status %d body %s", p.Role, rr.Code, rr.Body.String())
		}
		var snap agent.BackgroundSessionsSnapshot
		if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
			t.Fatal(err)
		}
		if snap.Sessions == nil || len(snap.Sessions) != 0 || snap.Cap != 50 || snap.Lingering != 0 {
			t.Fatalf("snapshot = %+v", snap)
		}
	}

	// Another agent cannot list or stop this agent's sessions.
	otherP := auth.Principal{Role: auth.RoleAgent, AgentID: other}
	if rr := doBackgroundSessions(t, mux, http.MethodGet, "/api/v1/agents/"+self+"/background-sessions", otherP); rr.Code != http.StatusForbidden {
		t.Fatalf("cross-agent list status = %d", rr.Code)
	}
	if rr := doBackgroundSessions(t, mux, http.MethodDelete, "/api/v1/agents/"+self+"/background-sessions/groupdm:gd_1", otherP); rr.Code != http.StatusForbidden {
		t.Fatalf("cross-agent stop status = %d", rr.Code)
	}

	// Unknown agent / session / task → 404. The key is URL-encoded (':').
	if rr := doBackgroundSessions(t, mux, http.MethodGet, "/api/v1/agents/ag_missing/background-sessions", owner); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown agent status = %d", rr.Code)
	}
	if rr := doBackgroundSessions(t, mux, http.MethodDelete, "/api/v1/agents/"+self+"/background-sessions/"+self+"%3Aslack%3AC1%3A1.0", selfP); rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "background session not found") {
		t.Fatalf("unknown session status = %d body %s", rr.Code, rr.Body.String())
	}
	if rr := doBackgroundSessions(t, mux, http.MethodDelete, "/api/v1/agents/"+self+"/background-sessions/groupdm%3Agd_1/tasks/t1", selfP); rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "background session not found") {
		t.Fatalf("unknown task status = %d body %s", rr.Code, rr.Body.String())
	}
}

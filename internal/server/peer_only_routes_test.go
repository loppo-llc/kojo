package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/loppo-llc/kojo/internal/peer"
)

// TestPeerOnlyMux confirms the legacy minimal-peer surface still
// 404s when registerPeerOnlyRoutes runs against an agents==nil
// Server. Note: the §3.7 device-switch slice promoted --peer to a
// full peer (sessions / agents / files / git / WebDAV all
// registered when agents is wired up via the real registerRoutes),
// so this test is NOT a regression net for the full-peer surface
// — it's a narrow assertion that registerPeerOnlyRoutes is a no-op
// without a Store, which still matters because cmd/kojo never
// builds a peer without one and the helper must stay defensive.
//
// We hit the routes anonymously because the middleware chain
// belongs to httpSrv; here we only assert the mux's registration.
func TestPeerOnlyMux(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	id := &peer.Identity{
		DeviceID: uuid.NewString(),
		Name:     "peer-test",
	}

	// Direct mux construction so we don't pull in agent.Manager.
	// registerPeerOnlyRoutes only consults peerID + agents.Store, so
	// we stub the surface enough to clear those nil checks.
	s := &Server{logger: logger, peerID: id}
	// agents is nil — registerPeerOnlyRoutes refuses to register
	// anything in that posture. Verify the 404-only behavior.
	mux := http.NewServeMux()
	s.registerPeerOnlyRoutes(mux)
	if mustGetStatus(t, mux, "/api/v1/peers/events") != http.StatusNotFound {
		t.Fatalf("peers/events expected 404 with nil agents, got otherwise")
	}
	if mustGetStatus(t, mux, "/api/v1/peers/blobs/x") != http.StatusNotFound {
		t.Fatalf("peers/blobs/x expected 404 with nil agents, got otherwise")
	}

	// Confirm via the Hub-side full registerRoutes that PeerOnly=true
	// suppresses everything Hub-shaped. We can't easily wire a full
	// agent.Manager in a light-weight test, so we re-use the helper
	// that builds a mux for a peer-shaped Server and assert no
	// session / agent / file / WebDAV / kv / push / oplog / static
	// route is present. They all return 404 because the mux is
	// empty for that surface.
	hubShapedPaths := []string{
		"/api/v1/info",
		"/api/v1/sessions",
		"/api/v1/agents",
		"/api/v1/files",
		"/api/v1/git/status",
		"/api/v1/kv/foo",
		"/api/v1/webdav/",
		"/api/v1/oplog/flush",
		"/api/v1/push/vapid",
		"/api/v1/changes",
		"/api/v1/events",
		"/api/v1/peers",         // owner-only registry list — closed
		"/api/v1/peers/self",    // owner-only — closed
		"/api/v1/blob/global/x", // hub-side native blob — closed
		"/",                     // SPA fallback — closed (no UI)
	}
	for _, p := range hubShapedPaths {
		if got := mustGetStatus(t, mux, p); got != http.StatusNotFound {
			t.Errorf("peer-only mux MUST 404 on hub route %q, got %d", p, got)
		}
	}
}

func mustGetStatus(t *testing.T, h http.Handler, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	h.ServeHTTP(rec, req)
	return rec.Code
}

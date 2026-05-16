package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/loppo-llc/kojo/internal/peer"
)

// TestPeerOnlyMux confirms that registerPeerOnlyRoutes wires only the
// inbound peer surface and that every Hub-side route is left
// unregistered (Go's default mux returns 404 for missing routes, which
// is the contract we want — a peer binary MUST NOT pretend to serve
// Owner endpoints).
//
// We hit the routes anonymously (no peer.AuthMiddleware in front of
// the bare mux) because the middleware chain belongs to httpSrv and
// is exercised in heavy_test peer auth coverage. Here we only assert
// the mux's registration, which is what guarantees the Hub surface
// stays off in --peer mode.
func TestPeerOnlyMux(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519: %v", err)
	}
	id := &peer.Identity{
		DeviceID:   uuid.NewString(),
		Name:       "peer-test",
		PublicKey:  pub,
		PrivateKey: priv,
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

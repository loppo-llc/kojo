package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// newAuthTsnetTestServer wires the minimum Server fields that
// ensureAuthTsnetServer reads (logger, mux, resolveNodeKey/
// currentSelfNodeKey via Server methods, unsafePeer flag, agents
// nil so the optional middlewares stay off). It also opens a temp
// Store and registers one peer row so TailnetIdentityMiddleware can
// resolve a NodeKey → RolePeer on Hit.
//
// The chain under test is the one ServeAuthTsnet wraps around the
// shared buildAuthHandler: TailnetIdentityMiddleware →
// AuthMiddleware → EnforceMiddleware → mux. The test mux registers a
// single bookkeeping handler at /api/v1/peers/agent-sync (the
// production RolePeer admit target) so we can assert what Principal
// the handler sees per case.
func newAuthTsnetTestServer(t *testing.T, nodeKey string, unsafe bool) (*Server, *store.Store, string, *auth.Principal) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		t.Fatalf("mkdir blobs: %v", err)
	}
	st, err := store.Open(context.Background(), store.Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const deviceID = "device-tsnet-test"
	if _, err := st.UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID: deviceID,
		Name:     "peerB",
		URL:      "http://100.64.0.3:8080",
		NodeKey:  nodeKey,
		Status:   store.PeerStatusOnline,
	}); err != nil {
		t.Fatalf("UpsertPeer: %v", err)
	}

	// Capture whatever Principal reaches the handler.
	captured := &auth.Principal{}
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/peers/agent-sync", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*captured = auth.FromContext(r.Context())
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	s := &Server{
		logger:        slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		mux:           mux,
		unsafePeer:    unsafe,
		pendingSyncDB: st, // ensureAuthTsnetServer's test-friendly Store fallback
		peerID:        &peer.Identity{DeviceID: "device-self-not-the-registered-one", Name: "self"},
	}
	// Stand in for cmd/kojo's late-bound resolver: any RemoteAddr maps
	// to the fixed nodeKey so the middleware doesn't have to know
	// about tsnet. The test cases supply different nodeKey values to
	// drive the Hit / Miss branches.
	s.SetNodeKeyResolver(func(ctx context.Context, addr string) (string, error) {
		return nodeKey, nil
	})
	return s, st, deviceID, captured
}

// TestServeAuthTsnet_PeerRegistryHitStampsRolePeer covers the
// production path: a paired peer's WhoIs resolves to a peer_registry
// row, TailnetIdentityMiddleware stamps RolePeer, AuthMiddleware's
// "skip when already non-Guest" branch keeps it, EnforceMiddleware
// admits /api/v1/peers/agent-sync via the RolePeer block in
// policy.AllowNonOwner, and the handler sees the principal.
func TestServeAuthTsnet_PeerRegistryHitStampsRolePeer(t *testing.T) {
	s, _, deviceID, got := newAuthTsnetTestServer(t, "nodekey:peerB", false)
	resolver := auth.NewResolver(nil, nil)
	srv := s.ensureAuthTsnetServer(resolver)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/peers/agent-sync", nil)
	req.RemoteAddr = "100.64.0.42:54321"
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (RolePeer admit); body=%s", rec.Code, rec.Body.String())
	}
	if got.Role != auth.RolePeer {
		t.Fatalf("role = %v, want RolePeer", got.Role)
	}
	if got.PeerID != deviceID {
		t.Fatalf("peer_id = %q, want %q", got.PeerID, deviceID)
	}
}

// TestServeAuthTsnet_UnpairedTailnetCallerNoBearerForbidden confirms
// the chain's default-deny: unknown NodeKey → TailnetIdentity falls
// through as Guest → AuthMiddleware finds no Bearer → Enforce 403s
// the privileged inter-peer route.
func TestServeAuthTsnet_UnpairedTailnetCallerNoBearerForbidden(t *testing.T) {
	// peer_registry has a row for nodekey:peerB, but the resolver
	// returns a different (unpaired) key.
	s, _, _, got := newAuthTsnetTestServer(t, "nodekey:peerB", false)
	s.SetNodeKeyResolver(func(ctx context.Context, addr string) (string, error) {
		return "nodekey:stranger", nil
	})
	resolver := auth.NewResolver(nil, nil)
	srv := s.ensureAuthTsnetServer(resolver)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/peers/agent-sync", nil)
	req.RemoteAddr = "100.64.0.99:54321"
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (unpaired tailnet caller → Guest → default-deny)", rec.Code)
	}
	if got.Role == auth.RolePeer {
		t.Fatalf("handler should not have been reached; principal was RolePeer")
	}
}

// TestServeAuthTsnet_SelfNodeKeyDemotedToGuest covers the resolver
// wrapper in ensureAuthTsnetServer: peer_registry holds a self-row
// with the local NodeKey so other peers can dial us, but a request
// whose WhoIs resolves to that key on OUR auth listener is either
// a misconfig or a self-loop — it must NOT bypass AuthMiddleware as
// RolePeer just because the self-row also matches GetPeerByNodeKey.
// The resolver returns "" for self so the middleware falls through
// to Bearer resolution.
func TestServeAuthTsnet_SelfNodeKeyDemotedToGuest(t *testing.T) {
	s, _, _, got := newAuthTsnetTestServer(t, "nodekey:self", false)
	s.SetSelfNodeKey("nodekey:self")
	resolver := auth.NewResolver(nil, nil)
	srv := s.ensureAuthTsnetServer(resolver)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/peers/agent-sync", nil)
	req.RemoteAddr = "100.64.0.1:54321"
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (self NodeKey demoted to Guest, Bearer absent)", rec.Code)
	}
	if got.Role == auth.RolePeer {
		t.Fatalf("handler reached as RolePeer; self should not bypass AuthMiddleware")
	}
}

// TestServeAuthTsnet_SelfDeviceIDDemotedDespiteStartupRace covers
// the second self-loop guard: even when the resolver wrapper's
// currentSelfNodeKey check is empty (startup race — tsnet has bound
// but the self-NodeKey capture goroutine hasn't published yet), the
// peer_registry self-row still carries the local NodeKey so a
// looped-back caller would resolve to it. The middleware's
// SelfDeviceID guard demotes the hit before stamping RolePeer.
func TestServeAuthTsnet_SelfDeviceIDDemotedDespiteStartupRace(t *testing.T) {
	// Register the row under the same device_id we will plug into
	// Server.peerID so the SelfDeviceID guard fires.
	const selfDeviceID = "device-tsnet-test"
	s, _, deviceID, got := newAuthTsnetTestServer(t, "nodekey:self-on-startup", false)
	if deviceID != selfDeviceID {
		t.Fatalf("helper changed device id %q (expected %q)", deviceID, selfDeviceID)
	}
	// peerID matches the registered row; selfNodeKey is intentionally
	// left empty to simulate the pre-capture window.
	s.peerID = &peer.Identity{DeviceID: selfDeviceID, Name: "self"}
	// Do NOT call SetSelfNodeKey: the resolver wrapper's check is
	// inert in this state.
	resolver := auth.NewResolver(nil, nil)
	srv := s.ensureAuthTsnetServer(resolver)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/peers/agent-sync", nil)
	req.RemoteAddr = "100.64.0.1:54321"
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (self DeviceID demoted)", rec.Code)
	}
	if got.Role == auth.RolePeer {
		t.Fatalf("handler reached as RolePeer; self DeviceID guard failed")
	}
}

// TestServeAuthTsnet_UnsafeStampsOwner covers the --unsafe escape
// hatch: TailnetIdentityMiddleware short-circuits WhoIs and stamps
// RoleOwner via UnsafeAsHub=true. Owner skips peer_registry equality
// gates in the peer↔peer handlers, which would otherwise fail on an
// empty PeerID — that's the LAN / CI trust-the-boundary contract.
func TestServeAuthTsnet_UnsafeStampsOwner(t *testing.T) {
	s, _, _, got := newAuthTsnetTestServer(t, "nodekey:peerB", true)
	resolver := auth.NewResolver(nil, nil)
	srv := s.ensureAuthTsnetServer(resolver)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/peers/agent-sync", nil)
	req.RemoteAddr = "10.0.0.5:12345"
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (Owner via --unsafe)", rec.Code)
	}
	if got.Role != auth.RoleOwner {
		t.Fatalf("role = %v, want RoleOwner under --unsafe", got.Role)
	}
}

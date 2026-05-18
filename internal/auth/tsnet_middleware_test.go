package auth_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

// stamp captures whatever Principal the middleware injected so the
// test can assert per-case role + peer_id.
func stamp() (http.Handler, *auth.Principal) {
	got := &auth.Principal{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := auth.FromContext(r.Context())
		*got = p
		w.WriteHeader(http.StatusNoContent)
	})
	return h, got
}

// newTestStore opens a temp Store and registers one peer row with
// the supplied node_key. Returns the store + the row's device_id.
func newTestStore(t *testing.T, nodeKey string) (*store.Store, string) {
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
	const deviceID = "device-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if _, err := st.UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID: deviceID,
		Name:     "peerA",
		URL:      "http://100.64.0.2:8080",
		NodeKey:  nodeKey,
		Status:   store.PeerStatusOnline,
	}); err != nil {
		t.Fatalf("UpsertPeer: %v", err)
	}
	return st, deviceID
}

func newReq() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	req.RemoteAddr = "100.64.0.42:54321"
	return req
}

func TestTailnetIdentityMiddleware_PeerMatch(t *testing.T) {
	st, deviceID := newTestStore(t, "nodekey:peerA")
	next, got := stamp()
	h := auth.TailnetIdentityMiddleware(auth.TailnetIdentityConfig{
		Store: st,
		Resolver: func(ctx context.Context, addr string) (string, error) {
			return "nodekey:peerA", nil
		},
		Logger: slog.Default(),
	})(next)
	h.ServeHTTP(httptest.NewRecorder(), newReq())
	if got.Role != auth.RolePeer {
		t.Fatalf("role = %v, want RolePeer", got.Role)
	}
	if got.PeerID != deviceID {
		t.Fatalf("peer_id = %q, want %q", got.PeerID, deviceID)
	}
}

func TestTailnetIdentityMiddleware_SelfOwner(t *testing.T) {
	st, _ := newTestStore(t, "nodekey:peerA")
	next, got := stamp()
	h := auth.TailnetIdentityMiddleware(auth.TailnetIdentityConfig{
		Store: st,
		Resolver: func(ctx context.Context, addr string) (string, error) {
			return "nodekey:self", nil
		},
		SelfNodeKeyFunc: func() string { return "nodekey:self" },
	})(next)
	h.ServeHTTP(httptest.NewRecorder(), newReq())
	if got.Role != auth.RoleOwner {
		t.Fatalf("role = %v, want RoleOwner", got.Role)
	}
}

func TestTailnetIdentityMiddleware_UnknownTailnetCallerGuest(t *testing.T) {
	st, _ := newTestStore(t, "nodekey:peerA")
	next, got := stamp()
	h := auth.TailnetIdentityMiddleware(auth.TailnetIdentityConfig{
		Store: st,
		Resolver: func(ctx context.Context, addr string) (string, error) {
			return "nodekey:stranger", nil
		},
	})(next)
	h.ServeHTTP(httptest.NewRecorder(), newReq())
	if got.Role != auth.RoleGuest {
		t.Fatalf("role = %v, want RoleGuest (unapproved tailnet caller)", got.Role)
	}
}

func TestTailnetIdentityMiddleware_ResolverError(t *testing.T) {
	st, _ := newTestStore(t, "nodekey:peerA")
	next, got := stamp()
	h := auth.TailnetIdentityMiddleware(auth.TailnetIdentityConfig{
		Store: st,
		Resolver: func(ctx context.Context, addr string) (string, error) {
			return "", errors.New("tailscaled unreachable")
		},
	})(next)
	h.ServeHTTP(httptest.NewRecorder(), newReq())
	if got.Role != auth.RoleGuest {
		t.Fatalf("role = %v, want RoleGuest (resolver failure)", got.Role)
	}
}

func TestTailnetIdentityMiddleware_UnsafePeer(t *testing.T) {
	next, got := stamp()
	h := auth.TailnetIdentityMiddleware(auth.TailnetIdentityConfig{
		Unsafe:      true,
		UnsafeAsHub: false,
	})(next)
	h.ServeHTTP(httptest.NewRecorder(), newReq())
	if got.Role != auth.RolePeer {
		t.Fatalf("role = %v, want RolePeer (unsafe + UnsafeAsHub=false)", got.Role)
	}
}

func TestTailnetIdentityMiddleware_UnsafeHub(t *testing.T) {
	next, got := stamp()
	h := auth.TailnetIdentityMiddleware(auth.TailnetIdentityConfig{
		Unsafe:      true,
		UnsafeAsHub: true,
	})(next)
	h.ServeHTTP(httptest.NewRecorder(), newReq())
	if got.Role != auth.RoleOwner {
		t.Fatalf("role = %v, want RoleOwner (unsafe + UnsafeAsHub=true)", got.Role)
	}
}

func TestTailnetIdentityMiddleware_NoResolverFallsThroughGuest(t *testing.T) {
	next, got := stamp()
	h := auth.TailnetIdentityMiddleware(auth.TailnetIdentityConfig{})(next)
	h.ServeHTTP(httptest.NewRecorder(), newReq())
	if got.Role != auth.RoleGuest {
		t.Fatalf("role = %v, want RoleGuest (no resolver, no Unsafe)", got.Role)
	}
}

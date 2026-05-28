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

// Hub public listener: every WhoIs-resolved tailnet caller is Owner,
// peer_registry is NOT consulted. Covers the paired-peer browser case
// that the develop-v1 regression broke (paired peer was being demoted
// to RolePeer, then policy.AllowNonOwner gated the bare list
// endpoints and Dashboard rendered empty).
func TestTailnetIdentityMiddleware_HubPromotesPairedPeerToOwner(t *testing.T) {
	st, _ := newTestStore(t, "nodekey:peerA")
	next, got := stamp()
	h := auth.TailnetIdentityMiddleware(auth.TailnetIdentityConfig{
		Store: st,
		Resolver: func(ctx context.Context, addr string) (string, error) {
			return "nodekey:peerA", nil
		},
		PromoteUnknownTailnetToOwner: true,
	})(next)
	h.ServeHTTP(httptest.NewRecorder(), newReq())
	if got.Role != auth.RoleOwner {
		t.Fatalf("role = %v, want RoleOwner (Hub trusts every tailnet caller, paired-peer included)", got.Role)
	}
	// PeerID is stamped alongside RoleOwner when WhoIs matches a
	// paired peer so handlers downstream (e.g. /api/v1/peers/events)
	// can identify the connection without a second registry query.
	// The Owner role itself is unchanged — the elevated trust still
	// matches the documented "Tailscale reach == Owner" UX.
	if got.PeerID == "" {
		t.Fatalf("peer_id is empty, want the matched device_id")
	}
}

// Hub public listener: an unpaired tailnet caller is still Owner —
// peer_registry lookup is skipped entirely. Preserves the legacy
// "Tailscale reach == Owner" UX.
func TestTailnetIdentityMiddleware_HubPromotesUnknownTailnetCallerToOwner(t *testing.T) {
	st, _ := newTestStore(t, "nodekey:peerA")
	next, got := stamp()
	h := auth.TailnetIdentityMiddleware(auth.TailnetIdentityConfig{
		Store: st,
		Resolver: func(ctx context.Context, addr string) (string, error) {
			return "nodekey:stranger", nil
		},
		PromoteUnknownTailnetToOwner: true,
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

// Hub mode (PromoteUnknownTailnetToOwner=true): a WhoIs that errors —
// tailscaled blip, transient context timeout, etc. — must NOT 403 the
// operator's Dashboard. The tsnet listener is only reachable over
// Tailscale, so a caller arriving here has already passed the tailnet
// boundary regardless of WhoIs status. Owner-fallback.
func TestTailnetIdentityMiddleware_HubResolverErrorPromotesOwner(t *testing.T) {
	st, _ := newTestStore(t, "nodekey:peerA")
	next, got := stamp()
	h := auth.TailnetIdentityMiddleware(auth.TailnetIdentityConfig{
		Store: st,
		Resolver: func(ctx context.Context, addr string) (string, error) {
			return "", errors.New("tailscaled unreachable")
		},
		PromoteUnknownTailnetToOwner: true,
	})(next)
	h.ServeHTTP(httptest.NewRecorder(), newReq())
	if got.Role != auth.RoleOwner {
		t.Fatalf("role = %v, want RoleOwner (Hub fallback on WhoIs error)", got.Role)
	}
}

// Hub mode: WhoIs returns (nil, nil) — tsnet recognises the listener
// reach but its view of the tailnet has not refreshed for this caller
// yet (just-joined node, recent rekey, …). This is the case that
// silently broke desktop-tvt (100.117.138.20) on develop-v1: err==nil
// so the Warn log never fired, but nodeKey=="" demoted the caller to
// Guest, and policy.AllowNonOwner 403'd /agents and /groupdms.
func TestTailnetIdentityMiddleware_HubEmptyNodeKeyPromotesOwner(t *testing.T) {
	st, _ := newTestStore(t, "nodekey:peerA")
	next, got := stamp()
	h := auth.TailnetIdentityMiddleware(auth.TailnetIdentityConfig{
		Store: st,
		Resolver: func(ctx context.Context, addr string) (string, error) {
			return "", nil
		},
		PromoteUnknownTailnetToOwner: true,
	})(next)
	h.ServeHTTP(httptest.NewRecorder(), newReq())
	if got.Role != auth.RoleOwner {
		t.Fatalf("role = %v, want RoleOwner (Hub fallback on empty NodeKey)", got.Role)
	}
}

// Hub mode: resolver returns the not-ready sentinel — the http
// listener accepted a connection before SetNodeKeyResolver finished
// wiring. The middleware must NOT apply the Hub Owner-fallback for
// this case: an unidentified caller in the startup window has no
// claim to Owner, and the wiring race resolves on its own. Guest is
// the correct stamp; the policy layer then 403s privileged surface.
func TestTailnetIdentityMiddleware_HubResolverNotReadyStaysGuest(t *testing.T) {
	st, _ := newTestStore(t, "nodekey:peerA")
	next, got := stamp()
	h := auth.TailnetIdentityMiddleware(auth.TailnetIdentityConfig{
		Store: st,
		Resolver: func(ctx context.Context, addr string) (string, error) {
			return "", auth.ErrNodeKeyResolverNotReady
		},
		PromoteUnknownTailnetToOwner: true,
	})(next)
	h.ServeHTTP(httptest.NewRecorder(), newReq())
	if got.Role != auth.RoleGuest {
		t.Fatalf("role = %v, want RoleGuest (resolver not ready must not Owner-fallback)", got.Role)
	}
}

// Peer mode (PromoteUnknownTailnetToOwner=false): WhoIs returning
// (nil, nil) must NOT promote the caller. Stays Guest so the
// auth-required listener's AuthMiddleware can resolve a Bearer
// instead, and a stray tailnet node never silently gains a role.
func TestTailnetIdentityMiddleware_PeerEmptyNodeKeyStaysGuest(t *testing.T) {
	st, _ := newTestStore(t, "nodekey:peerA")
	next, got := stamp()
	h := auth.TailnetIdentityMiddleware(auth.TailnetIdentityConfig{
		Store: st,
		Resolver: func(ctx context.Context, addr string) (string, error) {
			return "", nil
		},
	})(next)
	h.ServeHTTP(httptest.NewRecorder(), newReq())
	if got.Role != auth.RoleGuest {
		t.Fatalf("role = %v, want RoleGuest (peer mode keeps strict fallthrough)", got.Role)
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

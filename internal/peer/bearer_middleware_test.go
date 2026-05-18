package peer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

func openStoreWithPeer(t *testing.T, deviceID string, trusted bool) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rec := &store.PeerRecord{
		DeviceID:  deviceID,
		Name:      "test-peer",
		URL:       "http://example:8080",
		
		Trusted:   trusted,
	}
	if _, err := st.RegisterPeerMetadata(context.Background(), rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	if trusted {
		if err := st.UpdatePeerTrust(context.Background(), deviceID, true); err != nil {
			t.Fatalf("trust: %v", err)
		}
	}
	return st
}

// captureHandler records the auth.Principal it observes so the test can
// assert that the middleware stamped the request.
type captureHandler struct {
	gotPrincipal auth.Principal
	called       bool
}

func (c *captureHandler) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	c.called = true
	c.gotPrincipal = auth.FromContext(r.Context())
}

func TestBearerMiddleware_ValidBearerStampsRolePeer(t *testing.T) {
	st := openStoreWithPeer(t, "dev-alpha", true)
	issued, err := st.IssuePeerToken(context.Background(), "dev-alpha", store.PeerTokenRolePeerToHub)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	mw := NewBearerPeerMiddleware(st, "")
	cap := &captureHandler{}
	h := mw.Wrap(cap)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	req.Header.Set("Authorization", "Bearer "+issued.Raw)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !cap.called {
		t.Fatal("downstream handler not called")
	}
	if cap.gotPrincipal.Role != auth.RolePeer {
		t.Fatalf("role = %q want RolePeer", cap.gotPrincipal.Role)
	}
	if cap.gotPrincipal.PeerID != "dev-alpha" {
		t.Fatalf("peer_id = %q", cap.gotPrincipal.PeerID)
	}
	if !cap.gotPrincipal.PeerTrusted {
		t.Fatal("trust bit lost")
	}
}

func TestBearerMiddleware_MissingHeaderFallsThrough(t *testing.T) {
	st := openStoreWithPeer(t, "dev-alpha", true)
	mw := NewBearerPeerMiddleware(st, "")
	cap := &captureHandler{}
	h := mw.Wrap(cap)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !cap.called {
		t.Fatal("downstream not called")
	}
	if cap.gotPrincipal.Role == auth.RolePeer {
		t.Fatalf("missing-Bearer should not stamp RolePeer: %v", cap.gotPrincipal)
	}
}

func TestBearerMiddleware_RevokedTokenFallsThrough(t *testing.T) {
	st := openStoreWithPeer(t, "dev-alpha", true)
	issued, _ := st.IssuePeerToken(context.Background(), "dev-alpha", store.PeerTokenRolePeerToHub)
	if err := st.RevokePeerToken(context.Background(), issued.Raw); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	mw := NewBearerPeerMiddleware(st, "")
	cap := &captureHandler{}
	h := mw.Wrap(cap)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	req.Header.Set("Authorization", "Bearer "+issued.Raw)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if cap.gotPrincipal.Role == auth.RolePeer {
		t.Fatal("revoked token granted RolePeer")
	}
}

func TestBearerMiddleware_SelfLoopbackRefused(t *testing.T) {
	st := openStoreWithPeer(t, "self-dev", true)
	issued, _ := st.IssuePeerToken(context.Background(), "self-dev", store.PeerTokenRolePeerToHub)
	mw := NewBearerPeerMiddleware(st, "self-dev")
	cap := &captureHandler{}
	h := mw.Wrap(cap)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	req.Header.Set("Authorization", "Bearer "+issued.Raw)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if cap.gotPrincipal.Role == auth.RolePeer {
		t.Fatal("self-loopback Bearer admitted")
	}
}

func TestBearerMiddleware_UntrustedPeerStampsFalse(t *testing.T) {
	st := openStoreWithPeer(t, "dev-untrusted", false)
	issued, _ := st.IssuePeerToken(context.Background(), "dev-untrusted", store.PeerTokenRolePeerToHub)
	mw := NewBearerPeerMiddleware(st, "")
	cap := &captureHandler{}
	h := mw.Wrap(cap)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	req.Header.Set("Authorization", "Bearer "+issued.Raw)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if cap.gotPrincipal.Role != auth.RolePeer {
		t.Fatal("RolePeer not stamped")
	}
	if cap.gotPrincipal.PeerTrusted {
		t.Fatal("untrusted peer reported trusted")
	}
}

func TestExtractBearer(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", false},
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"BEARER abc", "abc", true},
		{"Bearer ", "", false},
		{"Token abc", "", false},
		{"X-Kojo-Token: foo", "", false},
		{"Bearer    spaced  ", "spaced", true},
	}
	for _, c := range cases {
		got, ok := extractBearer(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("extractBearer(%q) = %q,%v want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

package peer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

// newAuthFixture builds a store with one peer_registry row + a
// matching Ed25519 keypair and returns the middleware-wrapped
// handler under test plus the keys for the test to drive.
func newAuthFixture(t *testing.T) (*AuthMiddleware, ed25519.PrivateKey, string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	deviceID := "deadbeef0123456789abcdef01234567"
	if _, err := st.UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID:  deviceID,
		Name:      "remote-peer",
		PublicKey: base64.StdEncoding.EncodeToString(pub),
		Status:    store.PeerStatusOnline,
	}); err != nil {
		t.Fatalf("UpsertPeer: %v", err)
	}
	mw := NewAuthMiddleware(st, NewNonceCache(AuthMaxClockSkew), "")
	return mw, priv, deviceID, st
}

// stampedHandler captures the principal the chain forwards so the
// test can assert role attribution.
func stampedHandler(t *testing.T) (http.Handler, *auth.Principal) {
	var seen auth.Principal
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = auth.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	return h, &seen
}

func signedRequest(t *testing.T, method, path string, body []byte, deviceID string, priv ed25519.PrivateKey) *http.Request {
	t.Helper()
	var br *bytes.Reader
	if body != nil {
		br = bytes.NewReader(body)
	}
	var bodyReader io.Reader
	if br != nil {
		bodyReader = br
	}
	r := httptest.NewRequest(method, path, bodyReader)
	// Tests using the default selfDeviceID="" (audience check
	// disabled) — pass any audience; the test fixtures set
	// selfDeviceID per-test where needed.
	if err := SignRequest(r, deviceID, priv, freshNonceB64(t), "test-audience"); err != nil {
		t.Fatalf("SignRequest: %v", err)
	}
	return r
}

func freshNonceB64(t *testing.T) string {
	t.Helper()
	var b [AuthNonceLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b[:])
}

func TestAuthMiddleware_PassThroughWhenNoHeaders(t *testing.T) {
	mw, _, _, _ := newAuthFixture(t)
	next, seen := stampedHandler(t)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/peers/events", nil)
	w := httptest.NewRecorder()
	mw.Wrap(next).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("pass-through: status = %d, want 200", w.Code)
	}
	if seen.Role != auth.RoleGuest {
		t.Errorf("pass-through: principal mutated, got %v", seen.Role)
	}
}

func TestAuthMiddleware_StampsPeerOnValidSignature(t *testing.T) {
	mw, priv, dev, _ := newAuthFixture(t)
	next, seen := stampedHandler(t)
	r := signedRequest(t, http.MethodGet, "/api/v1/peers/events", nil, dev, priv)
	w := httptest.NewRecorder()
	mw.Wrap(next).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if seen.Role != auth.RolePeer {
		t.Errorf("role = %v, want RolePeer", seen.Role)
	}
	if seen.PeerID != dev {
		t.Errorf("peer_id = %q, want %q", seen.PeerID, dev)
	}
}

func TestAuthMiddleware_RejectsUnknownPeer(t *testing.T) {
	mw, _, _, _ := newAuthFixture(t)
	// New keypair, NEVER registered.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	r := signedRequest(t, http.MethodGet, "/api/v1/peers/events", nil, "ffffffffffffffffffffffffffffffff", otherPriv)
	w := httptest.NewRecorder()
	mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler reached on unknown peer")
	})).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "unknown") {
		t.Errorf("error should mention 'unknown': %s", w.Body.String())
	}
}

func TestAuthMiddleware_RejectsWrongSignature(t *testing.T) {
	mw, _, dev, _ := newAuthFixture(t)
	// Sign with a DIFFERENT private key — verification against the
	// registered public key will fail.
	_, wrongPriv, _ := ed25519.GenerateKey(rand.Reader)
	r := signedRequest(t, http.MethodGet, "/api/v1/peers/events", nil, dev, wrongPriv)
	w := httptest.NewRecorder()
	mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler reached on bad sig")
	})).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (body=%s)", w.Code, w.Body.String())
	}
}

func TestAuthMiddleware_RejectsStaleTimestamp(t *testing.T) {
	mw, priv, dev, _ := newAuthFixture(t)
	// Build a request with a manually-set stale timestamp. We can't
	// use SignRequest because it stamps time.Now(); build by hand.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/peers/events", nil)
	nonce := freshNonceB64(t)
	staleTS := time.Now().Add(-10 * time.Minute).UnixMilli()
	in := SigningInput{DeviceID: dev, Audience: "test-aud", TS: staleTS, Nonce: nonce,
		Method: "GET", Path: "/api/v1/peers/events"}
	sig := Sign(priv, in)
	r.Header.Set(AuthHeaderID, dev)
	r.Header.Set(AuthHeaderAud, "test-aud")
	r.Header.Set(AuthHeaderTS, strconv.FormatInt(staleTS, 10))
	r.Header.Set(AuthHeaderNonce, nonce)
	r.Header.Set(AuthHeaderSig, sig)
	w := httptest.NewRecorder()
	mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler reached on stale timestamp")
	})).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestAuthMiddleware_RejectsReplayedNonce(t *testing.T) {
	mw, priv, dev, _ := newAuthFixture(t)
	// First request: ok.
	r1 := signedRequest(t, http.MethodGet, "/api/v1/peers/events", nil, dev, priv)
	w1 := httptest.NewRecorder()
	mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(w1, r1)
	if w1.Code != http.StatusOK {
		t.Fatalf("first request: status = %d", w1.Code)
	}
	// Replay: SAME headers (clone), should be rejected.
	r2 := r1.Clone(context.Background())
	w2 := httptest.NewRecorder()
	mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler reached on replay")
	})).ServeHTTP(w2, r2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("replay: status = %d, want 401", w2.Code)
	}
}

func TestAuthMiddleware_PartialHeadersAre400(t *testing.T) {
	mw, _, _, _ := newAuthFixture(t)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/peers/events", nil)
	r.Header.Set(AuthHeaderID, "deadbeef")
	// other 3 headers absent
	w := httptest.NewRecorder()
	mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler reached on partial headers")
	})).ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestAuthMiddleware_RejectsCrossPeerAudienceReplay(t *testing.T) {
	mw, priv, dev, _ := newAuthFixture(t)
	// Local peer claims to be "hub-bravo". A request signed for
	// audience "hub-charlie" replayed against us must be refused.
	mw.selfDeviceID = "hub-bravo"
	r := httptest.NewRequest(http.MethodGet, "/api/v1/peers/events", nil)
	if err := SignRequest(r, dev, priv, freshNonceB64(t), "hub-charlie"); err != nil {
		t.Fatalf("SignRequest: %v", err)
	}
	w := httptest.NewRecorder()
	mw.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler reached on cross-peer replay")
	})).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("cross-peer audience: status = %d, want 401 (body=%s)",
			w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "audience") {
		t.Errorf("error should mention audience: %s", w.Body.String())
	}
}

func TestAuthMiddleware_RejectsSelfLoopback(t *testing.T) {
	mw, priv, dev, _ := newAuthFixture(t)
	mw.selfDeviceID = dev // pretend we ARE this peer
	r := signedRequest(t, http.MethodGet, "/api/v1/peers/events", nil, dev, priv)
	w := httptest.NewRecorder()
	mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler reached on self-loopback")
	})).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (body=%s)", w.Code, w.Body.String())
	}
}

func TestAuthMiddleware_BodyHashCoversPayload(t *testing.T) {
	mw, priv, dev, _ := newAuthFixture(t)
	body := []byte(`{"x":1}`)
	r := signedRequest(t, http.MethodPost, "/api/v1/peers/blobs/foo", body, dev, priv)
	// Tamper with the body AFTER signing. Verification must fail.
	r.Body = io.NopCloser(bytes.NewReader([]byte(`{"x":2}`)))
	w := httptest.NewRecorder()
	mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler reached on tampered body")
	})).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 on tampered body", w.Code)
	}
}

func TestAuthMiddleware_OversizeBodyReturns413(t *testing.T) {
	mw, priv, dev, _ := newAuthFixture(t)
	big := make([]byte, AuthMaxBodyBytes+1)
	r := signedRequest(t, http.MethodPost, "/api/v1/peers/blobs/foo", big, dev, priv)
	w := httptest.NewRecorder()
	mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler reached on oversize body")
	})).ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

func TestAuthMiddleware_RebindsBodyForHandler(t *testing.T) {
	mw, priv, dev, _ := newAuthFixture(t)
	body := []byte(`{"hello":"world"}`)
	r := signedRequest(t, http.MethodPost, "/api/v1/peers/blobs/foo", body, dev, priv)
	var seen []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		seen, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("handler read body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	})
	w := httptest.NewRecorder()
	mw.Wrap(next).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if string(seen) != string(body) {
		t.Errorf("handler body = %q, want %q", seen, body)
	}
}

//go:build heavy_test

// Heavy because the test boots a full agent.Manager (whose store
// backs peer_registry). Run with:
//
//	go test -tags heavy_test ./internal/server/...

package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// newPeerTestServer wires a Server with a manager + a synthetic peer
// identity. The identity is NOT loaded from kv (LoadOrCreate would
// require a KEK setup that the tests don't model) — we mint a
// throw-away keypair here so the routes register and self-detection
// works. For the same reason the registrar is NOT started, so the
// "self" row only exists when the test seeds it explicitly.
func newPeerTestServer(t *testing.T) (*Server, *store.Store, *peer.Identity) {
	t.Helper()
	t.Setenv("KOJO_CONFIG_DIR", t.TempDir())

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	mgr, err := agent.NewManager(logger)
	if err != nil {
		t.Fatalf("agent.NewManager: %v", err)
	}
	t.Cleanup(func() { mgr.Shutdown() })

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519: %v", err)
	}
	id := &peer.Identity{
		DeviceID:   uuid.NewString(),
		Name:       "test-self",
		PublicKey:  pub,
		PrivateKey: priv,
	}

	srv := New(Config{
		Addr:         ":0",
		Logger:       logger,
		Version:      "test",
		AgentManager: mgr,
		PeerIdentity: id,
	})
	return srv, mgr.Store(), id
}

// freshKey returns a base64-std encoded random Ed25519 public key. The
// peer registry tests don't care about the corresponding private key
// — they only validate the wire format.
func freshKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519: %v", err)
	}
	return base64.StdEncoding.EncodeToString(pub)
}

// insertSpace splices a literal ' ' into the middle of a base64
// string. The non-strict base64 decoder silently skips whitespace,
// so the result still decodes to the same 32-byte key — but the
// stored value is no longer canonical, and the strict validator
// must reject it. (We use space rather than '\n' because raw LF is
// invalid inside a JSON string per RFC 8259, and Go's json decoder
// would 400 the request before validation runs.)
func insertSpace(b64 string) string {
	mid := len(b64) / 2
	return b64[:mid] + " " + b64[mid:]
}

func ownerReq(method, path, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	return r.WithContext(auth.WithPrincipal(context.Background(), auth.Principal{Role: auth.RoleOwner}))
}

func TestListPeersIncludesSelfFlag(t *testing.T) {
	srv, st, self := newPeerTestServer(t)

	// Seed the local row (the registrar is not running in this test).
	if _, err := st.UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID:  self.DeviceID,
		Name:      self.Name,
		PublicKey: self.PublicKeyBase64(),
		LastSeen:  store.NowMillis(),
		Status:    store.PeerStatusOnline,
	}); err != nil {
		t.Fatalf("seed self: %v", err)
	}

	// And a remote.
	remoteID := uuid.NewString()
	if _, err := st.UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID:  remoteID,
		Name:      "remote",
		PublicKey: freshKey(t),
		Status:    store.PeerStatusOffline,
	}); err != nil {
		t.Fatalf("seed remote: %v", err)
	}

	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, ownerReq(http.MethodGet, "/api/v1/peers", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	var resp peerListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if resp.SelfDeviceID != self.DeviceID {
		t.Errorf("selfDeviceId mismatch: %q vs %q", resp.SelfDeviceID, self.DeviceID)
	}
	var sawSelf, sawRemote bool
	for _, item := range resp.Items {
		switch item.DeviceID {
		case self.DeviceID:
			sawSelf = true
			if !item.IsSelf {
				t.Errorf("self row missing isSelf flag")
			}
		case remoteID:
			sawRemote = true
			if item.IsSelf {
				t.Errorf("remote row should not be flagged self")
			}
		}
	}
	if !sawSelf || !sawRemote {
		t.Errorf("expected to see self+remote, got %+v", resp.Items)
	}
}

func TestRegisterPeerHappyPath(t *testing.T) {
	srv, st, _ := newPeerTestServer(t)
	id := uuid.NewString()
	body := `{"deviceId":"` + id + `","name":"laptop","publicKey":"` + freshKey(t) + `"}`
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, ownerReq(http.MethodPost, "/api/v1/peers", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	rec, err := st.GetPeer(context.Background(), id)
	if err != nil {
		t.Fatalf("GetPeer: %v", err)
	}
	if rec.Status != store.PeerStatusOffline {
		t.Errorf("freshly-registered peer should be offline until first heartbeat, got %q", rec.Status)
	}
	if rec.Name != "laptop" {
		t.Errorf("name mismatch: %q", rec.Name)
	}
}

func TestRegisterPeerRejectsSelf(t *testing.T) {
	srv, _, self := newPeerTestServer(t)
	body := `{"deviceId":"` + self.DeviceID + `","name":"x","publicKey":"` + freshKey(t) + `"}`
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, ownerReq(http.MethodPost, "/api/v1/peers", body))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 self-registration; got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRegisterPeerValidatesInputs(t *testing.T) {
	srv, _, _ := newPeerTestServer(t)
	cases := []struct {
		name string
		body string
	}{
		{"empty body", ``},
		{"invalid json", `not-json`},
		{"missing deviceId", `{"name":"x","publicKey":"` + freshKey(t) + `"}`},
		{"non-uuid deviceId", `{"deviceId":"abc","name":"x","publicKey":"` + freshKey(t) + `"}`},
		// uuid.Parse is permissive about URN / braced / un-hyphenated /
		// uppercase forms; the wire contract demands canonical
		// 8-4-4-4-12 lowercase so the same logical UUID can't land in
		// peer_registry under multiple keys (and so self-detection
		// can't be bypassed by submitting a different spelling of the
		// local device id).
		{"urn-prefixed uuid", `{"deviceId":"urn:uuid:` + uuid.NewString() + `","name":"x","publicKey":"` + freshKey(t) + `"}`},
		{"braced uuid", `{"deviceId":"{` + uuid.NewString() + `}","name":"x","publicKey":"` + freshKey(t) + `"}`},
		{"uppercase uuid", `{"deviceId":"` + strings.ToUpper(uuid.NewString()) + `","name":"x","publicKey":"` + freshKey(t) + `"}`},
		{"unhyphenated uuid", `{"deviceId":"` + strings.ReplaceAll(uuid.NewString(), "-", "") + `","name":"x","publicKey":"` + freshKey(t) + `"}`},
		{"missing name", `{"deviceId":"` + uuid.NewString() + `","publicKey":"` + freshKey(t) + `"}`},
		{"name with newline", `{"deviceId":"` + uuid.NewString() + `","name":"a\nb","publicKey":"` + freshKey(t) + `"}`},
		{"name with tab", `{"deviceId":"` + uuid.NewString() + `","name":"a\tb","publicKey":"` + freshKey(t) + `"}`},
		{"name with ESC", `{"deviceId":"` + uuid.NewString() + `","name":"ab","publicKey":"` + freshKey(t) + `"}`},
		{"name with DEL", `{"deviceId":"` + uuid.NewString() + `","name":"ab","publicKey":"` + freshKey(t) + `"}`},
		{"missing publicKey", `{"deviceId":"` + uuid.NewString() + `","name":"x"}`},
		{"non-base64 publicKey", `{"deviceId":"` + uuid.NewString() + `","name":"x","publicKey":"not%base64"}`},
		{"wrong-size publicKey", `{"deviceId":"` + uuid.NewString() + `","name":"x","publicKey":"AAAA"}`},
		// 32-byte key with embedded whitespace decodes successfully
		// under non-strict base64 but is non-canonical — strict mode
		// must reject it so the same key can't be registered under
		// two distinct stored values.
		{"publicKey with internal space", `{"deviceId":"` + uuid.NewString() + `","name":"x","publicKey":"` + insertSpace(freshKey(t)) + `"}`},
		{"capabilities not json", `{"deviceId":"` + uuid.NewString() + `","name":"x","publicKey":"` + freshKey(t) + `","capabilities":"not-json"}`},
		// Capabilities is documented as `JSON: {os, arch, gpu, ...}`
		// so non-objects (scalar / array / null) are rejected even
		// though they're technically valid JSON.
		{"capabilities is array", `{"deviceId":"` + uuid.NewString() + `","name":"x","publicKey":"` + freshKey(t) + `","capabilities":"[1,2]"}`},
		{"capabilities is scalar", `{"deviceId":"` + uuid.NewString() + `","name":"x","publicKey":"` + freshKey(t) + `","capabilities":"42"}`},
		{"capabilities is null", `{"deviceId":"` + uuid.NewString() + `","name":"x","publicKey":"` + freshKey(t) + `","capabilities":"null"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, ownerReq(http.MethodPost, "/api/v1/peers", c.body))
			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestRegisterPeerKeyImmutableOnReRegister(t *testing.T) {
	srv, st, _ := newPeerTestServer(t)
	id := uuid.NewString()
	originalKey := freshKey(t)
	first := `{"deviceId":"` + id + `","name":"v1","publicKey":"` + originalKey + `"}`
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, ownerReq(http.MethodPost, "/api/v1/peers", first))
	if w.Code != http.StatusOK {
		t.Fatalf("first register: %d body=%s", w.Code, w.Body.String())
	}

	// Re-register with a different key — UpsertPeer is documented to
	// preserve public_key on conflict. Confirm via GetPeer.
	hostileKey := freshKey(t)
	if hostileKey == originalKey {
		t.Skip("ed25519 collision (impossible in practice); rerun")
	}
	second := `{"deviceId":"` + id + `","name":"v2","publicKey":"` + hostileKey + `"}`
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, ownerReq(http.MethodPost, "/api/v1/peers", second))
	if w.Code != http.StatusOK {
		t.Fatalf("second register: %d body=%s", w.Code, w.Body.String())
	}
	rec, err := st.GetPeer(context.Background(), id)
	if err != nil {
		t.Fatalf("GetPeer: %v", err)
	}
	if rec.PublicKey != originalKey {
		t.Errorf("public_key was overwritten — silent identity rotation: had %q, now %q", originalKey, rec.PublicKey)
	}
	if rec.Name != "v2" {
		t.Errorf("name should update on re-register, got %q", rec.Name)
	}
}

// TestRegisterPeerPreservesLivenessOnReRegister verifies that editing
// a peer's name (or any other metadata) via re-POST does NOT wipe the
// last_seen / status that the heartbeat path has been maintaining.
// Without this guarantee a UI rename would surface as the row "going
// offline" until the next heartbeat from that peer arrived.
func TestRegisterPeerPreservesLivenessOnReRegister(t *testing.T) {
	srv, st, _ := newPeerTestServer(t)
	id := uuid.NewString()
	key := freshKey(t)

	// Seed an "online" row directly through the store so the test
	// does not depend on the registrar loop running.
	if _, err := st.UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID:  id,
		Name:      "v1",
		PublicKey: key,
		LastSeen:  98765,
		Status:    store.PeerStatusOnline,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body := `{"deviceId":"` + id + `","name":"v2","publicKey":"` + key + `"}`
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, ownerReq(http.MethodPost, "/api/v1/peers", body))
	if w.Code != http.StatusOK {
		t.Fatalf("re-register: %d body=%s", w.Code, w.Body.String())
	}

	rec, err := st.GetPeer(context.Background(), id)
	if err != nil {
		t.Fatalf("GetPeer: %v", err)
	}
	if rec.Name != "v2" {
		t.Errorf("name should update on re-register, got %q", rec.Name)
	}
	if rec.LastSeen != 98765 {
		t.Errorf("last_seen was clobbered on re-register: had 98765, now %d", rec.LastSeen)
	}
	if rec.Status != store.PeerStatusOnline {
		t.Errorf("status was clobbered on re-register: had %q, now %q",
			store.PeerStatusOnline, rec.Status)
	}
}

func TestDeletePeerRejectsSelf(t *testing.T) {
	srv, _, self := newPeerTestServer(t)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, ownerReq(http.MethodDelete, "/api/v1/peers/"+self.DeviceID, ""))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 self-delete; got %d body=%s", w.Code, w.Body.String())
	}
}

func TestDeletePeerRemovesRow(t *testing.T) {
	srv, st, _ := newPeerTestServer(t)
	id := uuid.NewString()
	if _, err := st.UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID:  id,
		Name:      "doomed",
		PublicKey: freshKey(t),
		Status:    store.PeerStatusOffline,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, ownerReq(http.MethodDelete, "/api/v1/peers/"+id, ""))
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204; got %d body=%s", w.Code, w.Body.String())
	}
	if _, err := st.GetPeer(context.Background(), id); err == nil {
		t.Errorf("row still present after DELETE")
	}
}

func TestDeletePeerIdempotent(t *testing.T) {
	srv, _, _ := newPeerTestServer(t)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, ownerReq(http.MethodDelete, "/api/v1/peers/"+uuid.NewString(), ""))
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 even when row missing; got %d body=%s", w.Code, w.Body.String())
	}
}

func TestDeletePeerInvalidIDReturns400(t *testing.T) {
	srv, _, _ := newPeerTestServer(t)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, ownerReq(http.MethodDelete, "/api/v1/peers/not-a-uuid", ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d body=%s", w.Code, w.Body.String())
	}
}

func TestSelfPeerReturnsCurrentRow(t *testing.T) {
	srv, st, self := newPeerTestServer(t)
	if _, err := st.UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID:  self.DeviceID,
		Name:      self.Name,
		PublicKey: self.PublicKeyBase64(),
		LastSeen:  12345,
		Status:    store.PeerStatusOnline,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, ownerReq(http.MethodGet, "/api/v1/peers/self", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	var got peerResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DeviceID != self.DeviceID || !got.IsSelf {
		t.Errorf("self row mismatch: %+v", got)
	}
}

func TestSelfPeerReturns503WhenRegistrarHasntInsertedYet(t *testing.T) {
	srv, _, _ := newPeerTestServer(t)
	// Don't seed anything — the registrar wasn't started by the
	// test harness, so the self row doesn't exist yet.
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, ownerReq(http.MethodGet, "/api/v1/peers/self", ""))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503; got %d body=%s", w.Code, w.Body.String())
	}
}

func TestPeersAPIRejectsNonOwner(t *testing.T) {
	srv, _, _ := newPeerTestServer(t)
	// GET /api/v1/peers is intentionally open to RoleAgent
	// (reduced view, no publicKey / capabilities) so an agent
	// driving handoff/switch can discover targets by Tailscale
	// machine name. Every OTHER peer-API route stays owner-only.
	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/peers/self"},
		{http.MethodPost, "/api/v1/peers"},
		{http.MethodDelete, "/api/v1/peers/" + uuid.NewString()},
		{http.MethodPost, "/api/v1/peers/" + uuid.NewString() + "/rotate-key"},
	}
	for _, c := range cases {
		t.Run(c.method+c.path, func(t *testing.T) {
			r := httptest.NewRequest(c.method, c.path, nil)
			r = r.WithContext(auth.WithPrincipal(context.Background(),
				auth.Principal{Role: auth.RoleAgent, AgentID: "ag_x"}))
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Errorf("expected 403; got %d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// TestPeersListAgentReducedView pins the reduced-view contract:
// RoleAgent sees device_id + name + status + isSelf but neither
// publicKey nor capabilities. Owner sees both. The handoff
// orchestrator's UX depends on agents being able to find a target
// by name without learning every peer's Ed25519 identity.
func TestPeersListAgentReducedView(t *testing.T) {
	srv, st, _ := newPeerTestServer(t)
	id := uuid.NewString()
	if _, err := st.UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID:     id,
		Name:         "remote.tailnet.ts.net:8080",
		PublicKey:    freshKey(t),
		Capabilities: `{"os":"linux"}`,
		Status:       store.PeerStatusOnline,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/peers", nil)
	r = r.WithContext(auth.WithPrincipal(context.Background(),
		auth.Principal{Role: auth.RoleAgent, AgentID: "ag_x"}))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("agent GET: status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, id) || !strings.Contains(body, "remote.tailnet.ts.net") {
		t.Errorf("agent view missing device_id/name: %s", body)
	}
	if strings.Contains(body, `"os":"linux"`) {
		t.Errorf("agent view leaked capabilities: %s", body)
	}
	// The seed pubkey is base64 — checking the literal substring
	// would be fragile, so check the field name was suppressed.
	if strings.Contains(body, `"publicKey":"`) {
		t.Errorf("agent view leaked publicKey: %s", body)
	}
}

func TestRotatePeerKeyHappyPath(t *testing.T) {
	srv, st, _ := newPeerTestServer(t)
	id := uuid.NewString()
	originalKey := freshKey(t)
	if _, err := st.UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID:  id,
		Name:      "remote",
		PublicKey: originalKey,
		LastSeen:  98765,
		Status:    store.PeerStatusOnline,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	newKey := freshKey(t)
	if newKey == originalKey {
		t.Skip("ed25519 collision; rerun")
	}
	body := `{"publicKey":"` + newKey + `"}`
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, ownerReq(http.MethodPost, "/api/v1/peers/"+id+"/rotate-key", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	var resp peerRotateKeyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if resp.PreviousPublicKey != originalKey {
		t.Errorf("previous key mismatch: got %q want %q", resp.PreviousPublicKey, originalKey)
	}
	if resp.Peer.PublicKey != newKey {
		t.Errorf("response key mismatch: got %q want %q", resp.Peer.PublicKey, newKey)
	}
	// last_seen and status must NOT have been touched — rotation is
	// an operator-driven metadata change, not a liveness signal.
	rec, err := st.GetPeer(context.Background(), id)
	if err != nil {
		t.Fatalf("GetPeer: %v", err)
	}
	if rec.PublicKey != newKey {
		t.Errorf("DB key not rotated: %q", rec.PublicKey)
	}
	if rec.LastSeen != 98765 {
		t.Errorf("last_seen clobbered by rotation: %d", rec.LastSeen)
	}
	if rec.Status != store.PeerStatusOnline {
		t.Errorf("status clobbered by rotation: %q", rec.Status)
	}
}

func TestRotatePeerKeyRejectsSelf(t *testing.T) {
	srv, _, self := newPeerTestServer(t)
	body := `{"publicKey":"` + freshKey(t) + `"}`
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, ownerReq(http.MethodPost, "/api/v1/peers/"+self.DeviceID+"/rotate-key", body))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 self-rotation; got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRotatePeerKeyMissingRowReturns404(t *testing.T) {
	srv, _, _ := newPeerTestServer(t)
	body := `{"publicKey":"` + freshKey(t) + `"}`
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, ownerReq(http.MethodPost, "/api/v1/peers/"+uuid.NewString()+"/rotate-key", body))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404; got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRotatePeerKeyRejectsNoOp(t *testing.T) {
	srv, st, _ := newPeerTestServer(t)
	id := uuid.NewString()
	key := freshKey(t)
	if _, err := st.UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID:  id,
		Name:      "remote",
		PublicKey: key,
		Status:    store.PeerStatusOffline,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	body := `{"publicKey":"` + key + `"}`
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, ownerReq(http.MethodPost, "/api/v1/peers/"+id+"/rotate-key", body))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 no-op rotation; got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRotatePeerKeyValidatesInputs(t *testing.T) {
	srv, st, _ := newPeerTestServer(t)
	id := uuid.NewString()
	if _, err := st.UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID:  id,
		Name:      "remote",
		PublicKey: freshKey(t),
		Status:    store.PeerStatusOffline,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cases := []struct {
		name, path, body string
		wantStatus       int
	}{
		{"missing publicKey", "/api/v1/peers/" + id + "/rotate-key", `{}`, http.StatusBadRequest},
		{"non-base64 publicKey", "/api/v1/peers/" + id + "/rotate-key", `{"publicKey":"not%base64"}`, http.StatusBadRequest},
		{"wrong-size publicKey", "/api/v1/peers/" + id + "/rotate-key", `{"publicKey":"AAAA"}`, http.StatusBadRequest},
		{"non-strict publicKey", "/api/v1/peers/" + id + "/rotate-key", `{"publicKey":"` + insertSpace(freshKey(t)) + `"}`, http.StatusBadRequest},
		{"non-canonical UUID", "/api/v1/peers/" + strings.ToUpper(id) + "/rotate-key", `{"publicKey":"` + freshKey(t) + `"}`, http.StatusBadRequest},
		{"invalid JSON", "/api/v1/peers/" + id + "/rotate-key", `not-json`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, ownerReq(http.MethodPost, c.path, c.body))
			if w.Code != c.wantStatus {
				t.Errorf("expected %d, got %d body=%s", c.wantStatus, w.Code, w.Body.String())
			}
		})
	}
}

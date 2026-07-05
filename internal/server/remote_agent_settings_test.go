package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

// newRemoteAgentPatchServer seeds a hub server with an agent whose
// runtime lock is held by peer "peer-away" (seeded with the given
// status). The agent is NOT in the in-memory manager map, so
// remoteAgentProxyMiddleware treats it as remote.
func newRemoteAgentPatchServer(t *testing.T, agentID, peerStatus string) *Server {
	t.Helper()
	srv := newQueueTestServer(t)
	ctx := context.Background()
	st := srv.agents.Store()
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: agentID, Name: "old-name"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := st.AcquireAgentLock(ctx, agentID, "peer-away", now, 10*60*1000); err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	seedQueuePeer(t, srv, "peer-away", "http://127.0.0.1:1", peerStatus)
	return srv
}

// patchRemoteAgent drives the full remoteAgentProxyMiddleware with a
// sentinel next handler (599 = passed through, which none of these
// PATCH cases should do).
func patchRemoteAgent(srv *Server, agentID, body string, p auth.Principal) *httptest.ResponseRecorder {
	h := srv.remoteAgentProxyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(599)
	}))
	r := httptest.NewRequest(http.MethodPatch, "/api/v1/agents/"+agentID, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = authedRequest(r, p)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Hub-local-safe PATCH must succeed against the hub row while the
// holder peer is offline.
func TestRemoteAgentPatchHubLocalWhenHolderOffline(t *testing.T) {
	srv := newRemoteAgentPatchServer(t, "ag_off", store.PeerStatusOffline)

	w := patchRemoteAgent(srv, "ag_off",
		`{"name":"new-name","publicProfile":"notes here","autoEffort":true,"disabledInjections":["status"]}`,
		auth.Principal{Role: auth.RoleOwner})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp["name"] != "new-name" {
		t.Fatalf("response name = %v", resp["name"])
	}

	rec, err := srv.agents.Store().GetAgent(context.Background(), "ag_off")
	if err != nil {
		t.Fatalf("get agent row: %v", err)
	}
	if rec.Name != "new-name" {
		t.Fatalf("store name = %q, want new-name", rec.Name)
	}
	if rec.ETag == "" {
		t.Fatal("store etag not set after hub-local patch")
	}
}

// A holder-only field (model) keeps the historic proxy behaviour:
// offline holder → 502 peer_offline, and nothing lands hub-side.
func TestRemoteAgentPatchHolderOnlyFieldStillProxiedWhenOffline(t *testing.T) {
	srv := newRemoteAgentPatchServer(t, "ag_mdl", store.PeerStatusOffline)

	w := patchRemoteAgent(srv, "ag_mdl", `{"model":"opus"}`,
		auth.Principal{Role: auth.RoleOwner})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "peer_offline") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

// Mixed payload (hub-safe + holder-only) fails closed: proxied (502
// offline) and the hub-safe half is NOT partially applied.
func TestRemoteAgentPatchMixedPayloadFailsClosed(t *testing.T) {
	srv := newRemoteAgentPatchServer(t, "ag_mix", store.PeerStatusOffline)

	w := patchRemoteAgent(srv, "ag_mix", `{"name":"sneaky","model":"opus"}`,
		auth.Principal{Role: auth.RoleOwner})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	rec, err := srv.agents.Store().GetAgent(context.Background(), "ag_mix")
	if err != nil {
		t.Fatalf("get agent row: %v", err)
	}
	if rec.Name != "old-name" {
		t.Fatalf("store name = %q, want old-name (no partial apply)", rec.Name)
	}
}

// Unknown / privileged keys are not in the allowlist → fail closed to
// the proxy path.
func TestRemoteAgentPatchPrivilegedKeyFailsClosed(t *testing.T) {
	srv := newRemoteAgentPatchServer(t, "ag_prv", store.PeerStatusOffline)

	w := patchRemoteAgent(srv, "ag_prv", `{"privileged":true}`,
		auth.Principal{Role: auth.RoleOwner})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
}

// disabledInjections stays owner-only on the hub-local path.
func TestRemoteAgentPatchDisabledInjectionsOwnerOnly(t *testing.T) {
	srv := newRemoteAgentPatchServer(t, "ag_inj", store.PeerStatusOffline)

	w := patchRemoteAgent(srv, "ag_inj", `{"disabledInjections":["status"]}`,
		auth.Principal{Role: auth.RoleAgent, AgentID: "ag_inj"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
}

// A stale If-Match on the hub-local path returns 412 and applies
// nothing.
func TestRemoteAgentPatchIfMatchMismatch(t *testing.T) {
	srv := newRemoteAgentPatchServer(t, "ag_etag", store.PeerStatusOffline)

	h := srv.remoteAgentProxyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(599)
	}))
	r := httptest.NewRequest(http.MethodPatch, "/api/v1/agents/ag_etag",
		strings.NewReader(`{"name":"stale-write"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("If-Match", `"definitely-not-the-etag"`)
	r = authedRequest(r, auth.Principal{Role: auth.RoleOwner})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	rec, err := srv.agents.Store().GetAgent(context.Background(), "ag_etag")
	if err != nil {
		t.Fatalf("get agent row: %v", err)
	}
	if rec.Name != "old-name" {
		t.Fatalf("store name = %q, want old-name", rec.Name)
	}
}

// When the holder is ONLINE the PATCH must still be proxied so the
// holder's in-memory agent stays the write authority. The seeded URL
// has no listener → dial failure → 502 proxy_failed (NOT a hub-local
// 200, and NOT peer_offline).
func TestRemoteAgentPatchHubSafeStillProxiedWhenHolderOnline(t *testing.T) {
	srv := newRemoteAgentPatchServer(t, "ag_onl", store.PeerStatusOnline)

	w := patchRemoteAgent(srv, "ag_onl", `{"name":"proxied-name"}`,
		auth.Principal{Role: auth.RoleOwner})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "proxy_failed") {
		t.Fatalf("body = %s", w.Body.String())
	}
	rec, err := srv.agents.Store().GetAgent(context.Background(), "ag_onl")
	if err != nil {
		t.Fatalf("get agent row: %v", err)
	}
	if rec.Name != "old-name" {
		t.Fatalf("store name = %q, want old-name (online edits are holder-side)", rec.Name)
	}
}

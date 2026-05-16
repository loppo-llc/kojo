package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestTokenStore_OwnerAndAgent(t *testing.T) {
	dir := t.TempDir()
	st, err := NewTokenStore(dir, nil, "")
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	if owner := st.OwnerToken(); len(owner) != 64 {
		t.Fatalf("owner token: want 64 hex chars, got %d (%q)", len(owner), owner)
	}
	if !st.VerifyOwner(st.OwnerToken()) {
		t.Fatal("VerifyOwner(rawOwner) returned false")
	}
	if st.VerifyOwner("not-the-owner") {
		t.Fatal("VerifyOwner accepted bogus token")
	}

	tok, err := st.AgentToken("ag_alice")
	if err != nil {
		t.Fatalf("AgentToken: %v", err)
	}
	if tok == "" {
		t.Fatal("agent token: empty")
	}
	id, ok := st.LookupAgent(tok)
	if !ok || id != "ag_alice" {
		t.Fatalf("LookupAgent: got (%q,%v), want (ag_alice,true)", id, ok)
	}

	// Calling AgentToken again must return the same token (idempotent
	// in-memory; the raw is cached for THIS boot).
	if again, _ := st.AgentToken("ag_alice"); again != tok {
		t.Fatalf("AgentToken not idempotent: %q vs %q", again, tok)
	}

	// Reload from disk: the hash-only path means the raw owner token
	// is gone but VerifyOwner still works (and LookupAgent on the
	// previously-issued raw still resolves through hash comparison).
	originalOwner := st.OwnerToken()
	st2, err := NewTokenStore(dir, nil, "")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if id, ok := st2.LookupAgent(tok); !ok || id != "ag_alice" {
		t.Fatalf("after reopen: got (%q,%v)", id, ok)
	}
	if !st2.VerifyOwner(originalOwner) {
		t.Fatal("post-reopen VerifyOwner(originalOwner) returned false")
	}
	if got := st2.OwnerToken(); got != "" {
		t.Errorf("post-reopen OwnerToken should be \"\", got %q", got)
	}
	// Agent's raw should also be gone post-reopen — caller gets
	// ErrTokenRawUnavailable instead of an out-of-thin-air token.
	if _, err := st2.AgentToken("ag_alice"); err == nil {
		t.Error("post-reopen AgentToken should error (raw not in memory)")
	}

	st.RemoveAgentToken("ag_alice")
	if _, ok := st.LookupAgent(tok); ok {
		t.Fatal("agent token still present after Remove")
	}
	if _, err := st.AgentToken(""); err == nil {
		t.Fatal("AgentToken(\"\") expected error")
	}
}

func TestTokenStore_LegacyMigration(t *testing.T) {
	dir := t.TempDir()
	// Pre-populate a legacy raw owner.token + raw agent token.
	owner := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(filepath.Join(dir, "owner.token"), []byte(owner+"\n"), 0o600); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "agent_tokens"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rawAgent := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	if err := os.WriteFile(filepath.Join(dir, "agent_tokens", "ag_a"), []byte(rawAgent+"\n"), 0o600); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	st, err := NewTokenStore(dir, nil, "")
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	// Migration boot: raw is still available so console URL print works.
	if got := st.OwnerToken(); got != owner {
		t.Errorf("post-migration owner = %q, want %q", got, owner)
	}
	if got, err := st.AgentToken("ag_a"); err != nil || got != rawAgent {
		t.Errorf("post-migration agent = (%q, %v), want (%q, nil)", got, err, rawAgent)
	}
	// Disk file must now carry the hashed prefix, not the raw token.
	data, err := os.ReadFile(filepath.Join(dir, "owner.token"))
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	body := string(data)
	if !startsWith(body, "sha256:") {
		t.Errorf("post-migration owner.token body = %q; want sha256: prefix", body)
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func TestTokenStore_OverrideOwner(t *testing.T) {
	dir := t.TempDir()
	st, err := NewTokenStore(dir, nil, "deadbeef")
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	if got := st.OwnerToken(); got != "deadbeef" {
		t.Fatalf("override owner: got %q", got)
	}
	// Override owner is NOT persisted to disk.
	if _, err := os.Stat(filepath.Join(dir, "owner.token")); err == nil {
		t.Fatal("override owner should not write owner.token")
	}
}

func TestResolver_Roles(t *testing.T) {
	dir := t.TempDir()
	st, err := NewTokenStore(dir, nil, "owner-secret")
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	aliceTok, _ := st.AgentToken("ag_alice")
	bobTok, _ := st.AgentToken("ag_bob")

	r := NewResolver(st, func(id string) bool { return id == "ag_bob" })

	cases := []struct {
		name string
		tok  string
		role Role
		id   string
	}{
		{"empty=guest", "", RoleGuest, ""},
		{"unknown=guest", "garbage", RoleGuest, ""},
		{"owner", "owner-secret", RoleOwner, ""},
		{"alice=agent", aliceTok, RoleAgent, "ag_alice"},
		{"bob=privagent", bobTok, RolePrivAgent, "ag_bob"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := r.Resolve(c.tok)
			if p.Role != c.role || p.AgentID != c.id {
				t.Fatalf("Resolve(%q) = %+v, want role=%v id=%q", c.tok, p, c.role, c.id)
			}
		})
	}
}

func TestPrincipal_Caps(t *testing.T) {
	owner := Principal{Role: RoleOwner}
	priv := Principal{Role: RolePrivAgent, AgentID: "ag_x"}
	ag := Principal{Role: RoleAgent, AgentID: "ag_x"}
	guest := Principal{Role: RoleGuest}

	if !owner.CanReadFull("any") || !priv.CanReadFull("ag_x") || priv.CanReadFull("ag_y") {
		t.Fatal("CanReadFull")
	}
	if !ag.CanReadFull("ag_x") || ag.CanReadFull("ag_y") {
		t.Fatal("CanReadFull(agent self only)")
	}
	if guest.CanReadFull("ag_x") {
		t.Fatal("guest CanReadFull")
	}
	if !priv.CanDeleteOrReset("ag_y") || !owner.CanDeleteOrReset("ag_x") {
		t.Fatal("priv/owner can delete others")
	}
	if ag.CanDeleteOrReset("ag_y") {
		t.Fatal("agent must not delete others")
	}
	if priv.CanForkOrCreate() || ag.CanForkOrCreate() {
		t.Fatal("only owner forks/creates")
	}
	if priv.CanSetPrivileged() {
		t.Fatal("priv must not set privileged")
	}
}

func TestAllowNonOwner_Whitelist(t *testing.T) {
	owner := Principal{Role: RoleOwner}
	ag := Principal{Role: RoleAgent, AgentID: "ag_x"}
	priv := Principal{Role: RolePrivAgent, AgentID: "ag_x"}
	guest := Principal{Role: RoleGuest}

	cases := []struct {
		method, path string
		p            Principal
		want         bool
	}{
		// owner always allowed
		{http.MethodPost, "/api/v1/agents", owner, true},
		// public reads
		{http.MethodGet, "/api/v1/info", ag, true},
		{http.MethodGet, "/api/v1/agents", ag, true},
		{http.MethodGet, "/api/v1/agents/directory", ag, true},
		{http.MethodGet, "/api/v1/agents/ag_x", ag, true},
		{http.MethodGet, "/api/v1/agents/ag_y/avatar", ag, true},
		// agent self-mutation
		{http.MethodPatch, "/api/v1/agents/ag_x", ag, true},
		{http.MethodPatch, "/api/v1/agents/ag_y", ag, false},
		{http.MethodPost, "/api/v1/agents/ag_x/reset", ag, true},
		{http.MethodPost, "/api/v1/agents/ag_y/reset", ag, false},
		// privileged: cross-agent delete/reset
		{http.MethodDelete, "/api/v1/agents/ag_y", priv, true},
		{http.MethodPost, "/api/v1/agents/ag_y/reset", priv, true},
		// fork & privilege are owner only
		{http.MethodPost, "/api/v1/agents/ag_y/fork", priv, false},
		{http.MethodPost, "/api/v1/agents/ag_x/fork", ag, false},
		{http.MethodPost, "/api/v1/agents/ag_x/privilege", priv, false},
		// create is owner only
		{http.MethodPost, "/api/v1/agents", ag, false},
		// session/git/etc. are owner only
		{http.MethodGet, "/api/v1/sessions", ag, false},
		{http.MethodGet, "/api/v1/git/status", priv, false},
		{http.MethodGet, "/api/v1/files", ag, false},
		// self-scoped sub-resources for the agent
		{http.MethodGet, "/api/v1/agents/ag_x/messages", ag, true},
		{http.MethodGet, "/api/v1/agents/ag_y/messages", ag, false},
		{http.MethodGet, "/api/v1/agents/ag_x/credentials", ag, true},
		{http.MethodPost, "/api/v1/agents/ag_x/credentials", ag, true},
		{http.MethodPatch, "/api/v1/agents/ag_x/credentials/cred_1", ag, true},
		{http.MethodGet, "/api/v1/agents/ag_x/tasks", ag, true},
		{http.MethodPost, "/api/v1/agents/ag_x/tasks", ag, true},
		{http.MethodGet, "/api/v1/agents/ag_x/notify-sources", ag, true},
		{http.MethodGet, "/api/v1/agents/ag_x/slackbot", ag, true},
		{http.MethodPut, "/api/v1/agents/ag_x/slackbot", ag, true},
		{http.MethodPost, "/api/v1/agents/ag_x/pre-compact", ag, true},
		// MCP transport — agent's own tool surface
		{http.MethodPost, "/api/v1/agents/ag_x/mcp", ag, true},
		{http.MethodPost, "/api/v1/agents/ag_y/mcp", ag, false},
		// per-agent ws — only self
		{http.MethodGet, "/api/v1/agents/ag_x/ws", ag, true},
		{http.MethodGet, "/api/v1/agents/ag_y/ws", ag, false},
		{http.MethodGet, "/api/v1/agents/ag_y/ws", guest, false},
		// global ws — owner only
		{http.MethodGet, "/api/v1/ws", ag, false},
		{http.MethodGet, "/api/v1/ws", priv, false},
		// group DM creation — agents may create (handler enforces
		// caller-in-memberIds); guests must 403.
		{http.MethodPost, "/api/v1/groupdms", ag, true},
		{http.MethodPost, "/api/v1/groupdms", priv, true},
		{http.MethodPost, "/api/v1/groupdms", guest, false},
		// Bare /api/v1/groupdms is POST-only for non-Owner. GET (list)
		// and DELETE (the route doesn't exist but should still 403)
		// must stay denied so the Agent path doesn't widen by accident.
		{http.MethodGet, "/api/v1/groupdms", ag, false},
		{http.MethodGet, "/api/v1/groupdms", priv, false},
		{http.MethodDelete, "/api/v1/groupdms", ag, false},
		{http.MethodPatch, "/api/v1/groupdms", ag, false},
		// guest sees only directory/info
		{http.MethodGet, "/api/v1/info", guest, true},
		{http.MethodGet, "/api/v1/agents/directory", guest, true},
		{http.MethodGet, "/api/v1/agents", guest, true},
		{http.MethodPatch, "/api/v1/agents/ag_x", guest, false},
		// Agent peer list: agents must reach the list endpoint
		// (with a reduced view enforced by the handler) so a
		// human prompt can name a target by tailscale machine
		// name. Other /api/v1/peers/* routes stay owner-only.
		{http.MethodGet, "/api/v1/peers", ag, true},
		{http.MethodGet, "/api/v1/peers", guest, false},
		{http.MethodPost, "/api/v1/peers", ag, false},
		{http.MethodDelete, "/api/v1/peers/abc", ag, false},
		{http.MethodGet, "/api/v1/peers/self", ag, false},
		// §3.7 agent-self orchestrated device switch — only the
		// agent itself may migrate its own data; another agent
		// must NOT be able to push someone else's blobs to a
		// peer they don't own. Owner is permitted by the
		// IsOwner short-circuit above the AllowNonOwner gate.
		{http.MethodPost, "/api/v1/agents/ag_x/handoff/switch", ag, true},
		{http.MethodPost, "/api/v1/agents/ag_y/handoff/switch", ag, false},
		{http.MethodPost, "/api/v1/agents/ag_y/handoff/switch", guest, false},
		{http.MethodPost, "/api/v1/agents/ag_x/handoff/begin", ag, false},
		{http.MethodPost, "/api/v1/agents/ag_x/handoff/complete", ag, false},
		{http.MethodPost, "/api/v1/agents/ag_x/handoff/abort", ag, false},
		// §3.7 step 4 target-side pull endpoint. RolePeer is
		// the production dispatcher (Hub signs as its own peer
		// identity). RoleAgent / Guest must NOT reach it —
		// the orchestrator wraps it for the agent.
		{http.MethodPost, "/api/v1/peers/pull",
			Principal{Role: RolePeer, PeerID: "src-device-0"}, true},
		{http.MethodGet, "/api/v1/peers/pull",
			Principal{Role: RolePeer, PeerID: "src-device-0"}, false},
		{http.MethodPost, "/api/v1/peers/pull", ag, false},
		{http.MethodPost, "/api/v1/peers/pull", guest, false},
		// §3.7 agent-sync surfaces. Same trust model: RolePeer
		// only (handler enforces signer-equals-source + holder
		// check). Agent / Guest principals MUST be denied —
		// they have no legitimate reason to reach these routes,
		// and a leaked agent token shouldn't let the caller
		// probe target state or commit cross-peer mutations.
		{http.MethodPost, "/api/v1/peers/agent-sync",
			Principal{Role: RolePeer, PeerID: "src-device-0"}, true},
		{http.MethodPost, "/api/v1/peers/agent-sync", ag, false},
		{http.MethodPost, "/api/v1/peers/agent-sync", guest, false},
		{http.MethodPost, "/api/v1/peers/agent-sync/state",
			Principal{Role: RolePeer, PeerID: "src-device-0"}, true},
		{http.MethodPost, "/api/v1/peers/agent-sync/state", ag, false},
		{http.MethodPost, "/api/v1/peers/agent-sync/state", guest, false},
		{http.MethodGet, "/api/v1/peers/agent-sync/state",
			Principal{Role: RolePeer, PeerID: "src-device-0"}, false},
		{http.MethodPost, "/api/v1/peers/agent-sync/finalize",
			Principal{Role: RolePeer, PeerID: "src-device-0"}, true},
		{http.MethodPost, "/api/v1/peers/agent-sync/finalize", ag, false},
		{http.MethodPost, "/api/v1/peers/agent-sync/finalize", guest, false},
		{http.MethodPost, "/api/v1/peers/agent-sync/drop",
			Principal{Role: RolePeer, PeerID: "src-device-0"}, true},
		{http.MethodPost, "/api/v1/peers/agent-sync/drop", ag, false},
		{http.MethodPost, "/api/v1/peers/agent-sync/drop", guest, false},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path+"/"+roleName(c.p.Role), func(t *testing.T) {
			got := AllowNonOwner(c.p, c.method, c.path)
			if got != c.want {
				t.Fatalf("AllowNonOwner(%s %s, role=%v) = %v, want %v", c.method, c.path, c.p.Role, got, c.want)
			}
		})
	}
}

func roleName(r Role) string {
	switch r {
	case RoleOwner:
		return "owner"
	case RolePrivAgent:
		return "priv"
	case RoleAgent:
		return "agent"
	default:
		return "guest"
	}
}

func TestAuthMiddleware_PrincipalInContext(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewTokenStore(dir, nil, "owner-x")
	tok, _ := st.AgentToken("ag_alice")
	r := NewResolver(st, func(string) bool { return false })

	var got Principal
	h := AuthMiddleware(r)(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got = FromContext(req.Context())
	}))

	// owner via Authorization header
	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	req.Header.Set("Authorization", "Bearer owner-x")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got.Role != RoleOwner {
		t.Fatalf("owner: got role %v", got.Role)
	}

	// agent via X-Kojo-Token
	req = httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	req.Header.Set("X-Kojo-Token", tok)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got.Role != RoleAgent || got.AgentID != "ag_alice" {
		t.Fatalf("agent: got %+v", got)
	}

	// guest with no header
	req = httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got.Role != RoleGuest {
		t.Fatalf("guest: got role %v", got.Role)
	}
}

func TestEnforceMiddleware_403ForNonOwner(t *testing.T) {
	denied := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	stack := EnforceMiddleware(denied)

	// guest reaching sessions endpoint must 403
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	req = req.WithContext(WithPrincipal(context.Background(), Principal{Role: RoleGuest}))
	w := httptest.NewRecorder()
	stack.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("guest sessions: code %d", w.Code)
	}
	// owner reaches handler
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	req = req.WithContext(WithPrincipal(context.Background(), Principal{Role: RoleOwner}))
	w = httptest.NewRecorder()
	stack.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("owner sessions: code %d", w.Code)
	}
}

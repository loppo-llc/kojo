//go:build heavy_test

// These tests boot a full agent.Manager (cron scheduler, notify poller,
// SQLite credential store) per test and are gated behind the
// `heavy_test` build tag so a normal `go test ./...` does not OOM
// resource-constrained dev hosts. Run with:
//
//	go test -tags heavy_test ./internal/server/...

package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

// newTestServer builds a Server backed by a fresh tempdir-rooted
// configdir / agent manager. Returns the server and the agent manager
// so the caller can pre-populate agents.
//
// Isolation strategy: each test gets its own $HOME pointing to t.TempDir.
// configdir.Path() resolves via os.UserHomeDir() when no override is set,
// so HOME-redirection gives true per-test directories. Calling
// configdir.Set() would be one-shot (sync.Once) and would leak state
// across tests in the same package.
//
// The manager spawns background goroutines (cron, notify poller, slack
// hub-less here) that must be torn down via mgr.Shutdown() — registered
// via t.Cleanup so a failed assertion does not strand them.
func newTestServer(t *testing.T) (*Server, *agent.Manager) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// On macOS os.UserHomeDir prefers HOME but some platforms also read
	// USERPROFILE / XDG_CONFIG_HOME. Override the obvious ones too so a
	// future change to configdir does not silently re-share state.
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp+"/.config")

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	mgr, err := agent.NewManager(logger)
	if err != nil {
		t.Fatalf("agent.NewManager: %v", err)
	}
	t.Cleanup(func() {
		mgr.Shutdown()
		_ = mgr.Close()
	})

	srv := New(Config{
		Addr:           ":0",
		Logger:         logger,
		Version:        "test",
		AgentManager:   mgr,
		GroupDMManager: nil,
	})
	return srv, mgr
}

func mkReq(method, path string, body string, p auth.Principal) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	return r.WithContext(auth.WithPrincipal(context.Background(), p))
}

// extractFirstAgent decodes {agents: [...]} and returns the first
// element as a generic map for field inspection.
func extractFirstAgent(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var resp struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, body)
	}
	if len(resp.Agents) == 0 {
		t.Fatalf("expected at least 1 agent in response, got 0\nbody=%s", body)
	}
	return resp.Agents[0]
}

func TestListAgents_OwnerSeesPersona(t *testing.T) {
	srv, mgr := newTestServer(t)
	a, err := mgr.Create(agent.AgentConfig{Name: "alice", Persona: "very secret persona", Tool: "claude", Model: "sonnet"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_ = a

	req := mkReq(http.MethodGet, "/api/v1/agents", "", auth.Principal{Role: auth.RoleOwner})
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	got := extractFirstAgent(t, w.Body.Bytes())
	if got["persona"] != "very secret persona" {
		t.Errorf("owner should see persona, got %v", got["persona"])
	}
}

func TestListAgents_AgentSelfFullOthersDirectory(t *testing.T) {
	srv, mgr := newTestServer(t)
	alice, _ := mgr.Create(agent.AgentConfig{Name: "alice", Persona: "alice persona internal", Tool: "claude", Model: "sonnet"})
	bob, _ := mgr.Create(agent.AgentConfig{Name: "bob", Persona: "bob persona internal", Tool: "claude", Model: "sonnet"})

	// Force PublicProfile so directory view has something visible.
	if _, err := mgr.Update(bob.ID, agent.AgentUpdateConfig{
		PublicProfile:         strPtr("bob short"),
		PublicProfileOverride: boolPtr(true),
	}); err != nil {
		t.Fatalf("set bob profile: %v", err)
	}

	req := mkReq(http.MethodGet, "/api/v1/agents", "", auth.Principal{Role: auth.RoleAgent, AgentID: alice.ID})
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Agents) != 2 {
		t.Fatalf("want 2 agents, got %d", len(resp.Agents))
	}

	var aliceView, bobView map[string]any
	for _, ag := range resp.Agents {
		switch ag["id"] {
		case alice.ID:
			aliceView = ag
		case bob.ID:
			bobView = ag
		}
	}
	if aliceView["persona"] != "alice persona internal" {
		t.Errorf("alice should see her own persona, got %v", aliceView["persona"])
	}
	if _, hasPersona := bobView["persona"]; hasPersona {
		t.Errorf("alice must not see bob.persona; got %v", bobView["persona"])
	}
	if bobView["publicProfile"] != "bob short" {
		t.Errorf("alice should see bob.publicProfile, got %v", bobView["publicProfile"])
	}
}

func TestGetAgent_DirectoryViewForNonOwnerOnOthers(t *testing.T) {
	srv, mgr := newTestServer(t)
	bob, _ := mgr.Create(agent.AgentConfig{Name: "bob", Persona: "bob secret", Tool: "claude", Model: "sonnet"})

	req := mkReq(http.MethodGet, "/api/v1/agents/"+bob.ID, "",
		auth.Principal{Role: auth.RoleAgent, AgentID: "ag_other"})
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, has := got["persona"]; has {
		t.Fatalf("non-owner non-self must not see persona, got %v", got["persona"])
	}
	if got["id"] != bob.ID {
		t.Errorf("id field: %v", got["id"])
	}
}

// TestGetAgent_NextCronAtSurvivesGlobalPause locks in the v1 fix: when
// the Dashboard's global cron toggle is paused, GET /agents/{id} must
// still surface the agent's configured next-tick (`nextCronAt`) alongside
// a `cronPausedGlobal: true` indicator. Hiding the time was the bug —
// Settings then rendered "Next check-in: —" and the user read it as a
// missing-value regression.
func TestGetAgent_NextCronAtSurvivesGlobalPause(t *testing.T) {
	srv, mgr := newTestServer(t)
	a, err := mgr.Create(agent.AgentConfig{Name: "t", Tool: "claude", Model: "sonnet"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	expr := "*/5 * * * *"
	if _, err := mgr.Update(a.ID, agent.AgentUpdateConfig{CronExpr: &expr}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	mgr.StartSchedulers()
	// Cron entries get their Next computed asynchronously via the
	// scheduler goroutine — poll the response field rather than racing
	// a fixed sleep so this test stays robust on slow CI.
	if err := mgr.SetCronPaused(true); err != nil {
		t.Fatalf("SetCronPaused: %v", err)
	}

	var got map[string]any
	for i := 0; i < 50; i++ {
		req := mkReq(http.MethodGet, "/api/v1/agents/"+a.ID, "",
			auth.Principal{Role: auth.RoleOwner})
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, has := got["nextCronAt"]; has {
			break
		}
		// allow scheduler goroutine to populate entry.Next
	}
	if _, has := got["nextCronAt"]; !has {
		t.Fatalf("nextCronAt missing under global pause; body=%v", got)
	}
	if v, _ := got["cronPausedGlobal"].(bool); !v {
		t.Fatalf("cronPausedGlobal=%v, want true; body=%v", got["cronPausedGlobal"], got)
	}
}

// TestGetAgent_ETagFlipsOnCronPause locks in the GET-side composite
// ETag: toggling the global pause must invalidate the client's cached
// representation (cronPausedGlobal in the body changes without bumping
// the agent row's etag). Without the `.p` suffix, the 304 fast path
// would silently serve a stale paused indicator.
func TestGetAgent_ETagFlipsOnCronPause(t *testing.T) {
	srv, mgr := newTestServer(t)
	a, err := mgr.Create(agent.AgentConfig{Name: "t", Tool: "claude", Model: "sonnet"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	doGet := func() (status int, etag string) {
		req := mkReq(http.MethodGet, "/api/v1/agents/"+a.ID, "",
			auth.Principal{Role: auth.RoleOwner})
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		return w.Code, w.Header().Get("ETag")
	}

	_, runningETag := doGet()
	if runningETag == "" {
		t.Skip("server returned no ETag (store not wired?)")
	}

	if err := mgr.SetCronPaused(true); err != nil {
		t.Fatalf("SetCronPaused: %v", err)
	}
	_, pausedETag := doGet()
	if pausedETag == runningETag {
		t.Fatalf("ETag did not flip on pause: %q == %q", pausedETag, runningETag)
	}
	if !strings.HasSuffix(strings.Trim(pausedETag, `"`), ".p") {
		t.Errorf("paused ETag should carry .p suffix, got %q", pausedETag)
	}
}

func TestUpdateAgent_RejectsCrossSelfAndPrivilegedField(t *testing.T) {
	srv, mgr := newTestServer(t)
	alice, _ := mgr.Create(agent.AgentConfig{Name: "alice", Tool: "claude", Model: "sonnet"})
	bob, _ := mgr.Create(agent.AgentConfig{Name: "bob", Tool: "claude", Model: "sonnet"})

	// Alice trying to edit bob: forbidden.
	req := mkReq(http.MethodPatch, "/api/v1/agents/"+bob.ID, `{"name":"hacked"}`,
		auth.Principal{Role: auth.RoleAgent, AgentID: alice.ID})
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-edit: want 403, got %d (%s)", w.Code, w.Body.String())
	}

	// Alice trying to set Privileged=true on herself: forbidden.
	req = mkReq(http.MethodPatch, "/api/v1/agents/"+alice.ID, `{"name":"alice","privileged":true}`,
		auth.Principal{Role: auth.RoleAgent, AgentID: alice.ID})
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("self-privilege smuggle: want 403, got %d (%s)", w.Code, w.Body.String())
	}

	// Owner setting Privileged via dedicated endpoint: ok.
	req = mkReq(http.MethodPost, "/api/v1/agents/"+alice.ID+"/privilege", `{"privileged":true}`,
		auth.Principal{Role: auth.RoleOwner})
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("owner privilege: status %d (%s)", w.Code, w.Body.String())
	}
	if !mgr.IsPrivileged(alice.ID) {
		t.Fatalf("manager state: alice should be privileged after POST /privilege")
	}

	// Agent calling /privilege on self: forbidden.
	req = mkReq(http.MethodPost, "/api/v1/agents/"+alice.ID+"/privilege", `{"privileged":false}`,
		auth.Principal{Role: auth.RolePrivAgent, AgentID: alice.ID})
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("priv revoke self: want 403, got %d", w.Code)
	}
}

func TestDeleteAgent_PrivAgentCanDeleteOthers(t *testing.T) {
	srv, mgr := newTestServer(t)
	alice, _ := mgr.Create(agent.AgentConfig{Name: "alice", Tool: "claude", Model: "sonnet"})
	bob, _ := mgr.Create(agent.AgentConfig{Name: "bob", Tool: "claude", Model: "sonnet"})

	// Regular Agent alice -> delete bob: forbidden.
	req := mkReq(http.MethodDelete, "/api/v1/agents/"+bob.ID, "",
		auth.Principal{Role: auth.RoleAgent, AgentID: alice.ID})
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("regular agent cross-delete: want 403, got %d", w.Code)
	}
	if _, ok := mgr.Get(bob.ID); !ok {
		t.Fatal("bob should still exist after rejected delete")
	}

	// Privileged agent alice -> delete bob: ok.
	req = mkReq(http.MethodDelete, "/api/v1/agents/"+bob.ID, "",
		auth.Principal{Role: auth.RolePrivAgent, AgentID: alice.ID})
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("priv agent delete: status %d (%s)", w.Code, w.Body.String())
	}
	if _, ok := mgr.Get(bob.ID); ok {
		t.Fatal("bob should be gone after priv delete")
	}
}

func TestForkAgent_AgentForbidden(t *testing.T) {
	srv, mgr := newTestServer(t)
	alice, _ := mgr.Create(agent.AgentConfig{Name: "alice", Tool: "claude", Model: "sonnet"})

	// Even privileged agent cannot fork (would copy persona/memory).
	req := mkReq(http.MethodPost, "/api/v1/agents/"+alice.ID+"/fork", `{"name":"alice2"}`,
		auth.Principal{Role: auth.RolePrivAgent, AgentID: alice.ID})
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("priv fork: want 403, got %d", w.Code)
	}
}

// TestUpdateAgent_IfMatch covers the four observable If-Match outcomes
// on PATCH /api/v1/agents/{id}:
//
//   - malformed precondition (weak etag) → 400
//   - non-matching strong etag → 412
//   - matching strong etag → 200, response carries the new ETag header,
//     and a follow-up PATCH using that header also succeeds (proving the
//     etag echoed back is the current row, not a stale snapshot)
//   - wildcard `*` → 200 regardless of current etag (used by Web UI for
//     "force overwrite" admin paths)
//
// The owner principal is used so privilege gating doesn't interact with
// the precondition check.
func TestUpdateAgent_IfMatch(t *testing.T) {
	srv, mgr := newTestServer(t)
	alice, err := mgr.Create(agent.AgentConfig{Name: "alice", Tool: "claude", Model: "sonnet"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	getETag := func(t *testing.T) string {
		t.Helper()
		req := mkReq(http.MethodGet, "/api/v1/agents/"+alice.ID, "",
			auth.Principal{Role: auth.RoleOwner})
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("seed GET: status %d (%s)", w.Code, w.Body.String())
		}
		et := w.Header().Get("ETag")
		if et == "" {
			t.Fatal("seed GET missing ETag header (store not wired?)")
		}
		return et
	}

	doPatch := func(t *testing.T, ifMatch, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := mkReq(http.MethodPatch, "/api/v1/agents/"+alice.ID, body,
			auth.Principal{Role: auth.RoleOwner})
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		return w
	}

	// 1) malformed -> 400
	if w := doPatch(t, `W/"3-deadbeef"`, `{"name":"alice2"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("weak If-Match: want 400, got %d (%s)", w.Code, w.Body.String())
	}

	// 2) mismatch -> 412
	if w := doPatch(t, `"v0-cafebabe"`, `{"name":"alice2"}`); w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: want 412, got %d (%s)", w.Code, w.Body.String())
	}

	// 3) match -> 200, ETag header reflects new version
	current := getETag(t)
	w := doPatch(t, current, `{"name":"alice2"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("matching If-Match: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	newETag := w.Header().Get("ETag")
	if newETag == "" {
		t.Fatal("matching If-Match: response missing ETag header")
	}
	if newETag == current {
		t.Fatalf("etag should advance after PATCH; both = %q", newETag)
	}
	// Chain another PATCH with the just-issued etag — proves it's the
	// current row, not a cached pre-image.
	if w := doPatch(t, newETag, `{"name":"alice3"}`); w.Code != http.StatusOK {
		t.Fatalf("chained If-Match: want 200, got %d (%s)", w.Code, w.Body.String())
	}

	// 4) wildcard accepted when row exists in store
	if w := doPatch(t, `*`, `{"name":"alice4"}`); w.Code != http.StatusOK {
		t.Fatalf("wildcard If-Match: want 200, got %d (%s)", w.Code, w.Body.String())
	}
}

// TestUpdateAgent_IfMatchConcurrent verifies that two PATCH requests
// arriving with the same If-Match etag cannot both succeed. Without
// the per-agent patch lock the precheck would race (both reads return
// the same etag, both pass), so this is the regression guard for
// Codex's slice-2 critical finding.
func TestUpdateAgent_IfMatchConcurrent(t *testing.T) {
	srv, mgr := newTestServer(t)
	alice, _ := mgr.Create(agent.AgentConfig{Name: "alice", Tool: "claude", Model: "sonnet"})

	// Seed etag.
	seedReq := mkReq(http.MethodGet, "/api/v1/agents/"+alice.ID, "",
		auth.Principal{Role: auth.RoleOwner})
	seedW := httptest.NewRecorder()
	srv.mux.ServeHTTP(seedW, seedReq)
	if seedW.Code != http.StatusOK {
		t.Fatalf("seed GET: %d", seedW.Code)
	}
	startETag := seedW.Header().Get("ETag")
	if startETag == "" {
		t.Fatal("seed ETag empty")
	}

	// Fire N concurrent PATCHes all carrying the same starting etag.
	// Exactly one should win with 200; the rest must fail with 412.
	// N kept small (was 8) because newTestServer boots a full Manager
	// per test (cron + notify poller + 2 SQLite handles) and stacking
	// many goroutines under -race blew memory on the dev host.
	const N = 3
	codes := make(chan int, N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		go func() {
			<-start
			req := mkReq(http.MethodPatch, "/api/v1/agents/"+alice.ID,
				`{"name":"contender"}`, auth.Principal{Role: auth.RoleOwner})
			req.Header.Set("If-Match", startETag)
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, req)
			codes <- w.Code
		}()
	}
	close(start)

	wins, fails := 0, 0
	for i := 0; i < N; i++ {
		switch c := <-codes; c {
		case http.StatusOK:
			wins++
		case http.StatusPreconditionFailed:
			fails++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if wins != 1 {
		t.Fatalf("want exactly 1 winner, got %d (fails=%d)", wins, fails)
	}
	if fails != N-1 {
		t.Fatalf("want %d losers, got %d", N-1, fails)
	}
}

// TestUpdateAgent_IfMatchOnArchivedAgent: archive should not bypass
// If-Match. The mutation might still be accepted (Manager.Update has
// its own archived-handling), but the precondition path must still
// gate it.
func TestUpdateAgent_IfMatchOnArchivedAgent(t *testing.T) {
	srv, mgr := newTestServer(t)
	alice, _ := mgr.Create(agent.AgentConfig{Name: "alice", Tool: "claude", Model: "sonnet"})
	if err := mgr.Archive(alice.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	// Stale etag against an archived agent: must still be 412 (the
	// precondition is what failed; archival state is orthogonal).
	req := mkReq(http.MethodPatch, "/api/v1/agents/"+alice.ID,
		`{"name":"x"}`, auth.Principal{Role: auth.RoleOwner})
	req.Header.Set("If-Match", `"v999-deadbeef"`)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("archived + stale If-Match: want 412, got %d (%s)", w.Code, w.Body.String())
	}
}

// TestUpdateMessage_IfMatch covers PATCH /api/v1/agents/{id}/messages/{msgId}
// with If-Match. Unlike the agent-update path, message edits go through
// the v1 store transactionally — the precondition check is atomic with
// the UPDATE, so no per-handler mutex is needed and the concurrent-PATCH
// guarantee falls out of SQLite serializing on the row's etag.
//
// Cases:
//   - malformed (weak etag) → 400
//   - wildcard `*` rejected as unsupported → 400
//   - mismatch → 412
//   - match → 200 + new ETag header
//   - chained PATCH using the just-issued ETag → 200
//   - N concurrent same-ETag PATCHes → exactly one 200, rest 412
func TestUpdateMessage_IfMatch(t *testing.T) {
	srv, mgr := newTestServer(t)
	alice, err := mgr.Create(agent.AgentConfig{Name: "alice", Tool: "llama.cpp", Model: "local-test", CustomBaseURL: "http://localhost:0"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Seed a single user message via the store directly; the public
	// agent.Manager Chat path requires a live llama.cpp backend which
	// these unit tests don't have.
	st := mgr.Store()
	if st == nil {
		t.Fatal("Manager.Store() is nil — message tests need a wired-up DB")
	}
	rec, err := st.AppendMessage(context.Background(), &store.MessageRecord{
		ID: "msg_test_1", AgentID: alice.ID, Role: "user", Content: "hello",
	}, store.MessageInsertOptions{})
	if err != nil {
		t.Fatalf("seed AppendMessage: %v", err)
	}
	currentETag := `"` + rec.ETag + `"`

	doPatch := func(t *testing.T, ifMatch, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := mkReq(http.MethodPatch,
			"/api/v1/agents/"+alice.ID+"/messages/"+rec.ID, body,
			auth.Principal{Role: auth.RoleOwner})
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		return w
	}

	// 1) malformed -> 400
	if w := doPatch(t, `W/"v1-bad"`, `{"content":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("weak If-Match: want 400, got %d (%s)", w.Code, w.Body.String())
	}

	// 2) wildcard rejected
	if w := doPatch(t, `*`, `{"content":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("wildcard: want 400, got %d (%s)", w.Code, w.Body.String())
	}

	// 3) mismatch -> 412
	if w := doPatch(t, `"v0-cafebabe"`, `{"content":"x"}`); w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: want 412, got %d (%s)", w.Code, w.Body.String())
	}

	// 4) match -> 200, new ETag
	w := doPatch(t, currentETag, `{"content":"edited once"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("matching If-Match: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	newETag := w.Header().Get("ETag")
	if newETag == "" {
		t.Fatal("matching If-Match: missing ETag header")
	}
	if newETag == currentETag {
		t.Fatalf("etag should advance; got same %q", newETag)
	}

	// 5) chained PATCH with new etag
	w2 := doPatch(t, newETag, `{"content":"edited twice"}`)
	if w2.Code != http.StatusOK {
		t.Fatalf("chained PATCH: want 200, got %d (%s)", w2.Code, w2.Body.String())
	}
	twiceETag := w2.Header().Get("ETag")
	if twiceETag == "" || twiceETag == newETag {
		t.Fatalf("chain ETag did not advance: %q -> %q", newETag, twiceETag)
	}

	// 6) N concurrent same-ETag PATCHes — exactly one wins.
	//    Losers may surface as 412 (store-level If-Match mismatch
	//    when they reach the SQLite UPDATE) OR 409 (the per-agent
	//    editing flag held by acquireTranscriptEdit refused entry
	//    before the store check ran). Both are valid "the precondition
	//    you sent would not have produced a write" outcomes; the
	//    409-vs-412 mix depends on goroutine scheduling. Don't assert
	//    on the split — only that exactly one writer survives.
	//    N kept small because newTestServer is heavy (full Manager
	//    per test); see TestUpdateAgent_IfMatchConcurrent rationale.
	const N = 3
	codes := make(chan int, N)
	startCh := make(chan struct{})
	for i := 0; i < N; i++ {
		go func() {
			<-startCh
			req := mkReq(http.MethodPatch,
				"/api/v1/agents/"+alice.ID+"/messages/"+rec.ID,
				`{"content":"race"}`, auth.Principal{Role: auth.RoleOwner})
			req.Header.Set("If-Match", twiceETag)
			rr := httptest.NewRecorder()
			srv.mux.ServeHTTP(rr, req)
			codes <- rr.Code
		}()
	}
	close(startCh)
	wins, fails, others := 0, 0, 0
	for i := 0; i < N; i++ {
		switch c := <-codes; c {
		case http.StatusOK:
			wins++
		case http.StatusPreconditionFailed, http.StatusConflict:
			fails++
		default:
			others++
			t.Logf("concurrent: unexpected status %d", c)
		}
	}
	if wins != 1 || fails != N-1 || others != 0 {
		t.Fatalf("concurrent PATCH: want 1 win + %d fails (412 or 409), got %d/%d (others=%d)",
			N-1, wins, fails, others)
	}
}

// TestDeleteMessage_IfMatch covers DELETE /api/v1/agents/{id}/messages/{msgId}
// with optional If-Match. The store enforces the precondition atomically
// inside SoftDeleteMessage, so the handler is a thin pass-through. Cases:
//   - no If-Match → 200 (legacy unconditional delete still works)
//   - malformed (weak etag) → 400
//   - wildcard `*` rejected → 400
//   - stale → 412 + row still alive
//   - matching → 200
//   - matching against already-tombstoned row → 404 (NOT 200) so a stale
//     client that didn't see the prior delete refetches instead of thinking
//     its own delete landed
func TestDeleteMessage_IfMatch(t *testing.T) {
	srv, mgr := newTestServer(t)
	alice, err := mgr.Create(agent.AgentConfig{Name: "alice", Tool: "llama.cpp", Model: "local-test", CustomBaseURL: "http://localhost:0"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	st := mgr.Store()
	if st == nil {
		t.Fatal("Manager.Store() is nil")
	}

	seed := func(id string) string {
		t.Helper()
		rec, err := st.AppendMessage(context.Background(), &store.MessageRecord{
			ID: id, AgentID: alice.ID, Role: "user", Content: "hi",
		}, store.MessageInsertOptions{})
		if err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		return `"` + rec.ETag + `"`
	}

	doDelete := func(t *testing.T, msgID, ifMatch string) *httptest.ResponseRecorder {
		t.Helper()
		req := mkReq(http.MethodDelete,
			"/api/v1/agents/"+alice.ID+"/messages/"+msgID, "",
			auth.Principal{Role: auth.RoleOwner})
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		return w
	}

	// 1) no If-Match — legacy unconditional delete still succeeds.
	seed("msg_d_unconditional")
	if w := doDelete(t, "msg_d_unconditional", ""); w.Code != http.StatusOK {
		t.Fatalf("unconditional delete: want 200, got %d (%s)", w.Code, w.Body.String())
	}

	// 2) malformed weak etag → 400
	etag := seed("msg_d_malformed")
	_ = etag
	if w := doDelete(t, "msg_d_malformed", `W/"v1-bad"`); w.Code != http.StatusBadRequest {
		t.Fatalf("weak If-Match: want 400, got %d (%s)", w.Code, w.Body.String())
	}

	// 3) wildcard rejected
	if w := doDelete(t, "msg_d_malformed", `*`); w.Code != http.StatusBadRequest {
		t.Fatalf("wildcard: want 400, got %d (%s)", w.Code, w.Body.String())
	}

	// 4) stale → 412, row still alive
	if w := doDelete(t, "msg_d_malformed", `"v0-stale"`); w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: want 412, got %d (%s)", w.Code, w.Body.String())
	}
	if got, err := st.GetMessage(context.Background(), "msg_d_malformed"); err != nil || got == nil {
		t.Fatalf("row should still be alive after 412; got %v / %v", got, err)
	}

	// 5) matching → 200
	if w := doDelete(t, "msg_d_malformed", etag); w.Code != http.StatusOK {
		t.Fatalf("matching If-Match: want 200, got %d (%s)", w.Code, w.Body.String())
	}

	// 6) conditional delete against vanished row → 404 (distinguish from
	//    silent-success on already-gone). Sending the now-stale etag
	//    against a tombstoned row must NOT be silently OK.
	if w := doDelete(t, "msg_d_malformed", etag); w.Code != http.StatusNotFound {
		t.Fatalf("post-tombstone conditional: want 404, got %d (%s)", w.Code, w.Body.String())
	}

	// 7) bare DELETE on the same vanished row → 404. SoftDeleteMessage
	//    with empty ifMatch is idempotent on missing rows at the store
	//    layer, but the agent.deleteMessage layer does a GetMessage up
	//    front for the cross-agent guard and surfaces ErrMessageNotFound
	//    when the row is gone. The handler maps that to 404. This is
	//    the actual contract — assert it instead of just logging.
	if w := doDelete(t, "msg_d_malformed", ""); w.Code != http.StatusNotFound {
		t.Fatalf("bare DELETE on tombstoned row: want 404, got %d (%s)", w.Code, w.Body.String())
	}
}

// TestRegenerateMessage_IfMatch covers the request-parsing and
// precondition paths of POST /messages/{msgId}/regenerate. The
// success case is intentionally NOT exercised here: regenerate goes
// through Manager.prepareChat and a real llama.cpp backend, which
// these unit tests don't have. The 4xx surfaces — malformed,
// wildcard, stale 412, not-found — all complete BEFORE prepareChat,
// so they're observable without a backend.
func TestRegenerateMessage_IfMatch(t *testing.T) {
	srv, mgr := newTestServer(t)
	alice, err := mgr.Create(agent.AgentConfig{Name: "alice", Tool: "llama.cpp", Model: "local-test", CustomBaseURL: "http://localhost:0"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	st := mgr.Store()
	if st == nil {
		t.Fatal("Manager.Store() is nil")
	}
	// Seed a user message; regenerate on a user msg keeps it as the
	// source, so the etag-check path is fully exercised before the
	// backend (which we don't have) is invoked.
	rec, err := st.AppendMessage(context.Background(), &store.MessageRecord{
		ID: "msg_regen_1", AgentID: alice.ID, Role: "user", Content: "hello",
	}, store.MessageInsertOptions{})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	currentETag := `"` + rec.ETag + `"`

	doRegen := func(t *testing.T, ifMatch string) *httptest.ResponseRecorder {
		t.Helper()
		req := mkReq(http.MethodPost,
			"/api/v1/agents/"+alice.ID+"/messages/"+rec.ID+"/regenerate", "",
			auth.Principal{Role: auth.RoleOwner})
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		return w
	}

	// 1) malformed weak etag → 400
	if w := doRegen(t, `W/"v1-bad"`); w.Code != http.StatusBadRequest {
		t.Fatalf("weak If-Match: want 400, got %d (%s)", w.Code, w.Body.String())
	}

	// 2) wildcard rejected
	if w := doRegen(t, `*`); w.Code != http.StatusBadRequest {
		t.Fatalf("wildcard: want 400, got %d (%s)", w.Code, w.Body.String())
	}

	// 3) stale → 412
	if w := doRegen(t, `"v0-stale"`); w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: want 412, got %d (%s)", w.Code, w.Body.String())
	}

	// 4) not-found target → 404 even with valid-looking etag
	{
		req := mkReq(http.MethodPost,
			"/api/v1/agents/"+alice.ID+"/messages/msg_does_not_exist/regenerate", "",
			auth.Principal{Role: auth.RoleOwner})
		req.Header.Set("If-Match", currentETag)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("not-found target: want 404, got %d (%s)", w.Code, w.Body.String())
		}
	}

	// 5) post-tombstone conditional → 404 (the pivot guard inside the
	//    truncate transaction maps a vanished row to ErrNotFound).
	if err := st.SoftDeleteMessage(context.Background(), rec.ID, ""); err != nil {
		t.Fatalf("tombstone seed: %v", err)
	}
	if w := doRegen(t, currentETag); w.Code != http.StatusNotFound {
		t.Fatalf("tombstoned target: want 404, got %d (%s)", w.Code, w.Body.String())
	}
}

// TestNotifySource_IfMatch covers POST/PATCH/DELETE on
// /api/v1/agents/{id}/notify-sources[/{sourceId}] with If-Match. The
// resource-level etag is the parent agent's etag — notify-sources are
// stored as a JSON slice on the agent row — so the precondition gates
// on whatever GET /agents/{id} would return.
//
// Cases:
//   - CREATE with malformed If-Match → 400
//   - CREATE with stale If-Match → 412
//   - CREATE with matching If-Match → 201 + new ETag header
//   - PATCH with stale If-Match → 412
//   - PATCH with matching If-Match → 200 + new ETag header
//   - DELETE with stale If-Match → 412
//   - DELETE with matching If-Match → 204 + new ETag header
//   - bare CREATE/PATCH/DELETE without If-Match → unconditional success
func TestNotifySource_IfMatch(t *testing.T) {
	srv, mgr := newTestServer(t)
	alice, err := mgr.Create(agent.AgentConfig{Name: "alice", Tool: "claude", Model: "sonnet"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	owner := auth.Principal{Role: auth.RoleOwner}

	// Read the agent's current etag once.
	getETag := func(t *testing.T) string {
		t.Helper()
		req := mkReq(http.MethodGet, "/api/v1/agents/"+alice.ID, "", owner)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET agent: %d (%s)", w.Code, w.Body.String())
		}
		etag := w.Header().Get("ETag")
		if etag == "" {
			t.Fatal("agent ETag empty — slice 1 broken?")
		}
		return etag
	}

	// --- CREATE ---

	// 400 on malformed If-Match (weak etag).
	{
		req := mkReq(http.MethodPost, "/api/v1/agents/"+alice.ID+"/notify-sources",
			`{"type":"gmail","intervalMinutes":10}`, owner)
		req.Header.Set("If-Match", `W/"v1-deadbeef"`)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("CREATE malformed: want 400, got %d (%s)", w.Code, w.Body.String())
		}
	}

	// 412 on stale If-Match.
	{
		req := mkReq(http.MethodPost, "/api/v1/agents/"+alice.ID+"/notify-sources",
			`{"type":"gmail","intervalMinutes":10}`, owner)
		req.Header.Set("If-Match", `"v999-deadbeef"`)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if w.Code != http.StatusPreconditionFailed {
			t.Fatalf("CREATE stale: want 412, got %d (%s)", w.Code, w.Body.String())
		}
	}

	// 201 on matching If-Match. Capture the source ID + new etag.
	startETag := getETag(t)
	var sourceID string
	{
		req := mkReq(http.MethodPost, "/api/v1/agents/"+alice.ID+"/notify-sources",
			`{"type":"gmail","intervalMinutes":15,"query":"is:unread"}`, owner)
		req.Header.Set("If-Match", startETag)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("CREATE match: want 201, got %d (%s)", w.Code, w.Body.String())
		}
		if w.Header().Get("ETag") == "" {
			t.Fatal("CREATE match: missing ETag header on success")
		}
		var resp struct {
			Source struct {
				ID string `json:"id"`
			} `json:"source"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode CREATE body: %v", err)
		}
		if resp.Source.ID == "" {
			t.Fatalf("CREATE: missing source id in body: %s", w.Body.String())
		}
		sourceID = resp.Source.ID
	}

	// The CREATE bumped the etag — using startETag for PATCH is now
	// stale. Verify that.
	{
		req := mkReq(http.MethodPatch,
			"/api/v1/agents/"+alice.ID+"/notify-sources/"+sourceID,
			`{"intervalMinutes":20}`, owner)
		req.Header.Set("If-Match", startETag)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if w.Code != http.StatusPreconditionFailed {
			t.Fatalf("PATCH stale: want 412, got %d (%s)", w.Code, w.Body.String())
		}
	}

	// --- PATCH ---
	currentETag := getETag(t)

	// 200 on matching If-Match.
	{
		req := mkReq(http.MethodPatch,
			"/api/v1/agents/"+alice.ID+"/notify-sources/"+sourceID,
			`{"intervalMinutes":20}`, owner)
		req.Header.Set("If-Match", currentETag)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("PATCH match: want 200, got %d (%s)", w.Code, w.Body.String())
		}
		if w.Header().Get("ETag") == "" {
			t.Fatal("PATCH match: missing ETag header on success")
		}
	}

	// --- DELETE ---

	// 412 on stale (the just-bumped etag is now ahead of currentETag).
	{
		req := mkReq(http.MethodDelete,
			"/api/v1/agents/"+alice.ID+"/notify-sources/"+sourceID, "", owner)
		req.Header.Set("If-Match", currentETag)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if w.Code != http.StatusPreconditionFailed {
			t.Fatalf("DELETE stale: want 412, got %d (%s)", w.Code, w.Body.String())
		}
	}

	// 204 on matching If-Match.
	currentETag = getETag(t)
	{
		req := mkReq(http.MethodDelete,
			"/api/v1/agents/"+alice.ID+"/notify-sources/"+sourceID, "", owner)
		req.Header.Set("If-Match", currentETag)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("DELETE match: want 204, got %d (%s)", w.Code, w.Body.String())
		}
		if w.Header().Get("ETag") == "" {
			t.Fatal("DELETE match: missing ETag header on success")
		}
	}

	// --- bare unconditional path keeps working ---
	// CREATE without If-Match → 201.
	var bareID string
	{
		req := mkReq(http.MethodPost, "/api/v1/agents/"+alice.ID+"/notify-sources",
			`{"type":"gmail","intervalMinutes":10}`, owner)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("bare CREATE: want 201, got %d (%s)", w.Code, w.Body.String())
		}
		var resp struct {
			Source struct {
				ID string `json:"id"`
			} `json:"source"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		bareID = resp.Source.ID
	}
	// PATCH without If-Match → 200.
	{
		req := mkReq(http.MethodPatch,
			"/api/v1/agents/"+alice.ID+"/notify-sources/"+bareID,
			`{"intervalMinutes":30}`, owner)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("bare PATCH: want 200, got %d (%s)", w.Code, w.Body.String())
		}
	}
	// DELETE without If-Match → 204.
	{
		req := mkReq(http.MethodDelete,
			"/api/v1/agents/"+alice.ID+"/notify-sources/"+bareID, "", owner)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("bare DELETE: want 204, got %d (%s)", w.Code, w.Body.String())
		}
	}
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

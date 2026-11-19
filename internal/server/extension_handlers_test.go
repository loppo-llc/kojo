package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/extpkg"
)

const extTestManifest = `{
  "id": "demo",
  "name": "Demo",
  "version": "1.0.0",
  "scopes": ["chat:send"],
  "contributes": { "skills": ["skills/demo"] }
}`

// newExtTestRepo builds a git repository holding a minimal valid
// package and returns its path.
func newExtTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeExtTestFiles(t, dir, map[string]string{
		extpkg.ManifestFilename: extTestManifest,
		"skills/demo/SKILL.md":  "# demo\n",
	})
	runExtTestGit(t, dir, "init", "-q", "-b", "main")
	commitExtTestRepo(t, dir)
	return dir
}

// writeExtTestFiles drops repository-relative files into dir, creating
// parent directories. Overwrites, so a caller can replace the manifest
// a previous helper wrote.
func writeExtTestFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func commitExtTestRepo(t *testing.T, dir string) {
	t.Helper()
	runExtTestGit(t, dir, "add", "-A")
	runExtTestGit(t, dir, "commit", "-q", "-m", "change")
}

func runExtTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// newExtTestServer builds a Server carrying only an extension registry.
// The handlers touch nothing else, so the heavyweight agent fixture is
// deliberately skipped; s.agents stays nil, which also exercises the
// "no agent manager wired" branch of the binding handler.
func newExtTestServer(t *testing.T) *Server {
	t.Helper()
	mgr, err := extpkg.NewManager(t.TempDir(), "v0.127.0", slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	return &Server{extensions: mgr, logger: slog.Default()}
}

func extCall(t *testing.T, h http.HandlerFunc, method, target, body string, pathValues map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, bytes.NewBufferString(body))
	}
	for k, v := range pathValues {
		r.SetPathValue(k, v)
	}
	rr := httptest.NewRecorder()
	h(rr, r)
	return rr
}

func decodeExt(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	return out
}

func TestExtensionHandlersUnavailableWithoutRegistry(t *testing.T) {
	srv := &Server{logger: slog.Default()}
	rr := extCall(t, srv.handleListExtensions, http.MethodGet, "/api/v1/extensions", "", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestExtensionListStartsEmpty(t *testing.T) {
	srv := newExtTestServer(t)
	rr := extCall(t, srv.handleListExtensions, http.MethodGet, "/api/v1/extensions", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}
	// An empty registry must serialise as [], not null, so the UI can
	// map over the value unconditionally.
	if !strings.Contains(rr.Body.String(), `"extensions":[]`) {
		t.Fatalf("body = %s", rr.Body)
	}
}

// extPreviewCommit runs the preview half of the install flow and
// returns the commit it resolved. Install refuses a request without
// one, so every install test goes through the same two steps the UI
// does.
func extPreviewCommit(t *testing.T, srv *Server, repo string) string {
	t.Helper()
	rr := extCall(t, srv.handlePreviewExtension, http.MethodPost, "/api/v1/extensions/preview",
		`{"url":`+jsonString(repo)+`}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview status = %d: %s", rr.Code, rr.Body)
	}
	commit, _ := decodeExt(t, rr)["commit"].(string)
	if commit == "" {
		t.Fatalf("preview returned no commit: %s", rr.Body)
	}
	return commit
}

func TestExtensionPreviewThenInstall(t *testing.T) {
	srv := newExtTestServer(t)
	repo := newExtTestRepo(t)
	body := `{"url":` + jsonString(repo) + `}`

	rr := extCall(t, srv.handlePreviewExtension, http.MethodPost, "/api/v1/extensions/preview", body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview status = %d: %s", rr.Code, rr.Body)
	}
	got := decodeExt(t, rr)
	if got["installed"] != false {
		t.Fatalf("installed = %v, want false", got["installed"])
	}
	scopes, _ := got["scopes"].([]any)
	if len(scopes) != 1 {
		t.Fatalf("scopes = %v", got["scopes"])
	}
	// The consent dialog needs a human-readable label per scope.
	first, _ := scopes[0].(map[string]any)
	if first["scope"] != "chat:send" || first["description"] == "" {
		t.Fatalf("scope summary = %v", first)
	}

	commit, _ := got["commit"].(string)
	if commit == "" {
		t.Fatalf("preview returned no commit: %s", rr.Body)
	}
	install := `{"url":` + jsonString(repo) + `,"commit":` + jsonString(commit) +
		`,"ackScopes":["chat:send"]}`
	rr = extCall(t, srv.handleInstallExtension, http.MethodPost, "/api/v1/extensions", install, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("install status = %d: %s", rr.Code, rr.Body)
	}
	row := decodeExt(t, rr)
	if row["id"] != "demo" || row["enabled"] != true {
		t.Fatalf("row = %v", row)
	}

	// A second preview of the same URL must report it as installed so
	// the UI can offer "update" instead of "install".
	rr = extCall(t, srv.handlePreviewExtension, http.MethodPost, "/api/v1/extensions/preview", body, nil)
	if decodeExt(t, rr)["installed"] != true {
		t.Fatalf("preview after install did not report installed: %s", rr.Body)
	}

	// Duplicate install is a conflict, not a silent overwrite.
	rr = extCall(t, srv.handleInstallExtension, http.MethodPost, "/api/v1/extensions", install, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("duplicate install status = %d: %s", rr.Code, rr.Body)
	}
}

func TestExtensionInstallScopeMismatchReturnsManifest(t *testing.T) {
	srv := newExtTestServer(t)
	repo := newExtTestRepo(t)
	commit := extPreviewCommit(t, srv, repo)
	rr := extCall(t, srv.handleInstallExtension, http.MethodPost, "/api/v1/extensions",
		`{"url":`+jsonString(repo)+`,"commit":`+jsonString(commit)+`}`, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}
	got := decodeExt(t, rr)
	errObj, _ := got["error"].(map[string]any)
	if errObj["code"] != "scope_mismatch" {
		t.Fatalf("error = %v", got["error"])
	}
	// The manifest rides along so the UI can render consent without a
	// second fetch of the repository.
	mf, _ := got["manifest"].(map[string]any)
	if mf["id"] != "demo" {
		t.Fatalf("manifest = %v", got["manifest"])
	}
	if missing, _ := got["missing"].([]any); len(missing) != 1 || missing[0] != "chat:send" {
		t.Fatalf("missing = %v", got["missing"])
	}
}

func TestExtensionInstallRejectsBadURL(t *testing.T) {
	srv := newExtTestServer(t)
	rr := extCall(t, srv.handleInstallExtension, http.MethodPost, "/api/v1/extensions",
		`{"url":"ext::sh -c whoami","commit":"0123456789abcdef0123456789abcdef01234567"}`, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}
}

// Without a commit there is no proof the operator ever saw the code
// they are consenting to, so the route refuses the request outright.
func TestExtensionInstallRequiresCommit(t *testing.T) {
	srv := newExtTestServer(t)
	repo := newExtTestRepo(t)
	rr := extCall(t, srv.handleInstallExtension, http.MethodPost, "/api/v1/extensions",
		`{"url":`+jsonString(repo)+`,"ackScopes":["chat:send"]}`, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}
}

func TestExtensionInstallRejectsInvalidJSON(t *testing.T) {
	srv := newExtTestServer(t)
	rr := extCall(t, srv.handleInstallExtension, http.MethodPost, "/api/v1/extensions", `{`, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}
}

func TestExtensionLifecycleHandlers(t *testing.T) {
	srv := newExtTestServer(t)
	repo := newExtTestRepo(t)
	commit := extPreviewCommit(t, srv, repo)
	rr := extCall(t, srv.handleInstallExtension, http.MethodPost, "/api/v1/extensions",
		`{"url":`+jsonString(repo)+`,"commit":`+jsonString(commit)+`,"ackScopes":["chat:send"]}`, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("install failed: %s", rr.Body)
	}
	ids := map[string]string{"id": "demo"}

	rr = extCall(t, srv.handleGetExtension, http.MethodGet, "/api/v1/extensions/demo", "", ids)
	if rr.Code != http.StatusOK || decodeExt(t, rr)["id"] != "demo" {
		t.Fatalf("get = %d %s", rr.Code, rr.Body)
	}

	rr = extCall(t, srv.handlePatchExtension, http.MethodPatch, "/api/v1/extensions/demo", `{"enabled":false}`, ids)
	if rr.Code != http.StatusOK || decodeExt(t, rr)["enabled"] != false {
		t.Fatalf("patch = %d %s", rr.Code, rr.Body)
	}

	// A PATCH without the field is a client error, not a silent
	// disable — `enabled` is a tri-state on the wire on purpose.
	rr = extCall(t, srv.handlePatchExtension, http.MethodPatch, "/api/v1/extensions/demo", `{}`, ids)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("patch without enabled = %d: %s", rr.Code, rr.Body)
	}

	rr = extCall(t, srv.handlePutExtensionAgentBinding, http.MethodPut,
		"/api/v1/extensions/demo/agents/ag_1", `{"enabled":true,"config":{"channel":"#ops"}}`,
		map[string]string{"id": "demo", "agentId": "ag_1"})
	if rr.Code != http.StatusOK {
		t.Fatalf("binding = %d: %s", rr.Code, rr.Body)
	}
	agents, _ := decodeExt(t, rr)["agents"].(map[string]any)
	binding, _ := agents["ag_1"].(map[string]any)
	if binding["enabled"] != true {
		t.Fatalf("binding = %v", agents)
	}

	// The package contributes no global settings, so config writes
	// must be refused rather than stored where nothing reads them.
	rr = extCall(t, srv.handlePutExtensionConfig, http.MethodPut, "/api/v1/extensions/demo/config", `{"a":1}`, ids)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("config on schemaless package = %d: %s", rr.Code, rr.Body)
	}

	rr = extCall(t, srv.handleDeleteExtension, http.MethodDelete, "/api/v1/extensions/demo", "", ids)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", rr.Code, rr.Body)
	}
	rr = extCall(t, srv.handleGetExtension, http.MethodGet, "/api/v1/extensions/demo", "", ids)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d: %s", rr.Code, rr.Body)
	}
	rr = extCall(t, srv.handleDeleteExtension, http.MethodDelete, "/api/v1/extensions/demo", "", ids)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("double delete = %d: %s", rr.Code, rr.Body)
	}
}

func TestExtensionAutoUpdateAndCheckHandlers(t *testing.T) {
	srv := newExtTestServer(t)
	repo := newExtTestRepo(t)
	commit := extPreviewCommit(t, srv, repo)
	rr := extCall(t, srv.handleInstallExtension, http.MethodPost, "/api/v1/extensions",
		`{"url":`+jsonString(repo)+`,"commit":`+jsonString(commit)+`,"ackScopes":["chat:send"]}`, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("install failed: %s", rr.Body)
	}
	if _, set := decodeExt(t, rr)["autoUpdate"]; set {
		t.Fatalf("autoUpdate on by default: %s", rr.Body)
	}
	ids := map[string]string{"id": "demo"}

	// autoUpdate alone is a valid PATCH and leaves enabled untouched.
	rr = extCall(t, srv.handlePatchExtension, http.MethodPatch, "/api/v1/extensions/demo", `{"autoUpdate":true}`, ids)
	row := decodeExt(t, rr)
	if rr.Code != http.StatusOK || row["autoUpdate"] != true || row["enabled"] != true {
		t.Fatalf("patch autoUpdate = %d %s", rr.Code, rr.Body)
	}

	// Not checked yet: the list carries an empty updates map.
	rr = extCall(t, srv.handleListExtensions, http.MethodGet, "/api/v1/extensions", "", nil)
	if updates, _ := decodeExt(t, rr)["updates"].(map[string]any); updates == nil || len(updates) != 0 {
		t.Fatalf("updates before check = %s", rr.Body)
	}

	writeExtTestFiles(t, repo, map[string]string{"skills/demo/SKILL.md": "# demo v2\n"})
	commitExtTestRepo(t, repo)
	rr = extCall(t, srv.handleCheckExtensionUpdates, http.MethodPost, "/api/v1/extensions/check-updates", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("check = %d %s", rr.Code, rr.Body)
	}
	updates, _ := decodeExt(t, rr)["updates"].(map[string]any)
	demo, _ := updates["demo"].(map[string]any)
	if demo["available"] != true || demo["latest"] == "" || demo["checkedAt"] == "" {
		t.Fatalf("check result = %s", rr.Body)
	}
	// The manual check never applies, even with autoUpdate on.
	got, _ := srv.extensions.Get("demo")
	if got.Commit != commit {
		t.Fatalf("check moved the checkout to %s", got.Commit)
	}

	// The periodic pass does.
	srv.runExtensionUpdatePass(context.Background())
	got, _ = srv.extensions.Get("demo")
	if got.Commit == commit {
		t.Fatal("auto-update pass did not apply the new commit")
	}
	rr = extCall(t, srv.handleListExtensions, http.MethodGet, "/api/v1/extensions", "", nil)
	updates, _ = decodeExt(t, rr)["updates"].(map[string]any)
	if demo, _ := updates["demo"].(map[string]any); demo["available"] != false {
		t.Fatalf("updates after pass = %s", rr.Body)
	}
}

// TestExtensionRoutesAreOwnerOnly pins the security property the whole
// design leans on: installing a package from a URL is reachable by the
// Owner alone, never by an agent or a paired peer.
func TestExtensionRoutesAreOwnerOnly(t *testing.T) {
	paths := []string{
		"/api/v1/extensions",
		"/api/v1/extensions/preview",
		"/api/v1/extensions/check-updates",
		"/api/v1/extensions/demo",
		"/api/v1/extensions/demo/update",
		"/api/v1/extensions/demo/config",
		"/api/v1/extensions/demo/agents/ag_1",
	}
	methods := []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	principals := []auth.Principal{
		{Role: auth.RoleAgent, AgentID: "ag_1"},
		{Role: auth.RolePeer},
		{Role: auth.RoleGuest},
	}
	for _, p := range principals {
		for _, path := range paths {
			for _, m := range methods {
				if auth.AllowNonOwner(p, m, path) {
					t.Fatalf("%v allowed %s %s", p.Role, m, path)
				}
			}
		}
	}
	if !auth.AllowNonOwner(auth.Principal{Role: auth.RoleOwner}, http.MethodPost, "/api/v1/extensions") {
		t.Fatal("owner denied")
	}
}

// jsonString quotes s as a JSON string literal so temp-dir paths with
// unusual characters cannot break the request bodies above.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// The locale list is the one extension route the frontend calls on
// every page load, including on builds with no registry at all, so it
// answers with an empty list instead of the 503 the others return.
func TestLocaleListEmptyWithoutRegistry(t *testing.T) {
	srv := &Server{logger: slog.Default()}
	rr := extCall(t, srv.handleListLocales, http.MethodGet, "/api/v1/locales", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}
	if strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Fatalf("body = %s, want []", rr.Body)
	}
}

func TestLocaleRoutesServeContributedCatalogue(t *testing.T) {
	srv := newExtTestServer(t)
	repo := newExtTestLocaleRepo(t)
	commit := extPreviewCommit(t, srv, repo)
	rr := extCall(t, srv.handleInstallExtension, http.MethodPost, "/api/v1/extensions",
		`{"url":`+jsonString(repo)+`,"commit":`+jsonString(commit)+`}`, nil)
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("install status = %d: %s", rr.Code, rr.Body)
	}

	rr = extCall(t, srv.handleListLocales, http.MethodGet, "/api/v1/locales", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", rr.Code, rr.Body)
	}
	var list []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0]["tag"] != "zh-Hans" || list[0]["name"] != "简体中文" {
		t.Fatalf("locales = %v", list)
	}

	rr = extCall(t, srv.handleGetLocaleMessages, http.MethodGet, "/api/v1/locales/zh-Hans", "",
		map[string]string{"tag": "zh-Hans"})
	if rr.Code != http.StatusOK {
		t.Fatalf("messages status = %d: %s", rr.Code, rr.Body)
	}
	if got := decodeExt(t, rr)["common.cancel"]; got != "取消" {
		t.Fatalf("common.cancel = %v", got)
	}

	rr = extCall(t, srv.handleGetLocaleMessages, http.MethodGet, "/api/v1/locales/ko", "",
		map[string]string{"tag": "ko"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status for an uncontributed tag = %d, want 404", rr.Code)
	}
}

// newExtTestLocaleRepo builds a package whose only contribution is a
// single translation catalogue.
func newExtTestLocaleRepo(t *testing.T) string {
	t.Helper()
	dir := newExtTestRepo(t)
	manifest := `{"id":"lang","name":"Lang","version":"1.0.0",
	  "contributes":{"locales":[{"tag":"zh-Hans","name":"简体中文","file":"locales/zh-Hans.json"}]}}`
	writeExtTestFiles(t, dir, map[string]string{
		extpkg.ManifestFilename: manifest,
		"locales/zh-Hans.json":  `{"common.cancel":"取消"}`,
	})
	commitExtTestRepo(t, dir)
	return dir
}

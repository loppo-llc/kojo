package server

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/selfupdate"
)

func newUpdateRequest(method, path string, body io.Reader, p auth.Principal) *http.Request {
	r := httptest.NewRequest(method, path, body)
	return authedRequest(r, p)
}

func TestSystemUpdateStatus_ForbiddenForRegularAgent(t *testing.T) {
	srv := &Server{logger: slog.Default()}
	rr := httptest.NewRecorder()
	r := newUpdateRequest(http.MethodGet, "/api/v1/system/update", nil,
		auth.Principal{Role: auth.RoleAgent, AgentID: "ag_x"})
	srv.handleSystemUpdateStatus(rr, r)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestSystemUpdateStatus_NilChecker(t *testing.T) {
	srv := &Server{logger: slog.Default()}
	rr := httptest.NewRecorder()
	r := newUpdateRequest(http.MethodGet, "/api/v1/system/update", nil,
		auth.Principal{Role: auth.RoleOwner})
	srv.handleSystemUpdateStatus(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var body map[string]any
	readJSONResponse(t, rr, &body)
	if body["supported"] != false {
		t.Fatalf("supported = %v, want false", body["supported"])
	}
}

func TestSystemUpdateStatus_ShapeWithChecker(t *testing.T) {
	gh := releaseAPIServerWithAssets(t, "v0.2.0", nil)
	t.Cleanup(gh.Close)

	client := selfupdate.NewClient("v0.1.0")
	client.BaseURL = gh.URL
	client.HTTPClient = gh.Client()
	checker := selfupdate.NewChecker(client, "v0.1.0", slog.New(slog.DiscardHandler))

	srv := &Server{logger: slog.Default(), version: "v0.1.0", updateChecker: checker}
	// No restart trigger → supported:false even though checker works.
	rr := httptest.NewRecorder()
	r := newUpdateRequest(http.MethodGet, "/api/v1/system/update?refresh=1", nil,
		auth.Principal{Role: auth.RoleOwner})
	srv.handleSystemUpdateStatus(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var body map[string]any
	readJSONResponse(t, rr, &body)
	if body["supported"] != false {
		t.Errorf("supported = %v, want false (no restart trigger)", body["supported"])
	}
	if body["current"] != "v0.1.0" {
		t.Errorf("current = %v, want v0.1.0", body["current"])
	}
	if body["latest"] != "v0.2.0" {
		t.Errorf("latest = %v, want v0.2.0", body["latest"])
	}
	if body["updateAvailable"] != true {
		t.Errorf("updateAvailable = %v, want true", body["updateAvailable"])
	}
	if _, ok := body["checkedAt"].(string); !ok {
		t.Errorf("checkedAt missing/not string: %v", body["checkedAt"])
	}
	if notes, _ := body["notesUrl"].(string); notes == "" {
		t.Errorf("notesUrl empty")
	}

	// With a trigger, supported flips to true.
	srv.SetRestartTrigger(func() bool { return true })
	rr2 := httptest.NewRecorder()
	r2 := newUpdateRequest(http.MethodGet, "/api/v1/system/update", nil,
		auth.Principal{Role: auth.RoleOwner})
	srv.handleSystemUpdateStatus(rr2, r2)
	var body2 map[string]any
	readJSONResponse(t, rr2, &body2)
	if body2["supported"] != true {
		t.Errorf("supported = %v, want true with trigger", body2["supported"])
	}
}

func TestSystemUpdate_ForbiddenForRegularAgent(t *testing.T) {
	srv := &Server{logger: slog.Default()}
	srv.SetRestartTrigger(func() bool { return true })
	rr := httptest.NewRecorder()
	srv.handleSystemUpdate(rr, newUpdateRequest(http.MethodPost, "/api/v1/system/update", nil,
		auth.Principal{Role: auth.RoleAgent, AgentID: "ag_x"}))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestSystemUpdate_UnsupportedWithoutTrigger(t *testing.T) {
	client := selfupdate.NewClient("v0.1.0")
	checker := selfupdate.NewChecker(client, "v0.1.0", slog.New(slog.DiscardHandler))
	srv := &Server{logger: slog.Default(), updateChecker: checker}
	rr := httptest.NewRecorder()
	srv.handleSystemUpdate(rr, newUpdateRequest(http.MethodPost, "/api/v1/system/update", nil,
		auth.Principal{Role: auth.RoleOwner}))
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (body %s)", rr.Code, rr.Body.String())
	}
}

func TestSystemUpdate_UpToDate(t *testing.T) {
	gh := releaseAPIServerWithAssets(t, "v0.1.0", nil)
	t.Cleanup(gh.Close)

	client := selfupdate.NewClient("v0.1.0")
	client.BaseURL = gh.URL
	client.HTTPClient = gh.Client()
	checker := selfupdate.NewChecker(client, "v0.1.0", slog.New(slog.DiscardHandler))

	srv := &Server{logger: slog.Default(), version: "v0.1.0", updateChecker: checker}
	srv.SetRestartTrigger(func() bool { return true })

	rr := httptest.NewRecorder()
	srv.handleSystemUpdate(rr, newUpdateRequest(http.MethodPost, "/api/v1/system/update", nil,
		auth.Principal{Role: auth.RoleOwner}))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var body map[string]any
	readJSONResponse(t, rr, &body)
	if body["status"] != "up_to_date" {
		t.Fatalf("status field = %v, want up_to_date", body["status"])
	}
	if srv.restartPending.Load() {
		t.Fatal("up_to_date must not arm restartPending")
	}
}

func TestSystemUpdate_SuccessSwapsAndRestarts(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Fake swap target — never touch the real test binary.
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "kojo")
	if err := os.WriteFile(target, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("write target: %v", err)
	}
	selfupdate.SetExecPathForTest(t, target)

	newPayload := []byte("new-binary-v0.2.0")
	assetName := selfupdate.AssetName(runtime.GOOS, runtime.GOARCH)
	archive := buildPlatformArchive(t, runtime.GOOS, newPayload)
	sum := sha256Hex(archive)

	gh := releaseAPIServerWithAssets(t, "v0.2.0", map[string][]byte{
		assetName:                     archive,
		selfupdate.ChecksumsAssetName: []byte(sum + "  " + assetName + "\n"),
	})
	t.Cleanup(gh.Close)

	client := selfupdate.NewClient("v0.1.0")
	client.BaseURL = gh.URL
	client.HTTPClient = gh.Client()
	checker := selfupdate.NewChecker(client, "v0.1.0", slog.New(slog.DiscardHandler))

	srv := newChunkedSyncTestServer(t)
	srv.version = "v0.1.0"
	srv.updateChecker = checker
	fired := make(chan struct{})
	srv.SetRestartTrigger(func() bool { close(fired); return true })

	rr := httptest.NewRecorder()
	srv.handleSystemUpdate(rr, newUpdateRequest(http.MethodPost, "/api/v1/system/update", nil,
		auth.Principal{Role: auth.RolePrivAgent, AgentID: "ag_x"}))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rr.Code, rr.Body.String())
	}
	var body map[string]any
	readJSONResponse(t, rr, &body)
	if body["status"] != "pending" {
		t.Fatalf("status field = %v, want pending", body["status"])
	}
	if body["from"] != "v0.1.0" || body["to"] != "v0.2.0" {
		t.Fatalf("from/to = %v/%v, want v0.1.0/v0.2.0", body["from"], body["to"])
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read swapped target: %v", err)
	}
	if !bytes.Equal(got, newPayload) {
		t.Fatalf("swapped content = %q, want %q", got, newPayload)
	}

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("restart trigger did not fire after update drain")
	}
}

func TestSystemUpdate_ConcurrentRestartWhileUpdating(t *testing.T) {
	// Block LatestRelease so Apply holds the restart claim mid-download.
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/loppo-llc/kojo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(selfupdate.Release{
			TagName: "v0.1.0", // same as current → up_to_date after claim
			HTMLURL: "https://example.com/v0.1.0",
		})
	})
	gh := httptest.NewServer(mux)
	t.Cleanup(gh.Close)

	client := selfupdate.NewClient("v0.1.0")
	client.BaseURL = gh.URL
	client.HTTPClient = gh.Client()
	checker := selfupdate.NewChecker(client, "v0.1.0", slog.New(slog.DiscardHandler))

	srv := &Server{logger: slog.Default(), version: "v0.1.0", updateChecker: checker}
	srv.SetRestartTrigger(func() bool { return true })

	done := make(chan int, 1)
	go func() {
		rr := httptest.NewRecorder()
		srv.handleSystemUpdate(rr, newUpdateRequest(http.MethodPost, "/api/v1/system/update", nil,
			auth.Principal{Role: auth.RoleOwner}))
		done <- rr.Code
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Apply never entered LatestRelease")
	}
	if !srv.restartPending.Load() {
		t.Fatal("update must claim restartPending before Apply finishes")
	}

	// Concurrent restart while update holds the claim.
	rrRestart := httptest.NewRecorder()
	srv.handleSystemRestart(rrRestart, newUpdateRequest(http.MethodPost, "/api/v1/system/restart", nil,
		auth.Principal{Role: auth.RoleOwner}))
	if rrRestart.Code != http.StatusAccepted {
		t.Fatalf("concurrent restart status = %d, want 202 (body %s)", rrRestart.Code, rrRestart.Body.String())
	}
	var restartBody map[string]any
	readJSONResponse(t, rrRestart, &restartBody)
	if restartBody["status"] != "already_pending" {
		t.Fatalf("concurrent restart status field = %v, want already_pending", restartBody["status"])
	}

	close(release)
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("update final status = %d, want 200 up_to_date", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("update handler did not finish")
	}
	if srv.restartPending.Load() {
		t.Fatal("Apply failure (up_to_date) must release restartPending")
	}
}

func TestSystemUpdate_ApplyFailureReleasesClaim(t *testing.T) {
	gh := releaseAPIServerWithAssets(t, "v0.1.0", nil)
	t.Cleanup(gh.Close)

	client := selfupdate.NewClient("v0.1.0")
	client.BaseURL = gh.URL
	client.HTTPClient = gh.Client()
	checker := selfupdate.NewChecker(client, "v0.1.0", slog.New(slog.DiscardHandler))

	srv := &Server{logger: slog.Default(), version: "v0.1.0", updateChecker: checker}
	fired := make(chan struct{})
	srv.SetRestartTrigger(func() bool { close(fired); return true })

	rr := httptest.NewRecorder()
	srv.handleSystemUpdate(rr, newUpdateRequest(http.MethodPost, "/api/v1/system/update", nil,
		auth.Principal{Role: auth.RoleOwner}))
	if rr.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if srv.restartPending.Load() {
		t.Fatal("up_to_date must release restartPending")
	}

	// A subsequent restart must be able to claim the slot.
	rr2 := httptest.NewRecorder()
	srv.handleSystemRestart(rr2, newUpdateRequest(http.MethodPost, "/api/v1/system/restart", nil,
		auth.Principal{Role: auth.RoleOwner}))
	if rr2.Code != http.StatusAccepted {
		t.Fatalf("restart after failed update status = %d, want 202 (body %s)", rr2.Code, rr2.Body.String())
	}
	var body map[string]any
	readJSONResponse(t, rr2, &body)
	if body["status"] != "pending" {
		t.Fatalf("restart status field = %v, want pending", body["status"])
	}
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("restart trigger did not fire after released claim")
	}
}

// --- helpers ---

// releaseAPIServerWithAssets serves /repos/loppo-llc/kojo/releases/latest
// plus optional /files/<name> asset bodies. Asset BrowserDownloadURLs
// point at this server so Apply can download without hitting GitHub.
func releaseAPIServerWithAssets(t *testing.T, tag string, files map[string][]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		name := path.Base(r.URL.Path)
		body, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	})

	// Bind the server first so we can stamp absolute download URLs.
	// The latest handler is registered after NewServer via a closure
	// over srv.URL — use a two-step so the URL is known.
	var srv *httptest.Server
	mux.HandleFunc("/repos/loppo-llc/kojo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		rel := selfupdate.Release{
			TagName: tag,
			HTMLURL: "https://github.com/loppo-llc/kojo/releases/tag/" + tag,
		}
		for name, body := range files {
			rel.Assets = append(rel.Assets, selfupdate.Asset{
				Name:               name,
				Size:               int64(len(body)),
				BrowserDownloadURL: srv.URL + "/files/" + name,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rel)
	})
	srv = httptest.NewServer(mux)
	return srv
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func buildPlatformArchive(t *testing.T, goos string, payload []byte) []byte {
	t.Helper()
	member := "kojo"
	if goos == "windows" {
		member = "kojo.exe"
		return buildZip(t, member, payload)
	}
	return buildTarGz(t, member, payload)
}

func buildTarGz(t *testing.T, member string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name: member,
		Mode: 0o755,
		Size: int64(len(payload)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func buildZip(t *testing.T, member string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(member)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

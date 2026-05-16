//go:build heavy_test

// Same heavy_test gating as agent_handlers_test.go — these tests pull
// in store.Open (modernc.org/sqlite) and exercise the full mux end to
// end, which makes them too heavy for the default `go test ./...` run.
// Run with `go test -tags heavy_test ./internal/server/...`.

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

	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/store"
)

// testBlobMaxPutBytes is the per-PUT cap used by these tests. Small
// enough that TestBlobPayloadTooLarge can construct (cap+1)-byte
// bodies without burning real memory.
const testBlobMaxPutBytes int64 = 64 * 1024 // 64 KiB

// newBlobTestServer wires a Server whose only configured subsystem is a
// blob.Store — no agents, no groupdms. Tests bypass auth by hitting
// srv.mux directly. The blob.Store is wired With Refs so PUT / GET /
// HEAD exercise the cache path that production uses.
func newBlobTestServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{ConfigDir: root})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	bs := blob.New(root,
		blob.WithRefs(blob.NewStoreRefs(st, "peer-test")),
		blob.WithHomePeer("peer-test"),
	)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(Config{
		Addr:            ":0",
		Logger:          logger,
		Version:         "test",
		BlobStore:       bs,
		MaxBlobPutBytes: testBlobMaxPutBytes,
	})
	return srv
}

// doBlob runs a single request against the server's mux. Auth is
// intentionally bypassed — the handler-level concerns we test (path
// parsing, etag handling, error mapping) are independent of who's
// calling.
func doBlob(srv *Server, method, target string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

func TestBlobPutGetRoundTrip(t *testing.T) {
	srv := newBlobTestServer(t)

	body := "alice avatar"
	w := doBlob(srv, http.MethodPut, "/api/v1/blob/global/agents/ag_1/avatar.png",
		strings.NewReader(body), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	etag := w.Header().Get("ETag")
	if !strings.HasPrefix(etag, `"sha256:`) || !strings.HasSuffix(etag, `"`) {
		t.Errorf("ETag %q not strong sha256 form", etag)
	}
	if w.Header().Get("X-Kojo-SHA256") == "" {
		t.Errorf("missing X-Kojo-SHA256")
	}
	var putResp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &putResp); err != nil {
		t.Fatalf("PUT body not JSON: %v (%s)", err, w.Body.String())
	}
	if int64(putResp["size"].(float64)) != int64(len(body)) {
		t.Errorf("PUT size = %v, want %d", putResp["size"], len(body))
	}

	w = doBlob(srv, http.MethodGet, "/api/v1/blob/global/agents/ag_1/avatar.png", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != body {
		t.Errorf("GET body = %q, want %q", got, body)
	}
	if w.Header().Get("ETag") != etag {
		t.Errorf("GET etag drift: %q vs %q", w.Header().Get("ETag"), etag)
	}

	// HEAD: same headers, empty body.
	w = doBlob(srv, http.MethodHead, "/api/v1/blob/global/agents/ag_1/avatar.png", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD: %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD body non-empty: %d bytes", w.Body.Len())
	}
	if w.Header().Get("ETag") != etag {
		t.Errorf("HEAD etag drift")
	}
}

func TestBlobIfNoneMatch304(t *testing.T) {
	srv := newBlobTestServer(t)
	w := doBlob(srv, http.MethodPut, "/api/v1/blob/global/x.bin",
		strings.NewReader("hello"), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	etag := w.Header().Get("ETag")

	w = doBlob(srv, http.MethodGet, "/api/v1/blob/global/x.bin", nil,
		map[string]string{"If-None-Match": etag})
	if w.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match match: want 304, got %d", w.Code)
	}
}

func TestBlobPutIfMatchMismatch(t *testing.T) {
	srv := newBlobTestServer(t)
	w := doBlob(srv, http.MethodPut, "/api/v1/blob/global/x.bin",
		strings.NewReader("v1"), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT v1: %d %s", w.Code, w.Body.String())
	}

	// Stale etag — must 412.
	w = doBlob(srv, http.MethodPut, "/api/v1/blob/global/x.bin",
		strings.NewReader("v2"),
		map[string]string{"If-Match": `"sha256:00ff00ff"`})
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: want 412, got %d %s", w.Code, w.Body.String())
	}
}

func TestBlobIfMatchMustBeStrong(t *testing.T) {
	srv := newBlobTestServer(t)
	if w := doBlob(srv, http.MethodPut, "/api/v1/blob/global/x.bin",
		strings.NewReader("v1"), nil); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d", w.Code)
	}

	// `*` is rejected — blob's IfMatch is content-hash equality, not
	// "any current version".
	w := doBlob(srv, http.MethodPut, "/api/v1/blob/global/x.bin",
		strings.NewReader("v2"),
		map[string]string{"If-Match": "*"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("If-Match=*: want 400, got %d", w.Code)
	}
	// Weak etag is rejected.
	w = doBlob(srv, http.MethodPut, "/api/v1/blob/global/x.bin",
		strings.NewReader("v2"),
		map[string]string{"If-Match": `W/"sha256:abc"`})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("weak If-Match: want 400, got %d", w.Code)
	}
	// Comma list is rejected.
	w = doBlob(srv, http.MethodPut, "/api/v1/blob/global/x.bin",
		strings.NewReader("v2"),
		map[string]string{"If-Match": `"sha256:a","sha256:b"`})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("multi If-Match: want 400, got %d", w.Code)
	}
}

func TestBlobExpectedSHA256Mismatch(t *testing.T) {
	srv := newBlobTestServer(t)
	// Wrong expected → atomicWrite aborts pre-rename, no body lands on disk.
	w := doBlob(srv, http.MethodPut, "/api/v1/blob/global/x.bin",
		strings.NewReader("hello"),
		map[string]string{"X-Kojo-Expected-SHA256": "deadbeef"})
	if w.Code == http.StatusOK {
		t.Fatalf("expected mismatch: want non-200, got 200")
	}
	// And no row was created.
	w = doBlob(srv, http.MethodGet, "/api/v1/blob/global/x.bin", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("after aborted PUT: GET should 404, got %d", w.Code)
	}
}

func TestBlobDelete(t *testing.T) {
	srv := newBlobTestServer(t)
	w := doBlob(srv, http.MethodPut, "/api/v1/blob/global/x.bin",
		strings.NewReader("v1"), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: %d", w.Code)
	}
	etag := w.Header().Get("ETag")

	// DELETE with stale If-Match → 412.
	w = doBlob(srv, http.MethodDelete, "/api/v1/blob/global/x.bin", nil,
		map[string]string{"If-Match": `"sha256:0000"`})
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("DELETE stale If-Match: want 412, got %d", w.Code)
	}
	// Body still there.
	w = doBlob(srv, http.MethodGet, "/api/v1/blob/global/x.bin", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET after failed DELETE: %d", w.Code)
	}

	// DELETE with current If-Match → 204.
	w = doBlob(srv, http.MethodDelete, "/api/v1/blob/global/x.bin", nil,
		map[string]string{"If-Match": etag})
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE current If-Match: want 204, got %d %s", w.Code, w.Body.String())
	}
	// Gone.
	w = doBlob(srv, http.MethodGet, "/api/v1/blob/global/x.bin", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("after DELETE: GET want 404, got %d", w.Code)
	}
}

func TestBlobList(t *testing.T) {
	srv := newBlobTestServer(t)
	for _, p := range []string{
		"agents/ag_1/avatar.png",
		"agents/ag_1/books/x.md",
		"agents/ag_2/avatar.png",
	} {
		if w := doBlob(srv, http.MethodPut, "/api/v1/blob/global/"+p,
			strings.NewReader("body-"+p), nil); w.Code != http.StatusOK {
			t.Fatalf("seed %s: %d %s", p, w.Code, w.Body.String())
		}
	}
	w := doBlob(srv, http.MethodGet,
		"/api/v1/blob/global/?prefix=agents/ag_1/", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("LIST: %d %s", w.Code, w.Body.String())
	}
	var resp listResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("LIST body: %v (%s)", err, w.Body.String())
	}
	if resp.Scope != "global" || resp.Prefix != "agents/ag_1/" {
		t.Errorf("envelope: %+v", resp)
	}
	if len(resp.Objects) != 2 {
		t.Errorf("LIST count = %d, want 2 (%+v)", len(resp.Objects), resp.Objects)
	}
}

func TestBlobInvalidScope(t *testing.T) {
	srv := newBlobTestServer(t)
	// `cas` is reserved but invalid for the public API.
	w := doBlob(srv, http.MethodGet, "/api/v1/blob/cas/x.bin", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid scope: want 400, got %d", w.Code)
	}
}

func TestBlobInvalidPath(t *testing.T) {
	srv := newBlobTestServer(t)
	// `..` traversal — blob.validatePath rejects.
	w := doBlob(srv, http.MethodPut, "/api/v1/blob/global/../etc/passwd",
		strings.NewReader("x"), nil)
	if w.Code == http.StatusOK {
		t.Fatalf("traversal: want non-200, got 200")
	}
}

func TestBlobPayloadTooLarge(t *testing.T) {
	srv := newBlobTestServer(t)
	// Send (cap+1) bytes against the test's small cap — handler must
	// 413. The test cap (testBlobMaxPutBytes) is small enough that the
	// allocation here is bounded; production uses defaultBlobMaxPutBytes.
	body := strings.NewReader(strings.Repeat("x", int(testBlobMaxPutBytes)+1))
	w := doBlob(srv, http.MethodPut, "/api/v1/blob/global/big.bin", body, nil)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize PUT: want 413, got %d %s", w.Code, w.Body.String())
	}
}

func TestBlobNotFound(t *testing.T) {
	srv := newBlobTestServer(t)
	w := doBlob(srv, http.MethodGet, "/api/v1/blob/global/nope.bin", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("missing: want 404, got %d", w.Code)
	}
	w = doBlob(srv, http.MethodDelete, "/api/v1/blob/global/nope.bin", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("delete missing: want 404, got %d", w.Code)
	}
}

func TestBlobUnconfigured(t *testing.T) {
	// When BlobStore is nil, the routes should not be registered at all,
	// so the mux returns 404 (not the 503 the handler would have given).
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(Config{Addr: ":0", Logger: logger, Version: "test"})
	w := doBlob(srv, http.MethodGet, "/api/v1/blob/global/x.bin", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("nil blob store: want 404 (route not registered), got %d", w.Code)
	}
}

//go:build heavy_test

// Heavy because the middleware exercises the agent.Manager-backed
// store. Run with:
//
//	go test -tags heavy_test ./internal/server/...

package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/loppo-llc/kojo/internal/agent"
)

// freshIdemKey returns a new UUID string. Tests use this rather than
// hard-coded "k1" / "k2" because the middleware validates the
// Idempotency-Key header as a canonical UUID.
func freshIdemKey() string {
	return uuid.NewString()
}

// newIdemTestServer wires a Server with a manager so the middleware
// has a real store. The mux carries a single test handler at
// POST /api/v1/_idem_test that records hit count and writes a JSON
// response. Tests can assert that the count goes up only once per
// distinct Idempotency-Key.
func newIdemTestServer(t *testing.T) (*Server, *atomic.Int64) {
	t.Helper()
	t.Setenv("KOJO_CONFIG_DIR", t.TempDir())

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	mgr, err := agent.NewManager(logger)
	if err != nil {
		t.Fatalf("agent.NewManager: %v", err)
	}
	t.Cleanup(func() { mgr.Shutdown() })

	srv := New(Config{
		Addr:         ":0",
		Logger:       logger,
		Version:      "test",
		AgentManager: mgr,
	})

	var hits atomic.Int64
	srv.mux.HandleFunc("POST /api/v1/_idem_test", func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"hits":` + strings.TrimSpace(string(body)) + `,"n":` + idemItoa(n) + `}`))
	})
	return srv, &hits
}

func idemItoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

func doIdemReq(srv *Server, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/_idem_test",
		strings.NewReader(body))
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	r.Header.Set("Content-Type", "application/json")
	// Idempotency middleware is wired on both listener handlers; the
	// underlying handler chain is mux-based for tests, so call the
	// middleware directly.
	w := httptest.NewRecorder()
	srv.idempotencyMiddleware(srv.mux).ServeHTTP(w, r)
	return w
}

func TestIdempotencyReplay(t *testing.T) {
	srv, hits := newIdemTestServer(t)
	key := freshIdemKey()

	w1 := doIdemReq(srv, key, "1")
	if w1.Code != http.StatusOK {
		t.Fatalf("first: %d body=%s", w1.Code, w1.Body.String())
	}
	if hits.Load() != 1 {
		t.Errorf("first run hits=%d want 1", hits.Load())
	}

	// Second request, same key, same body — should NOT re-run handler.
	w2 := doIdemReq(srv, key, "1")
	if w2.Code != http.StatusOK {
		t.Fatalf("replay: %d body=%s", w2.Code, w2.Body.String())
	}
	if hits.Load() != 1 {
		t.Errorf("replay re-ran handler: hits=%d want 1", hits.Load())
	}
	if w2.Header().Get("X-Idempotent-Replay") != "true" {
		t.Errorf("replay header missing")
	}
	if w2.Body.String() != w1.Body.String() {
		t.Errorf("replay body mismatch:\nfirst=%s\nreplay=%s", w1.Body.String(), w2.Body.String())
	}
	if w2.Header().Get("ETag") != `"v1"` {
		t.Errorf("replay etag missing: %q", w2.Header().Get("ETag"))
	}
}

func TestIdempotencyConflictOnDifferentBody(t *testing.T) {
	srv, hits := newIdemTestServer(t)
	key := freshIdemKey()

	if w := doIdemReq(srv, key, "1"); w.Code != http.StatusOK {
		t.Fatalf("first: %d body=%s", w.Code, w.Body.String())
	}
	w2 := doIdemReq(srv, key, "2")
	if w2.Code != http.StatusConflict {
		t.Fatalf("expected 409 on different body; got %d body=%s", w2.Code, w2.Body.String())
	}
	if hits.Load() != 1 {
		t.Errorf("conflicting request executed handler: hits=%d want 1", hits.Load())
	}
}

func TestIdempotencyPassThroughWithoutKey(t *testing.T) {
	srv, hits := newIdemTestServer(t)

	if w := doIdemReq(srv, "", "1"); w.Code != http.StatusOK {
		t.Fatalf("first: %d body=%s", w.Code, w.Body.String())
	}
	if w := doIdemReq(srv, "", "1"); w.Code != http.StatusOK {
		t.Fatalf("second: %d body=%s", w.Code, w.Body.String())
	}
	if hits.Load() != 2 {
		t.Errorf("no-key requests should both execute: hits=%d want 2", hits.Load())
	}
}

func TestIdempotencyValidatesKey(t *testing.T) {
	srv, _ := newIdemTestServer(t)
	cases := []string{
		"contains space",
		"contains\ttab",
		"control\x01char",
		"not-a-uuid-just-some-string",
		"urn:uuid:" + uuid.NewString(),                // URN form
		"{" + uuid.NewString() + "}",                  // braced
		strings.ToUpper(uuid.NewString()),             // uppercase
		strings.ReplaceAll(uuid.NewString(), "-", ""), // no hyphens
		strings.Repeat("a", 257),
	}
	for _, k := range cases {
		t.Run(k, func(t *testing.T) {
			w := doIdemReq(srv, k, "1")
			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for invalid key; got %d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestIdempotencySkipsGET(t *testing.T) {
	srv, hits := newIdemTestServer(t)
	srv.mux.HandleFunc("GET /api/v1/_idem_get", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	key := freshIdemKey()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/_idem_get", nil)
	r.Header.Set("Idempotency-Key", key)
	w1 := httptest.NewRecorder()
	srv.idempotencyMiddleware(srv.mux).ServeHTTP(w1, r)
	if w1.Code != http.StatusOK {
		t.Fatalf("first: %d", w1.Code)
	}
	r2 := httptest.NewRequest(http.MethodGet, "/api/v1/_idem_get", nil)
	r2.Header.Set("Idempotency-Key", key)
	w2 := httptest.NewRecorder()
	srv.idempotencyMiddleware(srv.mux).ServeHTTP(w2, r2)
	if hits.Load() != 2 {
		t.Errorf("GETs should not be deduped: hits=%d", hits.Load())
	}
}

func TestIdempotencySkipsNon4xxFor5xx(t *testing.T) {
	srv, hits := newIdemTestServer(t)
	srv.mux.HandleFunc("POST /api/v1/_idem_5xx", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"err":"boom"}`))
	})

	key := freshIdemKey()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/_idem_5xx",
		strings.NewReader("1"))
	r.Header.Set("Idempotency-Key", key)
	r.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	srv.idempotencyMiddleware(srv.mux).ServeHTTP(w1, r)
	if w1.Code != http.StatusInternalServerError {
		t.Fatalf("first: %d body=%s", w1.Code, w1.Body.String())
	}

	// Same key + same body — must re-execute because 5xx is treated
	// as transient and not saved.
	r2 := httptest.NewRequest(http.MethodPost, "/api/v1/_idem_5xx",
		strings.NewReader("1"))
	r2.Header.Set("Idempotency-Key", key)
	r2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	srv.idempotencyMiddleware(srv.mux).ServeHTTP(w2, r2)
	if hits.Load() != 2 {
		t.Errorf("5xx should not be replayed: hits=%d want 2", hits.Load())
	}
}

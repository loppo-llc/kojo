package peer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/blob"
)

// fixedSourceFixture mounts an httptest server that pretends to
// be a source peer's /api/v1/peers/blobs/{uri} endpoint. The
// handler returns a configurable status / body / sha256 header
// so each subtest can pin one behaviour. It does NOT mount the
// AuthMiddleware: SignRequest is verified by the existing
// Sign/Verify roundtrip suite, and bringing the middleware in
// here would require a store + peer_registry row whose only
// purpose is to re-prove a check that already has coverage.
func fixedSourceFixture(t *testing.T, status int, body []byte, sha256Hdr string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/peers/blobs/") {
			http.NotFound(w, r)
			return
		}
		// Sanity-check that SignRequest stamped the auth headers
		// on the outbound request — the source-side middleware
		// would 401 otherwise.
		if r.Header.Get(AuthHeaderSig) == "" || r.Header.Get(AuthHeaderID) == "" {
			http.Error(w, "missing auth headers", http.StatusBadRequest)
			return
		}
		if sha256Hdr != "" {
			w.Header().Set("X-Kojo-Blob-SHA256", sha256Hdr)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestIdentity(t *testing.T) *Identity {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519: %v", err)
	}
	return &Identity{
		DeviceID:   "client-device-0123456789abcdef0",
		Name:       "client",
		PublicKey:  pub,
		PrivateKey: priv,
	}
}

func newTestBlobStore(t *testing.T) *blob.Store {
	t.Helper()
	return blob.New(t.TempDir())
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestPullOne_HappyPath verifies a 200 with a matching sha256
// header produces an "ok" result and writes the body to the
// target blob store at the canonical (scope, path).
func TestPullOne_HappyPath(t *testing.T) {
	body := []byte("hello kojo handoff")
	digest := hexSHA256(body)
	srv := fixedSourceFixture(t, http.StatusOK, body, digest)

	id := newTestIdentity(t)
	dst := newTestBlobStore(t)
	client := NewPullClient(id, nil, nil)
	src := PullSource{DeviceID: "source-device-fedcba9876543210", Address: srv.URL}

	uri := "kojo://global/agents/ag_x/transcript"
	res, err := client.PullOne(context.Background(), src, PullItem{URI: uri, ExpectedSHA256: digest}, dst)
	if err != nil {
		t.Fatalf("PullOne err = %v", err)
	}
	if res.Status != "ok" {
		t.Fatalf("status = %q, want ok (err=%q)", res.Status, res.Error)
	}
	if res.SHA256 != digest {
		t.Fatalf("sha256 = %q, want %q", res.SHA256, digest)
	}
	if res.Size != int64(len(body)) {
		t.Fatalf("size = %d, want %d", res.Size, len(body))
	}

	// Body landed on disk under the canonical path.
	f, obj, err := dst.Open(blob.ScopeGlobal, "agents/ag_x/transcript")
	if err != nil {
		t.Fatalf("Open after pull: %v", err)
	}
	defer f.Close()
	if obj.Size != int64(len(body)) {
		t.Fatalf("on-disk size = %d, want %d", obj.Size, len(body))
	}
}

// TestPullOne_SHA256Mismatch verifies the client refuses to
// commit a body whose digest disagrees with the source-supplied
// header. blob.Store.Put aborts before rename, so nothing
// reaches disk.
func TestPullOne_SHA256Mismatch(t *testing.T) {
	body := []byte("real body")
	lyingDigest := hexSHA256([]byte("different content"))
	srv := fixedSourceFixture(t, http.StatusOK, body, lyingDigest)

	id := newTestIdentity(t)
	dst := newTestBlobStore(t)
	client := NewPullClient(id, nil, nil)
	src := PullSource{DeviceID: "source-device-fedcba9876543210", Address: srv.URL}

	// No orchestrator-supplied digest: only the response header
	// is checked, and the on-the-wire bytes hash differently
	// than that header, so blob.Store.Put aborts pre-rename.
	res, err := client.PullOne(context.Background(),
		src, PullItem{URI: "kojo://global/agents/ag_x/memory"}, dst)
	if err != nil {
		t.Fatalf("PullOne err = %v", err)
	}
	if res.Status != "sha256_mismatch" {
		t.Fatalf("status = %q, want sha256_mismatch (err=%q)", res.Status, res.Error)
	}
	// On-disk path MUST NOT exist after a mismatch.
	if _, _, err := dst.Open(blob.ScopeGlobal, "agents/ag_x/memory"); err == nil {
		t.Fatalf("body should not have landed on disk after sha256 mismatch")
	}
}

// TestPullOne_HTTPNon200 pins the contract that a non-200
// response (handler refusing the fetch — 409 not_in_handoff /
// wrong_home, 410 body_missing, etc.) does NOT abort the batch;
// it records "http_status" and the caller decides whether to
// roll back the parent handoff.
func TestPullOne_HTTPNon200(t *testing.T) {
	srv := fixedSourceFixture(t, http.StatusConflict, []byte(`{"error":{"code":"not_in_handoff"}}`), "")

	id := newTestIdentity(t)
	dst := newTestBlobStore(t)
	client := NewPullClient(id, nil, nil)
	src := PullSource{DeviceID: "source-device-fedcba9876543210", Address: srv.URL}

	res, err := client.PullOne(context.Background(),
		src, PullItem{URI: "kojo://global/agents/ag_x/avatar.png"}, dst)
	if err != nil {
		t.Fatalf("PullOne err = %v", err)
	}
	if res.Status != "http_status" {
		t.Fatalf("status = %q, want http_status", res.Status)
	}
	if !strings.Contains(res.Error, "409") {
		t.Fatalf("error %q should mention 409", res.Error)
	}
}

// TestPullOne_MissingSHAHeader refuses to write a body the
// source didn't sign with a digest header — without ExpectedSHA256
// the target would have to trust whatever arrived, which is the
// whole vulnerability we're closing.
func TestPullOne_MissingSHAHeader(t *testing.T) {
	body := []byte("some body")
	srv := fixedSourceFixture(t, http.StatusOK, body, "" /* no header */)

	id := newTestIdentity(t)
	dst := newTestBlobStore(t)
	client := NewPullClient(id, nil, nil)
	src := PullSource{DeviceID: "source-device-fedcba9876543210", Address: srv.URL}

	// Pass an empty ExpectedSHA256 so the helper has nothing to
	// fall back on; both digest channels are missing, so the
	// pull refuses pre-Put.
	res, err := client.PullOne(context.Background(),
		src, PullItem{URI: "kojo://global/agents/ag_x/file"}, dst)
	if err != nil {
		t.Fatalf("PullOne err = %v", err)
	}
	if res.Status != "error" || !strings.Contains(res.Error, "expected sha256") {
		t.Fatalf("status=%q err=%q, want error w/ no-digest mention", res.Status, res.Error)
	}
}

// TestPullOne_OrchestratorDigestOverridesHeader pins the trust
// model added during Codex review: the orchestrator-supplied
// ExpectedSHA256 is the authoritative digest. A compromised
// source returning a body whose actual hash matches its OWN
// header value but disagrees with the orchestrator MUST NOT
// land on disk.
func TestPullOne_OrchestratorDigestOverridesHeader(t *testing.T) {
	body := []byte("evil body")
	headerSHA := hexSHA256(body) // source's own (matching) header
	orchestratorSHA := hexSHA256([]byte("what the hub stored"))
	srv := fixedSourceFixture(t, http.StatusOK, body, headerSHA)

	id := newTestIdentity(t)
	dst := newTestBlobStore(t)
	client := NewPullClient(id, nil, nil)
	src := PullSource{DeviceID: "source-device-fedcba9876543210", Address: srv.URL}

	res, err := client.PullOne(context.Background(),
		src, PullItem{
			URI:            "kojo://global/agents/ag_x/secret",
			ExpectedSHA256: orchestratorSHA,
		}, dst)
	if err != nil {
		t.Fatalf("PullOne err = %v", err)
	}
	if res.Status != "sha256_mismatch" {
		t.Fatalf("status = %q, want sha256_mismatch (err=%q)", res.Status, res.Error)
	}
	if _, _, err := dst.Open(blob.ScopeGlobal, "agents/ag_x/secret"); err == nil {
		t.Fatalf("body should not have landed on disk when orchestrator digest disagrees")
	}
}

// TestPullMany_StopsOnLocalFatal verifies that a local-fatal
// error (we simulate by passing a cancelled context) returns the
// partial result slice without claiming success on the unvisited
// entries.
func TestPullMany_StopsOnLocalFatal(t *testing.T) {
	body := []byte("payload")
	srv := fixedSourceFixture(t, http.StatusOK, body, hexSHA256(body))

	id := newTestIdentity(t)
	dst := newTestBlobStore(t)
	client := NewPullClient(id, nil, nil)
	src := PullSource{DeviceID: "source-device-fedcba9876543210", Address: srv.URL}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the loop runs

	items := []PullItem{
		{URI: "kojo://global/agents/ag_x/a", ExpectedSHA256: hexSHA256(body)},
		{URI: "kojo://global/agents/ag_x/b", ExpectedSHA256: hexSHA256(body)},
	}
	results, err := client.PullMany(ctx, src, items, dst)
	if err == nil {
		t.Fatalf("expected context error, got nil")
	}
	if len(results) >= len(items) {
		t.Fatalf("got %d results, want fewer than %d (cancellation should short-circuit)",
			len(results), len(items))
	}
}

// TestBuildPeerBlobURL_StripsCallerPath defends against a
// misconfigured peer_registry.name that includes a path: the
// helper MUST overwrite path/query so the request lands on the
// canonical blob endpoint regardless of how the address was
// stamped.
//
// The helper also strips the "kojo://" prefix from the URI so
// the URL path never contains "//" — Go's ServeMux path-cleans
// double-slashes with a 301 redirect, and the redirect resends
// the same signed nonce which triggers a 401 replay rejection.
func TestBuildPeerBlobURL_StripsCallerPath(t *testing.T) {
	cases := []struct {
		base string
		uri  string
		want string
	}{
		{
			base: "http://peer:8080",
			uri:  "kojo://global/agents/ag_x/a",
			want: "http://peer:8080/api/v1/peers/blobs/global/agents/ag_x/a",
		},
		{
			// Caller path is overwritten — defense in depth.
			base: "http://peer:8080/admin?token=evil",
			uri:  "kojo://global/agents/ag_x/a",
			want: "http://peer:8080/api/v1/peers/blobs/global/agents/ag_x/a",
		},
		{
			// Non-kojo:// URI passes through as-is.
			base: "http://peer:8080",
			uri:  "global/agents/ag_x/a",
			want: "http://peer:8080/api/v1/peers/blobs/global/agents/ag_x/a",
		},
	}
	for _, tc := range cases {
		got, err := buildPeerBlobURL(tc.base, tc.uri)
		if err != nil {
			t.Fatalf("buildPeerBlobURL(%q): %v", tc.base, err)
		}
		if got != tc.want {
			t.Fatalf("buildPeerBlobURL(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

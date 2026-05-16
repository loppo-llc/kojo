package agent

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/store"
)

// newWiredBlob returns a fully-wired blob.Store backed by a fresh
// kojo.db so SaveAvatar / DeleteAvatar / resolveAvatarBlob exercise
// the same blob_refs row insert / delete path that production runs.
// The store handle is closed at test teardown so callers don't have
// to thread the cleanup themselves.
func newWiredBlob(t *testing.T) *blob.Store {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{ConfigDir: root})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return blob.New(root,
		blob.WithRefs(blob.NewStoreRefs(st, "peer-test")),
		blob.WithHomePeer("peer-test"),
	)
}

// TestSaveAvatar_PublishesBlob pins the happy path of SaveAvatar:
// the body lands at the expected blob URI with Content-Type-friendly
// extension, and resolveAvatarBlob finds it on the next read.
func TestSaveAvatar_PublishesBlob(t *testing.T) {
	bs := newWiredBlob(t)
	body := "fake-png-body"
	if err := SaveAvatar(bs, "ag_1", strings.NewReader(body), ".png"); err != nil {
		t.Fatalf("SaveAvatar: %v", err)
	}

	ext, obj, ok := resolveAvatarBlob(bs, "ag_1")
	if !ok {
		t.Fatalf("resolveAvatarBlob: !ok after SaveAvatar")
	}
	if ext != ".png" {
		t.Errorf("ext = %q; want %q", ext, ".png")
	}
	if obj.Size != int64(len(body)) {
		t.Errorf("Size = %d; want %d", obj.Size, len(body))
	}
	if obj.SHA256 == "" {
		t.Errorf("SHA256 unset; expected blob_refs to populate")
	}
	if obj.ETag == "" {
		t.Errorf("ETag unset; expected blob_refs to populate")
	}
}

// TestSaveAvatar_RemovesOtherExtensions verifies the design contract
// that an agent presents at most one avatar at a time. Uploading
// avatar.svg after avatar.png must drop the .png blob — without
// this, resolveAvatarBlob's fixed probe order would surface the
// stale .png and silently shadow the new .svg.
func TestSaveAvatar_RemovesOtherExtensions(t *testing.T) {
	bs := newWiredBlob(t)
	if err := SaveAvatar(bs, "ag_1", strings.NewReader("png"), ".png"); err != nil {
		t.Fatalf("SaveAvatar png: %v", err)
	}
	if err := SaveAvatar(bs, "ag_1", strings.NewReader("svg"), ".svg"); err != nil {
		t.Fatalf("SaveAvatar svg: %v", err)
	}

	ext, _, ok := resolveAvatarBlob(bs, "ag_1")
	if !ok {
		t.Fatalf("resolveAvatarBlob: !ok after dual SaveAvatar")
	}
	if ext != ".svg" {
		t.Errorf("ext = %q; want %q (latest upload wins, prior .png must be deleted)", ext, ".svg")
	}

	// .png blob row must be gone.
	if _, err := bs.Head(blob.ScopeGlobal, "agents/ag_1/avatar.png"); err == nil {
		t.Errorf("avatar.png still present after .svg SaveAvatar; want ErrNotFound")
	}
}

// TestSaveAvatar_NormalizesExtensionWithoutDot verifies that callers
// that pass "png" instead of ".png" still produce a valid URI. The
// HTTP handler today already lowercases + dots the extension via
// filepath.Ext, but defensive normalisation keeps internal callers
// (fork, future migration code) from accidentally publishing at
// `agents/<id>/avatarpng`.
func TestSaveAvatar_NormalizesExtensionWithoutDot(t *testing.T) {
	bs := newWiredBlob(t)
	if err := SaveAvatar(bs, "ag_1", strings.NewReader("body"), "png"); err != nil {
		t.Fatalf("SaveAvatar: %v", err)
	}
	if _, err := bs.Head(blob.ScopeGlobal, "agents/ag_1/avatar.png"); err != nil {
		t.Errorf("expected avatar.png; got %v", err)
	}
}

// TestSaveAvatar_NilStoreReturnsError pins the safety net for HTTP
// handlers that race startup: a SaveAvatar called before Manager.
// SetBlobStore wired the store must surface a clear error rather
// than panic on the nil dereference.
func TestSaveAvatar_NilStoreReturnsError(t *testing.T) {
	if err := SaveAvatar(nil, "ag_1", strings.NewReader("x"), ".png"); err == nil {
		t.Errorf("SaveAvatar(nil store): want error, got nil")
	}
}

// TestResolveAvatarBlob_NotFound verifies the no-avatar path. A fresh
// agent with no upload returns ok=false; callers (avatarMeta,
// ServeAvatar) must take their fallback branch (zero hash, SVG
// fallback respectively).
func TestResolveAvatarBlob_NotFound(t *testing.T) {
	bs := newWiredBlob(t)
	if ext, obj, ok := resolveAvatarBlob(bs, "no-such-agent"); ok || ext != "" || obj != nil {
		t.Errorf("resolveAvatarBlob on fresh agent: ok=%v ext=%q obj=%+v; want ok=false", ok, ext, obj)
	}
}

// TestResolveAvatarBlob_NilStore guards against a nil store; required
// for tests that build *Manager without SetBlobStore.
func TestResolveAvatarBlob_NilStore(t *testing.T) {
	if _, _, ok := resolveAvatarBlob(nil, "ag_1"); ok {
		t.Errorf("resolveAvatarBlob(nil): want ok=false")
	}
}

// TestDeleteAvatar_RemovesAllExtensions verifies the cleanup helper
// used by Manager.Delete: a re-created agent inheriting the same id
// must NOT see a stale blob from the prior incarnation. The helper
// must sweep every probed extension regardless of which one was
// canonical at delete time.
func TestDeleteAvatar_RemovesAllExtensions(t *testing.T) {
	bs := newWiredBlob(t)
	if err := SaveAvatar(bs, "ag_1", strings.NewReader("png"), ".png"); err != nil {
		t.Fatalf("SaveAvatar: %v", err)
	}

	if err := DeleteAvatar(bs, "ag_1"); err != nil {
		t.Fatalf("DeleteAvatar: %v", err)
	}
	if _, _, ok := resolveAvatarBlob(bs, "ag_1"); ok {
		t.Errorf("avatar still resolvable after DeleteAvatar")
	}
}

// TestDeleteAvatar_MissingIsNoop pins the idempotent contract:
// callers (Manager.Delete) call DeleteAvatar unconditionally, so
// the no-avatar case must succeed silently rather than surface
// ErrNotFound to the operator.
func TestDeleteAvatar_MissingIsNoop(t *testing.T) {
	bs := newWiredBlob(t)
	if err := DeleteAvatar(bs, "never-uploaded"); err != nil {
		t.Errorf("DeleteAvatar on missing: %v; want nil", err)
	}
}

// TestDeleteAvatar_NilStoreNoop guards the safety net for the
// reset/delete paths that may run before SetBlobStore wired the
// store (test fixtures, very early shutdown).
func TestDeleteAvatar_NilStoreNoop(t *testing.T) {
	if err := DeleteAvatar(nil, "ag_1"); err != nil {
		t.Errorf("DeleteAvatar(nil): %v; want nil", err)
	}
}

// TestServeAvatar_BlobFound serves the published blob bytes with the
// extension-derived Content-Type. Verifies the Open + ServeContent
// path so a regression that mishandled the blob.Open ReadCloser /
// http.ServeContent ETag header wouldn't slip past tests.
func TestServeAvatar_BlobFound(t *testing.T) {
	bs := newWiredBlob(t)
	body := "fake-png"
	if err := SaveAvatar(bs, "ag_1", strings.NewReader(body), ".png"); err != nil {
		t.Fatalf("SaveAvatar: %v", err)
	}

	r := httptest.NewRequest("GET", "/api/v1/agents/ag_1/avatar", nil)
	w := httptest.NewRecorder()
	a := &Agent{ID: "ag_1", Name: "Alice"}

	ServeAvatar(bs, w, r, a)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q; want image/png", ct)
	}
	if etag := resp.Header.Get("ETag"); etag == "" {
		t.Errorf("ETag header empty; expected sha256-derived etag")
	}
	got := w.Body.String()
	if got != body {
		t.Errorf("body = %q; want %q", got, body)
	}
}

// TestServeAvatar_FallbackSVG pins the no-avatar branch: a fresh
// agent without an uploaded avatar gets the generated SVG so the
// UI never shows a broken image. Required by the v0 contract; an
// earlier draft of the cutover regressed this and only showed
// blank when blob_refs was empty.
func TestServeAvatar_FallbackSVG(t *testing.T) {
	bs := newWiredBlob(t)

	r := httptest.NewRequest("GET", "/api/v1/agents/ag_1/avatar", nil)
	w := httptest.NewRecorder()
	a := &Agent{ID: "ag_1", Name: "Alice"}

	ServeAvatar(bs, w, r, a)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("Content-Type = %q; want image/svg+xml", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<svg") {
		t.Errorf("body missing <svg> tag: %q", body[:min(80, len(body))])
	}
}

// TestServeAvatar_NilStoreFallsBackToSVG guards a stricter invariant:
// when the blob store hasn't been wired (test scaffolding), the
// handler must NOT panic — it falls through to the SVG fallback.
func TestServeAvatar_NilStoreFallsBackToSVG(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/v1/agents/ag_1/avatar", nil)
	w := httptest.NewRecorder()
	a := &Agent{ID: "ag_1", Name: "Alice"}

	ServeAvatar(nil, w, r, a)
	if ct := w.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("Content-Type = %q; want image/svg+xml (SVG fallback)", ct)
	}
}

// TestManagerAvatarMeta_SHA256Hash pins the AvatarHash contract: the
// hash is derived from blob_refs sha256 (strong) when available, so
// a re-upload with the same body produces the same hash and HTTP
// caches keyed by ?h=<hash> stay warm. A regression that fell back
// to ModTime even when sha was present would surface here.
func TestManagerAvatarMeta_SHA256Hash(t *testing.T) {
	bs := newWiredBlob(t)
	if err := SaveAvatar(bs, "ag_1", bytes.NewReader([]byte("body")), ".png"); err != nil {
		t.Fatalf("SaveAvatar: %v", err)
	}
	m := &Manager{blobStore: bs}
	exists, hash := m.avatarMeta("ag_1")
	if !exists {
		t.Fatalf("exists=false after SaveAvatar")
	}
	if hash == "" {
		t.Fatalf("hash empty")
	}
	// Re-upload identical body — hash must match.
	if err := SaveAvatar(bs, "ag_1", bytes.NewReader([]byte("body")), ".png"); err != nil {
		t.Fatalf("re-upload: %v", err)
	}
	_, hash2 := m.avatarMeta("ag_1")
	if hash != hash2 {
		t.Errorf("hash changed across identical re-upload: %q vs %q", hash, hash2)
	}

	// Uploading a different body must produce a different hash.
	if err := SaveAvatar(bs, "ag_1", bytes.NewReader([]byte("different")), ".png"); err != nil {
		t.Fatalf("re-upload different: %v", err)
	}
	_, hash3 := m.avatarMeta("ag_1")
	if hash == hash3 {
		t.Errorf("hash unchanged after body change: %q", hash)
	}
}

// TestManagerAvatarMeta_NoAvatarReturnsZero pins the negative path
// so applyAvatarMeta's "fall back to UpdatedAt" branch is reachable.
func TestManagerAvatarMeta_NoAvatarReturnsZero(t *testing.T) {
	bs := newWiredBlob(t)
	m := &Manager{blobStore: bs}
	exists, hash := m.avatarMeta("never-uploaded")
	if exists || hash != "" {
		t.Errorf("avatarMeta on fresh agent: exists=%v hash=%q; want false, \"\"", exists, hash)
	}
}

// TestManagerAvatarMeta_NilStoreReturnsZero guards the nil-store
// branch: tests that build *Manager without SetBlobStore must not
// crash on the avatar-meta probe done by Get / List / Update etc.
func TestManagerAvatarMeta_NilStoreReturnsZero(t *testing.T) {
	m := &Manager{}
	exists, _ := m.avatarMeta("ag_1")
	if exists {
		t.Errorf("avatarMeta(nil store): exists=true; want false")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

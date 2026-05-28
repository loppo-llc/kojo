package blob

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
)

// openWiredStore returns a blob.Store wired to a fresh *store.Store
// under a private temp dir. The store handle is closed automatically
// at test teardown so callers don't have to thread the cleanup.
func openWiredStore(t *testing.T) (*Store, *store.Store) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{ConfigDir: root})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	bs := New(root,
		WithRefs(NewStoreRefs(st, "peer-test")),
		WithHomePeer("peer-test"),
	)
	return bs, st
}

func TestWiredPutPopulatesRefs(t *testing.T) {
	bs, st := openWiredStore(t)
	body := "alice avatar"

	o, err := bs.Put(ScopeGlobal, "agents/ag_1/avatar.png",
		strings.NewReader(body), PutOptions{})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if o.SHA256 == "" || o.ETag != "sha256:"+o.SHA256 {
		t.Errorf("Put result missing digest: %+v", o)
	}

	// blob_refs row must mirror the fs publish exactly.
	ref, err := st.GetBlobRef(context.Background(), "kojo://global/agents/ag_1/avatar.png")
	if err != nil {
		t.Fatalf("GetBlobRef: %v", err)
	}
	if ref.SHA256 != o.SHA256 {
		t.Errorf("ref.sha256 = %s, want %s", ref.SHA256, o.SHA256)
	}
	if ref.Size != int64(len(body)) {
		t.Errorf("ref.size = %d, want %d", ref.Size, len(body))
	}
	if ref.HomePeer != "peer-test" {
		t.Errorf("ref.home_peer = %q, want peer-test", ref.HomePeer)
	}
	if ref.Scope != "global" {
		t.Errorf("ref.scope = %q", ref.Scope)
	}
}

func TestWiredHeadGetReadDigestFromCache(t *testing.T) {
	bs, _ := openWiredStore(t)
	body := "books body"
	want, err := bs.Put(ScopeGlobal, "agents/ag_1/books/x.md",
		strings.NewReader(body), PutOptions{})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	h, err := bs.Head(ScopeGlobal, "agents/ag_1/books/x.md")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if h.ETag != want.ETag || h.SHA256 != want.SHA256 {
		t.Errorf("Head missed cache: %+v vs Put %+v", h, want)
	}
	rc, g, err := bs.Get(ScopeGlobal, "agents/ag_1/books/x.md")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	if _, err := io.Copy(io.Discard, rc); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if g.ETag != want.ETag {
		t.Errorf("Get etag = %s, want %s", g.ETag, want.ETag)
	}
}

func TestWiredIfMatchUsesCacheNotFile(t *testing.T) {
	bs, st := openWiredStore(t)
	first, err := bs.Put(ScopeGlobal, "agents/ag_1/avatar.png",
		strings.NewReader("v1"), PutOptions{})
	if err != nil {
		t.Fatalf("Put v1: %v", err)
	}

	// Pin the cache row to a known digest, then prove IfMatch reads
	// it (not the file): force an out-of-band sha256 in the row that
	// disagrees with the filesystem and confirm an IfMatch with the
	// cached value succeeds even though Verify would say otherwise.
	if _, err := st.InsertOrReplaceBlobRef(context.Background(), &store.BlobRefRecord{
		URI: "kojo://global/agents/ag_1/avatar.png", Scope: "global", HomePeer: "peer-test",
		Size: 2, SHA256: "00ff00ff",
	}, store.BlobRefInsertOptions{}); err != nil {
		t.Fatalf("force ref: %v", err)
	}

	// IfMatch with the (forged) cache etag must succeed because the
	// cache, not the file, is the source of truth in the wired path.
	if _, err := bs.Put(ScopeGlobal, "agents/ag_1/avatar.png",
		strings.NewReader("v2"),
		PutOptions{IfMatch: "sha256:00ff00ff"}); err != nil {
		t.Fatalf("IfMatch via cache: %v", err)
	}
	// And IfMatch with the original (real) etag must now fail since
	// the cache no longer reflects it.
	if _, err := bs.Put(ScopeGlobal, "agents/ag_1/avatar.png",
		strings.NewReader("v3"),
		PutOptions{IfMatch: first.ETag}); !errors.Is(err, ErrETagMismatch) {
		t.Errorf("IfMatch with stale etag: got %v want ErrETagMismatch", err)
	}
}

// TestPutRefusesHandoffPending pins the §3.7 invariant added
// during Codex review: blob.Store.Put on a row whose
// handoff_pending flag is set MUST refuse before atomicWrite so
// the on-disk body is unchanged.
func TestPutRefusesHandoffPending(t *testing.T) {
	bs, st := openWiredStore(t)
	first, err := bs.Put(ScopeGlobal, "agents/ag_1/transcript",
		strings.NewReader("v1"), PutOptions{})
	if err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	ctx := context.Background()
	if err := st.SetBlobRefHandoffPending(ctx,
		"kojo://global/agents/ag_1/transcript", true); err != nil {
		t.Fatalf("SetBlobRefHandoffPending: %v", err)
	}

	// Runtime write that would change the body: must refuse
	// pre-atomicWrite so the file on disk stays at v1.
	if _, err := bs.Put(ScopeGlobal, "agents/ag_1/transcript",
		strings.NewReader("v2"), PutOptions{}); !errors.Is(err, ErrHandoffPending) {
		t.Fatalf("Put under handoff_pending: err = %v, want ErrHandoffPending", err)
	}
	rc, obj, err := bs.Get(ScopeGlobal, "agents/ag_1/transcript")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "v1" {
		t.Errorf("body changed after refused Put: %q", string(got))
	}
	if obj.SHA256 != first.SHA256 {
		t.Errorf("ref sha changed: %s vs %s", obj.SHA256, first.SHA256)
	}

	// Same digest re-Put IS admitted (idempotent path) — the
	// scrubber relies on this even mid-handoff.
	if _, err := bs.Put(ScopeGlobal, "agents/ag_1/transcript",
		strings.NewReader("v1"),
		PutOptions{ExpectedSHA256: first.SHA256}); err != nil {
		t.Fatalf("idempotent re-Put under handoff_pending: %v", err)
	}

	// BypassHandoffPending unlocks the change: this is the
	// orchestrator's pull path.
	if _, err := bs.Put(ScopeGlobal, "agents/ag_1/transcript",
		strings.NewReader("v2"),
		PutOptions{BypassHandoffPending: true}); err != nil {
		t.Fatalf("Put with BypassHandoffPending: %v", err)
	}
	rc2, _, err := bs.Get(ScopeGlobal, "agents/ag_1/transcript")
	if err != nil {
		t.Fatalf("Get after bypass: %v", err)
	}
	defer rc2.Close()
	got2, _ := io.ReadAll(rc2)
	if string(got2) != "v2" {
		t.Errorf("body did not advance after bypass: %q", string(got2))
	}
}

func TestWiredDeleteRemovesRefRow(t *testing.T) {
	bs, st := openWiredStore(t)
	if _, err := bs.Put(ScopeGlobal, "agents/ag_1/avatar.png",
		strings.NewReader("x"), PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := bs.Delete(ScopeGlobal, "agents/ag_1/avatar.png", DeleteOptions{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.GetBlobRef(context.Background(), "kojo://global/agents/ag_1/avatar.png"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("blob_refs row not removed: %v", err)
	}
}

func TestWiredPutRequiresHomePeer(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{ConfigDir: root})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	// Wired refs but no WithHomePeer — Put must fail rather than
	// publishing a body without a tracking row.
	bs := New(root, WithRefs(NewStoreRefs(st, "")))
	if _, err := bs.Put(ScopeGlobal, "agents/ag_1/avatar.png",
		strings.NewReader("x"), PutOptions{}); err == nil {
		t.Fatal("expected home_peer required error")
	}
}

func TestBuildURIEncodesAmbiguousChars(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		// Plain ASCII path: no encoding required, separators preserved.
		{"agents/ag_1/avatar.png", "kojo://global/agents/ag_1/avatar.png"},
		// Space in segment must be percent-encoded so log readers
		// don't normalize to `+` and so the URI is HTTP-clean.
		{"agents/ag 1/avatar.png", "kojo://global/agents/ag%201/avatar.png"},
		// `#` would otherwise be parsed as a fragment by every URI
		// parser in the food chain.
		{"agents/ag_1/note#1.md", "kojo://global/agents/ag_1/note%231.md"},
		// Literal `%` must be escaped or it would look like an
		// already-encoded byte.
		{"agents/ag_1/100%.png", "kojo://global/agents/ag_1/100%25.png"},
	}
	for _, c := range cases {
		got := BuildURI(ScopeGlobal, c.path)
		if got != c.want {
			t.Errorf("BuildURI(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestNoopRefsLeavesDigestEmptyOnHead(t *testing.T) {
	// No WithRefs → noopRefs → Head returns Size/ModTime only.
	root := t.TempDir()
	bs := New(root)
	if _, err := bs.Put(ScopeGlobal, "agents/ag_1/avatar.png",
		strings.NewReader("x"), PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	h, err := bs.Head(ScopeGlobal, "agents/ag_1/avatar.png")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if h.SHA256 != "" || h.ETag != "" {
		t.Errorf("noopRefs path leaked digest into Head: %+v", h)
	}
}

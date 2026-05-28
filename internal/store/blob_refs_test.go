package store

import (
	"context"
	"errors"
	"testing"
)

func seedBlobRef(t *testing.T, s *Store, uri, scope, sha string, size int64) *BlobRefRecord {
	t.Helper()
	rec, err := s.InsertOrReplaceBlobRef(context.Background(), &BlobRefRecord{
		URI:      uri,
		Scope:    scope,
		HomePeer: "peer-a",
		Size:     size,
		SHA256:   sha,
	}, BlobRefInsertOptions{})
	if err != nil {
		t.Fatalf("seed %s: %v", uri, err)
	}
	return rec
}

func TestInsertOrReplaceBlobRefRequiredFields(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	cases := []struct {
		name string
		rec  *BlobRefRecord
	}{
		{"nil", nil},
		{"empty uri", &BlobRefRecord{Scope: "global", HomePeer: "p", SHA256: "abc"}},
		{"empty scope", &BlobRefRecord{URI: "kojo://global/x", HomePeer: "p", SHA256: "abc"}},
		{"empty sha", &BlobRefRecord{URI: "kojo://global/x", Scope: "global", HomePeer: "p"}},
		{"empty home_peer", &BlobRefRecord{URI: "kojo://global/x", Scope: "global", SHA256: "abc"}},
	}
	for _, c := range cases {
		if _, err := s.InsertOrReplaceBlobRef(ctx, c.rec, BlobRefInsertOptions{}); err == nil {
			t.Errorf("%s: expected validation error", c.name)
		}
	}
}

func TestInsertOrReplaceBlobRefRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	first := seedBlobRef(t, s, "kojo://global/agents/ag_1/avatar.png", "global", "deadbeef", 42)
	if first.Refcount != 1 {
		t.Errorf("default refcount = %d, want 1", first.Refcount)
	}
	if first.CreatedAt == 0 || first.UpdatedAt == 0 {
		t.Errorf("timestamps not set: %+v", first)
	}

	got, err := s.GetBlobRef(ctx, first.URI)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SHA256 != "deadbeef" || got.Size != 42 {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// A re-Put bumps size/sha256/updated_at but preserves created_at.
	prevCreated := first.CreatedAt
	updated, err := s.InsertOrReplaceBlobRef(ctx, &BlobRefRecord{
		URI: first.URI, Scope: "global", HomePeer: "peer-a",
		Size: 99, SHA256: "cafebabe",
	}, BlobRefInsertOptions{})
	if err != nil {
		t.Fatalf("re-Put: %v", err)
	}
	if updated.SHA256 != "cafebabe" || updated.Size != 99 {
		t.Errorf("re-Put body: %+v", updated)
	}
	if updated.CreatedAt != prevCreated {
		t.Errorf("created_at changed: %d → %d", prevCreated, updated.CreatedAt)
	}
}

func TestInsertOrReplaceBlobRefPreservesManagedFields(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	uri := "kojo://global/agents/ag_1/avatar.png"
	// Initial Put with management fields populated by some other
	// subsystem (pin policy from an admin UI, last_seen_ok from a
	// scrub job, handoff_pending set during a device handoff).
	if _, err := s.InsertOrReplaceBlobRef(ctx, &BlobRefRecord{
		URI: uri, Scope: "global", HomePeer: "peer-a", Size: 1, SHA256: "abc",
		PinPolicy: `{"peers":["p1","p2"]}`, LastSeenOK: 1700,
		HandoffPending: true,
	}, BlobRefInsertOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Idempotent re-Put (sha256 unchanged) must NOT wipe management
	// fields. last_seen_ok is preserved because the body hash
	// hasn't moved.
	if _, err := s.InsertOrReplaceBlobRef(ctx, &BlobRefRecord{
		URI: uri, Scope: "global", HomePeer: "peer-a", Size: 1, SHA256: "abc",
	}, BlobRefInsertOptions{}); err != nil {
		t.Fatalf("re-Put (same sha): %v", err)
	}
	got, err := s.GetBlobRef(ctx, uri)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.PinPolicy != `{"peers":["p1","p2"]}` {
		t.Errorf("pin_policy wiped: %q", got.PinPolicy)
	}
	if got.LastSeenOK != 1700 {
		t.Errorf("last_seen_ok wiped on idempotent re-Put: %d", got.LastSeenOK)
	}
	if !got.HandoffPending {
		t.Errorf("handoff_pending wiped")
	}

	// Different-sha re-Put during handoff_pending=true MUST be
	// refused. §3.7 device-switch invariant: the orchestrator
	// commits the target peer to pull a body with a specific
	// digest; a runtime write that flips sha256 underneath
	// would silently corrupt the handoff.
	if _, err := s.InsertOrReplaceBlobRef(ctx, &BlobRefRecord{
		URI: uri, Scope: "global", HomePeer: "peer-a", Size: 2, SHA256: "def",
	}, BlobRefInsertOptions{}); !errors.Is(err, ErrHandoffPending) {
		t.Fatalf("re-Put (new sha during handoff): err=%v, want ErrHandoffPending", err)
	}
	got, err = s.GetBlobRef(ctx, uri)
	if err != nil {
		t.Fatalf("Get after refused re-Put: %v", err)
	}
	if got.SHA256 != "abc" || got.Size != 1 {
		t.Errorf("row should remain untouched after refused re-Put: %+v", got)
	}

	// Clear handoff_pending (the §3.7 abort / complete path)
	// and the same different-sha re-Put now succeeds:
	// management fields preserved, last_seen_ok cleared per
	// §3.15-bis because the digest changed.
	if err := s.SetBlobRefHandoffPending(ctx, uri, false); err != nil {
		t.Fatalf("clear handoff_pending: %v", err)
	}
	if _, err := s.InsertOrReplaceBlobRef(ctx, &BlobRefRecord{
		URI: uri, Scope: "global", HomePeer: "peer-a", Size: 2, SHA256: "def",
	}, BlobRefInsertOptions{}); err != nil {
		t.Fatalf("re-Put (new sha, handoff cleared): %v", err)
	}
	got, err = s.GetBlobRef(ctx, uri)
	if err != nil {
		t.Fatalf("Get after sha change: %v", err)
	}
	if got.SHA256 != "def" || got.Size != 2 {
		t.Errorf("body not refreshed: %+v", got)
	}
	if got.LastSeenOK != 0 {
		t.Errorf("last_seen_ok must clear when sha256 changes; got %d", got.LastSeenOK)
	}
	if got.PinPolicy != `{"peers":["p1","p2"]}` {
		t.Errorf("pin_policy wiped on sha change: %q", got.PinPolicy)
	}
}

func TestInsertOrReplaceBlobRefClearsGCMark(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	rec := seedBlobRef(t, s, "kojo://global/agents/ag_1/avatar.png", "global", "abc", 1)
	if err := s.MarkBlobRefForGC(ctx, rec.URI); err != nil {
		t.Fatalf("mark: %v", err)
	}
	marked, _ := s.GetBlobRef(ctx, rec.URI)
	if marked.MarkedForGCAt == 0 {
		t.Fatal("mark did not stamp")
	}
	// A subsequent Put implicitly resurrects the row — the mark must
	// clear so a stale sweeper doesn't physically delete a now-live
	// blob.
	if _, err := s.InsertOrReplaceBlobRef(ctx, &BlobRefRecord{
		URI: rec.URI, Scope: "global", HomePeer: "peer-a",
		Size: 2, SHA256: "def",
	}, BlobRefInsertOptions{}); err != nil {
		t.Fatalf("re-Put: %v", err)
	}
	got, _ := s.GetBlobRef(ctx, rec.URI)
	if got.MarkedForGCAt != 0 {
		t.Errorf("re-Put did not clear mark: %d", got.MarkedForGCAt)
	}
}

func TestGetBlobRefNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetBlobRef(context.Background(), "kojo://global/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing uri: got %v want ErrNotFound", err)
	}
}

func TestDeleteBlobRefIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	uri := "kojo://global/agents/ag_1/avatar.png"
	seedBlobRef(t, s, uri, "global", "abc", 1)
	if err := s.DeleteBlobRef(ctx, uri); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Second delete is a no-op (no error).
	if err := s.DeleteBlobRef(ctx, uri); err != nil {
		t.Fatalf("repeat delete: %v", err)
	}
	if _, err := s.GetBlobRef(ctx, uri); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after delete: %v", err)
	}
}

func TestListBlobRefsByScopeAndPrefix(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedBlobRef(t, s, "kojo://global/agents/ag_1/avatar.png", "global", "a", 1)
	seedBlobRef(t, s, "kojo://global/agents/ag_1/books/x.md", "global", "b", 2)
	seedBlobRef(t, s, "kojo://global/agents/ag_2/avatar.png", "global", "c", 3)
	seedBlobRef(t, s, "kojo://local/agents/ag_1/temp/t.bin", "local", "d", 4)

	all, err := s.ListBlobRefs(ctx, ListBlobRefsOptions{Scope: "global"})
	if err != nil {
		t.Fatalf("list global: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("global count = %d, want 3", len(all))
	}

	ag1, err := s.ListBlobRefs(ctx, ListBlobRefsOptions{
		URIPrefix: "kojo://global/agents/ag_1/",
	})
	if err != nil {
		t.Fatalf("list ag_1: %v", err)
	}
	if len(ag1) != 2 {
		t.Errorf("ag_1 count = %d, want 2", len(ag1))
	}
	// Ordered by uri ASC.
	if ag1[0].URI > ag1[1].URI {
		t.Errorf("List not ordered: %s, %s", ag1[0].URI, ag1[1].URI)
	}
}

func TestListBlobRefsHidesGCMarkedByDefault(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	rec := seedBlobRef(t, s, "kojo://global/agents/ag_1/avatar.png", "global", "abc", 1)
	if err := s.MarkBlobRefForGC(ctx, rec.URI); err != nil {
		t.Fatalf("mark: %v", err)
	}
	got, err := s.ListBlobRefs(ctx, ListBlobRefsOptions{Scope: "global"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("default list should hide GC-marked rows: got %d", len(got))
	}
	withGC, err := s.ListBlobRefs(ctx, ListBlobRefsOptions{Scope: "global", IncludeMarkedForGC: true})
	if err != nil {
		t.Fatalf("list incl GC: %v", err)
	}
	if len(withGC) != 1 {
		t.Errorf("incl GC list count = %d, want 1", len(withGC))
	}
}

func TestNextPrefix(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"abc", "abd", true},
		{"ab\xff", "ac", true},
		{"a\xff\xff", "b", true},
		{"\xff", "", false},
		{"\xff\xff", "", false},
	}
	for _, c := range cases {
		got, ok := nextPrefix(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("nextPrefix(%q) = (%q, %v), want (%q, %v)",
				c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestListBlobRefsPrefixIsLiteralAndCaseSensitive(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// The range scan must treat `_` as a literal byte (LIKE would
	// have matched any single character) AND must be case-sensitive
	// (SQLite's default LIKE is ASCII case-insensitive).
	seedBlobRef(t, s, "kojo://global/a_b.png", "global", "a", 1)
	seedBlobRef(t, s, "kojo://global/aXb.png", "global", "b", 1)
	seedBlobRef(t, s, "kojo://global/A_b.png", "global", "c", 1) // upper-case sibling

	got, err := s.ListBlobRefs(ctx, ListBlobRefsOptions{
		URIPrefix: "kojo://global/a_",
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].URI != "kojo://global/a_b.png" {
		t.Errorf("range scan over-matched: %+v", got)
	}
}

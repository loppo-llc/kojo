package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// fixedClockStore wraps NewWebDAVTokenStore + a deterministic clock so
// tests can advance "now" without sleeping. The store's now field is
// package-private; tests in the same package mutate it directly here.
func newWebDAVStoreForTest(t *testing.T) (*WebDAVTokenStore, *store.Store, *int64) {
	t.Helper()
	kv := newKVStore(t)
	s, err := NewWebDAVTokenStore(context.Background(), kv)
	if err != nil {
		t.Fatalf("NewWebDAVTokenStore: %v", err)
	}
	var now int64 = 1_700_000_000_000 // ms since epoch, deterministic
	s.now = func() time.Time { return time.UnixMilli(now) }
	return s, kv, &now
}

func TestWebDAVTokenStore_IssueAndVerify(t *testing.T) {
	s, _, _ := newWebDAVStoreForTest(t)
	res, err := s.Issue(context.Background(), "iPad mount", 1*time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if res.Token == "" || len(res.Token) != 64 {
		t.Fatalf("Issue: token shape = %q (len=%d)", res.Token, len(res.Token))
	}
	if res.ID == "" || len(res.ID) != 32 {
		t.Fatalf("Issue: id shape = %q (len=%d)", res.ID, len(res.ID))
	}
	if !s.Verify(res.Token) {
		t.Errorf("Verify: fresh token rejected")
	}
	if s.Verify("not-the-token") {
		t.Errorf("Verify: random string accepted")
	}
	if s.Verify("") {
		t.Errorf("Verify: empty string accepted")
	}
}

func TestWebDAVTokenStore_ListReturnsMetadataNotRaw(t *testing.T) {
	s, _, _ := newWebDAVStoreForTest(t)
	res, err := s.Issue(context.Background(), "label-A", 1*time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("List: want 1 entry, got %d", len(list))
	}
	if list[0].ID != res.ID || list[0].Label != "label-A" {
		t.Errorf("List mismatch: %+v", list[0])
	}
	// The raw token must NOT leak through metadata.
	for _, m := range list {
		if strings.Contains(m.ID, res.Token) || strings.Contains(m.Label, res.Token) {
			t.Errorf("List leaked raw token via %+v", m)
		}
	}
}

func TestWebDAVTokenStore_RevokeRemovesFromVerify(t *testing.T) {
	s, _, _ := newWebDAVStoreForTest(t)
	res, err := s.Issue(context.Background(), "to-revoke", 1*time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if !s.Verify(res.Token) {
		t.Fatal("Verify failed before revoke")
	}
	if err := s.Revoke(context.Background(), res.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if s.Verify(res.Token) {
		t.Errorf("Verify accepted token after revoke")
	}
	// Second revoke is idempotent on a known id (cache miss path);
	// it should return ErrNotFound so the HTTP layer can map to 404
	// for an unknown id but still surface success when the cache
	// was already empty.
	err = s.Revoke(context.Background(), res.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Errorf("repeat revoke: %v", err)
	}
}

func TestWebDAVTokenStore_VerifyRejectsExpired(t *testing.T) {
	s, _, now := newWebDAVStoreForTest(t)
	res, err := s.Issue(context.Background(), "soon-gone", 10*time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if !s.Verify(res.Token) {
		t.Fatal("Verify before expiry")
	}
	// Advance time past the expiry.
	*now += 11 * 60 * 1000
	if s.Verify(res.Token) {
		t.Errorf("Verify accepted expired token")
	}
	// Sweep should physically remove the row.
	n, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("Sweep: removed %d, want 1", n)
	}
	if len(s.List()) != 0 {
		t.Errorf("List: want 0 after sweep, got %d", len(s.List()))
	}
}

func TestWebDAVTokenStore_IssueRejectsBadInputs(t *testing.T) {
	s, _, _ := newWebDAVStoreForTest(t)
	cases := []struct {
		name  string
		label string
		ttl   time.Duration
	}{
		{"empty label", "", 1 * time.Hour},
		{"control chars", "bad\nlabel", 1 * time.Hour},
		{"label too long", strings.Repeat("a", webdavTokenLabelMaxLen+1), 1 * time.Hour},
		{"ttl too short", "ok", 1 * time.Second},
		{"ttl too long", "ok", 60 * 24 * time.Hour},
		{"ttl zero", "ok", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.Issue(context.Background(), c.label, c.ttl); err == nil {
				t.Errorf("Issue: want error for %s", c.name)
			}
		})
	}
}

func TestWebDAVTokenStore_ReloadRehydrates(t *testing.T) {
	s, kv, _ := newWebDAVStoreForTest(t)
	res, err := s.Issue(context.Background(), "across-restart", 1*time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// Drop the in-memory map and re-open from kv to simulate a
	// process restart. The initial reload would delete the row as
	// "expired" against real time.Now (fixed test stamp is in 2023);
	// construct the fresh store with the clock pinned BEFORE the
	// initial load so the row stays intact.
	fresh := &WebDAVTokenStore{
		kv:     kv,
		now:    func() time.Time { return time.UnixMilli(1_700_000_001_000) },
		random: nil, // verifier path doesn't touch random
		hashes: make(map[string]string),
		rows:   make(map[string]webdavTokenRow),
	}
	if err := fresh.reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !fresh.Verify(res.Token) {
		t.Errorf("re-opened store did not recognise existing token")
	}
}

func TestWebDAVTokenStore_RevokeUnknownIDReturnsNotFound(t *testing.T) {
	s, _, _ := newWebDAVStoreForTest(t)
	err := s.Revoke(context.Background(), "abcdef0123456789abcdef0123456789")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoke unknown: want ErrNotFound, got %v", err)
	}
	if err := s.Revoke(context.Background(), "../bad"); err == nil {
		t.Errorf("revoke invalid id: want error")
	}
}

func TestWebDAVTokenStore_ResolverIntegration(t *testing.T) {
	// Resolver.Resolve should map a presented WebDAV token to
	// RoleWebDAV after both VerifyOwner and LookupAgent miss. Pair
	// the standalone TokenStore + WebDAVTokenStore against a fresh
	// kv-backed Resolver.
	kv := newKVStore(t)
	tokenStore, err := NewTokenStore(t.TempDir(), kv, "owner-secret")
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	webdavStore, err := NewWebDAVTokenStore(context.Background(), kv)
	if err != nil {
		t.Fatalf("NewWebDAVTokenStore: %v", err)
	}
	res, err := webdavStore.Issue(context.Background(), "for-resolver", 1*time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	resolver := NewResolver(tokenStore, nil)
	resolver.SetWebDAVStore(webdavStore)

	if p := resolver.Resolve(res.Token); p.Role != RoleWebDAV {
		t.Errorf("Resolve: got role %v, want RoleWebDAV", p.Role)
	}
	if p := resolver.Resolve("owner-secret"); p.Role != RoleOwner {
		t.Errorf("Resolve: owner missed (role=%v)", p.Role)
	}
	if p := resolver.Resolve("nope"); p.Role != RoleGuest {
		t.Errorf("Resolve: unknown should be Guest, got %v", p.Role)
	}
}

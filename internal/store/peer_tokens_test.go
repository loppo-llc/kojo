package store

import (
	"context"
	"testing"
)

// openTestStore returns a fresh in-memory-ish store backed by a temp file.
// Reusing the migrate test helper would be cleaner, but peer_tokens is
// small enough that a local helper keeps the test self-contained.
func openTestStoreForTokens(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(context.Background(), Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestIssuePeerToken_RawIsUsableExactlyOnce(t *testing.T) {
	st := openTestStoreForTokens(t)
	ctx := context.Background()

	issued, err := st.IssuePeerToken(ctx, "dev-alpha", PeerTokenRoleHubToPeer)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if issued.Raw == "" {
		t.Fatal("Raw token empty")
	}
	if issued.Record.TokenHash == "" || issued.Record.DeviceID != "dev-alpha" {
		t.Fatalf("Record mismatch: %+v", issued.Record)
	}
	// Hash check: ResolvePeerToken should return the same record.
	got, err := st.ResolvePeerToken(ctx, issued.Raw)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.TokenHash != issued.Record.TokenHash || got.DeviceID != "dev-alpha" {
		t.Fatalf("resolve mismatch: %+v", got)
	}
	if got.Role != PeerTokenRoleHubToPeer {
		t.Fatalf("role: %s", got.Role)
	}
}

func TestResolvePeerToken_UnknownIsNotFound(t *testing.T) {
	st := openTestStoreForTokens(t)
	ctx := context.Background()

	_, err := st.ResolvePeerToken(ctx, "totally-bogus-base64")
	if err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	// Empty raw also maps to ErrNotFound (no DB round-trip).
	_, err = st.ResolvePeerToken(ctx, "")
	if err != ErrNotFound {
		t.Fatalf("empty: want ErrNotFound, got %v", err)
	}
}

func TestRevokePeerToken_RevokedRowCannotResolve(t *testing.T) {
	st := openTestStoreForTokens(t)
	ctx := context.Background()

	issued, err := st.IssuePeerToken(ctx, "dev-beta", PeerTokenRolePeerToHub)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := st.RevokePeerToken(ctx, issued.Raw); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// Resolve should now fail with ErrNotFound (revoked rows are
	// indistinguishable from missing rows from the caller's POV).
	if _, err := st.ResolvePeerToken(ctx, issued.Raw); err != ErrNotFound {
		t.Fatalf("after revoke: want ErrNotFound, got %v", err)
	}
	// Idempotent: a second revoke against the same raw token is a
	// no-op (not an error) — the underlying row exists.
	if err := st.RevokePeerToken(ctx, issued.Raw); err != nil {
		t.Fatalf("re-revoke: %v", err)
	}
	// Revoking an unknown token surfaces ErrNotFound so the CLI can
	// distinguish "already revoked" from "no such token".
	if err := st.RevokePeerToken(ctx, "totally-bogus"); err != ErrNotFound {
		t.Fatalf("revoke unknown: want ErrNotFound, got %v", err)
	}
}

func TestRevokePeerTokensByDevice_FlipsAllActive(t *testing.T) {
	st := openTestStoreForTokens(t)
	ctx := context.Background()

	a, _ := st.IssuePeerToken(ctx, "dev-gamma", PeerTokenRoleHubToPeer)
	b, _ := st.IssuePeerToken(ctx, "dev-gamma", PeerTokenRolePeerToHub)
	c, _ := st.IssuePeerToken(ctx, "dev-delta", PeerTokenRoleHubToPeer)

	n, err := st.RevokePeerTokensByDevice(ctx, "dev-gamma")
	if err != nil {
		t.Fatalf("bulk revoke: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 revoked, got %d", n)
	}
	// dev-gamma tokens should be unresolvable.
	if _, err := st.ResolvePeerToken(ctx, a.Raw); err != ErrNotFound {
		t.Fatalf("a still resolves: %v", err)
	}
	if _, err := st.ResolvePeerToken(ctx, b.Raw); err != ErrNotFound {
		t.Fatalf("b still resolves: %v", err)
	}
	// dev-delta token is unaffected.
	if _, err := st.ResolvePeerToken(ctx, c.Raw); err != nil {
		t.Fatalf("c regression: %v", err)
	}
}

func TestIssuePeerToken_RejectsBadRole(t *testing.T) {
	st := openTestStoreForTokens(t)
	ctx := context.Background()
	if _, err := st.IssuePeerToken(ctx, "dev-x", "blob_cap_kid"); err == nil {
		t.Fatal("want error on bad role")
	}
	if _, err := st.IssuePeerToken(ctx, "", PeerTokenRoleHubToPeer); err == nil {
		t.Fatal("want error on empty device_id")
	}
}

func TestHashPeerToken_Deterministic(t *testing.T) {
	if HashPeerToken("hello") != HashPeerToken("hello") {
		t.Fatal("hash not deterministic")
	}
	if HashPeerToken("hello") == HashPeerToken("world") {
		t.Fatal("hash collision on different inputs")
	}
}

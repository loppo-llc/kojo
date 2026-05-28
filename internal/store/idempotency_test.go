package store

import (
	"context"
	"errors"
	"testing"
)

func TestClaimIdempotencyKeyFreshClaim(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	expires := NowMillis() + 60_000
	entry, err := s.ClaimIdempotencyKey(ctx, "key-1", "op-1", "hash-a", expires)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if entry != nil {
		t.Errorf("fresh claim should return nil entry; got %+v", entry)
	}
}

func TestClaimIdempotencyKeyConcurrentInFlight(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	expires := NowMillis() + 60_000
	if _, err := s.ClaimIdempotencyKey(ctx, "key-1", "op-1", "hash-a", expires); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	_, err := s.ClaimIdempotencyKey(ctx, "key-1", "op-1", "hash-a", expires)
	if !errors.Is(err, ErrIdempotencyInFlight) {
		t.Errorf("expected in-flight; got %v", err)
	}
}

func TestClaimIdempotencyKeyConflictOnDifferentHash(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	expires := NowMillis() + 60_000
	if _, err := s.ClaimIdempotencyKey(ctx, "key-1", "op-1", "hash-a", expires); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	_, err := s.ClaimIdempotencyKey(ctx, "key-1", "op-1", "hash-b", expires)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("expected conflict; got %v", err)
	}
}

func TestFinalizeAndReplay(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	expires := NowMillis() + 60_000
	if _, err := s.ClaimIdempotencyKey(ctx, "key-1", "op-1", "hash-a", expires); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.FinalizeIdempotencyKey(ctx, "key-1", "op-1", 200, `"abc"`, `{"ok":true}`); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	// A subsequent claim with the same key+hash should return the
	// saved entry verbatim.
	entry, err := s.ClaimIdempotencyKey(ctx, "key-1", "op-1", "hash-a", expires)
	if err != nil {
		t.Fatalf("replay claim: %v", err)
	}
	if entry == nil {
		t.Fatal("expected saved entry; got nil (would re-execute handler)")
	}
	if entry.ResponseStatus != 200 {
		t.Errorf("status: %d", entry.ResponseStatus)
	}
	if entry.ResponseEtag != `"abc"` {
		t.Errorf("etag: %q", entry.ResponseEtag)
	}
	if entry.ResponseBody != `{"ok":true}` {
		t.Errorf("body: %q", entry.ResponseBody)
	}
}

func TestExpiredKeyOverwritesOnRefresh(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := NowMillis()
	// Insert a row that is already expired (expires_at < now).
	if _, err := s.ClaimIdempotencyKey(ctx, "key-1", "op-old", "hash-old", now-1_000); err != nil {
		t.Fatalf("seed expired: %v", err)
	}
	if err := s.FinalizeIdempotencyKey(ctx, "key-1", "op-old", 200, "", "stale"); err != nil {
		t.Fatalf("finalize seed: %v", err)
	}
	// Re-claim under a different hash: must succeed (expired row is
	// transparently overwritten).
	entry, err := s.ClaimIdempotencyKey(ctx, "key-1", "op-new", "hash-new", now+60_000)
	if err != nil {
		t.Errorf("expired re-claim: %v", err)
	}
	if entry != nil {
		t.Errorf("expected fresh claim (entry nil); got %+v", entry)
	}
}

func TestAbandonIdempotencyKeyDropsPendingRow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	expires := NowMillis() + 60_000
	if _, err := s.ClaimIdempotencyKey(ctx, "key-1", "op-1", "hash-a", expires); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.AbandonIdempotencyKey(ctx, "key-1", "op-1"); err != nil {
		t.Errorf("abandon: %v", err)
	}
	// Re-claim should succeed because the pending row was dropped.
	entry, err := s.ClaimIdempotencyKey(ctx, "key-1", "op-1", "hash-a", expires)
	if err != nil {
		t.Errorf("re-claim after abandon: %v", err)
	}
	if entry != nil {
		t.Errorf("expected fresh claim after abandon; got %+v", entry)
	}
}

func TestAbandonIdempotencyKeyRefusesToTouchCompletedRow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	expires := NowMillis() + 60_000
	if _, err := s.ClaimIdempotencyKey(ctx, "key-1", "op-1", "hash-a", expires); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.FinalizeIdempotencyKey(ctx, "key-1", "op-1", 200, "", "ok"); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	// Abandon must not delete a completed row — its dedup is what
	// the next retry depends on.
	if err := s.AbandonIdempotencyKey(ctx, "key-1", "op-1"); err != nil {
		t.Errorf("abandon: %v", err)
	}
	entry, err := s.ClaimIdempotencyKey(ctx, "key-1", "op-1", "hash-a", expires)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if entry == nil {
		t.Fatal("completed row was wiped by abandon — dedup defeated")
	}
}

func TestExpireIdempotencyKeysSweepsPastEntries(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := NowMillis()
	// Live row.
	if _, err := s.ClaimIdempotencyKey(ctx, "live", "op", "hash", now+60_000); err != nil {
		t.Fatalf("seed live: %v", err)
	}
	// Stale row.
	if _, err := s.ClaimIdempotencyKey(ctx, "stale", "op", "hash", now-60_000); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	n, err := s.ExpireIdempotencyKeys(ctx, now)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 expired; got %d", n)
	}
}

func TestClaimIdempotencyKeyValidation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	cases := []struct {
		name      string
		key, hash string
		expires   int64
	}{
		{"empty key", "", "h", 1_000},
		{"empty hash", "k", "", 1_000},
		{"zero expires", "k", "h", 0},
		{"negative expires", "k", "h", -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.ClaimIdempotencyKey(ctx, c.key, "op", c.hash, c.expires); err == nil {
				t.Errorf("expected error")
			}
		})
	}
}

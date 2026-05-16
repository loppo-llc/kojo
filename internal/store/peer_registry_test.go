package store

import (
	"context"
	"errors"
	"testing"
)

func seedPeer(t *testing.T, s *Store, id, name string) *PeerRecord {
	t.Helper()
	rec, err := s.UpsertPeer(context.Background(), &PeerRecord{
		DeviceID:  id,
		Name:      name,
		PublicKey: "pk-" + id,
		Status:    PeerStatusOnline,
	})
	if err != nil {
		t.Fatalf("seed peer %s: %v", id, err)
	}
	return rec
}

func TestUpsertPeerRequiredFields(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	cases := []struct {
		name string
		rec  *PeerRecord
	}{
		{"nil", nil},
		{"empty device_id", &PeerRecord{Name: "n", PublicKey: "k"}},
		{"empty name", &PeerRecord{DeviceID: "d", PublicKey: "k"}},
		{"empty public_key", &PeerRecord{DeviceID: "d", Name: "n"}},
		{"invalid status", &PeerRecord{DeviceID: "d", Name: "n", PublicKey: "k", Status: "weird"}},
	}
	for _, c := range cases {
		if _, err := s.UpsertPeer(ctx, c.rec); err == nil {
			t.Errorf("%s: expected validation error", c.name)
		}
	}
}

func TestUpsertPeerRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	first, err := s.UpsertPeer(ctx, &PeerRecord{
		DeviceID:     "dev-1",
		Name:         "alice-mac",
		PublicKey:    "pk-1",
		Capabilities: `{"os":"darwin"}`,
		LastSeen:     1700,
		Status:       PeerStatusOnline,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if first.Status != "online" || first.Capabilities != `{"os":"darwin"}` {
		t.Errorf("round-trip mismatch: %+v", first)
	}
	got, err := s.GetPeer(ctx, "dev-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PublicKey != "pk-1" {
		t.Errorf("public_key: %q", got.PublicKey)
	}
}

func TestUpsertPeerPreservesPublicKey(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.UpsertPeer(ctx, &PeerRecord{
		DeviceID: "dev-1", Name: "n", PublicKey: "pk-original", Status: "online",
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Re-upsert with a different public_key — the schema tolerates this
	// at SQL level, but our helper preserves the prior key so a hostile
	// peer can't silently rotate identity. The mutable columns
	// (capabilities / status / name) overwrite as expected.
	rec, err := s.UpsertPeer(ctx, &PeerRecord{
		DeviceID: "dev-1", Name: "renamed", PublicKey: "pk-rotation",
		Status: "degraded", Capabilities: `{"gpu":true}`,
	})
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if rec.PublicKey != "pk-original" {
		t.Errorf("public_key rotated silently: %q", rec.PublicKey)
	}
	if rec.Name != "renamed" || rec.Status != "degraded" || rec.Capabilities != `{"gpu":true}` {
		t.Errorf("mutable cols not refreshed: %+v", rec)
	}
}

func TestGetPeerNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetPeer(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: got %v want ErrNotFound", err)
	}
}

func TestListPeersOrderAndStatusFilter(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// Insert with explicit last_seen so the ORDER BY is deterministic.
	for _, p := range []struct {
		id, status string
		seen       int64
	}{
		{"dev-a", "online", 3000},
		{"dev-b", "offline", 1000},
		{"dev-c", "online", 2000},
	} {
		if _, err := s.UpsertPeer(ctx, &PeerRecord{
			DeviceID: p.id, Name: p.id, PublicKey: "pk-" + p.id,
			LastSeen: p.seen, Status: p.status,
		}); err != nil {
			t.Fatalf("seed %s: %v", p.id, err)
		}
	}
	all, err := s.ListPeers(ctx, ListPeersOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("count: %d", len(all))
	}
	if all[0].DeviceID != "dev-a" || all[1].DeviceID != "dev-c" || all[2].DeviceID != "dev-b" {
		t.Errorf("not ordered by last_seen DESC: %+v", all)
	}
	online, _ := s.ListPeers(ctx, ListPeersOptions{Status: "online"})
	if len(online) != 2 {
		t.Errorf("status filter: %d", len(online))
	}
	if _, err := s.ListPeers(ctx, ListPeersOptions{Status: "weird"}); err == nil {
		t.Error("invalid status filter accepted")
	}
}

func TestTouchPeer(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedPeer(t, s, "dev-1", "alice")
	if err := s.TouchPeer(ctx, "dev-1", "", 9999); err != nil {
		t.Fatalf("touch: %v", err)
	}
	got, _ := s.GetPeer(ctx, "dev-1")
	if got.LastSeen != 9999 {
		t.Errorf("last_seen not updated: %d", got.LastSeen)
	}
	if got.Status != "online" {
		t.Errorf("status changed unexpectedly: %s", got.Status)
	}
	if err := s.TouchPeer(ctx, "dev-1", "degraded", 10000); err != nil {
		t.Fatalf("touch + status: %v", err)
	}
	got, _ = s.GetPeer(ctx, "dev-1")
	if got.Status != "degraded" {
		t.Errorf("status: %s", got.Status)
	}
	if err := s.TouchPeer(ctx, "ghost", "", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing peer: got %v want ErrNotFound", err)
	}
}

func TestDeletePeerIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedPeer(t, s, "dev-1", "alice")
	if err := s.DeletePeer(ctx, "dev-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeletePeer(ctx, "dev-1"); err != nil {
		t.Fatalf("repeat delete: %v", err)
	}
	if _, err := s.GetPeer(ctx, "dev-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete: %v", err)
	}
}

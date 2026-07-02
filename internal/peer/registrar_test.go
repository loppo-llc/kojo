package peer

import (
	"context"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
)

func openRegistrarTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{ConfigDir: t.TempDir()})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestRegistrarStartStampsNodeKey confirms Start writes the self-row's
// node_key when SetSelfNodeKey has already run (identity columns match
// what RefreshPublicName would write). It also confirms the empty-at-
// start case leaves node_key blank rather than erroring.
func TestRegistrarStartStampsNodeKey(t *testing.T) {
	ctx := context.Background()

	t.Run("nodekey known at start", func(t *testing.T) {
		st := openRegistrarTestStore(t)
		id := &Identity{DeviceID: "11111111-1111-4111-8111-111111111111", Name: "alpha"}
		r := NewRegistrar(st, id, nil)
		r.SetSelfNodeKey("nodekey:abc123")
		if err := r.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		r.Stop()
		rec, err := st.GetPeer(ctx, id.DeviceID)
		if err != nil {
			t.Fatalf("GetPeer: %v", err)
		}
		if rec.NodeKey != "nodekey:abc123" {
			t.Fatalf("node_key = %q, want nodekey:abc123", rec.NodeKey)
		}
	})

	t.Run("nodekey empty at start", func(t *testing.T) {
		st := openRegistrarTestStore(t)
		id := &Identity{DeviceID: "22222222-2222-4222-8222-222222222222", Name: "bravo"}
		r := NewRegistrar(st, id, nil)
		if err := r.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		r.Stop()
		rec, err := st.GetPeer(ctx, id.DeviceID)
		if err != nil {
			t.Fatalf("GetPeer: %v", err)
		}
		if rec.NodeKey != "" {
			t.Fatalf("node_key = %q, want empty", rec.NodeKey)
		}
	})
}

// TestRegistrarReseedStampsNodeKey drives the tickOnce reseed path: the
// self-row is gone (operator delete / cross-targeted DELETE), so the
// heartbeat reseeds it. The reseed must carry the NodeKey so the row
// reappears admissible on the privileged surface instead of quarantined
// with a NULL node_key until the next RefreshPublicName.
func TestRegistrarReseedStampsNodeKey(t *testing.T) {
	ctx := context.Background()
	st := openRegistrarTestStore(t)
	id := &Identity{DeviceID: "33333333-3333-4333-8333-333333333333", Name: "charlie"}
	r := NewRegistrar(st, id, nil)
	r.SetSelfNodeKey("nodekey:reseed99")

	// No prior Start: the row does not exist, so tickOnce's TouchPeer
	// hits ErrNotFound and takes the reseed branch.
	r.tickOnce()

	rec, err := st.GetPeer(ctx, id.DeviceID)
	if err != nil {
		t.Fatalf("GetPeer after reseed: %v", err)
	}
	if rec.NodeKey != "nodekey:reseed99" {
		t.Fatalf("reseed node_key = %q, want nodekey:reseed99", rec.NodeKey)
	}
	if rec.Status != store.PeerStatusOnline {
		t.Fatalf("reseed status = %q, want online", rec.Status)
	}
}

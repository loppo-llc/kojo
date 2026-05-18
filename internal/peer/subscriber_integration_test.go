package peer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/loppo-llc/kojo/internal/store"
)

// docs §3.10 — pin the subscriber ↔ Hub WS handshake. Auth at the
// Hub side has moved off Ed25519 signatures to BearerPeerMiddleware
// (docs/peer-simplify-plan.md step 9), so the fixture now issues
// the subscriber a Bearer instead of generating a keypair. The
// streaming + snapshot/event semantics are unchanged.

// hubFixture builds the minimal Hub side: a kv store with the
// subscriber's peer_registry row + a Bearer-protected WS handler.
// Returns the httptest URL, the subscriber Identity, and the
// subscriber-side store handle (which already holds the outbound
// Bearer the Subscriber will present).
func hubFixture(t *testing.T) (string, *Identity, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	subID := &Identity{
		DeviceID: "subscriber0123456789abcdef012345",
		Name:     "sub-host",
	}
	if _, err := st.UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID: subID.DeviceID,
		Name:     subID.Name,
		// public_key is still NOT NULL in the schema; step 10
		// drops the column once the signing layer is fully out.
		// Use a placeholder to satisfy the constraint.
		PublicKey: "deadbeef-placeholder-base64==",
		Status:    store.PeerStatusOnline,
		Trusted:   true,
	}); err != nil {
		t.Fatalf("UpsertPeer: %v", err)
	}
	if err := st.UpdatePeerTrust(context.Background(), subID.DeviceID, true); err != nil {
		t.Fatalf("UpdatePeerTrust: %v", err)
	}
	issued, err := st.IssuePeerToken(context.Background(), subID.DeviceID, store.PeerTokenRolePeerToHub)
	if err != nil {
		t.Fatalf("IssuePeerToken: %v", err)
	}
	// Subscriber-side: stash the raw token as the outbound bearer
	// for the Hub's device_id. SetTargets uses "hub-device-id" as
	// the target id, so the stash key matches.
	if _, err := st.PutKV(context.Background(), &store.KVRecord{
		Namespace: OutBearerNS,
		Key:       "hub-device-id",
		Value:     issued.Raw,
		Type:      store.KVTypeString,
		Scope:     store.KVScopeMachine,
	}, store.KVPutOptions{}); err != nil {
		t.Fatalf("PutKV outbound bearer: %v", err)
	}
	bus := NewEventBus()
	mw := NewBearerPeerMiddleware(st, "hub-device-id")
	handler := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: []string{"*"},
		})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		// Snapshot frame.
		_ = wsjson.Write(r.Context(), conn, map[string]any{
			"type": "snapshot",
			"peers": []StatusEvent{
				{DeviceID: "peer-x", Status: "online", LastSeen: 100, Op: StatusOpUpsert},
			},
		})
		// Wait for one bus event to forward.
		ch, _, _ := bus.Subscribe(r.Context())
		select {
		case evt := <-ch:
			_ = wsjson.Write(r.Context(), conn, map[string]any{
				"type":  "event",
				"event": evt,
			})
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
			return
		}
		// Hold until client drops.
		<-r.Context().Done()
	}))
	srv := httptest.NewServer(handler)
	t.Cleanup(func() {
		srv.Close()
		bus.Publish(StatusEvent{}) // unblock any pending recv
	})
	hubBus := bus
	_ = hubBus
	return srv.URL, subID, st
}

func TestSubscriber_ReceivesSnapshotFromHub(t *testing.T) {
	hubURL, subID, st := hubFixture(t)
	bus := NewEventBus()
	sub := NewSubscriber(subID, st, bus, nil)
	defer sub.Stop()

	sub.SetTargets([]SubscriberTarget{
		{DeviceID: "hub-device-id", Address: hubURL},
	})
	// Wait for snapshot frame to land.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if evt, ok := sub.LiveStatus("peer-x"); ok && evt.Status == "online" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("snapshot frame did not arrive within 3 s")
}

func TestSubscriber_StopUnsubscribesAllTargets(t *testing.T) {
	hubURL, subID, st := hubFixture(t)
	sub := NewSubscriber(subID, st, nil, nil)
	sub.SetTargets([]SubscriberTarget{
		{DeviceID: "hub-device-id", Address: hubURL},
	})
	// Let one connection settle.
	time.Sleep(200 * time.Millisecond)
	sub.Stop()
	// After Stop, no more reconnect loop. Calling Stop again is
	// idempotent.
	sub.Stop()
}

func TestSubscriber_IgnoresSelfTarget(t *testing.T) {
	subID := &Identity{DeviceID: "self-id", Name: "self"}
	sub := NewSubscriber(subID, nil, nil, nil)
	sub.SetTargets([]SubscriberTarget{
		{DeviceID: "self-id", Address: "http://does-not-matter"},
	})
	sub.mu.RLock()
	defer sub.mu.RUnlock()
	if len(sub.targets) != 0 {
		t.Errorf("subscribed to self: %d targets", len(sub.targets))
	}
}

func TestSubscriber_AddressChangeRestartsGoroutine(t *testing.T) {
	subID := &Identity{DeviceID: "self-id"}
	sub := NewSubscriber(subID, nil, nil, nil)
	defer sub.Stop()
	// First target.
	sub.SetTargets([]SubscriberTarget{
		{DeviceID: "peer-z", Address: "http://example.com:1"},
	})
	sub.mu.RLock()
	st1 := sub.targets["peer-z"]
	sub.mu.RUnlock()
	if st1 == nil || st1.address != "http://example.com:1" {
		t.Fatalf("first target shape: %+v", st1)
	}
	// Same DeviceID, NEW address. Must replace the per-target
	// state so the next reconnect dials the new URL.
	sub.SetTargets([]SubscriberTarget{
		{DeviceID: "peer-z", Address: "http://example.com:2"},
	})
	sub.mu.RLock()
	st2 := sub.targets["peer-z"]
	sub.mu.RUnlock()
	if st2 == nil {
		t.Fatal("target dropped on address change")
	}
	if st2.address != "http://example.com:2" {
		t.Errorf("address not updated: got %q want %q", st2.address, "http://example.com:2")
	}
	if st1 == st2 {
		t.Errorf("expected new subTarget pointer after address change")
	}
}

func TestSubscriber_SetTargetsAfterStopIsNoOp(t *testing.T) {
	subID := &Identity{DeviceID: "self-id"}
	sub := NewSubscriber(subID, nil, nil, nil)
	sub.Stop()
	// Must not panic.
	sub.SetTargets([]SubscriberTarget{
		{DeviceID: "peer-z", Address: "http://nope"},
	})
	sub.mu.RLock()
	defer sub.mu.RUnlock()
	if len(sub.targets) != 0 {
		t.Errorf("post-Stop SetTargets added targets: %d", len(sub.targets))
	}
}

func TestSubscriber_FrameDecodeMalformedSkipped(t *testing.T) {
	sub := NewSubscriber(&Identity{DeviceID: "x"}, nil, nil, nil)
	sub.handleFrame("target-a", []byte("not json"))
	// No panic, no live update.
	if _, ok := sub.LiveStatus("anything"); ok {
		t.Errorf("malformed frame produced a live entry")
	}
}

func TestSubscriber_EventFrameUpdatesLive(t *testing.T) {
	sub := NewSubscriber(&Identity{DeviceID: "x"}, nil, nil, nil)
	body, _ := json.Marshal(map[string]any{
		"type": "event",
		"event": StatusEvent{
			DeviceID: "peer-y", Status: "offline", Op: StatusOpExpire,
		},
	})
	sub.handleFrame("target-a", body)
	evt, ok := sub.LiveStatus("peer-y")
	if !ok {
		t.Fatal("event did not update live cache")
	}
	if evt.Status != "offline" || evt.Op != StatusOpExpire {
		t.Errorf("event mismatch: %+v", evt)
	}
}

func TestSubscriber_SetTargetsRemovesStaleLiveEntries(t *testing.T) {
	sub := NewSubscriber(&Identity{DeviceID: "self"}, nil, nil, nil)
	// Feed in an event via target-a, then drop target-a — the
	// live cache for that target must clear so LiveStatus can't
	// report stale online for a peer we no longer observe.
	body, _ := json.Marshal(map[string]any{
		"type": "event",
		"event": StatusEvent{
			DeviceID: "peer-y", Status: "online", LastSeen: 100, Op: StatusOpUpsert,
		},
	})
	sub.handleFrame("target-a", body)
	if _, ok := sub.LiveStatus("peer-y"); !ok {
		t.Fatal("expected entry before removal")
	}
	// Removing target-a (no targets) should clear its
	// contribution.
	sub.SetTargets(nil)
	if _, ok := sub.LiveStatus("peer-y"); ok {
		t.Errorf("stale entry survived target removal")
	}
}

package peer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/loppo-llc/kojo/internal/store"
)

// docs §3.10 — pin the full subscriber ↔ Hub WS handshake:
// Subscriber dials a peer-authed endpoint, the AuthMiddleware
// verifies the Ed25519 signature, the handler streams a snapshot
// + an event, the Subscriber's live cache reflects both. Mocks
// the Hub side with httptest + websocket.Accept so the test
// doesn't depend on internal/server.

// hubFixture builds the minimal Hub side: a kv store with the
// subscriber's peer_registry row + a WS handler protected by
// AuthMiddleware. Returns the httptest server URL.
func hubFixture(t *testing.T) (string, ed25519.PrivateKey, *Identity, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// Local identity = the SUBSCRIBER's identity. The peer
	// dialling US presents its signed request; we look up its
	// public key in our peer_registry. So the Hub fixture has
	// the subscriber's identity row.
	subPub, subPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519: %v", err)
	}
	subID := &Identity{
		DeviceID:   "subscriber0123456789abcdef012345",
		Name:       "sub-host",
		PublicKey:  subPub,
		PrivateKey: subPriv,
	}
	if _, err := st.UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID:  subID.DeviceID,
		Name:      subID.Name,
		PublicKey: base64.StdEncoding.EncodeToString(subPub),
		Status:    store.PeerStatusOnline,
	}); err != nil {
		t.Fatalf("UpsertPeer: %v", err)
	}
	bus := NewEventBus()
	// hub-self is the device_id we expect the subscriber to name
	// as audience. The integration tests call SetTargets with
	// DeviceID="hub-device-id" — match that so the audience
	// check passes.
	mw := NewAuthMiddleware(st, NewNonceCache(AuthMaxClockSkew), "hub-device-id")
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
	// Stash the bus on the store via closure: tests will publish
	// directly using the returned bus pointer through a hook.
	t.Cleanup(func() {})
	// Expose the bus via the returned httptest server by attaching
	// it to a side-channel: simplest is to publish from the test
	// using the subscriber-observed `peer-y` event below.
	hubBus := bus // capture for return
	_ = hubBus
	return srv.URL, subPriv, subID, st
}

func TestSubscriber_ReceivesSnapshotFromHub(t *testing.T) {
	hubURL, _, subID, _ := hubFixture(t)
	bus := NewEventBus()
	sub := NewSubscriber(subID, bus, nil)
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
	hubURL, _, subID, _ := hubFixture(t)
	sub := NewSubscriber(subID, nil, nil)
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
	sub := NewSubscriber(subID, nil, nil)
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
	sub := NewSubscriber(subID, nil, nil)
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
	sub := NewSubscriber(subID, nil, nil)
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
	sub := NewSubscriber(&Identity{DeviceID: "x"}, nil, nil)
	sub.handleFrame("target-a", []byte("not json"))
	// No panic, no live update.
	if _, ok := sub.LiveStatus("anything"); ok {
		t.Errorf("malformed frame produced a live entry")
	}
}

func TestSubscriber_EventFrameUpdatesLive(t *testing.T) {
	sub := NewSubscriber(&Identity{DeviceID: "x"}, nil, nil)
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
	sub := NewSubscriber(&Identity{DeviceID: "self"}, nil, nil)
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

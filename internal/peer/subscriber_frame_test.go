package peer

import (
	"encoding/json"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
)

// TestSubscriberHandleFrameDelete verifies a StatusOpDelete event drops
// the row from the target's live cache instead of parking a stale entry
// that would otherwise linger until the next full snapshot.
func TestSubscriberHandleFrameDelete(t *testing.T) {
	s := &Subscriber{live: make(map[string]map[string]StatusEvent)}
	const target = "hub-device"
	const gone = "peer-b"

	eventFrame := func(evt StatusEvent) []byte {
		b, err := json.Marshal(struct {
			Type  string      `json:"type"`
			Event StatusEvent `json:"event"`
		}{Type: "event", Event: evt})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}

	// Seed an online entry via a normal upsert event.
	s.handleFrame(target, eventFrame(StatusEvent{
		DeviceID: gone, Status: store.PeerStatusOnline, LastSeen: 100, Op: StatusOpUpsert,
	}))
	if _, ok := s.LiveStatus(gone); !ok {
		t.Fatalf("expected %s present after upsert event", gone)
	}

	// A delete event must remove it.
	s.handleFrame(target, eventFrame(StatusEvent{
		DeviceID: gone, Status: store.PeerStatusOffline, LastSeen: 200, Op: StatusOpDelete,
	}))
	if evt, ok := s.LiveStatus(gone); ok {
		t.Fatalf("expected %s dropped after delete event, still present: %+v", gone, evt)
	}
}

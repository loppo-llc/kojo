package peer

import (
	"context"
	"errors"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
)

type fakeLookupStore struct {
	rows map[string]*store.PeerRecord
}

func (f *fakeLookupStore) GetPeer(_ context.Context, id string) (*store.PeerRecord, error) {
	rec, ok := f.rows[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return rec, nil
}

func (f *fakeLookupStore) ListPeers(_ context.Context, _ store.ListPeersOptions) ([]*store.PeerRecord, error) {
	out := make([]*store.PeerRecord, 0, len(f.rows))
	for _, r := range f.rows {
		out = append(out, r)
	}
	return out, nil
}

func TestResolvePeerTarget_AcceptsUUID(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	st := &fakeLookupStore{rows: map[string]*store.PeerRecord{
		id: {DeviceID: id, Name: "bravo:8080"},
	}}
	got, err := ResolvePeerTarget(context.Background(), st, id)
	if err != nil {
		t.Fatalf("UUID lookup: %v", err)
	}
	if got.DeviceID != id {
		t.Errorf("got %s, want %s", got.DeviceID, id)
	}
}

func TestResolvePeerTarget_AcceptsShortHostname(t *testing.T) {
	id := "22222222-2222-4222-8222-222222222222"
	st := &fakeLookupStore{rows: map[string]*store.PeerRecord{
		id: {DeviceID: id, Name: "http://bravo:8080"},
	}}
	got, err := ResolvePeerTarget(context.Background(), st, "bravo")
	if err != nil {
		t.Fatalf("hostname lookup: %v", err)
	}
	if got.DeviceID != id {
		t.Errorf("got %s, want %s", got.DeviceID, id)
	}
}

func TestResolvePeerTarget_AcceptsFQDN(t *testing.T) {
	id := "33333333-3333-4333-8333-333333333333"
	st := &fakeLookupStore{rows: map[string]*store.PeerRecord{
		id: {DeviceID: id, Name: "bravo.tailnet.ts.net:8080"},
	}}
	// Full FQDN match.
	got, err := ResolvePeerTarget(context.Background(), st, "bravo.tailnet.ts.net")
	if err != nil {
		t.Fatalf("FQDN lookup: %v", err)
	}
	if got.DeviceID != id {
		t.Errorf("got %s, want %s", got.DeviceID, id)
	}
	// Leftmost-label fallback.
	got, err = ResolvePeerTarget(context.Background(), st, "bravo")
	if err != nil {
		t.Fatalf("leftmost label lookup: %v", err)
	}
	if got.DeviceID != id {
		t.Errorf("got %s, want %s", got.DeviceID, id)
	}
}

func TestResolvePeerTarget_NotFound(t *testing.T) {
	st := &fakeLookupStore{rows: map[string]*store.PeerRecord{}}
	_, err := ResolvePeerTarget(context.Background(), st, "ghost")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResolvePeerTarget_Ambiguous(t *testing.T) {
	// Two rows whose leftmost labels both match "bravo" —
	// resolution must refuse rather than guess.
	st := &fakeLookupStore{rows: map[string]*store.PeerRecord{
		"44444444-4444-4444-8444-444444444444": {
			DeviceID: "44444444-4444-4444-8444-444444444444",
			Name:     "bravo.east.ts.net:8080",
		},
		"55555555-5555-4555-8555-555555555555": {
			DeviceID: "55555555-5555-4555-8555-555555555555",
			Name:     "bravo.west.ts.net:8080",
		},
	}}
	_, err := ResolvePeerTarget(context.Background(), st, "bravo")
	if !errors.Is(err, ErrAmbiguousPeerName) {
		t.Fatalf("err = %v, want ErrAmbiguousPeerName", err)
	}
}

func TestNameMatches(t *testing.T) {
	cases := []struct {
		rowName, input string
		want           bool
	}{
		{"bravo:8080", "bravo", true},
		{"http://bravo:8080", "bravo", true},
		{"https://bravo.tailnet.ts.net:8443", "bravo", true},
		{"https://bravo.tailnet.ts.net:8443", "bravo.tailnet.ts.net", true},
		{"bravo.tailnet.ts.net", "bravo", true},
		{"bravo.tailnet.ts.net", "tailnet", false}, // only leftmost label
		{"bravo:8080", "alpha", false},
		{"100.64.0.42:8080", "100.64.0.42", true},
		{"100.64.0.42:8080", "100", false}, // no DNS labels on bare v4
		{"BRAVO:8080", "bravo", true},      // case-insensitive
		{"bravo:8080", "BRAVO", true},
		{"", "bravo", false},
		{"bravo:8080", "", false},
		// Input side is normalised through HostFromRegistryName,
		// so a caller passing the same shape as the registry
		// stores still resolves.
		{"bravo:8080", "http://bravo:8080", true},
		{"bravo:8080", "https://bravo:9090", true}, // port on input ignored
		{"http://bravo.tailnet.ts.net", "https://bravo.tailnet.ts.net:1/junk", true},
		// Unbracketed IPv6 literal: HostFromRegistryName leaves
		// it as the bare address; NameMatches treats it as an IP
		// literal so only exact match succeeds.
		{"::1", "::1", true},
		{"::1", "1", false},
		// Bracketed IPv6 in registry, bare on input — normalised
		// equal.
		{"[::1]:8080", "::1", true},
		{"[::1]:8080", "[::1]", true},
	}
	for _, c := range cases {
		got := NameMatches(c.rowName, c.input)
		if got != c.want {
			t.Errorf("NameMatches(%q, %q) = %v, want %v", c.rowName, c.input, got, c.want)
		}
	}
}

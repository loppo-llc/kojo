package peer

import (
	"context"
	"errors"
	"fmt"

	"github.com/loppo-llc/kojo/internal/store"
)

// ErrAmbiguousPeerName is returned by ResolvePeerTarget when an
// operator-supplied name matches more than one peer_registry row.
// The caller surfaces this with a 400 listing the candidate
// device_ids so the operator can disambiguate.
var ErrAmbiguousPeerName = errors.New("peer: ambiguous name; multiple peer_registry rows matched")

// PeerLookupStore is the minimal store contract ResolvePeerTarget
// needs. *store.Store satisfies it; tests can pass a fake.
type PeerLookupStore interface {
	GetPeer(ctx context.Context, deviceID string) (*store.PeerRecord, error)
	ListPeers(ctx context.Context, opts store.ListPeersOptions) ([]*store.PeerRecord, error)
}

// ResolvePeerTarget accepts either a canonical UUID device_id or a
// Tailscale machine name and returns the matching peer_registry
// row. UUID input goes straight to GetPeer; name input is matched
// case-insensitively against every row via NameMatches (full host
// or leftmost label).
//
// Returns store.ErrNotFound when no match exists.
// Returns ErrAmbiguousPeerName (wrapped with the candidate IDs)
// when multiple rows match — the caller MUST disambiguate by
// device_id rather than guess.
//
// Self-rows are intentionally NOT filtered here: the caller
// (orchestrator) refuses target==self with its own message and a
// 400, so blanking the row in resolution would muddy the
// diagnostic.
func ResolvePeerTarget(ctx context.Context, st PeerLookupStore, idOrName string) (*store.PeerRecord, error) {
	if st == nil {
		return nil, errors.New("peer.ResolvePeerTarget: nil store")
	}
	if idOrName == "" {
		return nil, errors.New("peer.ResolvePeerTarget: id or name required")
	}
	if err := ValidateDeviceID(idOrName); err == nil {
		return st.GetPeer(ctx, idOrName)
	}
	rows, err := st.ListPeers(ctx, store.ListPeersOptions{})
	if err != nil {
		return nil, err
	}
	var hits []*store.PeerRecord
	for _, rec := range rows {
		if NameMatches(rec.Name, idOrName) {
			hits = append(hits, rec)
		}
	}
	switch len(hits) {
	case 0:
		return nil, store.ErrNotFound
	case 1:
		return hits[0], nil
	default:
		ids := make([]string, 0, len(hits))
		for _, h := range hits {
			ids = append(ids, h.DeviceID)
		}
		return nil, fmt.Errorf("%w: candidates=%v", ErrAmbiguousPeerName, ids)
	}
}

// Package peer manages this kojo binary's peer identity in the multi-
// device storage cluster (docs/multi-device-storage.md §3.5–§3.7).
//
// At startup the binary loads (or, on first ever run, generates) a
// stable {device_id, name} pair that survives across restarts and
// binary upgrades. With docs/peer-simplify-plan.md step 9, the
// Ed25519 keypair that used to live here was retired — peer identity
// is now anchored in the Bearer tokens delivered through the
// pairing flow and in the network-layer cert (Tailscale TLS or
// operator-supplied). The pair survives only as the join-request
// payload + the operator-facing audit row.
//
// Storage (all in kv namespace="peer", scope=machine — never crosses
// a peer boundary):
//
//   - peer/device_id : string, RFC 4122 UUID v4
//   - peer/name      : string, os.Hostname() at first run
//
// Older deploys also carried peer/public_key + peer/private_key;
// migration 0010 deletes those rows so a fresh install never sees
// them and an upgrade installs cleans them out alongside the
// peer_registry column drop.
package peer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/loppo-llc/kojo/internal/store"
)

// KV namespace + key constants. Single source of truth — exported so
// the migrate / clean / debug tooling can address the rows by name
// without re-deriving them.
const (
	KVNamespace = "peer"
	KeyDeviceID = "device_id"
	KeyName     = "name"
)

// Identity is the loaded-or-generated peer identity. After
// docs/peer-simplify-plan.md step 9 it carries only the device_id +
// human-readable name; auth material lives in peer_tokens (server
// side) and in the OutBearerNS kv (client side).
type Identity struct {
	DeviceID string
	Name     string
}

// LoadOrCreate returns the persisted peer identity. On first run it
// generates a UUID v4 + the hostname-derived name and persists both
// kv rows; subsequent runs read them back.
//
// Error contract:
//   - Partial-row state (one present, the other missing) returns
//     ErrCorrupt; recovery requires either restoring kojo.db from a
//     snapshot or deleting the surviving row so LoadOrCreate can
//     mint a fresh identity. Auto-recovery is intentionally skipped
//     so a half-rotated identity surfaces visibly.
//   - kv I/O errors propagate verbatim.
func LoadOrCreate(ctx context.Context, st *store.Store) (*Identity, error) {
	if st == nil {
		return nil, errors.New("peer.LoadOrCreate: nil store")
	}

	id, idErr := getKVString(ctx, st, KeyDeviceID)
	name, nameErr := getKVString(ctx, st, KeyName)

	allMissing := errors.Is(idErr, store.ErrNotFound) &&
		errors.Is(nameErr, store.ErrNotFound)
	allPresent := idErr == nil && nameErr == nil

	switch {
	case allMissing:
		return createIdentity(ctx, st)
	case allPresent:
		return &Identity{DeviceID: id, Name: name}, nil
	default:
		for _, p := range []struct {
			label string
			err   error
		}{
			{KeyDeviceID, idErr},
			{KeyName, nameErr},
		} {
			if p.err != nil && !errors.Is(p.err, store.ErrNotFound) {
				return nil, fmt.Errorf("peer.LoadOrCreate: kv read %s/%s: %w",
					KVNamespace, p.label, p.err)
			}
		}
		return nil, fmt.Errorf("peer.LoadOrCreate: %w (partial rows present, refusing to auto-regenerate)", ErrCorrupt)
	}
}

// ErrCorrupt is returned when peer/* kv rows are in a mixed state
// (some present, some missing) — see LoadOrCreate's contract.
var ErrCorrupt = errors.New("peer: corrupt identity (mixed kv row presence)")

func createIdentity(ctx context.Context, st *store.Store) (*Identity, error) {
	deviceID := uuid.NewString()
	name, _ := os.Hostname()
	if name == "" {
		name = "kojo-local"
	}
	// Best-effort sequential inserts. The IfMatchAny=* sentinel
	// asserts row MUST NOT exist — losing the race against a
	// concurrent first-run callbacks the loser into the load
	// path next time. A partial commit here recovers via the
	// ErrCorrupt branch on the next LoadOrCreate.
	if err := putKVString(ctx, st, KeyDeviceID, deviceID); err != nil {
		return nil, err
	}
	if err := putKVString(ctx, st, KeyName, name); err != nil {
		return nil, err
	}
	return &Identity{DeviceID: deviceID, Name: name}, nil
}

// kvOpTimeout bounds individual kv reads / writes. SQLite is local;
// the realistic stall is fsync. 10 s mirrors the VAPID kv path.
const kvOpTimeout = 10 * time.Second

func getKVString(ctx context.Context, st *store.Store, key string) (string, error) {
	c, cancel := context.WithTimeout(ctx, kvOpTimeout)
	defer cancel()
	rec, err := st.GetKV(c, KVNamespace, key)
	if err != nil {
		return "", err
	}
	return rec.Value, nil
}

func putKVString(ctx context.Context, st *store.Store, key, value string) error {
	c, cancel := context.WithTimeout(ctx, kvOpTimeout)
	defer cancel()
	_, err := st.PutKV(c, &store.KVRecord{
		Namespace: KVNamespace,
		Key:       key,
		Value:     value,
		Type:      store.KVTypeString,
		Scope:     store.KVScopeMachine,
	}, store.KVPutOptions{IfMatchETag: store.IfMatchAny})
	return err
}

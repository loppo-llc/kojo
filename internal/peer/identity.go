// Package peer manages this kojo binary's peer identity in the multi-
// device storage cluster (docs/multi-device-storage.md §3.5–§3.7).
//
// At startup the binary loads (or, on first ever run, generates) a
// stable {device_id, Ed25519 keypair} that survives across restarts
// and binary upgrades. The identity backs:
//
//   - peer_registry rows: every peer in the cluster has a row
//     keyed by device_id with name + public_key + last_seen + status.
//   - blob_refs.home_peer: every blob the local binary publishes is
//     stamped with device_id (replacing the placeholder os.Hostname()
//     fallback used pre-Phase G).
//   - inter-peer auth (future): the public key advertises this peer's
//     long-lived identity; private key signs whatever cross-peer
//     handshake the cluster eventually adopts. Not used yet — slice 1
//     of Phase G focuses on registry + heartbeat.
//
// Storage (all in kv namespace="peer", scope=machine — never crosses
// a peer boundary):
//
//   - peer/device_id     : string,  RFC 4122 UUID v4
//   - peer/name          : string,  os.Hostname() at first run
//   - peer/public_key    : string,  base64 raw Ed25519 (32 bytes)
//   - peer/private_key   : binary,  envelope-sealed (KEK + AAD)
//
// AAD = "peer/private_key" so a copy-paste of one row's ciphertext
// into another row's slot fails authentication. Pattern mirrors the
// VAPID private-key handling in internal/notify (Phase 5 slice 10).
package peer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/loppo-llc/kojo/internal/store"
	"github.com/loppo-llc/kojo/internal/store/secretcrypto"
)

// KV namespace + key constants. Single source of truth — exported so
// the migrate / clean / debug tooling can address the rows by name
// without re-deriving them.
const (
	KVNamespace  = "peer"
	KeyDeviceID  = "device_id"
	KeyName      = "name"
	KeyPublicKey = "public_key"
	KeyPrivKey   = "private_key"
)

// privateKeyAAD returns a fresh AAD slice on each call so a consumer
// that mutates the result can't leak the change into the package-
// level constant. Same defensive pattern as notify.VAPIDPrivateAAD.
func privateKeyAAD() []byte { return []byte(KVNamespace + "/" + KeyPrivKey) }

// Identity is the loaded-or-generated peer identity. PublicKey /
// PrivateKey are the raw Ed25519 byte slices (not PEM-wrapped). The
// caller MUST treat PrivateKey as secret — it should not be logged,
// returned over HTTP, or written to disk outside the kv envelope.
type Identity struct {
	DeviceID   string
	Name       string
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
}

// PublicKeyBase64 returns the public key in the canonical wire form
// (raw bytes → base64 standard encoding) used by peer_registry rows
// and the future cross-peer handshake.
func (i Identity) PublicKeyBase64() string {
	return base64.StdEncoding.EncodeToString(i.PublicKey)
}

// LoadOrCreate returns the persisted peer identity. On first run it
// generates a UUID v4, an Ed25519 keypair, and the hostname-derived
// name; persists all four kv rows in a single best-effort burst; and
// returns the new Identity. On subsequent runs every row is read back
// from kv and the keypair is decrypted with kek.
//
// Error contract:
//   - kek must be exactly secretcrypto.KEKSize bytes; otherwise
//     ErrInvalidKEK.
//   - Partial-row state (e.g. device_id present but private_key
//     missing) is treated as corruption and returned as ErrCorrupt;
//     recovery requires either restoring kojo.db from a snapshot or
//     manually deleting the surviving rows so LoadOrCreate can mint
//     a fresh identity. Auto-recovery is intentionally NOT done
//     because a partial state usually means the prior identity was
//     compromised mid-rotation, and silently regenerating would
//     burn whatever cluster trust the old identity carried.
//   - kv I/O errors propagate verbatim.
//
// The function is idempotent across goroutines via SQLite's per-row
// transactional Put/Get; concurrent first-run callers race on the
// IfMatchAny=* sentinel and the loser falls through to the load
// path.
func LoadOrCreate(ctx context.Context, st *store.Store, kek []byte) (*Identity, error) {
	if st == nil {
		return nil, errors.New("peer.LoadOrCreate: nil store")
	}
	if len(kek) != secretcrypto.KEKSize {
		return nil, fmt.Errorf("peer.LoadOrCreate: %w", ErrInvalidKEK)
	}

	id, idErr := getKVString(ctx, st, KeyDeviceID)
	pubB64, pubErr := getKVString(ctx, st, KeyPublicKey)
	name, nameErr := getKVString(ctx, st, KeyName)
	privCT, privErr := getKVBinary(ctx, st, KeyPrivKey)

	allMissing := errors.Is(idErr, store.ErrNotFound) &&
		errors.Is(pubErr, store.ErrNotFound) &&
		errors.Is(nameErr, store.ErrNotFound) &&
		errors.Is(privErr, store.ErrNotFound)
	allPresent := idErr == nil && pubErr == nil && nameErr == nil && privErr == nil

	switch {
	case allMissing:
		return createIdentity(ctx, st, kek)
	case allPresent:
		return loadIdentity(id, name, pubB64, privCT, kek)
	default:
		// Mixed state — surface whichever real (non-NotFound) error
		// we got first so the operator sees the actual failure
		// rather than a generic "corrupt" message. If every error
		// IS NotFound except one row, that row's lonely presence is
		// itself the corruption signal.
		for _, p := range []struct {
			label string
			err   error
		}{
			{KeyDeviceID, idErr},
			{KeyName, nameErr},
			{KeyPublicKey, pubErr},
			{KeyPrivKey, privErr},
		} {
			if p.err != nil && !errors.Is(p.err, store.ErrNotFound) {
				return nil, fmt.Errorf("peer.LoadOrCreate: kv read %s/%s: %w",
					KVNamespace, p.label, p.err)
			}
		}
		return nil, fmt.Errorf("peer.LoadOrCreate: %w (partial rows present, refusing to auto-regenerate)", ErrCorrupt)
	}
}

// ErrInvalidKEK is returned when the supplied KEK is the wrong size.
var ErrInvalidKEK = errors.New("peer: invalid KEK size")

// ErrCorrupt is returned when peer/* kv rows are in a mixed state
// (some present, some missing) — see LoadOrCreate's contract.
var ErrCorrupt = errors.New("peer: corrupt identity (mixed kv row presence)")

func createIdentity(ctx context.Context, st *store.Store, kek []byte) (*Identity, error) {
	deviceID := uuid.NewString()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("peer.LoadOrCreate: ed25519 gen: %w", err)
	}
	name, _ := os.Hostname()
	if name == "" {
		name = "kojo-local"
	}

	sealed, err := secretcrypto.Seal(kek, priv, privateKeyAAD())
	if err != nil {
		return nil, fmt.Errorf("peer.LoadOrCreate: seal private key: %w", err)
	}

	// Insert all four rows. The IfMatchAny=* sentinel asserts row
	// MUST NOT exist — losing the race against a concurrent
	// first-run drops us into the load path on the next call. We
	// run putKVString / putKVBinary sequentially rather than
	// transactionally because the kv layer doesn't expose a
	// multi-row atomic API, and a partial commit here is recoverable
	// from the next LoadOrCreate which sees the partial state and
	// surfaces ErrCorrupt rather than fixing it silently.
	rows := []func() error{
		func() error { return putKVString(ctx, st, KeyDeviceID, deviceID) },
		func() error { return putKVString(ctx, st, KeyName, name) },
		func() error {
			return putKVString(ctx, st, KeyPublicKey,
				base64.StdEncoding.EncodeToString(pub))
		},
		func() error { return putKVBinary(ctx, st, KeyPrivKey, sealed) },
	}
	for _, fn := range rows {
		if err := fn(); err != nil {
			return nil, err
		}
	}
	return &Identity{
		DeviceID:   deviceID,
		Name:       name,
		PublicKey:  pub,
		PrivateKey: priv,
	}, nil
}

func loadIdentity(deviceID, name, pubB64 string, privCT, kek []byte) (*Identity, error) {
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil {
		return nil, fmt.Errorf("peer.LoadOrCreate: decode public key: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("peer.LoadOrCreate: public key wrong size: got %d want %d",
			len(pub), ed25519.PublicKeySize)
	}
	priv, err := secretcrypto.Open(kek, privCT, privateKeyAAD())
	if err != nil {
		return nil, fmt.Errorf("peer.LoadOrCreate: open private key: %w", err)
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("peer.LoadOrCreate: private key wrong size: got %d want %d",
			len(priv), ed25519.PrivateKeySize)
	}
	return &Identity{
		DeviceID:   deviceID,
		Name:       name,
		PublicKey:  ed25519.PublicKey(pub),
		PrivateKey: ed25519.PrivateKey(priv),
	}, nil
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

func getKVBinary(ctx context.Context, st *store.Store, key string) ([]byte, error) {
	c, cancel := context.WithTimeout(ctx, kvOpTimeout)
	defer cancel()
	rec, err := st.GetKV(c, KVNamespace, key)
	if err != nil {
		return nil, err
	}
	return rec.ValueEncrypted, nil
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

func putKVBinary(ctx context.Context, st *store.Store, key string, value []byte) error {
	c, cancel := context.WithTimeout(ctx, kvOpTimeout)
	defer cancel()
	_, err := st.PutKV(c, &store.KVRecord{
		Namespace:      KVNamespace,
		Key:            key,
		ValueEncrypted: value,
		Type:           store.KVTypeBinary,
		Secret:         true,
		Scope:          store.KVScopeMachine,
	}, store.KVPutOptions{IfMatchETag: store.IfMatchAny})
	return err
}

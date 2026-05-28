package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/notify"
	"github.com/loppo-llc/kojo/internal/store"
	"github.com/loppo-llc/kojo/internal/store/secretcrypto"
)

// buildVAPIDStore wires a kv-backed VAPID store on top of the agent
// manager's *store.Store and a KEK loaded from <configDir>/auth/kek.bin.
// Returns nil (and logs a warning) on any setup failure — the caller
// then falls back to the legacy file-only notify.Manager. Booting
// without VAPID encryption is acceptable in v1; refusing to boot would
// be a regression for installs where the auth dir is read-only or KEK
// setup fails for an environmental reason.
func buildVAPIDStore(agentMgr *agent.Manager, configDir string, logger *slog.Logger) notify.VAPIDStore {
	if agentMgr == nil {
		return nil
	}
	st := agentMgr.Store()
	if st == nil {
		logger.Warn("VAPID kv store unavailable: no agent.Manager.Store()")
		return nil
	}
	authDir := filepath.Join(configDir, "auth")
	kek, err := secretcrypto.LoadOrCreateKEK(authDir)
	if err != nil {
		logger.Warn("VAPID kv store disabled: KEK setup failed", "err", err)
		return nil
	}
	vs, err := newVAPIDKVStore(st, kek)
	if err != nil {
		logger.Warn("VAPID kv store disabled", "err", err)
		return nil
	}
	return vs
}

// vapidKVStore implements notify.VAPIDStore on top of the kv table.
//
// Layout:
//   - notify/vapid_public  — non-secret string (publishable to clients)
//   - notify/vapid_private — secret blob, envelope-encrypted with the
//     KEK; AAD = "notify/vapid_private" so a copy/paste of one row's
//     ciphertext into another row's slot fails authentication.
//
// The KEK is loaded once at construction; rotating it is out of scope
// for v1 (operators must rewrap every secret row before swapping
// kek.bin, and we have no migration tooling for that yet).
type vapidKVStore struct {
	st  *store.Store
	kek []byte
}

// kv layout aliases — single source of truth in internal/notify.
// Keeping the runtime and the importer pinned to the same constants
// lets the compiler catch drift on the namespace / key / AAD trio.
const (
	vapidKVNamespace  = notify.KVNamespace
	vapidKVPublicKey  = notify.KVKeyVAPIDPublic
	vapidKVPrivateKey = notify.KVKeyVAPIDPrivate
)

// vapidKVOpTimeout bounds each PutKV / GetKV. The kv layer is local
// SQLite so the only realistic stall is fsync; 10 s is a generous cap.
const vapidKVOpTimeout = 10 * time.Second

// vapidPrivateAAD returns a fresh []byte clone of the canonical AAD on
// each call. Wrapping notify.VAPIDPrivateAAD() keeps the constant
// immutable across packages — a consumer that mutates the slice
// returned here cannot leak the change into the package-level wire
// constant.
func vapidPrivateAAD() []byte { return notify.VAPIDPrivateAAD() }

// newVAPIDKVStore returns a notify.VAPIDStore backed by the kv table.
// Returns an error if the KEK is missing or unreadable — the caller
// (main.go) should fall back to the legacy file-only Manager rather
// than booting silently with no encryption.
func newVAPIDKVStore(st *store.Store, kek []byte) (notify.VAPIDStore, error) {
	if st == nil {
		return nil, errors.New("vapidKVStore: nil store")
	}
	if len(kek) != secretcrypto.KEKSize {
		return nil, fmt.Errorf("vapidKVStore: KEK must be %d bytes, got %d", secretcrypto.KEKSize, len(kek))
	}
	return &vapidKVStore{st: st, kek: kek}, nil
}

// LoadVAPID returns ("","") with nil error if no row exists yet — the
// notify.Manager treats that as "fall through to file or generate".
// Any decryption failure surfaces as an error rather than empty so a
// corrupted secret row doesn't silently regenerate keys (which would
// invalidate every existing browser subscription).
func (s *vapidKVStore) LoadVAPID() (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), vapidKVOpTimeout)
	defer cancel()

	pubRec, err := s.st.GetKV(ctx, vapidKVNamespace, vapidKVPublicKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("vapidKVStore.Load public: %w", err)
	}
	privRec, err := s.st.GetKV(ctx, vapidKVNamespace, vapidKVPrivateKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Half-installed state: public present but private missing.
			// Refuse to silently regenerate; the operator should
			// investigate (likely a partial restore from backup).
			return "", "", errors.New("vapidKVStore: public present but private missing")
		}
		return "", "", fmt.Errorf("vapidKVStore.Load private: %w", err)
	}

	if !privRec.Secret || len(privRec.ValueEncrypted) == 0 {
		return "", "", errors.New("vapidKVStore: private row not encrypted (db hand-edited?)")
	}

	priv, err := secretcrypto.Open(s.kek, privRec.ValueEncrypted, vapidPrivateAAD())
	if err != nil {
		return "", "", fmt.Errorf("vapidKVStore.Open private: %w", err)
	}
	return string(priv), pubRec.Value, nil
}

// SaveVAPID writes both rows. On failure mid-way the public row may
// land without the private row; LoadVAPID detects that as a
// half-installed state and refuses to proceed, which is the desired
// behavior — better to surface the inconsistency than to regenerate.
func (s *vapidKVStore) SaveVAPID(priv, pub string) error {
	if priv == "" || pub == "" {
		return errors.New("vapidKVStore.Save: empty key")
	}

	sealed, err := secretcrypto.Seal(s.kek, []byte(priv), vapidPrivateAAD())
	if err != nil {
		return fmt.Errorf("vapidKVStore.Seal: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), vapidKVOpTimeout)
	defer cancel()

	if _, err := s.st.PutKV(ctx, &store.KVRecord{
		Namespace: vapidKVNamespace, Key: vapidKVPublicKey,
		Value: pub, Type: store.KVTypeString, Scope: store.KVScopeGlobal,
	}, store.KVPutOptions{}); err != nil {
		return fmt.Errorf("vapidKVStore.Put public: %w", err)
	}
	if _, err := s.st.PutKV(ctx, &store.KVRecord{
		Namespace: vapidKVNamespace, Key: vapidKVPrivateKey,
		ValueEncrypted: sealed, Type: store.KVTypeBinary, Scope: store.KVScopeMachine, Secret: true,
	}, store.KVPutOptions{}); err != nil {
		return fmt.Errorf("vapidKVStore.Put private: %w", err)
	}
	return nil
}

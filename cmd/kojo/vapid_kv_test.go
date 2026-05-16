package main

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
	"github.com/loppo-llc/kojo/internal/store/secretcrypto"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(context.Background(), store.Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustKEK(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, secretcrypto.KEKSize)
	if _, err := io.ReadFull(rand.Reader, k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

func TestVAPIDKVRoundTrip(t *testing.T) {
	st := openTestStore(t)
	kek := mustKEK(t)
	vs, err := newVAPIDKVStore(st, kek)
	if err != nil {
		t.Fatalf("newVAPIDKVStore: %v", err)
	}

	// Empty initially.
	priv, pub, err := vs.LoadVAPID()
	if err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	if priv != "" || pub != "" {
		t.Errorf("empty store should return ('','',nil), got (%q,%q)", priv, pub)
	}

	if err := vs.SaveVAPID("private-bytes", "public-bytes"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	priv2, pub2, err := vs.LoadVAPID()
	if err != nil {
		t.Fatalf("Load post-save: %v", err)
	}
	if priv2 != "private-bytes" || pub2 != "public-bytes" {
		t.Errorf("got (%q, %q)", priv2, pub2)
	}
}

func TestVAPIDKVRefusesHalfInstalled(t *testing.T) {
	st := openTestStore(t)
	kek := mustKEK(t)
	vs, err := newVAPIDKVStore(st, kek)
	if err != nil {
		t.Fatalf("newVAPIDKVStore: %v", err)
	}

	// Drop only the public row.
	ctx := context.Background()
	if _, err := st.PutKV(ctx, &store.KVRecord{
		Namespace: vapidKVNamespace, Key: vapidKVPublicKey,
		Value: "pub", Type: store.KVTypeString, Scope: store.KVScopeGlobal,
	}, store.KVPutOptions{}); err != nil {
		t.Fatalf("Put public: %v", err)
	}

	if _, _, err := vs.LoadVAPID(); err == nil {
		t.Error("Load with half-installed state accepted")
	}
}

func TestVAPIDKVDifferentKEKFails(t *testing.T) {
	st := openTestStore(t)
	kek1 := mustKEK(t)
	kek2 := mustKEK(t)

	vs1, _ := newVAPIDKVStore(st, kek1)
	if err := vs1.SaveVAPID("priv", "pub"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	vs2, _ := newVAPIDKVStore(st, kek2)
	if _, _, err := vs2.LoadVAPID(); err == nil {
		t.Error("Load with wrong KEK accepted")
	}
}

func TestVAPIDKVRequiresProperKEK(t *testing.T) {
	st := openTestStore(t)
	if _, err := newVAPIDKVStore(st, []byte("short")); err == nil {
		t.Error("short KEK accepted")
	}
	if _, err := newVAPIDKVStore(nil, mustKEK(t)); err == nil {
		t.Error("nil store accepted")
	}
}

func TestVAPIDKVTamperedCiphertextErrors(t *testing.T) {
	st := openTestStore(t)
	kek := mustKEK(t)
	vs, _ := newVAPIDKVStore(st, kek)
	if err := vs.SaveVAPID("priv", "pub"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Corrupt the encrypted private row directly.
	ctx := context.Background()
	rec, err := st.GetKV(ctx, vapidKVNamespace, vapidKVPrivateKey)
	if err != nil {
		t.Fatalf("GetKV: %v", err)
	}
	rec.ValueEncrypted[len(rec.ValueEncrypted)-1] ^= 0x01
	if _, err := st.PutKV(ctx, rec, store.KVPutOptions{IfMatchETag: rec.ETag}); err != nil {
		t.Fatalf("PutKV tamper: %v", err)
	}
	if _, _, err := vs.LoadVAPID(); err == nil {
		t.Error("tampered ciphertext accepted")
	}
}

func TestBuildVAPIDStoreFallbackOnMissingDir(t *testing.T) {
	// LoadOrCreateKEK creates the dir, so a fresh path should succeed.
	tmp := t.TempDir()
	authDir := filepath.Join(tmp, "auth")
	if _, err := secretcrypto.LoadOrCreateKEK(authDir); err != nil {
		t.Fatalf("LoadOrCreateKEK: %v", err)
	}
	if _, err := os.Stat(filepath.Join(authDir, "kek.bin")); err != nil {
		t.Errorf("KEK file missing: %v", err)
	}
}

// _ "use" sentinel keeps the errors import alive on platforms where the
// half-installed test path is the only consumer.
var _ = errors.New

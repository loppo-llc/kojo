package secretcrypto

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func mustKEK(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, KEKSize)
	if _, err := io.ReadFull(rand.Reader, k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	kek := mustKEK(t)
	pt := []byte("vapid-private-key-bytes")
	aad := []byte("notify/vapid")

	sealed, err := Seal(kek, pt, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	got, err := Open(kek, sealed, aad)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Errorf("plaintext mismatch")
	}
}

func TestOpenRejectsWrongKEK(t *testing.T) {
	kek := mustKEK(t)
	other := mustKEK(t)
	sealed, err := Seal(kek, []byte("data"), []byte("aad"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := Open(other, sealed, []byte("aad")); err == nil {
		t.Error("Open with wrong KEK accepted")
	}
}

func TestOpenRejectsWrongAAD(t *testing.T) {
	kek := mustKEK(t)
	sealed, err := Seal(kek, []byte("data"), []byte("ns/k1"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Same KEK, different AAD must fail — proves AAD is bound.
	if _, err := Open(kek, sealed, []byte("ns/k2")); err == nil {
		t.Error("Open accepted ciphertext with mismatched AAD")
	}
}

func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	kek := mustKEK(t)
	sealed, err := Seal(kek, []byte("data"), nil)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	sealed[len(sealed)-1] ^= 0x01
	if _, err := Open(kek, sealed, nil); err == nil {
		t.Error("Open accepted tampered ciphertext")
	}
}

func TestOpenRejectsTruncated(t *testing.T) {
	kek := mustKEK(t)
	sealed, err := Seal(kek, []byte("data"), nil)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	short := sealed[:5]
	_, err = Open(kek, short, nil)
	if !errors.Is(err, ErrFormat) {
		t.Errorf("err = %v, want ErrFormat", err)
	}
}

func TestOpenRejectsBadVersion(t *testing.T) {
	kek := mustKEK(t)
	sealed, err := Seal(kek, []byte("data"), nil)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	sealed[0] = 99
	_, err = Open(kek, sealed, nil)
	if !errors.Is(err, ErrFormat) {
		t.Errorf("err = %v, want ErrFormat", err)
	}
}

func TestSealRejectsBadKEK(t *testing.T) {
	if _, err := Seal(make([]byte, 16), []byte("x"), nil); err == nil {
		t.Error("Seal accepted 16-byte KEK")
	}
	if _, err := Open(make([]byte, 16), []byte("x"), nil); err == nil {
		t.Error("Open accepted 16-byte KEK")
	}
}

func TestLoadOrCreateKEKCreatesNew(t *testing.T) {
	dir := t.TempDir()
	k, err := LoadOrCreateKEK(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateKEK: %v", err)
	}
	if len(k) != KEKSize {
		t.Errorf("len = %d, want %d", len(k), KEKSize)
	}
	// File must exist with mode 0600.
	info, err := os.Stat(filepath.Join(dir, KEKFileName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("mode = %#o, want 0600", mode)
		}
	}
	// Second call returns the same bytes.
	k2, err := LoadOrCreateKEK(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateKEK 2nd: %v", err)
	}
	if !bytes.Equal(k, k2) {
		t.Error("KEK changed across loads")
	}
}

func TestLoadOrCreateKEKRefusesLoosePerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perm check is unix-only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, KEKFileName)
	// Write a fresh-looking KEK with 0644.
	if err := os.WriteFile(path, make([]byte, KEKSize), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := LoadOrCreateKEK(dir); err == nil {
		t.Error("loose perms accepted")
	}
}

func TestLoadOrCreateKEKMissingDirCreates(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "auth")
	if _, err := LoadOrCreateKEK(dir); err != nil {
		t.Fatalf("LoadOrCreateKEK: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, KEKFileName)); err != nil {
		t.Errorf("KEK file missing: %v", err)
	}
}

func TestLoadOrCreateKEKRejectsCorruptLength(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, KEKFileName)
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := LoadOrCreateKEK(dir); err == nil {
		t.Error("corrupt-length KEK accepted")
	}
}

// fs sentinel used in test imports to keep io/fs alive on platforms
// where the perm test is skipped.
var _ = fs.ErrPermission

// Package secretcrypto implements the envelope encryption used by the
// kv table's secret rows and (future) by the snapshot path described
// in docs/multi-device-storage.md §3.5 / §3.6.
//
// Threat model:
//
//   - The KEK (key encryption key) is a 32-byte random value held only
//     on the Hub peer's filesystem (mode 0600, kojo's auth dir). Loss
//     of the KEK means loss of every secret row — there is no hosted
//     KMS in v1. Backup procedure is "include the KEK file in your
//     out-of-band Hub backup, separately from the SQLite snapshot".
//   - Per-row DEKs (data encryption keys) are random AES-256 keys
//     generated on every Encrypt and themselves AES-GCM-sealed by the
//     KEK. Snapshots can include the DEK ciphertext blob without
//     leaking secrets — restoring requires re-pairing the KEK.
//   - AAD (additional-authenticated data) on the inner GCM seal is the
//     row's namespace+key, so a copy/paste of one row's ciphertext
//     onto a different row's slot fails decryption rather than
//     silently aliasing the value.
//
// Wire format of the value_encrypted column:
//
//	[1B version=1] [12B kek_nonce] [N B kek_ciphertext_with_tag]
//	             [12B dek_nonce] [M B dek_ciphertext_with_tag]
//
// The KEK ciphertext seals the DEK (32 B + 16 B tag = 48 B). The DEK
// ciphertext seals the plaintext.
package secretcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// KEKSize is the required KEK length in bytes (AES-256).
const KEKSize = 32

// dekSize is the per-record DEK length (AES-256).
const dekSize = 32

// gcmNonceSize is the standard GCM nonce length (96 bits).
const gcmNonceSize = 12

// envelopeVersion is the leading byte of a sealed blob — bumped when
// the format changes so old blobs decode under their original rules.
const envelopeVersion byte = 1

// kekCiphertextLen is the length of dek+tag once sealed by the KEK
// (32 B DEK + 16 B GCM tag).
const kekCiphertextLen = dekSize + 16

// ErrFormat is returned when a sealed blob fails structural decoding
// (truncated, wrong version byte, etc). Distinct from a decryption
// error so callers can tell "your bytes don't even look like an
// envelope" from "your KEK is wrong".
var ErrFormat = errors.New("secretcrypto: bad envelope format")

// Seal encrypts plaintext under a fresh DEK and seals the DEK with
// the KEK. AAD is mixed into the inner GCM seal so a row's ciphertext
// cannot be copy-pasted onto another row's slot. Pass the row's
// namespace+key as AAD for kv use.
func Seal(kek, plaintext, aad []byte) ([]byte, error) {
	if len(kek) != KEKSize {
		return nil, fmt.Errorf("secretcrypto.Seal: KEK must be %d bytes, got %d", KEKSize, len(kek))
	}
	dek := make([]byte, dekSize)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, fmt.Errorf("secretcrypto.Seal: dek random: %w", err)
	}

	// Seal DEK with KEK.
	kekGCM, err := newGCM(kek)
	if err != nil {
		return nil, err
	}
	kekNonce := make([]byte, gcmNonceSize)
	if _, err := io.ReadFull(rand.Reader, kekNonce); err != nil {
		return nil, fmt.Errorf("secretcrypto.Seal: kek nonce: %w", err)
	}
	kekCT := kekGCM.Seal(nil, kekNonce, dek, nil)

	// Seal plaintext with DEK + AAD.
	dekGCM, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	dekNonce := make([]byte, gcmNonceSize)
	if _, err := io.ReadFull(rand.Reader, dekNonce); err != nil {
		return nil, fmt.Errorf("secretcrypto.Seal: dek nonce: %w", err)
	}
	dataCT := dekGCM.Seal(nil, dekNonce, plaintext, aad)

	// Layout: ver | kekNonce | kekCT | dekNonce | dataCT
	out := make([]byte, 0, 1+gcmNonceSize+len(kekCT)+gcmNonceSize+len(dataCT))
	out = append(out, envelopeVersion)
	out = append(out, kekNonce...)
	out = append(out, kekCT...)
	out = append(out, dekNonce...)
	out = append(out, dataCT...)
	return out, nil
}

// Open reverses Seal. Returns ErrFormat for structural decode errors,
// any other error from the AEAD layer for authentication failure
// (wrong KEK, tampered ciphertext, mismatched AAD).
func Open(kek, sealed, aad []byte) ([]byte, error) {
	if len(kek) != KEKSize {
		return nil, fmt.Errorf("secretcrypto.Open: KEK must be %d bytes, got %d", KEKSize, len(kek))
	}
	if len(sealed) < 1+gcmNonceSize+kekCiphertextLen+gcmNonceSize {
		return nil, ErrFormat
	}
	if sealed[0] != envelopeVersion {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrFormat, sealed[0])
	}
	off := 1
	kekNonce := sealed[off : off+gcmNonceSize]
	off += gcmNonceSize
	kekCT := sealed[off : off+kekCiphertextLen]
	off += kekCiphertextLen
	dekNonce := sealed[off : off+gcmNonceSize]
	off += gcmNonceSize
	dataCT := sealed[off:]

	kekGCM, err := newGCM(kek)
	if err != nil {
		return nil, err
	}
	dek, err := kekGCM.Open(nil, kekNonce, kekCT, nil)
	if err != nil {
		return nil, fmt.Errorf("secretcrypto.Open: kek decrypt: %w", err)
	}

	dekGCM, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	plaintext, err := dekGCM.Open(nil, dekNonce, dataCT, aad)
	if err != nil {
		return nil, fmt.Errorf("secretcrypto.Open: data decrypt: %w", err)
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretcrypto: aes new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretcrypto: gcm new: %w", err)
	}
	return gcm, nil
}

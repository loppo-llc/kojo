package peer

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

// PeerNameMaxBytes is the upper bound on peer_registry.name. 255
// is the typical hostname limit; the FQDN + port form
// `<host>.<tailnet>.ts.net:8080` fits comfortably.
const PeerNameMaxBytes = 255

// ValidateDeviceID enforces the canonical 8-4-4-4-12 lowercase
// UUID form. uuid.Parse on its own is too permissive (accepts
// URN form, braced form, raw bytes, uppercase) — letting any of
// those through would let the same logical UUID land twice in
// peer_registry under different keys and bypass self-detection
// by submitting an alternate spelling of the local device id.
// Empty input is a distinct error so callers can produce clearer
// messages ("required" vs "invalid format").
//
// Shared by the HTTP handler (internal/server/peer_handlers.go)
// and the CLI (cmd/kojo/peer_cmd.go) so the same shape gate
// applies regardless of entry point.
func ValidateDeviceID(id string) error {
	if id == "" {
		return errors.New("deviceId is required")
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return errors.New("deviceId must be a UUID")
	}
	if parsed.String() != id {
		return errors.New("deviceId must be canonical lowercase 8-4-4-4-12 UUID form")
	}
	return nil
}

// ValidateName checks the human-readable peer name. Trimmed
// length > 0 and ≤ PeerNameMaxBytes; all Unicode control
// characters rejected so a UI rendering the value can't be
// tricked into ANSI escape / null / DEL / TAB injection.
// unicode.IsControl covers the C0 range (NUL, TAB, LF, CR,
// ESC), DEL (U+007F), and the C1 range (U+0080..U+009F).
func ValidateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("name is required")
	}
	if len(name) > PeerNameMaxBytes {
		return errors.New("name exceeds maximum length")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("name contains control characters")
		}
	}
	return nil
}

// ValidatePublicKey decodes the wire-form (strict base64-std,
// raw 32-byte Ed25519) and rejects anything else. Strict
// decoding catches embedded whitespace / non-canonical padding
// that would otherwise let the "same" public key land as two
// distinct rows. The round-trip check (re-encode == input) also
// guards against alternate-but-decoder-accepting forms.
func ValidatePublicKey(b64 string) error {
	if b64 == "" {
		return errors.New("publicKey is required")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(b64)
	if err != nil {
		return errors.New("publicKey must be base64-standard encoded (no whitespace, canonical padding)")
	}
	if len(raw) != ed25519.PublicKeySize {
		return errors.New("publicKey must be a 32-byte Ed25519 public key")
	}
	if base64.StdEncoding.EncodeToString(raw) != b64 {
		return errors.New("publicKey must be canonical base64-standard encoding")
	}
	return nil
}

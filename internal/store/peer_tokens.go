package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// peer_tokens persists the Bearer credentials that docs/peer-simplify-plan.md
// uses in place of the retired Ed25519 per-request signing scheme. Each row
// represents one active (or revoked) token; the raw token value is never
// stored — only sha256(raw) — so a DB-only leak does not yield reusable
// credentials.

// PeerTokenRecord mirrors one row of `peer_tokens`.
type PeerTokenRecord struct {
	// TokenHash is sha256(raw_token) in base64-std. PRIMARY KEY in the DB.
	TokenHash string
	// DeviceID names the peer the token authorises (peer→Hub: the calling
	// peer; Hub→peer: the receiving peer).
	DeviceID string
	// Role is `peer_to_hub` or `hub_to_peer`. CHECK-enforced in the
	// migration. The pair is asymmetric so a leaked peer→Hub token can
	// not be used to impersonate the Hub against the peer.
	Role string
	// CreatedAt is unix millis at IssuePeerToken time.
	CreatedAt int64
	// RevokedAt is unix millis when RevokePeerToken landed, or 0 when the
	// row is still active.
	RevokedAt int64
}

// Token roles. Strings match the CHECK constraint in 0009_peer_tokens.sql.
const (
	PeerTokenRoleHubToPeer = "hub_to_peer"
	PeerTokenRolePeerToHub = "peer_to_hub"
)

// peerTokenRawBytes is the entropy size of the raw token before base64.
// 32 bytes = 256 bits, satisfying the Codex review's minimum.
const peerTokenRawBytes = 32

// IssuedPeerToken bundles the raw token (one-shot, returned to caller) with
// the persisted record. The caller is responsible for shipping `Raw` to the
// receiving side and then dropping it from memory; the record stays in the
// store for verification.
type IssuedPeerToken struct {
	Raw    string
	Record *PeerTokenRecord
}

// generatePeerTokenRaw mints a fresh 256-bit token and returns its
// base64-std encoding (no padding). Used by IssuePeerToken; broken out so
// tests can stub it with a deterministic value.
func generatePeerTokenRaw() (string, error) {
	buf := make([]byte, peerTokenRawBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("store.generatePeerTokenRaw: %w", err)
	}
	return base64.RawStdEncoding.EncodeToString(buf), nil
}

// HashPeerToken returns the canonical hash form used both at issue time and
// at verification time. Exported so the peer-side stash (kv) and any
// middleware can recompute the same value without re-importing crypto.
func HashPeerToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}

// IssuePeerToken mints a fresh Bearer for (deviceID, role), inserts the
// hash, and returns the raw value + record. The raw token is the only
// chance the caller has to learn the secret; the store rejects future
// reads. Role must be one of PeerTokenRole*.
func (s *Store) IssuePeerToken(ctx context.Context, deviceID, role string) (*IssuedPeerToken, error) {
	if deviceID == "" {
		return nil, errors.New("store.IssuePeerToken: device_id required")
	}
	switch role {
	case PeerTokenRoleHubToPeer, PeerTokenRolePeerToHub:
		// ok
	default:
		return nil, fmt.Errorf("store.IssuePeerToken: invalid role %q", role)
	}
	raw, err := generatePeerTokenRaw()
	if err != nil {
		return nil, err
	}
	hash := HashPeerToken(raw)
	now := time.Now().UnixMilli()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO peer_tokens (token_hash, device_id, role, created_at) VALUES (?, ?, ?, ?)`,
		hash, deviceID, role, now)
	if err != nil {
		return nil, fmt.Errorf("store.IssuePeerToken: insert: %w", err)
	}
	return &IssuedPeerToken{
		Raw: raw,
		Record: &PeerTokenRecord{
			TokenHash: hash,
			DeviceID:  deviceID,
			Role:      role,
			CreatedAt: now,
		},
	}, nil
}

// ResolvePeerToken looks up a raw token presented in an Authorization
// header. Returns ErrNotFound when the token is unknown or revoked, so the
// caller can map both to a generic 401 without leaking which side failed.
//
// The lookup is keyed by sha256(raw); raw is never compared against any
// stored plaintext. A revoked row (revoked_at != NULL) is treated as
// missing — the row is retained for audit, but it grants no privileges.
func (s *Store) ResolvePeerToken(ctx context.Context, raw string) (*PeerTokenRecord, error) {
	if raw == "" {
		return nil, ErrNotFound
	}
	hash := HashPeerToken(raw)
	row := s.db.QueryRowContext(ctx,
		`SELECT token_hash, device_id, role, created_at, COALESCE(revoked_at, 0)
		 FROM peer_tokens WHERE token_hash = ?`, hash)
	var rec PeerTokenRecord
	if err := row.Scan(&rec.TokenHash, &rec.DeviceID, &rec.Role, &rec.CreatedAt, &rec.RevokedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store.ResolvePeerToken: %w", err)
	}
	if rec.RevokedAt != 0 {
		return nil, ErrNotFound
	}
	return &rec, nil
}

// RevokePeerToken stamps revoked_at on the row identified by sha256(raw).
// Idempotent — a second call against an already-revoked token is a no-op
// rather than an error. Missing rows return ErrNotFound so the operator
// CLI can surface "no such token" cleanly.
func (s *Store) RevokePeerToken(ctx context.Context, raw string) error {
	if raw == "" {
		return ErrNotFound
	}
	hash := HashPeerToken(raw)
	now := time.Now().UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`UPDATE peer_tokens SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL`,
		now, hash)
	if err != nil {
		return fmt.Errorf("store.RevokePeerToken: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either missing or already revoked. SELECT to distinguish so
		// callers know which case they hit.
		row := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM peer_tokens WHERE token_hash = ?`, hash)
		var dummy int
		if err := row.Scan(&dummy); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("store.RevokePeerToken: probe: %w", err)
		}
		// Row exists but was already revoked — idempotent no-op.
		return nil
	}
	return nil
}

// RevokePeerTokensByDevice stamps revoked_at on every active token bound
// to deviceID. Called from `kojo --peer-remove <device_id>` style flows so
// a decommissioned peer can't replay any token it cached. Returns the
// number of rows actually flipped (helps the CLI report "revoked N
// tokens").
func (s *Store) RevokePeerTokensByDevice(ctx context.Context, deviceID string) (int, error) {
	if deviceID == "" {
		return 0, errors.New("store.RevokePeerTokensByDevice: device_id required")
	}
	now := time.Now().UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`UPDATE peer_tokens SET revoked_at = ? WHERE device_id = ? AND revoked_at IS NULL`,
		now, deviceID)
	if err != nil {
		return 0, fmt.Errorf("store.RevokePeerTokensByDevice: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

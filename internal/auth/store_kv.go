package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// Auth tokens are now backed by the kv table per design doc §2.3
// (table row "auth/owner.token / agent_tokens/<id> → kv namespace=auth").
//
// Layout:
//
//	namespace = "auth"
//	key       = "owner.token"           — owner secret hash (singleton)
//	key       = "agent_tokens/<id>"     — per-agent secret hash
//	scope     = "global"                — owner / per-agent hashes
//	                                      replicate cross-peer so any
//	                                      Hub can verify a presented
//	                                      token (HA). The raw tokens
//	                                      themselves never reach the
//	                                      store; only the SHA-256
//	                                      verifier hash does.
//	type      = "string"
//	value     = "sha256:<64-hex>"       — same format the legacy disk
//	                                      file held, so the hash logic
//	                                      in parseTokenFile / writeHashFile
//	                                      stays algorithm-agnostic
//	secret    = false                   — the hash is NOT a credential;
//	                                      it's verifier material and
//	                                      replicating it openly is the
//	                                      whole point of the cutover.
const (
	authKVNamespace        = "auth"
	authKVOwnerKey         = "owner.token"
	authKVAgentTokenPrefix = "agent_tokens/"
	authKVTimeout          = 5 * time.Second
)

// authKVAgentTokenKey returns the kv key used for agentID's per-agent
// token row. Centralised so a typo in one call site can't quietly
// diverge from the other.
func authKVAgentTokenKey(agentID string) string {
	return authKVAgentTokenPrefix + agentID
}

// loadOwnerKV returns the owner-token hash from kv if present.
// Returns ("", store.ErrNotFound) when no row exists; surface other
// errors to the caller. Hash is the bare 64-hex form (the
// "sha256:" prefix is stripped — callers that re-emit it via
// writeHashFile need to add it back).
//
// Row-shape gate is strict: anything other than (type=string,
// scope=global, secret=false, value="sha256:<hex>") is treated as
// a corrupt row and yields an error. A malformed row replicated
// from a peer with a buggy writer would otherwise let an attacker
// forge a verifier hash via the very channel meant to detect drift.
func loadOwnerKV(kv *store.Store) (string, error) {
	if kv == nil {
		return "", store.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(context.Background(), authKVTimeout)
	defer cancel()
	rec, err := kv.GetKV(ctx, authKVNamespace, authKVOwnerKey)
	if err != nil {
		return "", err
	}
	hash, err := parseAuthKVValue(rec)
	if err != nil {
		return "", fmt.Errorf("auth: kv owner row: %w", err)
	}
	return hash, nil
}

// saveOwnerKV writes the owner hash to kv. ifMatch="" overwrites
// unconditionally (used on routine save); ifMatch=store.IfMatchAny
// asserts "row must not exist" (used during disk → kv migration so a
// concurrent peer-replicated write can't be silently clobbered).
//
// The supplied hash MUST be a 64-hex string; uppercase digits are
// canonicalized to lowercase here so the on-wire row equals what
// parseAuthKVValue accepts (legacy disk migration is the typical
// uppercase source — isHex64 admits mixed case for back-compat,
// but the kv canonical form is strictly lowercase). The "sha256:"
// prefix is added here so the on-the-wire format matches the
// legacy file format byte-for-byte (a peer running the old disk-
// based code could still read it once mirrored back, in the
// unlikely rollback scenario).
func saveOwnerKV(kv *store.Store, hash, ifMatch string) error {
	if kv == nil {
		return errors.New("auth: kv store not configured")
	}
	if !isHex64(hash) {
		return fmt.Errorf("auth: refusing to write malformed hash to kv")
	}
	hash = strings.ToLower(hash)
	ctx, cancel := context.WithTimeout(context.Background(), authKVTimeout)
	defer cancel()
	rec := &store.KVRecord{
		Namespace: authKVNamespace,
		Key:       authKVOwnerKey,
		Value:     hashedTokenPrefix + hash,
		Type:      store.KVTypeString,
		Scope:     store.KVScopeGlobal,
	}
	if _, err := kv.PutKV(ctx, rec, store.KVPutOptions{IfMatchETag: ifMatch}); err != nil {
		return fmt.Errorf("auth: kv put owner: %w", err)
	}
	return nil
}

// loadAgentTokensKV returns a map of agentID → bare-hex hash for all
// per-agent rows currently in kv. Rows whose key doesn't match the
// "agent_tokens/<validID>" pattern, or whose value fails the row-
// shape gate, are skipped (with the same fail-quiet posture the
// disk-based loop has — a corrupt entry shouldn't take down the
// whole store).
func loadAgentTokensKV(kv *store.Store) (map[string]string, error) {
	if kv == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), authKVTimeout)
	defer cancel()
	rows, err := kv.ListKV(ctx, authKVNamespace)
	if err != nil {
		return nil, fmt.Errorf("auth: kv list agent_tokens: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		id, ok := strings.CutPrefix(r.Key, authKVAgentTokenPrefix)
		if !ok {
			continue
		}
		if validateAgentID(id) != nil {
			continue
		}
		hash, err := parseAuthKVValue(r)
		if err != nil {
			continue
		}
		out[id] = hash
	}
	return out, nil
}

// saveAgentTokenKV writes the per-agent token hash to kv. See
// saveOwnerKV for the ifMatch + lowercase-canonicalization
// semantics; this function takes the agent id and re-derives the
// kv key to keep the namespace prefix in one place.
func saveAgentTokenKV(kv *store.Store, agentID, hash, ifMatch string) error {
	if kv == nil {
		return errors.New("auth: kv store not configured")
	}
	if err := validateAgentID(agentID); err != nil {
		return err
	}
	if !isHex64(hash) {
		return fmt.Errorf("auth: refusing to write malformed hash to kv")
	}
	hash = strings.ToLower(hash)
	ctx, cancel := context.WithTimeout(context.Background(), authKVTimeout)
	defer cancel()
	rec := &store.KVRecord{
		Namespace: authKVNamespace,
		Key:       authKVAgentTokenKey(agentID),
		Value:     hashedTokenPrefix + hash,
		Type:      store.KVTypeString,
		Scope:     store.KVScopeGlobal,
	}
	if _, err := kv.PutKV(ctx, rec, store.KVPutOptions{IfMatchETag: ifMatch}); err != nil {
		return fmt.Errorf("auth: kv put agent_token: %w", err)
	}
	return nil
}

// deleteAgentTokenKV removes the per-agent row. ErrNotFound is
// folded into success — the post-condition is "row absent", which
// is already true. Other errors propagate so the caller can decide
// whether to retry.
func deleteAgentTokenKV(kv *store.Store, agentID string) error {
	if kv == nil {
		return nil
	}
	if err := validateAgentID(agentID); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), authKVTimeout)
	defer cancel()
	if err := kv.DeleteKV(ctx, authKVNamespace, authKVAgentTokenKey(agentID), ""); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("auth: kv delete agent_token: %w", err)
	}
	return nil
}

// parseAuthKVValue validates the row shape and extracts the bare
// 64-hex hash from rec.Value. The expected format mirrors the
// legacy on-disk format BUT with strict equality — no trailing
// whitespace / newline tolerance:
//
//	value  = "sha256:<64-hex-LOWERCASE>"
//	type   = "string"
//	scope  = "global"
//	secret = false
//
// Strict matching matters because the value IS the verifier
// material; loose parsing would let a peer-replicated row with
// "sha256:<hex>\n" or "  sha256:<HEX>  " pass the gate even
// though it doesn't match a value we ourselves would have
// written. A row with any of the wrong shape is rejected as
// corrupt rather than silently coerced — see loadOwnerKV's
// comment for why this matters for verifier integrity.
func parseAuthKVValue(rec *store.KVRecord) (string, error) {
	if rec == nil {
		return "", errors.New("nil record")
	}
	if rec.Type != store.KVTypeString || rec.Scope != store.KVScopeGlobal || rec.Secret {
		return "", fmt.Errorf("row shape mismatch (type=%q scope=%q secret=%t)", rec.Type, rec.Scope, rec.Secret)
	}
	hash, ok := strings.CutPrefix(rec.Value, hashedTokenPrefix)
	if !ok {
		return "", fmt.Errorf("missing %q prefix", hashedTokenPrefix)
	}
	if !isHex64(hash) {
		return "", fmt.Errorf("malformed hex (len=%d)", len(hash))
	}
	// Require lowercase. isHex64 already accepts mixed case for
	// robustness when reading legacy disk files; for the kv
	// canonical form we narrow further so a "sha256:DEADBEEF..."
	// row written by a buggy peer doesn't equal-compare with our
	// hashToken output (which is always lowercase).
	for i := 0; i < len(hash); i++ {
		if c := hash[i]; c >= 'A' && c <= 'F' {
			return "", fmt.Errorf("uppercase hex in canonical kv value (idx %d)", i)
		}
	}
	return hash, nil
}

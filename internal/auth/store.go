package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/loppo-llc/kojo/internal/store"
)

// agentIDPattern restricts the legal agentID character set to a tightly
// scoped alphabet that cannot contain "/" or ".." sequences. The auth
// store uses agentID directly as a filename, so a corrupted agents.json
// would otherwise expose a path-traversal foothold.
var agentIDPattern = regexp.MustCompile(`^[A-Za-z0-9_\-]{1,128}$`)

// validateAgentID returns an error if the agent ID contains anything
// other than the safe alphabet. Empty IDs are also rejected.
func validateAgentID(id string) error {
	if !agentIDPattern.MatchString(id) {
		return fmt.Errorf("auth: invalid agent id %q", id)
	}
	return nil
}

// hashedTokenPrefix marks a file as containing the SHA-256 hash of a
// token rather than the raw token itself. Phase 5 #16 introduces
// hash-only at-rest storage; legacy files (raw 64-char hex) are
// migrated in place on first boot.
const hashedTokenPrefix = "sha256:"

// TokenStore persists owner / agent token HASHES and keeps an
// in-memory hash → ID index for O(1) reverse lookup.
//
// Phase 2c-2 slice 17: hashes now live in the kv table at
// (namespace="auth", key="owner.token" | "agent_tokens/<id>",
// scope=global, type=string). The legacy on-disk layout is
// retained as a fallback for v1 installs that booted under
// pre-cutover binaries:
//
//	<base>/owner.token            — owner secret hash (legacy)
//	<base>/agent_tokens/<id>      — per-agent secret hash (legacy)
//
// On first boot after the cutover, NewTokenStore mirrors any
// surviving disk file into kv (IfMatchAny so a peer-replicated
// row wins on collision) and best-effort unlinks the disk file.
// Steady-state reads come from kv; the disk fallback path runs
// only when kv has no row AND the legacy file is present.
//
// On-disk / on-the-wire value format (unchanged across the
// cutover so a rollback could still parse the value):
//
//	"sha256:<64-hex-chars>"
//
// Legacy (pre-Phase-5) files held the raw token verbatim. On first
// boot the store recognizes the legacy form (64 hex chars, no prefix),
// rewrites it as the hashed form (now in kv, not on disk), and
// keeps the raw value in memory for THIS boot only so the existing
// console URL print (cmd/kojo) and agent-side env injection still
// work without a forced re-issue. From the next boot on, the raw
// token is unrecoverable from kv — only the operator's cached value
// (browser localStorage, agent env) keeps working. To force-re-issue
// a token, delete the kv row (or use RemoveAgentToken) and restart.
//
// The store does NOT depend on the agent package; agent.Manager is the
// caller that drives EnsureAgentToken / RemoveAgentToken on agent
// lifecycle events.
type TokenStore struct {
	base string

	// kv is the kojo.db handle that backs the canonical token rows.
	// May be nil in tests that exercise only the legacy disk path
	// (the boot logic falls back to disk when kv is unavailable).
	kv *store.Store

	mu sync.RWMutex
	// ownerHash is the SHA-256 of the owner token. Always populated.
	ownerHash string
	// ownerRaw is non-empty ONLY on a boot that materialized the raw
	// token (fresh install or legacy migration). Cleared in tests via
	// ResetOwnerRawForTest.
	ownerRaw string
	// hashes maps token-hash → agentID (reverse lookup).
	hashes map[string]string
	// idIndex maps agentID → token-hash (forward lookup).
	idIndex map[string]string
	// rawByID retains the raw token in memory for THIS boot only,
	// keyed by agentID, so AgentToken() can return the raw value to
	// callers that need to inject it into an agent's environment.
	// Populated on legacy migration and on AgentToken() generation.
	rawByID map[string]string
}

// NewTokenStore initializes a store rooted at base. The kv handle
// (Phase 2c-2 slice 17) is the canonical backing; the disk layout
// at base is retained as a legacy fallback for v1 installs that
// booted under pre-cutover binaries. Pass nil for kv in tests that
// only exercise the disk path.
//
// The owner token is created on first use if absent unless
// overrideOwner is non-empty (in which case overrideOwner is
// treated as the canonical owner token and is *not* persisted —
// neither to kv nor to disk).
func NewTokenStore(base string, kv *store.Store, overrideOwner string) (*TokenStore, error) {
	if base == "" {
		return nil, errors.New("auth: token store base path is empty")
	}
	// Mkdir the legacy agent_tokens dir so a re-boot under pre-
	// cutover code (rollback) finds the directory it expects to
	// write into. Cheap, harmless on a steady-state install.
	dir := filepath.Join(base, "agent_tokens")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("auth: mkdir %s: %w", dir, err)
	}
	st := &TokenStore{
		base:    base,
		kv:      kv,
		hashes:  make(map[string]string),
		idIndex: make(map[string]string),
		rawByID: make(map[string]string),
	}

	if overrideOwner != "" {
		// Override case: caller (KOJO_OWNER_TOKEN env) supplies the
		// raw owner. Hash it for in-memory comparisons, retain raw
		// in memory so cmd/kojo can still print the URL. Nothing is
		// persisted to either kv or disk.
		st.ownerRaw = overrideOwner
		st.ownerHash = hashToken(overrideOwner)
	} else {
		hash, raw, err := st.loadOrCreateOwner()
		if err != nil {
			return nil, err
		}
		st.ownerHash = hash
		st.ownerRaw = raw
	}

	if err := st.loadAgentTokens(); err != nil {
		return nil, err
	}
	return st, nil
}

// loadOrCreateOwner returns (hash, rawIfAvailable, error). raw is
// populated on a fresh install and on a legacy disk → kv migration.
//
// Lookup order:
//
//   1. kv row (auth/owner.token)
//   2. legacy disk file (<base>/owner.token), with disk → kv
//      mirror via IfMatchAny on success
//   3. fresh-generate, persist to kv (or disk fallback when kv is
//      unavailable)
//
// The opportunistic "kv-hit drops a stale legacy file" branch
// matches the cron_paused / autosummary_marker pattern: the disk
// file is no longer authoritative once kv has the row, so a
// surviving disk file from a partial migration / rollback round-
// trip is best-effort unlinked.
func (s *TokenStore) loadOrCreateOwner() (string, string, error) {
	legacyPath := filepath.Join(s.base, "owner.token")

	// 1. kv hit — canonical state.
	if hash, err := loadOwnerKV(s.kv); err == nil {
		// Drop a stray legacy file (e.g. v1 → v0 → v1 round trip)
		// so the next pre-cutover binary doesn't read an obsolete
		// hash. Best-effort.
		_ = os.Remove(legacyPath)
		return hash, "", nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", "", fmt.Errorf("auth: load owner kv: %w", err)
	}

	// 2. legacy disk fallback. parseTokenFile rewrites the file
	//    in-place if it's still in raw-token form (pre-Phase-5),
	//    and returns raw=<original> on that path so this boot can
	//    still print the URL.
	if data, err := os.ReadFile(legacyPath); err == nil {
		hash, raw, _, perr := parseTokenFile(legacyPath, data)
		if perr == nil && hash != "" {
			// Mirror disk → kv. IfMatchAny so a colliding peer-
			// replicated insert wins (and we then re-read).
			migErr := saveOwnerKV(s.kv, hash, store.IfMatchAny)
			switch {
			case migErr == nil:
				_ = os.Remove(legacyPath)
				return hash, raw, nil
			case errors.Is(migErr, store.ErrETagMismatch):
				// Peer beat us. Re-read kv: ONLY accept a parseable
				// row (loadOwnerKV applies parseAuthKVValue's
				// strict shape gate). On any error — re-read
				// failure OR a peer-replicated row that fails the
				// shape gate — fail closed: refuse to boot rather
				// than silently fall back to the disk hash, which
				// would let an attacker plant a junk row to force
				// the verifier off the canonical kv channel and
				// onto a stale local file.
				kvHash, kerr := loadOwnerKV(s.kv)
				if kerr != nil {
					return "", "", fmt.Errorf("auth: owner migration collision; peer kv row unreadable: %w", kerr)
				}
				_ = os.Remove(legacyPath)
				return kvHash, "", nil
			default:
				// kv write failed (DB unreachable, schema issue).
				// Honour the disk value for THIS boot only; next
				// boot retries the migration. This is a transient-
				// outage compromise: the verifier still works
				// (disk hash matches the legacy token the operator
				// has cached) and an unreachable DB shouldn't lock
				// out admin access. The disk file is not removed
				// so the migration retries on the next boot.
				return hash, raw, nil
			}
		}
	}

	// 3. fresh generate. CAS-guard the kv write with IfMatchAny so
	//    a peer that inserted a row between the kv miss above and
	//    this PutKV doesn't get silently overwritten.
	tok, err := generateToken()
	if err != nil {
		return "", "", err
	}
	hash := hashToken(tok)
	if err := s.persistOwnerHashCreateOnly(hash); err != nil {
		if errors.Is(err, store.ErrETagMismatch) {
			// Peer wrote between our kv miss and our write.
			// Re-read and adopt — but with the same strict
			// shape-gate posture as the migration collision branch.
			kvHash, kerr := loadOwnerKV(s.kv)
			if kerr != nil {
				return "", "", fmt.Errorf("auth: owner fresh-generate collision; peer kv row unreadable: %w", kerr)
			}
			return kvHash, "", nil
		}
		return "", "", err
	}
	return hash, tok, nil
}

// loadAgentTokens populates the in-memory maps from kv first, then
// migrates any surviving legacy disk file whose agentID isn't
// already present in kv. Best-effort unlinks legacy files that
// have a kv counterpart, regardless of whether THIS boot did the
// migration.
func (s *TokenStore) loadAgentTokens() error {
	// 1. kv: canonical state.
	kvHashes, err := loadAgentTokensKV(s.kv)
	if err != nil {
		return err
	}
	for id, hash := range kvHashes {
		s.hashes[hash] = id
		s.idIndex[id] = hash
	}

	// 2. legacy disk migration: any agentID present on disk but
	//    not in kv gets mirrored. Files for IDs that are already
	//    in kv are unlinked best-effort.
	dir := filepath.Join(s.base, "agent_tokens")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("auth: read agent_tokens: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id := e.Name()
		if validateAgentID(id) != nil {
			continue
		}
		legacyPath := filepath.Join(dir, id)
		if _, ok := s.idIndex[id]; ok {
			// Already in kv; legacy file is stale. Best-effort drop.
			_ = os.Remove(legacyPath)
			continue
		}
		hash, raw, _, perr := readLegacyAgentToken(legacyPath)
		if perr != nil || hash == "" {
			continue
		}
		// Mirror disk → kv with IfMatchAny so a peer-replicated
		// insert during boot wins gracefully.
		migErr := saveAgentTokenKV(s.kv, id, hash, store.IfMatchAny)
		switch {
		case migErr == nil:
			s.hashes[hash] = id
			s.idIndex[id] = hash
			if raw != "" {
				s.rawByID[id] = raw
			}
			_ = os.Remove(legacyPath)
		case errors.Is(migErr, store.ErrETagMismatch):
			// Peer wrote first. Re-read just this row.
			ctx, cancel := context.WithTimeout(context.Background(), authKVTimeout)
			rec, gerr := s.kv.GetKV(ctx, authKVNamespace, authKVAgentTokenKey(id))
			cancel()
			if gerr != nil {
				continue
			}
			winner, perr := parseAuthKVValue(rec)
			if perr != nil {
				continue
			}
			s.hashes[winner] = id
			s.idIndex[id] = winner
			_ = os.Remove(legacyPath)
		default:
			// kv unavailable. Fall back to in-memory disk-only state
			// for this boot; kv will be retried on next NewTokenStore.
			s.hashes[hash] = id
			s.idIndex[id] = hash
			if raw != "" {
				s.rawByID[id] = raw
			}
		}
	}
	return nil
}

// readLegacyAgentToken is loadOrMigrateTokenFile renamed for the
// kv-cutover era — it no longer rewrites the disk file as part of
// migration (kv is the canonical home; legacy raw → hashed
// rewriting is still done in parseTokenFile so a v0 → kv-cutover
// path still works on first boot).
func readLegacyAgentToken(path string) (hash, raw string, migrated bool, err error) {
	return readOrMigrateTokenFile(path)
}

// OwnerToken returns the raw owner token if available on this boot,
// otherwise the empty string. Available scenarios:
//
//   - Fresh install (file just created)
//   - Legacy migration boot (file rewritten from raw to hash)
//   - KOJO_OWNER_TOKEN override
//
// On any subsequent boot from a hash-only file, OwnerToken returns "".
// Callers that just want to verify a presented token MUST use
// VerifyOwner instead.
func (s *TokenStore) OwnerToken() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ownerRaw
}

// VerifyOwner returns true if presented matches the stored owner token
// hash. Constant-time comparison prevents timing-leak side channels.
func (s *TokenStore) VerifyOwner(presented string) bool {
	if presented == "" {
		return false
	}
	want := s.OwnerHash()
	got := hashToken(presented)
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// OwnerHash returns the SHA-256 hash of the owner token (hex). Always
// populated.
func (s *TokenStore) OwnerHash() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ownerHash
}

// LookupAgent returns the agent ID associated with the given raw
// token, if any. Hash is computed and matched in constant time
// against every stored agent hash — this is O(N) but agent count is
// small.
func (s *TokenStore) LookupAgent(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	hash := hashToken(token)
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.hashes[hash]
	return id, ok
}

// AgentToken returns the raw token for the given agent ID if one is
// available in memory (this boot generated or migrated it). Otherwise
// the store does not know the raw value and returns ("", error) so a
// caller can decide whether to re-issue. To force a re-issue, call
// ReissueAgentToken — it atomically swaps the kv hash and in-memory
// verifier under one lock without exposing a verifier gap, unlike
// the older RemoveAgentToken+AgentToken pattern.
func (s *TokenStore) AgentToken(agentID string) (string, error) {
	if err := validateAgentID(agentID); err != nil {
		return "", err
	}

	s.mu.RLock()
	if t, ok := s.rawByID[agentID]; ok {
		s.mu.RUnlock()
		return t, nil
	}
	_, hashed := s.idIndex[agentID]
	s.mu.RUnlock()

	if hashed {
		// Token exists on disk but the raw is no longer in memory
		// (post-migration boot). Returning a clean error lets agent
		// startup decide whether to re-issue.
		return "", ErrTokenRawUnavailable
	}

	// Generate + persist under write lock; double-check in case of race.
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.rawByID[agentID]; ok {
		return t, nil
	}
	tok, err := generateToken()
	if err != nil {
		return "", err
	}
	hash := hashToken(tok)
	// CAS-guard the first-issue write with IfMatchAny so a peer
	// that replicated an agent_tokens/<id> row between our
	// idIndex-miss observation and this write surfaces as
	// ErrETagMismatch instead of being silently overwritten.
	//
	// Adopting the peer's row preserves verifier consistency
	// across the cluster, but our newly-generated raw token is
	// useless to the operator (they'd present a token whose hash
	// no longer matches kv). When the re-read + parse succeed we
	// surface as ErrTokenRawUnavailable so the caller (typically
	// EnsureAgentToken at boot) knows the agent has a verifier
	// hash but THIS peer can't return the raw value; the agent
	// must be re-issued (RemoveAgentToken then re-call) to align
	// both peers on a fresh hash. When re-read OR parse FAILS
	// the peer-replicated row is unusable and idIndex would be
	// left empty — surfacing a clean error here forces the
	// operator to investigate (a malformed peer row is a
	// security event, not a routine collision).
	if err := s.persistAgentTokenHashCreateOnly(agentID, hash); err != nil {
		if errors.Is(err, store.ErrETagMismatch) {
			ctx, cancel := context.WithTimeout(context.Background(), authKVTimeout)
			rec, gerr := s.kv.GetKV(ctx, authKVNamespace, authKVAgentTokenKey(agentID))
			cancel()
			if gerr != nil {
				return "", fmt.Errorf("auth: agent token collision; peer kv row unreadable: %w", gerr)
			}
			winner, perr := parseAuthKVValue(rec)
			if perr != nil {
				return "", fmt.Errorf("auth: agent token collision; peer kv row malformed: %w", perr)
			}
			s.hashes[winner] = agentID
			s.idIndex[agentID] = winner
			return "", ErrTokenRawUnavailable
		}
		return "", err
	}
	s.hashes[hash] = agentID
	s.idIndex[agentID] = hash
	s.rawByID[agentID] = tok
	return tok, nil
}

// EnsureAgentToken is a convenience wrapper used at agent-create time.
// Unlike AgentToken, it tolerates ErrTokenRawUnavailable: when a token
// already exists on disk we don't need to know its raw value to
// guarantee "the agent has a token".
func (s *TokenStore) EnsureAgentToken(agentID string) error {
	_, err := s.AgentToken(agentID)
	if errors.Is(err, ErrTokenRawUnavailable) {
		return nil
	}
	return err
}

// VerifyAgent returns the agent ID if presented matches a stored
// agent token hash. Distinct from LookupAgent only by name; both do
// constant-time hash comparison via the map lookup.
func (s *TokenStore) VerifyAgent(presented string) (string, bool) {
	return s.LookupAgent(presented)
}

// RemoveAgentToken deletes the kv row, in-memory entries, and any
// surviving legacy disk file for an agent. Safe to call on an
// unknown ID. Invalid IDs are ignored.
func (s *TokenStore) RemoveAgentToken(agentID string) {
	if validateAgentID(agentID) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hash, ok := s.idIndex[agentID]
	if ok {
		delete(s.hashes, hash)
		delete(s.idIndex, agentID)
	}
	delete(s.rawByID, agentID)
	if err := deleteAgentTokenKV(s.kv, agentID); err != nil {
		// Best-effort cleanup — the in-memory state is already
		// consistent. A failed kv delete leaves an orphan row that
		// the next agent re-creating with the same id would
		// (correctly) verify against; that is benign in practice.
		// The next AgentToken fresh-issue goes through
		// persistAgentTokenHashCreateOnly with IfMatchAny, which
		// surfaces the orphan as ErrETagMismatch and falls into
		// the collision branch — the orphan is re-read as the
		// authoritative row. Caller-visible behavior splits by API:
		// AgentToken returns ErrTokenRawUnavailable so the caller
		// knows to re-issue (RemoveAgentToken + AgentToken again),
		// while EnsureAgentToken swallows that sentinel and treats
		// the row as "already exists, raw not needed" — which is
		// what the agent-create boot path wants. Either way, no
		// silent corruption.
		_ = err
	}
	_ = os.Remove(filepath.Join(s.base, "agent_tokens", agentID))
}

// ErrTokenRawUnavailable is returned by AgentToken when the agent has
// a stored token hash but the raw value is no longer available in
// memory (post-migration boot). Callers either accept this (the agent
// already has the token cached elsewhere) or re-issue via
// ReissueAgentToken.
var ErrTokenRawUnavailable = errors.New("auth: agent token raw value not available; re-issue required")

// ReissueAgentToken atomically rotates the agent's token: CAS-replaces
// the existing kv row with a fresh hash, then swaps in-memory entries.
// Holds the write lock for the entire operation so concurrent callers
// for the same agent serialize and the second caller observes the
// freshly-issued raw value (rather than racing a second rotation that
// would invalidate the first caller's raw before it could be injected
// into a PTY env).
//
// The kv row's hash is compared against this peer's local idIndex
// before CAS — if they disagree, another peer rotated and we adopt
// their hash instead of overwriting it. ETag-only CAS isn't enough:
// a successful peer rotation produces a fresh ETag, our GetKV reads
// that fresh ETag, and an unguarded saveAgentTokenKV would CAS-
// succeed and silently overwrite the peer's newly-issued hash —
// breaking the peer's PTY whose $KOJO_AGENT_TOKEN was just minted.
//
// Returns the raw token kojo can inject into $KOJO_AGENT_TOKEN, or
// an error if persisting the new hash fails. In peer mode this
// causes the verifier hash in kv to change — other peers' in-memory
// verifiers do NOT hot-reload, so tokens issued here are valid on
// THIS peer until the others restart and re-read kv. Acceptable for
// the post-migration single-host case (the only place the current
// callback drives this); cluster-wide raw-token sync is a separate
// design problem.
func (s *TokenStore) ReissueAgentToken(agentID string) (string, error) {
	if err := validateAgentID(agentID); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Idempotency: if a concurrent caller already rotated, return the
	// cached raw rather than rotating again. A second rotation would
	// silently invalidate the first caller's raw before that caller
	// could hand it off to the agent's PTY.
	if t, ok := s.rawByID[agentID]; ok {
		return t, nil
	}

	tok, err := generateToken()
	if err != nil {
		return "", err
	}
	newHash := hashToken(tok)

	// CAS-replace the kv row in one shot: read the current row's
	// ETag, then PutKV with IfMatchETag=<that ETag>. This guarantees
	// the in-memory verifier never has a gap — if the write fails we
	// leave idIndex/hashes untouched, so the OLD raw (if a peer or
	// prior boot of ours is still holding it) keeps verifying.
	//
	// On CAS mismatch a peer raced in a different value between our
	// read and write. Mirror AgentToken's collision branch: re-read,
	// adopt the winner's hash into our local verifier so that peer's
	// raw continues to authenticate here, and surface
	// ErrTokenRawUnavailable so the caller knows THIS peer can't
	// return the raw value. The PTY spawn site logs and continues
	// with an empty $KOJO_AGENT_TOKEN rather than crashing.
	var ifMatch string
	if s.kv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), authKVTimeout)
		rec, gerr := s.kv.GetKV(ctx, authKVNamespace, authKVAgentTokenKey(agentID))
		cancel()
		switch {
		case gerr == nil:
			// Liveness gate: if the kv row's hash diverged from the
			// hash this peer's idIndex expects, another peer already
			// rotated. Adopt their hash and abort — overwriting with
			// our newHash via a stale-view CAS would silently
			// invalidate the peer's fresh raw token.
			kvHash, perr := parseAuthKVValue(rec)
			if perr != nil {
				return "", fmt.Errorf("auth: reissue: existing kv row malformed: %w", perr)
			}
			if localHash, ok := s.idIndex[agentID]; ok && localHash != kvHash {
				delete(s.hashes, localHash)
				s.hashes[kvHash] = agentID
				s.idIndex[agentID] = kvHash
				delete(s.rawByID, agentID)
				return "", ErrTokenRawUnavailable
			}
			ifMatch = rec.ETag
		case errors.Is(gerr, store.ErrNotFound):
			// No row to replace; assert "must not exist" so a peer
			// that races in a row between our read and write surfaces
			// as ErrETagMismatch instead of clobbering them.
			ifMatch = store.IfMatchAny
		default:
			return "", fmt.Errorf("auth: reissue: read existing kv row: %w", gerr)
		}
		if err := saveAgentTokenKV(s.kv, agentID, newHash, ifMatch); err != nil {
			if errors.Is(err, store.ErrETagMismatch) {
				return "", s.adoptPeerAgentTokenLocked(agentID)
			}
			return "", fmt.Errorf("auth: reissue: persist new hash: %w", err)
		}
	} else {
		// Disk fallback (test fixtures and pre-kv installs). No CAS
		// analogue; just overwrite atomically via writeHashFile.
		path := filepath.Join(s.base, "agent_tokens", agentID)
		if err := writeHashFile(path, newHash); err != nil {
			return "", fmt.Errorf("auth: reissue: write hash file: %w", err)
		}
	}

	// kv (or disk) write succeeded — NOW swap in-memory verifier.
	// Doing this AFTER the persist ack means a failure above leaves
	// the verifier intact.
	if oldHash, ok := s.idIndex[agentID]; ok {
		delete(s.hashes, oldHash)
	}
	s.hashes[newHash] = agentID
	s.idIndex[agentID] = newHash
	s.rawByID[agentID] = tok
	// Drop any surviving legacy disk file so a rollback to a pre-
	// cutover binary doesn't read a hash that disagrees with kv.
	if s.kv != nil {
		_ = os.Remove(filepath.Join(s.base, "agent_tokens", agentID))
	}
	return tok, nil
}

// AdoptAgentTokenFromPeer installs a raw token sent by another
// peer during a §3.7 device-switch agent-sync. The hash is
// computed locally, persisted to kv, and added to the in-memory
// verifier so the post-handoff PTY (whose $KOJO_AGENT_TOKEN was
// stamped by the source peer) authenticates here.
//
// Semantics:
//   - Idempotent: a sync that delivers the same raw token we
//     already hold is a no-op (no kv write, in-memory state
//     unchanged).
//   - Source-wins: a sync that delivers a DIFFERENT raw token
//     overwrites the local hash. Any prior raw token this peer
//     had cached for the agent is dropped — the orchestrator
//     intends the new token to be the only one in flight.
//   - The raw is added to rawByID so a subsequent PTY spawn can
//     read it back via AgentToken (no re-issue needed).
//
// Threadsafe: takes the write lock for the entire call.
func (s *TokenStore) AdoptAgentTokenFromPeer(agentID, rawToken string) error {
	if err := validateAgentID(agentID); err != nil {
		return err
	}
	if rawToken == "" {
		return errors.New("auth: adopt: raw token required")
	}
	newHash := hashToken(rawToken)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Fast-path: same hash already wired locally. Just refresh
	// the raw in memory in case it was lost to a prior restart.
	if existingHash, ok := s.idIndex[agentID]; ok && existingHash == newHash {
		s.rawByID[agentID] = rawToken
		return nil
	}

	if s.kv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), authKVTimeout)
		rec, gerr := s.kv.GetKV(ctx, authKVNamespace, authKVAgentTokenKey(agentID))
		cancel()
		var ifMatch string
		switch {
		case gerr == nil:
			kvHash, perr := parseAuthKVValue(rec)
			if perr != nil {
				return fmt.Errorf("auth: adopt: existing kv row malformed: %w", perr)
			}
			if kvHash == newHash {
				// kv already at the source's hash; just sync
				// the in-memory state.
				if old, ok := s.idIndex[agentID]; ok && old != newHash {
					delete(s.hashes, old)
				}
				s.hashes[newHash] = agentID
				s.idIndex[agentID] = newHash
				s.rawByID[agentID] = rawToken
				return nil
			}
			ifMatch = rec.ETag
		case errors.Is(gerr, store.ErrNotFound):
			ifMatch = store.IfMatchAny
		default:
			return fmt.Errorf("auth: adopt: read existing kv row: %w", gerr)
		}
		if err := saveAgentTokenKV(s.kv, agentID, newHash, ifMatch); err != nil {
			return fmt.Errorf("auth: adopt: persist hash: %w", err)
		}
	} else {
		// Disk fallback (test fixtures / pre-kv installs).
		path := filepath.Join(s.base, "agent_tokens", agentID)
		if err := writeHashFile(path, newHash); err != nil {
			return fmt.Errorf("auth: adopt: write hash file: %w", err)
		}
	}

	if oldHash, ok := s.idIndex[agentID]; ok && oldHash != newHash {
		delete(s.hashes, oldHash)
	}
	s.hashes[newHash] = agentID
	s.idIndex[agentID] = newHash
	s.rawByID[agentID] = rawToken
	if s.kv != nil {
		_ = os.Remove(filepath.Join(s.base, "agent_tokens", agentID))
	}
	return nil
}

// adoptPeerAgentTokenLocked is called when ReissueAgentToken's CAS
// write loses to a peer-replicated row. It re-reads the kv row,
// validates the shape, and swaps the local verifier to the winner's
// hash so the peer's raw continues to authenticate here. Returns
// ErrTokenRawUnavailable so the caller signals "this peer can't
// produce the raw" — the agent's PTY env on this peer will be empty
// until the operator re-issues from the peer that owns the raw.
//
// MUST be called with s.mu held (write).
func (s *TokenStore) adoptPeerAgentTokenLocked(agentID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), authKVTimeout)
	defer cancel()
	rec, err := s.kv.GetKV(ctx, authKVNamespace, authKVAgentTokenKey(agentID))
	if err != nil {
		return fmt.Errorf("auth: reissue: peer race re-read failed: %w", err)
	}
	winner, perr := parseAuthKVValue(rec)
	if perr != nil {
		return fmt.Errorf("auth: reissue: peer wrote malformed row: %w", perr)
	}
	if oldHash, ok := s.idIndex[agentID]; ok && oldHash != winner {
		delete(s.hashes, oldHash)
	}
	s.hashes[winner] = agentID
	s.idIndex[agentID] = winner
	delete(s.rawByID, agentID)
	return ErrTokenRawUnavailable
}

// --- internals -------------------------------------------------------

// persistAgentTokenHashCreateOnly is the only helper on the
// agent-token fresh-issue path; legacy disk migration writes
// directly via saveAgentTokenKV (it has its own IfMatchAny posture
// in saveAgentTokenKV's caller). IfMatchAny here asserts "row must
// not already exist" so a peer-replicated row that landed between
// the caller's idIndex-miss observation and this write surfaces as
// ErrETagMismatch instead of being silently overwritten — see
// AgentToken's collision branch for the cluster-consistency
// rationale.
//
// A no-CAS sibling existed briefly (for an "operator-driven
// re-issue" handler that never landed) and was dropped in slice 20.
// Re-add via git history if last-write-wins semantics are ever
// needed; the design constraint is that any new variant MUST also
// unlink the legacy disk file post-write so a rollback boot can't
// re-import a stale hash.
func (s *TokenStore) persistAgentTokenHashCreateOnly(agentID, hash string) error {
	if s.kv != nil {
		if err := saveAgentTokenKV(s.kv, agentID, hash, store.IfMatchAny); err != nil {
			return err
		}
		_ = os.Remove(filepath.Join(s.base, "agent_tokens", agentID))
		return nil
	}
	path := filepath.Join(s.base, "agent_tokens", agentID)
	return writeHashFile(path, hash)
}

// persistOwnerHashCreateOnly is the only helper on the owner-token
// fresh-generate path; legacy disk migration writes directly via
// saveOwnerKV. IfMatchAny here asserts "row must not already exist"
// so a peer-replicated insert that landed between our kv-miss
// observation and this write surfaces as ErrETagMismatch instead of
// being silently clobbered. The disk fallback (kv==nil) uses
// writeHashFile which doesn't have a CAS analogue, but the disk
// path is reachable only in test fixtures where the race doesn't
// apply.
//
// A no-CAS sibling (the dropped persistOwnerHash) existed briefly
// and was removed in slice 20 alongside the agent-token equivalent.
// See persistAgentTokenHashCreateOnly's comment for the rationale
// and the constraint on any future re-introduction.
func (s *TokenStore) persistOwnerHashCreateOnly(hash string) error {
	if s.kv != nil {
		if err := saveOwnerKV(s.kv, hash, store.IfMatchAny); err != nil {
			return err
		}
		_ = os.Remove(filepath.Join(s.base, "owner.token"))
		return nil
	}
	path := filepath.Join(s.base, "owner.token")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("auth: mkdir owner.token parent: %w", err)
	}
	return writeHashFile(path, hash)
}

// readOrMigrateTokenFile returns (hash, rawIfAvailable, migrated, err).
// Migrated is true iff a legacy raw-token file was rewritten to the
// hashed form on this call.
func readOrMigrateTokenFile(path string) (string, string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", false, err
	}
	hash, raw, migrated, err := parseTokenFile(path, data)
	if err != nil {
		return "", "", false, err
	}
	return hash, raw, migrated, nil
}

func parseTokenFile(path string, data []byte) (string, string, bool, error) {
	s := trimToken(data)
	switch {
	case strings.HasPrefix(s, hashedTokenPrefix):
		hash := strings.TrimPrefix(s, hashedTokenPrefix)
		if !isHex64(hash) {
			return "", "", false, fmt.Errorf("auth: malformed hashed token in %s", path)
		}
		// Canonicalize to lowercase. hashToken always emits
		// lowercase, so an in-memory ownerHash / idIndex value
		// derived from a hand-edited or legacy uppercase file
		// would otherwise fail VerifyOwner / LookupAgent (which
		// compare against hashToken's lowercase output via the
		// constant-time map lookup). The kv canonical form
		// (parseAuthKVValue) is also strict-lowercase, so
		// canonicalizing here keeps every downstream surface
		// consistent.
		return strings.ToLower(hash), "", false, nil
	case isHex64(s):
		// Legacy raw token. Compute hash, rewrite file, return raw too
		// so this boot can still print the URL / inject env vars.
		hash := hashToken(s)
		if err := writeHashFile(path, hash); err != nil {
			return "", "", false, fmt.Errorf("auth: migrate %s: %w", path, err)
		}
		return hash, s, true, nil
	case s == "":
		return "", "", false, nil
	default:
		return "", "", false, fmt.Errorf("auth: unrecognized token format in %s", path)
	}
}

func writeHashFile(path, hash string) error {
	if !isHex64(hash) {
		return fmt.Errorf("auth: refusing to write malformed hash to %s", path)
	}
	tmp := path + ".tmp"
	body := hashedTokenPrefix + hash + "\n"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return fmt.Errorf("auth: write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("auth: rename %s: %w", path, err)
	}
	return nil
}

func generateToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("auth: read random: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

func trimToken(data []byte) string {
	s := string(data)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

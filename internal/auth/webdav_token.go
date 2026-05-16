package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// WebDAV short-lived tokens (docs/multi-device-storage.md §3.4 / §5.6)
// give the kojo user a way to mount the WebDAV share from a client that
// can't carry the Owner browser cookie (macOS Finder, Windows Explorer,
// mobile file apps). The Owner UI mints a token with a TTL of a few
// hours and feeds the raw value to the mount client; the server stores
// only the SHA-256 hash. Verification is constant-time map lookup so
// the hot path matches the agent-token store.
//
// On-disk format (kv row):
//
//	namespace = "auth"
//	key       = "webdav_tokens/<id>"      — id is a UUIDv4 (32-hex; "-"-stripped)
//	scope     = "global"                    — replicate cross-peer so any
//	                                          Hub can verify a presented
//	                                          token (Hub failover §3.6).
//	type      = "json"
//	secret    = false                       — the hash is verifier
//	                                          material, not a credential.
//	value     = JSON {"hash":"sha256:<64hex>","label":"...","created_at":N,"expires_at":N}
//
// The id is independent of the raw token (the operator names tokens via
// `label`); only the hash identifies the token cryptographically. The
// id serves as the kv key + the revoke URL parameter.

const (
	// webdavKVKeyPrefix is the kv key prefix every WebDAV token row
	// uses. Centralised so a typo can't quietly diverge between the
	// list / put / delete paths.
	webdavKVKeyPrefix = "webdav_tokens/"

	// webdavTokenLabelMaxLen caps the operator-supplied label so a
	// malicious or buggy UI can't write multi-MB rows. The label is
	// purely informational — 200 chars is plenty for "iPad Finder
	// mount, 2026-05-11".
	webdavTokenLabelMaxLen = 200

	// webdavMinTTL is the floor on the TTL the issuer can request.
	// Tokens that expire within seconds of creation are almost
	// certainly a config mistake; surface them as an explicit error
	// rather than letting the operator generate a token they can't
	// even hand over before it dies.
	webdavMinTTL = 5 * time.Minute

	// webdavMaxTTL is the ceiling. §3.4 frames the design intent as
	// "TTL 数時間" (TTL several hours); the hardest cap we enforce
	// is 24 h so even an "always-on mount" workflow has to re-issue
	// the credential daily, bounding the blast radius of a leaked
	// token to one operational day. The operator can re-issue with
	// the same label anytime to keep a mount alive past the
	// expiry.
	webdavMaxTTL = 24 * time.Hour
)

// webdavIDPattern bounds the URL parameter shape so a hand-crafted
// DELETE /api/v1/auth/webdav-tokens/<id> can't smuggle path traversal
// or kv key separators into the kv row lookup. Accepts the 32-hex form
// `newID` emits AND a longer alphanumeric form for any future
// migration that re-keys rows. The kv row-shape gate
// (parseWebDAVKVValue) narrows further to "matches newID's exact
// shape" so a peer-replicated row with a hand-crafted id can't slip
// past the verifier.
var webdavIDPattern = regexp.MustCompile(`^[A-Za-z0-9_\-]{8,128}$`)

// webdavCanonicalIDPattern is the strict form newID emits: 32 hex
// lowercase. parseWebDAVKVValue uses this to reject malformed
// peer-replicated rows; the URL gate uses the looser webdavIDPattern
// so a future id-shape migration doesn't require a coordinated client
// release.
var webdavCanonicalIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

// WebDAVTokenStore is the in-memory + kv-backed verifier for short-lived
// WebDAV tokens. Concurrent verification is lock-free on the read path
// (sync.RWMutex with RLock); issuance / revocation / sweep take the
// write lock briefly to mutate the maps.
type WebDAVTokenStore struct {
	kv     *store.Store
	now    func() time.Time // injectable for tests; defaults to time.Now
	random func([]byte) (int, error)

	mu sync.RWMutex
	// hashes maps verifier-hash → id. O(1) lookup keyed by hashToken
	// (which the verifier computes from the presented raw token).
	hashes map[string]string
	// rows holds the cached metadata for each live token. Verify
	// re-reads rows[id].ExpiresAt under RLock so an expired token is
	// rejected even before the sweeper deletes the row.
	rows map[string]webdavTokenRow
}

// webdavTokenRow mirrors the kv JSON value plus the id for in-memory
// lookups. Hash is the bare hex (no "sha256:" prefix) so the verifier
// can compare against hashToken() output directly.
type webdavTokenRow struct {
	ID        string `json:"-"`
	Hash      string `json:"hash"` // "sha256:<64hex>" on the wire
	Label     string `json:"label"`
	CreatedAt int64  `json:"created_at"` // unix millis
	ExpiresAt int64  `json:"expires_at"` // unix millis; required, > 0
}

// WebDAVTokenMeta is the read-only view a list endpoint returns. The
// raw token is NEVER persisted, so a list can only show metadata. The
// operator is expected to remember the raw value at issue time (the UI
// shows it once).
type WebDAVTokenMeta struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
}

// IssueResult bundles the one-time raw value + the persistent metadata
// returned by Issue. The raw value is the only side of the credential
// the operator ever sees; subsequent reads (List / Verify) operate on
// the hash.
type IssueResult struct {
	ID        string
	Token     string // raw — return to operator once, never persisted
	Label     string
	CreatedAt int64
	ExpiresAt int64
}

// NewWebDAVTokenStore loads every existing row from kv into memory and
// returns a ready-to-verify store. Rows that have already expired by
// the time we read them are pruned in-place (best-effort; if the kv
// delete fails the entry is dropped from memory anyway so the verifier
// stays correct, and the sweeper will retry).
func NewWebDAVTokenStore(ctx context.Context, kv *store.Store) (*WebDAVTokenStore, error) {
	if kv == nil {
		return nil, errors.New("auth: webdav token store requires kv handle")
	}
	s := &WebDAVTokenStore{
		kv:     kv,
		now:    time.Now,
		random: rand.Read,
		hashes: make(map[string]string),
		rows:   make(map[string]webdavTokenRow),
	}
	if err := s.reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// reload rebuilds the in-memory maps from kv. Used at construction;
// also called by tests that mutate kv directly to re-sync the cache.
//
// Expired rows are filtered out of the cache AND scheduled for
// best-effort kv deletion so a sweep round doesn't have to start
// fresh on every boot. Verifier correctness does not depend on the
// kv delete succeeding (the cache filter is the load-bearing
// surface); the cleanup is purely to keep the row count bounded
// across long-running deployments.
func (s *WebDAVTokenStore) reload(ctx context.Context) error {
	rows, err := s.kv.ListKV(ctx, authKVNamespace)
	if err != nil {
		return fmt.Errorf("auth: list webdav tokens: %w", err)
	}
	nowMs := s.now().UnixMilli()
	hashes := make(map[string]string)
	cached := make(map[string]webdavTokenRow)
	type expiredEntry struct {
		key, etag string
	}
	var expired []expiredEntry
	for _, r := range rows {
		id, ok := strings.CutPrefix(r.Key, webdavKVKeyPrefix)
		if !ok {
			continue
		}
		if !webdavCanonicalIDPattern.MatchString(id) {
			// A row with a non-canonical id can't have been written
			// by this binary; treat it as junk and skip. The strict
			// id gate sits here (rather than in parseWebDAVKVValue)
			// so the URL-side webdavIDPattern can stay looser for
			// future id-shape migrations.
			continue
		}
		row, err := parseWebDAVKVValue(r)
		if err != nil {
			// Malformed row from a buggy peer — log-and-skip rather
			// than fail the whole boot. The sweeper / next reload
			// will surface the inconsistency in a different signal.
			continue
		}
		row.ID = id
		if row.ExpiresAt <= nowMs {
			// Already expired — drop from the verifier AND queue
			// for kv cleanup. Without this, a row that expired
			// while the binary was offline would survive every
			// future Sweep (the sweeper only walks the cache).
			expired = append(expired, expiredEntry{key: r.Key, etag: r.ETag})
			continue
		}
		hashes[row.Hash] = id
		cached[id] = row
	}
	s.mu.Lock()
	s.hashes = hashes
	s.rows = cached
	s.mu.Unlock()
	for _, e := range expired {
		// Conditional delete keyed on the etag we observed during
		// the list scan. A concurrent re-Issue with the same id
		// (effectively impossible, but cheap to guard) advances the
		// etag; we leave that row alone and the next sweep round
		// re-evaluates. Best-effort otherwise — the verifier path
		// already filters expired rows correctly.
		if err := s.kv.DeleteKV(ctx, authKVNamespace, e.key, e.etag); err != nil {
			if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrETagMismatch) {
				continue
			}
			continue
		}
	}
	return nil
}

// Verify reports whether presented matches a live (non-expired) token.
// Comparison is constant-time at the map-key level (hashToken on the
// presented value, then a single map lookup whose key length is fixed).
func (s *WebDAVTokenStore) Verify(presented string) bool {
	if s == nil || presented == "" {
		return false
	}
	h := hashToken(presented)
	s.mu.RLock()
	id, ok := s.hashes[h]
	if !ok {
		s.mu.RUnlock()
		return false
	}
	row, rowOK := s.rows[id]
	s.mu.RUnlock()
	if !rowOK {
		return false
	}
	// Defence in depth: re-verify the stored hash matches (constant-
	// time) to guard against a future caller mutating the map outside
	// the locking discipline.
	if subtle.ConstantTimeCompare([]byte(h), []byte(row.Hash)) != 1 {
		return false
	}
	if s.now().UnixMilli() >= row.ExpiresAt {
		return false
	}
	return true
}

// Issue mints a fresh token with the given label and TTL. The returned
// Token is the only chance the caller has to read the raw value; the
// store keeps only the hash.
func (s *WebDAVTokenStore) Issue(ctx context.Context, label string, ttl time.Duration) (*IssueResult, error) {
	if s == nil {
		return nil, errors.New("auth: nil webdav token store")
	}
	label = strings.TrimSpace(label)
	if label == "" {
		return nil, errors.New("auth: label required")
	}
	if len(label) > webdavTokenLabelMaxLen {
		return nil, fmt.Errorf("auth: label too long (%d > %d)", len(label), webdavTokenLabelMaxLen)
	}
	if !isPrintableLabel(label) {
		return nil, errors.New("auth: label contains control characters")
	}
	if ttl < webdavMinTTL {
		return nil, fmt.Errorf("auth: ttl too short (min %s)", webdavMinTTL)
	}
	if ttl > webdavMaxTTL {
		return nil, fmt.Errorf("auth: ttl too long (max %s)", webdavMaxTTL)
	}
	id, err := s.newID()
	if err != nil {
		return nil, err
	}
	raw, err := s.newRaw()
	if err != nil {
		return nil, err
	}
	hash := hashToken(raw)
	nowMs := s.now().UnixMilli()
	row := webdavTokenRow{
		ID:        id,
		Hash:      hash,
		Label:     label,
		CreatedAt: nowMs,
		ExpiresAt: nowMs + ttl.Milliseconds(),
	}
	if err := s.persist(ctx, row); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.hashes[hash] = id
	s.rows[id] = row
	s.mu.Unlock()
	return &IssueResult{
		ID:        id,
		Token:     raw,
		Label:     label,
		CreatedAt: nowMs,
		ExpiresAt: row.ExpiresAt,
	}, nil
}

// Revoke removes the token by id. Returns ErrNotFound when the id is
// unknown so the HTTP layer can map it to 404. Unknown id on a fresh
// boot (cache miss) falls through to kv; absent there as well returns
// ErrNotFound. Idempotent on a known id — calling twice is a no-op
// on the second call (returns ErrNotFound).
func (s *WebDAVTokenStore) Revoke(ctx context.Context, id string) error {
	if s == nil {
		return errors.New("auth: nil webdav token store")
	}
	if !webdavIDPattern.MatchString(id) {
		return fmt.Errorf("auth: invalid webdav token id")
	}
	// Probe kv first to distinguish "id doesn't exist anywhere" from
	// "id exists somewhere we should clean up". A cache miss alone
	// isn't sufficient evidence: a peer-replicated row could have
	// landed after our last reload.
	rec, getErr := s.kv.GetKV(ctx, authKVNamespace, webdavKVKey(id))
	if getErr != nil {
		if errors.Is(getErr, store.ErrNotFound) {
			// kv has no row. The cache should be empty too in a
			// correctly-synchronized store, but defensively drop any
			// surviving cache entry before reporting absence.
			s.mu.Lock()
			if row, ok := s.rows[id]; ok {
				delete(s.hashes, row.Hash)
				delete(s.rows, id)
			}
			s.mu.Unlock()
			return store.ErrNotFound
		}
		return fmt.Errorf("auth: get webdav token: %w", getErr)
	}
	// kv-first delete: a transient delete failure must NOT leave the
	// cache empty while the kv row survives (or the next reload
	// would resurrect a "revoked" token). We delete kv up front and
	// only mutate the cache after success.
	if err := s.kv.DeleteKV(ctx, authKVNamespace, webdavKVKey(id), rec.ETag); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Concurrent revoke / sweep removed it after our GET.
			// Drop the cache entry to stay consistent and treat
			// this caller's revoke as a no-op success.
			s.mu.Lock()
			if row, ok := s.rows[id]; ok {
				delete(s.hashes, row.Hash)
				delete(s.rows, id)
			}
			s.mu.Unlock()
			return nil
		}
		if errors.Is(err, store.ErrETagMismatch) {
			// kv's conditional DELETE returns ErrETagMismatch for
			// EITHER "row gone" OR "etag advanced". Disambiguate by
			// re-reading: a missing row means a concurrent revoke
			// won, which is success for our caller too; an updated
			// row means a concurrent Issue with the same id (next
			// to impossible at 128-bit randomness) — surface so the
			// operator can retry.
			if _, getErr := s.kv.GetKV(ctx, authKVNamespace, webdavKVKey(id)); getErr != nil {
				if errors.Is(getErr, store.ErrNotFound) {
					s.mu.Lock()
					if row, ok := s.rows[id]; ok {
						delete(s.hashes, row.Hash)
						delete(s.rows, id)
					}
					s.mu.Unlock()
					return nil
				}
				return fmt.Errorf("auth: webdav token revoke verify: %w", getErr)
			}
			return fmt.Errorf("auth: webdav token mutated during revoke: %w", err)
		}
		return fmt.Errorf("auth: delete webdav token: %w", err)
	}
	s.mu.Lock()
	if row, ok := s.rows[id]; ok {
		delete(s.hashes, row.Hash)
		delete(s.rows, id)
	}
	s.mu.Unlock()
	return nil
}

// List returns metadata for every live token in ascending CreatedAt
// order. Expired rows are filtered out (the sweeper will physically
// remove them on its next tick).
func (s *WebDAVTokenStore) List() []WebDAVTokenMeta {
	if s == nil {
		return nil
	}
	nowMs := s.now().UnixMilli()
	s.mu.RLock()
	out := make([]WebDAVTokenMeta, 0, len(s.rows))
	for _, row := range s.rows {
		if row.ExpiresAt <= nowMs {
			continue
		}
		out = append(out, WebDAVTokenMeta{
			ID:        row.ID,
			Label:     row.Label,
			CreatedAt: row.CreatedAt,
			ExpiresAt: row.ExpiresAt,
		})
	}
	s.mu.RUnlock()
	// Stable order by CreatedAt then ID for deterministic UI rendering.
	sortMetaByCreated(out)
	return out
}

// Sweep removes every expired row from both kv and the in-memory
// cache. Returns the count of rows removed. Errors on individual
// deletes are logged via the returned error chain — the caller
// (typically a periodic goroutine) is expected to retry on its next
// tick.
//
// Walks kv (not the cache) so an expired row that was created on a
// peer between reloads — or one that landed in kv before the
// reload-time cleanup got to it — is reachable for deletion.
func (s *WebDAVTokenStore) Sweep(ctx context.Context) (int, error) {
	if s == nil {
		return 0, nil
	}
	rows, err := s.kv.ListKV(ctx, authKVNamespace)
	if err != nil {
		return 0, fmt.Errorf("auth: sweep list: %w", err)
	}
	nowMs := s.now().UnixMilli()
	removed := 0
	var firstErr error
	for _, r := range rows {
		id, ok := strings.CutPrefix(r.Key, webdavKVKeyPrefix)
		if !ok {
			continue
		}
		// Use the loose URL gate here, not the strict canonical one,
		// so a row written by a future binary with a longer id is
		// still subject to expiry cleanup. The verifier path
		// (reload) keeps the strict shape.
		if !webdavIDPattern.MatchString(id) {
			continue
		}
		row, perr := parseWebDAVKVValue(r)
		if perr != nil {
			// Garbage row — defer to manual remediation. Sweeper
			// shouldn't decide whether to delete a row it can't
			// parse (it might be a future format we don't know
			// how to interpret).
			continue
		}
		if row.ExpiresAt > nowMs {
			continue
		}
		// Conditional delete keyed on the row's etag so a concurrent
		// re-issue with the same id (next to impossible at 128-bit
		// randomness, but the cost of guarding is one extra etag
		// compare) does NOT wipe the fresh row. ErrETagMismatch
		// signals "row advanced" — skip this tick, the next sweep
		// will re-evaluate; ErrNotFound signals "someone else
		// deleted it" — count as success.
		if err := s.kv.DeleteKV(ctx, authKVNamespace, r.Key, r.ETag); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.mu.Lock()
				if cur, ok := s.rows[id]; ok && cur.Hash == row.Hash {
					delete(s.hashes, row.Hash)
					delete(s.rows, id)
				}
				s.mu.Unlock()
				removed++
				continue
			}
			if errors.Is(err, store.ErrETagMismatch) {
				// kv's conditional DELETE returns ErrETagMismatch
				// for EITHER "row gone" OR "etag advanced".
				// Disambiguate so we still drop a stale cache
				// entry when another peer's revoke / sweep already
				// removed the row out from under us. Without this,
				// a cross-peer race could leave an expired row in
				// our in-memory `rows` map until the next reload —
				// Verify is safe (it re-checks expiry) but the
				// sweep post-condition wouldn't hold.
				_, getErr := s.kv.GetKV(ctx, authKVNamespace, r.Key)
				switch {
				case errors.Is(getErr, store.ErrNotFound):
					// Row already gone — drop cache and count
					// as a successful sweep so the operator-
					// visible metric reflects reality.
					s.mu.Lock()
					if cur, ok := s.rows[id]; ok && cur.Hash == row.Hash {
						delete(s.hashes, row.Hash)
						delete(s.rows, id)
					}
					s.mu.Unlock()
					removed++
				case getErr != nil:
					// Disambiguation failed (kv unreachable,
					// transient lock). Surface so the caller
					// can retry on the next tick instead of
					// silently losing the cache-cleanup
					// opportunity.
					if firstErr == nil {
						firstErr = fmt.Errorf("auth: sweep verify %s: %w", id, getErr)
					}
				default:
					// Row still exists with a newer etag (a
					// concurrent re-Issue with the same id,
					// vanishingly unlikely at 128-bit
					// randomness) — leave it for the next sweep
					// tick.
				}
				continue
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("auth: sweep delete %s: %w", id, err)
			}
			continue
		}
		// Drop the cache after a confirmed kv delete. Doing it
		// post-delete keeps Verify correct under contention: a
		// concurrent Verify that hit the cache between list and
		// delete still sees the row as live until we actually
		// commit the delete.
		s.mu.Lock()
		if cur, ok := s.rows[id]; ok && cur.Hash == row.Hash {
			delete(s.hashes, row.Hash)
			delete(s.rows, id)
		}
		s.mu.Unlock()
		removed++
	}
	return removed, firstErr
}

// persist writes the row to kv with IfMatchAny so a hash-collision id
// (statistically impossible at our scale, but the constraint is cheap)
// surfaces as ErrETagMismatch instead of silently overwriting.
func (s *WebDAVTokenStore) persist(ctx context.Context, row webdavTokenRow) error {
	body, err := json.Marshal(struct {
		Hash      string `json:"hash"`
		Label     string `json:"label"`
		CreatedAt int64  `json:"created_at"`
		ExpiresAt int64  `json:"expires_at"`
	}{
		// The on-wire form mirrors the auth/owner.token row format:
		// "sha256:<64hex>". The in-memory Hash is bare hex (the form
		// hashToken returns) so the verifier can compare the
		// presented value's hash without an extra strip. The
		// prefix is added here at the persist boundary and removed
		// by parseWebDAVKVValue on reload.
		Hash:      hashedTokenPrefix + row.Hash,
		Label:     row.Label,
		CreatedAt: row.CreatedAt,
		ExpiresAt: row.ExpiresAt,
	})
	if err != nil {
		return fmt.Errorf("auth: marshal webdav row: %w", err)
	}
	rec := &store.KVRecord{
		Namespace: authKVNamespace,
		Key:       webdavKVKey(row.ID),
		Value:     string(body),
		Type:      store.KVTypeJSON,
		Scope:     store.KVScopeGlobal,
	}
	if _, err := s.kv.PutKV(ctx, rec, store.KVPutOptions{IfMatchETag: store.IfMatchAny}); err != nil {
		return fmt.Errorf("auth: put webdav token: %w", err)
	}
	return nil
}

// newID returns a 128-bit random hex string suitable for use as the
// kv-row identifier. Not a UUID — we don't need the version/variant
// bits, and a raw hex string keeps url-encoding simple.
func (s *WebDAVTokenStore) newID() (string, error) {
	var buf [16]byte
	if _, err := s.random(buf[:]); err != nil {
		return "", fmt.Errorf("auth: read random id: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// newRaw returns a 256-bit random hex string for the token body. The
// hex form is convenient for shoving into a Basic auth password field
// without URL-encoding concerns.
func (s *WebDAVTokenStore) newRaw() (string, error) {
	var buf [32]byte
	if _, err := s.random(buf[:]); err != nil {
		return "", fmt.Errorf("auth: read random raw: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// webdavKVKey builds the kv key for a given token id.
func webdavKVKey(id string) string {
	return webdavKVKeyPrefix + id
}

// parseWebDAVKVValue is the strict row-shape gate: a malformed row
// (wrong scope/type/secret combo, missing hash, malformed expiry) is
// rejected so a peer-replicated junk row can't backdoor the verifier.
func parseWebDAVKVValue(rec *store.KVRecord) (webdavTokenRow, error) {
	var zero webdavTokenRow
	if rec == nil {
		return zero, errors.New("nil record")
	}
	if rec.Type != store.KVTypeJSON || rec.Scope != store.KVScopeGlobal || rec.Secret {
		return zero, fmt.Errorf("row shape mismatch (type=%q scope=%q secret=%t)", rec.Type, rec.Scope, rec.Secret)
	}
	var raw struct {
		Hash      string `json:"hash"`
		Label     string `json:"label"`
		CreatedAt int64  `json:"created_at"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(rec.Value), &raw); err != nil {
		return zero, fmt.Errorf("decode json: %w", err)
	}
	hash, ok := strings.CutPrefix(raw.Hash, hashedTokenPrefix)
	if !ok {
		return zero, errors.New("hash missing sha256: prefix")
	}
	if !isHex64(hash) {
		return zero, fmt.Errorf("malformed hash (len=%d)", len(hash))
	}
	for i := 0; i < len(hash); i++ {
		if c := hash[i]; c >= 'A' && c <= 'F' {
			return zero, fmt.Errorf("uppercase hex in webdav hash (idx %d)", i)
		}
	}
	if raw.ExpiresAt <= 0 || raw.CreatedAt <= 0 {
		return zero, errors.New("missing created_at or expires_at")
	}
	if raw.ExpiresAt <= raw.CreatedAt {
		return zero, errors.New("expires_at must be after created_at")
	}
	// TTL bound mirrors what Issue enforces. Compare in raw int64
	// milliseconds — `time.Duration(ms) * time.Millisecond` would
	// overflow for absurd `expires_at` values and silently wrap the
	// comparison around zero, letting a 100-year row appear to fit
	// in [5min, 24h].
	durMs := raw.ExpiresAt - raw.CreatedAt
	if durMs < webdavMinTTL.Milliseconds() || durMs > webdavMaxTTL.Milliseconds() {
		return zero, fmt.Errorf("ttl %dms out of bounds [%d, %d]", durMs,
			webdavMinTTL.Milliseconds(), webdavMaxTTL.Milliseconds())
	}
	if len(raw.Label) > webdavTokenLabelMaxLen {
		return zero, fmt.Errorf("label too long (len=%d)", len(raw.Label))
	}
	if !isPrintableLabel(raw.Label) {
		return zero, errors.New("label contains control characters")
	}
	return webdavTokenRow{
		Hash:      hash,
		Label:     raw.Label,
		CreatedAt: raw.CreatedAt,
		ExpiresAt: raw.ExpiresAt,
	}, nil
}

// isPrintableLabel rejects control characters so a token label can't
// smuggle newlines / NULs into log lines or JSON-encoded list output.
// Spaces and the printable ASCII range are fine; everything outside
// is fine too as long as it's a non-control rune (multi-byte UTF-8 is
// allowed). This matches the slackbot label / agent name gate posture.
func isPrintableLabel(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// sortMetaByCreated does an insertion sort on the (small) list slice.
// We avoid importing sort just for this: WebDAV token count rarely
// exceeds a dozen per cluster (one or two long-lived mounts per device).
func sortMetaByCreated(out []WebDAVTokenMeta) {
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			if out[j-1].CreatedAt < out[j].CreatedAt ||
				(out[j-1].CreatedAt == out[j].CreatedAt && out[j-1].ID < out[j].ID) {
				break
			}
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
}


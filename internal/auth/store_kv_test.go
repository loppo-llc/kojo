package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// newKVStore opens an isolated kojo.db rooted at a t.TempDir() and
// returns the *store.Store. The store handle is closed at test
// teardown so callers don't have to thread the cleanup themselves.
func newKVStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestNewTokenStore_KVCanonicalOnFreshInstall pins the basic
// post-cutover happy path: a fresh install with kv wired writes
// the owner hash to kv (not to disk) and a subsequent boot reads
// it back without touching the legacy disk file.
func TestNewTokenStore_KVCanonicalOnFreshInstall(t *testing.T) {
	base := t.TempDir()
	kv := newKVStore(t)

	st, err := NewTokenStore(base, kv, "")
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	if st.OwnerHash() == "" {
		t.Fatalf("OwnerHash empty after fresh install")
	}
	raw := st.OwnerToken()
	if raw == "" {
		t.Fatalf("OwnerToken empty on fresh install (expected raw)")
	}

	// Disk file must NOT have been written — kv is canonical.
	if _, err := os.Stat(filepath.Join(base, "owner.token")); !os.IsNotExist(err) {
		t.Errorf("legacy disk file present after fresh install: err=%v", err)
	}

	// kv row must exist.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, err := kv.GetKV(ctx, authKVNamespace, authKVOwnerKey)
	if err != nil {
		t.Fatalf("GetKV after fresh install: %v", err)
	}
	if rec.Scope != store.KVScopeGlobal {
		t.Errorf("scope = %q; want global", rec.Scope)
	}
	if rec.Type != store.KVTypeString {
		t.Errorf("type = %q; want string", rec.Type)
	}
	if rec.Secret {
		t.Errorf("secret = true; want false (verifier hash, not credential)")
	}
}

// TestNewTokenStore_KVOwnerLegacyMigration verifies the disk →
// kv migration path: a pre-cutover install with a disk
// owner.token file gets mirrored into kv on first boot, the disk
// file is unlinked, and a second boot reads kv only.
func TestNewTokenStore_KVOwnerLegacyMigration(t *testing.T) {
	base := t.TempDir()
	kv := newKVStore(t)

	// Seed legacy disk file in hashed form (post-Phase-5 layout).
	legacyHash := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	legacyPath := filepath.Join(base, "owner.token")
	if err := os.WriteFile(legacyPath, []byte(hashedTokenPrefix+legacyHash+"\n"), 0o600); err != nil {
		t.Fatalf("seed legacy file: %v", err)
	}

	st, err := NewTokenStore(base, kv, "")
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	if got := st.OwnerHash(); got != legacyHash {
		t.Errorf("OwnerHash = %q; want %q (migrated from disk)", got, legacyHash)
	}

	// Legacy file must be unlinked.
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Errorf("legacy disk file present after migration: err=%v", err)
	}

	// kv row must exist with the migrated hash.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, err := kv.GetKV(ctx, authKVNamespace, authKVOwnerKey)
	if err != nil {
		t.Fatalf("GetKV after migration: %v", err)
	}
	if rec.Value != hashedTokenPrefix+legacyHash {
		t.Errorf("kv value = %q; want %q", rec.Value, hashedTokenPrefix+legacyHash)
	}
}

// TestNewTokenStore_KVOwnerKVHitDropsStaleLegacyFile pins the
// "kv is authoritative" branch: when both the kv row and a
// legacy disk file exist (a v1 → v0 → v1 round trip), kv wins
// and the disk file is best-effort unlinked so the next pre-
// cutover boot doesn't read a stale value.
func TestNewTokenStore_KVOwnerKVHitDropsStaleLegacyFile(t *testing.T) {
	base := t.TempDir()
	kv := newKVStore(t)

	// Seed authoritative kv row.
	canonicalHash := "1111111111111111111111111111111111111111111111111111111111111111"
	if err := saveOwnerKV(kv, canonicalHash, ""); err != nil {
		t.Fatalf("saveOwnerKV: %v", err)
	}

	// Seed stale legacy file (lying — different hash).
	staleHash := "2222222222222222222222222222222222222222222222222222222222222222"
	legacyPath := filepath.Join(base, "owner.token")
	if err := os.WriteFile(legacyPath, []byte(hashedTokenPrefix+staleHash+"\n"), 0o600); err != nil {
		t.Fatalf("seed stale legacy file: %v", err)
	}

	st, err := NewTokenStore(base, kv, "")
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	if got := st.OwnerHash(); got != canonicalHash {
		t.Errorf("OwnerHash = %q; want %q (kv must win over stale disk)", got, canonicalHash)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Errorf("stale legacy file not unlinked: err=%v", err)
	}
}

// TestPersistAgentTokenHash_KVWritesAndDropsLegacy verifies that
// AgentToken's fresh-generate path writes to kv and unlinks any
// surviving legacy disk file, so a re-boot under the new code
// reads from kv (and a re-boot under pre-cutover code finds no
// stale file). Uses a fresh agent id (no seeded hash) so the
// AgentToken path takes the "generate + persist" branch rather
// than the "hash exists, raw unavailable" branch.
func TestPersistAgentTokenHash_KVWritesAndDropsLegacy(t *testing.T) {
	base := t.TempDir()
	kv := newKVStore(t)

	st, err := NewTokenStore(base, kv, "owner-x")
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	// Generate a fresh agent token for an agent that has no
	// seeded hash; persist path should land in kv.
	tok, err := st.AgentToken("ag_fresh")
	if err != nil {
		t.Fatalf("AgentToken: %v", err)
	}
	if tok == "" {
		t.Fatalf("AgentToken empty")
	}

	// kv row must hold the new hash.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, err := kv.GetKV(ctx, authKVNamespace, authKVAgentTokenKey("ag_fresh"))
	if err != nil {
		t.Fatalf("GetKV: %v", err)
	}
	wantHash := hashToken(tok)
	if rec.Value != hashedTokenPrefix+wantHash {
		t.Errorf("kv value = %q; want %q", rec.Value, hashedTokenPrefix+wantHash)
	}

	// No legacy file should have been written (kv is canonical).
	if _, err := os.Stat(filepath.Join(base, "agent_tokens", "ag_fresh")); !os.IsNotExist(err) {
		t.Errorf("legacy file written when kv is wired: err=%v", err)
	}
}

// TestRemoveAgentToken_DropsKVAndLegacy verifies the cleanup
// helper used by Manager.Delete: after RemoveAgentToken both
// the kv row and any legacy disk file are gone, and a second
// boot enumerates no entry.
func TestRemoveAgentToken_DropsKVAndLegacy(t *testing.T) {
	base := t.TempDir()
	kv := newKVStore(t)
	st, err := NewTokenStore(base, kv, "owner-x")
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	if _, err := st.AgentToken("ag"); err != nil {
		t.Fatalf("seed AgentToken: %v", err)
	}
	st.RemoveAgentToken("ag")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := kv.GetKV(ctx, authKVNamespace, authKVAgentTokenKey("ag")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("kv row present after Remove: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "agent_tokens", "ag")); !os.IsNotExist(err) {
		t.Errorf("legacy file present after Remove: err=%v", err)
	}
}

// TestReissueAgentToken_PostRestartRecovery pins the
// post-migration-restart recovery path: after a fresh-issue,
// reopen-from-kv drops the raw, AgentToken errors with
// ErrTokenRawUnavailable, and ReissueAgentToken atomically swaps
// in a fresh token whose hash supersedes the old one in kv. The
// previously-issued raw must NO LONGER verify; the new raw must.
func TestReissueAgentToken_PostRestartRecovery(t *testing.T) {
	base := t.TempDir()
	kv := newKVStore(t)

	st1, err := NewTokenStore(base, kv, "owner-x")
	if err != nil {
		t.Fatalf("NewTokenStore #1: %v", err)
	}
	oldTok, err := st1.AgentToken("ag_restart")
	if err != nil {
		t.Fatalf("seed AgentToken: %v", err)
	}

	// Reopen — kv has the hash, in-memory raw is gone.
	st2, err := NewTokenStore(base, kv, "owner-x")
	if err != nil {
		t.Fatalf("NewTokenStore #2: %v", err)
	}
	if _, err := st2.AgentToken("ag_restart"); !errors.Is(err, ErrTokenRawUnavailable) {
		t.Fatalf("post-reopen AgentToken: got err=%v, want ErrTokenRawUnavailable", err)
	}

	newTok, err := st2.ReissueAgentToken("ag_restart")
	if err != nil {
		t.Fatalf("ReissueAgentToken: %v", err)
	}
	if newTok == "" || newTok == oldTok {
		t.Fatalf("reissued token: empty or unchanged (old=%q new=%q)", oldTok, newTok)
	}
	if id, ok := st2.LookupAgent(newTok); !ok || id != "ag_restart" {
		t.Errorf("LookupAgent(new): got (%q,%v), want (ag_restart,true)", id, ok)
	}
	if _, ok := st2.LookupAgent(oldTok); ok {
		t.Errorf("LookupAgent(old): still resolves after reissue (verifier hash not rotated)")
	}

	// Idempotency under concurrency: a second Reissue MUST return
	// the same raw the first call cached, so racing PTY spawns for
	// the same agent never invalidate each other's tokens.
	again, err := st2.ReissueAgentToken("ag_restart")
	if err != nil {
		t.Fatalf("ReissueAgentToken #2: %v", err)
	}
	if again != newTok {
		t.Errorf("ReissueAgentToken not idempotent: %q vs %q", again, newTok)
	}

	// kv row reflects the new hash, not the old.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, err := kv.GetKV(ctx, authKVNamespace, authKVAgentTokenKey("ag_restart"))
	if err != nil {
		t.Fatalf("GetKV: %v", err)
	}
	if rec.Value != hashedTokenPrefix+hashToken(newTok) {
		t.Errorf("kv hash not rotated: got %q, want %q", rec.Value, hashedTokenPrefix+hashToken(newTok))
	}
}

// TestReissueAgentToken_ConcurrentSingleWinner pins the race
// invariant Codex flagged: multiple goroutines racing
// ReissueAgentToken for the same agent (the post-restart "first PTY
// spawn" pattern under a burst of incoming requests) MUST observe a
// single rotation. Every caller receives the same raw, the kv row
// is written exactly once with that hash, and the previously-issued
// raw is uniformly invalidated.
func TestReissueAgentToken_ConcurrentSingleWinner(t *testing.T) {
	base := t.TempDir()
	kv := newKVStore(t)

	// Seed: fresh-issue once, then reopen so the in-memory raw is
	// gone (simulating a post-restart store).
	st1, err := NewTokenStore(base, kv, "owner-x")
	if err != nil {
		t.Fatalf("NewTokenStore #1: %v", err)
	}
	oldTok, err := st1.AgentToken("ag_race")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	st, err := NewTokenStore(base, kv, "owner-x")
	if err != nil {
		t.Fatalf("NewTokenStore #2: %v", err)
	}

	const N = 16
	var wg sync.WaitGroup
	results := make([]string, N)
	errs := make([]error, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = st.ReissueAgentToken("ag_race")
		}(i)
	}
	wg.Wait()

	first := ""
	for i := 0; i < N; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: err=%v", i, errs[i])
			continue
		}
		if results[i] == "" {
			t.Errorf("goroutine %d: empty token", i)
			continue
		}
		if first == "" {
			first = results[i]
		} else if results[i] != first {
			t.Errorf("goroutine %d: token diverged: got %q want %q", i, results[i], first)
		}
	}
	if first == "" {
		t.Fatal("no goroutine succeeded")
	}
	if first == oldTok {
		t.Errorf("reissued raw equals pre-reopen raw; rotation didn't happen")
	}

	// Old raw must NO LONGER verify; new raw MUST.
	if _, ok := st.LookupAgent(oldTok); ok {
		t.Error("LookupAgent(oldTok) resolves after concurrent reissue")
	}
	if id, ok := st.LookupAgent(first); !ok || id != "ag_race" {
		t.Errorf("LookupAgent(new): got (%q,%v), want (ag_race,true)", id, ok)
	}

	// kv row must equal hash(first), confirming a single CAS-success
	// landed (additional in-flight calls observed the cached raw via
	// the idempotency short-circuit and skipped the write).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, err := kv.GetKV(ctx, authKVNamespace, authKVAgentTokenKey("ag_race"))
	if err != nil {
		t.Fatalf("GetKV: %v", err)
	}
	if rec.Value != hashedTokenPrefix+hashToken(first) {
		t.Errorf("kv row hash mismatch: got %q, want %q", rec.Value, hashedTokenPrefix+hashToken(first))
	}
}

// TestReissueAgentToken_StaleLocalPeerAdoptsWinner pins the
// cluster-safety invariant Codex flagged: when peer A reopens with a
// stale idIndex hash and peer B has already rotated the agent's kv
// row to a fresh hash, peer A's ReissueAgentToken MUST NOT overwrite
// peer B's hash (which would invalidate peer B's just-minted raw).
// Instead peer A adopts peer B's hash, returns ErrTokenRawUnavailable,
// and the kv row remains unchanged.
//
// Modelled with two distinct TokenStore handles over a shared kv:
// stA seeds and reopens (stale idIndex, no raw); stB rotates fresh
// (writes new hash); stA.Reissue must observe the divergence and
// abort.
func TestReissueAgentToken_StaleLocalPeerAdoptsWinner(t *testing.T) {
	base := t.TempDir()
	kv := newKVStore(t)

	// stA seeds, then reopens so its idIndex holds the seeded hash
	// but rawByID is empty (mirrors a peer that just restarted).
	stSeed, err := NewTokenStore(base, kv, "owner-x")
	if err != nil {
		t.Fatalf("NewTokenStore seed: %v", err)
	}
	if _, err := stSeed.AgentToken("ag_peer"); err != nil {
		t.Fatalf("seed AgentToken: %v", err)
	}
	stA, err := NewTokenStore(base, kv, "owner-x")
	if err != nil {
		t.Fatalf("NewTokenStore stA: %v", err)
	}

	// stB independently reopens and rotates — simulating the other
	// peer that legitimately re-issued.
	stB, err := NewTokenStore(base, kv, "owner-x")
	if err != nil {
		t.Fatalf("NewTokenStore stB: %v", err)
	}
	bTok, err := stB.ReissueAgentToken("ag_peer")
	if err != nil {
		t.Fatalf("stB.Reissue: %v", err)
	}
	bHash := hashToken(bTok)

	// stA's Reissue must NOT overwrite stB's hash.
	got, err := stA.ReissueAgentToken("ag_peer")
	if !errors.Is(err, ErrTokenRawUnavailable) {
		t.Fatalf("stA.Reissue: got (%q, %v); want (\"\", ErrTokenRawUnavailable)", got, err)
	}
	if got != "" {
		t.Errorf("stA returned raw %q on adopt path; expected empty", got)
	}

	// stA's idIndex must now hold stB's hash so stB's raw verifies
	// here.
	if id, ok := stA.LookupAgent(bTok); !ok || id != "ag_peer" {
		t.Errorf("stA.LookupAgent(bTok): got (%q,%v); want (ag_peer,true) — adopt failed", id, ok)
	}

	// kv row must still hold stB's hash, not anything stA generated.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, err := kv.GetKV(ctx, authKVNamespace, authKVAgentTokenKey("ag_peer"))
	if err != nil {
		t.Fatalf("GetKV: %v", err)
	}
	if rec.Value != hashedTokenPrefix+bHash {
		t.Errorf("kv row diverged from stB: got %q, want %q (stA clobbered the peer)", rec.Value, hashedTokenPrefix+bHash)
	}
}

// TestNewTokenStore_KVAgentTokensLegacyMigration verifies
// per-agent disk → kv migration with multiple entries: each
// disk file gets mirrored, the in-memory maps populate, and
// every disk file is unlinked.
func TestNewTokenStore_KVAgentTokensLegacyMigration(t *testing.T) {
	base := t.TempDir()
	kv := newKVStore(t)

	dir := filepath.Join(base, "agent_tokens")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seeds := map[string]string{
		"ag_a": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"ag_b": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	for id, hash := range seeds {
		if err := os.WriteFile(filepath.Join(dir, id), []byte(hashedTokenPrefix+hash+"\n"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	st, err := NewTokenStore(base, kv, "owner-x")
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	for id, want := range seeds {
		st.mu.RLock()
		got := st.idIndex[id]
		st.mu.RUnlock()
		if got != want {
			t.Errorf("idIndex[%s] = %q; want %q", id, got, want)
		}
		if _, err := os.Stat(filepath.Join(dir, id)); !os.IsNotExist(err) {
			t.Errorf("legacy file %s still present after migration: err=%v", id, err)
		}
	}
}

// TestNewTokenStore_NilKVFallsBackToDisk pins the test-fixture
// fallback: when kv is nil the constructor uses the legacy disk
// path verbatim. Existing auth_test.go tests rely on this.
func TestNewTokenStore_NilKVFallsBackToDisk(t *testing.T) {
	base := t.TempDir()
	st, err := NewTokenStore(base, nil, "")
	if err != nil {
		t.Fatalf("NewTokenStore(nil kv): %v", err)
	}
	if st.OwnerHash() == "" {
		t.Fatalf("OwnerHash empty")
	}
	if _, err := os.Stat(filepath.Join(base, "owner.token")); err != nil {
		t.Errorf("disk owner.token not written when kv is nil: err=%v", err)
	}
}

// TestParseAuthKVValue_RowShapeGate exercises the row-shape
// validation: rows with the wrong type / scope / secret flag, or
// with malformed value, fail to parse rather than silently
// surface a corrupt verifier hash. This is the security spine of
// the cutover — a peer that replicated junk into the auth
// namespace cannot trick the local store into honouring it.
func TestParseAuthKVValue_RowShapeGate(t *testing.T) {
	good := &store.KVRecord{
		Type:  store.KVTypeString,
		Scope: store.KVScopeGlobal,
		Value: hashedTokenPrefix + "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	}
	if hash, err := parseAuthKVValue(good); err != nil || hash != "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" {
		t.Errorf("good row: hash=%q err=%v", hash, err)
	}

	cases := []struct {
		name string
		mut  func(*store.KVRecord)
	}{
		{"wrong type", func(r *store.KVRecord) { r.Type = store.KVTypeJSON }},
		{"wrong scope local", func(r *store.KVRecord) { r.Scope = store.KVScopeLocal }},
		{"wrong scope machine", func(r *store.KVRecord) { r.Scope = store.KVScopeMachine }},
		{"secret flag set", func(r *store.KVRecord) { r.Secret = true }},
		{"missing prefix", func(r *store.KVRecord) { r.Value = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" }},
		{"non-hex hash", func(r *store.KVRecord) { r.Value = hashedTokenPrefix + "not-hex-not-hex-not-hex-not-hex-not-hex-not-hex-not-hex-not-hex-" }},
		{"truncated hash", func(r *store.KVRecord) { r.Value = hashedTokenPrefix + "ff" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := *good
			c.mut(&r)
			if _, err := parseAuthKVValue(&r); err == nil {
				t.Errorf("expected error for %s; got nil", c.name)
			}
		})
	}
}

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// newAgentStore opens an isolated kojo.db rooted at a t.TempDir() HOME and
// returns the *agentStore. Tests for cron_paused stick to this minimal
// scaffolding — they don't need a *Manager, just direct access to the
// kv-backed Load/Save pair.
func newAgentStore(t *testing.T) *agentStore {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("APPDATA", "")
	st, err := newStore(testLogger())
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestCronPausedKV_DefaultFalse verifies a fresh install (no kv row, no
// legacy file) reports "not paused" — the safe default that a daemon
// missing both signals can run schedules without surprise.
func TestCronPausedKV_DefaultFalse(t *testing.T) {
	st := newAgentStore(t)
	if st.LoadCronPaused() {
		t.Fatalf("LoadCronPaused on fresh install = true; want false")
	}
}

// TestCronPausedKV_SaveLoadRoundtrip walks the basic state machine:
//
//	false → save(true) → load==true → save(false) → load==false
//
// Both states must persist as kv rows (a missing row is "not paused", but
// after explicit save(false) the next load must read that row, not fall
// through to the legacy-file branch). The test inspects the kv row
// directly to pin the value="false" representation.
func TestCronPausedKV_SaveLoadRoundtrip(t *testing.T) {
	st := newAgentStore(t)

	if err := st.SaveCronPaused(true); err != nil {
		t.Fatalf("SaveCronPaused(true): %v", err)
	}
	if !st.LoadCronPaused() {
		t.Errorf("after Save(true): Load = false; want true")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, err := st.db.GetKV(ctx, cronPausedKVNamespace, cronPausedKVKey)
	if err != nil {
		t.Fatalf("GetKV after Save(true): %v", err)
	}
	if rec.Value != "true" {
		t.Errorf("kv value after Save(true) = %q; want \"true\"", rec.Value)
	}

	if err := st.SaveCronPaused(false); err != nil {
		t.Fatalf("SaveCronPaused(false): %v", err)
	}
	if st.LoadCronPaused() {
		t.Errorf("after Save(false): Load = true; want false")
	}
	rec, err = st.db.GetKV(ctx, cronPausedKVNamespace, cronPausedKVKey)
	if err != nil {
		t.Fatalf("GetKV after Save(false): %v", err)
	}
	if rec.Value != "false" {
		t.Errorf("kv value after Save(false) = %q; want \"false\"", rec.Value)
	}
}

// TestCronPausedKV_LegacyFileMigration verifies the v0 → v1 migration:
// a marker file in `<configdir>/agents/cron_paused` with no kv row must
// flip the flag to true on first Load, mint the kv row, and remove the
// file. A second Load must see the kv row and report true without
// touching the (now absent) file.
func TestCronPausedKV_LegacyFileMigration(t *testing.T) {
	st := newAgentStore(t)

	// Seed the legacy file under the captured st.dir — bypassing
	// SaveCronPaused (which would write to kv first).
	legacyDir := filepath.Join(st.dir, "agents")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	legacyPath := filepath.Join(legacyDir, cronPausedFile)
	if err := os.WriteFile(legacyPath, nil, 0o644); err != nil {
		t.Fatalf("seed legacy file: %v", err)
	}

	// First Load: should pick up the file, write kv, remove the file.
	if !st.LoadCronPaused() {
		t.Errorf("first Load with legacy file = false; want true")
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Errorf("legacy file still present after migration: err=%v", err)
	}

	// kv row must exist and equal "true".
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, err := st.db.GetKV(ctx, cronPausedKVNamespace, cronPausedKVKey)
	if err != nil {
		t.Fatalf("GetKV after migration: %v", err)
	}
	if rec.Value != "true" {
		t.Errorf("migrated kv value = %q; want \"true\"", rec.Value)
	}

	// Second Load: kv-only path.
	if !st.LoadCronPaused() {
		t.Errorf("second Load (kv-only) = false; want true")
	}
}

// TestCronPausedKV_StaleLegacyFileCleanup verifies that a legacy file
// surviving alongside an authoritative kv row is removed on the next
// Load — protects against a v0 → v1 → v0 → v1 round-trip leaving a
// stale marker that would otherwise just sit there.
func TestCronPausedKV_StaleLegacyFileCleanup(t *testing.T) {
	st := newAgentStore(t)

	// Authoritative kv row says "false".
	if err := st.SaveCronPaused(false); err != nil {
		t.Fatalf("SaveCronPaused(false): %v", err)
	}

	// Plant a stale legacy file (lying — claiming "paused").
	legacyDir := filepath.Join(st.dir, "agents")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	legacyPath := filepath.Join(legacyDir, cronPausedFile)
	if err := os.WriteFile(legacyPath, nil, 0o644); err != nil {
		t.Fatalf("seed stale legacy file: %v", err)
	}

	// kv must win.
	if st.LoadCronPaused() {
		t.Errorf("Load with kv=false + stale file = true; want false (kv is authoritative)")
	}
	// And the stale file must have been swept.
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Errorf("stale legacy file not removed: err=%v", err)
	}
}

// TestCronPausedKV_GlobalScope pins the kv row's scope to global so a
// regression that downgrades it to local/machine surfaces in CI rather
// than leaking through to multi-device testing.
func TestCronPausedKV_GlobalScope(t *testing.T) {
	st := newAgentStore(t)
	if err := st.SaveCronPaused(true); err != nil {
		t.Fatalf("SaveCronPaused: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, err := st.db.GetKV(ctx, cronPausedKVNamespace, cronPausedKVKey)
	if err != nil {
		t.Fatalf("GetKV: %v", err)
	}
	if rec.Scope != store.KVScopeGlobal {
		t.Errorf("kv scope = %q; want %q (design doc §2.3)", rec.Scope, store.KVScopeGlobal)
	}
	if rec.Type != store.KVTypeString {
		t.Errorf("kv type = %q; want %q", rec.Type, store.KVTypeString)
	}
	if rec.Secret {
		t.Errorf("kv secret = true; want false (cron pause is operator metadata, not a credential)")
	}
}

// TestCronPausedKV_MalformedRowFailsClosed verifies that a row whose
// value is neither "true" nor "false" — a peer-replicated junk write,
// or someone manually editing the row — collapses to "paused" rather
// than silently letting cron run. Operator sees the Warn log and can
// repair the row.
func TestCronPausedKV_MalformedRowFailsClosed(t *testing.T) {
	st := newAgentStore(t)

	// Plant a malformed row directly via PutKV (the public API
	// accepts any string value; only LoadCronPaused validates).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bad := &store.KVRecord{
		Namespace: cronPausedKVNamespace,
		Key:       cronPausedKVKey,
		Value:     "garbage",
		Type:      store.KVTypeString,
		Scope:     store.KVScopeGlobal,
	}
	if _, err := st.db.PutKV(ctx, bad, store.KVPutOptions{}); err != nil {
		t.Fatalf("seed malformed kv row: %v", err)
	}

	if !st.LoadCronPaused() {
		t.Errorf("LoadCronPaused with malformed value = false; want true (fail closed)")
	}
}

// TestCronPausedKV_WrongTypeFailsClosed verifies the row-shape validation
// rejects a row whose type/scope/secret disagrees with the design contract.
// A row where value happens to be "false" but type is JSON should not be
// trusted to mean "not paused".
func TestCronPausedKV_WrongTypeFailsClosed(t *testing.T) {
	st := newAgentStore(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wrongType := &store.KVRecord{
		Namespace: cronPausedKVNamespace,
		Key:       cronPausedKVKey,
		Value:     "false",
		Type:      store.KVTypeJSON, // contract violation
		Scope:     store.KVScopeGlobal,
	}
	if _, err := st.db.PutKV(ctx, wrongType, store.KVPutOptions{}); err != nil {
		t.Fatalf("seed wrong-type kv row: %v", err)
	}

	if !st.LoadCronPaused() {
		t.Errorf("LoadCronPaused with type=JSON = false; want true (fail closed on shape mismatch)")
	}
}

// TestCronPausedKV_LegacyMigrationCollision exercises the IfMatchAny
// ErrETagMismatch branch end-to-end: a fresh install with a legacy
// marker, where a colliding kv row materialises between LoadCronPaused's
// initial GetKV(miss) and its IfMatchAny PutKV. Without a hook the
// branch is unreachable from a single-goroutine test (the GetKV miss
// and the PutKV are back-to-back inside one call), so we use the
// cronPausedMigrationTestHook injection point to slip a peer's "false"
// row into the table at exactly the right moment.
//
// Expected outcome: the IfMatchAny PutKV fails with ErrETagMismatch,
// LoadCronPaused re-reads the kv row, honours the peer's "false" over
// the legacy file's implicit "true", and removes the legacy file.
func TestCronPausedKV_LegacyMigrationCollision(t *testing.T) {
	st := newAgentStore(t)

	// Seed legacy file (v0 says "paused").
	legacyDir := filepath.Join(st.dir, "agents")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	legacyPath := filepath.Join(legacyDir, cronPausedFile)
	if err := os.WriteFile(legacyPath, nil, 0o644); err != nil {
		t.Fatalf("seed legacy file: %v", err)
	}

	// Inject a peer write between LoadCronPaused's GetKV miss and its
	// IfMatchAny PutKV. The hook fires once; we clear it after this
	// test to keep the package-level variable clean for the next test.
	hookFired := 0
	cronPausedMigrationTestHook = func() {
		hookFired++
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		peer := &store.KVRecord{
			Namespace: cronPausedKVNamespace,
			Key:       cronPausedKVKey,
			Value:     "false",
			Type:      store.KVTypeString,
			Scope:     store.KVScopeGlobal,
		}
		if _, err := st.db.PutKV(ctx, peer, store.KVPutOptions{}); err != nil {
			t.Fatalf("hook peer write: %v", err)
		}
	}
	t.Cleanup(func() { cronPausedMigrationTestHook = nil })

	// LoadCronPaused must hit the IfMatchAny branch, fail with
	// ErrETagMismatch, re-read, and honour the peer's "false".
	if st.LoadCronPaused() {
		t.Errorf("Load = true; want false (peer's kv=false wins via IfMatchAny collision-resolve)")
	}
	if hookFired != 1 {
		t.Errorf("migration hook fired %d times; want 1 (collision branch must run)", hookFired)
	}
	// Legacy file must be removed (kv is now authoritative).
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Errorf("legacy file not removed after collision-resolve: err=%v", err)
	}
}

// TestSetCronPaused_PersistFailureRevertsMemory pins the asymmetric
// pause/unpause ordering policy in Manager.SetCronPaused: a paused=true
// request must NOT leave m.cronPaused at true if the kv write fails.
// We trigger a write failure by closing the underlying store before
// the toggle.
func TestSetCronPaused_PersistFailureRevertsMemory(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("APPDATA", "")
	st, err := newStore(testLogger())
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	mgr := &Manager{
		store:  st,
		logger: testLogger(),
	}
	// cronPaused starts at false (default).

	// Force the underlying DB to error by closing it.
	_ = st.Close()

	if err := mgr.SetCronPaused(true); err == nil {
		t.Fatalf("SetCronPaused(true) on closed store: want error, got nil")
	}
	mgr.mu.Lock()
	gotMem := mgr.cronPaused
	mgr.mu.Unlock()
	if gotMem {
		t.Errorf("after failed SetCronPaused(true): m.cronPaused = true; want false (must revert)")
	}
}

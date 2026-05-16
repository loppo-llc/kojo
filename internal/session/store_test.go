package session

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/configdir"
	"github.com/loppo-llc/kojo/internal/store"
)

// kvTestStore opens kojo.db at a temp HOME so configdir.Path() resolves
// underneath it. The Phase 2c-2 slice 28 Save/Load contract is:
// kv-canonical, legacy file fallback on first Load, best-effort
// unlink afterwards. These tests exercise each branch.
func kvTestStore(t *testing.T) *store.Store {
	t.Helper()
	tmp := t.TempDir()
	// Pin every env source configdir.defaultPath consults to the
	// temp dir. XDG_CONFIG_HOME wins over HOME on Linux; APPDATA on
	// Windows. Setenv with the tmp value defeats both — we want the
	// resolved path to always sit under t.TempDir().
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)
	t.Setenv("APPDATA", tmp)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := store.Open(ctx, store.Options{ConfigDir: configdir.Path()})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func sessionTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func sampleInfo(id string, createdAt time.Time) SessionInfo {
	return SessionInfo{
		ID:        id,
		Tool:      "claude",
		WorkDir:   "/tmp/x",
		Args:      []string{"--no-color"},
		Status:    StatusExited,
		YoloMode:  true,
		CreatedAt: createdAt.Format(time.RFC3339),
	}
}

// TestStoreKV_RoundTrip verifies Save → Load round-trips through kv
// when the store handle is wired and no legacy file exists.
func TestStoreKV_RoundTrip(t *testing.T) {
	db := kvTestStore(t)
	st := newStore(sessionTestLogger(), db, configdir.V0Path())

	now := time.Now().UTC().Truncate(time.Second)
	want := []SessionInfo{
		sampleInfo("sess_a", now),
		sampleInfo("sess_b", now.Add(-time.Hour)),
	}
	st.Save(want)

	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2", len(got))
	}
	for i := range got {
		if got[i].ID != want[i].ID {
			t.Errorf("got[%d].ID = %q, want %q", i, got[i].ID, want[i].ID)
		}
	}

	// kv row must be JSON-typed and local-scoped per design.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, err := db.GetKV(ctx, sessionsKVNamespace, sessionsKVKey)
	if err != nil {
		t.Fatalf("GetKV: %v", err)
	}
	if rec.Type != store.KVTypeJSON {
		t.Errorf("kv type = %q; want %q", rec.Type, store.KVTypeJSON)
	}
	if rec.Scope != store.KVScopeLocal {
		t.Errorf("kv scope = %q; want %q", rec.Scope, store.KVScopeLocal)
	}

	// Legacy file must NOT exist on the kv-only path.
	if _, err := os.Stat(st.legacyPath); !os.IsNotExist(err) {
		t.Errorf("legacy file present after kv-only Save: %v", err)
	}
}

// TestStoreKV_MaxAgeFilter drops entries older than maxAge on Load.
func TestStoreKV_MaxAgeFilter(t *testing.T) {
	db := kvTestStore(t)
	st := newStore(sessionTestLogger(), db, configdir.V0Path())
	now := time.Now().UTC()
	stale := now.Add(-(maxAge + time.Hour))
	st.Save([]SessionInfo{
		sampleInfo("fresh", now),
		sampleInfo("stale", stale),
	})
	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].ID != "fresh" {
		t.Fatalf("filter dropped wrong row(s): %+v", got)
	}
}

// TestStoreKV_LegacyMigration verifies the v0 / pre-slice-28 fallback:
// kv miss + legacy sessions.json present → parse legacy, write kv,
// best-effort unlink legacy file. Subsequent Loads route through kv.
func TestStoreKV_LegacyMigration(t *testing.T) {
	db := kvTestStore(t)
	st := newStore(sessionTestLogger(), db, configdir.V0Path())

	// Seed a legacy file.
	if err := os.MkdirAll(filepath.Dir(st.legacyPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	seed := []SessionInfo{sampleInfo("seed_a", now)}
	body, err := json.Marshal(seed)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(st.legacyPath, body, 0o600); err != nil {
		t.Fatalf("seed legacy file: %v", err)
	}

	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].ID != "seed_a" {
		t.Fatalf("legacy migration returned %+v, want seed_a", got)
	}

	// kv must now have the row.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.GetKV(ctx, sessionsKVNamespace, sessionsKVKey); err != nil {
		t.Fatalf("GetKV after legacy migration: %v", err)
	}

	// Legacy file must be unlinked.
	if _, err := os.Stat(st.legacyPath); !os.IsNotExist(err) {
		t.Errorf("legacy file survived migration: %v", err)
	}

	// Re-Load comes from kv, returns the same.
	got2, err := st.Load()
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}
	if len(got2) != 1 || got2[0].ID != "seed_a" {
		t.Fatalf("post-migration Load returned %+v, want seed_a", got2)
	}
}

// TestStoreKV_StrayLegacyAfterKVHit drops a stray legacy file when kv
// already has the row (v1 → v0 → v1 round trip leftover).
func TestStoreKV_StrayLegacyAfterKVHit(t *testing.T) {
	db := kvTestStore(t)
	st := newStore(sessionTestLogger(), db, configdir.V0Path())

	// Populate kv via Save.
	now := time.Now().UTC().Truncate(time.Second)
	st.Save([]SessionInfo{sampleInfo("kv_winner", now)})

	// Plant a contradictory legacy file.
	if err := os.MkdirAll(filepath.Dir(st.legacyPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stray := []SessionInfo{sampleInfo("legacy_loser", now)}
	body, _ := json.Marshal(stray)
	if err := os.WriteFile(st.legacyPath, body, 0o600); err != nil {
		t.Fatalf("seed stray: %v", err)
	}

	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].ID != "kv_winner" {
		t.Fatalf("kv-hit branch returned %+v, want kv_winner", got)
	}
	if _, err := os.Stat(st.legacyPath); !os.IsNotExist(err) {
		t.Errorf("stray legacy file not unlinked after kv hit: %v", err)
	}
}

// TestStoreKV_MalformedRowNoLegacyReturnsError verifies the fail-
// closed posture when the kv row is unreadable AND there is no
// legacy file: Load returns a non-nil error so the caller's orphan
// cleanup does NOT proceed on the false assumption that the store
// is empty. (A clean (nil, nil) here would let the caller wipe
// every live tmux session because it thought "no sessions".)
func TestStoreKV_MalformedRowNoLegacyReturnsError(t *testing.T) {
	db := kvTestStore(t)
	st := newStore(sessionTestLogger(), db, configdir.V0Path())

	// Plant a malformed kv row, NO legacy file.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.PutKV(ctx, &store.KVRecord{
		Namespace: sessionsKVNamespace,
		Key:       sessionsKVKey,
		Value:     "garbage",
		Type:      store.KVTypeString, // wrong type
		Scope:     store.KVScopeLocal,
	}, store.KVPutOptions{}); err != nil {
		t.Fatalf("seed malformed kv row: %v", err)
	}

	got, err := st.Load()
	if err == nil {
		t.Fatalf("Load with malformed kv + no legacy: want error, got nil (returned %+v)", got)
	}
	if got != nil {
		t.Errorf("malformed-no-legacy Load returned %+v, want nil sessions", got)
	}
}

// TestStoreKV_ValidShapeUnparseableJSONFallsBackToLegacy covers the
// second malformed-kv branch: row shape passes the gate but the
// JSON body is garbage. Same posture as wrong-shape — fall back to
// legacy without unlinking.
func TestStoreKV_ValidShapeUnparseableJSONFallsBackToLegacy(t *testing.T) {
	db := kvTestStore(t)
	st := newStore(sessionTestLogger(), db, configdir.V0Path())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.PutKV(ctx, &store.KVRecord{
		Namespace: sessionsKVNamespace,
		Key:       sessionsKVKey,
		Value:     "not-json{",
		Type:      store.KVTypeJSON, // shape passes
		Scope:     store.KVScopeLocal,
	}, store.KVPutOptions{}); err != nil {
		t.Fatalf("seed valid-shape unparseable kv row: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(st.legacyPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	body, _ := json.Marshal([]SessionInfo{sampleInfo("legacy_keeps_data", now)})
	if err := os.WriteFile(st.legacyPath, body, 0o600); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].ID != "legacy_keeps_data" {
		t.Fatalf("got %+v, want legacy_keeps_data", got)
	}
	if _, err := os.Stat(st.legacyPath); err != nil {
		t.Errorf("legacy file unlinked despite valid-shape unparseable kv row: %v", err)
	}
}

// TestStoreKV_CollisionWinnerValidUnlinksLegacy uses the test hook
// to force the IfMatchAny PutKV into ErrETagMismatch. The injected
// winner row is well-formed, so the legacy file must be unlinked.
func TestStoreKV_CollisionWinnerValidUnlinksLegacy(t *testing.T) {
	db := kvTestStore(t)
	st := newStore(sessionTestLogger(), db, configdir.V0Path())

	if err := os.MkdirAll(filepath.Dir(st.legacyPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	body, _ := json.Marshal([]SessionInfo{sampleInfo("loser_legacy", now)})
	if err := os.WriteFile(st.legacyPath, body, 0o600); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	winner, _ := json.Marshal([]SessionInfo{sampleInfo("winner_kv", now)})
	sessionsKVCollisionTestHook = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := db.PutKV(ctx, &store.KVRecord{
			Namespace: sessionsKVNamespace,
			Key:       sessionsKVKey,
			Value:     string(winner),
			Type:      store.KVTypeJSON,
			Scope:     store.KVScopeLocal,
		}, store.KVPutOptions{}); err != nil {
			t.Fatalf("hook PutKV: %v", err)
		}
	}
	t.Cleanup(func() { sessionsKVCollisionTestHook = nil })

	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Load must return the canonical winner view — returning the
	// loser legacy snapshot would let the next Save() overwrite the
	// fresher winner row with stale data.
	if len(got) != 1 || got[0].ID != "winner_kv" {
		t.Fatalf("got %+v, want winner_kv (canonical winner)", got)
	}
	// Legacy file must be unlinked because the winner row validates.
	if _, err := os.Stat(st.legacyPath); !os.IsNotExist(err) {
		t.Errorf("legacy file survived after valid-winner collision: %v", err)
	}
}

// TestStoreKV_CollisionWinnerMalformedKeepsLegacy: same hook, but the
// injected winner row is malformed (wrong type). The legacy file
// must NOT be unlinked.
func TestStoreKV_CollisionWinnerMalformedKeepsLegacy(t *testing.T) {
	db := kvTestStore(t)
	st := newStore(sessionTestLogger(), db, configdir.V0Path())

	if err := os.MkdirAll(filepath.Dir(st.legacyPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	body, _ := json.Marshal([]SessionInfo{sampleInfo("survivor", now)})
	if err := os.WriteFile(st.legacyPath, body, 0o600); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	sessionsKVCollisionTestHook = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := db.PutKV(ctx, &store.KVRecord{
			Namespace: sessionsKVNamespace,
			Key:       sessionsKVKey,
			Value:     "junk",
			Type:      store.KVTypeString, // wrong shape
			Scope:     store.KVScopeLocal,
		}, store.KVPutOptions{}); err != nil {
			t.Fatalf("hook PutKV: %v", err)
		}
	}
	t.Cleanup(func() { sessionsKVCollisionTestHook = nil })

	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].ID != "survivor" {
		t.Fatalf("got %+v, want survivor", got)
	}
	// Legacy file must STILL exist because the winner row is malformed.
	if _, err := os.Stat(st.legacyPath); err != nil {
		t.Errorf("legacy file unlinked despite malformed winner: %v", err)
	}
}

// TestStoreKV_NilDB tolerates a nil store (test scaffolding posture).
func TestStoreKV_NilDB(t *testing.T) {
	st := newStore(sessionTestLogger(), nil, configdir.V0Path())
	st.Save([]SessionInfo{sampleInfo("x", time.Now())}) // must not panic
	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load with nil db: %v", err)
	}
	if got != nil {
		t.Errorf("Load with nil db returned %v, want nil", got)
	}
}

// TestStoreKV_AgeFilterKeepsRunningTmux verifies the age-cutoff escape
// hatch: a session row older than maxAge is normally dropped, BUT a
// row still reporting StatusRunning + a non-empty TmuxSessionName
// survives the filter. Dropping it would hide it from restoreSession
// (no reattach) AND leave its tmux pane out of the known set
// cleanupOrphanedTmuxSessions consults — the v1 binary would then
// kill a live v0 pane as orphan, the exact regression we are guarding
// against on v0 → v1 cutover.
func TestStoreKV_AgeFilterKeepsRunningTmux(t *testing.T) {
	db := kvTestStore(t)
	st := newStore(sessionTestLogger(), db, configdir.V0Path())

	old := time.Now().Add(-(maxAge + 30*24*time.Hour)).UTC().Truncate(time.Second)
	infos := []SessionInfo{
		{
			ID:              "live_running",
			Tool:            "claude",
			WorkDir:         "/tmp/x",
			Status:          StatusRunning,
			CreatedAt:       old.Format(time.RFC3339),
			TmuxSessionName: "kojo_live_running",
		},
		{
			// Same age, but no tmux session name → can drop.
			ID:        "old_archived",
			Tool:      "claude",
			WorkDir:   "/tmp/x",
			Status:    StatusExited,
			CreatedAt: old.Format(time.RFC3339),
		},
		{
			// Same age, status=running but no tmux name → can drop
			// (no live pane to reattach).
			ID:        "old_running_no_tmux",
			Tool:      "claude",
			WorkDir:   "/tmp/x",
			Status:    StatusRunning,
			CreatedAt: old.Format(time.RFC3339),
		},
	}
	if err := os.MkdirAll(filepath.Dir(st.legacyPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body, _ := json.Marshal(infos)
	if err := os.WriteFile(st.legacyPath, body, 0o600); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].ID != "live_running" {
		t.Fatalf("age filter dropped/kept wrong rows: %+v", got)
	}
}

// TestStoreKV_V0FallbackDisabled confirms that newStore with
// v0LegacyDir="" never reads the v0 directory, even if it has a
// well-formed sessions.json. This is the --fresh / no-migration
// posture: v0 data must be invisible to a runtime that explicitly
// opted out.
func TestStoreKV_V0FallbackDisabled(t *testing.T) {
	db := kvTestStore(t)
	st := newStore(sessionTestLogger(), db, "")

	v0Dir := configdir.V0Path()
	if err := os.MkdirAll(v0Dir, 0o755); err != nil {
		t.Fatalf("mkdir v0: %v", err)
	}
	v0Sessions := filepath.Join(v0Dir, sessionsFile)
	now := time.Now().UTC().Truncate(time.Second)
	body, _ := json.Marshal([]SessionInfo{sampleInfo("v0_must_not_load", now)})
	if err := os.WriteFile(v0Sessions, body, 0o600); err != nil {
		t.Fatalf("seed v0 legacy: %v", err)
	}

	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != nil {
		t.Fatalf("v0 fallback fired despite being disabled: %+v", got)
	}
	// kv must still be empty — Load must not have mirrored v0 into kv.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.GetKV(ctx, sessionsKVNamespace, sessionsKVKey); err == nil {
		t.Errorf("kv row materialized despite v0 fallback being disabled")
	}
	// v0 file must still exist (we never touch it from this code path).
	if _, err := os.Stat(v0Sessions); err != nil {
		t.Errorf("v0 legacy file disappeared: %v", err)
	}
}

// TestStoreKV_RemoveLegacyRefusesWhenPathsCollapse hardens the unlink
// guard: if a pathological override pointed v1's legacyPath at the v0
// dir, removeLegacyIfPresent must refuse to delete — even on the
// kv-canonical path. We seed kv directly and trigger the kv-hit branch
// to exercise removeLegacyIfPresent.
func TestStoreKV_RemoveLegacyRefusesWhenPathsCollapse(t *testing.T) {
	db := kvTestStore(t)
	st := newStore(sessionTestLogger(), db, configdir.V0Path())
	// Force the pathological collapse.
	st.legacyPath = st.v0LegacyPath

	if err := os.MkdirAll(filepath.Dir(st.legacyPath), 0o755); err != nil {
		t.Fatalf("mkdir collapsed: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	body, _ := json.Marshal([]SessionInfo{sampleInfo("v0_protected", now)})
	if err := os.WriteFile(st.legacyPath, body, 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	// Populate kv so the kv-hit branch triggers removeLegacyIfPresent.
	st.Save([]SessionInfo{sampleInfo("kv_canonical", now)})

	if _, err := st.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// File on the collapsed path MUST still exist.
	if _, err := os.Stat(st.legacyPath); err != nil {
		t.Errorf("collapsed legacy file deleted despite v0-protection guard: %v", err)
	}
}

// TestStoreKV_V0LegacyMigration covers the v0 → v1 upgrade path: the
// v1 dir is empty, the v0 dir (kojo) carries the pre-cutover
// sessions.json. Load must:
//
//  1. mirror the v0 file into kv,
//  2. NOT touch the v0 dir's sessions.json (rollback to v0 needs it,
//     `kojo --clean v0` is the only sanctioned remover),
//  3. NOT create a stray copy in the v1 dir.
//
// This is the path that makes tmux session reattach work across the
// v0 → v1 cutover — TmuxSessionName lives inside SessionInfo and the
// `kojo_<id>` naming is wire-compatible between majors.
func TestStoreKV_V0LegacyMigration(t *testing.T) {
	db := kvTestStore(t)
	st := newStore(sessionTestLogger(), db, configdir.V0Path())

	if st.v0LegacyPath == "" || st.v0LegacyPath == st.legacyPath {
		t.Fatalf("v0LegacyPath collapsed to v1 path; configdir helpers misconfigured: v0=%q v1=%q",
			st.v0LegacyPath, st.legacyPath)
	}

	// Seed only the v0 dir's sessions.json; v1 dir is empty.
	if err := os.MkdirAll(filepath.Dir(st.v0LegacyPath), 0o755); err != nil {
		t.Fatalf("mkdir v0: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	seed := []SessionInfo{
		{
			ID:              "v0_sess",
			Tool:            "claude",
			WorkDir:         "/tmp/x",
			Status:          StatusRunning,
			CreatedAt:       now.Format(time.RFC3339),
			TmuxSessionName: "kojo_v0_sess",
		},
	}
	body, err := json.Marshal(seed)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(st.v0LegacyPath, body, 0o600); err != nil {
		t.Fatalf("seed v0 legacy file: %v", err)
	}

	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].ID != "v0_sess" {
		t.Fatalf("v0 fallback returned %+v, want v0_sess", got)
	}
	if got[0].TmuxSessionName != "kojo_v0_sess" {
		t.Errorf("tmux session name lost across v0 fallback: %q", got[0].TmuxSessionName)
	}

	// kv must now hold the row (next Load comes from kv).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.GetKV(ctx, sessionsKVNamespace, sessionsKVKey); err != nil {
		t.Fatalf("GetKV after v0 fallback: %v", err)
	}

	// v0 file must STILL exist — `kojo --clean v0` owns its lifecycle.
	if _, err := os.Stat(st.v0LegacyPath); err != nil {
		t.Errorf("v0 legacy file unlinked by Load: %v", err)
	}
	// v1 file must NOT have been created.
	if _, err := os.Stat(st.legacyPath); !os.IsNotExist(err) {
		t.Errorf("v1 legacy file materialized as a side effect: %v", err)
	}

	// Re-Load comes from kv; v0 file still untouched.
	got2, err := st.Load()
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}
	if len(got2) != 1 || got2[0].ID != "v0_sess" {
		t.Fatalf("post-mirror Load returned %+v, want v0_sess", got2)
	}
	if _, err := os.Stat(st.v0LegacyPath); err != nil {
		t.Errorf("v0 legacy file disappeared on second Load: %v", err)
	}
}

// TestStoreKV_V1OverridesV0OnLegacyMigration verifies search order:
// when both v1 and v0 dirs have sessions.json, the v1 file wins and
// the v0 file is left alone.
func TestStoreKV_V1OverridesV0OnLegacyMigration(t *testing.T) {
	db := kvTestStore(t)
	st := newStore(sessionTestLogger(), db, configdir.V0Path())

	if err := os.MkdirAll(filepath.Dir(st.legacyPath), 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(st.v0LegacyPath), 0o755); err != nil {
		t.Fatalf("mkdir v0: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	v1Body, _ := json.Marshal([]SessionInfo{sampleInfo("v1_winner", now)})
	v0Body, _ := json.Marshal([]SessionInfo{sampleInfo("v0_loser", now)})
	if err := os.WriteFile(st.legacyPath, v1Body, 0o600); err != nil {
		t.Fatalf("seed v1: %v", err)
	}
	if err := os.WriteFile(st.v0LegacyPath, v0Body, 0o600); err != nil {
		t.Fatalf("seed v0: %v", err)
	}

	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].ID != "v1_winner" {
		t.Fatalf("v1 should win, got %+v", got)
	}
	// v1 file unlinked (kv canonical), v0 file preserved.
	if _, err := os.Stat(st.legacyPath); !os.IsNotExist(err) {
		t.Errorf("v1 legacy file survived after kv mirror: %v", err)
	}
	if _, err := os.Stat(st.v0LegacyPath); err != nil {
		t.Errorf("v0 legacy file removed despite being the loser: %v", err)
	}
}

// TestStoreKV_MalformedKVFallsBackToV0Legacy: the kv row is wrong-
// shaped, v1 dir has nothing, v0 dir has the file. loadLegacyOnly
// must reach the v0 file and return its data (without unlinking
// either — both are repair surfaces).
func TestStoreKV_MalformedKVFallsBackToV0Legacy(t *testing.T) {
	db := kvTestStore(t)
	st := newStore(sessionTestLogger(), db, configdir.V0Path())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.PutKV(ctx, &store.KVRecord{
		Namespace: sessionsKVNamespace,
		Key:       sessionsKVKey,
		Value:     "garbage",
		Type:      store.KVTypeString, // wrong shape
		Scope:     store.KVScopeLocal,
	}, store.KVPutOptions{}); err != nil {
		t.Fatalf("seed malformed kv row: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(st.v0LegacyPath), 0o755); err != nil {
		t.Fatalf("mkdir v0: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	body, _ := json.Marshal([]SessionInfo{sampleInfo("v0_repair", now)})
	if err := os.WriteFile(st.v0LegacyPath, body, 0o600); err != nil {
		t.Fatalf("seed v0 legacy: %v", err)
	}

	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].ID != "v0_repair" {
		t.Fatalf("malformed-kv → v0 fallback returned %+v, want v0_repair", got)
	}
	// Both legacy locations remain on disk: the malformed kv row is
	// in the way (v1 file would be created here only via Save, never
	// happens) and the v0 file is never owned by us.
	if _, err := os.Stat(st.v0LegacyPath); err != nil {
		t.Errorf("v0 legacy file disappeared during malformed-kv fallback: %v", err)
	}
}

// TestStoreKV_MalformedRowFallsBackToLegacy verifies the fail-closed
// posture: a kv row that violates the row-shape gate (wrong type or
// scope, or secret=true) is treated as malformed; Load falls back to
// the legacy file without unlinking it, so the operator can repair
// (delete the row) and let the next Load mirror legacy → kv normally.
func TestStoreKV_MalformedRowFallsBackToLegacy(t *testing.T) {
	db := kvTestStore(t)
	st := newStore(sessionTestLogger(), db, configdir.V0Path())

	// Plant a malformed kv row: wrong type (string instead of json).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.PutKV(ctx, &store.KVRecord{
		Namespace: sessionsKVNamespace,
		Key:       sessionsKVKey,
		Value:     "garbage",
		Type:      store.KVTypeString, // wrong type
		Scope:     store.KVScopeLocal,
	}, store.KVPutOptions{}); err != nil {
		t.Fatalf("seed malformed kv row: %v", err)
	}

	// Plant a valid legacy file.
	if err := os.MkdirAll(filepath.Dir(st.legacyPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	body, _ := json.Marshal([]SessionInfo{sampleInfo("legacy_survives", now)})
	if err := os.WriteFile(st.legacyPath, body, 0o600); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].ID != "legacy_survives" {
		t.Fatalf("malformed-kv fallback returned %+v, want legacy_survives", got)
	}
	// Legacy file must NOT be unlinked while the malformed row is
	// still in the way.
	if _, err := os.Stat(st.legacyPath); err != nil {
		t.Errorf("legacy file unlinked despite malformed kv row: %v", err)
	}
}

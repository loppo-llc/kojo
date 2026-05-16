package agent

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// markerKVTestSetup opens kojo.db at a temp HOME, registers the
// global store handle (so readMarker / writeMarker route to kv), and
// seeds an agent row so any future agent_messages FK is satisfied
// (not used by the marker tests but harmless).
func markerKVTestSetup(t *testing.T, agentID string) *agentStore {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("APPDATA", "")
	st, err := newStore(testLogger())
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if agentID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rec := &store.AgentRecord{ID: agentID, Name: agentID}
		if _, err := st.db.InsertAgent(ctx, rec, store.AgentInsertOptions{}); err != nil {
			t.Fatalf("seed agent: %v", err)
		}
	}
	return st
}

// TestAutosummaryMarkerKV_RoundTrip verifies write→read round-trips
// through kv (legacy file path is never created when the kv store is
// available).
func TestAutosummaryMarkerKV_RoundTrip(t *testing.T) {
	st := markerKVTestSetup(t, "ag")
	now := time.Date(2026, 5, 5, 12, 34, 56, 0, time.UTC)

	want := autoSummaryMarker{
		LastAt:   now,
		LastHash: "deadbeef",
		LastN:    7,
	}
	writeMarker("ag", want, testLogger())

	got := readMarker("ag")
	if !got.LastAt.Equal(want.LastAt) {
		t.Errorf("LastAt = %v; want %v", got.LastAt, want.LastAt)
	}
	if got.LastHash != want.LastHash {
		t.Errorf("LastHash = %q; want %q", got.LastHash, want.LastHash)
	}
	if got.LastN != want.LastN {
		t.Errorf("LastN = %d; want %d", got.LastN, want.LastN)
	}

	// kv row must be JSON-typed and global-scoped per design doc.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, err := st.db.GetKV(ctx, autosummaryKVNamespace, "ag")
	if err != nil {
		t.Fatalf("GetKV: %v", err)
	}
	if rec.Type != store.KVTypeJSON {
		t.Errorf("kv type = %q; want %q", rec.Type, store.KVTypeJSON)
	}
	if rec.Scope != store.KVScopeGlobal {
		t.Errorf("kv scope = %q; want %q", rec.Scope, store.KVScopeGlobal)
	}

	// No legacy file should have been created on the kv path.
	if _, err := os.Stat(markerLegacyPath("ag")); !os.IsNotExist(err) {
		t.Errorf("legacy file created on kv-only write: err=%v", err)
	}
}

// TestAutosummaryMarkerKV_LegacyMigration verifies the v0 → v1
// migration: a marker file present with no kv row mirrors into kv on
// first read and unlinks the file. The mirrored row contains the same
// JSON payload byte-for-byte so the next read returns the same struct.
func TestAutosummaryMarkerKV_LegacyMigration(t *testing.T) {
	st := markerKVTestSetup(t, "ag")
	if err := os.MkdirAll(agentDir("ag"), 0o755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}

	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	seed := autoSummaryMarker{
		LastAt:   now,
		LastHash: "legacyhash",
		LastN:    3,
	}
	data, err := json.Marshal(seed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(markerLegacyPath("ag"), data, 0o644); err != nil {
		t.Fatalf("seed legacy file: %v", err)
	}

	// First read must pick up the file, write kv, unlink the file.
	got := readMarker("ag")
	if got.LastHash != "legacyhash" || got.LastN != 3 || !got.LastAt.Equal(now) {
		t.Errorf("readMarker after migration = %+v; want %+v", got, seed)
	}
	if _, err := os.Stat(markerLegacyPath("ag")); !os.IsNotExist(err) {
		t.Errorf("legacy file not unlinked after migration: err=%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := st.db.GetKV(ctx, autosummaryKVNamespace, "ag"); err != nil {
		t.Errorf("kv row not present after migration: %v", err)
	}

	// Second read goes through kv only.
	got2 := readMarker("ag")
	if got2.LastHash != "legacyhash" {
		t.Errorf("second read (kv-only): LastHash = %q; want %q", got2.LastHash, "legacyhash")
	}
}

// TestAutosummaryMarkerKV_DeleteWipesBoth verifies deleteMarker drops
// both the kv row and the legacy file. Used by reset / agent cleanup.
func TestAutosummaryMarkerKV_DeleteWipesBoth(t *testing.T) {
	st := markerKVTestSetup(t, "ag")
	if err := os.MkdirAll(agentDir("ag"), 0o755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}

	// Seed both kv (via writeMarker) and a stray legacy file.
	writeMarker("ag", autoSummaryMarker{LastHash: "x", LastN: 1, LastAt: time.Now()}, testLogger())
	if err := os.WriteFile(markerLegacyPath("ag"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed stray legacy file: %v", err)
	}

	deleteMarker("ag", testLogger())

	// kv row gone.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := st.db.GetKV(ctx, autosummaryKVNamespace, "ag"); err == nil {
		t.Errorf("kv row still present after deleteMarker")
	}
	// File gone.
	if _, err := os.Stat(markerLegacyPath("ag")); !os.IsNotExist(err) {
		t.Errorf("legacy file still present after deleteMarker: err=%v", err)
	}

	// readMarker returns zero-value after delete.
	got := readMarker("ag")
	if got != (autoSummaryMarker{}) {
		t.Errorf("readMarker after delete = %+v; want zero", got)
	}
}

// TestAutosummaryMarkerKV_CorruptKVRowZeroFallback verifies a kv row
// containing invalid JSON yields a zero marker (not a panic) and is
// quietly clobbered by the next writeMarker. Important: bad data
// shouldn't permanently break PreCompact summarisation.
func TestAutosummaryMarkerKV_CorruptKVRowZeroFallback(t *testing.T) {
	st := markerKVTestSetup(t, "ag")

	// Plant a row whose value is garbage but still JSON-typed (to
	// pass any future shape check that compares Type).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bad := &store.KVRecord{
		Namespace: autosummaryKVNamespace,
		Key:       "ag",
		Value:     "{not valid json",
		Type:      store.KVTypeJSON,
		Scope:     store.KVScopeGlobal,
	}
	if _, err := st.db.PutKV(ctx, bad, store.KVPutOptions{}); err != nil {
		t.Fatalf("seed corrupt row: %v", err)
	}

	got := readMarker("ag")
	if got != (autoSummaryMarker{}) {
		t.Errorf("readMarker on corrupt row = %+v; want zero", got)
	}

	// A subsequent write should clobber the bad row, not error.
	writeMarker("ag", autoSummaryMarker{LastHash: "fresh", LastN: 1, LastAt: time.Now()}, testLogger())
	got2 := readMarker("ag")
	if got2.LastHash != "fresh" {
		t.Errorf("readMarker after re-write = %+v; want LastHash=fresh", got2)
	}
}

// TestAutosummaryMarkerKV_CopyMarker pins the fork-time copy: dst
// inherits src's marker; src remains unaffected.
func TestAutosummaryMarkerKV_CopyMarker(t *testing.T) {
	st := markerKVTestSetup(t, "src")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := st.db.InsertAgent(ctx, &store.AgentRecord{ID: "dst", Name: "dst"}, store.AgentInsertOptions{}); err != nil {
		t.Fatalf("seed dst agent: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	writeMarker("src", autoSummaryMarker{LastAt: now, LastHash: "srchash", LastN: 5}, testLogger())

	if err := copyMarker("src", "dst", testLogger()); err != nil {
		t.Fatalf("copyMarker: %v", err)
	}

	dst := readMarker("dst")
	if dst.LastHash != "srchash" || dst.LastN != 5 || !dst.LastAt.Equal(now) {
		t.Errorf("dst marker = %+v; want LastAt=%v LastHash=srchash LastN=5", dst, now)
	}
	src := readMarker("src")
	if src.LastHash != "srchash" {
		t.Errorf("src marker mutated by copy: %+v", src)
	}
}

// TestAutosummaryMarkerKV_CopyMarkerNoSrc verifies that copying from
// an agent that has no marker is a no-op — the dst doesn't end up
// with a zero-row that would otherwise churn the kv etag on the
// dst's first real write.
func TestAutosummaryMarkerKV_CopyMarkerNoSrc(t *testing.T) {
	st := markerKVTestSetup(t, "src")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := st.db.InsertAgent(ctx, &store.AgentRecord{ID: "dst", Name: "dst"}, store.AgentInsertOptions{}); err != nil {
		t.Fatalf("seed dst agent: %v", err)
	}

	// src has no marker.
	if err := copyMarker("src", "dst", testLogger()); err != nil {
		t.Fatalf("copyMarker (empty src): %v", err)
	}

	if _, err := st.db.GetKV(ctx, autosummaryKVNamespace, "dst"); err == nil {
		t.Errorf("dst kv row created from empty src; expected no-op")
	}
}

// TestAutosummaryMarkerKV_RowShapeMismatchKeepsLegacyFile pins the
// defensive behaviour added in round-1 review: a kv row whose shape
// doesn't match the contract (wrong type / scope / secret flag) must
// NOT cause the legacy file to be unlinked, otherwise an operator
// could lose the only valid copy of the marker. The function returns
// zero (first-run-equivalent) for this fire and leaves the file in
// place for an operator (or a subsequent successful kv write) to
// recover.
func TestAutosummaryMarkerKV_RowShapeMismatchKeepsLegacyFile(t *testing.T) {
	st := markerKVTestSetup(t, "ag")
	if err := os.MkdirAll(agentDir("ag"), 0o755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}

	// Seed a valid legacy file so we can detect post-call deletion.
	now := time.Now().UTC().Truncate(time.Second)
	good := autoSummaryMarker{LastAt: now, LastHash: "filevalid", LastN: 3}
	data, err := json.Marshal(good)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(markerLegacyPath("ag"), data, 0o644); err != nil {
		t.Fatalf("seed legacy file: %v", err)
	}

	// Plant a wrong-type row in kv.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bad := &store.KVRecord{
		Namespace: autosummaryKVNamespace,
		Key:       "ag",
		Value:     string(data),
		Type:      store.KVTypeString, // contract violation: must be json
		Scope:     store.KVScopeGlobal,
	}
	if _, err := st.db.PutKV(ctx, bad, store.KVPutOptions{}); err != nil {
		t.Fatalf("seed bad kv row: %v", err)
	}

	got := readMarker("ag")
	if got != (autoSummaryMarker{}) {
		t.Errorf("readMarker on shape-mismatch row = %+v; want zero", got)
	}
	if _, statErr := os.Stat(markerLegacyPath("ag")); statErr != nil {
		t.Errorf("legacy file removed despite shape-mismatch row: err=%v", statErr)
	}
}

// TestAutosummaryMarkerKV_WriteMarkerErrSurfaces verifies the error-
// returning write path used by copyMarker (and any future caller that
// needs to fail loud) propagates kv failures. Closing the store is
// the cleanest way to guarantee a kv-side failure under test.
func TestAutosummaryMarkerKV_WriteMarkerErrSurfaces(t *testing.T) {
	st := markerKVTestSetup(t, "ag")
	_ = st.Close()

	err := writeMarkerErr("ag", autoSummaryMarker{LastAt: time.Now(), LastHash: "x", LastN: 1})
	if err == nil {
		t.Errorf("writeMarkerErr on closed store: want error, got nil")
	}
}

// TestAutosummaryMarkerKV_ConcurrentMigratorRaceRetry exercises the
// retry path in readMarker / readMarkerErr that handles the (kv miss
// → file ENOENT) race: a concurrent migrator (peer replication /
// second reader on the same daemon) mirrored the legacy file into kv
// and unlinked it between our GetKV and our os.ReadFile. The hook
// fires AFTER the initial GetKV miss and BEFORE the file read, so
// the kv-row insertion lands inside the racy window. The retry must
// pick up the now-present row instead of falling through to "fresh
// install" zero.
func TestAutosummaryMarkerKV_ConcurrentMigratorRaceRetry(t *testing.T) {
	st := markerKVTestSetup(t, "ag")
	if err := os.MkdirAll(agentDir("ag"), 0o755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}

	want := autoSummaryMarker{LastAt: time.Now().UTC().Truncate(time.Second), LastHash: "racewinner", LastN: 4}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	hookFired := 0
	markerKVMigrationTestHook = func() {
		hookFired++
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rec := &store.KVRecord{
			Namespace: autosummaryKVNamespace,
			Key:       "ag",
			Value:     string(data),
			Type:      store.KVTypeJSON,
			Scope:     store.KVScopeGlobal,
		}
		if _, err := st.db.PutKV(ctx, rec, store.KVPutOptions{}); err != nil {
			t.Fatalf("hook seed kv: %v", err)
		}
	}
	t.Cleanup(func() { markerKVMigrationTestHook = nil })

	got := readMarker("ag")
	if got.LastHash != "racewinner" {
		t.Errorf("readMarker (race retry) = %+v; want LastHash=racewinner", got)
	}
	if hookFired != 1 {
		t.Errorf("migration hook fired %d times; want 1 (retry branch must run)", hookFired)
	}
}

// TestAutosummaryMarkerKV_ReadMarkerErrRaceRetry mirrors the race-
// retry test above for the error-strict path used by copyMarker.
// readMarkerErr must NOT return ErrNotFound when a concurrent
// migrator made the row appear between its GetKV miss and the file
// read.
func TestAutosummaryMarkerKV_ReadMarkerErrRaceRetry(t *testing.T) {
	st := markerKVTestSetup(t, "ag")
	if err := os.MkdirAll(agentDir("ag"), 0o755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}

	want := autoSummaryMarker{LastAt: time.Now().UTC().Truncate(time.Second), LastHash: "errsidewinner", LastN: 9}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	hookFired := 0
	markerKVMigrationTestHook = func() {
		hookFired++
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rec := &store.KVRecord{
			Namespace: autosummaryKVNamespace,
			Key:       "ag",
			Value:     string(data),
			Type:      store.KVTypeJSON,
			Scope:     store.KVScopeGlobal,
		}
		if _, err := st.db.PutKV(ctx, rec, store.KVPutOptions{}); err != nil {
			t.Fatalf("hook seed kv: %v", err)
		}
	}
	t.Cleanup(func() { markerKVMigrationTestHook = nil })

	got, gerr := readMarkerErr("ag")
	if gerr != nil {
		t.Fatalf("readMarkerErr (race retry) returned err: %v", gerr)
	}
	if got.LastHash != "errsidewinner" {
		t.Errorf("readMarkerErr (race retry) = %+v; want LastHash=errsidewinner", got)
	}
	if hookFired != 1 {
		t.Errorf("migration hook fired %d times; want 1 (retry branch must run)", hookFired)
	}
}

// TestAutosummaryMarkerKV_CopyMarkerSrcReadFailureSurfaces verifies
// the fork-side copy now propagates source-side read failures rather
// than silently dropping the marker. We force a read failure by
// closing the store (which clears the global handle and routes to
// the legacy-only fallback) AND planting a corrupt legacy file —
// readMarkerErr's legacy-only path then fails on json.Unmarshal.
func TestAutosummaryMarkerKV_CopyMarkerSrcReadFailureSurfaces(t *testing.T) {
	st := markerKVTestSetup(t, "src")
	if err := os.MkdirAll(agentDir("src"), 0o755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}
	// Plant a corrupt legacy file before closing the store. After
	// Close the global handle is nil → readMarkerErr takes the
	// legacy-only path → json.Unmarshal fails → error surfaces.
	if err := os.WriteFile(markerLegacyPath("src"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("plant corrupt legacy: %v", err)
	}
	_ = st.Close()

	if err := copyMarker("src", "dst", testLogger()); err == nil {
		t.Errorf("copyMarker with corrupt src marker: want error, got nil")
	}
}

// TestAutosummaryMarkerKV_LegacyOnlyFallback covers the test-fixture
// branch where readMarker / writeMarker run with no global store
// (e.g. tests that build a *Manager by hand without NewManager).
// The legacy file path must still work so older test scaffolding
// doesn't break under the cutover.
func TestAutosummaryMarkerKV_LegacyOnlyFallback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("APPDATA", "")

	// No NewManager call: globalStore stays nil.
	if getGlobalStore() != nil {
		t.Skip("global store unexpectedly set; another test leaked state")
	}

	if err := os.MkdirAll(agentDir("legacy"), 0o755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}
	writeMarker("legacy", autoSummaryMarker{LastHash: "fileonly", LastN: 2, LastAt: time.Now()}, testLogger())

	// File must exist on the legacy path.
	if _, err := os.Stat(markerLegacyPath("legacy")); err != nil {
		t.Errorf("legacy file not created in legacy-only fallback: %v", err)
	}

	got := readMarker("legacy")
	if got.LastHash != "fileonly" {
		t.Errorf("readMarker (legacy-only) = %+v; want LastHash=fileonly", got)
	}
}

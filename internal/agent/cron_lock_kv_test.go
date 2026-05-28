package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// TestAcquireCronLockDB_FirstFireSucceedsAndStampsRow exercises the
// first-fire branch: no kv row yet → PutKV runs, returns true, and the
// row records the supplied `now` as the last-fire millis. The next
// helper / agent looks for that row directly so a regression that
// silently no-ops the PutKV would surface here.
func TestAcquireCronLockDB_FirstFireSucceedsAndStampsRow(t *testing.T) {
	st := newAgentStore(t)
	now := int64(1_700_000_000_000)

	if ok, _ := acquireCronLockDB(st.db, "ag", now); !ok {
		t.Fatalf("first acquire: got false, want true")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, err := st.db.GetKV(ctx, cronLockKVNamespace, cronLockKVKey("ag"))
	if err != nil {
		t.Fatalf("GetKV after acquire: %v", err)
	}
	if rec.Value != strconv.FormatInt(now, 10) {
		t.Errorf("kv value = %q; want %q", rec.Value, strconv.FormatInt(now, 10))
	}
	if rec.Type != store.KVTypeString {
		t.Errorf("kv type = %q; want %q", rec.Type, store.KVTypeString)
	}
	if rec.Scope != store.KVScopeMachine {
		t.Errorf("kv scope = %q; want %q (per-host throttle, design doc §2.3)", rec.Scope, store.KVScopeMachine)
	}
	if rec.Secret {
		t.Errorf("kv secret = true; want false (operator metadata, not credential)")
	}
}

// TestAcquireCronLockDB_WithinWindowReturnsFalse pins the throttle
// behaviour: a second acquire within cronMinInterval after the first
// one must report "don't fire" and leave the row's value unchanged
// (no double-bump that would extend the window past its design
// duration).
func TestAcquireCronLockDB_WithinWindowReturnsFalse(t *testing.T) {
	st := newAgentStore(t)
	first := int64(1_700_000_000_000)
	if ok, _ := acquireCronLockDB(st.db, "ag", first); !ok {
		t.Fatalf("first acquire: got false")
	}

	// Second tick well inside the throttle window.
	if ok, _ := acquireCronLockDB(st.db, "ag", first+10_000); ok {
		t.Errorf("second acquire within window: got true, want false")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, err := st.db.GetKV(ctx, cronLockKVNamespace, cronLockKVKey("ag"))
	if err != nil {
		t.Fatalf("GetKV: %v", err)
	}
	if rec.Value != strconv.FormatInt(first, 10) {
		t.Errorf("kv value after rejected acquire = %q; want %q (must not bump on rejected fire)",
			rec.Value, strconv.FormatInt(first, 10))
	}
}

// TestAcquireCronLockDB_PastWindowReclaimsAndStamps verifies that once
// cronMinInterval has elapsed, the next acquire succeeds and the row
// is re-stamped with the new `now` so subsequent throttle checks
// measure from the new fire.
func TestAcquireCronLockDB_PastWindowReclaimsAndStamps(t *testing.T) {
	st := newAgentStore(t)
	first := int64(1_700_000_000_000)
	if ok, _ := acquireCronLockDB(st.db, "ag", first); !ok {
		t.Fatalf("first acquire: got false")
	}

	second := first + cronMinInterval.Milliseconds() + 1
	if ok, _ := acquireCronLockDB(st.db, "ag", second); !ok {
		t.Errorf("acquire past window: got false, want true")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, err := st.db.GetKV(ctx, cronLockKVNamespace, cronLockKVKey("ag"))
	if err != nil {
		t.Fatalf("GetKV: %v", err)
	}
	if rec.Value != strconv.FormatInt(second, 10) {
		t.Errorf("kv value after reclaim = %q; want %q (must bump to `second`)",
			rec.Value, strconv.FormatInt(second, 10))
	}
}

// TestAcquireCronLockDB_PerAgentIsolation pins the per-agent key
// scoping: agent A's recent fire must NOT throttle agent B. A
// regression that hashed both into the same key (e.g. by stripping
// the agentID suffix) would surface here.
func TestAcquireCronLockDB_PerAgentIsolation(t *testing.T) {
	st := newAgentStore(t)
	now := int64(1_700_000_000_000)

	if ok, _ := acquireCronLockDB(st.db, "agA", now); !ok {
		t.Fatalf("acquire agA: got false")
	}
	if ok, _ := acquireCronLockDB(st.db, "agB", now); !ok {
		t.Errorf("acquire agB while agA is throttled: got false, want true (per-agent isolation)")
	}
}

// TestAcquireCronLockDB_NilStoreFailsClosed verifies the safety net
// for callers that race shutdown: a nil *store.Store returns false so
// no cron job runs without backing storage.
func TestAcquireCronLockDB_NilStoreFailsClosed(t *testing.T) {
	if ok, _ := acquireCronLockDB(nil, "ag", 1_700_000_000_000); ok {
		t.Errorf("acquire with nil store: got true, want false (fail-closed)")
	}
}

// TestAcquireCronLockDB_MalformedRowFailsClosed verifies that a kv row
// whose value isn't a parseable integer (a peer-replicated junk write,
// manual edit) is treated as "throttled" — fail closed rather than
// silently re-fire on garbage state.
func TestAcquireCronLockDB_MalformedRowFailsClosed(t *testing.T) {
	st := newAgentStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bad := &store.KVRecord{
		Namespace: cronLockKVNamespace,
		Key:       cronLockKVKey("ag"),
		Value:     "not-a-number",
		Type:      store.KVTypeString,
		Scope:     store.KVScopeMachine,
	}
	if _, err := st.db.PutKV(ctx, bad, store.KVPutOptions{}); err != nil {
		t.Fatalf("seed bad row: %v", err)
	}

	if ok, _ := acquireCronLockDB(st.db, "ag", 1_700_000_000_000); ok {
		t.Errorf("acquire with malformed value: got true, want false (fail-closed)")
	}
}

// TestAcquireCronLockDB_WrongScopeFailsClosed verifies the row-shape
// gate. A row claiming the right namespace/key but with scope=global
// (e.g. a regression that downgrades the constant) collapses to
// "throttled" — better to skip a fire than to honour a row whose
// distribution semantics no longer match the design contract.
func TestAcquireCronLockDB_WrongScopeFailsClosed(t *testing.T) {
	st := newAgentStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wrongScope := &store.KVRecord{
		Namespace: cronLockKVNamespace,
		Key:       cronLockKVKey("ag"),
		Value:     "0", // would otherwise trigger "past window" + reclaim
		Type:      store.KVTypeString,
		Scope:     store.KVScopeGlobal, // contract violation
	}
	if _, err := st.db.PutKV(ctx, wrongScope, store.KVPutOptions{}); err != nil {
		t.Fatalf("seed wrong-scope row: %v", err)
	}

	if ok, _ := acquireCronLockDB(st.db, "ag", 1_700_000_000_000); ok {
		t.Errorf("acquire with scope=global: got true, want false (fail-closed)")
	}
}

// TestDeleteCronLockDB_RemovesRow verifies the delete primitive used
// by the reset path: after deletion, the next acquire sees no row and
// follows the first-fire branch (returns true).
func TestDeleteCronLockDB_RemovesRow(t *testing.T) {
	st := newAgentStore(t)

	if ok, _ := acquireCronLockDB(st.db, "ag", 1_700_000_000_000); !ok {
		t.Fatalf("seed acquire: got false")
	}
	if err := deleteCronLockDB(st.db, "ag"); err != nil {
		t.Fatalf("deleteCronLockDB: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := st.db.GetKV(ctx, cronLockKVNamespace, cronLockKVKey("ag")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetKV after delete: err=%v; want ErrNotFound", err)
	}

	// Within-window timestamp must NOT throttle the next acquire — the
	// row was removed, so the throttle window is reset.
	if ok, _ := acquireCronLockDB(st.db, "ag", 1_700_000_000_001); !ok {
		t.Errorf("post-delete acquire within original window: got false, want true (reset cleared throttle)")
	}
}

// TestDeleteCronLockDB_MissingRowIsIdempotent verifies that calling
// delete on an agent that never acquired (or whose row was already
// deleted) is success, not an error. Reset paths call this
// unconditionally and must not log spurious failures for fresh
// agents.
func TestDeleteCronLockDB_MissingRowIsIdempotent(t *testing.T) {
	st := newAgentStore(t)
	if err := deleteCronLockDB(st.db, "never-acquired"); err != nil {
		t.Errorf("deleteCronLockDB on missing row: %v; want nil", err)
	}
}

// TestDeleteCronLockDB_NilStoreNoop guards the shutdown-race safety
// net: callers may pass a nil store from m.Store() if newStore failed
// in a test fixture. The delete must no-op rather than panic.
func TestDeleteCronLockDB_NilStoreNoop(t *testing.T) {
	if err := deleteCronLockDB(nil, "ag"); err != nil {
		t.Errorf("deleteCronLockDB(nil): %v; want nil", err)
	}
}

// TestAcquireCronLockDB_PastWindowCASCollisionRejects pins the
// IfMatchETag branch: when GetKV captures an etag E1 for an in-DB
// row whose timestamp has aged past the throttle window, and a
// concurrent writer bumps the row to etag E2 before our PutKV,
// the IfMatchETag=E1 CAS must reject and acquireCronLockDB must
// report throttled=false. Without this, two same-store callers
// could both observe "past window" and both stamp, racing the
// throttle.
//
// Mechanism: the test-only cronLockKVCASTestHook fires after
// GetKV captures the etag and before the gated PutKV. We use it
// to land an out-of-band PutKV (etag bump) deterministically.
func TestAcquireCronLockDB_PastWindowCASCollisionRejects(t *testing.T) {
	st := newAgentStore(t)
	now := int64(1_700_000_000_000)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Seed an in-window row first — irrelevant to this test except
	// that we want SOME row present so GetKV returns it (rather
	// than ErrNotFound which is the IfMatchAny path).
	seed := &store.KVRecord{
		Namespace: cronLockKVNamespace,
		Key:       cronLockKVKey("ag"),
		Value:     strconv.FormatInt(now, 10),
		Type:      store.KVTypeString,
		Scope:     store.KVScopeMachine,
	}
	if _, err := st.db.PutKV(ctx, seed, store.KVPutOptions{}); err != nil {
		t.Fatalf("seed initial row: %v", err)
	}

	// Run with farFuture so the past-window check passes against
	// the seeded row, taking the IfMatchETag branch.
	farFuture := now + cronMinInterval.Milliseconds() + 1
	hookFired := 0
	cronLockKVCASTestHook = func() {
		hookFired++
		bump := &store.KVRecord{
			Namespace: cronLockKVNamespace,
			Key:       cronLockKVKey("ag"),
			Value:     strconv.FormatInt(farFuture-1, 10),
			Type:      store.KVTypeString,
			Scope:     store.KVScopeMachine,
		}
		if _, err := st.db.PutKV(ctx, bump, store.KVPutOptions{}); err != nil {
			t.Fatalf("hook bump: %v", err)
		}
	}
	t.Cleanup(func() { cronLockKVCASTestHook = nil })

	if ok, _ := acquireCronLockDB(st.db, "ag", farFuture); ok {
		t.Errorf("past-window acquire after concurrent bump: got true, want false (CAS must reject)")
	}
	if hookFired != 1 {
		t.Errorf("CAS hook fired %d times; want 1 (collision branch must run)", hookFired)
	}
}

// TestAcquireCronLockDB_NoRowCASCollisionRejects pins the
// IfMatchAny branch: when GetKV returns ErrNotFound (no row yet)
// and a concurrent writer inserts the row before our PutKV, the
// IfMatchAny=*  PutKV must reject because the row now exists.
// Mirrors the past-window test for the symmetric "first-fire"
// race entry path.
func TestAcquireCronLockDB_NoRowCASCollisionRejects(t *testing.T) {
	st := newAgentStore(t)
	now := int64(1_700_000_000_000)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hookFired := 0
	cronLockKVCASTestHook = func() {
		hookFired++
		// Insert from no-row state. acquireCronLockDB's outer
		// GetKV returned ErrNotFound, so the CAS argument it
		// will use is IfMatchAny ("row must not exist"). The
		// hook plants a row → outer PutKV must reject.
		insert := &store.KVRecord{
			Namespace: cronLockKVNamespace,
			Key:       cronLockKVKey("ag"),
			Value:     strconv.FormatInt(now, 10),
			Type:      store.KVTypeString,
			Scope:     store.KVScopeMachine,
		}
		if _, err := st.db.PutKV(ctx, insert, store.KVPutOptions{}); err != nil {
			t.Fatalf("hook insert: %v", err)
		}
	}
	t.Cleanup(func() { cronLockKVCASTestHook = nil })

	if ok, _ := acquireCronLockDB(st.db, "ag", now); ok {
		t.Errorf("no-row acquire after concurrent insert: got true, want false (IfMatchAny must reject)")
	}
	if hookFired != 1 {
		t.Errorf("CAS hook fired %d times; want 1 (collision branch must run)", hookFired)
	}
}

// TestRollbackCronLockDB_RemovesOwnStamp verifies the success path
// of the rollback used by runCronJob when Chat() refused the tick.
// After acquireCronLockDB returns (true, etag), rollback with that
// etag must remove the row so a follow-up acquire on the next tick
// is not throttled by the aborted fire's stamp.
func TestRollbackCronLockDB_RemovesOwnStamp(t *testing.T) {
	st := newAgentStore(t)
	now := int64(1_700_000_000_000)

	ok, stamp := acquireCronLockDB(st.db, "ag", now)
	if !ok {
		t.Fatalf("acquire: got false")
	}
	if stamp == "" {
		t.Fatalf("acquire: empty stamp etag (must be set on success)")
	}

	if err := rollbackCronLockDB(st.db, "ag", stamp); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// Row must be gone — a fresh acquire goes through the no-row
	// branch and succeeds even within what would have been the
	// throttle window.
	if ok2, _ := acquireCronLockDB(st.db, "ag", now+1); !ok2 {
		t.Errorf("post-rollback acquire within window: got false, want true (rollback should have cleared the stamp)")
	}
}

// TestRollbackCronLockDB_PreservesForeignStamp pins the CAS
// guard's central reason-for-being: when our Chat() takes longer
// than cronMinInterval to fail and a parallel acquire stamps the
// row past-window, our rollback's stale etag must NOT erase that
// foreign stamp. Codex round 3 flagged the unconditional-delete
// version of this code as a regression risk; this test would
// fail under that version.
func TestRollbackCronLockDB_PreservesForeignStamp(t *testing.T) {
	st := newAgentStore(t)
	now := int64(1_700_000_000_000)

	// We acquire at T0 — stamp etag E1.
	ok, ourStamp := acquireCronLockDB(st.db, "ag", now)
	if !ok {
		t.Fatalf("acquire: got false")
	}

	// Time skips past the window; a parallel acquirer stamps the
	// row with a fresh etag E2.
	farFuture := now + cronMinInterval.Milliseconds() + 1
	ok2, foreignStamp := acquireCronLockDB(st.db, "ag", farFuture)
	if !ok2 {
		t.Fatalf("second acquire (past window): got false")
	}
	if foreignStamp == ourStamp {
		t.Fatalf("second acquire reused the same etag (%q) — CAS test premise broken", ourStamp)
	}

	// Our rollback runs late; the row's etag is now E2, not E1.
	// rollbackCronLockDB must NOT delete the foreign stamp.
	if err := rollbackCronLockDB(st.db, "ag", ourStamp); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// The foreign stamp must still be in place — proof: a
	// within-window acquire from `farFuture+1` is throttled.
	if ok3, _ := acquireCronLockDB(st.db, "ag", farFuture+1); ok3 {
		t.Errorf("foreign stamp erased by stale rollback: got acquire=true, want false (CAS must protect foreign stamp)")
	}
}

// TestRollbackCronLockDB_NilStoreOrEmptyETagNoop guards the
// shutdown-race / no-acquire safety nets: the rollback helper is
// called even from paths where stampETag may legitimately be empty
// (acquireCronLockDB returned false) or st may be nil. Both must
// no-op rather than panic.
func TestRollbackCronLockDB_NilStoreOrEmptyETagNoop(t *testing.T) {
	st := newAgentStore(t)
	if err := rollbackCronLockDB(nil, "ag", "etag"); err != nil {
		t.Errorf("rollback(nil): %v; want nil", err)
	}
	if err := rollbackCronLockDB(st.db, "ag", ""); err != nil {
		t.Errorf("rollback(empty etag): %v; want nil", err)
	}
}

// TestRemoveLegacyCronLock_BestEffortUnlinks verifies the legacy
// dotfile cleanup helper: a stray `<agentDir>/.cron_last` file from a
// pre-cutover install gets removed on the next acquire path. Does
// NOT require the file to exist — best-effort is the contract.
func TestRemoveLegacyCronLock_BestEffortUnlinks(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("APPDATA", "")

	dir := agentDir("ag")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}
	legacy := filepath.Join(dir, cronLockFile)
	if err := os.WriteFile(legacy, nil, 0o644); err != nil {
		t.Fatalf("seed legacy file: %v", err)
	}

	removeLegacyCronLock("ag")

	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy file still present after cleanup: err=%v", err)
	}

	// Idempotent — second call on missing file must not error.
	removeLegacyCronLock("ag")
}

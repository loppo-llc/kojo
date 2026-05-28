package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// openLegacyTestStore opens a writable kv store for fixture seeding.
// Tests that want to exercise the production read-only path can call
// reopenReadOnly to swap to a fresh ReadOnly handle.
func openLegacyTestStore(t *testing.T, configDir string) (*store.Store, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rw, err := store.Open(ctx, store.Options{ConfigDir: configDir})
	if err != nil {
		t.Fatalf("open writer store: %v", err)
	}
	return rw, func() { rw.Close() }
}

func reopenReadOnly(t *testing.T, configDir string) (*store.Store, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ro, err := store.Open(ctx, store.Options{ConfigDir: configDir, ReadOnly: true})
	if err != nil {
		t.Fatalf("open read-only store: %v", err)
	}
	return ro, func() { ro.Close() }
}

// putKVRow seeds a kv row with arbitrary type/scope. Tests use it to
// plant well-formed rows (matching the runtime contract) AND
// malformed rows (wrong type, wrong value) so the validator coverage
// is real.
func putKVRow(t *testing.T, st *store.Store, ns, key, value string, typ store.KVType, scope store.KVScope) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := st.PutKV(ctx, &store.KVRecord{
		Namespace: ns,
		Key:       key,
		Value:     value,
		Type:      typ,
		Scope:     scope,
	}, store.KVPutOptions{}); err != nil {
		t.Fatalf("seed kv %s/%s: %v", ns, key, err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const fakeOwnerHash = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestPlanLegacyCleanupClassifies seeds a mix of post-cutover (kv
// row + disk file) and pre-cutover (disk file only) legacy artifacts
// and verifies they land in Redundant vs Pending respectively.
//
// Each kind seeds the kv row with the SAME (type, scope) the runtime
// would write (string/global for paused & tokens, string/machine for
// cron_last, json/global for autosummary marker). Tests that cover
// malformed rows live in TestPlanLegacyCleanupRejectsMalformedKV.
func TestPlanLegacyCleanupClassifies(t *testing.T) {
	root := t.TempDir()
	st, closeRW := openLegacyTestStore(t, root)
	defer closeRW()

	// Post-cutover redundant.
	putKVRow(t, st, "scheduler", "paused", "true", store.KVTypeString, store.KVScopeGlobal)
	writeFile(t, filepath.Join(root, "agents", "cron_paused"), "true")

	putKVRow(t, st, "auth", "owner.token", fakeOwnerHash, store.KVTypeString, store.KVScopeGlobal)
	writeFile(t, filepath.Join(root, "auth", "owner.token"), fakeOwnerHash)

	// agent_a: redundant on both per-agent kinds.
	putKVRow(t, st, "scheduler", "cron_last/agent_a", "1700000000000", store.KVTypeString, store.KVScopeMachine)
	putKVRow(t, st, "autosummary", "agent_a", `{"lastN":1}`, store.KVTypeJSON, store.KVScopeGlobal)
	writeFile(t, filepath.Join(root, "agents", "agent_a", ".cron_last"), "1700000000000")
	writeFile(t, filepath.Join(root, "agents", "agent_a", "autosummary_marker"), `{"lastN":1}`)

	// agent_b: pending on both (no kv).
	writeFile(t, filepath.Join(root, "agents", "agent_b", ".cron_last"), "1700000000000")
	writeFile(t, filepath.Join(root, "agents", "agent_b", "autosummary_marker"), `{"lastN":1}`)

	// agent_c: redundant on autosummary only.
	putKVRow(t, st, "autosummary", "agent_c", `{"lastN":1}`, store.KVTypeJSON, store.KVScopeGlobal)
	writeFile(t, filepath.Join(root, "agents", "agent_c", ".cron_last"), "1700000000000")
	writeFile(t, filepath.Join(root, "agents", "agent_c", "autosummary_marker"), `{"lastN":1}`)

	// auth/agent_tokens: agent_a redundant, agent_b pending.
	putKVRow(t, st, "auth", "agent_tokens/agent_a", fakeOwnerHash, store.KVTypeString, store.KVScopeGlobal)
	writeFile(t, filepath.Join(root, "auth", "agent_tokens", "agent_a"), fakeOwnerHash)
	writeFile(t, filepath.Join(root, "auth", "agent_tokens", "agent_b"), fakeOwnerHash)

	// Stray non-agentID dir/file. Must be ignored by the walker.
	writeFile(t, filepath.Join(root, "agents", ".DS_Store"), "")

	plan, err := planLegacyCleanup(context.Background(), st, root)
	if err != nil {
		t.Fatalf("planLegacyCleanup: %v", err)
	}
	// Redundant: cron_paused, owner.token, agent_a/.cron_last,
	// agent_a/autosummary_marker, agent_c/autosummary_marker,
	// auth/agent_tokens/agent_a — total 6.
	if got, want := len(plan.Redundant), 6; got != want {
		t.Errorf("Redundant = %d, want %d (%+v)", got, want, plan.Redundant)
	}
	// Pending: agent_b/.cron_last, agent_b/autosummary_marker,
	// agent_c/.cron_last, auth/agent_tokens/agent_b — total 4.
	if got, want := len(plan.Pending), 4; got != want {
		t.Errorf("Pending = %d, want %d (%+v)", got, want, plan.Pending)
	}
}

// TestPlanLegacyCleanupRejectsMalformedKV verifies that kv rows with
// the wrong type, scope, secret flag, or value format are routed to
// Pending instead of Redundant — even though the file exists. A
// malformed row would be rejected by the runtime (which falls back
// to disk), so the disk file is the only surviving authoritative
// copy and MUST NOT be deleted.
func TestPlanLegacyCleanupRejectsMalformedKV(t *testing.T) {
	cases := []struct {
		name      string
		seed      func(t *testing.T, st *store.Store, root string)
		wantPend  int
		wantRedun int
	}{
		{
			name: "cron_paused wrong type",
			seed: func(t *testing.T, st *store.Store, root string) {
				putKVRow(t, st, "scheduler", "paused", `"true"`, store.KVTypeJSON, store.KVScopeGlobal)
				writeFile(t, filepath.Join(root, "agents", "cron_paused"), "true")
			},
		},
		{
			name: "cron_paused wrong scope",
			seed: func(t *testing.T, st *store.Store, root string) {
				putKVRow(t, st, "scheduler", "paused", "true", store.KVTypeString, store.KVScopeMachine)
				writeFile(t, filepath.Join(root, "agents", "cron_paused"), "true")
			},
		},
		{
			name: "cron_paused unparseable value",
			seed: func(t *testing.T, st *store.Store, root string) {
				putKVRow(t, st, "scheduler", "paused", "yes", store.KVTypeString, store.KVScopeGlobal)
				writeFile(t, filepath.Join(root, "agents", "cron_paused"), "true")
			},
		},
		{
			name: "cron_last non-int value",
			seed: func(t *testing.T, st *store.Store, root string) {
				putKVRow(t, st, "scheduler", "cron_last/ag", "soon", store.KVTypeString, store.KVScopeMachine)
				writeFile(t, filepath.Join(root, "agents", "ag", ".cron_last"), "soon")
			},
		},
		{
			name: "cron_last wrong scope",
			seed: func(t *testing.T, st *store.Store, root string) {
				putKVRow(t, st, "scheduler", "cron_last/ag", "1700000000000", store.KVTypeString, store.KVScopeGlobal)
				writeFile(t, filepath.Join(root, "agents", "ag", ".cron_last"), "1700000000000")
			},
		},
		{
			name: "autosummary invalid JSON",
			seed: func(t *testing.T, st *store.Store, root string) {
				putKVRow(t, st, "autosummary", "ag", `not-json`, store.KVTypeJSON, store.KVScopeGlobal)
				writeFile(t, filepath.Join(root, "agents", "ag", "autosummary_marker"), "{}")
			},
		},
		{
			name: "autosummary wrong type",
			seed: func(t *testing.T, st *store.Store, root string) {
				putKVRow(t, st, "autosummary", "ag", `{"lastN":1}`, store.KVTypeString, store.KVScopeGlobal)
				writeFile(t, filepath.Join(root, "agents", "ag", "autosummary_marker"), `{"lastN":1}`)
			},
		},
		{
			// json.Valid passes but Unmarshal-into-struct fails:
			// JSON array vs object struct.
			name: "autosummary JSON array",
			seed: func(t *testing.T, st *store.Store, root string) {
				putKVRow(t, st, "autosummary", "ag", `[]`, store.KVTypeJSON, store.KVScopeGlobal)
				writeFile(t, filepath.Join(root, "agents", "ag", "autosummary_marker"), "[]")
			},
		},
		{
			// json.Valid passes but field-type mismatch (lastN
			// is int in the runtime struct).
			name: "autosummary field type mismatch",
			seed: func(t *testing.T, st *store.Store, root string) {
				putKVRow(t, st, "autosummary", "ag", `{"lastN":"bad"}`, store.KVTypeJSON, store.KVScopeGlobal)
				writeFile(t, filepath.Join(root, "agents", "ag", "autosummary_marker"), `{"lastN":"bad"}`)
			},
		},
		{
			// json.Valid passes but Unmarshal-into-struct fails:
			// JSON string scalar vs object struct.
			name: "autosummary JSON string",
			seed: func(t *testing.T, st *store.Store, root string) {
				putKVRow(t, st, "autosummary", "ag", `"x"`, store.KVTypeJSON, store.KVScopeGlobal)
				writeFile(t, filepath.Join(root, "agents", "ag", "autosummary_marker"), `"x"`)
			},
		},
		{
			name: "owner.token missing prefix",
			seed: func(t *testing.T, st *store.Store, root string) {
				putKVRow(t, st, "auth", "owner.token", "deadbeef", store.KVTypeString, store.KVScopeGlobal)
				writeFile(t, filepath.Join(root, "auth", "owner.token"), "deadbeef")
			},
		},
		{
			name: "owner.token uppercase hex",
			seed: func(t *testing.T, st *store.Store, root string) {
				putKVRow(t, st, "auth", "owner.token",
					"sha256:0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF",
					store.KVTypeString, store.KVScopeGlobal)
				writeFile(t, filepath.Join(root, "auth", "owner.token"), fakeOwnerHash)
			},
		},
		{
			name: "agent_tokens too short hash",
			seed: func(t *testing.T, st *store.Store, root string) {
				putKVRow(t, st, "auth", "agent_tokens/ag",
					"sha256:abcd",
					store.KVTypeString, store.KVScopeGlobal)
				writeFile(t, filepath.Join(root, "auth", "agent_tokens", "ag"), "sha256:abcd")
				// Need a corresponding agent dir to make the
				// per-agent walker do anything; this case targets
				// the auth/agent_tokens walker which doesn't need
				// it.
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			st, closeRW := openLegacyTestStore(t, root)
			defer closeRW()
			tc.seed(t, st, root)
			plan, err := planLegacyCleanup(context.Background(), st, root)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if got, want := len(plan.Redundant), tc.wantRedun; got != want {
				t.Errorf("Redundant = %d, want %d (%+v)", got, want, plan.Redundant)
			}
			// Every test case plants exactly one disk file with a
			// malformed kv row, so we expect at least 1 Pending.
			if got, min := len(plan.Pending), 1; got < min {
				t.Errorf("Pending = %d, want >= %d (%+v)", got, min, plan.Pending)
			}
		})
	}
}

// TestApplyLegacyCleanPlanRemovesOnlyRedundant verifies that apply
// drops Redundant entries while leaving Pending entries on disk.
func TestApplyLegacyCleanPlanRemovesOnlyRedundant(t *testing.T) {
	root := t.TempDir()
	st, closeRW := openLegacyTestStore(t, root)
	defer closeRW()

	putKVRow(t, st, "scheduler", "paused", "true", store.KVTypeString, store.KVScopeGlobal)
	pausedPath := filepath.Join(root, "agents", "cron_paused")
	writeFile(t, pausedPath, "true")

	pendingPath := filepath.Join(root, "agents", "agent_x", ".cron_last")
	writeFile(t, pendingPath, "1700000000000")

	plan, err := planLegacyCleanup(context.Background(), st, root)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if errs := applyLegacyCleanPlan(plan, st); len(errs) > 0 {
		t.Fatalf("apply: %v", errs)
	}

	if _, err := os.Stat(pausedPath); !os.IsNotExist(err) {
		t.Errorf("redundant file survived: err=%v", err)
	}
	if _, err := os.Stat(pendingPath); err != nil {
		t.Errorf("pending file removed: %v", err)
	}
}

// TestApplyLegacyCleanPlanReValidatesAtApply seeds a plan with a
// Redundant entry, then deletes the kv row before calling apply. The
// disk file must NOT be removed because the kv row is no longer the
// authoritative copy.
func TestApplyLegacyCleanPlanReValidatesAtApply(t *testing.T) {
	root := t.TempDir()
	st, closeRW := openLegacyTestStore(t, root)
	defer closeRW()

	putKVRow(t, st, "scheduler", "paused", "true", store.KVTypeString, store.KVScopeGlobal)
	pausedPath := filepath.Join(root, "agents", "cron_paused")
	writeFile(t, pausedPath, "true")

	plan, err := planLegacyCleanup(context.Background(), st, root)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Redundant) != 1 {
		t.Fatalf("expected 1 Redundant entry, got %+v", plan.Redundant)
	}

	// Simulate a runtime / operator action that nukes the kv row
	// between scan and apply.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := st.DeleteKV(ctx, "scheduler", "paused", ""); err != nil {
		t.Fatalf("delete kv: %v", err)
	}

	if errs := applyLegacyCleanPlan(plan, st); len(errs) > 0 {
		t.Fatalf("apply: %v", errs)
	}
	if _, err := os.Stat(pausedPath); err != nil {
		t.Errorf("file was removed despite kv row disappearing: %v", err)
	}
}

// TestPlanLegacyCleanupHandlesMissingDirs verifies the scan returns
// an empty plan (no error) when the configdir has no agents/ or
// auth/ subtree at all — common for fresh installs.
func TestPlanLegacyCleanupHandlesMissingDirs(t *testing.T) {
	root := t.TempDir()
	st, closeRW := openLegacyTestStore(t, root)
	defer closeRW()

	plan, err := planLegacyCleanup(context.Background(), st, root)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Redundant)+len(plan.Pending) != 0 {
		t.Errorf("plan = %+v, want empty", plan)
	}
}

// TestPlanLegacyCleanupSkipsInvalidAgentIDs verifies that filenames
// or directory names violating the agentID alphabet are ignored.
func TestPlanLegacyCleanupSkipsInvalidAgentIDs(t *testing.T) {
	root := t.TempDir()
	st, closeRW := openLegacyTestStore(t, root)
	defer closeRW()

	if err := os.MkdirAll(filepath.Join(root, "agents", "bad name!"), 0o755); err != nil {
		t.Fatalf("mkdir invalid agent: %v", err)
	}
	writeFile(t, filepath.Join(root, "agents", "bad name!", ".cron_last"), "1")

	if err := os.MkdirAll(filepath.Join(root, "auth", "agent_tokens"), 0o755); err != nil {
		t.Fatalf("mkdir auth/agent_tokens: %v", err)
	}
	writeFile(t, filepath.Join(root, "auth", "agent_tokens", "bad name!"), fakeOwnerHash)

	plan, err := planLegacyCleanup(context.Background(), st, root)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Redundant)+len(plan.Pending) != 0 {
		t.Errorf("invalid-id entries leaked: %+v", plan)
	}
}

// TestRunCleanCommandLegacyEndToEnd exercises the production path
// (runCleanCommand → ReadOnly store.Open → planLegacyCleanup →
// applyLegacyCleanPlan) so the read-only DSN, the kv handle plumbing,
// and the apply re-validation are all covered together.
func TestRunCleanCommandLegacyEndToEnd(t *testing.T) {
	root := t.TempDir()

	// Seed via a writable handle, then close it so the read-only
	// open inside runCleanCommand sees the migrations applied.
	rw, closeRW := openLegacyTestStore(t, root)
	putKVRow(t, rw, "scheduler", "paused", "true", store.KVTypeString, store.KVScopeGlobal)
	closeRW()

	pausedPath := filepath.Join(root, "agents", "cron_paused")
	writeFile(t, pausedPath, "true")

	rc := runCleanCommand(cleanFlags{
		target:        "legacy",
		apply:         true,
		configDirPath: root,
		logger:        slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if rc != 0 {
		t.Fatalf("runCleanCommand legacy returned rc=%d", rc)
	}
	if _, err := os.Stat(pausedPath); !os.IsNotExist(err) {
		t.Errorf("expected paused file to be removed, stat err=%v", err)
	}
}

// TestRunCleanCommandAllRunsBoth verifies target="all" exercises
// both snapshot and legacy paths in one invocation.
func TestRunCleanCommandAllRunsBoth(t *testing.T) {
	root := t.TempDir()

	// Pre-populate snapshot fixture (uses helpers from
	// clean_cmd_test.go).
	stale := makeSnapshot(t, root, 30*24*time.Hour, "old1")

	// Pre-populate legacy fixture.
	rw, closeRW := openLegacyTestStore(t, root)
	putKVRow(t, rw, "scheduler", "paused", "true", store.KVTypeString, store.KVScopeGlobal)
	closeRW()
	pausedPath := filepath.Join(root, "agents", "cron_paused")
	writeFile(t, pausedPath, "true")

	rc := runCleanCommand(cleanFlags{
		target:        "all",
		apply:         true,
		maxAgeDays:    7,
		keepLatest:    0,
		configDirPath: root,
		logger:        slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if rc != 0 {
		t.Fatalf("runCleanCommand all returned rc=%d", rc)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale snapshot survived: %v", err)
	}
	if _, err := os.Stat(pausedPath); !os.IsNotExist(err) {
		t.Errorf("legacy paused file survived: %v", err)
	}
}

// Sanity: ensure the read-only reopen helper is exercised; otherwise
// `unused` lint would catch a dead helper.
func TestReopenReadOnlyOpens(t *testing.T) {
	root := t.TempDir()
	rw, closeRW := openLegacyTestStore(t, root)
	closeRW()
	ro, closeRO := reopenReadOnly(t, root)
	defer closeRO()
	_ = rw
	_ = ro
}

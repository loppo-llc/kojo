package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// trashTestEnv pins v0PathOverride to a temp root and returns the
// (parent, v0Path) pair so tests can drop kojo.deleted-<ts>/ entries
// alongside it.
func trashTestEnv(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	v0 := filepath.Join(root, "kojo")
	v0PathOverride = func() string { return v0 }
	t.Cleanup(func() { v0PathOverride = nil })
	return root, v0
}

func seedTrashDir(t *testing.T, parent string, age time.Duration) string {
	t.Helper()
	stamp := time.Now().Add(-age).UnixMilli()
	path := filepath.Join(parent, fmt.Sprintf("kojo.deleted-%d", stamp))
	if err := os.MkdirAll(filepath.Join(path, "agents"), 0o755); err != nil {
		t.Fatalf("seed trash %s: %v", path, err)
	}
	if err := os.WriteFile(filepath.Join(path, "agents", "marker"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	return path
}

func TestPlanV0TrashCleanup_HappyPath(t *testing.T) {
	root, _ := trashTestEnv(t)
	old := seedTrashDir(t, root, 30*24*time.Hour)
	young := seedTrashDir(t, root, 1*time.Hour)

	plan, err := planV0TrashCleanup(0, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Purge) != 2 {
		t.Errorf("Purge size = %d, want 2 (min-age=0 includes both)", len(plan.Purge))
	}
	if len(plan.KeepYoung) != 0 {
		t.Errorf("KeepYoung size = %d, want 0", len(plan.KeepYoung))
	}
	_ = old
	_ = young
}

func TestPlanV0TrashCleanup_AgeFilterKeepsYoung(t *testing.T) {
	root, _ := trashTestEnv(t)
	old := seedTrashDir(t, root, 10*24*time.Hour)
	young := seedTrashDir(t, root, 1*time.Hour)

	plan, err := planV0TrashCleanup(7, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Purge) != 1 || plan.Purge[0].Path != old {
		t.Errorf("Purge = %v, want [%s]", plan.Purge, old)
	}
	if len(plan.KeepYoung) != 1 || plan.KeepYoung[0].Path != young {
		t.Errorf("KeepYoung = %v, want [%s]", plan.KeepYoung, young)
	}
}

func TestPlanV0TrashCleanup_AnomaliesReportedNotPurged(t *testing.T) {
	root, _ := trashTestEnv(t)
	// Symlink whose name matches the pattern.
	target := filepath.Join(root, "real")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	link := filepath.Join(root, "kojo.deleted-1700000000000")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	// Regular file matching the pattern (anomaly).
	if err := os.WriteFile(filepath.Join(root, "kojo.deleted-1700000001000"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file anomaly: %v", err)
	}

	plan, err := planV0TrashCleanup(0, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Anomalies) != 2 {
		t.Errorf("Anomalies size = %d, want 2", len(plan.Anomalies))
	}
	if len(plan.Purge) != 0 {
		t.Errorf("Purge should be empty for anomalies, got %v", plan.Purge)
	}
}

func TestPlanV0TrashCleanup_IgnoresUnrelatedDirs(t *testing.T) {
	root, _ := trashTestEnv(t)
	// Live v0/v1 dirs and unrelated user content must not be picked up.
	for _, name := range []string{"kojo", "kojo-v1", "other-dir", "kojo.something"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatalf("seed unrelated %s: %v", name, err)
		}
	}
	plan, err := planV0TrashCleanup(0, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if total := len(plan.Purge) + len(plan.KeepYoung) + len(plan.Anomalies); total != 0 {
		t.Errorf("expected zero entries, got %d", total)
	}
}

// TestPlanV0TrashCleanup_FutureDatedStamp: a stamp 30 days in the
// future is parseable but lands in KeepYoung (after the cutoff).
// Apply must not purge it.
func TestPlanV0TrashCleanup_FutureDatedStamp(t *testing.T) {
	root, _ := trashTestEnv(t)
	stamp := time.Now().Add(30 * 24 * time.Hour).UnixMilli()
	path := filepath.Join(root, fmt.Sprintf("kojo.deleted-%d", stamp))
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("seed future-dated: %v", err)
	}

	plan, err := planV0TrashCleanup(7, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Purge) != 0 {
		t.Errorf("future-stamped entry must not be Purge, got %v", plan.Purge)
	}
	if len(plan.KeepYoung) != 1 || plan.KeepYoung[0].Path != path {
		t.Errorf("future-stamped entry should be KeepYoung, got %v", plan.KeepYoung)
	}
	// Apply with the same plan: re-validation refuses on
	// "stamp is in the future".
	plan.Purge = append(plan.Purge, plan.KeepYoung[0])
	errs := applyV0TrashCleanPlan(plan, v0CleanLogger())
	if len(errs) == 0 {
		t.Error("apply with manually-promoted future entry should error")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("future entry should not be removed: %v", err)
	}
}

// TestPlanV0TrashCleanup_NumericOverflowStamp: a 100+ digit stamp
// can't be parsed as int64 → Anomalies.
func TestPlanV0TrashCleanup_NumericOverflowStamp(t *testing.T) {
	root, _ := trashTestEnv(t)
	bigStamp := "99999999999999999999999999"
	path := filepath.Join(root, "kojo.deleted-"+bigStamp)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	plan, err := planV0TrashCleanup(0, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Anomalies) != 1 {
		t.Errorf("overflow stamp must be Anomaly, got %v", plan)
	}
	if len(plan.Purge) != 0 {
		t.Errorf("overflow stamp must not be Purge, got %v", plan.Purge)
	}
}

// TestApplyV0TrashCleanPlan_PostScanReplacementAsSymlink: between
// scan and apply the operator replaced the trash dir with a symlink
// to a sensitive location. Apply must refuse to RemoveAll.
func TestApplyV0TrashCleanPlan_PostScanReplacementAsSymlink(t *testing.T) {
	root, _ := trashTestEnv(t)
	old := seedTrashDir(t, root, 30*24*time.Hour)

	plan, err := planV0TrashCleanup(0, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Purge) != 1 {
		t.Fatalf("setup: expected 1 Purge, got %v", plan.Purge)
	}
	// Replace the dir with a symlink to a sibling sensitive dir.
	sensitive := filepath.Join(root, "sensitive")
	if err := os.MkdirAll(sensitive, 0o755); err != nil {
		t.Fatalf("mkdir sensitive: %v", err)
	}
	if err := os.RemoveAll(old); err != nil {
		t.Fatalf("rm dir: %v", err)
	}
	if err := os.Symlink(sensitive, old); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	errs := applyV0TrashCleanPlan(plan, v0CleanLogger())
	if len(errs) == 0 {
		t.Error("apply must refuse to remove a symlink that replaced a planned trash dir")
	}
	// Sensitive dir must still exist.
	if _, err := os.Stat(sensitive); err != nil {
		t.Errorf("sensitive dir was touched: %v", err)
	}
}

func TestPlanV0TrashCleanup_NoParentDir(t *testing.T) {
	tmp := t.TempDir()
	v0 := filepath.Join(tmp, "missing-parent", "kojo")
	v0PathOverride = func() string { return v0 }
	t.Cleanup(func() { v0PathOverride = nil })
	plan, err := planV0TrashCleanup(0, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Purge)+len(plan.KeepYoung)+len(plan.Anomalies) != 0 {
		t.Error("expected empty plan for missing parent")
	}
}

func TestApplyV0TrashCleanPlan_Removes(t *testing.T) {
	root, _ := trashTestEnv(t)
	a := seedTrashDir(t, root, 10*24*time.Hour)
	b := seedTrashDir(t, root, 20*24*time.Hour)

	plan, err := planV0TrashCleanup(0, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if errs := applyV0TrashCleanPlan(plan, v0CleanLogger()); len(errs) > 0 {
		t.Fatalf("apply errors: %v", errs)
	}
	for _, p := range []string{a, b} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("trash dir %s still present: %v", p, err)
		}
	}
}

func TestApplyV0TrashCleanPlan_LeavesAnomalies(t *testing.T) {
	root, _ := trashTestEnv(t)
	bad := filepath.Join(root, "kojo.deleted-9999999999999")
	if err := os.WriteFile(bad, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	plan, err := planV0TrashCleanup(0, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	_ = applyV0TrashCleanPlan(plan, v0CleanLogger())
	if _, err := os.Stat(bad); err != nil {
		t.Errorf("anomaly file purged: %v", err)
	}
}

func TestRunCleanCommandV0TrashEndToEnd(t *testing.T) {
	root, _ := trashTestEnv(t)
	old := seedTrashDir(t, root, 30*24*time.Hour)
	young := seedTrashDir(t, root, 1*time.Hour)

	rc := runCleanCommand(cleanFlags{
		target:        "v0-trash",
		apply:         true,
		minAgeDays:    7,
		logger:        v0CleanLogger(),
		configDirPath: filepath.Join(root, "kojo-v1"),
	})
	if rc != 0 {
		t.Errorf("rc = %d, want 0", rc)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old trash still present: %v", err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Errorf("young trash purged: %v", err)
	}
}

// Sanity: target='snapshots' (the simplest non-trash target) must
// NOT touch trash. Protects the runV0Trash explicit-only gate. We
// use 'snapshots' rather than 'all' to avoid needing a kv store
// just for this assertion; both targets share the same runV0Trash
// (false) branch so the test is equally tight.
func TestRunCleanCommandSnapshotsSkipsV0Trash(t *testing.T) {
	root, _ := trashTestEnv(t)
	survivor := seedTrashDir(t, root, 30*24*time.Hour)

	v1 := filepath.Join(root, "kojo-v1")
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}

	rc := runCleanCommand(cleanFlags{
		target:        "snapshots",
		apply:         true,
		logger:        v0CleanLogger(),
		configDirPath: v1,
	})
	if rc != 0 {
		t.Errorf("rc = %d, want 0", rc)
	}
	if _, err := os.Stat(survivor); err != nil {
		t.Errorf("--clean snapshots purged a v0 trash dir (survivor missing: %v)", err)
	}
}

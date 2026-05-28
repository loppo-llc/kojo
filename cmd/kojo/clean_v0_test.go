package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/migrate"
)

func v0CleanLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// v0TestEnv sets up a v0 dir + v1 dir with migration_complete.json
// stamped to match the v0 manifest. Returns (v0Path, v1Path).
func v0TestEnv(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	v0 := filepath.Join(root, "kojo")     // v0
	v1 := filepath.Join(root, "kojo-v1")  // v1
	if err := os.MkdirAll(filepath.Join(v0, "agents"), 0o755); err != nil {
		t.Fatalf("mkdir v0: %v", err)
	}
	if err := os.WriteFile(filepath.Join(v0, "agents", "agents.json"), []byte("[]"), 0o644); err != nil {
		t.Fatalf("seed v0: %v", err)
	}
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}
	manifest, err := migrate.ManifestSHA256(v0)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	cf := migrate.CompleteFile{
		V0Path:           v0,
		V1SchemaVersion:  1,
		V0SHA256Manifest: manifest,
		MigratorVersion:  "test",
		CompletedAt:      time.Now().UnixMilli(),
	}
	body, _ := json.Marshal(&cf)
	if err := os.WriteFile(filepath.Join(v1, migrate.CompleteFileName), body, 0o644); err != nil {
		t.Fatalf("seed complete: %v", err)
	}
	v0PathOverride = func() string { return v0 }
	t.Cleanup(func() { v0PathOverride = nil })
	return v0, v1
}

// TestPlanV0Cleanup_HappyPath returns a clean plan when v0 manifest
// matches migration_complete.json — no PartialReason.
func TestPlanV0Cleanup_HappyPath(t *testing.T) {
	v0, v1 := v0TestEnv(t)
	plan, err := planV0Cleanup(v1, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.V0Path != v0 {
		t.Errorf("V0Path = %q, want %q", plan.V0Path, v0)
	}
	if plan.PartialReason != "" {
		t.Errorf("unexpected PartialReason: %s", plan.PartialReason)
	}
	if plan.TrashPath == "" {
		t.Error("TrashPath empty")
	}
	if filepath.Dir(plan.TrashPath) != filepath.Dir(v0) {
		t.Errorf("trash %q is not a sibling of v0 %q", plan.TrashPath, v0)
	}
}

// TestPlanV0Cleanup_NoV0Dir returns an empty plan when v0 is absent.
func TestPlanV0Cleanup_NoV0Dir(t *testing.T) {
	root := t.TempDir()
	v1 := filepath.Join(root, "kojo-v1")
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}
	v0PathOverride = func() string { return filepath.Join(root, "kojo") } // does not exist
	t.Cleanup(func() { v0PathOverride = nil })

	plan, err := planV0Cleanup(v1, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.V0Path != "" {
		t.Errorf("V0Path = %q, want empty", plan.V0Path)
	}
}

// TestPlanV0Cleanup_NoCompleteFile blocks with PartialReason when the
// v1 dir has no migration_complete.json.
func TestPlanV0Cleanup_NoCompleteFile(t *testing.T) {
	root := t.TempDir()
	v0 := filepath.Join(root, "kojo")
	v1 := filepath.Join(root, "kojo-v1")
	if err := os.MkdirAll(v0, 0o755); err != nil {
		t.Fatalf("mkdir v0: %v", err)
	}
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}
	v0PathOverride = func() string { return v0 }
	t.Cleanup(func() { v0PathOverride = nil })

	plan, err := planV0Cleanup(v1, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.PartialReason == "" {
		t.Error("expected PartialReason for missing migration_complete.json")
	}
}

// TestPlanV0Cleanup_ManifestDivergence flags a PartialReason when v0
// has been edited after migration.
func TestPlanV0Cleanup_ManifestDivergence(t *testing.T) {
	v0, v1 := v0TestEnv(t)
	// Modify v0 — append a fresh file so the manifest changes.
	if err := os.WriteFile(filepath.Join(v0, "agents", "extra.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatalf("modify v0: %v", err)
	}

	plan, err := planV0Cleanup(v1, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.PartialReason == "" {
		t.Error("expected PartialReason for v0 manifest divergence")
	}
}

// TestApplyV0CleanPlan_HappyPath performs the rename when no
// PartialReason is set.
func TestApplyV0CleanPlan_HappyPath(t *testing.T) {
	v0, v1 := v0TestEnv(t)
	plan, err := planV0Cleanup(v1, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := applyV0CleanPlan(plan, v0CleanLogger()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// v0 should now be at trash path.
	if _, err := os.Stat(v0); !os.IsNotExist(err) {
		t.Errorf("v0 still present at %s: %v", v0, err)
	}
	if _, err := os.Stat(plan.TrashPath); err != nil {
		t.Errorf("trash path not present: %v", err)
	}
}

// TestApplyV0CleanPlan_RefuseOnPartialReason refuses without --force.
func TestApplyV0CleanPlan_RefuseOnPartialReason(t *testing.T) {
	v0, v1 := v0TestEnv(t)
	if err := os.WriteFile(filepath.Join(v0, "agents", "drift.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("modify v0: %v", err)
	}
	plan, err := planV0Cleanup(v1, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.PartialReason == "" {
		t.Fatal("setup: expected PartialReason for divergence")
	}
	if err := applyV0CleanPlan(plan, v0CleanLogger()); err == nil {
		t.Error("apply succeeded despite PartialReason without --force")
	}
	// v0 should remain in place.
	if _, err := os.Stat(v0); err != nil {
		t.Errorf("v0 missing after refused apply: %v", err)
	}
}

// TestApplyV0CleanPlan_ForceOverridesPartialReason applies anyway when
// ForceUsed is set.
func TestApplyV0CleanPlan_ForceOverridesPartialReason(t *testing.T) {
	v0, v1 := v0TestEnv(t)
	if err := os.WriteFile(filepath.Join(v0, "agents", "drift.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("modify v0: %v", err)
	}
	plan, err := planV0Cleanup(v1, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	plan.ForceUsed = true
	if err := applyV0CleanPlan(plan, v0CleanLogger()); err != nil {
		t.Fatalf("apply with --force: %v", err)
	}
	if _, err := os.Stat(v0); !os.IsNotExist(err) {
		t.Errorf("v0 still present after --force apply: %v", err)
	}
}

// TestPlanV0Cleanup_NonDirRefused: v0 path exists but is a regular
// file (not a directory). Plan must record PartialReason WITHOUT
// ForceableReason so --clean-force cannot override.
func TestPlanV0Cleanup_NonDirRefused(t *testing.T) {
	root := t.TempDir()
	v0 := filepath.Join(root, "kojo")
	v1 := filepath.Join(root, "kojo-v1")
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}
	if err := os.WriteFile(v0, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("seed v0 as file: %v", err)
	}
	v0PathOverride = func() string { return v0 }
	t.Cleanup(func() { v0PathOverride = nil })

	plan, err := planV0Cleanup(v1, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.PartialReason == "" {
		t.Error("expected PartialReason for non-dir v0 path")
	}
	if plan.ForceableReason != "" {
		t.Error("non-dir block must NOT be force-overridable")
	}
	plan.ForceUsed = true
	if err := applyV0CleanPlan(plan, v0CleanLogger()); err == nil {
		t.Error("apply with --force succeeded despite non-overridable block")
	}
}

// TestPlanV0Cleanup_SymlinkRefused: v0 path is a symlink. Refused,
// not force-able.
func TestPlanV0Cleanup_SymlinkRefused(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real")
	v0 := filepath.Join(root, "kojo")
	v1 := filepath.Join(root, "kojo-v1")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}
	if err := os.Symlink(target, v0); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	v0PathOverride = func() string { return v0 }
	t.Cleanup(func() { v0PathOverride = nil })

	plan, err := planV0Cleanup(v1, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.PartialReason == "" {
		t.Error("expected PartialReason for symlink v0 path")
	}
	if plan.ForceableReason != "" {
		t.Error("symlink block must NOT be force-overridable")
	}
}

// TestPlanV0Cleanup_NoCompleteFileNotForceable: missing
// migration_complete.json blocks AND is not force-overridable.
func TestPlanV0Cleanup_NoCompleteFileNotForceable(t *testing.T) {
	root := t.TempDir()
	v0 := filepath.Join(root, "kojo")
	v1 := filepath.Join(root, "kojo-v1")
	if err := os.MkdirAll(v0, 0o755); err != nil {
		t.Fatalf("mkdir v0: %v", err)
	}
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}
	v0PathOverride = func() string { return v0 }
	t.Cleanup(func() { v0PathOverride = nil })

	plan, err := planV0Cleanup(v1, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.PartialReason == "" {
		t.Fatal("expected PartialReason for missing complete file")
	}
	if plan.ForceableReason != "" {
		t.Error("missing-complete-file block must NOT be force-overridable")
	}
	plan.ForceUsed = true
	if err := applyV0CleanPlan(plan, v0CleanLogger()); err == nil {
		t.Error("apply with --force succeeded despite non-overridable block")
	}
}

// TestPlanV0Cleanup_ManifestDivergenceForceable: manifest divergence
// IS the case --clean-force overrides.
func TestPlanV0Cleanup_ManifestDivergenceForceable(t *testing.T) {
	v0, v1 := v0TestEnv(t)
	if err := os.WriteFile(filepath.Join(v0, "agents", "drift.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("modify v0: %v", err)
	}
	plan, err := planV0Cleanup(v1, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.ForceableReason == "" {
		t.Error("manifest divergence must set ForceableReason")
	}
	if plan.ForceableReason != plan.PartialReason {
		t.Error("ForceableReason must equal PartialReason for manifest divergence")
	}
}

// TestApplyV0CleanPlan_ApplyTimeDriftToSymlink: plan was clean, but
// between plan and apply v0 became a symlink. Re-validation must
// catch this (NOT force-able).
func TestApplyV0CleanPlan_ApplyTimeDriftToSymlink(t *testing.T) {
	v0, v1 := v0TestEnv(t)
	plan, err := planV0Cleanup(v1, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.PartialReason != "" {
		t.Fatalf("setup: expected clean plan, got blocked: %s", plan.PartialReason)
	}
	// Drift v0 to a symlink (replace dir with link).
	if err := os.RemoveAll(v0); err != nil {
		t.Fatalf("remove v0: %v", err)
	}
	target := filepath.Join(filepath.Dir(v0), "real")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.Symlink(target, v0); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	if err := applyV0CleanPlan(plan, v0CleanLogger()); err == nil {
		t.Error("apply succeeded despite v0 becoming a symlink between plan and apply")
	}
}

// TestApplyV0CleanPlan_ApplyTimeDriftToNonDir: plan was clean, but
// v0 became a regular file. Re-validation must catch this.
func TestApplyV0CleanPlan_ApplyTimeDriftToNonDir(t *testing.T) {
	v0, v1 := v0TestEnv(t)
	plan, err := planV0Cleanup(v1, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := os.RemoveAll(v0); err != nil {
		t.Fatalf("remove v0: %v", err)
	}
	if err := os.WriteFile(v0, []byte("file now"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := applyV0CleanPlan(plan, v0CleanLogger()); err == nil {
		t.Error("apply succeeded despite v0 becoming a file between plan and apply")
	}
}

// TestApplyV0CleanPlan_ApplyTimeManifestDriftRefuse: plan was clean,
// but manifest drifted between plan and apply. Without --force, refuse.
func TestApplyV0CleanPlan_ApplyTimeManifestDriftRefuse(t *testing.T) {
	v0, v1 := v0TestEnv(t)
	plan, err := planV0Cleanup(v1, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	// Drift the manifest by adding a file.
	if err := os.WriteFile(filepath.Join(v0, "agents", "drift_post_plan.txt"), []byte("post"), 0o644); err != nil {
		t.Fatalf("drift v0: %v", err)
	}
	if err := applyV0CleanPlan(plan, v0CleanLogger()); err == nil {
		t.Error("apply succeeded despite post-plan manifest drift without --force")
	}
}

// TestApplyV0CleanPlan_ApplyTimeManifestDriftWithForce: with
// ForceUsed set, post-plan manifest drift is allowed.
func TestApplyV0CleanPlan_ApplyTimeManifestDriftWithForce(t *testing.T) {
	v0, v1 := v0TestEnv(t)
	plan, err := planV0Cleanup(v1, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(v0, "agents", "drift_post_plan.txt"), []byte("post"), 0o644); err != nil {
		t.Fatalf("drift v0: %v", err)
	}
	plan.ForceUsed = true
	if err := applyV0CleanPlan(plan, v0CleanLogger()); err != nil {
		t.Errorf("apply with --force should accept post-plan drift, got %v", err)
	}
}

// TestPlanV0Cleanup_V0PathMismatchRefused: migration_complete.json
// records a v0 path that doesn't match the cleanup target. Refused,
// not force-able.
func TestPlanV0Cleanup_V0PathMismatchRefused(t *testing.T) {
	v0, v1 := v0TestEnv(t)
	// Rewrite migration_complete.json with a different v0 path.
	bogus := filepath.Join(filepath.Dir(v0), "kojo-other")
	manifest, _ := migrate.ManifestSHA256(v0)
	cf := migrate.CompleteFile{
		V0Path:           bogus,
		V1SchemaVersion:  1,
		V0SHA256Manifest: manifest,
		MigratorVersion:  "test",
		CompletedAt:      time.Now().UnixMilli(),
	}
	body, _ := json.Marshal(&cf)
	if err := os.WriteFile(filepath.Join(v1, migrate.CompleteFileName), body, 0o644); err != nil {
		t.Fatalf("rewrite complete: %v", err)
	}

	plan, err := planV0Cleanup(v1, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.PartialReason == "" {
		t.Error("expected PartialReason for v0_path mismatch")
	}
	if plan.ForceableReason != "" {
		t.Error("v0_path mismatch must NOT be force-overridable")
	}
	plan.ForceUsed = true
	if err := applyV0CleanPlan(plan, v0CleanLogger()); err == nil {
		t.Error("apply with --force succeeded despite v0_path mismatch")
	}
}

// TestApplyV0CleanPlan_RefuseClobberTrash refuses if trash path
// already exists.
func TestApplyV0CleanPlan_RefuseClobberTrash(t *testing.T) {
	v0, v1 := v0TestEnv(t)
	plan, err := planV0Cleanup(v1, v0CleanLogger())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := os.MkdirAll(plan.TrashPath, 0o755); err != nil {
		t.Fatalf("seed trash: %v", err)
	}
	if err := applyV0CleanPlan(plan, v0CleanLogger()); err == nil {
		t.Error("apply succeeded despite pre-existing trash dir")
	}
	if _, err := os.Stat(v0); err != nil {
		t.Errorf("v0 missing after refused apply: %v", err)
	}
}

// TestRunCleanCommandV0EndToEnd exercises the full --clean v0 flow.
func TestRunCleanCommandV0EndToEnd(t *testing.T) {
	v0, v1 := v0TestEnv(t)
	rc := runCleanCommand(cleanFlags{
		target:        "v0",
		apply:         true,
		logger:        v0CleanLogger(),
		configDirPath: v1,
	})
	if rc != 0 {
		t.Errorf("rc = %d, want 0", rc)
	}
	if _, err := os.Stat(v0); !os.IsNotExist(err) {
		t.Errorf("v0 still present after clean v0: %v", err)
	}
}

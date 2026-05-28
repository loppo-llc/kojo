package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/snapshot"
	"github.com/loppo-llc/kojo/internal/store"
)

// makeSnapshot creates a real snapshot directory with a valid manifest
// at the given mtime. Returns the directory path.
func makeSnapshot(t *testing.T, root string, age time.Duration, name string) string {
	t.Helper()
	dir := filepath.Join(root, "snapshots", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Stub manifest just to satisfy LoadManifest; full snapshot test
	// lives in internal/snapshot.
	m := map[string]any{
		"version":        1,
		"started_at":     time.Now().Add(-age).UTC().Format(time.RFC3339),
		"schema_version": 1,
		"db_sha256":      "0000000000000000000000000000000000000000000000000000000000000000",
		"db_size":        0,
		"blob_scopes":    []string{},
		"host_hint":      "test",
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, snapshot.ManifestFileName), data, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, snapshot.DBFileName), nil, 0o600); err != nil {
		t.Fatalf("write db: %v", err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(dir, when, when); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return dir
}

func makePartial(t *testing.T, root string, age time.Duration, name string) string {
	t.Helper()
	dir := filepath.Join(root, "snapshots", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// No manifest -> partial.
	if err := os.WriteFile(filepath.Join(dir, snapshot.DBFileName), nil, 0o600); err != nil {
		t.Fatalf("write db: %v", err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(dir, when, when); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return dir
}

func TestPlanSnapshotCleanupClassifies(t *testing.T) {
	root := t.TempDir()
	makeSnapshot(t, root, 30*24*time.Hour, "old1")
	makeSnapshot(t, root, 20*24*time.Hour, "old2")
	makeSnapshot(t, root, 1*time.Hour, "fresh1")
	makeSnapshot(t, root, 5*time.Minute, "fresh2")
	makePartial(t, root, 12*time.Hour, "abandoned")

	plan, err := planSnapshotCleanup(cleanFlags{
		configDirPath: root,
		maxAgeDays:    7,
		keepLatest:    1,
	})
	if err != nil {
		t.Fatalf("planSnapshotCleanup: %v", err)
	}

	// Two stale snapshots (old1, old2). One partial. fresh2 kept by
	// keep-latest=1, fresh1 kept by mtime-within-cutoff.
	if len(plan.PartialSnapshots) != 1 {
		t.Errorf("PartialSnapshots = %d, want 1", len(plan.PartialSnapshots))
	}
	if len(plan.StaleSnapshots) != 2 {
		t.Errorf("StaleSnapshots = %d, want 2", len(plan.StaleSnapshots))
	}
	if len(plan.Kept) != 2 {
		t.Errorf("Kept = %d, want 2", len(plan.Kept))
	}
}

func TestPlanSnapshotCleanupKeepsLatestEvenIfAged(t *testing.T) {
	root := t.TempDir()
	makeSnapshot(t, root, 30*24*time.Hour, "old1")
	makeSnapshot(t, root, 20*24*time.Hour, "old2")
	plan, err := planSnapshotCleanup(cleanFlags{
		configDirPath: root,
		maxAgeDays:    7,
		keepLatest:    1, // 1 most-recent must be kept regardless of age
	})
	if err != nil {
		t.Fatalf("planSnapshotCleanup: %v", err)
	}
	if len(plan.Kept) != 1 {
		t.Errorf("Kept = %d, want 1", len(plan.Kept))
	}
	if len(plan.StaleSnapshots) != 1 {
		t.Errorf("StaleSnapshots = %d, want 1", len(plan.StaleSnapshots))
	}
}

func TestApplyCleanPlanRemovesEntries(t *testing.T) {
	root := t.TempDir()
	stale := makeSnapshot(t, root, 30*24*time.Hour, "old1")
	partial := makePartial(t, root, 12*time.Hour, "abandoned")
	keep := makeSnapshot(t, root, 1*time.Hour, "fresh1")

	plan, err := planSnapshotCleanup(cleanFlags{
		configDirPath: root,
		maxAgeDays:    7,
		keepLatest:    1,
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if errs := applyCleanPlan(plan); len(errs) > 0 {
		t.Fatalf("apply: %v", errs)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale survived: %v", err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Errorf("partial survived: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("kept entry was removed: %v", err)
	}
}

func TestPlanSnapshotCleanupHandlesMissingDir(t *testing.T) {
	root := t.TempDir()
	plan, err := planSnapshotCleanup(cleanFlags{
		configDirPath: root,
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.StaleSnapshots) != 0 || len(plan.PartialSnapshots) != 0 {
		t.Errorf("plan = %+v, want empty", plan)
	}
}

// Sanity: make sure the snapshot package imports stay alive even when
// only LoadManifest is referenced via the helper.
var _ = func() error {
	_, err := store.Open(context.Background(), store.Options{})
	return err
}

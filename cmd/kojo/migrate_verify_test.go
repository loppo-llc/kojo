package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/migrate"
	"github.com/loppo-llc/kojo/internal/store"
)

// seedVerifyEnv builds a paired v0 + v1 dir on disk:
//   - v0 holds one seed file
//   - v1 holds a real kojo.db (opened once so the schema is applied)
//   - v1/migration_complete.json points to v0 and records v0's manifest
//
// Returns (v0, v1, completePath) so tests can mutate v0 and re-call
// verifyCompleteFile.
func seedVerifyEnv(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	v0 := filepath.Join(root, "kojo")
	v1 := filepath.Join(root, "kojo-v1")
	if err := os.MkdirAll(v0, 0o755); err != nil {
		t.Fatalf("mkdir v0: %v", err)
	}
	if err := os.WriteFile(filepath.Join(v0, "seed.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("seed v0: %v", err)
	}
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}

	// Open + close to materialize kojo.db at the current schema_version.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := store.Open(ctx, store.Options{ConfigDir: v1})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	dbVer, err := st.SchemaVersion(ctx)
	if err != nil {
		st.Close()
		t.Fatalf("SchemaVersion: %v", err)
	}
	st.Close()

	manifest, err := migrate.ManifestSHA256(v0)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	cf := migrate.CompleteFile{
		V0Path:           v0,
		V1SchemaVersion:  dbVer,
		V0SHA256Manifest: manifest,
		MigratorVersion:  "test",
		CompletedAt:      time.Now().UnixMilli(),
	}
	body, _ := json.Marshal(&cf)
	completePath := filepath.Join(v1, migrate.CompleteFileName)
	if err := os.WriteFile(completePath, body, 0o600); err != nil {
		t.Fatalf("write complete: %v", err)
	}
	return v0, v1, completePath
}

// TestVerifyCompleteFile_HappyPath sanity-checks the seeded fixture:
// matching manifest + valid kojo.db → no error.
func TestVerifyCompleteFile_HappyPath(t *testing.T) {
	_, _, completePath := seedVerifyEnv(t)
	if err := verifyCompleteFile(completePath); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestVerifyCompleteFile_AllowsV0Divergence is the regression test for
// "v1移行完了後に ~/.config/kojo が残っている状態で引数なしで起動するとガードで起動しない".
// Before this fix the boot path re-walked the v0 manifest and refused to
// start when v0 was edited post-migration; that gate now lives only in
// `kojo --clean v0` (see verifyCompleteFile doc).
func TestVerifyCompleteFile_AllowsV0Divergence(t *testing.T) {
	v0, _, completePath := seedVerifyEnv(t)
	// Mutate v0 after migration_complete.json was stamped — this is the
	// "operator touched v0 since migration" case.
	if err := os.WriteFile(filepath.Join(v0, "seed.txt"), []byte("mutated"), 0o644); err != nil {
		t.Fatalf("mutate v0: %v", err)
	}
	if err := verifyCompleteFile(completePath); err != nil {
		t.Fatalf("boot must not refuse on v0 manifest divergence, got %v", err)
	}
}

// TestVerifyCompleteFile_AllowsV0Missing covers the cleaned-up steady
// state (v0 soft-deleted via `kojo --clean v0`). docs §5.9 calls this
// out as the expected post-cleanup boot.
func TestVerifyCompleteFile_AllowsV0Missing(t *testing.T) {
	v0, _, completePath := seedVerifyEnv(t)
	if err := os.RemoveAll(v0); err != nil {
		t.Fatalf("rm v0: %v", err)
	}
	if err := verifyCompleteFile(completePath); err != nil {
		t.Fatalf("boot must not refuse when v0 is gone, got %v", err)
	}
}

// TestVerifyCompleteFile_AllowsMissingV0Manifest pins the policy that
// `v0_sha256_manifest` is no longer a boot-required field. Older
// `migration_complete.json` that pre-dates the manifest field must boot;
// only `kojo --clean v0` continues to require it for `--clean-force`
// (see cmd/kojo/clean_v0.go).
func TestVerifyCompleteFile_AllowsMissingV0Manifest(t *testing.T) {
	_, _, completePath := seedVerifyEnv(t)
	body, err := os.ReadFile(completePath)
	if err != nil {
		t.Fatalf("read complete: %v", err)
	}
	var cf migrate.CompleteFile
	if err := json.Unmarshal(body, &cf); err != nil {
		t.Fatalf("parse complete: %v", err)
	}
	cf.V0SHA256Manifest = ""
	out, _ := json.Marshal(&cf)
	if err := os.WriteFile(completePath, out, 0o600); err != nil {
		t.Fatalf("rewrite complete: %v", err)
	}
	if err := verifyCompleteFile(completePath); err != nil {
		t.Fatalf("boot must accept missing v0_sha256_manifest, got %v", err)
	}
}

package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

func openSnapshotStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(context.Background(), store.Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func TestSnapshotTakeMinimum(t *testing.T) {
	s, root := openSnapshotStore(t)
	ctx := context.Background()

	// Insert a row so the snapshot has actual content.
	if _, err := s.InsertAgent(ctx, &store.AgentRecord{
		ID: "ag_1", Name: "Alice",
	}, store.AgentInsertOptions{}); err != nil {
		t.Fatalf("InsertAgent: %v", err)
	}

	now := time.Date(2026, 5, 1, 1, 2, 3, 0, time.UTC)
	dir, err := Take(ctx, s, filepath.Join(root, "blobs"), root, Options{
		Now: now, HostHint: "test-host",
	})
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	want := filepath.Join(root, "snapshots", "20260501T010203Z")
	if dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	if _, err := os.Stat(filepath.Join(dir, DBFileName)); err != nil {
		t.Errorf("kojo.db missing: %v", err)
	}
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.HostHint != "test-host" {
		t.Errorf("HostHint = %q", m.HostHint)
	}
	if m.SchemaVersion < 1 {
		t.Errorf("SchemaVersion = %d", m.SchemaVersion)
	}
	if m.DBSHA256 == "" || m.DBSize <= 0 {
		t.Errorf("manifest hash/size empty")
	}
	if err := VerifyDB(dir); err != nil {
		t.Errorf("VerifyDB: %v", err)
	}
}

func TestSnapshotIncludesGlobalBlobs(t *testing.T) {
	s, root := openSnapshotStore(t)
	ctx := context.Background()

	// Materialize a blob tree: blobs/global/agents/ag_1/avatar.png
	blobRoot := filepath.Join(root, "blobs")
	target := filepath.Join(blobRoot, "global", "agents", "ag_1", "avatar.png")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	want := []byte("fake-png")
	if err := os.WriteFile(target, want, 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}

	dir, err := Take(ctx, s, blobRoot, root, Options{Now: time.Now()})
	if err != nil {
		t.Fatalf("Take: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "blobs", "global", "agents", "ag_1", "avatar.png"))
	if err != nil {
		t.Fatalf("read snapshot blob: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("blob content mismatch")
	}

	m, _ := LoadManifest(dir)
	if len(m.BlobScopes) != 1 || m.BlobScopes[0] != "global" {
		t.Errorf("BlobScopes = %v, want [global]", m.BlobScopes)
	}
}

func TestSnapshotSkipsNonGlobalScopes(t *testing.T) {
	s, root := openSnapshotStore(t)
	ctx := context.Background()

	// Create a local-scope blob that should NOT be copied.
	blobRoot := filepath.Join(root, "blobs")
	local := filepath.Join(blobRoot, "local", "outbox", "tmp.bin")
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(local, []byte("local-only"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	dir, err := Take(ctx, s, blobRoot, root, Options{Now: time.Now()})
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", "local")); err == nil {
		t.Error("snapshot must NOT contain blobs/local")
	}
}

func TestSnapshotVacuumIntoRefusesQuote(t *testing.T) {
	s, root := openSnapshotStore(t)
	// A single-quote in the path would break the literal SQL string.
	dst := filepath.Join(root, "snapshots", "weird'name")
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := vacuumInto(context.Background(), s, dst); err == nil {
		t.Error("vacuumInto accepted path with single quote")
	}
}

func TestVerifyDBDetectsTamper(t *testing.T) {
	s, root := openSnapshotStore(t)
	ctx := context.Background()
	dir, err := Take(ctx, s, filepath.Join(root, "blobs"), root, Options{Now: time.Now()})
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	dbPath := filepath.Join(dir, DBFileName)
	// Truncate by 1 byte to break the hash.
	info, _ := os.Stat(dbPath)
	if err := os.Truncate(dbPath, info.Size()-1); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if err := VerifyDB(dir); err == nil {
		t.Error("VerifyDB accepted tampered db")
	}
}

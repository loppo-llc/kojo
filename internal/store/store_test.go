package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenAppliesMigrations(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	s, err := Open(ctx, Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	v, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v < 1 {
		t.Fatalf("expected schema_version >= 1, got %d", v)
	}

	// schema sanity: a few key tables must exist.
	for _, table := range []string{
		"agents", "agent_messages", "agent_memory", "memory_entries",
		"kv", "blob_refs", "agent_locks", "agent_fencing_counters", "peer_registry",
		"migration_status", "idempotency_keys", "cron_runs",
		"groupdms", "groupdm_messages", "sessions", "events",
	} {
		var got string
		err := s.DB().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&got)
		if err != nil {
			t.Errorf("table %q missing: %v", table, err)
		}
	}

	// re-Open is idempotent; version should not advance.
	s.Close()
	s2, err := Open(ctx, Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer s2.Close()
	v2, err := s2.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("re-Open SchemaVersion: %v", err)
	}
	if v2 != v {
		t.Errorf("schema_version after re-Open: got %d want %d", v2, v)
	}
}

func TestParseMigrationName(t *testing.T) {
	cases := []struct {
		in       string
		wantV    int
		wantName string
		wantErr  bool
	}{
		{"0001_initial.sql", 1, "initial", false},
		{"0042_add_index.sql", 42, "add_index", false},
		{"bad.sql", 0, "", true},
		{"0001.sql", 0, "", true},
		{"_initial.sql", 0, "", true},
	}
	for _, tc := range cases {
		v, name, err := parseMigrationName(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("%q: err=%v wantErr=%v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && (v != tc.wantV || name != tc.wantName) {
			t.Errorf("%q: got (%d,%q) want (%d,%q)", tc.in, v, name, tc.wantV, tc.wantName)
		}
	}
}

func TestCanonicalETagDeterministic(t *testing.T) {
	// Same logical content, different key insertion order → same etag.
	a := map[string]any{"id": "ag_1", "name": "Hana", "settings_json": `{"a":1}`}
	b := map[string]any{"settings_json": `{"a":1}`, "name": "Hana", "id": "ag_1"}

	ea, err := CanonicalETag(1, a)
	if err != nil {
		t.Fatalf("etag a: %v", err)
	}
	eb, err := CanonicalETag(1, b)
	if err != nil {
		t.Fatalf("etag b: %v", err)
	}
	if ea != eb {
		t.Errorf("canonical etag must be order-independent: %s vs %s", ea, eb)
	}

	// Version bump must change the etag prefix.
	ec, err := CanonicalETag(2, a)
	if err != nil {
		t.Fatalf("etag c: %v", err)
	}
	if !strings.HasPrefix(ec, "2-") || strings.HasPrefix(ec, "1-") {
		t.Errorf("expected version-2 prefix, got %q", ec)
	}
}

func TestNextGlobalSeqMonotonic(t *testing.T) {
	prev := NextGlobalSeq()
	for i := 0; i < 1000; i++ {
		cur := NextGlobalSeq()
		if cur <= prev {
			t.Fatalf("seq not monotonic at i=%d: %d <= %d", i, cur, prev)
		}
		prev = cur
	}
}

func TestOpenRejectsMissingConfigDir(t *testing.T) {
	_, err := Open(context.Background(), Options{})
	if err == nil {
		t.Fatal("expected error when no path given")
	}
}

func TestOpenWithExplicitPath(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "subdir", "alt.db")
	s, err := Open(context.Background(), Options{Path: dbPath})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if s.Path() != dbPath {
		t.Errorf("Path() = %q, want %q", s.Path(), dbPath)
	}
	// Sanity probe: WAL mode is on.
	var mode string
	if err := s.DB().QueryRowContext(context.Background(),
		`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if strings.ToLower(mode) != "wal" {
		t.Errorf("journal_mode = %q, want WAL", mode)
	}
}

func TestSchemaVersionEmptyOnReadOnlyMissing(t *testing.T) {
	// Open then close to materialize the file, then re-open read-only.
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(ctx, Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.Close()

	ro, err := Open(ctx, Options{ConfigDir: dir, ReadOnly: true})
	if err != nil {
		t.Fatalf("ReadOnly Open: %v", err)
	}
	defer ro.Close()

	v, err := ro.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v < 1 {
		t.Errorf("expected non-zero schema version, got %d", v)
	}

	// Verify ReadOnly really blocks writes.
	_, err = ro.DB().ExecContext(ctx, `INSERT INTO migration_status (domain, phase) VALUES ('x','pending')`)
	if err == nil {
		t.Error("ReadOnly: expected write to fail")
	}
	_ = sql.ErrNoRows // keep import alive in case future tests want it
}

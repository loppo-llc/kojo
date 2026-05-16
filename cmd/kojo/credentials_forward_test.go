package main

import (
	"context"
	"database/sql"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// credentialDBSchema mirrors the CREATE TABLE statements that
// internal/agent/credential.go (createCredentialTable + createTokenTable
// + the settings DDL in NewCredentialStore) emits at startup. Kept in
// one place so seeding helpers stay byte-compatible with a real v1 boot.
const credentialDBSchema = `
CREATE TABLE credentials (
  id TEXT NOT NULL, agent_id TEXT NOT NULL, label TEXT NOT NULL,
  username TEXT NOT NULL, password_enc TEXT NOT NULL DEFAULT '',
  totp_secret_enc TEXT NOT NULL DEFAULT '',
  totp_algorithm TEXT NOT NULL DEFAULT '',
  totp_digits INTEGER NOT NULL DEFAULT 0,
  totp_period INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
  PRIMARY KEY (agent_id, id));
CREATE TABLE notify_tokens (
  provider TEXT NOT NULL, agent_id TEXT NOT NULL,
  source_id TEXT NOT NULL, key TEXT NOT NULL,
  value_enc TEXT NOT NULL,
  expires_at INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (provider, agent_id, source_id, key));
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

// seedV0Creds writes a credentials.db with the canonical schema and the
// requested credential row count, plus a 32-byte credentials.key.
func seedV0Creds(t *testing.T, dir string, rows int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir v0: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "credentials.db"))
	if err != nil {
		t.Fatalf("open v0 db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(credentialDBSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	for i := 0; i < rows; i++ {
		if _, err := db.Exec(
			`INSERT INTO credentials (id,agent_id,label,username,created_at,updated_at)
			 VALUES (?, 'agent1', 'l', 'u', 't', 't')`,
			strings.Repeat("x", i+1)); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.key"),
		make([]byte, 32), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

// seedEmptyV1Schema writes a v1 credentials.db with the schema but zero
// rows in every user-owned table, plus a distinct key.
func seedEmptyV1Schema(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "credentials.db"))
	if err != nil {
		t.Fatalf("open v1 db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(credentialDBSchema); err != nil {
		t.Fatalf("create v1 schema: %v", err)
	}
	key := make([]byte, 32)
	key[0] = 0xff
	if err := os.WriteFile(filepath.Join(dir, "credentials.key"),
		key, 0o600); err != nil {
		t.Fatalf("write v1 key: %v", err)
	}
}

func TestCredentialsCarryForward_CopiesWhenV1Empty(t *testing.T) {
	root := t.TempDir()
	v0 := filepath.Join(root, "kojo")
	v1 := filepath.Join(root, "kojo-v1")
	seedV0Creds(t, v0, 3)
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}

	warns := applyCredentialsCarryForward(context.Background(), v0, v1, slog.Default())
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}

	hasRows, table, err := v1HasUserCredentialData(context.Background(),
		filepath.Join(v1, "credentials.db"))
	if err != nil {
		t.Fatalf("probe v1: %v", err)
	}
	if !hasRows || table != "credentials" {
		t.Fatalf("want hasRows=true table=credentials, got hasRows=%v table=%q", hasRows, table)
	}
	key, err := os.ReadFile(filepath.Join(v1, "credentials.key"))
	if err != nil {
		t.Fatalf("read v1 key: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("v1 key should be 32 bytes after carry, got %d", len(key))
	}
}

func TestCredentialsCarryForward_OverwritesEmptyV1Schema(t *testing.T) {
	// Schema-only v1 (operator launched v1 once before --migrate, all
	// tables present but no rows). Carry-forward should treat this as
	// "no user data" and overwrite both db and key.
	root := t.TempDir()
	v0 := filepath.Join(root, "kojo")
	v1 := filepath.Join(root, "kojo-v1")
	seedV0Creds(t, v0, 2)
	seedEmptyV1Schema(t, v1)

	before, _ := os.ReadFile(filepath.Join(v1, "credentials.key"))
	warns := applyCredentialsCarryForward(context.Background(), v0, v1, slog.Default())
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	hasRows, _, err := v1HasUserCredentialData(context.Background(),
		filepath.Join(v1, "credentials.db"))
	if err != nil || !hasRows {
		t.Fatalf("want hasRows=true err=nil, got hasRows=%v err=%v", hasRows, err)
	}
	after, _ := os.ReadFile(filepath.Join(v1, "credentials.key"))
	if string(before) == string(after) {
		t.Fatalf("v1 key was not replaced by v0 key — db/key pair broken")
	}
}

func TestCredentialsCarryForward_RefusesIfV1HasCredentialRows(t *testing.T) {
	root := t.TempDir()
	v0 := filepath.Join(root, "kojo")
	v1 := filepath.Join(root, "kojo-v1")
	seedV0Creds(t, v0, 5)
	seedEmptyV1Schema(t, v1)

	// Pre-existing v1 credential row — must be preserved.
	db, _ := sql.Open("sqlite", filepath.Join(v1, "credentials.db"))
	if _, err := db.Exec(
		`INSERT INTO credentials (id,agent_id,label,username,created_at,updated_at)
		 VALUES ('a','agent1','l','u','t','t')`); err != nil {
		t.Fatalf("seed v1 row: %v", err)
	}
	db.Close()

	warns := applyCredentialsCarryForward(context.Background(), v0, v1, slog.Default())
	if len(warns) == 0 {
		t.Fatalf("want a warning when v1 already has credential rows")
	}
	hasRows, table, _ := v1HasUserCredentialData(context.Background(),
		filepath.Join(v1, "credentials.db"))
	if !hasRows || table != "credentials" {
		t.Fatalf("v1 rows lost after refusal: hasRows=%v table=%q", hasRows, table)
	}
}

func TestCredentialsCarryForward_RefusesIfV1HasNotifyTokens(t *testing.T) {
	// The bug this guards: an earlier version of the carry-forward only
	// counted `credentials` rows. A user who entered notify tokens via
	// the OAuth flow before running --migrate would silently lose them.
	root := t.TempDir()
	v0 := filepath.Join(root, "kojo")
	v1 := filepath.Join(root, "kojo-v1")
	seedV0Creds(t, v0, 1)
	seedEmptyV1Schema(t, v1)

	db, _ := sql.Open("sqlite", filepath.Join(v1, "credentials.db"))
	if _, err := db.Exec(
		`INSERT INTO notify_tokens (provider,agent_id,source_id,key,value_enc,updated_at)
		 VALUES ('gmail','agent1','src1','access','enc','t')`); err != nil {
		t.Fatalf("seed v1 notify_tokens: %v", err)
	}
	db.Close()

	warns := applyCredentialsCarryForward(context.Background(), v0, v1, slog.Default())
	if len(warns) == 0 || !strings.Contains(warns[0], "notify_tokens") {
		t.Fatalf("want warning naming notify_tokens, got %v", warns)
	}
}

func TestCredentialsCarryForward_RefusesIfV1HasSettings(t *testing.T) {
	root := t.TempDir()
	v0 := filepath.Join(root, "kojo")
	v1 := filepath.Join(root, "kojo-v1")
	seedV0Creds(t, v0, 1)
	seedEmptyV1Schema(t, v1)

	db, _ := sql.Open("sqlite", filepath.Join(v1, "credentials.db"))
	if _, err := db.Exec(
		`INSERT INTO settings (key,value) VALUES ('embedding_model','foo')`); err != nil {
		t.Fatalf("seed v1 settings: %v", err)
	}
	db.Close()

	warns := applyCredentialsCarryForward(context.Background(), v0, v1, slog.Default())
	if len(warns) == 0 || !strings.Contains(warns[0], "settings") {
		t.Fatalf("want warning naming settings, got %v", warns)
	}
}

func TestCredentialsCarryForward_RefusesIfV1DBCorrupt(t *testing.T) {
	// A v1 credentials.db that is not a valid SQLite file must NOT be
	// silently overwritten — that could mask a real disk problem or
	// destroy a damaged-but-recoverable store.
	root := t.TempDir()
	v0 := filepath.Join(root, "kojo")
	v1 := filepath.Join(root, "kojo-v1")
	seedV0Creds(t, v0, 1)
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(v1, "credentials.db"),
		[]byte("not a sqlite file"), 0o600); err != nil {
		t.Fatalf("write corrupt v1 db: %v", err)
	}

	warns := applyCredentialsCarryForward(context.Background(), v0, v1, slog.Default())
	if len(warns) == 0 {
		t.Fatalf("want warning for corrupt v1 db")
	}
	// Corrupt file must remain untouched.
	body, _ := os.ReadFile(filepath.Join(v1, "credentials.db"))
	if string(body) != "not a sqlite file" {
		t.Fatalf("v1 credentials.db was overwritten despite corruption")
	}
}

func TestCredentialsCarryForward_NoV0DBIsSilentNoop(t *testing.T) {
	root := t.TempDir()
	v0 := filepath.Join(root, "kojo")
	v1 := filepath.Join(root, "kojo-v1")
	if err := os.MkdirAll(v0, 0o755); err != nil {
		t.Fatalf("mkdir v0: %v", err)
	}
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}

	warns := applyCredentialsCarryForward(context.Background(), v0, v1, slog.Default())
	if len(warns) != 0 {
		t.Fatalf("pre-SQLite v0 should be silent no-op, got: %v", warns)
	}
	for _, leaf := range []string{"credentials.db", "credentials.db-wal", "credentials.db-shm", "credentials.key"} {
		if _, err := os.Stat(filepath.Join(v1, leaf)); err == nil {
			t.Fatalf("v1/%s should not have been created", leaf)
		}
	}
}

func TestCredentialsCarryForward_RefusesIfV0KeyMissing(t *testing.T) {
	root := t.TempDir()
	v0 := filepath.Join(root, "kojo")
	v1 := filepath.Join(root, "kojo-v1")
	seedV0Creds(t, v0, 1)
	if err := os.Remove(filepath.Join(v0, "credentials.key")); err != nil {
		t.Fatalf("remove v0 key: %v", err)
	}
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}

	warns := applyCredentialsCarryForward(context.Background(), v0, v1, slog.Default())
	if len(warns) == 0 {
		t.Fatalf("want a warning when v0 has db without key")
	}
	if _, err := os.Stat(filepath.Join(v1, "credentials.db")); err == nil {
		t.Fatalf("v1/credentials.db must not be copied without key")
	}
}

func TestCredentialsCarryForward_RefusesIfV0KeyWrongSize(t *testing.T) {
	// 31-byte key — internal/agent/credential.go's loadOrCreateKey would
	// fail-close on this. Refusing here surfaces the problem instead of
	// shipping a broken pair to v1.
	root := t.TempDir()
	v0 := filepath.Join(root, "kojo")
	v1 := filepath.Join(root, "kojo-v1")
	seedV0Creds(t, v0, 1)
	if err := os.WriteFile(filepath.Join(v0, "credentials.key"),
		make([]byte, 31), 0o600); err != nil {
		t.Fatalf("write short v0 key: %v", err)
	}
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}

	warns := applyCredentialsCarryForward(context.Background(), v0, v1, slog.Default())
	if len(warns) == 0 || !strings.Contains(warns[0], "32 bytes") && !strings.Contains(warns[0], "got 31") {
		t.Fatalf("want warning naming size mismatch, got %v", warns)
	}
	if _, err := os.Stat(filepath.Join(v1, "credentials.db")); err == nil {
		t.Fatalf("v1/credentials.db must not be copied with malformed key")
	}
}

func TestCredentialsCarryForward_CopiesWAL(t *testing.T) {
	// If v0 has a WAL with uncheckpointed pages, it must travel with
	// the db — otherwise the v1 store sees a stale db.
	root := t.TempDir()
	v0 := filepath.Join(root, "kojo")
	v1 := filepath.Join(root, "kojo-v1")
	seedV0Creds(t, v0, 2)
	walPayload := []byte("synthetic wal contents")
	if err := os.WriteFile(filepath.Join(v0, "credentials.db-wal"),
		walPayload, 0o600); err != nil {
		t.Fatalf("write v0 wal: %v", err)
	}
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}

	warns := applyCredentialsCarryForward(context.Background(), v0, v1, slog.Default())
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	got, err := os.ReadFile(filepath.Join(v1, "credentials.db-wal"))
	if err != nil {
		t.Fatalf("read v1 wal: %v", err)
	}
	if string(got) != string(walPayload) {
		t.Fatalf("v1 wal contents diverge from v0")
	}
}

func TestCredentialsCarryForward_DropsStaleV1SHM(t *testing.T) {
	// Any pre-existing v1 db-shm must be removed: SQLite will rebuild
	// it from the new wal at next open. Leaving an old shm in place
	// would pair the new db with a stale memory-mapped index.
	root := t.TempDir()
	v0 := filepath.Join(root, "kojo")
	v1 := filepath.Join(root, "kojo-v1")
	seedV0Creds(t, v0, 1)
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(v1, "credentials.db-shm"),
		[]byte("stale-shm"), 0o600); err != nil {
		t.Fatalf("seed stale shm: %v", err)
	}

	warns := applyCredentialsCarryForward(context.Background(), v0, v1, slog.Default())
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	if _, err := os.Stat(filepath.Join(v1, "credentials.db-shm")); err == nil {
		t.Fatalf("stale v1 db-shm should have been removed")
	}
}

func TestCredentialsCarryForward_KeyPermissionsPreserved(t *testing.T) {
	root := t.TempDir()
	v0 := filepath.Join(root, "kojo")
	v1 := filepath.Join(root, "kojo-v1")
	seedV0Creds(t, v0, 1)
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}
	_ = applyCredentialsCarryForward(context.Background(), v0, v1, slog.Default())
	info, err := os.Stat(filepath.Join(v1, "credentials.key"))
	if err != nil {
		t.Fatalf("stat v1 key: %v", err)
	}
	mode := info.Mode() & fs.ModePerm
	if mode != 0o600 {
		t.Fatalf("v1 credentials.key mode = %v, want 0o600", mode)
	}
}

func TestCredentialsCarryForward_NoStrayTempsOnRefusal(t *testing.T) {
	// When the carry-forward refuses (e.g. v1 already has rows), no
	// *.tmp-* sibling must be left behind in v1.
	root := t.TempDir()
	v0 := filepath.Join(root, "kojo")
	v1 := filepath.Join(root, "kojo-v1")
	seedV0Creds(t, v0, 2)
	seedEmptyV1Schema(t, v1)
	db, _ := sql.Open("sqlite", filepath.Join(v1, "credentials.db"))
	if _, err := db.Exec(
		`INSERT INTO credentials (id,agent_id,label,username,created_at,updated_at)
		 VALUES ('x','a','l','u','t','t')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	db.Close()

	_ = applyCredentialsCarryForward(context.Background(), v0, v1, slog.Default())
	entries, _ := os.ReadDir(v1)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("stray temp file left in v1: %s", e.Name())
		}
	}
}

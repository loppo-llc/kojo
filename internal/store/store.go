// Package store owns the SQLite database that holds kojo v1's structured
// state. It is the single primary store for agents, messages, memory entries,
// tasks, kv settings, blob refs, peer registry, agent locks and migration
// status. Blob bodies (avatars, books, MEMORY.md, RAG indices) are stored on
// the filesystem under internal/blob and referenced by URI from blob_refs.
//
// Layout:
//
//	~/.config/kojo-v1/
//	  kojo.db                          # this database (WAL + SHM)
//	  kojo.db-wal
//	  kojo.db-shm
//	  global/  local/  machine/        # blob trees (managed by internal/blob)
//
// The database is opened with WAL journaling, foreign_keys ON, and
// busy_timeout=5000ms. Migrations are numbered SQL files embedded into the
// binary; on Open, all pending files are applied inside an explicit
// transaction in lexical order. The applied schema version is stored in the
// `schema_migrations` table and is exposed via Store.SchemaVersion().
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	_ "modernc.org/sqlite"
)

const (
	// DBFileName is the SQLite file name under the config dir.
	DBFileName = "kojo.db"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store wraps the SQLite database handle and exposes high-level helpers used
// by feature packages. The zero value is not usable; obtain one via Open.
type Store struct {
	db   *sql.DB
	path string

	mu     sync.Mutex // guards SchemaVersion read after Close
	closed bool

	// eventListener is the post-commit hook fired after every domain
	// mutation that records an events row. Loaded atomically so reads
	// in hot write paths (InsertAgent etc.) don't take a lock.
	eventListener atomic.Value // EventListener
}

// EventListener is invoked AFTER a tx that recorded one or more
// events table rows commits successfully. The listener fires once per
// recorded row, with the same seq the row holds in the events table —
// so a peer receiving a live broadcast can match it 1:1 against a
// `/api/v1/changes?since=<seq>` poll.
//
// Listeners run synchronously on the writer's goroutine; they MUST be
// fast and non-blocking. A typical implementation forwards to an
// in-memory broadcast bus and returns.
type EventListener func(EventRecord)

// SetEventListener installs (or replaces) the post-commit listener.
// Pass nil to disable.
func (s *Store) SetEventListener(l EventListener) {
	if l == nil {
		s.eventListener.Store(EventListener(nil))
		return
	}
	s.eventListener.Store(l)
}

// fireEvent invokes the listener if one is registered. Safe to call
// from any goroutine; listener absence is a fast no-op.
func (s *Store) fireEvent(e EventRecord) {
	v := s.eventListener.Load()
	if v == nil {
		return
	}
	l, ok := v.(EventListener)
	if !ok || l == nil {
		return
	}
	l(e)
}

// Options configures Open. All fields are optional.
type Options struct {
	// ConfigDir is the directory that will hold kojo.db. If empty, callers
	// should pass configdir.Path() explicitly — store has no implicit
	// dependency on configdir to keep this package testable.
	ConfigDir string

	// Path overrides ConfigDir/DBFileName. Used by tests and snapshots.
	Path string

	// ReadOnly opens the database read-only. Migrations are NOT applied.
	ReadOnly bool
}

// Open creates the config directory if needed, opens kojo.db with WAL and
// pragmas tuned for kojo's workload, and applies any pending migrations.
//
// On any error after the underlying *sql.DB has been opened, the handle is
// closed before returning so callers do not leak descriptors.
func Open(ctx context.Context, opts Options) (*Store, error) {
	dbPath := opts.Path
	if dbPath == "" {
		if opts.ConfigDir == "" {
			return nil, errors.New("store.Open: ConfigDir or Path required")
		}
		if err := os.MkdirAll(opts.ConfigDir, 0o755); err != nil {
			return nil, fmt.Errorf("store.Open: create config dir: %w", err)
		}
		dbPath = filepath.Join(opts.ConfigDir, DBFileName)
	} else {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			return nil, fmt.Errorf("store.Open: create parent dir: %w", err)
		}
	}

	dsn := buildDSN(dbPath, opts.ReadOnly)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store.Open: sql.Open: %w", err)
	}
	// modernc.org/sqlite serializes per-connection; allow a small pool.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store.Open: ping: %w", err)
	}

	s := &Store{db: db, path: dbPath}

	if !opts.ReadOnly {
		if err := s.ensureSchemaTable(ctx); err != nil {
			db.Close()
			return nil, err
		}
		if err := s.applyMigrations(ctx); err != nil {
			db.Close()
			return nil, err
		}
		// Re-seed the in-process global seq counter from the persisted MAX
		// across every table that carries a seq column. Without this, a
		// tight write loop in the previous run could have advanced seq past
		// the wall clock, and the next process would re-issue colliding
		// values until the clock caught up. Safe under the kojo.lock
		// invariant (single primary at a time).
		if err := s.seedGlobalSeq(ctx); err != nil {
			db.Close()
			return nil, err
		}
	}

	return s, nil
}

// seedGlobalSeq advances the process-local global seq counter to be at least
// MAX(seq) across every table whose `seq` is allocated globally. Per-(agent_id)
// or per-(groupdm_id) seqs are not in this set — those are allocated inside
// the relevant insert tx and don't share state with NextGlobalSeq().
func (s *Store) seedGlobalSeq(ctx context.Context) error {
	// UNION the per-table maxes. COALESCE(MAX,0) keeps the result well-defined
	// for an empty database. The list is the closed set of tables that use
	// the global allocator (see internal/store/agents.go,
	// internal/store/messages.go, etc. — search for NextGlobalSeq).
	const q = `
SELECT MAX(s) FROM (
  SELECT COALESCE(MAX(seq), 0) AS s FROM agents
  UNION ALL SELECT COALESCE(MAX(seq), 0) FROM agent_persona
  UNION ALL SELECT COALESCE(MAX(seq), 0) FROM agent_memory
  UNION ALL SELECT COALESCE(MAX(seq), 0) FROM groupdms
  UNION ALL SELECT COALESCE(MAX(seq), 0) FROM sessions
  UNION ALL SELECT COALESCE(MAX(seq), 0) FROM events
)`
	var max sql.NullInt64
	if err := s.db.QueryRowContext(ctx, q).Scan(&max); err != nil {
		return fmt.Errorf("store.seedGlobalSeq: %w", err)
	}
	if max.Valid && max.Int64 > 0 {
		// CAS the atomic to at least max — never roll back if another caller
		// has already raised it higher.
		for {
			cur := globalSeqClock.last.Load()
			if cur >= max.Int64 {
				return nil
			}
			if globalSeqClock.last.CompareAndSwap(cur, max.Int64) {
				return nil
			}
		}
	}
	return nil
}

// DB returns the underlying *sql.DB. Feature packages should keep usage
// scoped to short transactions and prefer the helpers in this package over
// raw SQL where possible so cross-cutting concerns (etag, seq) stay
// consistent.
func (s *Store) DB() *sql.DB { return s.db }

// Path returns the absolute path to kojo.db.
func (s *Store) Path() string { return s.path }

// Close flushes WAL and closes the database. Idempotent.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

// MaxSupportedSchemaVersion returns the highest migration version this
// build can apply. Callers (the startup gate, snapshot tools) compare it
// against an on-disk schema_version to detect "DB is from a newer build
// than this binary" — in which case starting up could silently mix
// incompatible state. Reads the embedded migration list at call time so
// the answer is always in sync with the binary's own migrations.
func MaxSupportedSchemaVersion() (int, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return 0, err
	}
	max := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, _, err := parseMigrationName(e.Name())
		if err != nil {
			return 0, err
		}
		if v > max {
			max = v
		}
	}
	return max, nil
}

// SchemaVersion returns the highest migration version applied. Returns 0 on
// a freshly-initialized database with no migrations.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// buildDSN composes a modernc.org/sqlite DSN with the pragmas kojo relies on.
// Pragmas are applied via _pragma= URI parameters so they take effect on
// every pooled connection (modernc opens connections on demand).
func buildDSN(path string, readOnly bool) string {
	var b strings.Builder
	b.WriteString("file:")
	b.WriteString(path)
	b.WriteString("?_pragma=journal_mode(WAL)")
	b.WriteString("&_pragma=foreign_keys(ON)")
	b.WriteString("&_pragma=busy_timeout(5000)")
	b.WriteString("&_pragma=synchronous(NORMAL)")
	if readOnly {
		b.WriteString("&mode=ro")
	} else {
		// _txlock=immediate forces sql.BeginTx to issue `BEGIN IMMEDIATE`.
		// Without this, deferred-mode read-then-write transactions (e.g.
		// AppendMessage's MAX(seq) → INSERT, UpdateAgent's SELECT → UPDATE)
		// can fail with SQLITE_BUSY_SNAPSHOT under concurrent writers
		// because WAL detects a competing write occurred between BEGIN and
		// the first write statement, and busy_timeout doesn't retry that
		// class of error. Immediate-mode acquires the writer lock up front,
		// which serializes our writers cleanly under WAL.
		b.WriteString("&_txlock=immediate")
	}
	return b.String()
}

func (s *Store) ensureSchemaTable(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  applied_at INTEGER NOT NULL
);`
	_, err := s.db.ExecContext(ctx, ddl)
	if err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	return nil
}

type migrationFile struct {
	version int
	name    string
	body    string
}

// applyMigrations runs every embedded migration whose version is greater than
// the highest applied version, in ascending order. Each file is applied in a
// dedicated transaction; if a file fails, the transaction is rolled back and
// the error is returned without touching subsequent files.
//
// Migration filenames must match `NNNN_name.sql` where NNNN is a
// zero-padded integer. Files that fail to parse this way are a build-time
// error — surfacing them early avoids silent skips.
func (s *Store) applyMigrations(ctx context.Context) error {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("store: read embedded migrations: %w", err)
	}

	files := make([]migrationFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, name, err := parseMigrationName(e.Name())
		if err != nil {
			return fmt.Errorf("store: %w", err)
		}
		body, err := fs.ReadFile(migrationsFS, "migrations/"+e.Name())
		if err != nil {
			return fmt.Errorf("store: read %s: %w", e.Name(), err)
		}
		files = append(files, migrationFile{version: v, name: name, body: string(body)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].version < files[j].version })

	current, err := s.SchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("store: read current schema version: %w", err)
	}

	for _, m := range files {
		if m.version <= current {
			continue
		}
		if err := s.applyOne(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyOne(ctx context.Context, m migrationFile) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin tx for migration %d: %w", m.version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.body); err != nil {
		return fmt.Errorf("store: apply migration %04d_%s: %w", m.version, m.name, err)
	}
	const ins = `INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`
	if _, err := tx.ExecContext(ctx, ins, m.version, m.name, NowMillis()); err != nil {
		return fmt.Errorf("store: record migration %d: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %d: %w", m.version, err)
	}
	return nil
}

func parseMigrationName(filename string) (int, string, error) {
	base := strings.TrimSuffix(filename, ".sql")
	idx := strings.IndexByte(base, '_')
	if idx <= 0 || idx == len(base)-1 {
		return 0, "", fmt.Errorf("invalid migration filename %q (want NNNN_name.sql)", filename)
	}
	v, err := strconv.Atoi(base[:idx])
	if err != nil {
		return 0, "", fmt.Errorf("invalid migration filename %q (version not int)", filename)
	}
	if v <= 0 {
		return 0, "", fmt.Errorf("invalid migration filename %q (version must be >0)", filename)
	}
	return v, base[idx+1:], nil
}

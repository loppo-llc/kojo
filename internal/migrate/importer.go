package migrate

import (
	"context"
	"database/sql"
	"errors"

	"github.com/loppo-llc/kojo/internal/store"
)

// Importer migrates a single v0 domain into the v1 store. Each Domain()
// must be unique and is used both as the migration_status key and in
// human-readable progress logs.
//
// Implementations MUST:
//   - Be idempotent: re-running an importer after partial success must
//     converge on the same final state. Use migration_status (`phase`
//     column) to skip work that has already been recorded as complete.
//   - Be read-only against opts.V0Dir. All file opens must go through
//     readAllRO / readOnlyOpen.
//   - Track work via store DB transactions; partial inserts must be
//     visible only after the importer's own commit.
//
// Phase 1 of the v1 implementation ships an empty importer list; later
// phases register concrete importers in init(). The orchestrator in
// migrate.go iterates this list in order.
type Importer interface {
	Domain() string
	Run(ctx context.Context, st *store.Store, opts Options) error
}

var registered []Importer

// Register adds an Importer to the run list. Importers are run in
// registration order, so packages with dependencies (e.g. agent_messages
// must run after agents) should register accordingly. Phase 1: the list
// is empty.
func Register(imp Importer) {
	registered = append(registered, imp)
}

func importers() []Importer {
	out := make([]Importer, len(registered))
	copy(out, registered)
	return out
}

// MarkPhase records `phase` for `domain` in migration_status. Helper for
// importers; safe to call multiple times.
//
// `checksum` is an optional content fingerprint for the domain's v0
// source files. Importers compute it via importers.domainChecksum
// before marking the domain imported (the value reflects the directory
// walk performed at the start of the run, not work done during the
// row-copy loop). Pass "" to leave the column as it was on the prior
// row — useful when an importer only updates phase mid-flight without
// re-walking source files. On insert with an empty checksum, the
// column is stored as NULL.
//
// finished_at is stamped for any terminal phase ('imported', 'cutover',
// 'complete') so operators can read elapsed durations from the table.
// 'pending' is treated as "in progress" and leaves finished_at NULL.
func MarkPhase(ctx context.Context, st *store.Store, domain, phase string, importedCount int, checksum string) error {
	now := store.NowMillis()
	// NULLIF on the EXCLUDED side lets the COALESCE on UPDATE keep the
	// prior checksum when the caller passes "". The same NULLIF runs on
	// INSERT so a fresh row gets NULL (not "") when no checksum yet.
	const upsert = `
INSERT INTO migration_status (domain, phase, source_checksum, imported_count, started_at, finished_at)
VALUES (?, ?, NULLIF(?, ''), ?, ?, CASE WHEN ?='pending' THEN NULL ELSE ? END)
ON CONFLICT(domain) DO UPDATE SET
  phase           = excluded.phase,
  source_checksum = COALESCE(NULLIF(excluded.source_checksum, ''), migration_status.source_checksum),
  imported_count  = excluded.imported_count,
  finished_at     = CASE
                      WHEN excluded.phase='pending' THEN migration_status.finished_at
                      ELSE ?
                    END
`
	_, err := st.DB().ExecContext(ctx, upsert, domain, phase, checksum, importedCount, now, phase, now, now)
	return err
}

// PhaseOf returns the recorded phase for `domain`, or "" if unrecorded.
// An sql.ErrNoRows is treated as "not started" so callers can branch on
// the empty string without importing database/sql.
func PhaseOf(ctx context.Context, st *store.Store, domain string) (string, error) {
	var phase string
	err := st.DB().QueryRowContext(ctx,
		`SELECT phase FROM migration_status WHERE domain = ?`, domain,
	).Scan(&phase)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return phase, nil
}

// ChecksumOf returns the stored source_checksum for `domain`, or "" if
// unrecorded / NULL. Used by tests and audit tooling to verify that a
// completed import wrote the fingerprint.
func ChecksumOf(ctx context.Context, st *store.Store, domain string) (string, error) {
	var checksum *string
	err := st.DB().QueryRowContext(ctx,
		`SELECT source_checksum FROM migration_status WHERE domain = ?`, domain,
	).Scan(&checksum)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if checksum == nil {
		return "", nil
	}
	return *checksum, nil
}

// CountOf returns the stored imported_count for `domain`, or 0 if the
// row is absent (sql.ErrNoRows) or the column is NULL (the schema
// declares imported_count as nullable INTEGER, see 0001_initial.sql).
// Used by tests to verify a marker importer ran with the expected row
// count — direct SQL access from tests would couple them to the schema
// layout, which this accessor avoids.
func CountOf(ctx context.Context, st *store.Store, domain string) (int, error) {
	var count sql.NullInt64
	err := st.DB().QueryRowContext(ctx,
		`SELECT imported_count FROM migration_status WHERE domain = ?`, domain,
	).Scan(&count)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	if !count.Valid {
		return 0, nil
	}
	return int(count.Int64), nil
}

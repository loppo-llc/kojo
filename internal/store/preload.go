package store

import (
	"context"
	"database/sql"
)

// preloadExistingKeysChunkSize is the maximum number of bind variables
// per round-trip when probing for existing rows. SQLite's default
// SQLITE_MAX_VARIABLE_NUMBER is 999; 500 keeps headroom for other
// parameters and matches the previous per-table chunk size.
const preloadExistingKeysChunkSize = 500

// preloadExistingKeys runs a chunked
// `SELECT keyColumn FROM table WHERE keyColumn IN (...)` and returns
// the set of keys that are already present in the table.
//
// `table` and `keyColumn` MUST be compile-time constants supplied by
// callers — both are interpolated into the SQL without quoting. The
// callers in this package only pass string literals.
//
// Used by bulk upsert paths to split a batch into insert-vs-update
// groups while staying well under SQLite's 999-variable cap. The
// per-table helpers in notify_cursors / sessions / push_subscriptions /
// external_chat_cursors are thin wrappers around this function.
//
// agent_tasks has its own variant (preloadExistingTaskIDs) because it
// scans a second column to detect cross-agent collisions.
func preloadExistingKeys[T any](
	ctx context.Context,
	tx *sql.Tx,
	table, keyColumn string,
	recs []T,
	extractKey func(T) string,
) (map[string]bool, error) {
	if len(recs) == 0 {
		return nil, nil
	}
	out := make(map[string]bool, len(recs))
	for start := 0; start < len(recs); start += preloadExistingKeysChunkSize {
		end := start + preloadExistingKeysChunkSize
		if end > len(recs) {
			end = len(recs)
		}
		keys := make([]any, 0, end-start)
		placeholders := make([]byte, 0, (end-start)*2)
		for i := start; i < end; i++ {
			if i > start {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
			keys = append(keys, extractKey(recs[i]))
		}
		q := `SELECT ` + keyColumn + ` FROM ` + table + ` WHERE ` + keyColumn + ` IN (` + string(placeholders) + `)`
		rows, err := tx.QueryContext(ctx, q, keys...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return nil, err
			}
			out[k] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

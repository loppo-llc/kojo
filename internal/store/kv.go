package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// KVScope mirrors the kv.scope CHECK constraint. Validated at Put/Get
// boundaries so a typo can't slip a misclassified row past the schema
// check on insert (the DB would reject it, but a clean Go-level error
// is friendlier).
type KVScope string

const (
	KVScopeGlobal  KVScope = "global"
	KVScopeLocal   KVScope = "local"
	KVScopeMachine KVScope = "machine"
)

// KVType mirrors the kv.type CHECK constraint.
type KVType string

const (
	KVTypeString KVType = "string"
	KVTypeJSON   KVType = "json"
	KVTypeBinary KVType = "binary"
)

// KVRecord is one kv row. Exactly one of Value (plaintext) and
// ValueEncrypted (envelope-protected) is set per row, gated by Secret.
type KVRecord struct {
	Namespace      string
	Key            string
	Value          string // plaintext; empty when Secret is true
	ValueEncrypted []byte // envelope ciphertext; nil when Secret is false
	Type           KVType
	Secret         bool
	Scope          KVScope
	Version        int64
	ETag           string
	CreatedAt      int64
	UpdatedAt      int64
}

// KVPutOptions narrows the Put path.
type KVPutOptions struct {
	// IfMatchETag, when non-empty, requires the existing row's etag to
	// match. Empty matches any (idempotent overwrite). The literal
	// string "*" is reserved for "row must not exist" checks.
	IfMatchETag string
	// Now overrides the wall clock for tests. 0 = NowMillis().
	Now int64
}

// IfMatchAny is the sentinel ifMatch value that asserts "row must not
// already exist". Mirrors the HTTP `If-None-Match: *` semantic used by
// the future PUT handler (§4.1).
const IfMatchAny = "*"

// PutKV inserts or updates a kv row. The (Value, ValueEncrypted) pair
// MUST agree with Secret: a secret row stores ValueEncrypted and an
// empty Value, a non-secret row stores Value and a nil ValueEncrypted.
// Mixing the two is a programming error and returned as a Go-level
// error rather than left to surface as a constraint violation later.
func (s *Store) PutKV(ctx context.Context, rec *KVRecord, opts KVPutOptions) (*KVRecord, error) {
	if rec == nil {
		return nil, errors.New("store.PutKV: nil record")
	}
	if rec.Namespace == "" || rec.Key == "" {
		return nil, errors.New("store.PutKV: namespace and key required")
	}
	if !validKVScope(rec.Scope) {
		return nil, fmt.Errorf("store.PutKV: invalid scope %q", rec.Scope)
	}
	if !validKVType(rec.Type) {
		return nil, fmt.Errorf("store.PutKV: invalid type %q", rec.Type)
	}
	if rec.Secret {
		if len(rec.ValueEncrypted) == 0 {
			return nil, errors.New("store.PutKV: secret row needs ValueEncrypted")
		}
		if rec.Value != "" {
			return nil, errors.New("store.PutKV: secret row must not set plaintext Value")
		}
	} else {
		if len(rec.ValueEncrypted) > 0 {
			return nil, errors.New("store.PutKV: non-secret row must not set ValueEncrypted")
		}
	}

	now := opts.Now
	if now == 0 {
		now = NowMillis()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Read current row (if any) to drive optimistic-lock semantics and
	// version increment.
	const sel = `
SELECT version, etag, created_at FROM kv WHERE namespace = ? AND key = ?`
	var (
		curVersion int64
		curETag    string
		createdAt  int64
	)
	hasCurrent := true
	if err := tx.QueryRowContext(ctx, sel, rec.Namespace, rec.Key).Scan(&curVersion, &curETag, &createdAt); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("store.PutKV: select: %w", err)
		}
		hasCurrent = false
	}

	// Pre-check the precondition against the SELECT result. Inside
	// SQLite's serializable TX this matches the ground truth, but
	// the eventual write needs the same check at SQL level (see
	// each branch below) to defeat the race where another writer's
	// commit lands between our SELECT and our INSERT/UPDATE.
	switch opts.IfMatchETag {
	case "":
		// Unconditional — fall through.
	case IfMatchAny:
		if hasCurrent {
			return nil, ErrETagMismatch
		}
	default:
		if !hasCurrent || curETag != opts.IfMatchETag {
			return nil, ErrETagMismatch
		}
	}

	out := *rec
	if hasCurrent {
		out.Version = curVersion + 1
		out.CreatedAt = createdAt
	} else {
		out.Version = 1
		out.CreatedAt = now
	}
	out.UpdatedAt = now
	out.ETag, err = computeKVETag(&out)
	if err != nil {
		return nil, fmt.Errorf("store.PutKV: etag: %w", err)
	}

	var valArg, encArg any
	if out.Secret {
		encArg = out.ValueEncrypted
		valArg = nil
	} else {
		valArg = out.Value
		encArg = nil
	}
	secretFlag := 0
	if out.Secret {
		secretFlag = 1
	}

	// Three SQL paths, one per precondition mode. Each path encodes
	// its precondition in the WHERE / ON CONFLICT clause so a
	// concurrent commit landing between our SELECT and write can't
	// produce a wrong outcome.
	switch opts.IfMatchETag {
	case "":
		// Unconditional: UPSERT — last-writer-wins. SQLite's
		// ON CONFLICT DO UPDATE handles the "row created by a
		// racing INSERT after our SELECT said !hasCurrent" case
		// without us blowing up on a UNIQUE violation.
		//
		// Note: when racing-insert lands first, our `hasCurrent`
		// stayed false, so out.Version = 1 and out.CreatedAt =
		// now. The DO UPDATE would clobber the racing row's
		// version+1 with our version=1. Rebuild the UPDATE to
		// take MAX-style version increment from the existing row
		// to keep the change feed monotonic.
		const upsert = `
INSERT INTO kv (
  namespace, key, value, value_encrypted, type, secret, scope,
  version, etag, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(namespace, key) DO UPDATE SET
  value = excluded.value,
  value_encrypted = excluded.value_encrypted,
  type = excluded.type,
  secret = excluded.secret,
  scope = excluded.scope,
  version = kv.version + 1,
  etag = excluded.etag,
  updated_at = excluded.updated_at`
		if _, err := tx.ExecContext(ctx, upsert,
			out.Namespace, out.Key, valArg, encArg, string(out.Type), secretFlag, string(out.Scope),
			out.Version, out.ETag, out.CreatedAt, out.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("store.PutKV: upsert: %w", err)
		}
		// Refresh out.Version + out.CreatedAt from the row we
		// just wrote so the returned record matches what's in
		// the DB (the ON CONFLICT path may have overridden them).
		const reread = `SELECT version, created_at FROM kv WHERE namespace = ? AND key = ?`
		if err := tx.QueryRowContext(ctx, reread, out.Namespace, out.Key).Scan(&out.Version, &out.CreatedAt); err != nil {
			return nil, fmt.Errorf("store.PutKV: reread: %w", err)
		}
		// Re-derive etag against the post-write version so callers
		// observe a consistent (version, etag) pair.
		out.ETag, err = computeKVETag(&out)
		if err != nil {
			return nil, fmt.Errorf("store.PutKV: etag: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE kv SET etag = ? WHERE namespace = ? AND key = ?`,
			out.ETag, out.Namespace, out.Key); err != nil {
			return nil, fmt.Errorf("store.PutKV: etag refresh: %w", err)
		}
	case IfMatchAny:
		// Create-only: INSERT, refuse on conflict. Atomic via the
		// UNIQUE constraint — no racing-insert window.
		const ins = `
INSERT INTO kv (
  namespace, key, value, value_encrypted, type, secret, scope,
  version, etag, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(namespace, key) DO NOTHING`
		res, err := tx.ExecContext(ctx, ins,
			out.Namespace, out.Key, valArg, encArg, string(out.Type), secretFlag, string(out.Scope),
			out.Version, out.ETag, out.CreatedAt, out.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("store.PutKV: insert (create-only): %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, ErrETagMismatch
		}
	default:
		// Conditional update: include the asserted etag in the
		// UPDATE predicate. A concurrent writer that bumped the
		// etag between our SELECT and this UPDATE matches 0 rows
		// and we surface as ErrETagMismatch.
		const upd = `
UPDATE kv SET
  value = ?, value_encrypted = ?, type = ?, secret = ?, scope = ?,
  version = ?, etag = ?, updated_at = ?
WHERE namespace = ? AND key = ? AND etag = ?`
		res, err := tx.ExecContext(ctx, upd,
			valArg, encArg, string(out.Type), secretFlag, string(out.Scope),
			out.Version, out.ETag, out.UpdatedAt,
			out.Namespace, out.Key, opts.IfMatchETag,
		)
		if err != nil {
			return nil, fmt.Errorf("store.PutKV: update: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, ErrETagMismatch
		}
	}

	// Decide insert vs update event from the post-write version (set
	// by the unconditional UPSERT branch above to kv.version+1 on
	// conflict). hasCurrent reflects the SELECT from before the
	// write, so a racing concurrent insert that landed between our
	// SELECT and the UPSERT would otherwise leave hasCurrent=false
	// and emit an EventOpInsert for what was actually an update.
	op := EventOpUpdate
	if out.Version <= 1 {
		op = EventOpInsert
	}
	// kv events use the composite key as the row id since kv has no
	// stand-alone `id`. Peers downstream key cache reads on
	// (namespace, key) so this round-trips cleanly.
	rowID := out.Namespace + "/" + out.Key
	evSeq, err := RecordEvent(ctx, tx, "kv", rowID, out.ETag, op, out.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("store.PutKV: record event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.fireEvent(EventRecord{Seq: evSeq, Table: "kv", ID: rowID, ETag: out.ETag, Op: op, TS: out.UpdatedAt})
	return &out, nil
}

// GetKV returns the row by composite key. Returns ErrNotFound when no
// row matches.
func (s *Store) GetKV(ctx context.Context, namespace, key string) (*KVRecord, error) {
	if namespace == "" || key == "" {
		return nil, errors.New("store.GetKV: namespace and key required")
	}
	const q = `
SELECT namespace, key, COALESCE(value, ''), value_encrypted, type, secret, scope,
       version, etag, created_at, updated_at
  FROM kv WHERE namespace = ? AND key = ?`
	row := s.db.QueryRowContext(ctx, q, namespace, key)
	var (
		out        KVRecord
		secretInt  int
		typ, scope string
	)
	if err := row.Scan(&out.Namespace, &out.Key, &out.Value, &out.ValueEncrypted,
		&typ, &secretInt, &scope, &out.Version, &out.ETag, &out.CreatedAt, &out.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	out.Type = KVType(typ)
	out.Scope = KVScope(scope)
	out.Secret = secretInt != 0
	if !out.Secret {
		// Defensive: a non-secret row should never carry encrypted bytes
		// (PutKV rejects that), but if a hand-edited DB has them, drop
		// them at the read boundary so callers don't accidentally treat
		// stale ciphertext as authoritative.
		out.ValueEncrypted = nil
	}
	return &out, nil
}

// DeleteKV removes the row. Empty ifMatchETag → idempotent (missing
// row is not an error). Non-empty ifMatchETag → conditional: missing
// row or etag mismatch surfaces as ErrETagMismatch so the caller
// can refetch + retry. Mirrors the SoftDeleteAgentMemory contract
// for tombstone-style endpoints.
func (s *Store) DeleteKV(ctx context.Context, namespace, key, ifMatchETag string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Atomic conditional delete: include the asserted etag in the
	// DELETE predicate so a concurrent PutKV that bumped the etag
	// between the caller's GET and our DELETE matches 0 rows and
	// surfaces as ErrETagMismatch. Without this, a separate SELECT
	// + DELETE pair leaves a race window where the row got
	// overwritten between our checks but we'd still delete it.
	var (
		res          interface{ RowsAffected() (int64, error) }
		execErr      error
		conditional  = ifMatchETag != ""
	)
	if conditional {
		res, execErr = tx.ExecContext(ctx,
			`DELETE FROM kv WHERE namespace = ? AND key = ? AND etag = ?`,
			namespace, key, ifMatchETag)
	} else {
		res, execErr = tx.ExecContext(ctx,
			`DELETE FROM kv WHERE namespace = ? AND key = ?`,
			namespace, key)
	}
	if execErr != nil {
		return execErr
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Conditional: the row either didn't exist OR its etag
		// drifted out from under us. Both surface as 412 to force
		// a refetch.
		// Unconditional: idempotent — missing row commits as no-op.
		if conditional {
			return ErrETagMismatch
		}
		return tx.Commit()
	}
	now := NowMillis()
	rowID := namespace + "/" + key
	evSeq, err := RecordEvent(ctx, tx, "kv", rowID, "", EventOpDelete, now)
	if err != nil {
		return fmt.Errorf("store.DeleteKV: record event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.fireEvent(EventRecord{Seq: evSeq, Table: "kv", ID: rowID, Op: EventOpDelete, TS: now})
	return nil
}

// ListKV returns all rows in the namespace ordered by key. For now no
// pagination — kv is intended for small config / secret blobs.
func (s *Store) ListKV(ctx context.Context, namespace string) ([]*KVRecord, error) {
	if namespace == "" {
		return nil, errors.New("store.ListKV: namespace required")
	}
	const q = `
SELECT namespace, key, COALESCE(value, ''), value_encrypted, type, secret, scope,
       version, etag, created_at, updated_at
  FROM kv WHERE namespace = ?
 ORDER BY key ASC`
	rows, err := s.db.QueryContext(ctx, q, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*KVRecord
	for rows.Next() {
		var (
			r         KVRecord
			secretInt int
			t, sc     string
		)
		if err := rows.Scan(&r.Namespace, &r.Key, &r.Value, &r.ValueEncrypted,
			&t, &secretInt, &sc, &r.Version, &r.ETag, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Type = KVType(t)
		r.Scope = KVScope(sc)
		r.Secret = secretInt != 0
		if !r.Secret {
			r.ValueEncrypted = nil
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

func validKVScope(s KVScope) bool {
	switch s {
	case KVScopeGlobal, KVScopeLocal, KVScopeMachine:
		return true
	}
	return false
}

func validKVType(t KVType) bool {
	switch t {
	case KVTypeString, KVTypeJSON, KVTypeBinary:
		return true
	}
	return false
}

// computeKVETag mirrors the canonical etag pattern but excludes
// version + timestamps so a no-op upsert (same payload) is detectable
// at the application layer if needed. The `secret`/`type`/`scope`
// metadata IS included — flipping a secret row to non-secret must
// produce a new etag so caches invalidate.
func computeKVETag(r *KVRecord) (string, error) {
	in := map[string]any{
		"namespace": r.Namespace,
		"key":       r.Key,
		"type":      string(r.Type),
		"scope":     string(r.Scope),
		"secret":    r.Secret,
	}
	if r.Secret {
		in["value_encrypted"] = r.ValueEncrypted
	} else {
		in["value"] = r.Value
	}
	return CanonicalETag(int(r.Version), in)
}

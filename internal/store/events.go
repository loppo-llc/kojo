package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// EventOp is the verb persisted in events.op. Restricted to the three
// values the schema CHECK constraint accepts.
type EventOp string

const (
	EventOpInsert EventOp = "insert"
	EventOpUpdate EventOp = "update"
	EventOpDelete EventOp = "delete"
)

// EventRecord is one durable row from the events table. It mirrors the
// WebSocket broadcast payload shape (eventbus.Event) so a peer that
// caught up via /changes?since=<seq> can replay the same logic it uses
// for live frames.
type EventRecord struct {
	Seq   int64
	Table string
	ID    string
	ETag  string // empty for delete
	Op    EventOp
	TS    int64 // unix millis
}

// RecordEvent appends one row to the events table inside the caller's
// tx. Seq is allocated from NextGlobalSeq() — the SAME source domain
// rows draw from, so a peer can use any seq it has seen as a cursor
// here without per-table bookkeeping. ts == 0 picks NowMillis().
//
// Callers should invoke this AFTER the domain mutation succeeds in the
// tx but BEFORE commit, so a transient DB error rolls both back. For
// inserts/updates pass the post-write etag; for deletes pass "".
//
// Returns the allocated seq so the caller can echo it back into a
// process-local broadcast (eventbus.Event.Seq) — the broadcast then
// matches exactly what /changes?since=<seq> will return for the same
// row, eliminating any "did the peer already see this?" ambiguity.
func RecordEvent(ctx context.Context, tx *sql.Tx, table, id, etag string, op EventOp, ts int64) (int64, error) {
	if tx == nil {
		return 0, errors.New("RecordEvent: nil tx")
	}
	if table == "" || id == "" {
		return 0, fmt.Errorf("RecordEvent: empty table/id (table=%q id=%q)", table, id)
	}
	switch op {
	case EventOpInsert, EventOpUpdate, EventOpDelete:
	default:
		return 0, fmt.Errorf("RecordEvent: invalid op %q", op)
	}
	if ts == 0 {
		ts = NowMillis()
	}
	seq := NextGlobalSeq()
	var etagArg any
	if etag == "" {
		etagArg = nil
	} else {
		etagArg = etag
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (seq, table_name, row_id, etag, op, ts) VALUES (?, ?, ?, ?, ?, ?)`,
		seq, table, id, etagArg, string(op), ts,
	); err != nil {
		return 0, fmt.Errorf("RecordEvent: insert: %w", err)
	}
	return seq, nil
}

// ListEventsSinceOptions narrows the cursor read.
type ListEventsSinceOptions struct {
	// Table, when non-empty, filters to a single domain. Defaults to
	// all tables.
	Table string
	// Limit caps the number of rows returned. <= 0 picks 500. The
	// caller is expected to page by passing the largest seq from the
	// previous batch as the next ?since.
	Limit int
}

// ListEventsResult is the body of a /changes?since=<seq> response.
type ListEventsResult struct {
	Events []EventRecord
	// NextSince is the seq value the caller should pass as ?since on
	// the next poll. Equal to the largest Seq in Events, or the
	// caller-supplied since when Events is empty (so a poll loop never
	// rewinds).
	NextSince int64
	// Watermark is the smallest seq currently present in the events
	// table — informational only in v1, since no retention worker
	// runs yet and the events table grows without bound. Reserved for
	// a future retention slice that will persist a pruned-through
	// floor in kv; the handler will then promote a sub-watermark
	// `since` to a `truncated` response.
	Watermark int64
}

// ListEventsSince returns rows with seq > since, ordered by seq. The
// store retention may have trimmed earlier rows; the result includes a
// Watermark so the caller can detect that case.
func (s *Store) ListEventsSince(ctx context.Context, since int64, opts ListEventsSinceOptions) (*ListEventsResult, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 500
	}
	out := &ListEventsResult{NextSince: since}

	// Watermark: smallest seq in events. We compute it once per call
	// rather than tagging each row so the result stays compact.
	var watermark sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MIN(seq) FROM events`).Scan(&watermark); err != nil {
		return nil, fmt.Errorf("ListEventsSince: watermark: %w", err)
	}
	if watermark.Valid {
		out.Watermark = watermark.Int64
	}

	var rows *sql.Rows
	var err error
	if opts.Table != "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT seq, table_name, row_id, COALESCE(etag, ''), op, ts
			   FROM events
			  WHERE table_name = ? AND seq > ?
			  ORDER BY seq ASC
			  LIMIT ?`,
			opts.Table, since, limit)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT seq, table_name, row_id, COALESCE(etag, ''), op, ts
			   FROM events
			  WHERE seq > ?
			  ORDER BY seq ASC
			  LIMIT ?`,
			since, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("ListEventsSince: query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var r EventRecord
		var op string
		if err := rows.Scan(&r.Seq, &r.Table, &r.ID, &r.ETag, &op, &r.TS); err != nil {
			return nil, fmt.Errorf("ListEventsSince: scan: %w", err)
		}
		r.Op = EventOp(op)
		out.Events = append(out.Events, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListEventsSince: rows: %w", err)
	}

	if n := len(out.Events); n > 0 {
		out.NextSince = out.Events[n-1].Seq
	}
	return out, nil
}

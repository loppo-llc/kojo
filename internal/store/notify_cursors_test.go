package store

import (
	"context"
	"errors"
	"testing"
)

func TestBulkInsertNotifyCursors(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	agentID := "ag"
	recs := []*NotifyCursorRecord{
		{ID: "ag:slack:Cxxx", Source: "slack", AgentID: &agentID, Cursor: "cur-1"},
		{ID: "ag:gmail:label-inbox", Source: "gmail", AgentID: &agentID, Cursor: "cur-2"},
		// Deployment-scoped row: agent_id NULL.
		{ID: "system:slack:announcement", Source: "slack", AgentID: nil, Cursor: "cur-3"},
	}
	n, err := s.BulkInsertNotifyCursors(ctx, recs, NotifyCursorInsertOptions{PeerID: "peer-1"})
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if n != 3 {
		t.Errorf("inserted = %d, want 3", n)
	}
	for _, r := range recs {
		if r.ETag == "" {
			t.Errorf("etag not stamped: %+v", r)
		}
		if r.PeerID != "peer-1" {
			t.Errorf("peer_id = %q, want peer-1", r.PeerID)
		}
		if r.Version != 1 {
			t.Errorf("version = %d, want 1", r.Version)
		}
	}

	// GetNotifyCursor round-trip.
	got, err := s.GetNotifyCursor(ctx, "ag:slack:Cxxx")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Source != "slack" {
		t.Errorf("source = %q, want slack", got.Source)
	}
	if got.AgentID == nil || *got.AgentID != "ag" {
		t.Errorf("agent_id = %v, want &ag", got.AgentID)
	}
	if got.Cursor != "cur-1" {
		t.Errorf("cursor = %q, want cur-1", got.Cursor)
	}
	if got.ETag != recs[0].ETag {
		t.Errorf("etag mismatch: read=%q insert=%q", got.ETag, recs[0].ETag)
	}

	// Detached row scans back with AgentID nil.
	got2, err := s.GetNotifyCursor(ctx, "system:slack:announcement")
	if err != nil {
		t.Fatalf("get system: %v", err)
	}
	if got2.AgentID != nil {
		t.Errorf("agent_id should be nil for detached, got %v", got2.AgentID)
	}

	// ListNotifyCursorsByAgent("ag")
	list, err := s.ListNotifyCursorsByAgent(ctx, "ag")
	if err != nil {
		t.Fatalf("list ag: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list ag = %d, want 2", len(list))
	}

	// Detached listing.
	detached, err := s.ListNotifyCursorsByAgent(ctx, "")
	if err != nil {
		t.Fatalf("list detached: %v", err)
	}
	if len(detached) != 1 || detached[0].ID != "system:slack:announcement" {
		t.Errorf("detached list: %+v", detached)
	}

	// Re-running silently skips (preload-hit).
	n2, err := s.BulkInsertNotifyCursors(ctx, recs, NotifyCursorInsertOptions{PeerID: "peer-1"})
	if err != nil {
		t.Fatalf("bulk re-run: %v", err)
	}
	if n2 != 0 {
		t.Errorf("idempotent re-run inserted = %d, want 0", n2)
	}

	// Mixed: some new, some duplicate.
	mix := []*NotifyCursorRecord{
		{ID: "ag:slack:Cxxx", Source: "slack", AgentID: &agentID, Cursor: "should-be-skipped"},
		{ID: "ag:slack:Cyyy", Source: "slack", AgentID: &agentID, Cursor: "fresh"},
	}
	n3, err := s.BulkInsertNotifyCursors(ctx, mix, NotifyCursorInsertOptions{PeerID: "peer-1"})
	if err != nil {
		t.Fatalf("bulk mixed: %v", err)
	}
	if n3 != 1 {
		t.Errorf("mixed inserted = %d, want 1", n3)
	}
	if mix[0].ETag != "" {
		t.Errorf("skipped record was mutated: %+v", mix[0])
	}
	if mix[1].ETag == "" {
		t.Errorf("inserted record not stamped: %+v", mix[1])
	}
	got3, err := s.GetNotifyCursor(ctx, "ag:slack:Cxxx")
	if err != nil {
		t.Fatalf("get after skip: %v", err)
	}
	if got3.Cursor != "cur-1" {
		t.Errorf("first-write-wins violated: cursor = %q, want cur-1", got3.Cursor)
	}
}

func TestBulkInsertNotifyCursorsRejectsEmptyFields(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	cases := []struct {
		name string
		rec  *NotifyCursorRecord
	}{
		{"empty id", &NotifyCursorRecord{ID: "", Source: "slack", Cursor: "c"}},
		{"empty source", &NotifyCursorRecord{ID: "x", Source: "", Cursor: "c"}},
		{"empty cursor", &NotifyCursorRecord{ID: "x", Source: "slack", Cursor: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.BulkInsertNotifyCursors(ctx, []*NotifyCursorRecord{tc.rec}, NotifyCursorInsertOptions{}); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestGetNotifyCursorNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetNotifyCursor(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestBulkInsertNotifyCursorsInBatchDuplicate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	recs := []*NotifyCursorRecord{
		{ID: "dup", Source: "slack", Cursor: "first"},
		{ID: "dup", Source: "gmail", Cursor: "second"},
	}
	n, err := s.BulkInsertNotifyCursors(ctx, recs, NotifyCursorInsertOptions{})
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if n != 1 {
		t.Errorf("inserted = %d, want 1 (first-write-wins on dup)", n)
	}
	got, err := s.GetNotifyCursor(ctx, "dup")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Cursor != "first" || got.Source != "slack" {
		t.Errorf("first-write-wins violated: %+v", got)
	}
}

func TestBulkInsertNotifyCursorsAgentIDEmptyStringNormalized(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	empty := ""
	recs := []*NotifyCursorRecord{
		{ID: "x", Source: "slack", AgentID: &empty, Cursor: "c"},
	}
	n, err := s.BulkInsertNotifyCursors(ctx, recs, NotifyCursorInsertOptions{})
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if n != 1 {
		t.Errorf("inserted = %d, want 1", n)
	}
	// The staged copy should have its AgentID set to nil before stamping
	// the etag, even though the caller passed &"" — that's how the
	// canonical record stays in agreement with the round-tripped row.
	if recs[0].AgentID != nil {
		t.Errorf("staged AgentID should be nil after normalization, got %v", recs[0].AgentID)
	}
	got, err := s.GetNotifyCursor(ctx, "x")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AgentID != nil {
		t.Errorf("agent_id should be nil after &\"\" normalization, got %v", got.AgentID)
	}
	if got.ETag != recs[0].ETag {
		t.Errorf("etag mismatch (insert vs read-back) — &\"\" not normalized: %q vs %q", recs[0].ETag, got.ETag)
	}
	// Belt-and-braces: recompute the canonical ETag from the read-back
	// record. If the staged copy and the persisted bytes ever diverge —
	// e.g. a future column added to the canonical record but not to the
	// scan path — this catches it independently of the caller's recs[0].
	want, err := computeNotifyCursorETag(got)
	if err != nil {
		t.Fatalf("recompute etag: %v", err)
	}
	if got.ETag != want {
		t.Errorf("read-back etag != recomputed etag: %q vs %q", got.ETag, want)
	}
}

func TestUpsertNotifyCursorInsertThenUpdate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	agentID := "ag"
	rec := &NotifyCursorRecord{
		ID: "ag:slack:Cxxx", Source: "slack", AgentID: &agentID, Cursor: "cur-1",
	}
	if err := s.UpsertNotifyCursor(ctx, rec, NotifyCursorInsertOptions{Now: 1000, PeerID: "peer-1"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if rec.Version != 1 {
		t.Errorf("insert version = %d, want 1", rec.Version)
	}
	if rec.ETag == "" {
		t.Errorf("insert etag empty")
	}
	if rec.CreatedAt != 1000 || rec.UpdatedAt != 1000 {
		t.Errorf("insert timestamps: created=%d updated=%d, want 1000/1000", rec.CreatedAt, rec.UpdatedAt)
	}
	if rec.PeerID != "peer-1" {
		t.Errorf("insert peer_id = %q, want peer-1", rec.PeerID)
	}

	// Update: cursor advance, peer change.
	rec.Cursor = "cur-2"
	if err := s.UpsertNotifyCursor(ctx, rec, NotifyCursorInsertOptions{Now: 2000, PeerID: "peer-2"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if rec.Version != 2 {
		t.Errorf("update version = %d, want 2", rec.Version)
	}
	if rec.CreatedAt != 1000 {
		t.Errorf("update created_at = %d, want 1000 (preserved)", rec.CreatedAt)
	}
	if rec.UpdatedAt != 2000 {
		t.Errorf("update updated_at = %d, want 2000", rec.UpdatedAt)
	}
	if rec.PeerID != "peer-2" {
		t.Errorf("update peer_id = %q, want peer-2 (overwritten)", rec.PeerID)
	}

	// Read-back agrees with the staged record.
	got, err := s.GetNotifyCursor(ctx, "ag:slack:Cxxx")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Cursor != "cur-2" || got.Version != 2 || got.ETag != rec.ETag {
		t.Errorf("read-back mismatch: %+v vs %+v", got, rec)
	}
	// Recompute etag from the read-back record so a future column drift
	// between scan and canonical-input is caught here.
	want, err := computeNotifyCursorETag(got)
	if err != nil {
		t.Fatalf("recompute etag: %v", err)
	}
	if got.ETag != want {
		t.Errorf("read-back etag != recomputed: %q vs %q", got.ETag, want)
	}
}

func TestUpsertNotifyCursorRejectsEmptyFields(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	cases := []struct {
		name string
		rec  *NotifyCursorRecord
	}{
		{"nil", nil},
		{"empty id", &NotifyCursorRecord{ID: "", Source: "slack", Cursor: "c"}},
		{"empty source", &NotifyCursorRecord{ID: "x", Source: "", Cursor: "c"}},
		{"empty cursor", &NotifyCursorRecord{ID: "x", Source: "slack", Cursor: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.UpsertNotifyCursor(ctx, tc.rec, NotifyCursorInsertOptions{}); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestUpsertNotifyCursorAgentIDEmptyStringNormalized(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	empty := ""
	rec := &NotifyCursorRecord{ID: "x", Source: "slack", AgentID: &empty, Cursor: "c"}
	if err := s.UpsertNotifyCursor(ctx, rec, NotifyCursorInsertOptions{Now: 1000}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if rec.AgentID != nil {
		t.Errorf("staged AgentID should be nil after &\"\" normalization, got %v", rec.AgentID)
	}
	got, err := s.GetNotifyCursor(ctx, "x")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AgentID != nil {
		t.Errorf("read-back agent_id should be nil, got %v", got.AgentID)
	}
	if got.ETag != rec.ETag {
		t.Errorf("etag mismatch: insert=%q read=%q", rec.ETag, got.ETag)
	}
}

// TestUpsertNotifyCursorOverTombstone exercises the rare path where a
// cursor row was previously soft-tombstoned (deleted_at NOT NULL) and
// then a fresh upsert reincarnates it. Today no v1 code path writes a
// tombstone — DeleteNotifyCursor is a hard delete and BulkInsert never
// sets deleted_at — but a future peer-replication slice may replay
// tombstones from the op_log. The contract here ensures that even if
// such a row exists, the upsert lands as version=1 (clean lineage)
// rather than version=prev+1 over a dead row.
func TestUpsertNotifyCursorOverTombstone(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	// Plant a tombstoned row directly via SQL.
	const ins = `
INSERT INTO notify_cursors (id, source, agent_id, cursor, version, etag, created_at, updated_at, deleted_at, peer_id)
VALUES ('ag:slack:Czzz', 'slack', 'ag', 'old', 7, 'stale-etag', 100, 200, 200, NULL)`
	if _, err := s.db.ExecContext(ctx, ins); err != nil {
		t.Fatalf("plant tombstone: %v", err)
	}

	agentID := "ag"
	rec := &NotifyCursorRecord{
		ID: "ag:slack:Czzz", Source: "slack", AgentID: &agentID, Cursor: "fresh",
	}
	if err := s.UpsertNotifyCursor(ctx, rec, NotifyCursorInsertOptions{Now: 1000}); err != nil {
		t.Fatalf("upsert over tombstone: %v", err)
	}
	if rec.Version != 1 {
		t.Errorf("version = %d, want 1 (fresh lineage over tombstone)", rec.Version)
	}
	if rec.CreatedAt != 1000 {
		t.Errorf("created_at = %d, want 1000 (not preserved from tombstone)", rec.CreatedAt)
	}
	got, err := s.GetNotifyCursor(ctx, "ag:slack:Czzz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Cursor != "fresh" || got.Version != 1 || got.DeletedAt != nil {
		t.Errorf("post-upsert state wrong: %+v", got)
	}
}

func TestDeleteNotifyCursor(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	agentID := "ag"
	rec := &NotifyCursorRecord{
		ID: "ag:slack:Cdel", Source: "slack", AgentID: &agentID, Cursor: "c",
	}
	if err := s.UpsertNotifyCursor(ctx, rec, NotifyCursorInsertOptions{Now: 1000}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := s.DeleteNotifyCursor(ctx, "ag:slack:Cdel"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetNotifyCursor(ctx, "ag:slack:Cdel"); !errors.Is(err, ErrNotFound) {
		t.Errorf("post-delete get: want ErrNotFound, got %v", err)
	}

	// Idempotent: deleting an absent row is a no-op.
	if err := s.DeleteNotifyCursor(ctx, "ag:slack:Cdel"); err != nil {
		t.Errorf("idempotent re-delete: %v", err)
	}
	if err := s.DeleteNotifyCursor(ctx, "never-existed"); err != nil {
		t.Errorf("delete never-existed: %v", err)
	}
}

// TestDeleteNotifyCursorSkipsTombstoneEvent mirrors the bulk-delete
// guarantee: a tombstoned row must be physically wiped without
// re-firing a delete event (the original tombstone-write event already
// announced removal). Symmetric to TestDeleteNotifyCursorsByAgentSkipsTombstoneEvents.
func TestDeleteNotifyCursorSkipsTombstoneEvent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	// Plant a tombstoned row directly via SQL.
	const ins = `
INSERT INTO notify_cursors (id, source, agent_id, cursor, version, etag, created_at, updated_at, deleted_at, peer_id)
VALUES ('ag:slack:Csolo', 'slack', 'ag', 'old', 5, 'stale-etag', 100, 200, 200, NULL)`
	if _, err := s.db.ExecContext(ctx, ins); err != nil {
		t.Fatalf("plant tombstone: %v", err)
	}

	var fired int
	s.SetEventListener(func(e EventRecord) {
		if e.ID == "ag:slack:Csolo" {
			fired++
		}
	})

	if err := s.DeleteNotifyCursor(ctx, "ag:slack:Csolo"); err != nil {
		t.Fatalf("delete tombstone: %v", err)
	}
	// Row physically gone.
	var cnt int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notify_cursors WHERE id = ?`, "ag:slack:Csolo").Scan(&cnt); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cnt != 0 {
		t.Errorf("row count = %d, want 0 (tombstone wiped)", cnt)
	}
	// No event fired (the tombstone's original delete event already covered it).
	if fired != 0 {
		t.Errorf("fired = %d, want 0 (tombstone delete must not re-fire event)", fired)
	}
}

func TestDeleteNotifyCursorEmptyID(t *testing.T) {
	s := openTestStore(t)
	if err := s.DeleteNotifyCursor(context.Background(), ""); err == nil {
		t.Fatal("expected validation error on empty id")
	}
}

func TestDeleteNotifyCursorsByAgent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag1")
	seedAgent(t, s, "ag2")

	ag1, ag2 := "ag1", "ag2"
	recs := []*NotifyCursorRecord{
		{ID: "ag1:slack:Cone", Source: "slack", AgentID: &ag1, Cursor: "c1"},
		{ID: "ag1:gmail:Ginbox", Source: "gmail", AgentID: &ag1, Cursor: "c2"},
		{ID: "ag2:slack:Ctwo", Source: "slack", AgentID: &ag2, Cursor: "c3"},
		// Detached row (agent_id IS NULL) — must NOT be touched by the
		// per-agent wipe even if someone passes an empty agentID.
		{ID: "system:slack:announce", Source: "slack", AgentID: nil, Cursor: "c4"},
	}
	if _, err := s.BulkInsertNotifyCursors(ctx, recs, NotifyCursorInsertOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	n, err := s.DeleteNotifyCursorsByAgent(ctx, "ag1")
	if err != nil {
		t.Fatalf("delete by agent: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted = %d, want 2", n)
	}

	// ag1 cursors gone.
	if _, err := s.GetNotifyCursor(ctx, "ag1:slack:Cone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ag1:slack:Cone: want ErrNotFound, got %v", err)
	}
	if _, err := s.GetNotifyCursor(ctx, "ag1:gmail:Ginbox"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ag1:gmail:Ginbox: want ErrNotFound, got %v", err)
	}
	// ag2 cursor untouched.
	if _, err := s.GetNotifyCursor(ctx, "ag2:slack:Ctwo"); err != nil {
		t.Errorf("ag2:slack:Ctwo: want survival, got %v", err)
	}
	// Detached row untouched.
	if _, err := s.GetNotifyCursor(ctx, "system:slack:announce"); err != nil {
		t.Errorf("detached row: want survival, got %v", err)
	}

	// Idempotent: re-running on an already-empty agent returns 0.
	n2, err := s.DeleteNotifyCursorsByAgent(ctx, "ag1")
	if err != nil {
		t.Fatalf("idempotent re-run: %v", err)
	}
	if n2 != 0 {
		t.Errorf("idempotent deleted = %d, want 0", n2)
	}
}

func TestDeleteNotifyCursorsByAgentRejectsEmptyAgent(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.DeleteNotifyCursorsByAgent(context.Background(), ""); err == nil {
		t.Fatal("expected validation error on empty agent id (would otherwise wipe NULL agent_id rows + the whole table)")
	}
}

// TestNotifyCursorEventsEmitted verifies that Upsert / DeleteNotifyCursor /
// DeleteNotifyCursorsByAgent emit one event per affected row through the
// store's event listener — the same hook /api/v1/changes and the WS
// invalidator subscribe to. Without this, peer replication and the
// cursor-changed WS frame would silently miss notify_cursors edits.
func TestNotifyCursorEventsEmitted(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	type fired struct {
		Table, ID string
		ETag      string
		Op        EventOp
	}
	var (
		mu     = make(chan struct{}, 1)
		events []fired
	)
	mu <- struct{}{}
	s.SetEventListener(func(e EventRecord) {
		<-mu
		events = append(events, fired{Table: e.Table, ID: e.ID, ETag: e.ETag, Op: e.Op})
		mu <- struct{}{}
	})

	agentID := "ag"
	rec := &NotifyCursorRecord{ID: "ag:slack:Cevt", Source: "slack", AgentID: &agentID, Cursor: "c1"}
	if err := s.UpsertNotifyCursor(ctx, rec, NotifyCursorInsertOptions{}); err != nil {
		t.Fatalf("upsert insert: %v", err)
	}
	rec.Cursor = "c2"
	if err := s.UpsertNotifyCursor(ctx, rec, NotifyCursorInsertOptions{}); err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	if err := s.DeleteNotifyCursor(ctx, "ag:slack:Cevt"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Idempotent miss must NOT fire an event.
	if err := s.DeleteNotifyCursor(ctx, "ag:slack:Cnope"); err != nil {
		t.Fatalf("delete miss: %v", err)
	}
	// Bulk delete: seed two then sweep. Inserted out of id order so the
	// id-ASC sort assertion below has something to verify.
	rec3 := &NotifyCursorRecord{ID: "ag:slack:Cbulk2", Source: "slack", AgentID: &agentID, Cursor: "y"}
	rec2 := &NotifyCursorRecord{ID: "ag:slack:Cbulk1", Source: "slack", AgentID: &agentID, Cursor: "x"}
	if err := s.UpsertNotifyCursor(ctx, rec3, NotifyCursorInsertOptions{}); err != nil {
		t.Fatalf("upsert bulk2: %v", err)
	}
	if err := s.UpsertNotifyCursor(ctx, rec2, NotifyCursorInsertOptions{}); err != nil {
		t.Fatalf("upsert bulk1: %v", err)
	}
	if _, err := s.DeleteNotifyCursorsByAgent(ctx, "ag"); err != nil {
		t.Fatalf("delete by agent: %v", err)
	}

	<-mu
	defer func() { mu <- struct{}{} }()

	// Expected: insert + update + delete + (skip miss) + insert(bulk2) + insert(bulk1) + 2*delete-in-id-order.
	wantOps := []EventOp{
		EventOpInsert, // first upsert
		EventOpUpdate, // second upsert
		EventOpDelete, // explicit delete
		EventOpInsert, // bulk2 (Cbulk2 inserted first)
		EventOpInsert, // bulk1 (Cbulk1 inserted second)
		EventOpDelete, // bulk delete: id ASC → Cbulk1 first
		EventOpDelete, // bulk delete: id ASC → Cbulk2 second
	}
	if len(events) != len(wantOps) {
		t.Fatalf("events = %d (%v), want %d (%v)", len(events), events, len(wantOps), wantOps)
	}
	for i, want := range wantOps {
		if events[i].Op != want {
			t.Errorf("event[%d].Op = %s, want %s (full: %+v)", i, events[i].Op, want, events[i])
		}
		if events[i].Table != "notify_cursors" {
			t.Errorf("event[%d].Table = %q, want notify_cursors", i, events[i].Table)
		}
	}
	// Bulk-delete event id order: must be id-ASC so peers see a
	// deterministic stream. events[5] should be Cbulk1, events[6] Cbulk2.
	if events[5].ID != "ag:slack:Cbulk1" {
		t.Errorf("bulk delete[0] id = %q, want ag:slack:Cbulk1 (id ASC)", events[5].ID)
	}
	if events[6].ID != "ag:slack:Cbulk2" {
		t.Errorf("bulk delete[1] id = %q, want ag:slack:Cbulk2 (id ASC)", events[6].ID)
	}
}

// TestDeleteNotifyCursorsByAgentSkipsTombstoneEvents verifies that when
// a peer has previously tombstoned a cursor (the v1 runtime doesn't
// today, but a future replication slice may), DeleteNotifyCursorsByAgent
// physically removes the row WITHOUT re-firing the delete event — the
// original tombstone-write event already covered that. Without this
// guard, peers would double-apply the same delete via /api/v1/changes.
func TestDeleteNotifyCursorsByAgentSkipsTombstoneEvents(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	// Plant one live + one tombstoned row.
	const insLive = `
INSERT INTO notify_cursors (id, source, agent_id, cursor, version, etag, created_at, updated_at, deleted_at, peer_id)
VALUES ('ag:slack:Clive', 'slack', 'ag', 'live', 1, 'live-etag', 100, 100, NULL, NULL)`
	const insTomb = `
INSERT INTO notify_cursors (id, source, agent_id, cursor, version, etag, created_at, updated_at, deleted_at, peer_id)
VALUES ('ag:slack:Ctomb', 'slack', 'ag', 'tomb', 2, 'tomb-etag', 100, 200, 200, NULL)`
	if _, err := s.db.ExecContext(ctx, insLive); err != nil {
		t.Fatalf("plant live: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, insTomb); err != nil {
		t.Fatalf("plant tomb: %v", err)
	}

	var fired []string
	s.SetEventListener(func(e EventRecord) {
		if e.Op == EventOpDelete {
			fired = append(fired, e.ID)
		}
	})

	n, err := s.DeleteNotifyCursorsByAgent(ctx, "ag")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	// n counts only the LIVE rows (event-emission count) — the
	// tombstoned row is physically removed but doesn't count.
	if n != 1 {
		t.Errorf("count = %d, want 1 (live only; tombstone wiped silently)", n)
	}
	// Both rows are gone from the table.
	for _, id := range []string{"ag:slack:Clive", "ag:slack:Ctomb"} {
		var cnt int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM notify_cursors WHERE id = ?`, id).Scan(&cnt); err != nil {
			t.Fatalf("count %s: %v", id, err)
		}
		if cnt != 0 {
			t.Errorf("%s: row count = %d, want 0 (physically removed)", id, cnt)
		}
	}
	// Only the live row's id appears in the delete event stream.
	if len(fired) != 1 || fired[0] != "ag:slack:Clive" {
		t.Errorf("fired delete events = %v, want [ag:slack:Clive] only", fired)
	}
}

// TestNotifyCursorTombstoneFiltered verifies that GetNotifyCursor and
// ListNotifyCursorsByAgent ignore tombstoned rows. No live v1 path
// writes a tombstone today, so we plant one via SQL — the row is then
// invisible to the live read APIs even though the underlying SELECT
// would otherwise pick it up.
func TestNotifyCursorTombstoneFiltered(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	const ins = `
INSERT INTO notify_cursors (id, source, agent_id, cursor, version, etag, created_at, updated_at, deleted_at, peer_id)
VALUES ('ag:slack:Ctomb', 'slack', 'ag', 'old', 7, 'stale-etag', 100, 200, 200, NULL)`
	if _, err := s.db.ExecContext(ctx, ins); err != nil {
		t.Fatalf("plant tombstone: %v", err)
	}

	if _, err := s.GetNotifyCursor(ctx, "ag:slack:Ctomb"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get on tombstone: want ErrNotFound, got %v", err)
	}
	list, err := s.ListNotifyCursorsByAgent(ctx, "ag")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, rec := range list {
		if rec.ID == "ag:slack:Ctomb" {
			t.Errorf("list returned tombstoned row: %+v", rec)
		}
	}
}

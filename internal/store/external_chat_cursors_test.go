package store

import (
	"context"
	"errors"
	"testing"
)

func TestBulkInsertExternalChatCursors(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")

	agentID := "ag"
	channelID := "C123"
	recs := []*ExternalChatCursorRecord{
		{ID: "ag:slack:C123", Source: "slack", AgentID: &agentID, ChannelID: &channelID, Cursor: "1712345678.100000"},
		{ID: "ag:slack:C123:1712345678.000000", Source: "slack", AgentID: &agentID, ChannelID: &channelID, Cursor: "1712345678.500000"},
		// Deployment-scoped row: agent_id NULL.
		{ID: "system:slack:Cdeploy", Source: "slack", AgentID: nil, ChannelID: &channelID, Cursor: "1712345700.000000"},
	}
	n, err := s.BulkInsertExternalChatCursors(ctx, recs, ExternalChatCursorInsertOptions{PeerID: "peer-1"})
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

	// GetExternalChatCursor round-trip.
	got, err := s.GetExternalChatCursor(ctx, "ag:slack:C123")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Source != "slack" {
		t.Errorf("source = %q, want slack", got.Source)
	}
	if got.AgentID == nil || *got.AgentID != "ag" {
		t.Errorf("agent_id = %v, want &ag", got.AgentID)
	}
	if got.ChannelID == nil || *got.ChannelID != "C123" {
		t.Errorf("channel_id = %v, want &C123", got.ChannelID)
	}
	if got.Cursor != "1712345678.100000" {
		t.Errorf("cursor = %q, want 1712345678.100000", got.Cursor)
	}
	if got.ETag != recs[0].ETag {
		t.Errorf("etag mismatch: read=%q insert=%q", got.ETag, recs[0].ETag)
	}

	// Detached row scans back with AgentID nil, ChannelID populated.
	got2, err := s.GetExternalChatCursor(ctx, "system:slack:Cdeploy")
	if err != nil {
		t.Fatalf("get system: %v", err)
	}
	if got2.AgentID != nil {
		t.Errorf("agent_id should be nil for detached, got %v", got2.AgentID)
	}

	// ListExternalChatCursorsByAgent("ag")
	list, err := s.ListExternalChatCursorsByAgent(ctx, "ag")
	if err != nil {
		t.Fatalf("list ag: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list ag = %d, want 2", len(list))
	}

	// Detached listing.
	detached, err := s.ListExternalChatCursorsByAgent(ctx, "")
	if err != nil {
		t.Fatalf("list detached: %v", err)
	}
	if len(detached) != 1 || detached[0].ID != "system:slack:Cdeploy" {
		t.Errorf("detached list: %+v", detached)
	}

	// Re-running silently skips (preload-hit).
	n2, err := s.BulkInsertExternalChatCursors(ctx, recs, ExternalChatCursorInsertOptions{PeerID: "peer-1"})
	if err != nil {
		t.Fatalf("bulk re-run: %v", err)
	}
	if n2 != 0 {
		t.Errorf("idempotent re-run inserted = %d, want 0", n2)
	}

	// Mixed: some new, some duplicate.
	mix := []*ExternalChatCursorRecord{
		{ID: "ag:slack:C123", Source: "slack", AgentID: &agentID, ChannelID: &channelID, Cursor: "should-be-skipped"},
		{ID: "ag:slack:C999", Source: "slack", AgentID: &agentID, ChannelID: stringPtr("C999"), Cursor: "fresh"},
	}
	n3, err := s.BulkInsertExternalChatCursors(ctx, mix, ExternalChatCursorInsertOptions{PeerID: "peer-1"})
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
	got3, err := s.GetExternalChatCursor(ctx, "ag:slack:C123")
	if err != nil {
		t.Fatalf("get after skip: %v", err)
	}
	if got3.Cursor != "1712345678.100000" {
		t.Errorf("first-write-wins violated: cursor = %q, want 1712345678.100000", got3.Cursor)
	}
}

func TestBulkInsertExternalChatCursorsRejectsEmptyFields(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	cases := []struct {
		name string
		rec  *ExternalChatCursorRecord
	}{
		{"empty id", &ExternalChatCursorRecord{ID: "", Source: "slack", Cursor: "c"}},
		{"empty source", &ExternalChatCursorRecord{ID: "x", Source: "", Cursor: "c"}},
		{"empty cursor", &ExternalChatCursorRecord{ID: "x", Source: "slack", Cursor: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.BulkInsertExternalChatCursors(ctx, []*ExternalChatCursorRecord{tc.rec}, ExternalChatCursorInsertOptions{}); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestGetExternalChatCursorNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetExternalChatCursor(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestBulkInsertExternalChatCursorsInBatchDuplicate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	recs := []*ExternalChatCursorRecord{
		{ID: "dup", Source: "slack", Cursor: "first"},
		{ID: "dup", Source: "discord", Cursor: "second"},
	}
	n, err := s.BulkInsertExternalChatCursors(ctx, recs, ExternalChatCursorInsertOptions{})
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if n != 1 {
		t.Errorf("inserted = %d, want 1 (first-write-wins on dup)", n)
	}
	got, err := s.GetExternalChatCursor(ctx, "dup")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Cursor != "first" || got.Source != "slack" {
		t.Errorf("first-write-wins violated: %+v", got)
	}
}

func TestBulkInsertExternalChatCursorsEmptyStringsNormalized(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	empty := ""
	recs := []*ExternalChatCursorRecord{
		{ID: "x", Source: "slack", AgentID: &empty, ChannelID: &empty, Cursor: "c"},
	}
	n, err := s.BulkInsertExternalChatCursors(ctx, recs, ExternalChatCursorInsertOptions{})
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if n != 1 {
		t.Errorf("inserted = %d, want 1", n)
	}
	if recs[0].AgentID != nil {
		t.Errorf("staged AgentID should be nil after normalization, got %v", recs[0].AgentID)
	}
	if recs[0].ChannelID != nil {
		t.Errorf("staged ChannelID should be nil after normalization, got %v", recs[0].ChannelID)
	}
	got, err := s.GetExternalChatCursor(ctx, "x")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AgentID != nil {
		t.Errorf("agent_id should be nil, got %v", got.AgentID)
	}
	if got.ChannelID != nil {
		t.Errorf("channel_id should be nil, got %v", got.ChannelID)
	}
	if got.ETag != recs[0].ETag {
		t.Errorf("etag mismatch (insert vs read-back): %q vs %q", recs[0].ETag, got.ETag)
	}
	want, err := computeExternalChatCursorETag(got)
	if err != nil {
		t.Fatalf("recompute etag: %v", err)
	}
	if got.ETag != want {
		t.Errorf("read-back etag != recomputed etag: %q vs %q", got.ETag, want)
	}
}

func stringPtr(s string) *string { return &s }

package store

import (
	"context"
	"testing"
)

func TestUpsertRemoteMirrorMessagesAndList(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rows := []RemoteMirrorMessage{
		{ID: "m_1", Role: "user", Content: "hi", Timestamp: "2026-05-26T10:00:00Z"},
		{ID: "m_2", Role: "assistant", Content: "yo", Timestamp: "2026-05-26T10:00:01Z"},
		{ID: "m_3", Role: "user", Content: "?", Timestamp: "2026-05-26T10:00:02Z"},
	}
	if err := s.UpsertRemoteMirrorMessages(ctx, "ag_a", "peer_b", rows); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, hasMore, err := s.ListRemoteMirrorMessages(ctx, "ag_a", 10, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if hasMore {
		t.Fatalf("hasMore=true, want false")
	}
	if len(got) != 3 {
		t.Fatalf("len=%d, want 3", len(got))
	}
	// oldest-first
	wantIDs := []string{"m_1", "m_2", "m_3"}
	for i, m := range got {
		if m.ID != wantIDs[i] {
			t.Errorf("got[%d].ID=%q, want %q", i, m.ID, wantIDs[i])
		}
	}
}

func TestUpsertRemoteMirrorDedup(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	first := []RemoteMirrorMessage{
		{ID: "m_x", Role: "user", Content: "old", Timestamp: "2026-05-26T10:00:00Z"},
	}
	if err := s.UpsertRemoteMirrorMessages(ctx, "ag_a", "peer_b", first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// 同じ message_id を別 content で再 upsert
	second := []RemoteMirrorMessage{
		{ID: "m_x", Role: "user", Content: "new", Timestamp: "2026-05-26T10:00:05Z"},
	}
	if err := s.UpsertRemoteMirrorMessages(ctx, "ag_a", "peer_b", second); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, _, err := s.ListRemoteMirrorMessages(ctx, "ag_a", 10, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1 (dedup)", len(got))
	}
	if got[0].Content != "new" {
		t.Errorf("content=%q, want %q (upsert overwrites)", got[0].Content, "new")
	}
}

func TestListRemoteMirrorBeforePagination(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rows := []RemoteMirrorMessage{
		{ID: "m_1", Role: "user", Content: "a", Timestamp: "2026-05-26T10:00:00Z"},
		{ID: "m_2", Role: "user", Content: "b", Timestamp: "2026-05-26T10:00:01Z"},
		{ID: "m_3", Role: "user", Content: "c", Timestamp: "2026-05-26T10:00:02Z"},
		{ID: "m_4", Role: "user", Content: "d", Timestamp: "2026-05-26T10:00:03Z"},
	}
	if err := s.UpsertRemoteMirrorMessages(ctx, "ag_a", "peer_b", rows); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// limit=2, no cursor: 最新 2 件を oldest-first で返す + hasMore=true
	got, hasMore, err := s.ListRemoteMirrorMessages(ctx, "ag_a", 2, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !hasMore {
		t.Errorf("hasMore=false, want true")
	}
	if len(got) != 2 || got[0].ID != "m_3" || got[1].ID != "m_4" {
		t.Errorf("page1=%v, want [m_3,m_4]", idsOf(got))
	}

	// before=m_3: m_3 より古い 2 件 [m_1, m_2]
	got, hasMore, err = s.ListRemoteMirrorMessages(ctx, "ag_a", 2, "m_3")
	if err != nil {
		t.Fatalf("list with before: %v", err)
	}
	if hasMore {
		t.Errorf("hasMore=true, want false")
	}
	if len(got) != 2 || got[0].ID != "m_1" || got[1].ID != "m_2" {
		t.Errorf("page2=%v, want [m_1,m_2]", idsOf(got))
	}
}

func TestDeleteRemoteMirrorForAgent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rowsA := []RemoteMirrorMessage{
		{ID: "m_1", Role: "user", Content: "a", Timestamp: "2026-05-26T10:00:00Z"},
		{ID: "m_2", Role: "user", Content: "b", Timestamp: "2026-05-26T10:00:01Z"},
	}
	rowsB := []RemoteMirrorMessage{
		{ID: "m_x", Role: "user", Content: "z", Timestamp: "2026-05-26T10:00:00Z"},
	}
	if err := s.UpsertRemoteMirrorMessages(ctx, "ag_a", "peer_b", rowsA); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	if err := s.UpsertRemoteMirrorMessages(ctx, "ag_other", "peer_b", rowsB); err != nil {
		t.Fatalf("upsert B: %v", err)
	}

	n, err := s.DeleteRemoteMirrorForAgent(ctx, "ag_a")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted=%d, want 2", n)
	}

	// ag_a は空
	got, _, _ := s.ListRemoteMirrorMessages(ctx, "ag_a", 10, "")
	if len(got) != 0 {
		t.Errorf("after delete ag_a got %d rows, want 0", len(got))
	}
	// ag_other は無傷
	got, _, _ = s.ListRemoteMirrorMessages(ctx, "ag_other", 10, "")
	if len(got) != 1 {
		t.Errorf("ag_other got %d rows, want 1", len(got))
	}
}

func idsOf(rows []RemoteMirrorMessage) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

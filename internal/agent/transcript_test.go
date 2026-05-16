package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMessagesPaginated(t *testing.T) {
	// Create a temp agents dir
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	agentID := "ag_test_pagination"
	dir := filepath.Join(tmpDir, ".config", "kojo-v1", "agents", agentID)
	os.MkdirAll(dir, 0o755)
	transcriptTestSetup(t, agentID)

	// Write test messages
	for i, content := range []string{"msg1", "msg2", "msg3", "msg4", "msg5"} {
		msg := &Message{
			ID:        "m_" + string(rune('a'+i)),
			Role:      "user",
			Content:   content,
			Timestamp: "2024-01-01T00:00:0" + string(rune('0'+i)) + "Z",
		}
		if err := appendMessage(agentID, msg); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("load all", func(t *testing.T) {
		msgs, hasMore, err := loadMessagesPaginated(agentID, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 5 {
			t.Errorf("expected 5 messages, got %d", len(msgs))
		}
		if hasMore {
			t.Error("expected hasMore=false")
		}
	})

	t.Run("load with limit", func(t *testing.T) {
		msgs, hasMore, err := loadMessagesPaginated(agentID, 3, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 3 {
			t.Errorf("expected 3 messages, got %d", len(msgs))
		}
		if !hasMore {
			t.Error("expected hasMore=true")
		}
		// Should be the last 3 messages
		if msgs[0].Content != "msg3" {
			t.Errorf("expected msg3, got %s", msgs[0].Content)
		}
	})

	t.Run("load with before cursor", func(t *testing.T) {
		msgs, hasMore, err := loadMessagesPaginated(agentID, 2, "m_c")
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 2 {
			t.Errorf("expected 2 messages, got %d", len(msgs))
		}
		if msgs[0].Content != "msg1" {
			t.Errorf("expected msg1, got %s", msgs[0].Content)
		}
		if msgs[1].Content != "msg2" {
			t.Errorf("expected msg2, got %s", msgs[1].Content)
		}
		if hasMore {
			t.Error("expected hasMore=false")
		}
	})

	t.Run("nonexistent agent", func(t *testing.T) {
		msgs, hasMore, err := loadMessagesPaginated("ag_nonexistent", 10, "")
		if err != nil {
			t.Fatal(err)
		}
		if msgs != nil {
			t.Errorf("expected nil, got %v", msgs)
		}
		if hasMore {
			t.Error("expected hasMore=false")
		}
	})
}

func TestUpdateAndDeleteMessage(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	agentID := "ag_test_edit"
	dir := filepath.Join(tmpDir, ".config", "kojo-v1", "agents", agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcriptTestSetup(t, agentID)

	ids := []string{"m_a", "m_b", "m_c"}
	for i, id := range ids {
		msg := &Message{
			ID:        id,
			Role:      "user",
			Content:   "c" + string(rune('0'+i)),
			Timestamp: "2024-01-01T00:00:0" + string(rune('0'+i)) + "Z",
		}
		if err := appendMessage(agentID, msg); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("update content", func(t *testing.T) {
		updated, etag, err := updateMessageContent(agentID, "m_b", "edited", "")
		if err != nil {
			t.Fatal(err)
		}
		if updated.Content != "edited" {
			t.Errorf("expected edited, got %q", updated.Content)
		}
		if etag == "" {
			t.Error("expected non-empty etag from store update")
		}
		msgs, _, err := loadMessagesPaginated(agentID, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 3 {
			t.Fatalf("expected 3 messages, got %d", len(msgs))
		}
		if msgs[1].Content != "edited" {
			t.Errorf("expected edited, got %q", msgs[1].Content)
		}
		if msgs[0].Content != "c0" || msgs[2].Content != "c2" {
			t.Errorf("other messages mutated: %q, %q", msgs[0].Content, msgs[2].Content)
		}
	})

	t.Run("update nonexistent", func(t *testing.T) {
		_, _, err := updateMessageContent(agentID, "m_zzz", "x", "")
		if !errors.Is(err, ErrMessageNotFound) {
			t.Errorf("expected ErrMessageNotFound, got %v", err)
		}
	})

	t.Run("update with stale If-Match", func(t *testing.T) {
		_, _, err := updateMessageContent(agentID, "m_a", "again", "v0-stale")
		if !errors.Is(err, ErrMessageETagMismatch) {
			t.Errorf("expected ErrMessageETagMismatch, got %v", err)
		}
	})

	t.Run("update with matching If-Match", func(t *testing.T) {
		// Read the current etag, then send it back as ifMatch — must succeed.
		msgs, _, err := loadMessagesPaginated(agentID, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		var current string
		for _, m := range msgs {
			if m.ID == "m_a" {
				current = m.ETag
				break
			}
		}
		if current == "" {
			t.Fatal("m_a has no etag — recordToMessage broken")
		}
		_, newETag, err := updateMessageContent(agentID, "m_a", "matched", current)
		if err != nil {
			t.Fatalf("matching ifMatch should succeed, got %v", err)
		}
		if newETag == current {
			t.Error("etag should advance after update")
		}
	})

	t.Run("delete message", func(t *testing.T) {
		if err := deleteMessage(agentID, "m_a", ""); err != nil {
			t.Fatal(err)
		}
		msgs, _, err := loadMessagesPaginated(agentID, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 2 {
			t.Fatalf("expected 2 messages, got %d", len(msgs))
		}
		for _, m := range msgs {
			if m.ID == "m_a" {
				t.Errorf("m_a should be deleted")
			}
		}
	})

	t.Run("delete nonexistent", func(t *testing.T) {
		err := deleteMessage(agentID, "m_zzz", "")
		if !errors.Is(err, ErrMessageNotFound) {
			t.Errorf("expected ErrMessageNotFound, got %v", err)
		}
	})

	t.Run("delete with stale If-Match", func(t *testing.T) {
		err := deleteMessage(agentID, "m_b", "v0-stale")
		if !errors.Is(err, ErrMessageETagMismatch) {
			t.Errorf("expected ErrMessageETagMismatch, got %v", err)
		}
		// Row must still be alive — the precondition failed before the write.
		msgs, _, err := loadMessagesPaginated(agentID, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		var stillThere bool
		for _, m := range msgs {
			if m.ID == "m_b" {
				stillThere = true
			}
		}
		if !stillThere {
			t.Error("m_b should not be deleted on stale If-Match")
		}
	})

	t.Run("delete with matching If-Match", func(t *testing.T) {
		msgs, _, err := loadMessagesPaginated(agentID, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		var current string
		for _, m := range msgs {
			if m.ID == "m_b" {
				current = m.ETag
				break
			}
		}
		if current == "" {
			t.Fatal("m_b has no etag — recordToMessage broken")
		}
		if err := deleteMessage(agentID, "m_b", current); err != nil {
			t.Fatalf("matching If-Match should succeed, got %v", err)
		}
	})

	t.Run("delete with If-Match against vanished row", func(t *testing.T) {
		// m_b is now tombstoned — a conditional follow-up must surface
		// not-found so the caller can refetch instead of silently no-op.
		err := deleteMessage(agentID, "m_b", "v0-anything")
		if !errors.Is(err, ErrMessageNotFound) {
			t.Errorf("expected ErrMessageNotFound for tombstoned row, got %v", err)
		}
	})
}

func TestFindRegenerateTargetAndTruncate(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	agentID := "ag_test_regen"
	dir := filepath.Join(tmpDir, ".config", "kojo-v1", "agents", agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcriptTestSetup(t, agentID, "ag_test_regen_bad")

	type row struct {
		id, role, content string
	}
	rows := []row{
		{"m_u1", "user", "hello"},
		{"m_a1", "assistant", "hi"},
		{"m_u2", "user", "again"},
		{"m_a2", "assistant", "sure"},
		{"m_u3", "user", "more"},
	}
	for i, r := range rows {
		msg := &Message{
			ID:        r.id,
			Role:      r.role,
			Content:   r.content,
			Timestamp: "2024-01-01T00:00:0" + string(rune('0'+i)) + "Z",
		}
		if err := appendMessage(agentID, msg); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("assistant target cuts inclusive", func(t *testing.T) {
		rt, err := findRegenerateTarget(context.Background(), agentID, "m_a2", "")
		if err != nil {
			t.Fatal(err)
		}
		if rt.SourceID != "m_u2" {
			t.Errorf("expected source m_u2, got %s", rt.SourceID)
		}
		if rt.PivotID != "m_a2" {
			t.Errorf("expected pivot m_a2, got %s", rt.PivotID)
		}
		if !rt.KillPivot {
			t.Errorf("assistant mode should kill pivot")
		}
	})

	t.Run("user target cuts exclusive", func(t *testing.T) {
		rt, err := findRegenerateTarget(context.Background(), agentID, "m_u2", "")
		if err != nil {
			t.Fatal(err)
		}
		if rt.SourceID != "m_u2" {
			t.Errorf("expected source m_u2, got %s", rt.SourceID)
		}
		if rt.PivotID != "m_u2" {
			t.Errorf("expected pivot m_u2, got %s", rt.PivotID)
		}
		if rt.KillPivot {
			t.Errorf("user mode must keep pivot")
		}
	})

	t.Run("assistant with no preceding user", func(t *testing.T) {
		// Fresh agent with only assistant messages.
		otherID := "ag_test_regen_bad"
		if err := os.MkdirAll(filepath.Join(tmpDir, ".config", "kojo-v1", "agents", otherID), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := appendMessage(otherID, &Message{ID: "m_x", Role: "assistant", Content: "orphan", Timestamp: "2024-01-01T00:00:00Z"}); err != nil {
			t.Fatal(err)
		}
		_, err := findRegenerateTarget(context.Background(), otherID, "m_x", "")
		if !errors.Is(err, ErrInvalidRegenerate) {
			t.Errorf("expected ErrInvalidRegenerate, got %v", err)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, err := findRegenerateTarget(context.Background(), agentID, "m_zzz", "")
		if !errors.Is(err, ErrMessageNotFound) {
			t.Errorf("expected ErrMessageNotFound, got %v", err)
		}
	})

	t.Run("stale If-Match on clicked message", func(t *testing.T) {
		_, err := findRegenerateTarget(context.Background(), agentID, "m_a2", "v0-stale")
		if !errors.Is(err, ErrMessageETagMismatch) {
			t.Errorf("expected ErrMessageETagMismatch, got %v", err)
		}
	})

	t.Run("matching If-Match on clicked message", func(t *testing.T) {
		// Read the live etag of m_a2 and feed it back — must succeed.
		msgs, _, err := loadMessagesPaginated(agentID, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		var current string
		for _, m := range msgs {
			if m.ID == "m_a2" {
				current = m.ETag
				break
			}
		}
		if current == "" {
			t.Fatal("m_a2 has no etag")
		}
		rt, err := findRegenerateTarget(context.Background(), agentID, "m_a2", current)
		if err != nil {
			t.Fatalf("matching ifMatch should succeed, got %v", err)
		}
		if rt.SourceID != "m_u2" || rt.PivotID != "m_a2" || !rt.KillPivot {
			t.Errorf("unexpected result: %+v", rt)
		}
	})

	t.Run("truncate keeps prefix", func(t *testing.T) {
		if err := truncateMessagesTo(agentID, 3); err != nil {
			t.Fatal(err)
		}
		msgs, _, err := loadMessagesPaginated(agentID, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 3 {
			t.Fatalf("expected 3 messages, got %d", len(msgs))
		}
		if msgs[2].ID != "m_u2" {
			t.Errorf("expected last=m_u2, got %s", msgs[2].ID)
		}
	})
}

package agent

import (
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
	dir := filepath.Join(tmpDir, ".config", "kojo", "agents", agentID)
	os.MkdirAll(dir, 0o755)

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
	dir := filepath.Join(tmpDir, ".config", "kojo", "agents", agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

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
		updated, err := updateMessageContent(agentID, "m_b", "edited")
		if err != nil {
			t.Fatal(err)
		}
		if updated.Content != "edited" {
			t.Errorf("expected edited, got %q", updated.Content)
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
		_, err := updateMessageContent(agentID, "m_zzz", "x")
		if !errors.Is(err, ErrMessageNotFound) {
			t.Errorf("expected ErrMessageNotFound, got %v", err)
		}
	})

	t.Run("delete message", func(t *testing.T) {
		if err := deleteMessage(agentID, "m_a"); err != nil {
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
		err := deleteMessage(agentID, "m_zzz")
		if !errors.Is(err, ErrMessageNotFound) {
			t.Errorf("expected ErrMessageNotFound, got %v", err)
		}
	})
}

func TestFindRegenerateTargetAndTruncate(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	agentID := "ag_test_regen"
	dir := filepath.Join(tmpDir, ".config", "kojo", "agents", agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

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
		target, keep, err := findRegenerateTarget(agentID, "m_a2")
		if err != nil {
			t.Fatal(err)
		}
		if target.ID != "m_u2" {
			t.Errorf("expected m_u2 as source, got %s", target.ID)
		}
		if keep != 3 {
			t.Errorf("expected keepCount=3, got %d", keep)
		}
	})

	t.Run("user target cuts exclusive", func(t *testing.T) {
		target, keep, err := findRegenerateTarget(agentID, "m_u2")
		if err != nil {
			t.Fatal(err)
		}
		if target.ID != "m_u2" {
			t.Errorf("expected m_u2 as source, got %s", target.ID)
		}
		if keep != 3 {
			t.Errorf("expected keepCount=3, got %d", keep)
		}
	})

	t.Run("assistant with no preceding user", func(t *testing.T) {
		// Fresh agent with only assistant messages.
		otherID := "ag_test_regen_bad"
		if err := os.MkdirAll(filepath.Join(tmpDir, ".config", "kojo", "agents", otherID), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := appendMessage(otherID, &Message{ID: "m_x", Role: "assistant", Content: "orphan", Timestamp: "2024-01-01T00:00:00Z"}); err != nil {
			t.Fatal(err)
		}
		_, _, err := findRegenerateTarget(otherID, "m_x")
		if !errors.Is(err, ErrInvalidRegenerate) {
			t.Errorf("expected ErrInvalidRegenerate, got %v", err)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, _, err := findRegenerateTarget(agentID, "m_zzz")
		if !errors.Is(err, ErrMessageNotFound) {
			t.Errorf("expected ErrMessageNotFound, got %v", err)
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

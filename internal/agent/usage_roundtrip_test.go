package agent

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/loppo-llc/kojo/internal/configdir"
	"github.com/loppo-llc/kojo/internal/store"
)

// TestMessageUsageRoundTrip locks down BUG 1's persistence chain: an
// assistant message assembled with a *Usage must survive a store append,
// and the same token metrics must reappear when the transcript is read
// back via the messages-list path (loadMessages) that the GET
// /api/v1/agents/{id}/messages handler serves. Before the fix the stream
// never populated Usage, so this guards the whole path from
// assembleAssistantMessage → appendMessage → loadMessages.
func TestMessageUsageRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, ".config"))

	st, err := store.Open(context.Background(), store.Options{ConfigDir: configdir.Path()})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
		setGlobalStore(nil)
	})
	setGlobalStore(st)

	const agentID = "ag_usage"
	if _, err := st.InsertAgent(context.Background(), &store.AgentRecord{
		ID:       agentID,
		Name:     "alice",
		Settings: map[string]any{"tool": "claude", "model": "claude-fable-5"},
	}, store.AgentInsertOptions{}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	msg := assembleAssistantMessage("hello", "", nil, &Usage{
		InputTokens:              3050,
		OutputTokens:             13,
		CacheReadInputTokens:     0,
		CacheCreationInputTokens: 21439,
	})
	if err := appendMessage(agentID, msg); err != nil {
		t.Fatalf("appendMessage: %v", err)
	}

	msgs, err := loadMessages(agentID, 0)
	if err != nil {
		t.Fatalf("loadMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	got := msgs[0]
	if got.Usage == nil {
		t.Fatal("usage did not survive store round-trip (nil after read)")
	}
	if got.Usage.InputTokens != 3050 || got.Usage.OutputTokens != 13 ||
		got.Usage.CacheCreationInputTokens != 21439 {
		t.Errorf("usage mismatch after round-trip: %+v", got.Usage)
	}
}

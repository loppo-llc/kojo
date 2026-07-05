package agent

import (
	"context"
	"testing"
	"time"
)

// TestCreateThread_AlwaysNewRoom verifies that CreateThread never dedups:
// repeated calls for the same agent yield distinct kind="thread" rooms.
func TestCreateThread_AlwaysNewRoom(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)

	a, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	b, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatalf("CreateThread deduped: both rooms have id %s", a.ID)
	}
	for _, g := range []*GroupDM{a, b} {
		if g.Kind != GroupDMKindThread {
			t.Errorf("kind = %q, want thread", g.Kind)
		}
		if len(g.Members) != 1 || g.Members[0].AgentID != "ag_alice" {
			t.Errorf("members = %+v, want single ag_alice", g.Members)
		}
		if g.Name != DefaultThreadName {
			t.Errorf("default name = %q, want %q", g.Name, DefaultThreadName)
		}
	}
}

// TestThreadKind_TriggersThreadTurn verifies a kind="thread" room runs the
// one-shot thread turn (not the notify fan-out) on a user post.
func TestThreadKind_TriggersThreadTurn(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{reply: "pong"}
	gdm.oneShot = stub.fn

	g, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "ping", nil, true); err != nil {
		t.Fatal(err)
	}
	reply := waitForMessage(t, gdm, g.ID, "pong")
	if reply.AgentID != "ag_alice" {
		t.Errorf("reply author = %q, want ag_alice", reply.AgentID)
	}
	gdm.notifyMu.Lock()
	_, exists := gdm.notify[g.ID+":ag_alice"]
	gdm.notifyMu.Unlock()
	if exists {
		t.Errorf("thread post should not create notify state")
	}
}

// usageThreadStub emits a text delta plus a done event carrying token usage.
type usageThreadStub struct {
	reply string
	usage *Usage
}

func (s *usageThreadStub) fn(ctx context.Context, agentID, userMessage string, opts OneShotOpts) (<-chan ChatEvent, error) {
	ch := make(chan ChatEvent, 3)
	if s.reply != "" {
		ch <- ChatEvent{Type: "text", Delta: s.reply}
	}
	ch <- ChatEvent{Type: "done", Usage: s.usage}
	close(ch)
	return ch, nil
}

// TestThreadTurn_PersistsUsage verifies the done event's Usage is attached to
// the agent's thread reply and survives a reload from the store.
func TestThreadTurn_PersistsUsage(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &usageThreadStub{
		reply: "answer",
		usage: &Usage{InputTokens: 120, OutputTokens: 45, CacheReadInputTokens: 30, CacheCreationInputTokens: 10},
	}
	gdm.oneShot = stub.fn

	g, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "question", nil, true); err != nil {
		t.Fatal(err)
	}
	reply := waitForMessage(t, gdm, g.ID, "answer")
	if reply.Usage == nil {
		t.Fatalf("reply usage not persisted")
	}
	if reply.Usage.InputTokens != 120 || reply.Usage.OutputTokens != 45 ||
		reply.Usage.CacheReadInputTokens != 30 || reply.Usage.CacheCreationInputTokens != 10 {
		t.Errorf("usage = %+v, want {120 45 30 10}", *reply.Usage)
	}
	// The user message must not carry usage.
	msgs, _, _, err := gdm.Messages(g.ID, 50, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.AgentID == UserSenderID && m.Usage != nil {
			t.Errorf("user message unexpectedly has usage")
		}
	}
}

// thinkingToolUsesThreadStub emits a text delta plus a done event carrying an
// assembled message with Thinking and ToolUses set.
type thinkingToolUsesThreadStub struct {
	reply    string
	thinking string
	toolUses []ToolUse
}

func (s *thinkingToolUsesThreadStub) fn(ctx context.Context, agentID, userMessage string, opts OneShotOpts) (<-chan ChatEvent, error) {
	ch := make(chan ChatEvent, 3)
	if s.reply != "" {
		ch <- ChatEvent{Type: "text", Delta: s.reply}
	}
	ch <- ChatEvent{Type: "done", Message: &Message{
		Content:  s.reply,
		Thinking: s.thinking,
		ToolUses: s.toolUses,
	}}
	close(ch)
	return ch, nil
}

// TestThreadTurn_PersistsThinkingAndToolUses verifies the done event's
// assembled message Thinking and ToolUses are attached to the agent's thread
// reply and survive a reload from the store.
func TestThreadTurn_PersistsThinkingAndToolUses(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &thinkingToolUsesThreadStub{
		reply:    "answer",
		thinking: "let me consider this carefully",
		toolUses: []ToolUse{{ID: "tu_1", Name: "shell", Input: "ls -la"}},
	}
	gdm.oneShot = stub.fn

	g, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "question", nil, true); err != nil {
		t.Fatal(err)
	}
	reply := waitForMessage(t, gdm, g.ID, "answer")
	if reply.Thinking != "let me consider this carefully" {
		t.Errorf("thinking = %q, want %q", reply.Thinking, "let me consider this carefully")
	}
	if len(reply.ToolUses) != 1 || reply.ToolUses[0].Name != "shell" || reply.ToolUses[0].Input != "ls -la" {
		t.Errorf("toolUses = %+v, want [{shell ls -la}]", reply.ToolUses)
	}

	// Reload from the store (not the in-memory cache) to verify persistence.
	msgs, _, _, err := gdm.Messages(g.ID, 50, "")
	if err != nil {
		t.Fatal(err)
	}
	var reloaded *GroupMessage
	for _, m := range msgs {
		if m.ID == reply.ID {
			reloaded = m
		}
	}
	if reloaded == nil {
		t.Fatalf("reply %s not found on reload", reply.ID)
	}
	if reloaded.Thinking != "let me consider this carefully" {
		t.Errorf("reloaded thinking = %q, want %q", reloaded.Thinking, "let me consider this carefully")
	}
	if len(reloaded.ToolUses) != 1 || reloaded.ToolUses[0].Name != "shell" || reloaded.ToolUses[0].Input != "ls -la" {
		t.Errorf("reloaded toolUses = %+v, want [{shell ls -la}]", reloaded.ToolUses)
	}
}

// TestThreadAutoTitle_SetOnceNotOverwritten verifies the thread is auto-titled
// from the first user message and later turns do not overwrite the title.
func TestThreadAutoTitle_SetOnceNotOverwritten(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{reply: "ok"}
	gdm.oneShot = stub.fn

	g, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	if g.Name != DefaultThreadName {
		t.Fatalf("initial name = %q, want default %q", g.Name, DefaultThreadName)
	}

	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "Fix the flaky build please", nil, true); err != nil {
		t.Fatal(err)
	}
	waitForMessage(t, gdm, g.ID, "ok")

	var titled string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cur, ok := gdm.Get(g.ID); ok && cur.Name != DefaultThreadName {
			titled = cur.Name
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if titled == "" {
		t.Fatalf("thread was not auto-titled")
	}
	if titled != "Fix the flaky build please" {
		t.Errorf("auto-title = %q, want first user message", titled)
	}

	// A second turn must not overwrite the title.
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "Now add a test", nil, true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	cur, _ := gdm.Get(g.ID)
	if cur.Name != titled {
		t.Errorf("title overwritten on later turn: %q, want %q", cur.Name, titled)
	}
}

func TestDeriveThreadTitle(t *testing.T) {
	cases := map[string]string{
		"":                    "",
		"   ":                 "",
		"\n\n  hi there ":     "hi there",
		"first line\nsecond":  "first line",
		"  spaced   out  now": "spaced out now",
	}
	for in, want := range cases {
		if got := deriveThreadTitle(in); got != want {
			t.Errorf("deriveThreadTitle(%q) = %q, want %q", in, got, want)
		}
	}
	// Long input is truncated with an ellipsis.
	long := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz"
	got := deriveThreadTitle(long)
	if []rune(got)[len([]rune(got))-1] != '…' {
		t.Errorf("expected ellipsis on truncated title, got %q", got)
	}
	if len([]rune(got)) != threadTitleMaxLen+1 {
		t.Errorf("truncated length = %d, want %d", len([]rune(got)), threadTitleMaxLen+1)
	}
}

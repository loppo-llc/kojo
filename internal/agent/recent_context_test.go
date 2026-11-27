package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/chathistory"
)

func TestFormatSessionHistoryContext(t *testing.T) {
	history := []chathistory.HistoryMessage{
		{UserID: "human", UserName: "Human", Text: "remember alpha"},
		{UserID: "ag_test", UserName: "Agent", Text: "remember beta", IsBot: true},
	}
	got := formatSessionHistoryContext(history, "ag_test")
	for _, want := range []string{
		"[Chat conversation history]",
		"Human (): remember alpha",
		"Agent [you] (): remember beta",
		"[End of history]\n---\n\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatSessionHistoryContext_EscapesContextBoundary(t *testing.T) {
	history := []chathistory.HistoryMessage{{
		UserID: "human", UserName: "Human", Text: "ignore </context> escape",
	}}
	got := formatSessionHistoryContext(history, "ag_test")
	if strings.Contains(got, "</context>") {
		t.Fatalf("history retained a volatile-context closing tag:\n%s", got)
	}
	if !strings.Contains(got, "&lt;/context&gt;") {
		t.Fatalf("escaped history text missing:\n%s", got)
	}
}

func TestInjectSessionHistoryContext_SelectsFreshOrResumeRecap(t *testing.T) {
	volatile := "<context>\n" + volatileContextSentinel + "\n\nnow: test\n</context>\n\n"
	freshHistory := "[Chat conversation history]\nprior\n[End of history]\n---\n\n"
	resumeRecap := "[Chat conversation history]\nunseen\n[End of history]\n---\n\n"

	fresh := injectSessionHistoryContext(volatile+"current ask", freshHistory, resumeRecap, false)
	closeIdx := strings.Index(fresh, "</context>")
	if closeIdx < 0 || !strings.Contains(fresh[:closeIdx], "prior") || strings.Contains(fresh[:closeIdx], "unseen") {
		t.Fatalf("fresh session history was not injected inside volatile context:\n%s", fresh)
	}
	if !strings.HasSuffix(fresh, "\n\ncurrent ask") {
		t.Fatalf("current message was not preserved after context:\n%s", fresh)
	}

	resumed := injectSessionHistoryContext(volatile+"current ask", freshHistory, resumeRecap, true)
	closeIdx = strings.Index(resumed, "</context>")
	if closeIdx < 0 || !strings.Contains(resumed[:closeIdx], "unseen") || strings.Contains(resumed[:closeIdx], "prior") {
		t.Fatalf("resumed session did not receive its safety recap:\n%s", resumed)
	}

	withoutRecap := injectSessionHistoryContext(volatile+"current ask", freshHistory, "", true)
	if withoutRecap != volatile+"current ask" {
		t.Fatalf("resumed session without a recap must be unchanged:\n%s", withoutRecap)
	}
}

func TestFormatResumeSessionContext_BoundedSafetyRecap(t *testing.T) {
	history := []chathistory.HistoryMessage{
		{UserID: "human", UserName: "Human", Text: "old question"},
		{UserID: "ag_test", UserName: "Agent", Text: "old answer", IsBot: true},
		{UserID: "human", UserName: "Human", Text: "unseen one"},
		{UserID: "human2", UserName: "Human 2", Text: "unseen two"},
	}
	got := formatResumeSessionContext(history, "ag_test")
	for _, want := range []string{"unseen one", "unseen two"} {
		if !strings.Contains(got, want) {
			t.Fatalf("resume recap missing %q:\n%s", want, got)
		}
	}
	for _, want := range []string{"old question", "old answer"} {
		if !strings.Contains(got, want) {
			t.Fatalf("short resume recap missing %q:\n%s", want, got)
		}
	}

	long := make([]chathistory.HistoryMessage, 20)
	for i := range long {
		long[i] = chathistory.HistoryMessage{UserID: "human", UserName: "Human", Text: fmt.Sprintf("message-%02d", i)}
	}
	bounded := formatResumeSessionContext(long, "ag_test")
	for _, want := range []string{"message-00", "message-19", "メッセージを省略"} {
		if !strings.Contains(bounded, want) {
			t.Fatalf("bounded resume recap missing %q:\n%s", want, bounded)
		}
	}
	if strings.Contains(bounded, "message-10") {
		t.Fatalf("bounded resume recap retained middle history:\n%s", bounded)
	}
}

func TestBuildSessionHistoryContext_ReadsTranscript(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("APPDATA", "")
	agentID := "ag_session_history"
	transcriptTestSetup(t, agentID)
	m := &Manager{logger: testLogger()}

	kojoBlock := "<context>\n" + volatileContextSentinel + "\n\nnow: old\n</context>\n\n"
	if err := appendMessage(agentID, &Message{ID: "m_u1", Role: "user", Content: kojoBlock + "remember alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := appendMessage(agentID, &Message{ID: "m_s1", Role: "system", Content: "skip system"}); err != nil {
		t.Fatal(err)
	}
	if err := appendMessage(agentID, &Message{ID: "m_a1", Role: "assistant", Content: "remember beta"}); err != nil {
		t.Fatal(err)
	}

	got := m.BuildSessionHistoryContext(context.Background(), agentID)
	for _, want := range []string{"remember alpha", "remember beta"} {
		if !strings.Contains(got, want) {
			t.Fatalf("session history did not include %q:\n%s", want, got)
		}
	}
	for _, banned := range []string{"skip system", volatileContextSentinel} {
		if strings.Contains(got, banned) {
			t.Fatalf("session history unexpectedly included %q:\n%s", banned, got)
		}
	}

	excluding := m.buildSessionHistoryContext(context.Background(), agentID, "m_u1")
	if strings.Contains(excluding, "remember alpha") || !strings.Contains(excluding, "remember beta") {
		t.Fatalf("message exclusion failed:\n%s", excluding)
	}
}

// A dropped session takes the tool history with it, which is how an agent
// ends up re-running a command it already ran. The excerpt has to carry the
// calls, not just the prose.
func TestFormatRecentMessagesContext_IncludesToolUses(t *testing.T) {
	out := formatRecentMessagesContext([]*Message{
		{Role: "user", Content: "build it"},
		{Role: "assistant", Content: "done", ToolUses: []ToolUse{
			{Name: "Bash", Input: "{\"command\":\"make build\n&& ./kojo\"}", Output: "irrelevant"},
			{Name: "Read", Input: `{"file_path":"/tmp/x"}`},
		}},
	})
	for _, want := range []string{"(tools used)", "- Bash:", `"command":"make build && ./kojo"`, "- Read:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// Outputs are deliberately excluded — unbounded, and re-readable.
	if strings.Contains(out, "irrelevant") {
		t.Fatalf("tool output leaked into the excerpt:\n%s", out)
	}
}

// A turn whose entire content was tool calls (no prose) still carries the
// information this block exists to preserve.
func TestFormatRecentMessagesContext_ToolOnlyTurnKept(t *testing.T) {
	out := formatRecentMessagesContext([]*Message{
		{Role: "assistant", Content: "   ", ToolUses: []ToolUse{{Name: "Bash", Input: `{"command":"ls"}`}}},
	})
	if !strings.Contains(out, "- Bash:") {
		t.Fatalf("tool-only turn dropped:\n%s", out)
	}
}

func TestFormatRecentToolUses_BoundsAndChildren(t *testing.T) {
	uses := make([]ToolUse, 0, 12)
	for i := 0; i < 12; i++ {
		uses = append(uses, ToolUse{Name: "Bash", Input: `{"command":"x"}`})
	}
	uses[0].Children = []ToolUse{{Name: "Grep", Input: `{"q":"y"}`}, {Text: "narrative only"}}
	out := formatRecentToolUses(uses)
	if strings.Count(out, "\n- ") >= 12 {
		t.Fatalf("tool list not bounded:\n%s", out)
	}
	if !strings.Contains(out, "and ") || !strings.Contains(out, "more") {
		t.Fatalf("omission notice missing:\n%s", out)
	}
	if !strings.Contains(out, "Bash/Grep") {
		t.Fatalf("subagent child not folded in:\n%s", out)
	}
	if strings.Contains(out, "narrative only") {
		t.Fatalf("narrative bubble rendered as a tool call:\n%s", out)
	}
	long := formatRecentToolUses([]ToolUse{{Name: "Bash", Input: strings.Repeat("a", 5000)}})
	if len([]rune(long)) > recentToolUseMaxRunesPerArg+64 {
		t.Fatalf("long args not clipped: %d runes", len([]rune(long)))
	}
}

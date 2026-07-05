package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// withFakeClassifier swaps the classifyEffort seam for the duration of a
// test and restores it afterwards.
func withFakeClassifier(t *testing.T, fn func(ctx context.Context, prompt string) (string, error)) {
	t.Helper()
	orig := classifyEffort
	classifyEffort = fn
	t.Cleanup(func() { classifyEffort = orig })
}

func fakeClassifierReturning(out string) func(context.Context, string) (string, error) {
	return func(context.Context, string) (string, error) { return out, nil }
}

// requireClaudeInPath fakes CLI availability so the LLM classifier path
// is reachable regardless of the test host's PATH.
func requireClaudeInPath(t *testing.T) {
	t.Helper()
	orig := classifierCLIAvailable
	classifierCLIAvailable = func() bool { return true }
	t.Cleanup(func() { classifierCLIAvailable = orig })
}

func TestResolveTurnEffortOptOutStatic(t *testing.T) {
	withFakeClassifier(t, func(context.Context, string) (string, error) {
		t.Fatal("classifier must not be called when auto effort is off")
		return "", nil
	})
	a := &Agent{Tool: "claude", Model: "sonnet", Effort: "high", AutoEffort: boolPtr(false)}
	eff, src := resolveTurnEffort(context.Background(), a, "debug this crash", false, "", testLogger())
	if eff != "high" || src != "static" {
		t.Fatalf("got (%q,%q), want (high,static)", eff, src)
	}
}

func TestResolveTurnEffortUnsupportedToolStatic(t *testing.T) {
	withFakeClassifier(t, func(context.Context, string) (string, error) {
		t.Fatal("classifier must not be called for non-claude/grok tools")
		return "", nil
	})
	for _, tool := range []string{"codex", "llama.cpp", "custom"} {
		a := &Agent{Tool: tool, Model: "gpt-5.5", Effort: "medium"}
		eff, src := resolveTurnEffort(context.Background(), a, "hi", false, "", testLogger())
		if eff != "medium" || src != "static" {
			t.Fatalf("tool %s: got (%q,%q), want (medium,static)", tool, eff, src)
		}
	}
}

func TestResolveTurnEffortSystemTurnRule(t *testing.T) {
	withFakeClassifier(t, func(context.Context, string) (string, error) {
		t.Fatal("system turns must not call the classifier")
		return "", nil
	})
	a := &Agent{Tool: "claude", Model: "sonnet", Effort: "high"}
	eff, src := resolveTurnEffort(context.Background(), a, "cron check-in", true, "", testLogger())
	if eff != "low" || src != "rule" {
		t.Fatalf("got (%q,%q), want (low,rule)", eff, src)
	}
	// Static already at/below low → keep static, no downgrade churn.
	a2 := &Agent{Tool: "claude", Model: "sonnet", Effort: "low"}
	eff, src = resolveTurnEffort(context.Background(), a2, "cron check-in", true, "", testLogger())
	if eff != "low" || src != "static" {
		t.Fatalf("got (%q,%q), want (low,static)", eff, src)
	}
}

func TestResolveTurnEffortHappyPath(t *testing.T) {
	requireClaudeInPath(t)
	cases := []struct {
		classifier string
		static     string
		want       string
	}{
		{"low", "high", "low"},
		{"medium", "high", "medium"},
		{"high", "high", "high"},
		{"LOW", "high", "low"},     // case/space tolerant
		{" medium\n", "high", "medium"},
		{"medium", "low", "low"},   // never raise above the static ceiling
	}
	for _, c := range cases {
		withFakeClassifier(t, fakeClassifierReturning(c.classifier))
		a := &Agent{Tool: "claude", Model: "sonnet", Effort: c.static}
		eff, src := resolveTurnEffort(context.Background(), a, "some question", false, "", testLogger())
		if eff != c.want || !strings.HasPrefix(src, "llm:") {
			t.Fatalf("classifier %q static %q: got (%q,%q), want (%q,llm)", c.classifier, c.static, eff, src, c.want)
		}
	}
}

func TestResolveTurnEffortHighKeepsXhighCeiling(t *testing.T) {
	requireClaudeInPath(t)
	withFakeClassifier(t, fakeClassifierReturning("high"))
	for _, static := range []string{"xhigh", "max"} {
		a := &Agent{Tool: "claude", Model: "opus", Effort: static}
		eff, src := resolveTurnEffort(context.Background(), a, "hard multi-file refactor", false, "", testLogger())
		if eff != static || !strings.HasPrefix(src, "llm:") {
			t.Fatalf("static %s: got (%q,%q), want (%s,llm)", static, eff, src, static)
		}
	}
}

func TestResolveTurnEffortClassifierErrorHeuristic(t *testing.T) {
	requireClaudeInPath(t)
	withFakeClassifier(t, func(context.Context, string) (string, error) {
		return "", errors.New("timeout")
	})
	a := &Agent{Tool: "claude", Model: "sonnet", Effort: "high"}

	// Short plain message → low.
	eff, src := resolveTurnEffort(context.Background(), a, "thanks!", false, "", testLogger())
	if eff != "low" || src != "heuristic" {
		t.Fatalf("short: got (%q,%q), want (low,heuristic)", eff, src)
	}
	// Long message → static (tagged "static" so the caller never
	// mistakes the echoed old value for a classifier verdict).
	long := strings.Repeat("な", 300)
	eff, src = resolveTurnEffort(context.Background(), a, long, false, "", testLogger())
	if eff != "high" || src != "static" {
		t.Fatalf("long: got (%q,%q), want (high,static)", eff, src)
	}
	// Short but contains a code fence → static.
	eff, _ = resolveTurnEffort(context.Background(), a, "```go\npanic()\n```", false, "", testLogger())
	if eff != "high" {
		t.Fatalf("code fence: got %q, want high", eff)
	}
	// Short but contains a URL → static.
	eff, _ = resolveTurnEffort(context.Background(), a, "read https://example.com/doc", false, "", testLogger())
	if eff != "high" {
		t.Fatalf("url: got %q, want high", eff)
	}
}

func TestResolveTurnEffortJunkOutputHeuristic(t *testing.T) {
	requireClaudeInPath(t)
	withFakeClassifier(t, fakeClassifierReturning("I think this is a medium difficulty task"))
	a := &Agent{Tool: "claude", Model: "sonnet", Effort: "high"}
	eff, src := resolveTurnEffort(context.Background(), a, "hey", false, "", testLogger())
	if eff != "low" || src != "heuristic" {
		t.Fatalf("got (%q,%q), want (low,heuristic)", eff, src)
	}
}

func TestResolveTurnEffortEmptyStaticTreatedAsHigh(t *testing.T) {
	requireClaudeInPath(t)
	// "" static = model default (≈ high tier): low classifier verdict
	// downgrades, high verdict keeps the default untouched.
	withFakeClassifier(t, fakeClassifierReturning("low"))
	a := &Agent{Tool: "claude", Model: "sonnet", Effort: ""}
	eff, src := resolveTurnEffort(context.Background(), a, "hi", false, "", testLogger())
	if eff != "low" || !strings.HasPrefix(src, "llm:") {
		t.Fatalf("got (%q,%q), want (low,llm)", eff, src)
	}
	withFakeClassifier(t, fakeClassifierReturning("high"))
	eff, _ = resolveTurnEffort(context.Background(), a, "hard task", false, "", testLogger())
	if eff != "high" {
		t.Fatalf("got %q, want high", eff)
	}
}

func TestMapTierToEffort(t *testing.T) {
	a := &Agent{Tool: "claude", Model: "sonnet", Effort: "medium"}
	// "high" resolves to high when the static tier is not above high.
	if got := mapTierToEffort(a, "high"); got != "high" {
		t.Fatalf("got %q, want high", got)
	}
	if got := mapTierToEffort(a, "low"); got != "low" {
		t.Fatalf("got %q, want low", got)
	}
	// medium tier on a medium agent — no change.
	if got := mapTierToEffort(a, "medium"); got != "medium" {
		t.Fatalf("got %q, want medium", got)
	}
	// xhigh/max static keeps the ceiling on "high".
	x := &Agent{Tool: "claude", Model: "opus", Effort: "xhigh"}
	if got := mapTierToEffort(x, "high"); got != "xhigh" {
		t.Fatalf("got %q, want xhigh", got)
	}
}

func TestIsAutoEffortEnabled(t *testing.T) {
	if !(&Agent{}).IsAutoEffortEnabled() {
		t.Fatal("nil AutoEffort must default to enabled")
	}
	if (&Agent{AutoEffort: boolPtr(false)}).IsAutoEffortEnabled() {
		t.Fatal("explicit false must disable")
	}
	if !(&Agent{AutoEffort: boolPtr(true)}).IsAutoEffortEnabled() {
		t.Fatal("explicit true must enable")
	}
	var nilAgent *Agent
	if !nilAgent.IsAutoEffortEnabled() {
		t.Fatal("nil agent defaults to enabled")
	}
}

func TestBuildEffortClassifierPromptCaps(t *testing.T) {
	diary := strings.Repeat("あ", 1000)
	msg := strings.Repeat("い", 3000)
	p := buildEffortClassifierPrompt(diary, msg)
	if strings.Count(p, "あ") != effortClassifierDiaryCap {
		t.Fatalf("diary not capped: %d", strings.Count(p, "あ"))
	}
	if strings.Count(p, "い") != effortClassifierMessageCap {
		t.Fatalf("message not capped: %d", strings.Count(p, "い"))
	}
	if !strings.Contains(p, "<message>") || !strings.Contains(p, "<recent-context>") {
		t.Fatal("delimiters missing")
	}
	// Empty diary omits the context block entirely.
	if strings.Contains(buildEffortClassifierPrompt("", "hi"), "recent-context") {
		t.Fatal("empty diary must omit the context block")
	}
}

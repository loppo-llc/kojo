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
	eff, src := resolveTurnEffort(context.Background(), a, "debug this crash", false, "", "", testLogger())
	if eff != "high" || src != "static" {
		t.Fatalf("got (%q,%q), want (high,static)", eff, src)
	}
}

func TestResolveTurnEffortUnsupportedToolStatic(t *testing.T) {
	withFakeClassifier(t, func(context.Context, string) (string, error) {
		t.Fatal("classifier must not be called for non-claude/grok tools")
		return "", nil
	})
	for _, tool := range []string{"codex", "custom-bare", "custom-claude", "custom-codex"} {
		a := &Agent{Tool: tool, Model: "gpt-5.5", Effort: "medium"}
		eff, src := resolveTurnEffort(context.Background(), a, "hi", false, "", "", testLogger())
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
	eff, src := resolveTurnEffort(context.Background(), a, "cron check-in", true, "", "", testLogger())
	if eff != "low" || src != "rule" {
		t.Fatalf("got (%q,%q), want (low,rule)", eff, src)
	}
	// Static already at/below low → keep static, no downgrade churn.
	a2 := &Agent{Tool: "claude", Model: "sonnet", Effort: "low"}
	eff, src = resolveTurnEffort(context.Background(), a2, "cron check-in", true, "", "", testLogger())
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
		{"LOW", "high", "low"}, // case/space tolerant
		{" medium\n", "high", "medium"},
		{"medium", "low", "low"}, // never raise above the static ceiling
	}
	for _, c := range cases {
		withFakeClassifier(t, fakeClassifierReturning(c.classifier))
		a := &Agent{Tool: "claude", Model: "sonnet", Effort: c.static}
		eff, src := resolveTurnEffort(context.Background(), a, "some question", false, "", "", testLogger())
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
		eff, src := resolveTurnEffort(context.Background(), a, "hard multi-file refactor", false, "", "", testLogger())
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
	eff, src := resolveTurnEffort(context.Background(), a, "thanks!", false, "", "", testLogger())
	if eff != "low" || src != "heuristic" {
		t.Fatalf("short: got (%q,%q), want (low,heuristic)", eff, src)
	}
	// Long message → static (tagged "static" so the caller never
	// mistakes the echoed old value for a classifier verdict).
	long := strings.Repeat("な", 300)
	eff, src = resolveTurnEffort(context.Background(), a, long, false, "", "", testLogger())
	if eff != "high" || src != "static" {
		t.Fatalf("long: got (%q,%q), want (high,static)", eff, src)
	}
	// Short but contains a code fence → static.
	eff, _ = resolveTurnEffort(context.Background(), a, "```go\npanic()\n```", false, "", "", testLogger())
	if eff != "high" {
		t.Fatalf("code fence: got %q, want high", eff)
	}
	// Short but contains a URL → static.
	eff, _ = resolveTurnEffort(context.Background(), a, "read https://example.com/doc", false, "", "", testLogger())
	if eff != "high" {
		t.Fatalf("url: got %q, want high", eff)
	}
}

func TestResolveTurnEffortJunkOutputHeuristic(t *testing.T) {
	requireClaudeInPath(t)
	withFakeClassifier(t, fakeClassifierReturning("I think this is a medium difficulty task"))
	a := &Agent{Tool: "claude", Model: "sonnet", Effort: "high"}
	eff, src := resolveTurnEffort(context.Background(), a, "hey", false, "", "", testLogger())
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
	eff, src := resolveTurnEffort(context.Background(), a, "hi", false, "", "", testLogger())
	if eff != "low" || !strings.HasPrefix(src, "llm:") {
		t.Fatalf("got (%q,%q), want (low,llm)", eff, src)
	}
	withFakeClassifier(t, fakeClassifierReturning("high"))
	eff, _ = resolveTurnEffort(context.Background(), a, "hard task", false, "", "", testLogger())
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

// withFakeJevClassifier swaps the classifyEffortJev seam for a test.
func withFakeJevClassifier(t *testing.T, fn func(ctx context.Context, apiKey, msg string) (string, error)) {
	t.Helper()
	orig := classifyEffortJev
	classifyEffortJev = fn
	t.Cleanup(func() { classifyEffortJev = orig })
}

// noClaudeInPath fakes an absent claude binary.
func noClaudeInPath(t *testing.T) {
	t.Helper()
	orig := classifierCLIAvailable
	classifierCLIAvailable = func() bool { return false }
	t.Cleanup(func() { classifierCLIAvailable = orig })
}

func TestResolveTurnEffortJevPreferredOverCLI(t *testing.T) {
	requireClaudeInPath(t)
	withFakeClassifier(t, func(context.Context, string) (string, error) {
		t.Fatal("claude classifier must not run when Jev answers")
		return "", nil
	})
	var gotKey, gotMsg string
	withFakeJevClassifier(t, func(_ context.Context, key, msg string) (string, error) {
		gotKey, gotMsg = key, msg
		return "low", nil
	})
	a := &Agent{Tool: "claude", Model: "sonnet", Effort: "high"}
	eff, src := resolveTurnEffort(context.Background(), a, "thanks", false, "diary tail", "ts-key", testLogger())
	if eff != "low" || src != "jev:low" {
		t.Fatalf("got (%q,%q), want (low,jev:low)", eff, src)
	}
	if gotKey != "ts-key" || gotMsg != "thanks" {
		t.Fatalf("jev inputs = (%q,%q)", gotKey, gotMsg)
	}
}

func TestResolveTurnEffortJevWorksWithoutClaudeCLI(t *testing.T) {
	noClaudeInPath(t)
	withFakeJevClassifier(t, func(context.Context, string, string) (string, error) {
		return "medium", nil
	})
	a := &Agent{Tool: "grok", Model: "grok-4", Effort: "high"}
	eff, src := resolveTurnEffort(context.Background(), a, "write a haiku about cats", false, "", "ts-key", testLogger())
	if eff != "medium" || src != "jev:medium" {
		t.Fatalf("got (%q,%q), want (medium,jev:medium)", eff, src)
	}
}

func TestResolveTurnEffortJevErrorFallsBackToCLI(t *testing.T) {
	requireClaudeInPath(t)
	withFakeJevClassifier(t, func(context.Context, string, string) (string, error) {
		return "", errors.New("typesafe: HTTP 503")
	})
	withFakeClassifier(t, fakeClassifierReturning("medium"))
	a := &Agent{Tool: "claude", Model: "sonnet", Effort: "high"}
	eff, src := resolveTurnEffort(context.Background(), a, "some question", false, "", "ts-key", testLogger())
	if eff != "medium" || src != "llm:medium" {
		t.Fatalf("got (%q,%q), want (medium,llm:medium)", eff, src)
	}
}

func TestResolveTurnEffortJevErrorNoCLIUsesHeuristic(t *testing.T) {
	noClaudeInPath(t)
	withFakeJevClassifier(t, func(context.Context, string, string) (string, error) {
		return "", context.DeadlineExceeded
	})
	a := &Agent{Tool: "claude", Model: "sonnet", Effort: "high"}
	eff, src := resolveTurnEffort(context.Background(), a, "ok", false, "", "ts-key", testLogger())
	if eff != "low" || src != "heuristic" {
		t.Fatalf("got (%q,%q), want (low,heuristic)", eff, src)
	}
	// Without Jev configured and without a CLI the historical "stay
	// static, don't guess" behavior is unchanged.
	eff, src = resolveTurnEffort(context.Background(), a, "ok", false, "", "", testLogger())
	if eff != "high" || src != "static" {
		t.Fatalf("no jev/no cli: got (%q,%q), want (high,static)", eff, src)
	}
}

func TestResolveTurnEffortJevTimeoutSkipsCLI(t *testing.T) {
	requireClaudeInPath(t)
	withFakeJevClassifier(t, func(context.Context, string, string) (string, error) {
		return "", context.DeadlineExceeded
	})
	withFakeClassifier(t, func(context.Context, string) (string, error) {
		t.Fatal("claude classifier must not stack on a Jev timeout")
		return "", nil
	})
	a := &Agent{Tool: "claude", Model: "sonnet", Effort: "high"}
	eff, src := resolveTurnEffort(context.Background(), a, "ok", false, "", "ts-key", testLogger())
	if eff != "low" || src != "heuristic" {
		t.Fatalf("got (%q,%q), want (low,heuristic)", eff, src)
	}
}

func TestResolveTurnEffortJevCancelledTurnStaysStatic(t *testing.T) {
	requireClaudeInPath(t)
	withFakeJevClassifier(t, func(ctx context.Context, _, _ string) (string, error) {
		return "", ctx.Err()
	})
	withFakeClassifier(t, func(context.Context, string) (string, error) {
		t.Fatal("claude classifier must not run on a cancelled turn")
		return "", nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := &Agent{Tool: "claude", Model: "sonnet", Effort: "high"}
	eff, src := resolveTurnEffort(ctx, a, "ok", false, "", "ts-key", testLogger())
	if eff != "high" || src != "static" {
		t.Fatalf("got (%q,%q), want (high,static)", eff, src)
	}
}

func TestResolveTurnEffortJevNotCalledWithoutKey(t *testing.T) {
	requireClaudeInPath(t)
	withFakeJevClassifier(t, func(context.Context, string, string) (string, error) {
		t.Fatal("jev must not be called without a key")
		return "", nil
	})
	withFakeClassifier(t, fakeClassifierReturning("low"))
	a := &Agent{Tool: "claude", Model: "sonnet", Effort: "high"}
	if eff, src := resolveTurnEffort(context.Background(), a, "hi", false, "", "", testLogger()); eff != "low" || src != "llm:low" {
		t.Fatalf("got (%q,%q), want (low,llm:low)", eff, src)
	}
}

func TestPickEffortTier(t *testing.T) {
	cases := []struct {
		name string
		ans  jevAnswer
		want string
		err  bool
	}{
		{"clear low", jevAnswer{Choice: "low", Probabilities: map[string]float64{"low": 0.9, "medium": 0.08, "high": 0.02}}, "low", false},
		{"clear high", jevAnswer{Choice: "high", Probabilities: map[string]float64{"low": 0.01, "medium": 0.1, "high": 0.89}}, "high", false},
		// medium 0.49 vs high 0.46: inside the margin → ties break upward.
		{"near tie breaks upward", jevAnswer{Choice: "medium", Probabilities: map[string]float64{"low": 0.05, "medium": 0.49, "high": 0.46}}, "high", false},
		{"outside margin keeps top", jevAnswer{Choice: "medium", Probabilities: map[string]float64{"low": 0.1, "medium": 0.6, "high": 0.3}}, "medium", false},
		{"low/medium tie → medium", jevAnswer{Choice: "low", Probabilities: map[string]float64{"low": 0.5, "medium": 0.45, "high": 0.05}}, "medium", false},
		{"no probabilities trusts choice", jevAnswer{Choice: " Medium "}, "medium", false},
		{"unknown keys only, no choice", jevAnswer{Choice: "", Probabilities: map[string]float64{"foo": 1}}, "", true},
		{"unknown keys only, trusts choice", jevAnswer{Choice: "low", Probabilities: map[string]float64{"foo": 1}}, "low", false},
		{"empty", jevAnswer{}, "", true},
	}
	for _, c := range cases {
		got, err := pickEffortTier(c.ans)
		if (err != nil) != c.err || got != c.want {
			t.Fatalf("%s: got (%q,%v), want (%q,err=%v)", c.name, got, err, c.want, c.err)
		}
	}
}

func TestRunJevEffortClassifierRequestShape(t *testing.T) {
	orig := jevCall
	t.Cleanup(func() { jevCall = orig })
	var gotState map[string]string
	var gotQ map[string]jevQuestion
	jevCall = func(ctx context.Context, apiKey string, state any, questions map[string]jevQuestion) (*jevResponse, error) {
		if apiKey != "k" {
			t.Fatalf("apiKey = %q", apiKey)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("jev call must carry a deadline")
		}
		gotState = state.(map[string]string)
		gotQ = questions
		return &jevResponse{Answers: map[string]jevAnswer{
			"effort": {Type: "choice", Choice: "high", Probabilities: map[string]float64{"high": 0.8, "medium": 0.15, "low": 0.05}},
		}}, nil
	}
	long := strings.Repeat("x", effortClassifierMessageCap+50)
	tier, err := runJevEffortClassifier(context.Background(), "k", long)
	if err != nil || tier != "high" {
		t.Fatalf("got (%q,%v)", tier, err)
	}
	if len([]rune(gotState["message"])) != effortClassifierMessageCap {
		t.Fatalf("message not capped: %d", len([]rune(gotState["message"])))
	}
	if _, ok := gotState["recent_context"]; ok {
		t.Fatal("historical diary context must not be sent to Jev")
	}
	q, ok := gotQ["effort"]
	if !ok || q.Type != "choice" {
		t.Fatalf("question = %+v", gotQ)
	}
	crit, _ := q.Criteria.(map[string]string)
	for _, tier := range []string{"low", "medium", "high"} {
		if crit[tier] == "" {
			t.Fatalf("criteria missing %s", tier)
		}
	}

	// Missing answer → error.
	jevCall = func(context.Context, string, any, map[string]jevQuestion) (*jevResponse, error) {
		return &jevResponse{Answers: map[string]jevAnswer{"other": {}}}, nil
	}
	if _, err := runJevEffortClassifier(context.Background(), "k", "hi"); err == nil {
		t.Fatal("expected error for missing effort answer")
	}
}

package agent

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"
)

// effortClassifierSystemPrompt drives the one-shot per-turn difficulty
// classifier. The model must answer with exactly one word.
const effortClassifierSystemPrompt = "You classify how much reasoning effort an AI assistant will need to answer a message. Output exactly one word: low, medium, or high. Nothing else.\n" +
	"low: greetings, small talk, acknowledgements, simple factual questions, short casual replies.\n" +
	"medium: multi-step questions, short writing/editing tasks, simple code snippets, planning a small task.\n" +
	"high: debugging, multi-file code work, math/logic problems, long-document analysis, ambiguous multi-constraint requests."

// Input caps for the classifier prompt. Runes, not bytes, so Japanese
// input isn't cut three times shorter than ASCII.
const (
	effortClassifierDiaryCap   = 500
	effortClassifierMessageCap = 1500
)

// heuristicShortMessageRunes is the fallback cutoff: when the classifier
// is unavailable, a short message with no code fence and no URL is
// treated as low effort.
const heuristicShortMessageRunes = 200

// classifyEffort is the LLM classification seam — swapped out by unit
// tests (mirrors the generateSummary var in autosummary.go). The returned
// string is expected to be exactly "low", "medium" or "high".
var classifyEffort = runClaudeEffortClassifier

// classifierCLIAvailable is a test seam around exec.LookPath so unit
// tests can exercise the LLM path without a claude binary in PATH.
var classifierCLIAvailable = func() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

// effortRank orders effort tiers for the ceiling comparison in
// resolveTurnEffort. The empty string (model default) is treated as the
// "high" tier — claude/grok models default to high (or better) effort.
var effortRank = map[string]int{
	"none": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6,
}

func rankEffort(effort string) int {
	if r, ok := effortRank[effort]; ok {
		return r
	}
	return effortRank["high"] // "" / unknown = model default ≈ high
}

// tailRunes returns the last n runes of s.
func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// headRunes returns the first n runes of s.
func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// buildEffortClassifierPrompt assembles the classifier's user prompt:
// an optional recent-diary tail for context, then the user message,
// clearly delimited so the classifier can't confuse the two.
func buildEffortClassifierPrompt(recentDiary, userMessage string) string {
	var sb strings.Builder
	if d := strings.TrimSpace(recentDiary); d != "" {
		sb.WriteString("<recent-context>\n")
		sb.WriteString(tailRunes(d, effortClassifierDiaryCap))
		sb.WriteString("\n</recent-context>\n\n")
	}
	sb.WriteString("<message>\n")
	sb.WriteString(headRunes(userMessage, effortClassifierMessageCap))
	sb.WriteString("\n</message>")
	return sb.String()
}

// heuristicTurnEffort is the no-LLM fallback used when the classifier
// times out, errors, or returns junk: a short message with no code fence
// and no URL is low effort; anything else keeps the static setting.
func heuristicTurnEffort(a *Agent, userMessage string) string {
	if len([]rune(userMessage)) < heuristicShortMessageRunes &&
		!strings.Contains(userMessage, "```") &&
		!strings.Contains(userMessage, "http://") &&
		!strings.Contains(userMessage, "https://") {
		if ValidModelEffort(a.Model, "low") && rankEffort("low") < rankEffort(a.Effort) {
			return "low"
		}
	}
	return a.Effort
}

// mapTierToEffort converts a classifier tier (low/medium/high) into the
// final per-turn effort:
//   - "low"/"medium" only ever override downward — an agent already
//     configured at or below the tier keeps its static value.
//   - "high" resolves to "high", except that agents whose static tier
//     sits ABOVE high (xhigh/max) keep their ceiling on hard tasks.
//
// The result is clamped through ValidModelEffort; invalid combos return
// the static value.
func mapTierToEffort(a *Agent, tier string) string {
	if tier == "high" {
		if rankEffort(a.Effort) > rankEffort("high") {
			return a.Effort
		}
	} else if rankEffort(tier) >= rankEffort(a.Effort) {
		return a.Effort
	}
	if !ValidModelEffort(a.Model, tier) {
		return a.Effort
	}
	return tier
}

// resolveTurnEffort picks the effort level for a single turn.
//
// Returns the effort string to launch the backend with, and a source tag
// for logging: "static" (feature off / unsupported tool / no CLI),
// "rule" (system turn → low, no LLM call), "llm" (classifier verdict),
// or "heuristic" (classifier failed; length-based fallback).
//
// ctx bounds the classifier call — derive it from the chat context so an
// aborted turn kills the classifier too. Never returns an error: any
// failure degrades to the agent's static Effort.
func resolveTurnEffort(ctx context.Context, a *Agent, userMessage string, systemTurn bool, recentDiary string, logger *slog.Logger) (effort string, source string) {
	if !a.IsAutoEffortEnabled() || (a.Tool != "claude" && a.Tool != "grok") {
		return a.Effort, "static"
	}
	// System turns (cron check-in, wake turn, group DM notification,
	// arrival prompt) are routine bookkeeping — pin them to low with
	// zero added latency.
	if systemTurn {
		if ValidModelEffort(a.Model, "low") && rankEffort("low") < rankEffort(a.Effort) {
			return "low", "rule"
		}
		return a.Effort, "static"
	}
	// The classifier always runs on the claude CLI (even for grok
	// agents); without it, stay static rather than guess.
	if !classifierCLIAvailable() {
		return a.Effort, "static"
	}
	out, err := classifyEffort(ctx, buildEffortClassifierPrompt(recentDiary, userMessage))
	if err != nil {
		logger.Warn("effort classifier failed; using heuristic",
			"agent", a.ID, "err", err)
		return heuristicOrStatic(a, userMessage)
	}
	tier := strings.ToLower(strings.TrimSpace(out))
	switch tier {
	case "low", "medium", "high":
	default:
		logger.Warn("effort classifier returned junk; using heuristic",
			"agent", a.ID, "output", headRunes(tier, 80))
		return heuristicOrStatic(a, userMessage)
	}
	// Encode the raw verdict in the source tag ("llm:<tier>") so callers
	// racing a concurrent settings PATCH can re-map the true tier against
	// the fresh agent copy instead of the value mapped against this
	// (possibly stale) snapshot.
	return mapTierToEffort(a, tier), "llm:" + tier
}

// heuristicOrStatic wraps heuristicTurnEffort with a source tag: the
// "heuristic" tag is used only when the heuristic actually downgraded;
// a keep-static outcome is tagged "static" so applyTurnEffort leaves the
// (possibly fresher) per-turn Effort untouched instead of treating the
// echoed old value as a classifier verdict.
func heuristicOrStatic(a *Agent, userMessage string) (string, string) {
	if eff := heuristicTurnEffort(a, userMessage); eff != a.Effort {
		return eff, "heuristic"
	}
	return a.Effort, "static"
}

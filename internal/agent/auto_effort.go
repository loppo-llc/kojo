package agent

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"strings"
	"time"
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

// Jev (TypeSafe System One) is the preferred classifier when a key is
// configured: one HTTP round trip (~hundreds of ms, ~400 input tokens)
// instead of spawning the claude CLI. The claude classifier stays as the
// fallback when Jev is unconfigured or fails.
const (
	// jevEffortTimeout bounds the Jev call. Jev answers in well under a
	// second; anything slower is an outage and the CLI/heuristic path
	// should take over without stalling the turn.
	jevEffortTimeout = 5 * time.Second
	// jevEffortTieMargin: when a higher-effort tier's probability is
	// within this margin of the top tier, prefer the higher effort. An
	// under-provisioned hard task costs quality; an over-provisioned easy
	// one only costs a few tokens, so ties break upward.
	jevEffortTieMargin = 0.15
)

// jevEffortCriteria are the tier definitions sent as Choice criteria —
// the same rubric as effortClassifierSystemPrompt, kept in English (Jev's
// primary training language) regardless of the message language.
var jevEffortCriteria = map[string]string{
	"low":    "Greetings, small talk, acknowledgements, simple factual questions, short casual replies, or a one-line instruction the assistant can act on directly",
	"medium": "Multi-step questions, short writing or editing tasks, simple code snippets, planning a small task",
	"high":   "Debugging, multi-file code work, math or logic problems, long-document analysis, ambiguous requests with several constraints, investigations whose scope is unknown up front",
}

// classifyEffortJev is the Jev classification seam, swapped out by unit
// tests. Returns the raw tier ("low"/"medium"/"high").
var classifyEffortJev = runJevEffortClassifier

// runJevEffortClassifier asks Jev one Choice question over the current
// message and reduces the returned distribution with pickEffortTier. Keep
// historical diary context local: configuring a global classifier key must
// not silently disclose an agent's activity log to a third party.
func runJevEffortClassifier(ctx context.Context, apiKey, userMessage string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, jevEffortTimeout)
	defer cancel()
	state := map[string]string{
		"message": headRunes(userMessage, effortClassifierMessageCap),
	}
	resp, err := jevCall(ctx, apiKey, state, map[string]jevQuestion{
		"effort": {
			Type:         "choice",
			Instructions: "How much reasoning effort will an AI coding/chat assistant need to answer `message`?",
			Criteria:     jevEffortCriteria,
		},
	})
	if err != nil {
		return "", err
	}
	ans, ok := resp.Answers["effort"]
	if !ok {
		return "", errors.New("typesafe: no effort answer")
	}
	return pickEffortTier(ans)
}

// pickEffortTier reduces a Choice answer to a tier. With a probability
// distribution it walks tiers from high to low and takes the first whose
// probability is within jevEffortTieMargin of the maximum (ties break
// toward more effort); without one it trusts the reported choice.
func pickEffortTier(ans jevAnswer) (string, error) {
	tiers := []string{"high", "medium", "low"}
	if len(ans.Probabilities) > 0 {
		best := -1.0
		for _, t := range tiers {
			if p := ans.Probabilities[t]; p > best {
				best = p
			}
		}
		// best <= 0 means only unknown keys were reported; fall
		// through and trust the choice instead.
		if best > 0 {
			for _, t := range tiers {
				if ans.Probabilities[t] >= best-jevEffortTieMargin {
					return t, nil
				}
			}
		}
	}
	switch c := strings.ToLower(strings.TrimSpace(ans.Choice)); c {
	case "low", "medium", "high":
		return c, nil
	default:
		return "", errors.New("typesafe: effort choice missing")
	}
}

// effortRank orders effort tiers for auto-effort mapping. The empty string
// (model default) is normalized to "high". Thus claude-opus-5-5's medium API
// default behaves like an explicit medium setting: a high classifier verdict
// promotes the turn to explicit high, while only xhigh/max remain above it.
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
		if ValidToolModelEffort(a.Tool, a.Model, "low") && rankEffort("low") < rankEffort(a.Effort) {
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
// The result is clamped through ValidToolModelEffort; invalid combos return
// the static value.
func mapTierToEffort(a *Agent, tier string) string {
	if tier == "high" {
		if rankEffort(a.Effort) > rankEffort("high") {
			return a.Effort
		}
	} else if rankEffort(tier) >= rankEffort(a.Effort) {
		return a.Effort
	}
	if !ValidToolModelEffort(a.Tool, a.Model, tier) {
		return a.Effort
	}
	return tier
}

// resolveTurnEffort picks the effort level for a single turn.
//
// Returns the effort string to launch the backend with, and a source tag
// for logging: "static" (feature off / unsupported tool / no classifier),
// "rule" (system turn → low, no LLM call), "jev:<tier>" (Jev verdict),
// "llm:<tier>" (claude CLI classifier verdict), or "heuristic"
// (classifiers failed; length-based fallback).
//
// jevKey is the TypeSafe API key ("" = Jev unconfigured → claude CLI
// path). ctx bounds the classifier calls — derive it from the chat
// context so an aborted turn kills the classifier too. Never returns an
// error: any failure degrades to the agent's static Effort.
func resolveTurnEffort(ctx context.Context, a *Agent, userMessage string, systemTurn bool, recentDiary string, jevKey string, logger *slog.Logger) (effort string, source string) {
	if !a.IsAutoEffortEnabled() || (a.Tool != "claude" && a.Tool != "grok") {
		return a.Effort, "static"
	}
	// System turns (cron check-in, wake turn, group DM notification,
	// arrival prompt) are routine bookkeeping — pin them to low with
	// zero added latency.
	if systemTurn {
		if ValidToolModelEffort(a.Tool, a.Model, "low") && rankEffort("low") < rankEffort(a.Effort) {
			return "low", "rule"
		}
		return a.Effort, "static"
	}
	// Jev first: one cheap HTTP call, no subprocess. Any failure falls
	// through to the claude CLI classifier (then the heuristic) so a
	// TypeSafe outage never changes behavior beyond added latency.
	if jevKey != "" {
		tier, err := classifyEffortJev(ctx, jevKey, userMessage)
		if err == nil {
			return mapTierToEffort(a, tier), "jev:" + tier
		}
		if ctx.Err() != nil {
			// The turn itself is gone; nothing downstream can run.
			return a.Effort, "static"
		}
		if errors.Is(err, context.DeadlineExceeded) {
			// Our own 5s budget expired. Don't stack the CLI's 15s on
			// top of it: go straight to the heuristic.
			logger.Info("jev effort classifier timed out; using heuristic",
				"agent", a.ID, "err", err)
			return heuristicOrStatic(a, userMessage)
		}
		logger.Warn("jev effort classifier failed; falling back",
			"agent", a.ID, "err", err)
	}
	// The CLI classifier always runs on the claude CLI (even for grok
	// agents); without it, stay static rather than guess.
	if !classifierCLIAvailable() {
		return heuristicOrStaticIfJev(a, userMessage, jevKey)
	}
	out, err := classifyEffort(ctx, buildEffortClassifierPrompt(recentDiary, userMessage))
	if err != nil {
		// Our own deadline killing the CLI is expected degradation
		// (the heuristic covers it), not an operational fault — log
		// it at Info. runCLIGenerateTimeout wraps the deadline kill
		// in context.DeadlineExceeded so it's distinguishable from a
		// genuine CLI failure, which stays at Warn.
		if errors.Is(err, context.DeadlineExceeded) {
			logger.Info("effort classifier timed out; using heuristic",
				"agent", a.ID, "err", err)
		} else {
			logger.Warn("effort classifier failed; using heuristic",
				"agent", a.ID, "err", err)
		}
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

// heuristicOrStaticIfJev is the no-CLI branch: with no classifier at all
// the historical behavior (stay static, don't guess) is kept; when Jev
// was configured but failed, the length heuristic is a better degrade
// than silently running every turn at the static ceiling.
func heuristicOrStaticIfJev(a *Agent, userMessage, jevKey string) (string, string) {
	if jevKey == "" {
		return a.Effort, "static"
	}
	return heuristicOrStatic(a, userMessage)
}

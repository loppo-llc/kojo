// Per-model Anthropic API pricing, in USD per million tokens.
//
// Source: the claude-api skill's authoritative pricing table (cached
// 2026-06-24) plus the standard cache multipliers documented in
// shared/prompt-caching.md:
//   - cache read      = 0.1x  the base input rate
//   - cache write 5m  = 1.25x the base input rate
//
// Base input/output rates (USD / 1M tokens):
//   claude-fable-5   : 10 / 50
//   claude-opus-4-8  :  5 / 25
//   claude-opus-4-7  :  5 / 25
//   claude-opus-4-6  :  5 / 25
//   claude-sonnet-5  :  3 / 15   (standard rate; an intro $2/$10 runs
//                                 through 2026-08-31 — we bill the standard
//                                 rate so the figure is stable past that date)
//   claude-sonnet-4-6:  3 / 15
//   claude-haiku-4-5 :  1 /  5
//
// Models not in the table (fable-5 bare alias, grok-*, gpt-*, custom,
// llama.cpp, "") return undefined from priceModel → no cost is shown.

export interface ModelPricing {
  /** USD per 1M input (uncached) tokens. */
  input: number;
  /** USD per 1M output tokens. */
  output: number;
  /** USD per 1M cache-read input tokens (0.1x input). */
  cacheRead: number;
  /** USD per 1M cache-write input tokens, 5-minute TTL (1.25x input). */
  cacheWrite: number;
}

function priced(input: number, output: number): ModelPricing {
  return {
    input,
    output,
    cacheRead: input * 0.1,
    cacheWrite: input * 1.25,
  };
}

// Keyed by the canonical (full) claude model id.
const CANONICAL_PRICING: Record<string, ModelPricing> = {
  "claude-fable-5": priced(10, 50),
  "claude-opus-4-8": priced(5, 25),
  "claude-opus-4-7": priced(5, 25),
  "claude-opus-4-6": priced(5, 25),
  "claude-sonnet-5": priced(3, 15),
  "claude-sonnet-4-6": priced(3, 15),
  "claude-haiku-4-5": priced(1, 5),
};

// kojo agent.model aliases (see web/src/lib/toolModels.ts) mapped to a
// canonical id. "opus" → Opus 4.8, "sonnet" → Sonnet 5, "haiku" → Haiku 4.5.
const ALIASES: Record<string, string> = {
  opus: "claude-opus-4-8",
  sonnet: "claude-sonnet-5",
  haiku: "claude-haiku-4-5",
};

/**
 * Resolve a kojo agent.model value to its pricing, or undefined when the
 * model is unpriced (grok, gpt/codex, custom, llama.cpp, unknown aliases).
 */
export function priceModel(model: string | undefined): ModelPricing | undefined {
  if (!model) return undefined;
  const canonical = ALIASES[model] ?? model;
  return CANONICAL_PRICING[canonical];
}

export interface TurnUsage {
  inputTokens: number;
  outputTokens: number;
  cacheReadInputTokens?: number;
  cacheCreationInputTokens?: number;
  /** Backend-reported exact cost (Claude CLI total_cost_usd), covering
   *  subagent usage and per-model rates. Preferred over the estimate. */
  costUSD?: number;
}

/**
 * Approximate USD cost of a single turn's token usage for the given model.
 * Returns undefined when the model has no known pricing so the caller can
 * suppress the cost display entirely.
 *
 * Anthropic bills cache-read and cache-creation tokens separately from plain
 * input tokens; `inputTokens` here is the uncached remainder (matches the
 * server's Usage struct / the API's usage.input_tokens semantics), so the
 * three input buckets are summed at their own rates and not double-counted.
 */
export function estimateTurnCost(
  model: string | undefined,
  usage: TurnUsage | undefined,
): number | undefined {
  if (!usage) return undefined;
  // A backend-reported cost is exact (includes subagent usage billed at
  // each model's own rate) — always prefer it over the estimate.
  if (usage.costUSD && usage.costUSD > 0) return usage.costUSD;
  const p = priceModel(model);
  if (!p) return undefined;
  const input = usage.inputTokens || 0;
  const output = usage.outputTokens || 0;
  const cacheRead = usage.cacheReadInputTokens || 0;
  const cacheWrite = usage.cacheCreationInputTokens || 0;
  return (
    (input * p.input +
      output * p.output +
      cacheRead * p.cacheRead +
      cacheWrite * p.cacheWrite) /
    1_000_000
  );
}

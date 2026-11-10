// Per-model API pricing, in USD per million tokens.
//
// Anthropic source: the claude-api skill's authoritative pricing table (cached
// 2026-06-24) plus the standard cache multipliers documented in
// shared/prompt-caching.md:
//   - cache read      = 0.1x  the base input rate
//   - cache write 5m  = 1.25x the base input rate
//
// Anthropic base input/output rates (USD / 1M tokens):
//   claude-fable-5-2 :  8 / 40   (cache read 0.025x, not 0.1x — see below)
//   claude-fable-5-1 : 10 / 50   (cache read 0.025x, not 0.1x — see below)
//   claude-fable-5   : 10 / 50
//   claude-opus-5-5  :  4 / 20   (cache read 0.05x, not 0.1x — see below)
//   claude-opus-5    :  5 / 25
//   claude-opus-4-8  :  5 / 25
//   claude-opus-4-7  :  5 / 25
//   claude-opus-4-6  :  5 / 25
//   claude-sonnet-5-5:  2 / 10
//   claude-sonnet-5  :  2 / 10   (launched as a $2/$10 intro price with a
//                                 $3/$15 step-up on 2026-09-01; the pricing
//                                 page (fetched 2026-09-29, footnote 3) says
//                                 the increase was cancelled and $2/$10 is
//                                 now the standard rate)
//   claude-sonnet-4-6:  3 / 15
//   claude-haiku-4-5 :  1 /  5
//
// xAI source: https://docs.x.ai/developers/pricing plus the per-model pages
// https://docs.x.ai/developers/models/grok-4.7, .../grok-4.6 and .../grok-4.5
// (grok-4.7 fetched 2026-09-22, the rest 2026-08-13).
// xAI publishes an explicit cached-input rate and has no cache-write surcharge
// (cacheCreation tokens bill at the plain input rate when reported).
//   grok-4.7: input $2.00 / cached input $0.50 / output $6.00 per 1M
//   grok-4.6: input $2.00 / cached input $0.50 / output $6.00 per 1M
//   grok-4.5: input $2.00 / cached input $0.30 / output $6.00 per 1M
//   grok-4.7-build-fast: input $4.00 / cached input $1.00 / output $12.00 per 1M
// grok-4.7-build-fast has no per-model page (it is not on the public xAI API;
// Grok Build and Cursor only) but the pricing page's "Grok 4.7 Fast pricing"
// section (fetched 2026-09-22) lists it at twice the standard grok-4.7 rates.
// The standard models double every rate for requests whose prompt reaches
// 200k tokens; build-fast's long-context tier is 1.5x ($6.00 / $1.50 / $18.00).
// The whole request bills at the long-context tier. kojo prices the short-
// context tier only, so estimates understate very large turns by up to 2x.
//
// claude-fable-5-2, claude-fable-5-1 and claude-opus-5-5 are the ids in this
// table that depart from the 0.1x cache-read rule:
//   - https://platform.claude.com/docs/en/models/fable-5-2/overview (fetched
//     2026-11-10) lists Fable 5.2 at $8 input / $40 output, 5m cache write
//     $10 (the usual 1.25x) and cache read $0.20, i.e. still 0.025x.
//   - https://platform.claude.com/docs/en/about-claude/pricing (fetched
//     2026-09-02) prices Fable 5.1's cache hits and refreshes at 0.025x the
//     base input rate, i.e. $0.25/1M against Fable 5's $1/1M. Everything else
//     about the two Fable models — input, output, both cache-write TTLs — is
//     identical.
//   - https://platform.claude.com/docs/en/models/opus-5-5/overview (fetched
//     2026-09-23) lists Opus 5.5 at $4 input / $20 output, 5m cache write
//     $5 (the usual 1.25x) but cache read $0.20, i.e. 0.05x.
// (Mythos 5.1 / 5.2 carry the same exception, but kojo does not offer them: the
// models are gated behind Anthropic's trusted access, so it never reaches
// toolModels.ts and has no row here.)
//
// Models not in the table (fable-5 bare id, gpt-*, custom-* endpoints, "")
// return undefined from priceModel → no cost is shown.

export interface ModelPricing {
  /** USD per 1M input (uncached) tokens. */
  input: number;
  /** USD per 1M output tokens. */
  output: number;
  /** USD per 1M cache-read input tokens. */
  cacheRead: number;
  /** USD per 1M cache-write / cache-creation input tokens. */
  cacheWrite: number;
}

/**
 * Anthropic: cache read 0.1x, cache write 5m 1.25x base input.
 *
 * The cache-read multiplier is an option rather than a constant because Fable
 * 5.1 / 5.2 read cache at 0.025x and Opus 5.5 at 0.05x. It is named at the call site on purpose: the
 * third positional argument of pricedXai below is a dollar rate, and two
 * neighbouring helpers whose third argument means different things is exactly
 * the mix-up that would misprice a model by 10x.
 */
function pricedAnthropic(
  input: number,
  output: number,
  opts: { cacheReadRate?: number } = {},
): ModelPricing {
  return {
    input,
    output,
    cacheRead: input * (opts.cacheReadRate ?? 0.1),
    cacheWrite: input * 1.25,
  };
}

/**
 * xAI: explicit cached-input rate from docs; no cache-write surcharge so
 * cacheCreation tokens (if reported) bill at the plain input rate.
 */
function pricedXai(
  input: number,
  output: number,
  cacheRead: number,
): ModelPricing {
  return {
    input,
    output,
    cacheRead,
    cacheWrite: input,
  };
}

// Keyed by the canonical (full) model id.
const CANONICAL_PRICING: Record<string, ModelPricing> = {
  "claude-fable-5-2": pricedAnthropic(8, 40, { cacheReadRate: 0.025 }),
  "claude-fable-5-1": pricedAnthropic(10, 50, { cacheReadRate: 0.025 }),
  "claude-fable-5": pricedAnthropic(10, 50),
  "claude-opus-5-5": pricedAnthropic(4, 20, { cacheReadRate: 0.05 }),
  "claude-opus-5": pricedAnthropic(5, 25),
  "claude-opus-4-8": pricedAnthropic(5, 25),
  "claude-opus-4-7": pricedAnthropic(5, 25),
  "claude-opus-4-6": pricedAnthropic(5, 25),
  // https://platform.claude.com/docs/en/models/sonnet-5-5/overview (fetched
  // 2026-09-29): $2 / $10, 5m cache write $2.50, cache read $0.20 (0.1x).
  "claude-sonnet-5-5": pricedAnthropic(2, 10),
  "claude-sonnet-5": pricedAnthropic(2, 10),
  "claude-sonnet-4-6": pricedAnthropic(3, 15),
  "claude-haiku-4-5": pricedAnthropic(1, 5),
  // xAI — https://docs.x.ai/developers/pricing (grok-4.7 2026-09-22, rest 2026-08-13)
  "grok-4.7": pricedXai(2, 6, 0.5),
  "grok-4.7-build-fast": pricedXai(4, 12, 1),
  "grok-4.6": pricedXai(2, 6, 0.5),
  "grok-4.5": pricedXai(2, 6, 0.3),
};

// kojo agent.model aliases (see web/src/lib/toolModels.ts) mapped to a
// canonical id. "opus" → Opus 5.5, "sonnet" → Sonnet 5, "haiku" → Haiku 4.5,
// "fable" → Fable 5.2.
// The "opus" and "fable" aliases follow what the installed claude CLI
// resolves them to: 2.1.276 mapped "opus" to claude-opus-5; 2.1.321's model
// catalog maps it to claude-opus-5-5 and adds "fable" → claude-fable-5-2
// (Anthropic API). "sonnet" resolves to claude-sonnet-5 up to CLI 2.1.282 and to
// claude-sonnet-5-5 from 2.1.284 (Anthropic API); both bill $2/$10, so the
// estimate is right on either side of that update.
const ALIASES: Record<string, string> = {
  opus: "claude-opus-5-5",
  fable: "claude-fable-5-2",
  sonnet: "claude-sonnet-5",
  haiku: "claude-haiku-4-5",
};

/**
 * Resolve a kojo agent.model value to its pricing, or undefined when the
 * model is unpriced (gpt/codex, custom-* endpoints, unknown aliases).
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
 * Cache-read and cache-creation tokens are billed separately from plain
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

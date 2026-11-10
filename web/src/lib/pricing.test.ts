import { describe, it, expect } from "vitest";
import { priceModel, estimateTurnCost } from "./pricing";

describe("priceModel", () => {
  it("resolves aliases to canonical pricing", () => {
    expect(priceModel("opus")).toEqual(priceModel("claude-opus-5-5"));
    expect(priceModel("fable")).toEqual(priceModel("claude-fable-5-2"));
    expect(priceModel("sonnet")).toEqual(priceModel("claude-sonnet-5"));
    expect(priceModel("haiku")).toEqual(priceModel("claude-haiku-5-5"));
  });

  it("prices canonical claude ids", () => {
    const haiku = priceModel("claude-haiku-5-5")!;
    expect(haiku.input).toBe(0.1);
    expect(haiku.output).toBe(0.5);
    expect(haiku.cacheRead).toBeCloseTo(0.01, 10);
    expect(haiku.cacheWrite).toBeCloseTo(0.125, 10);
    expect(priceModel("claude-opus-5")).toEqual({
      input: 5,
      output: 25,
      cacheRead: 0.5,
      cacheWrite: 6.25,
    });
    expect(priceModel("claude-opus-4-8")).toEqual({
      input: 5,
      output: 25,
      cacheRead: 0.5,
      cacheWrite: 6.25,
    });
    expect(priceModel("claude-fable-5")).toEqual({
      input: 10,
      output: 50,
      cacheRead: 1,
      cacheWrite: 12.5,
    });
    expect(priceModel("claude-sonnet-4-6")?.output).toBe(15);
  });

  it("prices Sonnet 5.5 and Sonnet 5 at $2/$10 with a 0.1x cache read", () => {
    // https://platform.claude.com/docs/en/about-claude/pricing (fetched
    // 2026-09-29): both rows list $2 input, $2.50 5m write, $0.20 read,
    // $10 output; Sonnet 5's $3/$15 step-up was cancelled.
    for (const m of ["claude-sonnet-5-5", "claude-sonnet-5", "sonnet"]) {
      expect(priceModel(m)).toEqual({
        input: 2,
        output: 10,
        cacheRead: 0.2,
        cacheWrite: 2.5,
      });
    }
  });

  it("prices Opus 5.5 at $4/$20 with a 0.05x cache read", () => {
    // https://platform.claude.com/docs/en/models/opus-5-5/overview: input $4,
    // output $20, 5m cache write $5, cache read $0.20.
    expect(priceModel("claude-opus-5-5")).toEqual({
      input: 4,
      output: 20,
      cacheRead: 0.2,
      cacheWrite: 5,
    });
    // claude CLI 2.1.321 resolves the opus alias to Opus 5.5.
    expect(priceModel("opus")).toEqual(priceModel("claude-opus-5-5"));
  });

  it("prices Fable 5.2 at $8/$40 with a 0.025x cache read", () => {
    // https://platform.claude.com/docs/en/models/fable-5-2/overview: input $8,
    // output $40, 5m cache write $10, cache read $0.20.
    for (const m of ["claude-fable-5-2", "fable"]) {
      expect(priceModel(m)).toEqual({
        input: 8,
        output: 40,
        cacheRead: 0.2,
        cacheWrite: 10,
      });
    }
  });

  it("reads cache at 0.025x for Fable 5.1, 0.1x for Fable 5", () => {
    // Same base rates; only the cache-read multiplier differs.
    expect(priceModel("claude-fable-5-1")).toEqual({
      input: 10,
      output: 50,
      cacheRead: 0.25,
      cacheWrite: 12.5,
    });
  });

  it("prices xAI grok models with their explicit cache-read rates", () => {
    expect(priceModel("grok-4.7")).toEqual({
      input: 2,
      output: 6,
      cacheRead: 0.5,
      cacheWrite: 2,
    });
    expect(priceModel("grok-4.7-build-fast")).toEqual({
      input: 4,
      output: 12,
      cacheRead: 1,
      cacheWrite: 4,
    });
    expect(priceModel("grok-4.6")).toEqual({
      input: 2,
      output: 6,
      cacheRead: 0.5,
      cacheWrite: 2, // no cache-write surcharge — plain input rate
    });
    expect(priceModel("grok-4.5")).toEqual({
      input: 2,
      output: 6,
      cacheRead: 0.3,
      cacheWrite: 2,
    });
  });

  it("returns undefined for unpriced / unknown models", () => {
    expect(priceModel("grok-composer-2.5-fast")).toBeUndefined();
    expect(priceModel("gpt-5.5")).toBeUndefined();
    expect(priceModel("fable-5")).toBeUndefined();
    expect(priceModel("")).toBeUndefined();
    expect(priceModel(undefined)).toBeUndefined();
  });
});

describe("estimateTurnCost", () => {
  it("sums input, output, and cache buckets at their own rates", () => {
    // Opus 5: input 5, output 25, cacheRead 0.5, cacheWrite 6.25 per 1M.
    // 1M in, 1M out, 1M cacheRead, 1M cacheWrite = 5 + 25 + 0.5 + 6.25.
    const cost = estimateTurnCost("claude-opus-5", {
      inputTokens: 1_000_000,
      outputTokens: 1_000_000,
      cacheReadInputTokens: 1_000_000,
      cacheCreationInputTokens: 1_000_000,
    });
    expect(cost).toBeCloseTo(36.75, 6);
  });

  it("handles missing cache fields as zero", () => {
    // Sonnet 5: input 2, output 10 per 1M. 100k in, 200k out.
    const cost = estimateTurnCost("claude-sonnet-5", {
      inputTokens: 100_000,
      outputTokens: 200_000,
    });
    expect(cost).toBeCloseTo(0.2 + 2.0, 6);
  });

  it("estimates grok-4.6 with xAI cache rates", () => {
    // grok-4.6: input 2, output 6, cacheRead 0.5, cacheWrite 2 per 1M.
    // 1M in, 1M out, 1M cacheRead, 1M cacheWrite = 2 + 6 + 0.5 + 2 = 10.5
    const cost = estimateTurnCost("grok-4.6", {
      inputTokens: 1_000_000,
      outputTokens: 1_000_000,
      cacheReadInputTokens: 1_000_000,
      cacheCreationInputTokens: 1_000_000,
    });
    expect(cost).toBeCloseTo(10.5, 6);
  });

  it("returns undefined for unpriced models", () => {
    expect(
      estimateTurnCost("grok-composer-2.5-fast", {
        inputTokens: 100,
        outputTokens: 100,
      }),
    ).toBeUndefined();
  });

  it("returns undefined when usage is absent", () => {
    expect(estimateTurnCost("claude-opus-4-8", undefined)).toBeUndefined();
  });
});

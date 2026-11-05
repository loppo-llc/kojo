import { describe, expect, it } from "vitest";
import {
  defaultEffortForModel,
  defaultModelForTool,
  effortLevelsForModel,
  modelsForTool,
  sessionEffortLevelsForModel,
} from "./toolModels";

describe("toolModels — Opus 5 / effort defaults", () => {
  it("lists claude-opus-5 as a claude model", () => {
    expect(modelsForTool("claude")).toContain("claude-opus-5");
  });

  it("lists claude-opus-4-8 as a claude model", () => {
    expect(modelsForTool("claude")).toContain("claude-opus-4-8");
  });

  it("lists both Fable models, newest first, as claude models", () => {
    const models = modelsForTool("claude");
    expect(models).toContain("claude-fable-5-1");
    expect(models).toContain("claude-fable-5");
    // The list is the dropdown order, and every other family in it runs
    // newest to oldest. Pin the adjacency so a later Fable lands above 5.1
    // rather than at the bottom of the list.
    expect(models.indexOf("claude-fable-5-1")).toBe(
      models.indexOf("claude-fable-5") - 1,
    );
  });

  it("Fable 5 and 5.1 support xhigh and max but default to high", () => {
    for (const m of ["claude-fable-5-1", "claude-fable-5"]) {
      expect(effortLevelsForModel(m)).toContain("xhigh");
      expect(effortLevelsForModel(m)).toContain("max");
      expect(defaultEffortForModel(m)).toBe("high");
    }
  });

  it("lists claude-opus-5-5 directly below the opus alias", () => {
    const models = modelsForTool("claude");
    expect(models).toContain("claude-opus-5-5");
    expect(models.indexOf("claude-opus-5-5")).toBe(models.indexOf("opus") + 1);
    expect(models.indexOf("claude-opus-5-5")).toBe(models.indexOf("claude-opus-5") - 1);
  });

  it("Opus 5.5 supports xhigh and max but defaults to medium", () => {
    // https://platform.claude.com/docs/en/build-with-claude/effort: Opus 5.5
    // supports all five levels and is the only Claude model defaulting to
    // medium.
    expect(effortLevelsForModel("claude-opus-5-5")).toEqual(["low", "medium", "high", "xhigh", "max"]);
    expect(defaultEffortForModel("claude-opus-5-5")).toBe("medium");
  });

  it("Opus 5 supports xhigh and max but defaults to high", () => {
    expect(effortLevelsForModel("claude-opus-5")).toContain("xhigh");
    expect(effortLevelsForModel("claude-opus-5")).toContain("max");
    expect(defaultEffortForModel("claude-opus-5")).toBe("high");
  });

  it("Opus 4.8 supports xhigh but defaults to high", () => {
    expect(effortLevelsForModel("claude-opus-4-8")).toContain("xhigh");
    expect(defaultEffortForModel("claude-opus-4-8")).toBe("high");
  });

  it("Opus 4.7 defaults to xhigh", () => {
    expect(effortLevelsForModel("claude-opus-4-7")).toContain("xhigh");
    expect(defaultEffortForModel("claude-opus-4-7")).toBe("xhigh");
  });

  it("Opus 4.6 has no xhigh and defaults to high", () => {
    expect(effortLevelsForModel("claude-opus-4-6")).not.toContain("xhigh");
    expect(defaultEffortForModel("claude-opus-4-6")).toBe("high");
  });

  it("opus alias is treated as Opus 5: supports xhigh, defaults to high", () => {
    expect(effortLevelsForModel("opus")).toContain("xhigh");
    expect(defaultEffortForModel("opus")).toBe("high");
  });

  it("sonnet supports xhigh and defaults to high", () => {
    // The alias resolves to Sonnet 5 / Sonnet 5.5, both xhigh-capable.
    expect(effortLevelsForModel("sonnet")).toContain("xhigh");
    expect(defaultEffortForModel("sonnet")).toBe("high");
  });

  it("lists claude-sonnet-5-5 directly below the sonnet alias", () => {
    const models = modelsForTool("claude");
    expect(models.indexOf("claude-sonnet-5-5")).toBe(models.indexOf("sonnet") + 1);
    expect(models.indexOf("claude-sonnet-5-5")).toBe(models.indexOf("claude-sonnet-5") - 1);
  });

  it("Sonnet 5.5 supports all five levels and defaults to high", () => {
    // https://platform.claude.com/docs/en/models/sonnet-5-5/overview
    // (fetched 2026-09-29): default effort high; the effort doc lists
    // xhigh and max for Sonnet 5.5.
    expect(effortLevelsForModel("claude-sonnet-5-5")).toEqual(["low", "medium", "high", "xhigh", "max"]);
    expect(defaultEffortForModel("claude-sonnet-5-5")).toBe("high");
  });

  it("lists claude-sonnet-5 as a claude model", () => {
    expect(modelsForTool("claude")).toContain("claude-sonnet-5");
  });

  it("Sonnet 5 supports xhigh and max but defaults to high", () => {
    expect(effortLevelsForModel("claude-sonnet-5")).toContain("xhigh");
    expect(effortLevelsForModel("claude-sonnet-5")).toContain("max");
    expect(defaultEffortForModel("claude-sonnet-5")).toBe("high");
  });

  it("lists claude-sonnet-4-6 as a claude model", () => {
    expect(modelsForTool("claude")).toContain("claude-sonnet-4-6");
  });

  it("Sonnet 4.6 has no xhigh, supports max, and defaults to high", () => {
    expect(effortLevelsForModel("claude-sonnet-4-6")).not.toContain("xhigh");
    expect(effortLevelsForModel("claude-sonnet-4-6")).toContain("max");
    expect(defaultEffortForModel("claude-sonnet-4-6")).toBe("high");
  });

  it("codex models before gpt-5.6 support xhigh and omit max; gpt-5.5 defaults to xhigh", () => {
    // codex CLI 0.159.2 models_cache.json: gpt-5.5 default_reasoning_level
    // is xhigh (gone from the 0.164.0 cache; kept as-is); gpt-5.4 and older
    // are no longer in the cache.
    expect(effortLevelsForModel("gpt-5.5")).toContain("xhigh");
    expect(effortLevelsForModel("gpt-5.5")).not.toContain("max");
    expect(defaultEffortForModel("gpt-5.5")).toBe("xhigh");
    expect(defaultEffortForModel("gpt-5.4")).toBe("medium");
  });

  it("lists exactly the public codex models, newest first, and defaults to gpt-6-astra", () => {
    // codex CLI 0.164.0 models_cache.json, visibility "list" only —
    // gpt-6-chat-thinking, gpt-reserve and codex-auto-review are hidden and
    // stay out. Order follows the cache's priority: gpt-6.1-sol, gpt-6-astra,
    // gpt-6.1-terra, then gpt-6-sol and gpt-6-luna. The trailing gpt-5.5 and
    // older are no longer in the cache and stay listed for existing agents.
    expect(modelsForTool("codex")).toEqual([
      "gpt-6.1-sol",
      "gpt-6-astra",
      "gpt-6.1-terra",
      "gpt-6-sol",
      "gpt-6-luna",
      "gpt-5.6-sol",
      "gpt-5.6-terra",
      "gpt-5.6-luna",
      "gpt-5.5",
      "gpt-5.4",
      "gpt-5.4-mini",
      "gpt-5.3-codex",
      "gpt-5.2",
    ]);
    expect(defaultModelForTool("codex")).toBe("gpt-6-astra");
  });

  it("the gpt-6.1 and gpt-6 families support xhigh and max; gpt-6.1 defaults to low, gpt-6 to medium", () => {
    // codex CLI 0.164.0 models_cache.json default_reasoning_level.
    for (const m of ["gpt-6.1-sol", "gpt-6.1-terra", "gpt-6-sol", "gpt-6-astra", "gpt-6-luna"]) {
      expect(effortLevelsForModel(m)).toEqual(["low", "medium", "high", "xhigh", "max"]);
    }
    expect(defaultEffortForModel("gpt-6.1-sol")).toBe("low");
    expect(defaultEffortForModel("gpt-6.1-terra")).toBe("low");
    expect(defaultEffortForModel("gpt-6-sol")).toBe("medium");
    expect(defaultEffortForModel("gpt-6-astra")).toBe("medium");
    expect(defaultEffortForModel("gpt-6-luna")).toBe("medium");
  });

  it("offers only the safe effort ladder for the CLI-default model", () => {
    expect(effortLevelsForModel("")).toEqual(["low", "medium", "high"]);
  });

  it("still lists the gpt-5.6 family", () => {
    expect(modelsForTool("codex")).toContain("gpt-5.6-sol");
    expect(modelsForTool("codex")).toContain("gpt-5.6-terra");
    expect(modelsForTool("codex")).toContain("gpt-5.6-luna");
  });

  it("gpt-5.6 family supports xhigh and max", () => {
    for (const m of ["gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"]) {
      expect(effortLevelsForModel(m)).toContain("xhigh");
      expect(effortLevelsForModel(m)).toContain("max");
    }
  });

  it("gpt-5.6-sol defaults to low, terra and luna to medium (codex CLI 0.164.0)", () => {
    expect(defaultEffortForModel("gpt-5.6-sol")).toBe("low");
    expect(defaultEffortForModel("gpt-5.6-terra")).toBe("medium");
    expect(defaultEffortForModel("gpt-5.6-luna")).toBe("medium");
  });

  it("lists both grok models, newest first", () => {
    expect(modelsForTool("grok")).toEqual(["grok-4.7", "grok-4.7-build-fast", "grok-4.6", "grok-4.5"]);
    expect(defaultModelForTool("grok")).toBe("grok-4.7");
  });

  it("grok-4.7 / 4.7-build-fast / 4.6 offer xhigh but not max, and default to high", () => {
    for (const m of ["grok-4.7", "grok-4.7-build-fast", "grok-4.6"]) {
      expect(effortLevelsForModel(m)).toEqual(["low", "medium", "high", "xhigh"]);
      expect(defaultEffortForModel(m)).toBe("high");
    }
  });

  it("grok-4.5 offers only low/medium/high and defaults to high", () => {
    expect(effortLevelsForModel("grok-4.5")).not.toContain("xhigh");
    expect(effortLevelsForModel("grok-4.5")).not.toContain("max");
    expect(defaultEffortForModel("grok-4.5")).toBe("high");
  });

  it("gemini lists argon (default) and flash with low/medium/high, default medium", () => {
    expect(modelsForTool("gemini-api")).toEqual(["gemini-4-argon", "gemini-3.8-flash"]);
    expect(defaultModelForTool("gemini-api")).toBe("gemini-4-argon");
    for (const m of ["gemini-4-argon", "gemini-3.8-flash"]) {
      expect(effortLevelsForModel(m)).toEqual(["low", "medium", "high"]);
      expect(defaultEffortForModel(m)).toBe("medium");
    }
    expect(effortLevelsForModel("flash")).toEqual(["low", "medium", "high"]);
  });
});

describe("sessionEffortLevelsForModel — ultra is session-only", () => {
  it("adds ultra for gpt-6.1-sol, gpt-6.1-terra, gpt-6-sol, gpt-6-astra, gpt-5.6-sol and gpt-5.6-terra", () => {
    expect(sessionEffortLevelsForModel("gpt-6.1-sol").slice(-2)).toEqual(["max", "ultra"]);
    expect(sessionEffortLevelsForModel("gpt-6.1-terra").slice(-2)).toEqual(["max", "ultra"]);
    expect(sessionEffortLevelsForModel("gpt-6-sol")).toContain("ultra");
    expect(sessionEffortLevelsForModel("gpt-6-sol").slice(-2)).toEqual(["max", "ultra"]);
    expect(sessionEffortLevelsForModel("gpt-6-astra")).toContain("ultra");
    expect(sessionEffortLevelsForModel("gpt-6-astra").slice(-2)).toEqual(["max", "ultra"]);
    expect(sessionEffortLevelsForModel("gpt-5.6-sol")).toContain("ultra");
    expect(sessionEffortLevelsForModel("gpt-5.6-terra")).toContain("ultra");
  });

  it("does not add ultra for gpt-6-luna, gpt-5.6-luna or other models", () => {
    expect(sessionEffortLevelsForModel("gpt-6-luna")).not.toContain("ultra");
    expect(sessionEffortLevelsForModel("gpt-5.6-luna")).not.toContain("ultra");
    expect(sessionEffortLevelsForModel("gpt-5.5")).not.toContain("ultra");
    expect(sessionEffortLevelsForModel("claude-fable-5-1")).not.toContain("ultra");
    expect(sessionEffortLevelsForModel("claude-fable-5")).not.toContain("ultra");
  });

  it("agent-facing effortLevelsForModel never offers ultra", () => {
    for (const m of ["gpt-6.1-sol", "gpt-6.1-terra", "gpt-6-sol", "gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra"]) {
      expect(effortLevelsForModel(m)).not.toContain("ultra" as never);
    }
  });
});

describe("toolModels — custom backends", () => {
  // The three custom-* backends point at an operator-supplied endpoint, so
  // kojo cannot know the model list: the UI has to fall back to free text.
  for (const tool of ["custom-claude", "custom-codex", "custom-bare"]) {
    it(`${tool} ships no preset models and no default`, () => {
      expect(modelsForTool(tool)).toEqual([]);
      expect(defaultModelForTool(tool)).toBe("");
    });
  }

  it("the pre-rename ids are gone from the table", () => {
    expect(modelsForTool("custom")).toEqual([]);
    expect(modelsForTool("llama.cpp")).toEqual([]);
  });
});

describe("gemini-agy", () => {
  it("offers low/medium/high only for bare family ids", () => {
    for (const m of ["", "gemini-4-argon", "gemini-3.8-flash", "claude-opus-5-5", "claude-sonnet-5-5"]) {
      expect(effortLevelsForModel(m, "gemini-agy")).toEqual(["low", "medium", "high"]);
    }
    expect(effortLevelsForModel("gpt-oss-120b-medium", "gemini-agy")).toEqual([]);
    // the claude backend's ladder is unaffected by the shared id
    expect(effortLevelsForModel("claude-opus-5-5")).toContain("max");
  });
});

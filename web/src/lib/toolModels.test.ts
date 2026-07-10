import { describe, expect, it } from "vitest";
import {
  defaultEffortForModel,
  defaultModelForTool,
  effortLevelsForModel,
  modelsForTool,
} from "./toolModels";

describe("toolModels — Opus 4.8 / effort defaults", () => {
  it("lists claude-opus-4-8 as a claude model", () => {
    expect(modelsForTool("claude")).toContain("claude-opus-4-8");
  });

  it("lists claude-fable-5 as a claude model", () => {
    expect(modelsForTool("claude")).toContain("claude-fable-5");
  });

  it("Fable 5 supports xhigh and max but defaults to high", () => {
    expect(effortLevelsForModel("claude-fable-5")).toContain("xhigh");
    expect(effortLevelsForModel("claude-fable-5")).toContain("max");
    expect(defaultEffortForModel("claude-fable-5")).toBe("high");
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

  it("opus alias is treated as Opus 4.8: supports xhigh, defaults to high", () => {
    expect(effortLevelsForModel("opus")).toContain("xhigh");
    expect(defaultEffortForModel("opus")).toBe("high");
  });

  it("sonnet has no xhigh and defaults to high", () => {
    expect(effortLevelsForModel("sonnet")).not.toContain("xhigh");
    expect(defaultEffortForModel("sonnet")).toBe("high");
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

  it("codex models before gpt-5.6 support xhigh, default to medium, and omit max", () => {
    expect(effortLevelsForModel("gpt-5.5")).toContain("xhigh");
    expect(effortLevelsForModel("gpt-5.5")).not.toContain("max");
    expect(defaultEffortForModel("gpt-5.5")).toBe("medium");
  });

  it("lists the gpt-5.6 family and defaults codex to gpt-5.6-sol", () => {
    expect(modelsForTool("codex")).toContain("gpt-5.6-sol");
    expect(modelsForTool("codex")).toContain("gpt-5.6-terra");
    expect(modelsForTool("codex")).toContain("gpt-5.6-luna");
    expect(defaultModelForTool("codex")).toBe("gpt-5.6-sol");
  });

  it("gpt-5.6 family supports xhigh and max", () => {
    for (const m of ["gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"]) {
      expect(effortLevelsForModel(m)).toContain("xhigh");
      expect(effortLevelsForModel(m)).toContain("max");
    }
  });

  it("gpt-5.6-sol defaults to low; terra and luna default to medium", () => {
    expect(defaultEffortForModel("gpt-5.6-sol")).toBe("low");
    expect(defaultEffortForModel("gpt-5.6-terra")).toBe("medium");
    expect(defaultEffortForModel("gpt-5.6-luna")).toBe("medium");
  });

  it("lists both grok models", () => {
    expect(modelsForTool("grok")).toEqual(["grok-4.5", "grok-composer-2.5-fast"]);
    expect(defaultModelForTool("grok")).toBe("grok-4.5");
  });

  it("grok-4.5 offers only low/medium/high and defaults to high", () => {
    expect(effortLevelsForModel("grok-4.5")).not.toContain("xhigh");
    expect(effortLevelsForModel("grok-4.5")).not.toContain("max");
    expect(defaultEffortForModel("grok-4.5")).toBe("high");
  });

  it("grok-composer-2.5-fast offers only low/medium/high and defaults to high", () => {
    expect(effortLevelsForModel("grok-composer-2.5-fast")).not.toContain("xhigh");
    expect(effortLevelsForModel("grok-composer-2.5-fast")).not.toContain("max");
    expect(defaultEffortForModel("grok-composer-2.5-fast")).toBe("high");
  });
});

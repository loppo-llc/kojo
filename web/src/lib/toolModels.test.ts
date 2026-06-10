import { describe, expect, it } from "vitest";
import {
  defaultEffortForModel,
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

  it("codex models support xhigh, default to medium, and omit max", () => {
    expect(effortLevelsForModel("gpt-5.5")).toContain("xhigh");
    expect(effortLevelsForModel("gpt-5.5")).not.toContain("max");
    expect(defaultEffortForModel("gpt-5.5")).toBe("medium");
  });
});

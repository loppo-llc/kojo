import { describe, it, expect } from "vitest";
import { shouldPromptReload } from "./versionCheck";

const none = new Set<string>();

describe("shouldPromptReload", () => {
  it("prompts when both versions are non-empty and differ", () => {
    expect(shouldPromptReload("1.0.0", "1.1.0", none)).toBe(true);
  });

  it("does not prompt when versions match", () => {
    expect(shouldPromptReload("1.0.0", "1.0.0", none)).toBe(false);
  });

  it("does not prompt when client version is empty (dev bundle)", () => {
    expect(shouldPromptReload("", "1.1.0", none)).toBe(false);
  });

  it("does not prompt when server version is empty", () => {
    expect(shouldPromptReload("1.0.0", "", none)).toBe(false);
  });

  it("does not prompt again for an already-shown server version", () => {
    expect(shouldPromptReload("1.0.0", "1.1.0", new Set(["1.1.0"]))).toBe(false);
  });

  it("prompts for a newer version even after an earlier one was shown", () => {
    expect(shouldPromptReload("1.0.0", "1.2.0", new Set(["1.1.0"]))).toBe(true);
  });
});

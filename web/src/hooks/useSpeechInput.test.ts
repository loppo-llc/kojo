import { describe, expect, it } from "vitest";
import { localeToBCP47 } from "./useSpeechInput";

describe("localeToBCP47", () => {
  it("keeps tags that already carry a region", () => {
    expect(localeToBCP47("ja-JP")).toBe("ja-JP");
    expect(localeToBCP47("EN-GB")).toBe("en-GB");
    expect(localeToBCP47("zh-Hant-TW")).toBe("zh-Hant-TW");
  });
  it("adds a default region to bare languages", () => {
    expect(localeToBCP47("ja")).toBe("ja-JP");
    expect(localeToBCP47("en")).toBe("en-US");
  });
  it("falls back to auto-detect for unknown or malformed tags", () => {
    expect(localeToBCP47("tlh")).toBe("");
    expect(localeToBCP47("")).toBe("");
    expect(localeToBCP47("ja JP")).toBe("");
  });
});

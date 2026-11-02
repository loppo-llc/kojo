import { describe, expect, it } from "vitest";
import { reuseUnchangedMessages } from "./reuseUnchanged";

type M = { id: string; content: string; extra?: { n: number } };

describe("reuseUnchangedMessages", () => {
  it("returns prev itself when the polled list is structurally identical", () => {
    const prev: M[] = [{ id: "a", content: "x", extra: { n: 1 } }, { id: "b", content: "y" }];
    const next: M[] = JSON.parse(JSON.stringify(prev));
    expect(reuseUnchangedMessages(prev, next)).toBe(prev);
  });

  it("keeps unchanged objects and takes changed or new ones", () => {
    const prev: M[] = [{ id: "a", content: "x" }, { id: "b", content: "y" }];
    const next: M[] = [{ id: "a", content: "x" }, { id: "b", content: "y2" }, { id: "c", content: "z" }];
    const out = reuseUnchangedMessages(prev, next);
    expect(out).not.toBe(prev);
    expect(out[0]).toBe(prev[0]);
    expect(out[1]).toBe(next[1]);
    expect(out[2]).toBe(next[2]);
  });

  it("detects removals and reordering", () => {
    const prev: M[] = [{ id: "a", content: "x" }, { id: "b", content: "y" }];
    const dropped = reuseUnchangedMessages(prev, [{ id: "b", content: "y" }]);
    expect(dropped).toEqual([{ id: "b", content: "y" }]);
    expect(dropped[0]).toBe(prev[1]);
    const swapped = reuseUnchangedMessages(prev, [{ id: "b", content: "y" }, { id: "a", content: "x" }]);
    expect(swapped).not.toBe(prev);
    expect(swapped.map((m) => m.id)).toEqual(["b", "a"]);
  });

  it("keeps the empty prev when the poll is still empty", () => {
    const prev: M[] = [];
    expect(reuseUnchangedMessages(prev, [])).toBe(prev);
  });

  it("returns next when prev is empty", () => {
    const next: M[] = [{ id: "a", content: "x" }];
    expect(reuseUnchangedMessages([], next)).toBe(next);
  });
});

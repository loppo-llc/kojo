import { describe, expect, it } from "vitest";
import type { AgentMessage, ChatEvent } from "../../lib/agentApi";
import {
  appendSystemErrorIfNew,
  appendUniqueMessage,
  applyDoneMessage,
  applyToolResult,
  matchToolForResult,
  newToolFromEvent,
  type StreamingTool,
} from "./chatEventReducer";

const msg = (id: string, role: AgentMessage["role"], content = ""): AgentMessage => ({
  id,
  role,
  content,
  timestamp: "2026-05-16T12:00:00Z",
});

const tool = (id: string, name: string, output: string | null = null): StreamingTool => ({
  id,
  name,
  input: "",
  output,
});

describe("matchToolForResult", () => {
  it("matches by id when toolUseId is present", () => {
    const m = matchToolForResult("u1", "Bash");
    expect(m(tool("u1", "Different"))).toBe(true);
    expect(m(tool("u2", "Bash"))).toBe(false);
  });

  it("matches by name + null output when toolUseId is empty", () => {
    const m = matchToolForResult("", "Bash");
    expect(m(tool("", "Bash"))).toBe(true);
    expect(m(tool("", "Bash", "already filled"))).toBe(false);
    expect(m(tool("", "Edit"))).toBe(false);
  });
});

describe("applyToolResult", () => {
  it("fills the most recent matching tool_use by id", () => {
    const prev: StreamingTool[] = [tool("u1", "Bash"), tool("u1", "Bash")];
    const after = applyToolResult(prev, {
      type: "tool_result",
      toolUseId: "u1",
      toolOutput: "done",
    });
    // The tail entry is the one that gets filled — Codex's two-Bash
    // workflow assigns the more recent invocation the inbound result.
    expect(after[0].output).toBeNull();
    expect(after[1].output).toBe("done");
  });

  it("fills by name + null-output when no id is supplied", () => {
    const prev: StreamingTool[] = [tool("", "Bash", "first"), tool("", "Bash")];
    const after = applyToolResult(prev, {
      type: "tool_result",
      toolName: "Bash",
      toolOutput: "second",
    });
    expect(after[0].output).toBe("first");
    expect(after[1].output).toBe("second");
  });

  it("is a no-op for orphan tool_result with no match", () => {
    const prev: StreamingTool[] = [tool("u1", "Bash", "done")];
    const after = applyToolResult(prev, {
      type: "tool_result",
      toolUseId: "u-orphan",
      toolOutput: "lost",
    });
    expect(after).toEqual(prev);
  });

  it("returns a copy for non-tool_result events (never mutates)", () => {
    const prev: StreamingTool[] = [tool("u1", "Bash")];
    const after = applyToolResult(prev, { type: "text", delta: "x" } as ChatEvent);
    expect(after).not.toBe(prev);
    expect(after).toEqual(prev);
  });

  it("treats undefined toolOutput as empty string", () => {
    const prev: StreamingTool[] = [tool("u1", "Bash")];
    const after = applyToolResult(prev, {
      type: "tool_result",
      toolUseId: "u1",
    });
    expect(after[0].output).toBe("");
  });
});

describe("newToolFromEvent", () => {
  it("builds a streaming tool entry from a tool_use event", () => {
    const built = newToolFromEvent({
      type: "tool_use",
      toolUseId: "u1",
      toolName: "Bash",
      toolInput: '{"cmd":"ls"}',
    });
    expect(built).toEqual({ id: "u1", name: "Bash", input: '{"cmd":"ls"}', output: null });
  });

  it("returns null when toolName is missing", () => {
    expect(newToolFromEvent({ type: "tool_use", toolUseId: "u1" })).toBeNull();
  });

  it("returns null for non-tool_use events", () => {
    expect(newToolFromEvent({ type: "text", delta: "x" } as ChatEvent)).toBeNull();
  });

  it("defaults id and input to empty strings when absent", () => {
    const built = newToolFromEvent({ type: "tool_use", toolName: "Bash" });
    expect(built).toEqual({ id: "", name: "Bash", input: "", output: null });
  });
});

describe("appendUniqueMessage", () => {
  it("appends when the id is new", () => {
    const prev = [msg("a", "user"), msg("b", "assistant")];
    const after = appendUniqueMessage(prev, msg("c", "assistant"));
    expect(after.map((m) => m.id)).toEqual(["a", "b", "c"]);
  });

  it("returns the SAME array reference when the id already exists (React fast-path)", () => {
    const prev = [msg("a", "user"), msg("b", "assistant")];
    const after = appendUniqueMessage(prev, msg("a", "user", "different content"));
    expect(after.map((m) => m.id)).toEqual(["a", "b"]);
    expect(after[0].content).toBe(""); // original kept, NOT replaced
    expect(after).toBe(prev);
  });
});

describe("appendSystemErrorIfNew", () => {
  const nowMs = () => 1700000000000;
  const ts = () => "2026-05-16T12:00:00Z";

  it("appends a synthesized system entry when the tail is not the same error", () => {
    const prev = [msg("a", "user", "hi")];
    const after = appendSystemErrorIfNew(prev, "⚠️ Error: oops", nowMs, ts);
    expect(after.length).toBe(2);
    expect(after[1]).toMatchObject({
      role: "system",
      content: "⚠️ Error: oops",
      timestamp: "2026-05-16T12:00:00Z",
    });
    expect(after[1].id).toBe("error_1700000000000");
  });

  it("returns the SAME array reference when the trailing entry is an identical system error", () => {
    const prev: AgentMessage[] = [
      msg("a", "user", "hi"),
      msg("error_old", "system", "⚠️ Error: oops"),
    ];
    const after = appendSystemErrorIfNew(prev, "⚠️ Error: oops", nowMs, ts);
    expect(after).toBe(prev); // React state setter fast-paths on identity
  });

  it("does NOT dedupe when the tail is a non-system entry with matching content", () => {
    const prev = [
      msg("a", "user", "⚠️ Error: oops"), // happens to match content but role differs
    ];
    const after = appendSystemErrorIfNew(prev, "⚠️ Error: oops", nowMs, ts);
    expect(after.length).toBe(2);
  });
});

describe("applyDoneMessage", () => {
  const done = (id: string, content = "ok"): ChatEvent => ({
    type: "done",
    message: msg(id, "assistant", content),
  });

  it("appends with dedupe when there is no abort marker", () => {
    const prev = [msg("a", "user")];
    const after = applyDoneMessage(prev, done("b"), null);
    expect(after.map((m) => m.id)).toEqual(["a", "b"]);
  });

  it("upgrades the abort marker in place when the server delivers a new id", () => {
    const prev = [msg("abort-1", "assistant", "<aborted>")];
    const after = applyDoneMessage(prev, done("real-1", "full reply"), "abort-1");
    expect(after.length).toBe(1);
    expect(after[0].id).toBe("real-1");
    expect(after[0].content).toBe("full reply");
  });

  it("drops the abort marker when the server's id is already in the transcript", () => {
    const prev = [
      msg("real-1", "assistant", "earlier completion"),
      msg("abort-1", "assistant", "<aborted>"),
    ];
    const after = applyDoneMessage(prev, done("real-1", "now arriving"), "abort-1");
    expect(after.map((m) => m.id)).toEqual(["real-1"]);
    expect(after[0].content).toBe("earlier completion"); // existing entry NOT clobbered
  });

  it("returns the SAME array reference when event.message is absent", () => {
    const prev = [msg("a", "user")];
    const after = applyDoneMessage(prev, { type: "done" }, null);
    expect(after).toBe(prev);
  });

  it("returns the SAME array reference for non-done events", () => {
    const prev = [msg("a", "user")];
    const after = applyDoneMessage(prev, { type: "text", delta: "x" } as ChatEvent, null);
    expect(after).toBe(prev);
  });
});

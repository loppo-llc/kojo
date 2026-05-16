import { describe, expect, it } from "vitest";
import {
  buildAgentSavePayload,
  needsCustomURLFor,
  type AgentSettingsFormState,
} from "./agentSettingsPayload";

// Minimal AgentSettingsFormState — every test overrides only the
// fields it cares about so the assertion is local.
function baseState(over: Partial<AgentSettingsFormState> = {}): AgentSettingsFormState {
  return {
    name: " test ",
    persona: " hi ",
    publicProfile: " profile ",
    publicProfileOverride: false,
    model: " claude-sonnet-4 ",
    effort: "medium",
    tool: "claude",
    customBaseURL: " http://x ",
    thinkingMode: "auto",
    workDir: " /tmp ",
    cronExpr: "0 * * * *",
    timeoutMinutes: 10,
    resumeIdleMinutes: 30,
    silentStart: "22:00",
    silentEnd: "07:00",
    notifyDuringSilent: false,
    cronMessage: "  check in  ",
    allowedTools: ["Bash"],
    allowProtectedPaths: ["/etc"],
    tts: { enabled: false, model: "", voice: "", stylePrompt: "" },
    ...over,
  };
}

describe("needsCustomURLFor", () => {
  it.each(["custom", "llama.cpp"])("returns true for %s", (tool) => {
    expect(needsCustomURLFor(tool)).toBe(true);
  });

  it.each(["claude", "codex", "gemini", ""])("returns false for %s", (tool) => {
    expect(needsCustomURLFor(tool)).toBe(false);
  });
});

describe("buildAgentSavePayload", () => {
  it("trims name / persona / model / tool / workDir", () => {
    const out = buildAgentSavePayload(baseState());
    expect(out.name).toBe("test");
    expect(out.persona).toBe("hi");
    expect(out.model).toBe("claude-sonnet-4");
    expect(out.tool).toBe("claude");
    expect(out.workDir).toBe("/tmp");
  });

  it("omits publicProfile when override is false", () => {
    const out = buildAgentSavePayload(baseState({ publicProfileOverride: false }));
    expect("publicProfile" in out).toBe(false);
    expect(out.publicProfileOverride).toBe(false);
  });

  it("includes (trimmed) publicProfile when override is true", () => {
    const out = buildAgentSavePayload(
      baseState({ publicProfileOverride: true, publicProfile: "  hello  " }),
    );
    expect(out.publicProfile).toBe("hello");
    expect(out.publicProfileOverride).toBe(true);
  });

  it("omits effort for tools that don't support an effort selector", () => {
    // gemini doesn't surface effort in toolModels
    const out = buildAgentSavePayload(baseState({ tool: "gemini" }));
    expect(out.effort).toBeUndefined();
  });

  it("forwards effort for tools that do support it", () => {
    // Only "claude" passes supportsEffort today.
    const out = buildAgentSavePayload(baseState({ tool: "claude", effort: "high" }));
    expect(out.effort).toBe("high");
  });

  it("emits customBaseURL only for custom / llama.cpp", () => {
    expect(buildAgentSavePayload(baseState({ tool: "claude" })).customBaseURL).toBeUndefined();
    expect(buildAgentSavePayload(baseState({ tool: "custom" })).customBaseURL).toBe("http://x");
    expect(
      buildAgentSavePayload(baseState({ tool: "llama.cpp", customBaseURL: " http://y " })).customBaseURL,
    ).toBe("http://y");
  });

  it("emits thinkingMode ONLY for llama.cpp", () => {
    expect(buildAgentSavePayload(baseState({ tool: "claude" })).thinkingMode).toBeUndefined();
    expect(buildAgentSavePayload(baseState({ tool: "custom" })).thinkingMode).toBeUndefined();
    expect(
      buildAgentSavePayload(baseState({ tool: "llama.cpp", thinkingMode: "deep" })).thinkingMode,
    ).toBe("deep");
  });

  it("emits allowedTools ONLY for custom", () => {
    expect(buildAgentSavePayload(baseState({ tool: "claude" })).allowedTools).toBeUndefined();
    expect(
      buildAgentSavePayload(baseState({ tool: "custom", allowedTools: ["Bash", "Edit"] }))
        .allowedTools,
    ).toEqual(["Bash", "Edit"]);
  });

  it("emits allowProtectedPaths for claude AND custom, nothing else", () => {
    expect(
      buildAgentSavePayload(baseState({ tool: "claude", allowProtectedPaths: ["/etc"] }))
        .allowProtectedPaths,
    ).toEqual(["/etc"]);
    expect(
      buildAgentSavePayload(baseState({ tool: "custom", allowProtectedPaths: ["/etc"] }))
        .allowProtectedPaths,
    ).toEqual(["/etc"]);
    expect(
      buildAgentSavePayload(baseState({ tool: "codex" })).allowProtectedPaths,
    ).toBeUndefined();
    expect(
      buildAgentSavePayload(baseState({ tool: "gemini" })).allowProtectedPaths,
    ).toBeUndefined();
    expect(
      buildAgentSavePayload(baseState({ tool: "llama.cpp" })).allowProtectedPaths,
    ).toBeUndefined();
  });

  it("collapses empty TTS strings to undefined and trims stylePrompt", () => {
    const out = buildAgentSavePayload(
      baseState({
        tts: { enabled: true, model: "", voice: "", stylePrompt: "  whisper  " },
      }),
    );
    expect(out.tts).toEqual({
      enabled: true,
      model: undefined,
      voice: undefined,
      stylePrompt: "whisper",
    });
  });

  it("passes through forced-bool / numeric fields verbatim", () => {
    const out = buildAgentSavePayload(
      baseState({
        notifyDuringSilent: true,
        timeoutMinutes: 25,
        resumeIdleMinutes: 0,
        silentStart: "23:00",
        silentEnd: "08:00",
        cronExpr: "*/5 * * * *",
      }),
    );
    expect(out.notifyDuringSilent).toBe(true);
    expect(out.timeoutMinutes).toBe(25);
    expect(out.resumeIdleMinutes).toBe(0);
    expect(out.silentStart).toBe("23:00");
    expect(out.silentEnd).toBe("08:00");
    expect(out.cronExpr).toBe("*/5 * * * *");
  });
});

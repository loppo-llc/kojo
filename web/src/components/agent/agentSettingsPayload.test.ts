import { describe, expect, it } from "vitest";
import {
  buildAgentSavePayload,
  defaultCodexApps,
  isCodexCLITool,
  needsCustomURLFor,
  discoversCustomModels,
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
    responseLanguage: "",
    workDir: " /tmp ",
    cronExpr: "0 * * * *",
    timeoutMinutes: 10,
    resumeIdleMinutes: 30,
    backgroundMaxMinutes: 0,
    silentStart: "22:00",
    silentEnd: "07:00",
    notifyDuringSilent: false,
    cronMessage: "  check in  ",
    allowedTools: ["Bash"],
    allowProtectedPaths: ["/etc"],
    tts: { enabled: false, model: "", voice: "", stylePrompt: "" },
    disabledInjections: [],
    autoEffort: true,
    maxOutputTokens: 0,
    ...over,
  };
}

describe("needsCustomURLFor", () => {
  it.each(["custom-claude", "custom-codex", "custom-bare", "custom", "llama.cpp"])(
    "returns true for %s",
    (tool) => {
      expect(needsCustomURLFor(tool)).toBe(true);
    },
  );

  it.each(["claude", "codex", ""])("returns false for %s", (tool) => {
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
    const out = buildAgentSavePayload(baseState({ tool: "custom-bare" }));
    expect(out.effort).toBeUndefined();
  });

  it("forwards codexApps only for codex-CLI tools", () => {
    expect(buildAgentSavePayload(baseState({ tool: "codex", codexApps: false })).codexApps).toBe(false);
    expect(buildAgentSavePayload(baseState({ tool: "custom-codex", codexApps: true })).codexApps).toBe(true);
    expect(buildAgentSavePayload(baseState({ tool: "codex" })).codexApps).toBeUndefined();
    expect(buildAgentSavePayload(baseState({ tool: "claude", codexApps: false })).codexApps).toBeUndefined();
  });

  it("forwards maxOutputTokens only for gemini", () => {
    expect(buildAgentSavePayload(baseState({ tool: "gemini-api", maxOutputTokens: 8192 })).maxOutputTokens).toBe(8192);
    expect(buildAgentSavePayload(baseState({ tool: "gemini-api" })).maxOutputTokens).toBe(0);
    expect(buildAgentSavePayload(baseState({ tool: "claude", maxOutputTokens: 8192 })).maxOutputTokens).toBeUndefined();
  });

  it("codexApps defaults: on for codex, off for custom-codex", () => {
    expect(defaultCodexApps("codex")).toBe(true);
    expect(defaultCodexApps("custom-codex")).toBe(false);
    expect(isCodexCLITool("codex")).toBe(true);
    expect(isCodexCLITool("custom-codex")).toBe(true);
    expect(isCodexCLITool("custom")).toBe(false);
    expect(isCodexCLITool("claude")).toBe(false);
  });

  it("forwards autoEffort as-is", () => {
    expect(buildAgentSavePayload(baseState({ autoEffort: true })).autoEffort).toBe(true);
    expect(buildAgentSavePayload(baseState({ autoEffort: false })).autoEffort).toBe(false);
  });

  it("forwards effort for tools that do support it", () => {
    const out = buildAgentSavePayload(baseState({ tool: "claude", effort: "high" }));
    expect(out.effort).toBe("high");
    expect(buildAgentSavePayload(baseState({ tool: "codex", effort: "medium" })).effort).toBe("medium");
  });

  it("emits customBaseURL only for the custom-* backends", () => {
    expect(buildAgentSavePayload(baseState({ tool: "claude" })).customBaseURL).toBeUndefined();
    expect(buildAgentSavePayload(baseState({ tool: "custom-claude" })).customBaseURL).toBe("http://x");
    expect(buildAgentSavePayload(baseState({ tool: "custom-codex" })).customBaseURL).toBe("http://x");
    expect(
      buildAgentSavePayload(baseState({ tool: "custom-bare", customBaseURL: " http://y " }))
        .customBaseURL,
    ).toBe("http://y");
  });

  it("always emits responseLanguage trimmed (empty = auto)", () => {
    expect(buildAgentSavePayload(baseState()).responseLanguage).toBe("");
    expect(
      buildAgentSavePayload(baseState({ responseLanguage: "  関西弁の日本語 " })).responseLanguage,
    ).toBe("関西弁の日本語");
  });

  it("emits thinkingMode ONLY for custom-bare", () => {
    expect(buildAgentSavePayload(baseState({ tool: "claude" })).thinkingMode).toBeUndefined();
    expect(buildAgentSavePayload(baseState({ tool: "custom-claude" })).thinkingMode).toBeUndefined();
    expect(buildAgentSavePayload(baseState({ tool: "custom-codex" })).thinkingMode).toBeUndefined();
    expect(
      buildAgentSavePayload(baseState({ tool: "custom-bare", thinkingMode: "deep" })).thinkingMode,
    ).toBe("deep");
  });

  it("emits allowedTools ONLY for custom-claude", () => {
    expect(buildAgentSavePayload(baseState({ tool: "claude" })).allowedTools).toBeUndefined();
    expect(buildAgentSavePayload(baseState({ tool: "custom-codex" })).allowedTools).toBeUndefined();
    expect(
      buildAgentSavePayload(baseState({ tool: "custom-claude", allowedTools: ["Bash", "Edit"] }))
        .allowedTools,
    ).toEqual(["Bash", "Edit"]);
  });

  it("emits allowProtectedPaths for claude AND custom-claude, nothing else", () => {
    expect(
      buildAgentSavePayload(baseState({ tool: "claude", allowProtectedPaths: ["/etc"] }))
        .allowProtectedPaths,
    ).toEqual(["/etc"]);
    expect(
      buildAgentSavePayload(baseState({ tool: "custom-claude", allowProtectedPaths: ["/etc"] }))
        .allowProtectedPaths,
    ).toEqual(["/etc"]);
    expect(
      buildAgentSavePayload(baseState({ tool: "codex" })).allowProtectedPaths,
    ).toBeUndefined();
    expect(
      buildAgentSavePayload(baseState({ tool: "custom-bare" })).allowProtectedPaths,
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
        backgroundMaxMinutes: 180,
        silentStart: "23:00",
        silentEnd: "08:00",
        cronExpr: "*/5 * * * *",
      }),
    );
    expect(out.notifyDuringSilent).toBe(true);
    expect(out.timeoutMinutes).toBe(25);
    expect(out.resumeIdleMinutes).toBe(0);
    expect(out.backgroundMaxMinutes).toBe(180);
    expect(out.silentStart).toBe("23:00");
    expect(out.silentEnd).toBe("08:00");
    expect(out.cronExpr).toBe("*/5 * * * *");
  });

  it("passes through disabledInjections verbatim", () => {
    expect(buildAgentSavePayload(baseState()).disabledInjections).toEqual([]);
    expect(
      buildAgentSavePayload(baseState({ disabledInjections: ["memory_search", "persona_anchor"] }))
        .disabledInjections,
    ).toEqual(["memory_search", "persona_anchor"]);
  });
});

describe("discoversCustomModels", () => {
  it("covers free-URL custom backends and local custom-opencode presets only", () => {
    expect(discoversCustomModels("custom-codex", "http://localhost:8080")).toBe(true);
    expect(discoversCustomModels("custom-opencode", "http://127.0.0.1:8099/v1")).toBe(true);
    expect(discoversCustomModels("custom-opencode", "https://openrouter.ai/api/v1")).toBe(false);
    expect(discoversCustomModels("custom-opencode", "")).toBe(false);
    expect(discoversCustomModels("custom-opencode", "http://127.0.0.1:8080")).toBe(false);
    expect(discoversCustomModels("custom-opencode", "http://127.0.0.1:8080/v1?x")).toBe(false);
    expect(discoversCustomModels("claude", "http://127.0.0.1:8080/v1")).toBe(false);
  });
});

import type { AgentUpdateParams } from "../../lib/agentApi";
import { supportsEffort } from "../../lib/toolModels";

/**
 * Snapshot of every form field that contributes to the AgentSettings
 * save payload. Stays a plain data object so buildAgentSavePayload
 * can be unit-tested without rendering the form. Keep this in sync
 * with the useState declarations at the top of AgentSettings.tsx —
 * if a field is added to the form, add it here too and let TypeScript
 * surface the missing wire-up.
 */
export interface AgentSettingsFormState {
  name: string;
  persona: string;
  publicProfile: string;
  publicProfileOverride: boolean;
  model: string;
  effort: string;
  tool: string;
  customBaseURL: string;
  thinkingMode: string;
  workDir: string;
  cronExpr: string;
  timeoutMinutes: number;
  resumeIdleMinutes: number;
  silentStart: string;
  silentEnd: string;
  notifyDuringSilent: boolean;
  cronMessage: string;
  allowedTools: string[];
  allowProtectedPaths: string[];
  tts: {
    enabled: boolean;
    model: string;
    voice: string;
    stylePrompt: string;
  };
}

/**
 * True when `tool` requires the operator to supply a CustomBaseURL.
 * "custom" is the generic OpenAI-compatible adapter (LM Studio,
 * llama.cpp's OpenAI server, third-party gateways); "llama.cpp"
 * targets the bespoke llama.cpp server endpoint.
 */
export function needsCustomURLFor(tool: string): boolean {
  return tool === "custom" || tool === "llama.cpp";
}

/**
 * Build the PATCH /agents/{id} body from form state. Pure — no I/O,
 * no React state, no clocks. Mirrors the conditional shape the
 * server expects:
 *
 *   - name / persona / workDir / cronMessage / model: always trimmed
 *   - publicProfile: only included when publicProfileOverride is true
 *   - effort: included only when the chosen tool supports an effort
 *     selector (otherwise the field would silently lock in an
 *     incompatible value).
 *   - customBaseURL: trimmed and included only for tools that need
 *     it; otherwise undefined so the server clears the field.
 *   - thinkingMode: only emitted for llama.cpp.
 *   - allowedTools: only emitted for the "custom" tool (where the
 *     operator picks per-tool permissions).
 *   - allowProtectedPaths: only emitted for claude / custom — those
 *     are the only backends whose Bash/Edit gating respects the
 *     server-side allowlist.
 *   - tts: nested object; empty strings collapse to undefined so the
 *     server treats them as "use default".
 */
export function buildAgentSavePayload(state: AgentSettingsFormState): AgentUpdateParams {
  const trimmed = {
    name: state.name.trim(),
    persona: state.persona.trim(),
    model: state.model.trim(),
    tool: state.tool.trim(),
    workDir: state.workDir.trim(),
  };
  const publicProfilePart = state.publicProfileOverride
    ? { publicProfile: state.publicProfile.trim() }
    : {};
  return {
    name: trimmed.name,
    persona: trimmed.persona,
    ...publicProfilePart,
    publicProfileOverride: state.publicProfileOverride,
    model: trimmed.model,
    effort: supportsEffort(state.tool) ? state.effort : undefined,
    tool: trimmed.tool,
    customBaseURL: needsCustomURLFor(state.tool) ? state.customBaseURL.trim() : undefined,
    thinkingMode: state.tool === "llama.cpp" ? state.thinkingMode : undefined,
    workDir: trimmed.workDir,
    cronExpr: state.cronExpr,
    timeoutMinutes: state.timeoutMinutes,
    resumeIdleMinutes: state.resumeIdleMinutes,
    silentStart: state.silentStart,
    silentEnd: state.silentEnd,
    notifyDuringSilent: state.notifyDuringSilent,
    cronMessage: state.cronMessage,
    allowedTools: state.tool === "custom" ? state.allowedTools : undefined,
    allowProtectedPaths:
      state.tool === "claude" || state.tool === "custom" ? state.allowProtectedPaths : undefined,
    tts: {
      enabled: state.tts.enabled,
      model: state.tts.model || undefined,
      voice: state.tts.voice || undefined,
      stylePrompt: state.tts.stylePrompt.trim() || undefined,
    },
  };
}

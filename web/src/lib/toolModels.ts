/** Per-backend model whitelist and defaults. */

export interface ToolModelConfig {
  default: string;
  models: string[];
}

export const toolModels: Record<string, ToolModelConfig> = {
  claude: {
    default: "sonnet",
    models: ["sonnet", "opus", "haiku"],
  },
  codex: {
    default: "gpt-5.4",
    models: [
      "gpt-5.3-codex",
      "gpt-5.4",
      "gpt-5.2-codex",
      "gpt-5.1-codex-max",
      "gpt-5.2",
      "gpt-5.1-codex-mini",
    ],
  },
  gemini: {
    default: "gemini-3-pro-preview",
    models: [
      "gemini-3-pro-preview",
      "gemini-3-flash-preview",
      "gemini-2.5-pro",
      "gemini-2.5-flash",
      "gemini-2.5-flash-lite",
    ],
  },
};

/** Return the default model for a given tool. */
export function defaultModelForTool(tool: string): string {
  return toolModels[tool]?.default ?? "sonnet";
}

/** Return available models for a given tool. */
export function modelsForTool(tool: string): string[] {
  return toolModels[tool]?.models ?? [];
}

/** Effort levels (claude only). */
export const effortLevels = ["low", "medium", "high", "max"] as const;
export type EffortLevel = (typeof effortLevels)[number];

export function supportsEffort(tool: string): boolean {
  return tool === "claude";
}

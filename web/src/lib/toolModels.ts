/** Per-backend model whitelist and defaults. */

export interface ToolModelConfig {
  default: string;
  models: string[];
}

export const toolModels: Record<string, ToolModelConfig> = {
  claude: {
    default: "sonnet",
    models: ["sonnet", "opus", "claude-opus-4-7", "claude-opus-4-6", "haiku"],
  },
  codex: {
    default: "gpt-5.5",
    models: [
      "gpt-5.5",
      "gpt-5.4",
      "gpt-5.4-mini",
      "gpt-5.3-codex",
      "gpt-5.2",
    ],
  },
  grok: {
    default: "grok-build",
    models: ["grok-build"],
  },
  custom: {
    default: "",
    models: [],
  },
  "llama.cpp": {
    default: "",
    models: [],
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
export const effortLevels = ["low", "medium", "high", "xhigh", "max"] as const;
export type EffortLevel = (typeof effortLevels)[number];

/** Models that support the xhigh effort level. */
const xhighModels = new Set(["opus", "claude-opus-4-7", "grok-build"]);

export function supportsEffort(tool: string): boolean {
  return tool === "claude" || tool === "grok";
}

/** Return available effort levels for a given model. */
export function effortLevelsForModel(model: string): readonly EffortLevel[] {
  if (xhighModels.has(model)) return effortLevels;
  return effortLevels.filter((e) => e !== "xhigh");
}

/** Return the default effort level label for a given model. */
export function defaultEffortForModel(model: string): string {
  return xhighModels.has(model) ? "xhigh" : "high";
}

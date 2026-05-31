/** Per-backend model whitelist and defaults. */

export interface ToolModelConfig {
  default: string;
  models: string[];
}

export const toolModels: Record<string, ToolModelConfig> = {
  claude: {
    default: "sonnet",
    models: ["sonnet", "opus", "claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6", "haiku"],
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

/** Effort levels shared by Claude/Grok. Codex omits "max". */
export const effortLevels = ["low", "medium", "high", "xhigh", "max"] as const;
export type EffortLevel = (typeof effortLevels)[number];

/** Models that support the xhigh effort level. */
const xhighModels = new Set(["opus", "claude-opus-4-8", "claude-opus-4-7", "grok-build"]);
const codexEffortModels = new Set(toolModels.codex.models);

/**
 * Models whose default effort is xhigh (rather than high).
 * Per https://code.claude.com/docs/en/model-config, Opus 4.8 supports xhigh but
 * defaults to high; only Opus 4.7 defaults to xhigh. The "opus" alias is treated
 * as Opus 4.8, so it defaults to high. grok-build keeps xhigh default.
 */
const defaultXhighModels = new Set(["claude-opus-4-7", "grok-build"]);

export function supportsEffort(tool: string): boolean {
  return tool === "claude" || tool === "grok" || tool === "codex";
}

/** Return available effort levels for a given model. */
export function effortLevelsForModel(model: string): readonly EffortLevel[] {
  if (codexEffortModels.has(model)) return ["low", "medium", "high", "xhigh"] as const;
  if (xhighModels.has(model)) return effortLevels;
  return effortLevels.filter((e) => e !== "xhigh");
}

/** Return the default effort level label for a given model. */
export function defaultEffortForModel(model: string): string {
  if (codexEffortModels.has(model)) return "medium";
  return defaultXhighModels.has(model) ? "xhigh" : "high";
}

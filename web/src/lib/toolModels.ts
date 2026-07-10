/** Per-backend model whitelist and defaults. */

export interface ToolModelConfig {
  default: string;
  models: string[];
}

export const toolModels: Record<string, ToolModelConfig> = {
  claude: {
    default: "sonnet",
    models: ["sonnet", "claude-sonnet-5", "claude-sonnet-4-6", "opus", "claude-fable-5", "claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6", "haiku"],
  },
  codex: {
    default: "gpt-5.6-sol",
    models: [
      "gpt-5.6-sol",
      "gpt-5.6-terra",
      "gpt-5.6-luna",
      "gpt-5.5",
      "gpt-5.4",
      "gpt-5.4-mini",
      "gpt-5.3-codex",
      "gpt-5.2",
    ],
  },
  grok: {
    default: "grok-4.5",
    models: ["grok-4.5", "grok-composer-2.5-fast"],
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

/** Effort levels shared by Claude/Grok. Codex models before gpt-5.6 omit "max". */
export const effortLevels = ["low", "medium", "high", "xhigh", "max"] as const;
export type EffortLevel = (typeof effortLevels)[number];

/** Models that support the xhigh effort level. */
const xhighModels = new Set(["opus", "claude-sonnet-5", "claude-fable-5", "claude-opus-4-8", "claude-opus-4-7"]);
const codexEffortModels = new Set(toolModels.codex.models);
// codex CLI 0.144.1 models_cache.json: the gpt-5.6 family advertises
// low/medium/high/xhigh/max (sol and terra also list "ultra", which kojo's
// effort scale doesn't model). Older gpt-5.x models stop at xhigh.
// Keep in sync with agent.go codexMaxEffortModels.
const codexMaxModels = new Set(["gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"]);
// gpt-5.6-sol advertises default_reasoning_level "low"; every other codex
// model defaults to medium.
const codexLowDefaultModels = new Set(["gpt-5.6-sol"]);
// grok CLI 0.2.91 advertises only low/medium/high for its models
// (grok-4.5 lists efforts [high,medium,low]; composer lists none), so
// neither xhigh nor max is offered. Keep in sync with agent.go grokEffortModels.
const grokEffortModels = new Set(toolModels.grok.models);

/**
 * Models whose default effort is xhigh (rather than high).
 * Per https://code.claude.com/docs/en/model-config, Opus 4.8 supports xhigh but
 * defaults to high; only Opus 4.7 defaults to xhigh. The "opus" alias is treated
 * as Opus 4.8, so it defaults to high. grok-4.5 advertises low/medium/high
 * (default high) and grok-composer-2.5-fast advertises an empty efforts list,
 * so neither offers xhigh and both default to high.
 */
const defaultXhighModels = new Set(["claude-opus-4-7"]);

export function supportsEffort(tool: string): boolean {
  return tool === "claude" || tool === "grok" || tool === "codex";
}

/** Return available effort levels for a given model. */
export function effortLevelsForModel(model: string): readonly EffortLevel[] {
  if (codexMaxModels.has(model)) return effortLevels;
  if (codexEffortModels.has(model)) return ["low", "medium", "high", "xhigh"] as const;
  if (grokEffortModels.has(model)) return ["low", "medium", "high"] as const;
  if (xhighModels.has(model)) return effortLevels;
  return effortLevels.filter((e) => e !== "xhigh");
}

/** Return the default effort level label for a given model. */
export function defaultEffortForModel(model: string): string {
  if (codexLowDefaultModels.has(model)) return "low";
  if (codexEffortModels.has(model)) return "medium";
  return defaultXhighModels.has(model) ? "xhigh" : "high";
}

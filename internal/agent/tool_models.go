package agent

import "slices"

// builtinToolModels is the server-side copy of the per-backend model
// whitelist in web/src/lib/toolModels.ts. The WebUI only offers these ids;
// the agent-driven model switch has no picker, so it validates against this
// list instead (a typo would otherwise leave the agent on a model it cannot
// run, with no turn left to repair itself from).
// TestBuiltinToolModelsMatchWeb fails when the two lists drift.
var builtinToolModels = map[string][]string{
	ToolClaude: {
		"sonnet", "claude-sonnet-5-5", "claude-sonnet-5", "claude-sonnet-4-6",
		"opus", "claude-opus-5-5", "claude-opus-5",
		"fable", "claude-fable-5-2", "claude-fable-5-1", "claude-fable-5",
		"claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6",
		"haiku", "claude-haiku-5-5",
	},
	ToolCodex: {
		"gpt-6.2-sol", "gpt-6.2-astra", "gpt-6.1-sol", "gpt-6.1-terra",
		"gpt-6-astra", "gpt-6-sol", "gpt-6-luna",
		"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
		"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex", "gpt-5.2",
	},
	ToolGrok: {"grok-4.7", "grok-4.7-build-fast", "grok-4.6", "grok-4.5"},
	ToolGeminiAPI: {
		"gemini-4-neon", "gemini-4-hydrogen", "gemini-4-carbon", "gemini-4-argon",
		"gemini-3.8-flash",
	},
	ToolGeminiAgy: {
		"gemini-4-neon", "gemini-4-hydrogen", "gemini-4-carbon", "gemini-4-argon",
		"gemini-3.8-flash", "claude-opus-5-5", "claude-sonnet-5-5", "gpt-oss-120b-medium",
	},
}

// customToolNames are the backends whose model ids come from an
// operator-supplied endpoint, so no fixed list exists.
var customToolNames = []string{ToolCustomClaude, ToolCustomCodex, ToolCustomBare, ToolCustomOpenCode}

// KnownTool reports whether tool names a backend kojo can run.
func KnownTool(tool string) bool {
	tool = NormalizeToolName(tool)
	_, builtin := builtinToolModels[tool]
	return builtin || slices.Contains(customToolNames, tool)
}

// ToolModelAllowed reports whether model is selectable on tool. Custom
// backends accept any non-empty id.
func ToolModelAllowed(tool, model string) bool {
	if model == "" {
		return false
	}
	tool = NormalizeToolName(tool)
	if models, ok := builtinToolModels[tool]; ok {
		return slices.Contains(models, model)
	}
	return slices.Contains(customToolNames, tool)
}

// ToolModelOptions returns a copy of the built-in model lists.
func ToolModelOptions() map[string][]string {
	out := make(map[string][]string, len(builtinToolModels))
	for tool, models := range builtinToolModels {
		out[tool] = slices.Clone(models)
	}
	return out
}

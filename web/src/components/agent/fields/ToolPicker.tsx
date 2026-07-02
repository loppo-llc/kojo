import {
  defaultModelForTool,
  effortLevelsForModel,
  type EffortLevel,
} from "../../../lib/toolModels";
import { Field } from "../../ui/Field";

const TOOLS = ["claude", "codex", "grok", "custom", "llama.cpp"];

/**
 * The "Tool" (backend) selector. Switching tools resets the model to the
 * backend default (or clears it for custom/llama.cpp) and drops an effort
 * level the new default model can't support.
 *
 * `isDisabled` is optional: when provided (AgentCreate gates on server
 * availability) each button gets a `disabled` attribute plus a dimmed style;
 * when omitted (AgentSettings) neither is rendered.
 */
export function ToolPicker({
  tool,
  setTool,
  setModel,
  effort,
  setEffort,
  isDisabled,
}: {
  tool: string;
  setTool: (t: string) => void;
  setModel: (m: string) => void;
  effort: EffortLevel | "";
  setEffort: (e: EffortLevel | "") => void;
  isDisabled?: (t: string) => boolean;
}) {
  return (
    <Field label="Tool">
      <div className="flex flex-wrap gap-2">
        {TOOLS.map((t) => {
          const selected = tool === t;
          return (
            <button
              key={t}
              type="button"
              onClick={() => {
                if (t !== tool) {
                  setTool(t);
                  if (t === "custom" || t === "llama.cpp") {
                    setModel("");
                  } else {
                    const m = defaultModelForTool(t);
                    setModel(m);
                    if (effort && !effortLevelsForModel(m).includes(effort)) setEffort("");
                  }
                }
              }}
              disabled={isDisabled ? isDisabled(t) : undefined}
              className={`rounded-lg border px-3 py-2 font-mono text-[13px] transition-colors disabled:opacity-30 ${
                selected
                  ? "border-copper bg-copper/15 text-copper-bright"
                  : "border-hairline bg-raised text-ink-dim hover:border-ink-faint hover:text-ink"
              }`}
            >
              {t}
            </button>
          );
        })}
      </div>
    </Field>
  );
}

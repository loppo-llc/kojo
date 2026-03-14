import { useState } from "react";
import type { ToolUse } from "../../lib/agentApi";

interface ToolUseCardProps {
  toolUse: ToolUse;
  defaultExpanded?: boolean;
}

function extractDescription(input: string): string | undefined {
  if (!input) return undefined;
  try {
    const parsed = JSON.parse(input);
    if (typeof parsed === "object" && parsed !== null && typeof parsed.description === "string") {
      return parsed.description;
    }
  } catch {
    // not JSON
  }
  return undefined;
}

export function ToolUseCard({ toolUse, defaultExpanded = false }: ToolUseCardProps) {
  const [expanded, setExpanded] = useState(defaultExpanded);
  const description = extractDescription(toolUse.input);

  return (
    <div className="my-1 border border-neutral-700 rounded-lg overflow-hidden text-xs">
      <button
        onClick={() => setExpanded(!expanded)}
        className="w-full flex items-center gap-2 px-3 py-1.5 bg-neutral-800/50 hover:bg-neutral-800 text-neutral-400"
      >
        <svg
          className={`w-3 h-3 transition-transform ${expanded ? "rotate-90" : ""}`}
          fill="none"
          viewBox="0 0 24 24"
          stroke="currentColor"
        >
          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M9 5l7 7-7 7" />
        </svg>
        <span className="font-mono text-neutral-300">
          {toolUse.name}{description ? `: ${description}` : ""}
        </span>
      </button>
      {expanded && (
        <div className="px-3 py-2 space-y-2 bg-neutral-900/50">
          {toolUse.input && (
            <div>
              <div className="text-neutral-500 mb-0.5">Input</div>
              <pre className="text-neutral-300 whitespace-pre-wrap break-all max-h-40 overflow-y-auto">
                {toolUse.input}
              </pre>
            </div>
          )}
          {toolUse.output && (
            <div>
              <div className="text-neutral-500 mb-0.5">Output</div>
              <pre className="text-neutral-300 whitespace-pre-wrap break-all max-h-40 overflow-y-auto">
                {toolUse.output}
              </pre>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

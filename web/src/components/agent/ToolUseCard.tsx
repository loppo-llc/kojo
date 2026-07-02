import { useState } from "react";
import type { ToolUse } from "../../lib/agentApi";

interface ToolUseCardProps {
  toolUse: ToolUse;
  defaultExpanded?: boolean;
}

function extractPreview(input: string): string | undefined {
  if (!input) return undefined;
  try {
    const parsed = JSON.parse(input);
    if (typeof parsed === "object" && parsed !== null) {
      if (typeof parsed.description === "string") return parsed.description;
      if (typeof parsed.command === "string") return parsed.command;
      if (typeof parsed.file_path === "string") return parsed.file_path;
      if (typeof parsed.pattern === "string") return parsed.pattern;
      if (typeof parsed.prompt === "string") return parsed.prompt.slice(0, 80);
    }
  } catch {
    // not JSON — use raw input preview
  }
  const line = input.split("\n")[0].trim();
  return line.length > 80 ? line.slice(0, 80) + "…" : line || undefined;
}

export function ToolUseCard({ toolUse, defaultExpanded = false }: ToolUseCardProps) {
  const [expanded, setExpanded] = useState(defaultExpanded);
  const preview = extractPreview(toolUse.input);

  return (
    <div className="my-1 overflow-hidden rounded-[10px] border border-hairline bg-surface text-xs">
      <button
        onClick={() => setExpanded(!expanded)}
        className="flex w-full min-w-0 items-center gap-2 px-3 py-1.5 text-ink-dim transition-colors hover:bg-hover"
      >
        <svg
          className={`h-3 w-3 shrink-0 transition-transform ${expanded ? "rotate-90" : ""}`}
          fill="none"
          viewBox="0 0 24 24"
          stroke="currentColor"
        >
          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M9 5l7 7-7 7" />
        </svg>
        <span className="shrink-0 font-mono text-[12px] text-ink">
          {toolUse.name}
        </span>
        {preview && (
          <span className="min-w-0 truncate font-mono text-[11px] text-ink-faint">
            {preview}
          </span>
        )}
      </button>
      {expanded && (
        <div className="space-y-2 border-t border-hairline bg-app/40 px-3 py-2">
          {toolUse.input && (
            <div>
              <div className="mb-0.5 font-mono text-[10px] uppercase tracking-wide text-ink-faint">Input</div>
              <pre className="max-h-60 overflow-x-auto overflow-y-auto whitespace-pre-wrap wrap-anywhere text-ink-dim">
                {toolUse.input}
              </pre>
            </div>
          )}
          {toolUse.output && (
            <div>
              <div className="mb-0.5 font-mono text-[10px] uppercase tracking-wide text-ink-faint">Output</div>
              <pre className="max-h-60 overflow-x-auto overflow-y-auto whitespace-pre-wrap wrap-anywhere text-ink-dim">
                {toolUse.output}
              </pre>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

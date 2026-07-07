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
  const children = toolUse.children ?? [];

  // A Task launched with run_in_background prints "Async agent launched" as its
  // immediate tool_result; its real output streams in later via the subagent
  // tailer (attached to children). Flag it so the card shows a background badge.
  const isBackground =
    toolUse.name === "Task" && /Async agent launched/i.test(toolUse.output ?? "");
  // Weak done heuristic: every observed tool child has an output (nothing left
  // pending). No children yet ⇒ still spinning up ⇒ running.
  const backgroundDone =
    isBackground &&
    children.length > 0 &&
    children.every((c) => c.name === "" || (c.output ?? "") !== "");

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
        {children.length > 0 && (
          <span className="shrink-0 rounded-full bg-app/60 px-1.5 py-0.5 font-mono text-[10px] text-ink-faint">
            {children.length} sub
          </span>
        )}
        {isBackground && (
          <span
            className={`shrink-0 rounded-full px-1.5 py-0.5 font-mono text-[10px] ${
              backgroundDone ? "bg-emerald-500/10 text-emerald-400" : "bg-amber-500/10 text-amber-400"
            }`}
          >
            {backgroundDone ? "background done" : "background running"}
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
          {children.length > 0 && (
            <div className="border-l-2 border-hairline pl-2">
              <div className="mb-1 font-mono text-[10px] uppercase tracking-wide text-ink-faint">Subagent</div>
              {children.map((child, i) =>
                child.name ? (
                  <ToolUseCard key={i} toolUse={child} />
                ) : (
                  <p key={i} className="my-1 whitespace-pre-wrap wrap-anywhere text-ink-dim">
                    {child.text}
                  </p>
                ),
              )}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

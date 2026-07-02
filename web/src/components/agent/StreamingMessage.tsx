import { useCallback, useEffect, useState } from "react";
import type { AgentMessageAttachment } from "../../lib/agentApi";
import { AgentAvatar } from "./AgentAvatar";
import { MarkdownRenderer } from "./MarkdownRenderer";
import { ToolUseCard } from "./ToolUseCard";
import { AttachmentList } from "./MessageAttachments";
import { FilePathChip, splitFilePaths } from "./filePaths";
import { MediaOverlay } from "./MediaOverlay";
import { Lamp } from "../ui/Lamp";
import type { StreamingTool } from "./chatEventReducer";

// Quiet meta-row control: mono 11px ink-faint, hover ink. The `isUser`
// parameter is retained for call-site compatibility; the meta styling is
// uniform across both authors now.
export function actionBtnClass(_isUser: boolean): string {
  return "flex items-center gap-1 font-mono text-[11px] text-ink-faint transition-colors hover:text-ink";
}

/** Collapsible thinking/reasoning block */
export function ThinkingBlock({ text, streaming = false }: { text: string; streaming?: boolean }) {
  const [expanded, setExpanded] = useState(false);

  if (!text) return null;

  return (
    <div className="mb-2">
      <button
        onClick={() => setExpanded(!expanded)}
        className="flex items-center gap-1.5 font-mono text-[11px] text-ink-faint transition-colors hover:text-ink"
      >
        <svg
          className={`h-3 w-3 transition-transform ${expanded ? "rotate-90" : ""}`}
          fill="none"
          viewBox="0 0 24 24"
          stroke="currentColor"
          strokeWidth={2}
        >
          <path strokeLinecap="round" strokeLinejoin="round" d="M8.25 4.5l7.5 7.5-7.5 7.5" />
        </svg>
        {streaming ? (
          <span className="flex items-center gap-1.5">
            <Lamp state="warn" pulse size={5} />
            Thinking…
          </span>
        ) : (
          "Thought"
        )}
      </button>
      {expanded && (
        <div className="mt-1 rounded-[10px] border border-hairline bg-surface px-3 py-2 text-xs leading-relaxed text-ink-dim whitespace-pre-wrap wrap-anywhere">
          {text}
        </div>
      )}
    </div>
  );
}

/** Streaming assistant response in progress — flat (no bubble). */
interface StreamingMessageProps {
  text: string;
  thinking: string;
  toolUses: StreamingTool[];
  attachments?: AgentMessageAttachment[];
  agentName: string;
  agentId: string;
  status: string;
  avatarHash?: string;
  startTime: number;
  viewMode: "markdown" | "plain";
  onViewModeChange: (mode: "markdown" | "plain") => void;
}

export function StreamingMessage({
  text,
  thinking,
  toolUses,
  attachments,
  agentName,
  agentId,
  status,
  avatarHash,
  startTime,
  viewMode,
  onViewModeChange,
}: StreamingMessageProps) {
  const [preview, setPreview] = useState<{ path: string; type: "image" | "video" } | null>(null);

  const processText = useCallback(
    (t: string): React.ReactNode => {
      const segs = splitFilePaths(t);
      if (segs.length === 1 && segs[0].type === "text") return t;
      return segs.map((seg, i) =>
        seg.type === "text" ? (
          seg.value
        ) : (
          <FilePathChip key={i} path={seg.value} onPreview={setPreview} />
        ),
      );
    },
    [],
  );

  let activeTool: string | null = null;
  for (let i = toolUses.length - 1; i >= 0; i--) {
    if (toolUses[i].output === null) {
      activeTool = toolUses[i].name;
      break;
    }
  }

  const btnCls = actionBtnClass(false);

  return (
    <div className="flex gap-3">
      <AgentAvatar agentId={agentId} name={agentName} size="xs" className="mt-0.5" cacheBust={avatarHash} />
      <div className="min-w-0 flex-1">
        <div className="mb-1 flex items-center gap-2">
          <span className="text-[13px] font-semibold text-ink">{agentName}</span>
          <span className="flex items-center gap-1.5 font-mono text-[11px] text-ink-faint">
            <Lamp state="warn" pulse size={6} />
            <ElapsedTimer startTime={startTime} threshold={1} />
          </span>
        </div>

        {status === "thinking" && !text && !thinking && toolUses.length === 0 && (
          <div className="py-1 font-mono text-xs text-ink-faint">Thinking…</div>
        )}
        {thinking && <ThinkingBlock text={thinking} streaming={!text} />}
        {/* Streaming attachments — rendered as they arrive from kojo-attach */}
        {attachments && attachments.length > 0 && (
          <AttachmentList attachments={attachments} isUser={false} />
        )}
        {text && (
          <div className="relative text-[14px] text-ink">
            {viewMode === "markdown" ? (
              <MarkdownRenderer content={text} processText={processText} />
            ) : (
              <div className="whitespace-pre-wrap wrap-anywhere text-sm leading-relaxed">
                {text}
              </div>
            )}
            <span className="ml-0.5 inline-block h-4 w-0.5 align-text-bottom bg-copper animate-cursor-blink" />
          </div>
        )}
        {toolUses.length > 0 && (
          <div className="mt-2">
            {toolUses.map((tu, i) => (
              <ToolUseCard key={i} toolUse={{ ...tu, output: tu.output ?? "" }} />
            ))}
          </div>
        )}
        {/* Status bar: elapsed time + active tool + view toggle */}
        {(text || toolUses.length > 0) && (
          <div className="mt-1.5 flex items-center gap-2 font-mono text-[11px] text-ink-faint">
            {activeTool && (
              <span className="flex items-center gap-1.5">
                <Lamp state="warn" pulse size={5} />
                {activeTool}
              </span>
            )}
            {text && (
              <button
                onClick={() => onViewModeChange(viewMode === "markdown" ? "plain" : "markdown")}
                className={`${btnCls} ml-auto`}
                title={viewMode === "markdown" ? "Show plain text" : "Show rendered"}
              >
                {viewMode === "markdown" ? (
                  <>
                    <svg className="h-3 w-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                      <path strokeLinecap="round" strokeLinejoin="round" d="M17.25 6.75L22.5 12l-5.25 5.25m-10.5 0L1.5 12l5.25-5.25m7.5-3l-4.5 16.5" />
                    </svg>
                    Raw
                  </>
                ) : (
                  <>
                    <svg className="h-3 w-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                      <path strokeLinecap="round" strokeLinejoin="round" d="M19.5 14.25v-2.625a3.375 3.375 0 00-3.375-3.375h-1.5A1.125 1.125 0 0113.5 7.125v-1.5a3.375 3.375 0 00-3.375-3.375H8.25m0 12.75h7.5m-7.5 3H12M10.5 2.25H5.625c-.621 0-1.125.504-1.125 1.125v17.25c0 .621.504 1.125 1.125 1.125h12.75c.621 0 1.125-.504 1.125-1.125V11.25a9 9 0 00-9-9z" />
                    </svg>
                    Render
                  </>
                )}
              </button>
            )}
          </div>
        )}
      </div>
      {preview && (
        <MediaOverlay
          path={preview.path}
          type={preview.type}
          onClose={() => setPreview(null)}
        />
      )}
    </div>
  );
}

/** Self-contained ticking elapsed timer. Only this component re-renders each second. */
function ElapsedTimer({ startTime, threshold = 0, className }: { startTime: number; threshold?: number; className?: string }) {
  const [elapsed, setElapsed] = useState(() => Math.floor((Date.now() - startTime) / 1000));

  useEffect(() => {
    const timer = setInterval(() => {
      setElapsed(Math.floor((Date.now() - startTime) / 1000));
    }, 1000);
    return () => clearInterval(timer);
  }, [startTime]);

  if (elapsed < threshold) return null;
  return <span className={className}>{formatElapsed(elapsed)}</span>;
}

function formatElapsed(s: number): string {
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  const sec = s % 60;
  return `${m}m${sec > 0 ? `${sec}s` : ""}`;
}

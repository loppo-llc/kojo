import { memo, useState, useCallback, useEffect, useRef } from "react";
import type { AgentMessage, ToolUse } from "../../lib/agentApi";
import { ToolUseCard } from "./ToolUseCard";
import { AgentAvatar } from "./AgentAvatar";
import { MarkdownRenderer } from "./MarkdownRenderer";
import { formatTime } from "../../lib/utils";
import { SystemMessage } from "./SystemMessage";
import { AttachmentList } from "./MessageAttachments";
import { FilePathChip, splitFilePaths } from "./filePaths";
import { MediaOverlay } from "./MediaOverlay";
import { actionBtnClass, ThinkingBlock } from "./StreamingMessage";
import { estimateTurnCost } from "../../lib/pricing";

// Re-exported so existing importers (GroupDMChat, AgentChat, tests) keep a
// single "./ChatMessage" entry point even though the implementations now
// live in sibling files.
export { AttachmentList } from "./MessageAttachments";
export { MediaOverlay } from "./MediaOverlay";
export { splitFilePaths } from "./filePaths";
export { StreamingMessage } from "./StreamingMessage";

interface ChatMessageProps {
  message: AgentMessage;
  agentName: string;
  agentId: string;
  avatarHash?: string;
  // Agent's configured model (AgentInfo.model), threaded through so the
  // per-turn usage line can show an approximate cost when the model is priced.
  agentModel?: string;
  onEdit?: (msgId: string, content: string) => Promise<void>;
  onDelete?: (msgId: string) => Promise<void>;
  onRegenerate?: (msgId: string) => Promise<void>;
  // TTS controls — when ttsEnabled is true a small play/stop button is
  // rendered next to assistant messages. The parent owns the audio
  // element so navigating between messages cancels the previous one.
  ttsEnabled?: boolean;
  ttsPlayState?: "idle" | "loading" | "playing" | "error";
  onTTSPlay?: (msgId: string, text: string) => void;
}

export const ChatMessage = memo(function ChatMessage({
  message,
  agentName,
  agentId,
  avatarHash,
  agentModel,
  onEdit,
  onDelete,
  onRegenerate,
  ttsEnabled,
  ttsPlayState,
  onTTSPlay,
}: ChatMessageProps) {
  const isUser = message.role === "user";
  const isSystem = message.role === "system";

  if (isSystem) {
    return <SystemMessage message={message} />;
  }

  // User (own) message: right-aligned bubble with a copper-tinted border.
  if (isUser) {
    return (
      <div className="flex justify-end">
        <div className="min-w-0 max-w-[85%] rounded-2xl rounded-br-md border border-copper/25 bg-raised px-3.5 py-2.5 text-[14px] text-ink lg:max-w-[70%]">
          {message.attachments && message.attachments.length > 0 && (
            <AttachmentList attachments={message.attachments} isUser />
          )}
          <MessageContent
            messageId={message.id}
            content={message.content}
            isUser
            timestamp={message.timestamp}
            onEdit={onEdit}
            onDelete={onDelete}
            onRegenerate={onRegenerate}
          />
        </div>
      </div>
    );
  }

  // Agent / other message: flat (no bubble). Avatar left, header line
  // (name + time), content full width below.
  const formattedTime = formatTime(message.timestamp);
  return (
    <div className="flex gap-3">
      <AgentAvatar agentId={agentId} name={agentName} size="xs" className="mt-0.5" cacheBust={avatarHash} />
      <div className="min-w-0 flex-1">
        <div className="mb-1 flex items-baseline gap-2">
          <span className="text-[13px] font-semibold text-ink">{agentName}</span>
          {formattedTime && (
            <span className="font-mono text-[11px] text-ink-faint">{formattedTime}</span>
          )}
        </div>

        {message.thinking && <ThinkingBlock text={message.thinking} />}
        {message.attachments && message.attachments.length > 0 && (
          <AttachmentList attachments={message.attachments} isUser={false} />
        )}
        <div className="text-[14px] text-ink">
          <MessageContent
            messageId={message.id}
            content={message.content}
            isUser={false}
            timestamp={message.timestamp}
            showTime={false}
            onEdit={onEdit}
            onDelete={onDelete}
            onRegenerate={onRegenerate}
          />
        </div>

        {/* Tool uses — collapsed by default for completed messages */}
        {message.toolUses && message.toolUses.length > 0 && (
          <CollapsedToolUses toolUses={message.toolUses} />
        )}

        {/* Usage */}
        {message.usage && message.usage.inputTokens != null && (
          <UsageLine usage={message.usage} model={agentModel} />
        )}

        {/* TTS playback button — assistant messages only */}
        {ttsEnabled && message.content && onTTSPlay && (
          <TTSPlayButton
            state={ttsPlayState ?? "idle"}
            onClick={() => onTTSPlay(message.id, message.content)}
          />
        )}
      </div>
    </div>
  );
});

// UsageLine renders the per-turn token counts (input→output, plus cache
// read/write when present) and an approximate USD cost when the agent's
// model is priced. Same muted mono style as the original token line.
function UsageLine({
  usage,
  model,
}: {
  usage: NonNullable<AgentMessage["usage"]>;
  model?: string;
}) {
  const cacheRead = usage.cacheReadInputTokens ?? 0;
  const cacheWrite = usage.cacheCreationInputTokens ?? 0;
  const cost = estimateTurnCost(model, usage);
  return (
    <div className="mt-1 font-mono text-[11px] text-ink-faint">
      {usage.inputTokens.toLocaleString()}&rarr;{usage.outputTokens.toLocaleString()} tokens
      {(cacheRead > 0 || cacheWrite > 0) && (
        <>
          {" "}
          (cache {cacheRead.toLocaleString()}r/{cacheWrite.toLocaleString()}w)
        </>
      )}
      {cost != null && <> &middot; &asymp;${cost.toFixed(4)}</>}
    </div>
  );
}

// TTSPlayButton renders a tiny speaker / spinner / stop icon based on
// the current play state. Kept inline because it has no other consumers
// and depends on the parent's onClick + state shape.
function TTSPlayButton({
  state,
  onClick,
}: {
  state: "idle" | "loading" | "playing" | "error";
  onClick: () => void;
}) {
  const label =
    state === "loading"
      ? "Loading..."
      : state === "playing"
        ? "Stop"
        : state === "error"
          ? "TTS error — retry"
          : "Play";
  const colorClass =
    state === "error"
      ? "text-lamp-err hover:text-lamp-err"
      : state === "playing"
        ? "text-copper hover:text-copper-bright"
        : "text-ink-faint hover:text-ink";
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={state === "loading"}
      className={`mt-1 inline-flex items-center gap-1 font-mono text-[11px] transition-colors ${colorClass} disabled:opacity-50`}
      title={label}
    >
      {state === "loading" ? (
        <svg className="h-3.5 w-3.5 animate-spin" viewBox="0 0 24 24" fill="none">
          <circle cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="3" opacity="0.25" />
          <path d="M22 12a10 10 0 0 1-10 10" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
        </svg>
      ) : state === "playing" ? (
        <svg className="h-3.5 w-3.5" viewBox="0 0 20 20" fill="currentColor">
          <rect x="5" y="4" width="3" height="12" rx="1" />
          <rect x="12" y="4" width="3" height="12" rx="1" />
        </svg>
      ) : (
        <svg className="h-3.5 w-3.5" viewBox="0 0 20 20" fill="currentColor">
          <path d="M9.383 3.076A1 1 0 0 1 11 4v12a1 1 0 0 1-1.617.781L5.586 13H3a1 1 0 0 1-1-1V8a1 1 0 0 1 1-1h2.586l3.797-3.924z" />
          <path
            d="M14.657 5.343a1 1 0 0 1 1.414 0 6 6 0 0 1 0 9.314 1 1 0 1 1-1.414-1.414 4 4 0 0 0 0-6.486 1 1 0 0 1 0-1.414z"
            opacity={state === "error" ? 0.5 : 1}
          />
        </svg>
      )}
      <span>{label}</span>
    </button>
  );
}

/** Renders text with markdown or plain text, plus copy/toggle buttons */
export function MessageContent({
  messageId,
  content,
  isUser,
  timestamp,
  showTime = true,
  onEdit,
  onDelete,
  onRegenerate,
}: {
  messageId: string;
  content: string;
  isUser: boolean;
  timestamp: string;
  /** Show the timestamp in the meta row. Off when a header line owns it. */
  showTime?: boolean;
  onEdit?: (msgId: string, content: string) => Promise<void>;
  onDelete?: (msgId: string) => Promise<void>;
  onRegenerate?: (msgId: string) => Promise<void>;
}) {
  const [preview, setPreview] = useState<{ path: string; type: "image" | "video" } | null>(null);
  const [viewMode, setViewMode] = useState<"markdown" | "plain">("markdown");
  const [copied, setCopied] = useState(false);
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(content);
  const [saving, setSaving] = useState(false);
  const editRef = useRef<HTMLTextAreaElement>(null);

  useEffect(() => {
    if (!editing) return;
    const el = editRef.current;
    if (!el) return;
    el.focus();
    el.style.height = "auto";
    el.style.height = el.scrollHeight + "px";
    const len = el.value.length;
    el.setSelectionRange(len, len);
  }, [editing]);

  const processText = useCallback(
    (text: string): React.ReactNode => {
      const segs = splitFilePaths(text);
      if (segs.length === 1 && segs[0].type === "text") return text;
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

  const handleCopy = useCallback(() => {
    navigator.clipboard.writeText(content).then(
      () => {
        setCopied(true);
        setTimeout(() => setCopied(false), 1500);
      },
      () => {/* clipboard not available */},
    );
  }, [content]);

  const btnCls = actionBtnClass(isUser);
  const formattedTime = formatTime(timestamp);

  // Action buttons (copy + toggle) + timestamp
  const actionButtons = (
    <div
      className={`mt-1.5 flex items-center gap-2 ${
        isUser ? "justify-end" : "justify-start"
      }`}
    >
      {showTime && formattedTime && (
        <span className="font-mono text-[11px] text-ink-faint">
          {formattedTime}
        </span>
      )}
      {/* Copy button */}
      <button onClick={handleCopy} className={btnCls} title="Copy">
        {copied ? (
          <svg className="h-3 w-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M4.5 12.75l6 6 9-13.5" />
          </svg>
        ) : (
          <svg className="h-3 w-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M15.666 3.888A2.25 2.25 0 0013.5 2.25h-3c-1.03 0-1.9.693-2.166 1.638m7.332 0c.055.194.084.4.084.612v0a.75.75 0 01-.75.75H9.75a.75.75 0 01-.75-.75v0c0-.212.03-.418.084-.612m7.332 0c.646.049 1.288.11 1.927.184 1.1.128 1.907 1.077 1.907 2.185V19.5a2.25 2.25 0 01-2.25 2.25H6.75A2.25 2.25 0 014.5 19.5V6.257c0-1.108.806-2.057 1.907-2.185a48.208 48.208 0 011.927-.184" />
          </svg>
        )}
        {copied ? "Copied" : "Copy"}
      </button>

      {/* Plain/Markdown toggle */}
      {<button
        onClick={() => setViewMode(viewMode === "markdown" ? "plain" : "markdown")}
        className={btnCls}
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
      </button>}

      {/* Edit / Delete (llama.cpp only — parent gates via handler presence) */}
      {onEdit && (
        <button
          onClick={() => {
            setDraft(content);
            setEditing(true);
          }}
          className={btnCls}
          title="Edit"
        >
          <svg className="h-3 w-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M16.862 4.487l1.687-1.688a1.875 1.875 0 112.652 2.652L10.582 16.07a4.5 4.5 0 01-1.897 1.13L6 18l.8-2.685a4.5 4.5 0 011.13-1.897l8.932-8.931zm0 0L19.5 7.125" />
          </svg>
          Edit
        </button>
      )}
      {onDelete && (
        <button
          onClick={async () => {
            if (!window.confirm("Delete this message?")) return;
            try {
              await onDelete(messageId);
            } catch (e) {
              console.error(e);
              alert("Failed to delete message");
            }
          }}
          className={btnCls}
          title="Delete"
        >
          <svg className="h-3 w-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M14.74 9l-.346 9m-4.788 0L9.26 9m9.968-3.21c.342.052.682.107 1.022.166m-1.022-.165L18.16 19.673a2.25 2.25 0 01-2.244 2.077H8.084a2.25 2.25 0 01-2.244-2.077L4.772 5.79m14.456 0a48.108 48.108 0 00-3.478-.397m-12 .562c.34-.059.68-.114 1.022-.165m0 0a48.11 48.11 0 013.478-.397m7.5 0v-.916c0-1.18-.91-2.164-2.09-2.201a51.964 51.964 0 00-3.32 0c-1.18.037-2.09 1.022-2.09 2.201v.916m7.5 0a48.667 48.667 0 00-7.5 0" />
          </svg>
          Delete
        </button>
      )}
      {onRegenerate && (
        <button
          onClick={async () => {
            // Parent surfaces errors inline; just await here.
            await onRegenerate(messageId);
          }}
          className={btnCls}
          title="Regenerate"
        >
          <svg className="h-3 w-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M16.023 9.348h4.992v-.001M2.985 19.644v-4.992m0 0h4.992m-4.993 0l3.181 3.183a8.25 8.25 0 0013.803-3.7M4.031 9.865a8.25 8.25 0 0113.803-3.7l3.181 3.182m0-4.991v4.99" />
          </svg>
          Regenerate
        </button>
      )}
    </div>
  );

  // Edit mode
  if (editing) {
    const handleSave = async () => {
      if (!onEdit || saving) return;
      setSaving(true);
      try {
        await onEdit(messageId, draft);
        setEditing(false);
      } catch (e) {
        console.error(e);
        alert("Failed to save message");
      } finally {
        setSaving(false);
      }
    };
    const handleCancel = () => {
      setEditing(false);
      setDraft(content);
    };
    return (
      <>
        <textarea
          ref={editRef}
          value={draft}
          onChange={(e) => {
            setDraft(e.target.value);
            const el = e.currentTarget;
            el.style.height = "auto";
            el.style.height = el.scrollHeight + "px";
          }}
          onKeyDown={(e) => {
            if (e.key === "Escape") handleCancel();
            if (e.key === "Enter" && !e.nativeEvent.isComposing && (e.metaKey || e.ctrlKey)) {
              e.preventDefault();
              handleSave();
            }
          }}
          className="w-full min-w-[200px] resize-none rounded-[10px] border border-hairline bg-app px-2 py-1.5 text-sm text-ink focus:border-copper focus:outline-none"
          rows={1}
        />
        <div className={`mt-1.5 flex items-center gap-2 ${isUser ? "justify-end" : "justify-start"}`}>
          <button onClick={handleCancel} disabled={saving} className={btnCls}>
            Cancel
          </button>
          <button
            onClick={handleSave}
            disabled={saving || draft === content}
            className={`${btnCls} disabled:opacity-40`}
          >
            {saving ? "Saving..." : "Save"}
          </button>
        </div>
      </>
    );
  }

  // Plain text mode
  if (viewMode === "plain") {
    return (
      <>
        <div className="whitespace-pre-wrap wrap-anywhere text-sm leading-relaxed">
          {content}
        </div>
        {actionButtons}
      </>
    );
  }

  // Markdown mode (with optional file path chips)
  return (
    <>
      <div className={isUser ? "md-content-user" : ""}>
        <MarkdownRenderer content={content} processText={processText} />
      </div>
      {actionButtons}
      {preview && (
        <MediaOverlay
          path={preview.path}
          type={preview.type}
          onClose={() => setPreview(null)}
        />
      )}
    </>
  );
}

/** Collapsed tool uses summary for completed messages */
export function CollapsedToolUses({ toolUses }: { toolUses: ToolUse[] }) {
  const [expanded, setExpanded] = useState(false);

  // Deduplicate tool names for summary
  const toolCounts = new Map<string, number>();
  for (const tu of toolUses) {
    toolCounts.set(tu.name, (toolCounts.get(tu.name) ?? 0) + 1);
  }
  const summary = [...toolCounts.entries()]
    .map(([name, count]) => count > 1 ? `${name} x${count}` : name)
    .join(", ");

  return (
    <div className="mt-2">
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
        <svg className="h-3 w-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
          <path strokeLinecap="round" strokeLinejoin="round" d="M11.42 15.17L17.25 21A2.652 2.652 0 0021 17.25l-5.877-5.877M11.42 15.17l2.496-3.03c.317-.384.74-.626 1.208-.766M11.42 15.17l-4.655 5.653a2.548 2.548 0 11-3.586-3.586l6.837-5.63m5.108-.233c.55-.164 1.163-.188 1.743-.14a4.5 4.5 0 004.486-6.336l-3.276 3.277a3.004 3.004 0 01-2.25-2.25l3.276-3.276a4.5 4.5 0 00-6.336 4.486c.091 1.076-.071 2.264-.904 2.95l-.102.085m-1.745 1.437L5.909 7.5H4.5L2.25 3.75l1.5-1.5L7.5 4.5v1.409l4.26 4.26m-1.745 1.437l1.745-1.437m6.615 8.206L15.75 15.75M4.867 19.125h.008v.008h-.008v-.008z" />
        </svg>
        <span>{toolUses.length} tool{toolUses.length > 1 ? "s" : ""}</span>
        {!expanded && <span className="max-w-[200px] truncate text-ink-faint">{summary}</span>}
      </button>
      {expanded && (
        <div className="mt-1">
          {toolUses.map((tu, i) => (
            <ToolUseCard key={i} toolUse={tu} />
          ))}
        </div>
      )}
    </div>
  );
}

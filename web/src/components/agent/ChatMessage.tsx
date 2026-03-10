import { memo, useState, useCallback, useMemo, useEffect } from "react";
import type { AgentMessage, AgentMessageAttachment } from "../../lib/agentApi";
import { ToolUseCard } from "./ToolUseCard";
import { AgentAvatar } from "./AgentAvatar";
import { MarkdownRenderer } from "./MarkdownRenderer";

// File extensions that can be previewed
const IMAGE_EXTS = /\.(png|jpe?g|gif|webp|svg|bmp|ico|avif)$/i;
// Match absolute file paths (Unix or Windows-style) with media extensions
// Unix: starts with /, Windows: starts with drive letter like C:\
// Path segments: exclude only delimiters that indicate end-of-path (comma, semicolon, quotes, newline, slash).
// This allows Unicode, spaces, parens, apostrophes, etc. in filenames.
const MEDIA_PATH_RE =
  /(?:(?:\/[^,;:"<>\n/][^,;:"<>\n/]*)+|[A-Z]:\\(?:[^,;:"<>\n\\]+\\)*[^,;:"<>\n\\]+)\.(png|jpe?g|gif|webp|svg|bmp|ico|avif|mp4|webm|mov|avi|mkv)\b/gi;

interface ChatMessageProps {
  message: AgentMessage;
  agentName: string;
  agentId: string;
  avatarHash?: string;
}

export const ChatMessage = memo(function ChatMessage({
  message,
  agentName,
  agentId,
  avatarHash,
}: ChatMessageProps) {
  const isUser = message.role === "user";
  const isSystem = message.role === "system";

  if (isSystem) {
    return <SystemMessage message={message} />;
  }

  return (
    <div className={`flex gap-3 ${isUser ? "flex-row-reverse" : "flex-row"}`}>
      {/* Avatar */}
      {!isUser && (
        <AgentAvatar agentId={agentId} name={agentName} size="sm" className="mt-1" cacheBust={avatarHash} />
      )}

      {/* Bubble */}
      <div
        className={`max-w-[80%] ${
          isUser
            ? "bg-blue-600/90 text-white rounded-2xl rounded-tr-sm"
            : "bg-neutral-800/80 text-neutral-200 rounded-2xl rounded-tl-sm"
        } px-3.5 py-2.5`}
      >
        {!isUser && message.thinking && <ThinkingBlock text={message.thinking} />}
        {/* Attachments */}
        {message.attachments && message.attachments.length > 0 && (
          <AttachmentList attachments={message.attachments} isUser={isUser} />
        )}
        <MessageContent content={message.content} isUser={isUser} timestamp={message.timestamp} />

        {/* Tool uses */}
        {message.toolUses && message.toolUses.length > 0 && (
          <div className="mt-2">
            {message.toolUses.map((tu, i) => (
              <ToolUseCard key={i} toolUse={tu} />
            ))}
          </div>
        )}

        {/* Usage */}
        {message.usage && (
          <div className="text-[10px] text-neutral-500 mt-1 font-mono">
            {message.usage.inputTokens.toLocaleString()}&rarr;{message.usage.outputTokens.toLocaleString()} tokens
          </div>
        )}

      </div>
    </div>
  );
});

/** Display file attachments on a message */
function AttachmentList({ attachments, isUser }: { attachments: AgentMessageAttachment[]; isUser: boolean }) {
  const [preview, setPreview] = useState<{ path: string; type: "image" | "video" } | null>(null);

  return (
    <>
      <div className="flex flex-wrap gap-1.5 mb-2">
        {attachments.map((att) => {
          const isImage = att.mime.startsWith("image/");
          if (isImage) {
            return (
              <button
                key={att.path}
                onClick={() => setPreview({ path: att.path, type: "image" })}
                className="block rounded-lg overflow-hidden hover:opacity-80 transition-opacity"
              >
                <img
                  src={`/api/v1/files/raw?path=${encodeURIComponent(att.path)}`}
                  alt={att.name}
                  className="max-w-[200px] max-h-[150px] object-cover rounded-lg"
                />
              </button>
            );
          }
          return (
            <div
              key={att.path}
              className={`flex items-center gap-1.5 px-2 py-1 rounded-lg text-xs ${
                isUser ? "bg-blue-500/30" : "bg-neutral-700/50"
              }`}
            >
              <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-4 h-4 opacity-60">
                <path d="M3 3.5A1.5 1.5 0 014.5 2h6.879a1.5 1.5 0 011.06.44l4.122 4.12A1.5 1.5 0 0117 7.622V16.5a1.5 1.5 0 01-1.5 1.5h-11A1.5 1.5 0 013 16.5v-13z" />
              </svg>
              <span className="max-w-[150px] truncate">{att.name}</span>
              <span className="opacity-50">{formatFileSize(att.size)}</span>
            </div>
          );
        })}
      </div>
      {preview && (
        <MediaOverlay path={preview.path} type={preview.type} onClose={() => setPreview(null)} />
      )}
    </>
  );
}

function formatFileSize(bytes: number): string {
  if (bytes < 1024) return `${bytes}B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(0)}KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)}MB`;
}

// Match "[Group DM: <name>] New message from <sender> at <timestamp>."
// Uses greedy match up to "] New message from" to handle "]" in group names.
const GROUP_DM_RE = /^\[Group DM: (.+)\] New message from (.+?)(?:\s+at\s+\S+)?\.?\n/;

/** System / error messages -- centered, distinct styling */
function SystemMessage({ message }: { message: AgentMessage }) {
  const isError = message.content.startsWith("\u26a0\ufe0f Error:");

  // Compact rendering for group DM notifications
  const gdmMatch = !isError && GROUP_DM_RE.exec(message.content);
  if (gdmMatch) {
    const [, groupName, sender] = gdmMatch;
    return (
      <div className="flex justify-center my-1.5">
        <div className="flex items-center gap-1.5 px-3 py-1 rounded-full bg-neutral-900/60 border border-neutral-800 text-[11px] text-neutral-500">
          <svg className="w-3 h-3 shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M20.25 8.511c.884.284 1.5 1.128 1.5 2.097v4.286c0 1.136-.847 2.1-1.98 2.193-.34.027-.68.052-1.02.072v3.091l-3-3c-1.354 0-2.694-.055-4.02-.163a2.115 2.115 0 01-.825-.242m9.345-8.334a2.126 2.126 0 00-.476-.095 48.64 48.64 0 00-8.048 0c-1.131.094-1.976 1.057-1.976 2.192v4.286c0 .837.46 1.58 1.155 1.951m9.345-8.334V6.637c0-1.621-1.152-3.026-2.76-3.235A48.455 48.455 0 0011.25 3c-2.115 0-4.198.137-6.24.402-1.608.209-2.76 1.614-2.76 3.235v6.226c0 1.621 1.152 3.026 2.76 3.235.577.075 1.157.14 1.74.194V21l4.155-4.155" />
          </svg>
          <span><span className="text-neutral-400">{sender}</span> &rarr; {groupName}</span>
          <span className="text-neutral-600">{formatTime(message.timestamp)}</span>
        </div>
      </div>
    );
  }

  const content = isError
    ? message.content.replace(/^\u26a0\ufe0f Error:\s*/, "")
    : message.content;

  return (
    <div className="flex justify-center my-2">
      <div
        className={`max-w-[90%] px-4 py-2.5 rounded-lg text-xs leading-relaxed ${
          isError
            ? "bg-red-950/50 border border-red-900/50 text-red-300"
            : "bg-neutral-900/60 border border-neutral-800 text-neutral-400"
        }`}
      >
        <div className="flex items-start gap-2">
          {isError ? (
            <svg
              className="w-4 h-4 text-red-400 shrink-0 mt-0.5"
              fill="none"
              viewBox="0 0 24 24"
              stroke="currentColor"
              strokeWidth={2}
            >
              <path
                strokeLinecap="round"
                strokeLinejoin="round"
                d="M12 9v3.75m-9.303 3.376c-.866 1.5.217 3.374 1.948 3.374h14.71c1.73 0 2.813-1.874 1.948-3.374L13.949 3.378c-.866-1.5-3.032-1.5-3.898 0L2.697 16.126zM12 15.75h.007v.008H12v-.008z"
              />
            </svg>
          ) : (
            <svg
              className="w-4 h-4 text-neutral-500 shrink-0 mt-0.5"
              fill="none"
              viewBox="0 0 24 24"
              stroke="currentColor"
              strokeWidth={2}
            >
              <path
                strokeLinecap="round"
                strokeLinejoin="round"
                d="M11.25 11.25l.041-.02a.75.75 0 011.063.852l-.708 2.836a.75.75 0 001.063.853l.041-.021M21 12a9 9 0 11-18 0 9 9 0 0118 0zm-9-3.75h.008v.008H12V8.25z"
              />
            </svg>
          )}
          <span className="whitespace-pre-wrap break-words">{content}</span>
        </div>
        <div className="text-[10px] text-neutral-600 mt-1.5 text-right">
          {formatTime(message.timestamp)}
        </div>
      </div>
    </div>
  );
}

function actionBtnClass(isUser: boolean): string {
  return `flex items-center gap-1 px-1.5 py-0.5 rounded text-[10px] transition-colors ${
    isUser
      ? "text-blue-200/50 hover:text-blue-100 hover:bg-blue-500/20"
      : "text-neutral-500 hover:text-neutral-300 hover:bg-neutral-700/50"
  }`;
}

/** Renders text with markdown or plain text, plus copy/toggle buttons */
function MessageContent({ content, isUser, timestamp }: { content: string; isUser: boolean; timestamp: string }) {
  const [preview, setPreview] = useState<{ path: string; type: "image" | "video" } | null>(null);
  const [viewMode, setViewMode] = useState<"markdown" | "plain">("markdown");
  const [copied, setCopied] = useState(false);

  const parts = useMemo(() => splitMediaPaths(content), [content]);
  const hasMedia = parts.length > 1 || (parts.length === 1 && parts[0].type === "media");

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
      className={`flex items-center gap-0.5 mt-1.5 ${
        isUser ? "justify-end" : "justify-start"
      }`}
    >
      {formattedTime && (
        <span className={`text-[10px] mr-1 ${isUser ? "text-blue-200/70" : "text-neutral-500"}`}>
          {formattedTime}
        </span>
      )}
      {/* Copy button */}
      <button onClick={handleCopy} className={btnCls} title="Copy">
        {copied ? (
          <svg className="w-3 h-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M4.5 12.75l6 6 9-13.5" />
          </svg>
        ) : (
          <svg className="w-3 h-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M15.666 3.888A2.25 2.25 0 0013.5 2.25h-3c-1.03 0-1.9.693-2.166 1.638m7.332 0c.055.194.084.4.084.612v0a.75.75 0 01-.75.75H9.75a.75.75 0 01-.75-.75v0c0-.212.03-.418.084-.612m7.332 0c.646.049 1.288.11 1.927.184 1.1.128 1.907 1.077 1.907 2.185V19.5a2.25 2.25 0 01-2.25 2.25H6.75A2.25 2.25 0 014.5 19.5V6.257c0-1.108.806-2.057 1.907-2.185a48.208 48.208 0 011.927-.184" />
          </svg>
        )}
        {copied ? "Copied" : "Copy"}
      </button>

      {/* Plain/Markdown toggle */}
      <button
        onClick={() => setViewMode(viewMode === "markdown" ? "plain" : "markdown")}
        className={btnCls}
        title={viewMode === "markdown" ? "Show plain text" : "Show rendered"}
      >
        {viewMode === "markdown" ? (
          <>
            <svg className="w-3 h-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
              <path strokeLinecap="round" strokeLinejoin="round" d="M17.25 6.75L22.5 12l-5.25 5.25m-10.5 0L1.5 12l5.25-5.25m7.5-3l-4.5 16.5" />
            </svg>
            Raw
          </>
        ) : (
          <>
            <svg className="w-3 h-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
              <path strokeLinecap="round" strokeLinejoin="round" d="M19.5 14.25v-2.625a3.375 3.375 0 00-3.375-3.375h-1.5A1.125 1.125 0 0113.5 7.125v-1.5a3.375 3.375 0 00-3.375-3.375H8.25m0 12.75h7.5m-7.5 3H12M10.5 2.25H5.625c-.621 0-1.125.504-1.125 1.125v17.25c0 .621.504 1.125 1.125 1.125h12.75c.621 0 1.125-.504 1.125-1.125V11.25a9 9 0 00-9-9z" />
            </svg>
            Render
          </>
        )}
      </button>
    </div>
  );

  // Plain text mode or media-containing messages
  if (viewMode === "plain" || hasMedia) {
    return (
      <>
        {hasMedia ? (
          <MediaTextContent parts={parts} onPreview={setPreview} />
        ) : (
          <div className="text-sm whitespace-pre-wrap break-words leading-relaxed">
            {content}
          </div>
        )}
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

  // Markdown mode
  return (
    <>
      <div className={isUser ? "md-content-user" : ""}>
        <MarkdownRenderer content={content} />
      </div>
      {actionButtons}
    </>
  );
}

/** Text with clickable media file paths */
export function MediaTextContent({
  parts,
  onPreview,
}: {
  parts: Array<{ type: "text" | "media"; value: string }>;
  onPreview: (p: { path: string; type: "image" | "video" }) => void;
}) {
  return (
    <div className="text-sm whitespace-pre-wrap break-words leading-relaxed">
      {parts.map((part, i) => {
        if (part.type === "text") return <span key={i}>{part.value}</span>;
        const isImage = IMAGE_EXTS.test(part.value);
        return (
          <button
            key={i}
            onClick={() =>
              onPreview({ path: part.value, type: isImage ? "image" : "video" })
            }
            className="inline-flex items-center gap-1 px-1.5 py-0.5 mx-0.5 bg-neutral-700/50 hover:bg-neutral-600/50 rounded text-xs font-mono text-blue-300 hover:text-blue-200 transition-colors max-w-full min-w-0"
            title={`Preview ${part.value}`}
          >
            {isImage ? (
              <svg className="w-3 h-3 shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                <path strokeLinecap="round" strokeLinejoin="round" d="M2.25 15.75l5.159-5.159a2.25 2.25 0 013.182 0l5.159 5.159m-1.5-1.5l1.409-1.409a2.25 2.25 0 013.182 0l2.909 2.909M3.75 21h16.5A2.25 2.25 0 0022.5 18.75V5.25A2.25 2.25 0 0020.25 3H3.75A2.25 2.25 0 001.5 5.25v13.5A2.25 2.25 0 003.75 21z" />
              </svg>
            ) : (
              <svg className="w-3 h-3 shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                <path strokeLinecap="round" strokeLinejoin="round" d="M5.25 5.653c0-.856.917-1.398 1.667-.986l11.54 6.348a1.125 1.125 0 010 1.971l-11.54 6.347a1.125 1.125 0 01-1.667-.985V5.653z" />
              </svg>
            )}
            <span className="min-w-0 overflow-hidden text-ellipsis whitespace-nowrap">{part.value.split(/[/\\]/).pop()}</span>
          </button>
        );
      })}
    </div>
  );
}

/** Full-screen overlay for image/video preview */
export function MediaOverlay({
  path,
  type,
  onClose,
}: {
  path: string;
  type: "image" | "video";
  onClose: () => void;
}) {
  const rawUrl = `/api/v1/files/raw?path=${encodeURIComponent(path)}`;

  const handleBackdrop = useCallback(
    (e: React.MouseEvent) => {
      if (e.target === e.currentTarget) onClose();
    },
    [onClose],
  );

  return (
    <div
      className="fixed inset-0 z-50 bg-black/80 backdrop-blur-sm flex items-center justify-center p-4"
      onClick={handleBackdrop}
    >
      <div className="relative max-w-[90vw] max-h-[90vh]">
        <button
          onClick={onClose}
          className="absolute -top-3 -right-3 w-8 h-8 bg-neutral-800 hover:bg-neutral-700 rounded-full flex items-center justify-center text-neutral-300 hover:text-white z-10 shadow-lg"
        >
          <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M6 18L18 6M6 6l12 12" />
          </svg>
        </button>

        {type === "image" ? (
          <img
            src={rawUrl}
            alt={path}
            className="max-w-[90vw] max-h-[85vh] object-contain rounded-lg shadow-2xl"
          />
        ) : (
          <video
            src={rawUrl}
            controls
            autoPlay
            className="max-w-[90vw] max-h-[85vh] rounded-lg shadow-2xl"
          />
        )}

        <div className="text-center mt-2 text-xs text-neutral-400 font-mono truncate">
          {path}
        </div>
      </div>
    </div>
  );
}

/** Split text into text parts and media file path parts */
export function splitMediaPaths(text: string): Array<{ type: "text" | "media"; value: string }> {
  const parts: Array<{ type: "text" | "media"; value: string }> = [];
  let lastIndex = 0;

  // Reset regex state
  MEDIA_PATH_RE.lastIndex = 0;
  let match;
  while ((match = MEDIA_PATH_RE.exec(text)) !== null) {
    if (match.index > lastIndex) {
      parts.push({ type: "text", value: text.slice(lastIndex, match.index) });
    }
    parts.push({ type: "media", value: match[0] });
    lastIndex = match.index + match[0].length;
  }

  if (lastIndex < text.length) {
    parts.push({ type: "text", value: text.slice(lastIndex) });
  }

  return parts.length > 0 ? parts : [{ type: "text", value: text }];
}

/** Collapsible thinking/reasoning block */
function ThinkingBlock({ text, streaming = false }: { text: string; streaming?: boolean }) {
  const [expanded, setExpanded] = useState(false);

  if (!text) return null;

  return (
    <div className="mb-2">
      <button
        onClick={() => setExpanded(!expanded)}
        className="flex items-center gap-1.5 text-[11px] text-neutral-500 hover:text-neutral-400 transition-colors"
      >
        <svg
          className={`w-3 h-3 transition-transform ${expanded ? "rotate-90" : ""}`}
          fill="none"
          viewBox="0 0 24 24"
          stroke="currentColor"
          strokeWidth={2}
        >
          <path strokeLinecap="round" strokeLinejoin="round" d="M8.25 4.5l7.5 7.5-7.5 7.5" />
        </svg>
        {streaming ? (
          <span className="flex items-center gap-1">
            <span className="w-1 h-1 bg-neutral-500 rounded-full animate-pulse" />
            Thinking…
          </span>
        ) : (
          "Thought"
        )}
      </button>
      {expanded && (
        <div className="mt-1 pl-4 border-l border-neutral-700/50 text-xs text-neutral-500 leading-relaxed whitespace-pre-wrap">
          {text}
        </div>
      )}
    </div>
  );
}

/** Streaming bubble for assistant response in progress */
interface StreamingMessageProps {
  text: string;
  thinking: string;
  toolUses: Array<{ name: string; input: string; output: string | null }>;
  agentName: string;
  agentId: string;
  status: string;
  avatarHash?: string;
  startTime: number;
}

export function StreamingMessage({
  text,
  thinking,
  toolUses,
  agentName,
  agentId,
  status,
  avatarHash,
  startTime,
}: StreamingMessageProps) {

  let activeTool: string | null = null;
  for (let i = toolUses.length - 1; i >= 0; i--) {
    if (toolUses[i].output === null) {
      activeTool = toolUses[i].name;
      break;
    }
  }

  return (
    <div className="flex gap-3 flex-row">
      <AgentAvatar agentId={agentId} name={agentName} size="sm" className="mt-1" cacheBust={avatarHash} />
      <div className="max-w-[80%] bg-neutral-800/80 text-neutral-200 rounded-2xl rounded-tl-sm px-3.5 py-2.5">
        {status === "thinking" && !text && !thinking && toolUses.length === 0 && (
          <div className="flex items-center gap-1.5 py-1">
            <span className="w-1.5 h-1.5 bg-neutral-400 rounded-full animate-bounce" style={{ animationDelay: "0ms" }} />
            <span className="w-1.5 h-1.5 bg-neutral-400 rounded-full animate-bounce" style={{ animationDelay: "150ms" }} />
            <span className="w-1.5 h-1.5 bg-neutral-400 rounded-full animate-bounce" style={{ animationDelay: "300ms" }} />
            <ElapsedTimer startTime={startTime} threshold={3} className="text-xs text-neutral-500 ml-2" />
          </div>
        )}
        {thinking && <ThinkingBlock text={thinking} streaming={!text} />}
        {text && (
          <div className="relative">
            <MarkdownRenderer content={text} />
            <span className="inline-block w-0.5 h-4 bg-neutral-400 animate-pulse ml-0.5 align-text-bottom" />
          </div>
        )}
        {toolUses.length > 0 && (
          <div className="mt-2">
            {toolUses.map((tu, i) => (
              <ToolUseCard key={i} toolUse={{ ...tu, output: tu.output ?? "" }} />
            ))}
          </div>
        )}
        {/* Status bar: elapsed time + active tool */}
        {(text || toolUses.length > 0) && (
          <div className="flex items-center gap-2 mt-1.5 text-[10px] text-neutral-500">
            <ElapsedTimer startTime={startTime} className="" />
            {activeTool && (
              <span className="flex items-center gap-1">
                <span className="w-1 h-1 bg-blue-400 rounded-full animate-pulse" />
                {activeTool}
              </span>
            )}
          </div>
        )}
      </div>
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

function formatTime(timestamp: string): string {
  try {
    const d = new Date(timestamp);
    const now = new Date();
    const isToday =
      d.getDate() === now.getDate() &&
      d.getMonth() === now.getMonth() &&
      d.getFullYear() === now.getFullYear();
    if (isToday) {
      return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    }
    return d.toLocaleDateString([], {
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
    });
  } catch {
    return "";
  }
}

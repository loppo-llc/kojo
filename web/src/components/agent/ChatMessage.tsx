import { useState, useCallback, useMemo } from "react";
import type { AgentMessage } from "../../lib/agentApi";
import { ToolUseCard } from "./ToolUseCard";
import { AgentAvatar } from "./AgentAvatar";

// File extensions that can be previewed
const IMAGE_EXTS = /\.(png|jpe?g|gif|webp|svg|bmp|ico|avif)$/i;
// Match absolute file paths (Unix or Windows-style) with media extensions
const MEDIA_PATH_RE =
  /(?:(?:\/[\w.@~+-]+)+|[A-Z]:\\(?:[\w.@~+ -]+\\)*[\w.@~+ -]+)\.(png|jpe?g|gif|webp|svg|bmp|ico|avif|mp4|webm|mov|avi|mkv)\b/gi;

interface ChatMessageProps {
  message: AgentMessage;
  agentName: string;
  agentId: string;
}

export function ChatMessage({ message, agentName, agentId }: ChatMessageProps) {
  const isUser = message.role === "user";
  const isSystem = message.role === "system";

  if (isSystem) {
    return <SystemMessage message={message} />;
  }

  return (
    <div className={`flex gap-3 ${isUser ? "flex-row-reverse" : "flex-row"}`}>
      {/* Avatar */}
      {!isUser && (
        <AgentAvatar agentId={agentId} name={agentName} size="sm" className="mt-1" />
      )}

      {/* Bubble */}
      <div
        className={`max-w-[80%] ${
          isUser
            ? "bg-blue-600/90 text-white rounded-2xl rounded-tr-sm"
            : "bg-neutral-800/80 text-neutral-200 rounded-2xl rounded-tl-sm"
        } px-3.5 py-2.5`}
      >
        <MessageContent content={message.content} />

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
            {message.usage.inputTokens.toLocaleString()}→{message.usage.outputTokens.toLocaleString()} tokens
          </div>
        )}

        {/* Timestamp */}
        <div
          className={`text-[10px] mt-1 ${
            isUser ? "text-blue-200/70" : "text-neutral-500"
          }`}
        >
          {formatTime(message.timestamp)}
        </div>
      </div>
    </div>
  );
}

/** System / error messages — centered, distinct styling */
function SystemMessage({ message }: { message: AgentMessage }) {
  const isError = message.content.startsWith("\u26a0\ufe0f Error:");
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

/** Renders text with clickable media file paths */
function MessageContent({ content }: { content: string }) {
  const [preview, setPreview] = useState<{ path: string; type: "image" | "video" } | null>(null);

  const parts = useMemo(() => splitMediaPaths(content), [content]);

  if (parts.length === 1 && parts[0].type === "text") {
    return (
      <div className="text-sm whitespace-pre-wrap break-words leading-relaxed">
        {content}
      </div>
    );
  }

  return (
    <>
      <div className="text-sm whitespace-pre-wrap break-words leading-relaxed">
        {parts.map((part, i) => {
          if (part.type === "text") return <span key={i}>{part.value}</span>;
          const isImage = IMAGE_EXTS.test(part.value);
          return (
            <button
              key={i}
              onClick={() =>
                setPreview({ path: part.value, type: isImage ? "image" : "video" })
              }
              className="inline-flex items-center gap-1 px-1.5 py-0.5 mx-0.5 bg-neutral-700/50 hover:bg-neutral-600/50 rounded text-xs font-mono text-blue-300 hover:text-blue-200 transition-colors"
              title={`Preview ${part.value}`}
            >
              {isImage ? (
                <svg className="w-3 h-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                  <path strokeLinecap="round" strokeLinejoin="round" d="M2.25 15.75l5.159-5.159a2.25 2.25 0 013.182 0l5.159 5.159m-1.5-1.5l1.409-1.409a2.25 2.25 0 013.182 0l2.909 2.909M3.75 21h16.5A2.25 2.25 0 0022.5 18.75V5.25A2.25 2.25 0 0020.25 3H3.75A2.25 2.25 0 001.5 5.25v13.5A2.25 2.25 0 003.75 21z" />
                </svg>
              ) : (
                <svg className="w-3 h-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                  <path strokeLinecap="round" strokeLinejoin="round" d="M5.25 5.653c0-.856.917-1.398 1.667-.986l11.54 6.348a1.125 1.125 0 010 1.971l-11.54 6.347a1.125 1.125 0 01-1.667-.985V5.653z" />
                </svg>
              )}
              {part.value.split("/").pop()}
            </button>
          );
        })}
      </div>

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

/** Full-screen overlay for image/video preview */
function MediaOverlay({
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
function splitMediaPaths(text: string): Array<{ type: "text" | "media"; value: string }> {
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

/** Streaming bubble for assistant response in progress */
interface StreamingMessageProps {
  text: string;
  toolUses: Array<{ name: string; input: string; output: string }>;
  agentName: string;
  agentId: string;
  status: string;
}

export function StreamingMessage({
  text,
  toolUses,
  agentName,
  agentId,
  status,
}: StreamingMessageProps) {
  return (
    <div className="flex gap-3 flex-row">
      <AgentAvatar agentId={agentId} name={agentName} size="sm" className="mt-1" />
      <div className="max-w-[80%] bg-neutral-800/80 text-neutral-200 rounded-2xl rounded-tl-sm px-3.5 py-2.5">
        {status === "thinking" && !text && toolUses.length === 0 && (
          <div className="flex items-center gap-1.5 py-1">
            <span className="w-1.5 h-1.5 bg-neutral-400 rounded-full animate-bounce" style={{ animationDelay: "0ms" }} />
            <span className="w-1.5 h-1.5 bg-neutral-400 rounded-full animate-bounce" style={{ animationDelay: "150ms" }} />
            <span className="w-1.5 h-1.5 bg-neutral-400 rounded-full animate-bounce" style={{ animationDelay: "300ms" }} />
          </div>
        )}
        {text && (
          <div className="text-sm whitespace-pre-wrap break-words leading-relaxed">
            {text}
            <span className="inline-block w-0.5 h-4 bg-neutral-400 animate-pulse ml-0.5 align-text-bottom" />
          </div>
        )}
        {toolUses.length > 0 && (
          <div className="mt-2">
            {toolUses.map((tu, i) => (
              <ToolUseCard key={i} toolUse={tu} />
            ))}
          </div>
        )}
      </div>
    </div>
  );
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

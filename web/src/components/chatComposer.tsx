// Presentational pieces shared by AgentChat and GroupDMChat composers.
// The composer chrome (attach / send / stop buttons, pending-file chips,
// error banner) and the "load older messages" pager live here so both chat
// surfaces render byte-identical controls. The only parametrized difference
// is PendingAttachments' `thumb` (AgentChat previews via the thumbnail
// endpoint, GroupDMChat loads the raw blob).

import type { AgentMessageAttachment } from "../lib/agentApi";
import { api, isThumbSupported } from "../lib/api";

/** The repeated 16x16 "×" glyph used on dismiss/remove buttons. */
export function CloseIcon() {
  return (
    <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" className="w-3 h-3">
      <path d="M5.28 4.22a.75.75 0 00-1.06 1.06L6.94 8l-2.72 2.72a.75.75 0 101.06 1.06L8 9.06l2.72 2.72a.75.75 0 101.06-1.06L9.06 8l2.72-2.72a.75.75 0 00-1.06-1.06L8 6.94 5.28 4.22z" />
    </svg>
  );
}

/** The generic (non-image) file glyph used in the pending-attachments chips. */
export function FileIcon() {
  return (
    <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-4 h-4 text-ink-faint">
      <path d="M3 3.5A1.5 1.5 0 014.5 2h6.879a1.5 1.5 0 011.06.44l4.122 4.12A1.5 1.5 0 0117 7.622V16.5a1.5 1.5 0 01-1.5 1.5h-11A1.5 1.5 0 013 16.5v-13z" />
    </svg>
  );
}

/** An inline error banner with a dismiss button. */
export function DismissibleError({ message, onDismiss }: { message: string; onDismiss: () => void }) {
  return (
    <div className="mb-2 flex items-center gap-2 rounded-[10px] border border-lamp-err/40 bg-lamp-err/10 px-3 py-1.5 text-xs text-lamp-err">
      <span className="flex-1">{message}</span>
      <button
        onClick={onDismiss}
        className="rounded text-lamp-err/70 transition-colors hover:text-lamp-err"
        aria-label="Dismiss"
      >
        <CloseIcon />
      </button>
    </div>
  );
}

/**
 * The pending-file chip row above the composer. `thumb` toggles the image
 * preview strategy to match each caller exactly:
 *   - thumb=true  → thumbnail endpoint when supported, lazy/async decode.
 *   - thumb=false → raw blob URL, no lazy/decoding hints.
 */
export function PendingAttachments({
  files,
  onRemove,
  thumb,
}: {
  files: AgentMessageAttachment[];
  onRemove: (index: number) => void;
  thumb: boolean;
}) {
  if (files.length === 0) return null;
  return (
    <div className="mb-2 flex flex-wrap gap-2">
      {files.map((file, i) => (
        <div
          key={file.path}
          className="flex items-center gap-1.5 rounded-[10px] border border-hairline bg-surface px-2 py-1 text-xs text-ink-dim"
        >
          {file.mime.startsWith("image/") ? (
            thumb ? (
              <img
                src={
                  isThumbSupported(file.path)
                    ? api.files.thumbUrl(file.path, 64)
                    : api.files.rawUrl(file.path)
                }
                alt={file.name}
                className="h-6 w-6 rounded object-cover"
                loading="lazy"
                decoding="async"
              />
            ) : (
              <img
                src={api.files.rawUrl(file.path)}
                alt={file.name}
                className="h-6 w-6 rounded object-cover"
              />
            )
          ) : (
            <FileIcon />
          )}
          <span className="max-w-[120px] truncate">{file.name}</span>
          <button
            onClick={() => onRemove(i)}
            className="ml-0.5 rounded text-ink-faint transition-colors hover:text-ink"
            aria-label={`Remove ${file.name}`}
          >
            <CloseIcon />
          </button>
        </div>
      ))}
    </div>
  );
}

/**
 * "Load older messages" pager shared by both surfaces. Renders a centered,
 * hairline-straddled pill; `loading` disables it (AgentChat's canonical
 * variant). Wrap in the messages scroll container's top.
 */
export function LoadMoreButton({
  onClick,
  loading = false,
}: {
  onClick: () => void;
  loading?: boolean;
}) {
  return (
    <div className="flex justify-center pt-1 pb-3">
      <button
        onClick={onClick}
        disabled={loading}
        className="group relative px-4 py-1.5 font-mono text-[11px] text-ink-faint transition-colors hover:text-ink disabled:opacity-50"
      >
        <span className="absolute inset-x-0 top-1/2 h-px bg-hairline" />
        <span className="relative inline-flex items-center gap-1.5 bg-app px-3">
          <svg
            className="h-3 w-3 transition-transform group-hover:-translate-y-0.5"
            viewBox="0 0 16 16"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinecap="round"
          >
            <path d="M8 12V4M4 7l4-4 4 4" />
          </svg>
          older messages
        </span>
      </button>
    </div>
  );
}

/** Quiet icon button that opens the file picker; shows a spinner while uploading. */
export function AttachButton({
  onClick,
  uploading,
  disabled,
  title,
}: {
  onClick: () => void;
  uploading: boolean;
  disabled?: boolean;
  title?: string;
}) {
  return (
    <button
      onClick={onClick}
      disabled={disabled}
      className="shrink-0 rounded-[10px] p-2 text-ink-faint transition-colors hover:text-ink disabled:opacity-40"
      title={title ?? "Attach files"}
      aria-label="Attach files"
    >
      {uploading ? (
        <svg className="h-5 w-5 animate-spin" xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24">
          <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
          <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z" />
        </svg>
      ) : (
        <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="h-5 w-5">
          <path fillRule="evenodd" d="M15.621 4.379a3 3 0 00-4.242 0l-7 7a3 3 0 004.241 4.243h.001l.497-.5a.75.75 0 011.064 1.057l-.498.501-.002.002a4.5 4.5 0 01-6.364-6.364l7-7a4.5 4.5 0 016.368 6.36l-3.455 3.553A2.625 2.625 0 119.52 9.52l3.45-3.451a.75.75 0 111.061 1.06l-3.45 3.451a1.125 1.125 0 001.587 1.595l3.454-3.553a3 3 0 000-4.242z" clipRule="evenodd" />
        </svg>
      )}
    </button>
  );
}

/**
 * Mic toggle for push-to-talk voice input. Idle: quiet mic glyph. Listening:
 * pulsing copper. Connecting: spinner. Click toggles start/stop.
 */
export function MicButton({
  listening,
  connecting,
  disabled,
  onClick,
  title,
}: {
  listening: boolean;
  connecting: boolean;
  disabled?: boolean;
  onClick: () => void;
  title?: string;
}) {
  return (
    <button
      onClick={onClick}
      disabled={disabled}
      title={title ?? (listening ? "Stop voice input" : "Voice input")}
      aria-label={listening ? "Stop voice input" : "Voice input"}
      aria-pressed={listening}
      className={
        "shrink-0 rounded-[10px] p-2 transition-colors disabled:opacity-40 " +
        (listening
          ? "animate-pulse text-copper hover:text-copper-bright"
          : "text-ink-faint hover:text-ink")
      }
    >
      {connecting ? (
        <svg className="h-5 w-5 animate-spin" xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24">
          <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
          <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z" />
        </svg>
      ) : (
        <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="h-5 w-5">
          <path d="M10 2a3 3 0 00-3 3v5a3 3 0 006 0V5a3 3 0 00-3-3z" />
          <path d="M4 9a.75.75 0 011.5 0 4.5 4.5 0 009 0A.75.75 0 0116 9a6 6 0 01-5.25 5.954V17.5a.75.75 0 01-1.5 0v-2.546A6 6 0 014 9z" />
        </svg>
      )}
    </button>
  );
}

/** 36px copper circular send button (arrow-up). Quiet/faint when disabled. */
export function SendButton({
  onClick,
  disabled,
  title,
}: {
  onClick: () => void;
  disabled?: boolean;
  title?: string;
}) {
  return (
    <button
      onClick={onClick}
      disabled={disabled}
      title={title ?? "Send"}
      aria-label="Send"
      className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-copper text-[#14100b] transition-colors hover:bg-copper-bright disabled:cursor-not-allowed disabled:bg-raised disabled:text-ink-faint"
    >
      <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" className="h-5 w-5">
        <path d="M10 16V4M5 9l5-5 5 5" />
      </svg>
    </button>
  );
}

/** Stop-streaming button: lamp-err outline, square glyph. */
export function StopButton({ onClick, title }: { onClick: () => void; title?: string }) {
  return (
    <button
      onClick={onClick}
      title={title ?? "Stop"}
      aria-label="Stop"
      className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full border border-lamp-err text-lamp-err transition-colors hover:bg-lamp-err/10"
    >
      <svg viewBox="0 0 20 20" fill="currentColor" className="h-4 w-4">
        <rect x="5" y="5" width="10" height="10" rx="1.5" />
      </svg>
    </button>
  );
}

/**
 * Shared Enter-to-send keydown behavior.
 *
 * Ctrl+Enter (and Cmd+Enter on macOS, via metaKey) always sends, in both
 * modes. Beyond that: when `enterSends` is true, plain Enter sends and
 * Shift+Enter inserts a newline; when false, Enter and Shift+Enter both
 * insert a newline (only Ctrl/Cmd+Enter sends). IME composition
 * (isComposing) never triggers a send.
 */
export function enterToSend(
  e: React.KeyboardEvent,
  enterSends: boolean,
  onSend: () => void,
): void {
  if (e.key !== "Enter" || e.nativeEvent.isComposing) return;
  // Ctrl+Enter / Cmd+Enter always sends; otherwise fall back to the
  // enterSends preference (plain Enter sends, Shift+Enter is a newline).
  const modSend = e.ctrlKey || e.metaKey;
  if (modSend || (enterSends && !e.shiftKey)) {
    e.preventDefault();
    onSend();
  }
}

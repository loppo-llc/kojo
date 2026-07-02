import { useCallback, useEffect, useMemo, useState } from "react";
import type { AgentMessageAttachment } from "../../lib/agentApi";
import { formatSize } from "../../lib/utils";
import {
  attachmentThumbURL,
  attachmentURL,
  MediaOverlay,
  type MediaPreviewType,
} from "./MediaOverlay";

// File extension categories
const IMAGE_EXTS = /\.(png|jpe?g|gif|webp|svg|bmp|ico|avif)$/i;
const VIDEO_EXTS = /\.(mp4|webm|mov|avi|mkv|ogv|m4v|flv|wmv)$/i;

type MediaPreviewItem = {
  path: string;
  type: MediaPreviewType;
};

export function getFileType(path: string): "image" | "video" | "other" {
  if (IMAGE_EXTS.test(path)) return "image";
  if (VIDEO_EXTS.test(path)) return "video";
  return "other";
}

function attachmentPreviewType(att: AgentMessageAttachment): MediaPreviewType | null {
  const extType = getFileType(att.name || att.path);
  if (att.mime.startsWith("image/") || extType === "image") return "image";
  if (att.mime.startsWith("video/") || extType === "video") return "video";
  return null;
}

/**
 * Image attachment chip with onError fallback.
 *
 * The native `<img>` collapses to 0×0 when the URL 404s (which
 * happens during peer-transfer if the kojo-attach hub forwarder
 * push hasn't reached hub yet, and the bytes only live on the
 * holder peer's disk). Without a min-size + onError swap, the
 * user sees no chip at all and assumes the attachment is missing.
 *
 * We render a placeholder tile until `load` fires; on `error` we
 * keep that tile in place with a broken-image icon. The container
 * always reserves space so the chip is visible regardless of
 * fetch outcome.
 */
function ImageAttachmentChip({
  att,
  url,
  onPreview,
}: {
  att: AgentMessageAttachment;
  url: string;
  onPreview: () => void;
}) {
  const [loaded, setLoaded] = useState(false);
  const [failed, setFailed] = useState(false);
  // Reset both load-state flags whenever the URL changes. Without
  // this, swapping the same `<ImageAttachmentChip>` instance to a
  // different attachment (e.g. message re-render after a server
  // sync that rewrote the path) would keep the prior `loaded=true`
  // and skip the placeholder during the new fetch, briefly showing
  // a stale-source flash.
  useEffect(() => {
    setLoaded(false);
    setFailed(false);
  }, [url]);
  return (
    <button
      onClick={onPreview}
      className="relative block rounded-[10px] overflow-hidden hover:opacity-80 transition-opacity bg-surface min-w-[80px] min-h-[60px]"
      title={att.name}
    >
      <img
        src={url}
        alt={att.name}
        onLoad={() => setLoaded(true)}
        onError={() => setFailed(true)}
        className={`max-w-[200px] max-h-[150px] object-cover rounded-[10px] ${
          failed || !loaded ? "invisible" : ""
        }`}
      />
      {!loaded && (
        <div className="absolute inset-0 flex flex-col items-center justify-center gap-1 px-2 text-[10px] text-ink-dim">
          {failed ? (
            <>
              <svg
                xmlns="http://www.w3.org/2000/svg"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                strokeWidth={1.6}
                className="w-6 h-6 opacity-60"
              >
                <path strokeLinecap="round" strokeLinejoin="round" d="M3 16.5V6.75A2.25 2.25 0 015.25 4.5h13.5A2.25 2.25 0 0121 6.75v9.75M3 16.5l4.5-4.5 3 3 4.5-4.5 6 6M3 16.5v.75A2.25 2.25 0 005.25 19.5h13.5A2.25 2.25 0 0021 17.25" />
                <path strokeLinecap="round" strokeLinejoin="round" d="M3 3l18 18" />
              </svg>
              <span className="text-center break-all line-clamp-2">{att.name}</span>
              <span className="opacity-60">image unavailable</span>
            </>
          ) : (
            <div className="w-4 h-4 rounded-full border-2 border-ink-faint border-t-transparent animate-spin" />
          )}
        </div>
      )}
    </button>
  );
}

/** Display file attachments on a message */
export function AttachmentList({ attachments, isUser }: { attachments: AgentMessageAttachment[]; isUser: boolean }) {
  const [previewPath, setPreviewPath] = useState<string | null>(null);
  const previewable = useMemo<MediaPreviewItem[]>(
    () =>
      attachments.flatMap((att) => {
        const type = attachmentPreviewType(att);
        return type ? [{ path: att.path, type }] : [];
      }),
    [attachments],
  );
  const previewIndex = previewPath ? previewable.findIndex((item) => item.path === previewPath) : -1;
  const preview = previewIndex >= 0 ? previewable[previewIndex] : null;
  const navigatePreview = useCallback(
    (dir: -1 | 1) => {
      if (previewIndex < 0 || previewable.length <= 1) return;
      const next = (previewIndex + dir + previewable.length) % previewable.length;
      setPreviewPath(previewable[next].path);
    },
    [previewIndex, previewable],
  );

  return (
    <>
      <div className="flex flex-wrap gap-1.5 mb-2">
        {attachments.map((att) => {
          const url = attachmentURL(att.path);
          const thumbUrl = attachmentThumbURL(att.path, 400);
          const previewType = attachmentPreviewType(att);
          if (previewType === "image") {
            return (
              <ImageAttachmentChip
                key={att.path}
                att={att}
                url={thumbUrl}
                onPreview={() => setPreviewPath(att.path)}
              />
            );
          }
          if (previewType === "video") {
            return (
              <button
                key={att.path}
                onClick={() => setPreviewPath(att.path)}
                className={`flex items-center gap-1.5 px-2 py-1 rounded-[10px] text-xs hover:opacity-80 transition-opacity ${
                  isUser ? "bg-copper/20 text-ink" : "bg-surface text-ink-dim"
                }`}
              >
                <svg className="w-4 h-4 opacity-60" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                  <path strokeLinecap="round" strokeLinejoin="round" d="M5.25 5.653c0-.856.917-1.398 1.667-.986l11.54 6.348a1.125 1.125 0 010 1.971l-11.54 6.347a1.125 1.125 0 01-1.667-.985V5.653z" />
                </svg>
                <span className="max-w-[150px] truncate">{att.name}</span>
                <span className="opacity-50">{formatSize(att.size)}</span>
              </button>
            );
          }
          // Non-image/non-video: render as a download anchor so the
          // user can save the file. Anchor's `download` attribute
          // hints at save-as while still rendering inline-clickable.
          return (
            <a
              key={att.path}
              href={url}
              download={att.name}
              className={`flex items-center gap-1.5 px-2 py-1 rounded-[10px] text-xs hover:opacity-80 transition-opacity no-underline ${
                isUser ? "bg-copper/20 text-ink" : "bg-surface text-ink-dim"
              }`}
              title={`Download ${att.name}`}
            >
              <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-4 h-4 opacity-60">
                <path d="M3 3.5A1.5 1.5 0 014.5 2h6.879a1.5 1.5 0 011.06.44l4.122 4.12A1.5 1.5 0 0117 7.622V16.5a1.5 1.5 0 01-1.5 1.5h-11A1.5 1.5 0 013 16.5v-13z" />
              </svg>
              <span className="max-w-[150px] truncate">{att.name}</span>
              <span className="opacity-50">{formatSize(att.size)}</span>
            </a>
          );
        })}
      </div>
      {preview && (
        <MediaOverlay
          path={preview.path}
          type={preview.type}
          currentIndex={previewIndex}
          total={previewable.length}
          onClose={() => setPreviewPath(null)}
          onNavigate={navigatePreview}
        />
      )}
    </>
  );
}

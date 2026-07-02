import { useCallback, useEffect, useRef, useState } from "react";
import { api, isThumbSupported } from "../../lib/api";

export type MediaPreviewType = "image" | "video";

// attachmentURL resolves an attachment's path to a downloadable
// URL. kojo:// URIs (agent-generated attachments, hub-ingested peer
// pushes) route to /api/v1/blob/<scope>/<path>; everything else is
// treated as a legacy filesystem path and served via the file
// browser raw endpoint.
export function attachmentURL(path: string): string {
  return api.blob.urlFromKojoURI(path) ?? api.files.rawUrl(path);
}

// attachmentThumbURL is the inline-grid variant of attachmentURL.
// Both kojo:// blob URIs and filesystem paths route through their
// respective thumb endpoints so the browser fetches a cached
// low-res JPEG instead of the full body.
export function attachmentThumbURL(path: string, size = 400): string {
  // Blob path: use ?thumb=<size> on the blob endpoint.
  const blobThumb = api.blob.thumbFromKojoURI(path, size);
  if (blobThumb) return blobThumb;
  // Filesystem path: use the dedicated thumb endpoint.
  return isThumbSupported(path) ? api.files.thumbUrl(path, size) : api.files.rawUrl(path);
}

/** Full-screen overlay for image/video preview */
export function MediaOverlay({
  path,
  type,
  currentIndex,
  total,
  onClose,
  onNavigate,
}: {
  path: string;
  type: MediaPreviewType;
  currentIndex?: number;
  total?: number;
  onClose: () => void;
  onNavigate?: (dir: -1 | 1) => void;
}) {
  // attachmentURL routes kojo:// → /api/v1/blob/... and falls back
  // to the file browser raw endpoint for legacy filesystem paths.
  const rawUrl = attachmentURL(path);
  const [videoError, setVideoError] = useState(false);
  const [mediaWidth, setMediaWidth] = useState<number | null>(null);
  const mediaFrameRef = useRef<HTMLDivElement>(null);
  const fileName = path.split(/[/\\]/).pop() || path;
  const caption = path;
  const canNavigate = !!onNavigate && (total ?? 0) > 1;

  const updateMediaWidth = useCallback(() => {
    const el = mediaFrameRef.current;
    if (!el) return;
    const width = el.getBoundingClientRect().width;
    if (width > 0) setMediaWidth(Math.round(width));
  }, []);

  useEffect(() => {
    setVideoError(false);
    setMediaWidth(null);
  }, [rawUrl, type]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        onClose();
        return;
      }
      if (!canNavigate) return;
      if (e.key === "ArrowLeft") {
        e.preventDefault();
        onNavigate?.(-1);
      } else if (e.key === "ArrowRight") {
        e.preventDefault();
        onNavigate?.(1);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [canNavigate, onClose, onNavigate]);

  useEffect(() => {
    const el = mediaFrameRef.current;
    if (!el) return;
    updateMediaWidth();
    let observer: ResizeObserver | null = null;
    if (typeof ResizeObserver !== "undefined") {
      observer = new ResizeObserver(updateMediaWidth);
      observer.observe(el);
    }
    window.addEventListener("resize", updateMediaWidth);
    return () => {
      observer?.disconnect();
      window.removeEventListener("resize", updateMediaWidth);
    };
  }, [path, type, videoError, updateMediaWidth]);

  const handleBackdrop = useCallback(
    (e: React.MouseEvent) => {
      if (e.target === e.currentTarget) onClose();
    },
    [onClose],
  );

  return (
    <div
      className="fixed inset-0 z-50 bg-black/90 backdrop-blur-sm flex items-center justify-center p-4"
      onClick={handleBackdrop}
    >
      <div className="relative flex max-w-[90vw] max-h-[90vh] flex-col items-center">
        <button
          type="button"
          onClick={onClose}
          className="absolute -top-3 -right-3 w-8 h-8 bg-raised hover:bg-hover rounded-full flex items-center justify-center text-ink-dim hover:text-ink z-10 shadow-lg"
          aria-label="Close preview"
        >
          <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M6 18L18 6M6 6l12 12" />
          </svg>
        </button>

        {canNavigate && currentIndex != null && total != null && (
          <div className="absolute -top-7 left-0 right-0 text-center font-mono text-[11px] text-ink-faint pointer-events-none">
            {currentIndex + 1} / {total}
          </div>
        )}

        {canNavigate && (
          <>
            <button
              type="button"
              onClick={(e) => {
                e.stopPropagation();
                onNavigate?.(-1);
              }}
              className="absolute left-2 top-1/2 z-10 flex h-10 w-10 -translate-y-1/2 items-center justify-center rounded-full bg-black/40 text-ink-dim transition-colors hover:bg-black/60 hover:text-ink"
              aria-label="Previous preview"
              title="Previous"
            >
              <svg className="w-5 h-5" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                <path strokeLinecap="round" strokeLinejoin="round" d="M15 19l-7-7 7-7" />
              </svg>
            </button>
            <button
              type="button"
              onClick={(e) => {
                e.stopPropagation();
                onNavigate?.(1);
              }}
              className="absolute right-2 top-1/2 z-10 flex h-10 w-10 -translate-y-1/2 items-center justify-center rounded-full bg-black/40 text-ink-dim transition-colors hover:bg-black/60 hover:text-ink"
              aria-label="Next preview"
              title="Next"
            >
              <svg className="w-5 h-5" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                <path strokeLinecap="round" strokeLinejoin="round" d="M9 5l7 7-7 7" />
              </svg>
            </button>
          </>
        )}

        <div ref={mediaFrameRef} className="inline-flex max-w-[90vw] items-center justify-center">
          {type === "image" ? (
            <img
              src={rawUrl}
              alt={caption}
              onLoad={updateMediaWidth}
              className="mx-auto block max-w-[90vw] max-h-[85vh] object-contain rounded-lg shadow-2xl"
            />
          ) : videoError ? (
            <div className="flex flex-col items-center gap-3 p-8 bg-surface rounded-[10px] shadow-2xl">
              <svg className="w-12 h-12 text-ink-faint" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={1.5}>
                <path strokeLinecap="round" strokeLinejoin="round" d="M12 9v3.75m9-.75a9 9 0 11-18 0 9 9 0 0118 0zm-9 3.75h.008v.008H12v-.008z" />
              </svg>
              <p className="text-sm text-ink-dim">This video format cannot be played in the browser.</p>
              <a
                href={`${rawUrl}&download=1`}
                download={fileName}
                className="px-4 py-2 bg-copper hover:bg-copper-bright text-[#14100b] font-semibold text-sm rounded-[10px] transition-colors"
                onClick={(e) => e.stopPropagation()}
              >
                Download
              </a>
            </div>
          ) : (
            <video
              src={rawUrl}
              controls
              autoPlay
              playsInline
              onLoadedMetadata={updateMediaWidth}
              onError={() => setVideoError(true)}
              className="mx-auto block max-w-[90vw] max-h-[85vh] rounded-lg shadow-2xl"
            />
          )}
        </div>

        <div
          className="mt-2 max-w-[90vw] whitespace-pre-wrap text-center text-xs text-ink-dim font-mono wrap-anywhere"
          style={{ width: mediaWidth ? `${mediaWidth}px` : "min(90vw, 32rem)" }}
        >
          {caption}
        </div>
      </div>
    </div>
  );
}

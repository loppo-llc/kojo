import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { api, type Attachment } from "../lib/api";
import { authHeaders } from "../lib/auth";
import { formatSize } from "../lib/utils";

type SortField = "modTime" | "createdAt" | "name" | "size";
type SortDir = "asc" | "desc";

const SORT_OPTIONS: { key: SortField; label: string }[] = [
  { key: "modTime", label: "Modified" },
  { key: "createdAt", label: "Created" },
  { key: "name", label: "Name" },
  { key: "size", label: "Size" },
];

interface Props {
  sessionId: string;
  attachments: Attachment[];
  // peerId, when set, forwards deleteAttachment to the peer that
  // owns the session.
  peerId?: string;
  onDelete: (path: string) => void;
}

export function AttachmentsTab({ sessionId, attachments, peerId, onDelete }: Props) {
  const [sortField, setSortField] = useState<SortField>("modTime");
  const [sortDir, setSortDir] = useState<SortDir>("desc");
  const [previewPath, setPreviewPath] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  const [feedback, setFeedback] = useState<{ path: string; msg: string } | null>(null);

  const sorted = useMemo(() => {
    const arr = [...attachments];
    arr.sort((a, b) => {
      let cmp = 0;
      switch (sortField) {
        case "modTime":
          cmp = new Date(a.modTime).getTime() - new Date(b.modTime).getTime();
          break;
        case "createdAt":
          cmp = new Date(a.createdAt).getTime() - new Date(b.createdAt).getTime();
          break;
        case "name":
          cmp = a.name.localeCompare(b.name);
          break;
        case "size":
          cmp = a.size - b.size;
          break;
      }
      // Stable tiebreaker by path
      if (cmp === 0) cmp = a.path < b.path ? -1 : a.path > b.path ? 1 : 0;
      return sortDir === "desc" ? -cmp : cmp;
    });
    return arr;
  }, [attachments, sortField, sortDir]);

  const showFeedback = (path: string, msg: string) => {
    setFeedback({ path, msg });
    setTimeout(() => setFeedback(null), 1500);
  };

  const handleCopyPath = async (path: string) => {
    try {
      await navigator.clipboard.writeText(path);
      showFeedback(path, "Path copied");
    } catch {
      // clipboard API may fail on insecure contexts
    }
  };

  const handleCopyImage = async (att: Attachment) => {
    let objectUrl: string | undefined;
    try {
      // Header-based auth keeps the Owner token out of the request
      // URL / access logs for fetch-driven calls.
      const res = await fetch(api.files.rawPath(att.path, peerId), {
        headers: authHeaders(),
      });
      if (!res.ok) {
        showFeedback(att.path, "Failed");
        return;
      }
      const blob = await res.blob();
      if (blob.type === "image/png") {
        await navigator.clipboard.write([new ClipboardItem({ "image/png": blob })]);
      } else {
        // Convert to PNG via canvas for browser compatibility
        const img = new Image();
        objectUrl = URL.createObjectURL(blob);
        await new Promise<void>((resolve, reject) => {
          img.onload = () => resolve();
          img.onerror = reject;
          img.src = objectUrl!;
        });
        const maxDim = 4096;
        let w = img.naturalWidth;
        let h = img.naturalHeight;
        if (w > maxDim || h > maxDim) {
          const scale = maxDim / Math.max(w, h);
          w = Math.round(w * scale);
          h = Math.round(h * scale);
        }
        const canvas = document.createElement("canvas");
        canvas.width = w;
        canvas.height = h;
        canvas.getContext("2d")!.drawImage(img, 0, 0, w, h);
        const pngBlob = await new Promise<Blob | null>((resolve) =>
          canvas.toBlob((b) => resolve(b), "image/png"),
        );
        if (!pngBlob) {
          showFeedback(att.path, "Failed");
          return;
        }
        await navigator.clipboard.write([new ClipboardItem({ "image/png": pngBlob })]);
      }
      showFeedback(att.path, "Copied");
    } catch {
      showFeedback(att.path, "Failed");
    } finally {
      if (objectUrl) URL.revokeObjectURL(objectUrl);
    }
  };

  const handleDelete = async (path: string) => {
    if (confirmDelete !== path) {
      setConfirmDelete(path);
      return;
    }
    try {
      await api.sessions.deleteAttachment(sessionId, path, peerId);
      onDelete(path);
    } catch {
      // silently ignore — file may already be gone
    }
    setConfirmDelete(null);
  };

  const handleSortToggle = (field: SortField) => {
    if (sortField === field) {
      setSortDir((d) => (d === "desc" ? "asc" : "desc"));
    } else {
      setSortField(field);
      setSortDir(field === "name" ? "asc" : "desc");
    }
  };

  const isImage = (mime: string) => mime.startsWith("image/");
  const isVideo = (mime: string) => mime.startsWith("video/");

  // Previewable items in sorted order (images + videos only)
  const previewable = useMemo(
    () => sorted.filter((a) => isImage(a.mime) || isVideo(a.mime)),
    [sorted],
  );
  const previewIdx = previewPath ? previewable.findIndex((a) => a.path === previewPath) : -1;
  const previewAttachment = previewIdx >= 0 ? previewable[previewIdx] : null;

  const navigatePreview = useCallback(
    (dir: -1 | 1) => {
      if (previewIdx < 0 || previewable.length <= 1) return;
      const next = (previewIdx + dir + previewable.length) % previewable.length;
      setPreviewPath(previewable[next].path);
    },
    [previewIdx, previewable],
  );

  // Keyboard: left/right arrows
  useEffect(() => {
    if (!previewPath) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "ArrowLeft") { e.preventDefault(); navigatePreview(-1); }
      else if (e.key === "ArrowRight") { e.preventDefault(); navigatePreview(1); }
      else if (e.key === "Escape") { e.preventDefault(); setPreviewPath(null); }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [previewPath, navigatePreview]);

  // Swipe tracking
  const touchStartX = useRef(0);

  if (attachments.length === 0) {
    return (
      <div className="flex items-center justify-center h-full text-neutral-600 text-sm">
        No attachments detected
      </div>
    );
  }

  return (
    <div className="h-full flex flex-col">
      {/* Sort bar */}
      <div className="flex items-center gap-1 px-3 py-2 border-b border-neutral-800/50 shrink-0">
        {SORT_OPTIONS.map((opt) => (
          <button
            key={opt.key}
            onClick={() => handleSortToggle(opt.key)}
            className={`px-2 py-1 text-xs rounded transition-colors ${
              sortField === opt.key
                ? "bg-neutral-800 text-neutral-200"
                : "text-neutral-500 hover:text-neutral-400"
            }`}
          >
            {opt.label}
            {sortField === opt.key && (
              <span className="ml-0.5">{sortDir === "desc" ? "\u2193" : "\u2191"}</span>
            )}
          </button>
        ))}
        <span className="ml-auto text-xs text-neutral-600">{attachments.length}</span>
      </div>

      {/* Grid */}
      <div className="flex-1 overflow-y-auto p-2">
        <div className="grid grid-cols-2 gap-2">
          {sorted.map((att) => (
            <div key={att.path} className="bg-neutral-900/80 rounded-lg overflow-hidden">
              {/* Thumbnail */}
              {isImage(att.mime) ? (
                <button
                  onClick={() => setPreviewPath(att.path)}
                  className="w-full aspect-square bg-neutral-900 relative block"
                >
                  <img
                    src={api.files.rawUrl(att.path, false, peerId)}
                    alt={att.name}
                    className="w-full h-full object-cover"
                    loading="lazy"
                  />
                  {feedback?.path === att.path && (
                    <div className="absolute inset-0 bg-black/60 flex items-center justify-center text-xs text-green-400 pointer-events-none">
                      {feedback.msg}
                    </div>
                  )}
                </button>
              ) : isVideo(att.mime) ? (
                <button
                  onClick={() => setPreviewPath(att.path)}
                  className="w-full aspect-square bg-neutral-900 flex items-center justify-center"
                >
                  <span className="text-3xl text-neutral-600">&#x25B6;</span>
                </button>
              ) : (
                <div className="w-full aspect-square bg-neutral-900 flex items-center justify-center">
                  <span className="text-2xl text-neutral-700">&#x1F4CE;</span>
                </div>
              )}

              {/* Info */}
              <div className="px-2 pt-1.5 pb-1">
                <div className="text-xs text-neutral-300 truncate">{att.name}</div>
                <div className="text-[10px] text-neutral-600">{formatSize(att.size)}</div>
              </div>

              {/* Actions */}
              <div className="flex border-t border-neutral-800/50">
                {isImage(att.mime) && (
                  <button
                    onClick={() => handleCopyImage(att)}
                    className="flex-1 py-1.5 text-[10px] text-neutral-500 hover:text-neutral-300 hover:bg-neutral-800/50 transition-colors"
                  >
                    Copy
                  </button>
                )}
                <button
                  onClick={() => handleCopyPath(att.path)}
                  className="flex-1 py-1.5 text-[10px] text-neutral-500 hover:text-neutral-300 hover:bg-neutral-800/50 transition-colors"
                >
                  Path
                </button>
                <button
                  onClick={() => handleDelete(att.path)}
                  className={`flex-1 py-1.5 text-[10px] transition-colors ${
                    confirmDelete === att.path
                      ? "bg-red-950 text-red-400"
                      : "text-neutral-500 hover:text-red-400 hover:bg-red-950/30"
                  }`}
                >
                  {confirmDelete === att.path ? "OK?" : "Del"}
                </button>
              </div>
            </div>
          ))}
        </div>
      </div>

      {/* Preview overlay */}
      {previewPath && previewAttachment && (
        <div
          className="fixed inset-0 z-50 bg-black/90 flex items-center justify-center"
          onClick={() => setPreviewPath(null)}
          onTouchStart={(e) => { touchStartX.current = e.touches[0].clientX; }}
          onTouchEnd={(e) => {
            const dx = e.changedTouches[0].clientX - touchStartX.current;
            if (Math.abs(dx) > 50) {
              e.preventDefault();
              navigatePreview(dx < 0 ? 1 : -1);
            }
          }}
        >
          {/* Close */}
          <button
            onClick={() => setPreviewPath(null)}
            className="absolute top-4 right-4 text-neutral-400 hover:text-white text-2xl z-10"
          >
            &times;
          </button>

          {/* Counter */}
          {previewable.length > 1 && (
            <div className="absolute top-4 left-0 right-0 text-center text-xs text-neutral-500 pointer-events-none">
              {previewIdx + 1} / {previewable.length}
            </div>
          )}

          {/* Prev/Next buttons (desktop) */}
          {previewable.length > 1 && (
            <>
              <button
                onClick={(e) => { e.stopPropagation(); navigatePreview(-1); }}
                className="absolute left-2 top-1/2 -translate-y-1/2 z-10 w-10 h-10 flex items-center justify-center text-neutral-500 hover:text-white text-xl"
              >
                &#x2039;
              </button>
              <button
                onClick={(e) => { e.stopPropagation(); navigatePreview(1); }}
                className="absolute right-2 top-1/2 -translate-y-1/2 z-10 w-10 h-10 flex items-center justify-center text-neutral-500 hover:text-white text-xl"
              >
                &#x203A;
              </button>
            </>
          )}

          {/* Content */}
          <div className="max-w-full max-h-full p-4" onClick={(e) => e.stopPropagation()}>
            {isImage(previewAttachment.mime) && (
              <img
                src={api.files.rawUrl(previewPath, false, peerId)}
                alt={previewAttachment.name}
                className="max-w-full max-h-[85vh] object-contain"
              />
            )}
            {isVideo(previewAttachment.mime) && (
              <video
                src={api.files.rawUrl(previewPath, false, peerId)}
                controls
                className="max-w-full max-h-[85vh]"
              />
            )}
          </div>

          {/* Filename */}
          <div className="absolute bottom-4 left-0 right-0 text-center text-xs text-neutral-500 pointer-events-none truncate px-8">
            {previewAttachment.name}
          </div>
        </div>
      )}
    </div>
  );
}

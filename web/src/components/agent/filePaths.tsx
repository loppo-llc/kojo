import { useEffect, useRef, useState } from "react";
import { api, isThumbSupported } from "../../lib/api";
import { authHeaders } from "../../lib/auth";
import { formatSize } from "../../lib/utils";
import { getFileType } from "./MessageAttachments";

// Match absolute file paths (Unix, ~/-relative, or Windows-style) with any file
// extension (1-10 chars). Bare chat autolinks intentionally reject whitespace:
// it avoids turning fragments like "/ hoge/foo.txt" into broken file links.
const FILE_PATH_RE =
  /(?:~?(?:\/[^\s,;:"'`<>\[\]{}()\\|/]+)+|[A-Z]:\\(?:[^\s,;:"'`<>\[\]{}()\\|]+\\)*[^\s,;:"'`<>\[\]{}()\\|]+)\.[a-zA-Z0-9]{1,10}\b/gi;

const FILE_PATH_PREFIX_CHARS = new Set([
  " ", "\t", "\n", "\r",
  "`", "\"", "'",
  "(", "[", "{", "<",
]);

const FILE_PATH_SUFFIX_CHARS = new Set([
  " ", "\t", "\n", "\r",
  "`", "\"", "'",
  ".", ",", ";", ":", "!", "?",
  ")", "]", "}", ">",
]);

/** Clickable file path chip with hover tooltip */
export function FilePathChip({
  path,
  onPreview,
}: {
  path: string;
  onPreview: (p: { path: string; type: "image" | "video" }) => void;
}) {
  const fileType = getFileType(path);
  const [hover, setHover] = useState(false);
  const [fileSize, setFileSize] = useState<string | null>(null);
  const fetchedRef = useRef(false);
  const rawUrl = api.files.rawUrl(path);
  // Hover thumbnail uses the thumb endpoint only for formats it can
  // decode; everything else falls back to raw so we never end up
  // requesting a known-415 URL.
  const thumbHoverUrl = isThumbSupported(path) ? api.files.thumbUrl(path, 400) : rawUrl;
  const linkUrl = api.files.rawUrl(path, fileType === "other");
  const fileName = path.split(/[/\\]/).pop() || path;

  // Reset fetch state when path changes (e.g. streaming token extends path)
  useEffect(() => {
    fetchedRef.current = false;
    setFileSize(null);
  }, [path]);

  // Fetch file size on first hover (for non-image files)
  useEffect(() => {
    if (!hover || fileType === "image" || fetchedRef.current) return;
    fetchedRef.current = true;
    // HEAD via header auth — keeps the token out of the URL.
    fetch(api.files.rawPath(path), { method: "HEAD", headers: authHeaders() })
      .then((res) => {
        const len = res.headers.get("content-length");
        setFileSize(len ? formatSize(Number(len)) : "—");
      })
      .catch(() => setFileSize("—"));
  }, [hover, fileType, rawUrl]);

  const handleClick = (e: React.MouseEvent<HTMLAnchorElement>) => {
    const selection = window.getSelection();
    if (selection && !selection.isCollapsed && selection.toString() && selection.rangeCount > 0) {
      for (let i = 0; i < selection.rangeCount; i += 1) {
        if (selection.getRangeAt(i).intersectsNode(e.currentTarget)) {
          e.preventDefault();
          return;
        }
      }
    }

    if (fileType !== "other") {
      e.preventDefault();
      onPreview({ path, type: fileType });
    }
  };

  return (
    <span
      className="relative inline align-bottom"
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
    >
      <a
        href={linkUrl}
        download={fileType === "other" ? fileName : undefined}
        onClick={handleClick}
        className="inline px-1.5 py-0.5 mx-0.5 bg-surface hover:bg-hover rounded text-xs font-mono text-copper hover:text-copper-bright transition-colors text-left wrap-anywhere box-decoration-clone cursor-pointer select-text"
        title={fileType === "other" ? `Download ${fileName}` : `Preview ${fileName}`}
      >
        {fileType === "image" ? (
          <svg className="inline-block w-3 h-3 mr-1 align-[-2px]" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M2.25 15.75l5.159-5.159a2.25 2.25 0 013.182 0l5.159 5.159m-1.5-1.5l1.409-1.409a2.25 2.25 0 013.182 0l2.909 2.909M3.75 21h16.5A2.25 2.25 0 0022.5 18.75V5.25A2.25 2.25 0 0020.25 3H3.75A2.25 2.25 0 001.5 5.25v13.5A2.25 2.25 0 003.75 21z" />
          </svg>
        ) : fileType === "video" ? (
          <svg className="inline-block w-3 h-3 mr-1 align-[-2px]" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M5.25 5.653c0-.856.917-1.398 1.667-.986l11.54 6.348a1.125 1.125 0 010 1.971l-11.54 6.347a1.125 1.125 0 01-1.667-.985V5.653z" />
          </svg>
        ) : (
          <svg className="inline-block w-3 h-3 mr-1 align-[-2px]" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M19.5 14.25v-2.625a3.375 3.375 0 00-3.375-3.375h-1.5A1.125 1.125 0 0113.5 7.125v-1.5a3.375 3.375 0 00-3.375-3.375H8.25m2.25 0H5.625c-.621 0-1.125.504-1.125 1.125v17.25c0 .621.504 1.125 1.125 1.125h12.75c.621 0 1.125-.504 1.125-1.125V11.25a9 9 0 00-9-9z" />
          </svg>
        )}
        {fileName}
      </a>
      {hover && (
        <div className="absolute bottom-full left-0 mb-1.5 z-40 pointer-events-none">
          {fileType === "image" ? (
            <img
              src={thumbHoverUrl}
              alt={fileName}
              className="max-w-[200px] max-h-[150px] object-contain rounded-[10px] shadow-lg border border-hairline bg-surface"
              loading="lazy"
              decoding="async"
            />
          ) : (
            <div className="px-2 py-1 bg-raised rounded text-xs text-ink-dim shadow-lg border border-hairline whitespace-nowrap">
              {fileSize || "…"}
            </div>
          )}
        </div>
      )}
    </span>
  );
}

/** Split text into text parts and file path parts */
export function splitFilePaths(text: string): Array<{ type: "text" | "file"; value: string }> {
  const parts: Array<{ type: "text" | "file"; value: string }> = [];
  let lastIndex = 0;

  // Reset regex state
  FILE_PATH_RE.lastIndex = 0;
  let match;
  while ((match = FILE_PATH_RE.exec(text)) !== null) {
    // Only match paths preceded/followed by whitespace, text boundaries, or
    // delimiters that commonly wrap paths in chat text.
    if (match.index > 0) {
      const prev = text[match.index - 1];
      if (!FILE_PATH_PREFIX_CHARS.has(prev)) continue;
    }
    const endIndex = match.index + match[0].length;
    if (endIndex < text.length && !FILE_PATH_SUFFIX_CHARS.has(text[endIndex])) continue;
    if (match.index > lastIndex) {
      parts.push({ type: "text", value: text.slice(lastIndex, match.index) });
    }
    parts.push({ type: "file", value: match[0] });
    lastIndex = endIndex;
  }

  if (lastIndex < text.length) {
    parts.push({ type: "text", value: text.slice(lastIndex) });
  }

  return parts.length > 0 ? parts : [{ type: "text", value: text }];
}

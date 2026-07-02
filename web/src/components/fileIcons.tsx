// File-kind classification + icon glyphs for the file browser list/rows.

export type FileKind = "image" | "video" | "audio" | "markdown" | "code" | "data" | "archive" | "pdf" | "text";

const IMAGE_EXTS = new Set(["png", "jpg", "jpeg", "gif", "webp", "svg", "bmp", "avif", "ico"]);
const VIDEO_EXTS = new Set(["mp4", "webm", "mov", "avi", "mkv", "m4v"]);
const AUDIO_EXTS = new Set(["mp3", "wav", "ogg", "flac", "m4a", "aac"]);
const CODE_EXTS = new Set([
  "js", "jsx", "ts", "tsx", "go", "rs", "py", "rb", "java", "c", "cpp", "h",
  "hpp", "swift", "kt", "sh", "bash", "zsh", "sql", "css", "scss", "html",
  "mod", "sum", "lua", "php", "cs", "dart", "vue", "svelte",
]);
const DATA_EXTS = new Set(["json", "yaml", "yml", "toml", "xml", "jsonl", "csv", "tsv", "ini", "env", "conf"]);
const MARKDOWN_EXTS = new Set(["md", "markdown", "mdx"]);
const ARCHIVE_EXTS = new Set(["zip", "tar", "gz", "tgz", "bz2", "xz", "7z", "rar"]);
const PDF_EXTS = new Set(["pdf"]);

export const KIND_STYLES: Record<FileKind, { bg: string; icon: string }> = {
  image:    { bg: "bg-emerald-500/10",  icon: "text-emerald-400" },
  video:    { bg: "bg-rose-500/10",     icon: "text-rose-400" },
  audio:    { bg: "bg-pink-500/10",     icon: "text-pink-400" },
  markdown: { bg: "bg-sky-500/10",      icon: "text-sky-400" },
  code:     { bg: "bg-violet-500/10",   icon: "text-violet-400" },
  data:     { bg: "bg-amber-500/10",    icon: "text-amber-400" },
  archive:  { bg: "bg-orange-500/10",   icon: "text-orange-400" },
  pdf:      { bg: "bg-red-500/10",      icon: "text-red-400" },
  text:     { bg: "bg-surface",         icon: "text-ink-dim" },
};

export function fileKind(name: string): FileKind {
  const dot = name.lastIndexOf(".");
  if (dot < 0) return "text";
  const ext = name.slice(dot + 1).toLowerCase();
  if (IMAGE_EXTS.has(ext)) return "image";
  if (VIDEO_EXTS.has(ext)) return "video";
  if (AUDIO_EXTS.has(ext)) return "audio";
  if (MARKDOWN_EXTS.has(ext)) return "markdown";
  if (CODE_EXTS.has(ext)) return "code";
  if (DATA_EXTS.has(ext)) return "data";
  if (ARCHIVE_EXTS.has(ext)) return "archive";
  if (PDF_EXTS.has(ext)) return "pdf";
  return "text";
}

export function isImage(name: string): boolean {
  return fileKind(name) === "image";
}

export function FolderIcon({ className = "" }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" fill="currentColor" className={className}>
      <path d="M3 6.75A2.25 2.25 0 015.25 4.5h3.19a2.25 2.25 0 011.59.66L11.56 6.56a.75.75 0 00.53.22h6.66A2.25 2.25 0 0121 9.03v8.72a2.25 2.25 0 01-2.25 2.25H5.25A2.25 2.25 0 013 17.75V6.75z" />
    </svg>
  );
}

export function FileGlyph({ kind, className = "" }: { kind: FileKind; className?: string }) {
  switch (kind) {
    case "image":
      return (
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" className={className}>
          <rect x="3" y="4.5" width="18" height="15" rx="2" />
          <circle cx="8.5" cy="10" r="1.5" fill="currentColor" />
          <path d="M21 16.5l-5-5-9 9" />
        </svg>
      );
    case "video":
      return (
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" className={className}>
          <rect x="3" y="5" width="14" height="14" rx="2" />
          <path d="M17 10l4-2v8l-4-2" fill="currentColor" stroke="none" />
        </svg>
      );
    case "audio":
      return (
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" className={className}>
          <path d="M9 18V6l10-2v12" />
          <circle cx="7" cy="18" r="2" fill="currentColor" />
          <circle cx="17" cy="16" r="2" fill="currentColor" />
        </svg>
      );
    case "markdown":
      return (
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" className={className}>
          <rect x="3" y="5" width="18" height="14" rx="2" />
          <path d="M7 15V9l2.5 3L12 9v6" />
          <path d="M16 9v6l2-2M18 15l2-2" />
        </svg>
      );
    case "code":
      return (
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" className={className}>
          <path d="M9 8l-4 4 4 4M15 8l4 4-4 4" />
        </svg>
      );
    case "data":
      return (
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" className={className}>
          <ellipse cx="12" cy="6" rx="8" ry="2.5" />
          <path d="M4 6v6c0 1.4 3.6 2.5 8 2.5s8-1.1 8-2.5V6" />
          <path d="M4 12v6c0 1.4 3.6 2.5 8 2.5s8-1.1 8-2.5v-6" />
        </svg>
      );
    case "archive":
      return (
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" className={className}>
          <rect x="4" y="4" width="16" height="16" rx="2" />
          <path d="M12 4v4M12 10v2M12 14v2" />
        </svg>
      );
    case "pdf":
      return (
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" className={className}>
          <path d="M14 3H7a2 2 0 00-2 2v14a2 2 0 002 2h10a2 2 0 002-2V8z" />
          <path d="M14 3v5h5" />
          <path d="M9 14h1.5a1.5 1.5 0 010 3H9v-3zM13 14v3M16 14h2M16 15.5h1.5" />
        </svg>
      );
    default:
      return (
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" className={className}>
          <path d="M14 3H7a2 2 0 00-2 2v14a2 2 0 002 2h10a2 2 0 002-2V8z" />
          <path d="M14 3v5h5" />
          <path d="M8 13h8M8 17h5" />
        </svg>
      );
  }
}

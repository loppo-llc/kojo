import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { useLocation, useNavigate, useSearchParams } from "react-router";
import type { DirEntry, FileView } from "../lib/api";
import { formatSize, timeAgo } from "../lib/utils";
import { MarkdownRenderer } from "./agent/MarkdownRenderer";

type SortKey = "name" | "size" | "modified";
type SortDir = "asc" | "desc";
type PathMode = "relative" | "absolute";
type HistoryKind = "dir" | "view";

interface FileDataSource {
  list: (path: string, hidden: boolean) => Promise<{ path: string; absPath?: string; entries: DirEntry[] }>;
  view: (path: string) => Promise<FileView>;
  rawUrl: (path: string, download?: boolean) => string;
  // Optional thumbnail URL — list cells use this for image previews so a
  // directory full of screenshots doesn't transfer megabytes per row.
  // Sources that don't implement it fall back to rawUrl. `v` is an
  // optional cache-busting string (typically the source's modTime).
  thumbUrl?: (path: string, size?: number, v?: string) => string;
}

interface FileDataBrowserProps {
  dataSource: FileDataSource;
  pathMode: PathMode;
  pathParam: string;
  rootPath?: string;
  rootLabel: string;
  title: ReactNode;
  subtitle?: ReactNode;
  leading?: ReactNode;
  showHeader?: boolean;
  ready?: boolean;
  onExit: () => void;
}

type FileKind = "image" | "video" | "audio" | "markdown" | "code" | "data" | "archive" | "pdf" | "text";

interface ViewerState {
  path: string;
  name: string;
  view?: FileView;
  error?: string;
  loading: boolean;
}

const SEP = "/";
const VIEW_PARAM = "view";
// Directory and preview navigation both live in URL search params. These
// history markers let the back button pop viewer-local entries instead of
// leaving stale file-browser routes behind the chat screen.
const HISTORY_STATE_KEY = "kojoFileBrowser";
const HISTORY_DEPTH_KEY = "kojoFileBrowserDepth";

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

const KIND_STYLES: Record<FileKind, { bg: string; icon: string }> = {
  image:    { bg: "bg-emerald-500/10",  icon: "text-emerald-400" },
  video:    { bg: "bg-rose-500/10",     icon: "text-rose-400" },
  audio:    { bg: "bg-pink-500/10",     icon: "text-pink-400" },
  markdown: { bg: "bg-sky-500/10",      icon: "text-sky-400" },
  code:     { bg: "bg-violet-500/10",   icon: "text-violet-400" },
  data:     { bg: "bg-amber-500/10",    icon: "text-amber-400" },
  archive:  { bg: "bg-orange-500/10",   icon: "text-orange-400" },
  pdf:      { bg: "bg-red-500/10",      icon: "text-red-400" },
  text:     { bg: "bg-neutral-700/40",  icon: "text-neutral-300" },
};

function fileKind(name: string): FileKind {
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

function isImage(name: string): boolean {
  return fileKind(name) === "image";
}

function sanitizeRelativePath(raw: string): string {
  return raw
    .split(/[/\\]+/)
    .filter((s) => s && s !== "." && s !== "..")
    .join(SEP);
}

function normalizePath(raw: string, mode: PathMode): string {
  if (mode === "relative") return sanitizeRelativePath(raw);
  return raw;
}

function pathSep(path: string): string {
  return path.includes("\\") && !path.includes("/") ? "\\" : "/";
}

function joinBrowserPath(parent: string, name: string, mode: PathMode): string {
  if (mode === "relative") return sanitizeRelativePath([parent, name].filter(Boolean).join(SEP));
  if (!parent) return name;
  const sep = pathSep(parent);
  return parent.endsWith("/") || parent.endsWith("\\") ? `${parent}${name}` : `${parent}${sep}${name}`;
}

function trimTrailingSep(path: string): string {
  if (!path) return "";
  if (path === "/" || /^[A-Za-z]:[\\/]?$/.test(path)) return path;
  return path.replace(/[/\\]+$/, "");
}

function unifiedPath(path: string): string {
  const unified = trimTrailingSep(path).replace(/\\/g, "/");
  return unified === "" && path.startsWith("/") ? "/" : unified;
}

function samePath(a: string, b: string): boolean {
  const ua = unifiedPath(a);
  const ub = unifiedPath(b);
  if (/^[A-Za-z]:/.test(ua) || /^[A-Za-z]:/.test(ub)) return ua.toLowerCase() === ub.toLowerCase();
  return ua === ub;
}

function pathWithin(path: string, root: string): boolean {
  const p = unifiedPath(path);
  const r = unifiedPath(root);
  if (samePath(p, r)) return true;
  const prefix = r.endsWith("/") ? r : `${r}/`;
  return p.startsWith(prefix);
}

function parentBrowserPath(path: string, mode: PathMode): string {
  if (mode === "relative") {
    const parts = sanitizeRelativePath(path).split(SEP).filter(Boolean);
    parts.pop();
    return parts.join(SEP);
  }

  const trimmed = trimTrailingSep(path);
  if (!trimmed || trimmed === "/" || /^[A-Za-z]:[\\/]?$/.test(trimmed)) return trimmed;
  const slash = Math.max(trimmed.lastIndexOf("/"), trimmed.lastIndexOf("\\"));
  if (slash < 0) return "";
  if (slash === 0) return "/";
  const parent = trimmed.slice(0, slash);
  return /^[A-Za-z]:$/.test(parent) ? `${parent}${pathSep(trimmed)}` : parent;
}

function basename(path: string): string {
  const trimmed = trimTrailingSep(path);
  return trimmed.split(/[/\\]+/).filter(Boolean).pop() || trimmed || path;
}

function splitPath(path: string): string[] {
  return unifiedPath(path).split("/").filter(Boolean);
}

function fsRoot(path: string): string {
  const drive = path.match(/^[A-Za-z]:[\\/]?/);
  if (drive) return drive[0].endsWith("\\") || drive[0].endsWith("/") ? drive[0] : `${drive[0]}\\`;
  return path.startsWith("/") ? "/" : "";
}

function relativeSegments(path: string, root: string): string[] {
  if (!root || !pathWithin(path, root)) return splitPath(path);
  const p = unifiedPath(path);
  const r = unifiedPath(root);
  const rel = p.slice(r.length).replace(/^\/+/, "");
  return rel ? rel.split("/").filter(Boolean) : [];
}

function buildBreadcrumbs(path: string, mode: PathMode, rootLabel: string, rootPath?: string) {
  if (mode === "relative") {
    const parts = sanitizeRelativePath(path).split(SEP).filter(Boolean);
    const crumbs: { label: string; path: string; isRoot?: boolean }[] = [
      { label: rootLabel, path: "", isRoot: true },
    ];
    let acc = "";
    for (const p of parts) {
      acc = acc ? `${acc}${SEP}${p}` : p;
      crumbs.push({ label: p, path: acc });
    }
    return crumbs;
  }

  const root = rootPath || fsRoot(path);
  const rootCrumbPath = root || path;
  const crumbs: { label: string; path: string; isRoot?: boolean }[] = [
    { label: rootLabel, path: rootCrumbPath, isRoot: true },
  ];
  const segments = root ? relativeSegments(path, root) : splitPath(path);
  let acc = rootCrumbPath;
  for (const segment of segments) {
    if (!root && /^[A-Za-z]:$/.test(segment)) {
      acc = `${segment}\\`;
      continue;
    }
    acc = joinBrowserPath(acc, segment, "absolute");
    crumbs.push({ label: segment, path: acc });
  }
  return crumbs;
}

function getHistoryKind(state: unknown): HistoryKind | null {
  if (!state || typeof state !== "object") return null;
  const value = (state as Record<string, unknown>)[HISTORY_STATE_KEY];
  return value === "dir" || value === "view" ? value : null;
}

function getHistoryDepth(state: unknown): number | null {
  if (!state || typeof state !== "object") return null;
  const value = (state as Record<string, unknown>)[HISTORY_DEPTH_KEY];
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

function withHistoryKind(state: unknown, kind: HistoryKind, depth: number | null): Record<string, unknown> {
  const next: Record<string, unknown> = {
    ...(state && typeof state === "object" ? state as Record<string, unknown> : {}),
    [HISTORY_STATE_KEY]: kind,
  };
  if (depth !== null) next[HISTORY_DEPTH_KEY] = depth;
  return next;
}

function FolderIcon({ className = "" }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" fill="currentColor" className={className}>
      <path d="M3 6.75A2.25 2.25 0 015.25 4.5h3.19a2.25 2.25 0 011.59.66L11.56 6.56a.75.75 0 00.53.22h6.66A2.25 2.25 0 0121 9.03v8.72a2.25 2.25 0 01-2.25 2.25H5.25A2.25 2.25 0 013 17.75V6.75z" />
    </svg>
  );
}

function FileGlyph({ kind, className = "" }: { kind: FileKind; className?: string }) {
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

export function FileDataBrowser({
  dataSource,
  pathMode,
  pathParam,
  rootPath,
  rootLabel,
  title,
  subtitle,
  leading,
  showHeader = true,
  ready = true,
  onExit,
}: FileDataBrowserProps) {
  const navigate = useNavigate();
  const location = useLocation();
  const [searchParams, setSearchParams] = useSearchParams();
  const [entries, setEntries] = useState<DirEntry[]>([]);
  const [listedPath, setListedPath] = useState("");
  const [copyFolderPath, setCopyFolderPath] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [showHidden, setShowHidden] = useState(false);
  const [sortKey, setSortKey] = useState<SortKey>("name");
  const [sortDir, setSortDir] = useState<SortDir>("asc");
  const [filter, setFilter] = useState("");
  const [viewer, setViewer] = useState<ViewerState | null>(null);
  const [copied, setCopied] = useState(false);

  const activePath = useMemo(() => {
    const raw = searchParams.get(pathParam);
    if (raw === null || raw === "") return pathMode === "absolute" ? rootPath ?? "" : "";
    return normalizePath(raw, pathMode);
  }, [pathMode, pathParam, rootPath, searchParams]);

  const viewPath = useMemo(() => {
    const raw = searchParams.get(VIEW_PARAM);
    return raw ? normalizePath(raw, pathMode) : "";
  }, [pathMode, searchParams]);

  const browserPath = listedPath || activePath;
  const historyKind = getHistoryKind(location.state);
  const historyDepth = getHistoryDepth(location.state);

  const updateParams = useCallback((
    mutate: (params: URLSearchParams) => void,
    opts?: { replace?: boolean; historyKind?: HistoryKind | null; depth?: number | null },
  ) => {
    const params = new URLSearchParams(searchParams);
    mutate(params);
    const navOptions: { replace?: boolean; state?: unknown } = { replace: opts?.replace ?? false };
    if (opts?.historyKind) {
      navOptions.state = withHistoryKind(location.state, opts.historyKind, opts.depth ?? null);
    }
    setSearchParams(params, navOptions);
  }, [location.state, searchParams, setSearchParams]);

  const setPath = useCallback((next: string, opts?: { replace?: boolean; historyKind?: HistoryKind | null }) => {
    const clean = normalizePath(next, pathMode);
    updateParams((params) => {
      const isRoot = pathMode === "relative"
        ? clean === ""
        : rootPath
          ? samePath(clean, rootPath)
          : false;
      if (isRoot) params.delete(pathParam);
      else params.set(pathParam, clean);
      params.delete(VIEW_PARAM);
    }, {
      replace: opts?.replace,
      historyKind: opts?.historyKind === undefined ? "dir" : opts.historyKind,
      depth: opts?.historyKind === undefined && historyDepth !== null ? historyDepth + 1 : null,
    });
  }, [historyDepth, pathMode, pathParam, rootPath, updateParams]);

  const openView = useCallback((next: string) => {
    const clean = normalizePath(next, pathMode);
    updateParams((params) => {
      params.set(VIEW_PARAM, clean);
    }, { historyKind: "view", depth: historyDepth !== null ? historyDepth + 1 : null });
  }, [historyDepth, pathMode, updateParams]);

  const closeViewer = useCallback(() => {
    if (historyKind === "view") {
      navigate(-1);
      return;
    }
    updateParams((params) => params.delete(VIEW_PARAM), { replace: true, historyKind: null });
  }, [historyKind, navigate, updateParams]);

  useEffect(() => {
    if (!ready) {
      setEntries([]);
      setListedPath("");
      setCopyFolderPath("");
      setLoading(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setError(null);
    dataSource
      .list(activePath, showHidden)
      .then((result) => {
        if (cancelled) return;
        setEntries(result.entries);
        setListedPath(result.path);
        setCopyFolderPath(result.absPath ?? result.path);
      })
      .catch((e) => {
        if (cancelled) return;
        setError(e instanceof Error ? e.message : String(e));
        setEntries([]);
        setListedPath("");
        setCopyFolderPath("");
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [activePath, dataSource, ready, showHidden]);

  useEffect(() => {
    if (!ready || !viewPath) {
      setViewer(null);
      return;
    }
    let cancelled = false;
    const name = basename(viewPath);
    setViewer({ path: viewPath, name, loading: true });
    dataSource
      .view(viewPath)
      .then((v) => {
        if (!cancelled) setViewer({ path: viewPath, name, view: v, loading: false });
      })
      .catch((err) => {
        if (!cancelled) {
          setViewer({
            path: viewPath,
            name,
            error: err instanceof Error ? err.message : String(err),
            loading: false,
          });
        }
      });
    return () => {
      cancelled = true;
    };
  }, [dataSource, ready, viewPath]);

  const canGoUp = useMemo(() => {
    if (!browserPath) return false;
    if (pathMode === "relative") return browserPath !== "";
    if (rootPath) return !samePath(browserPath, rootPath);
    const parent = parentBrowserPath(browserPath, pathMode);
    return parent !== "" && !samePath(parent, browserPath);
  }, [browserPath, pathMode, rootPath]);

  const handleBack = useCallback(() => {
    if (viewPath) {
      closeViewer();
      return;
    }
    if (canGoUp) {
      if (historyKind === "dir") {
        navigate(-1);
        return;
      }
      setPath(parentBrowserPath(browserPath, pathMode), { replace: true, historyKind: null });
      return;
    }
    if (historyDepth === 0) {
      navigate(-1);
      return;
    }
    onExit();
  }, [browserPath, canGoUp, closeViewer, historyDepth, historyKind, navigate, onExit, pathMode, setPath, viewPath]);

  const breadcrumbs = useMemo(
    () => buildBreadcrumbs(browserPath, pathMode, rootLabel, rootPath),
    [browserPath, pathMode, rootLabel, rootPath],
  );

  const sortedEntries = useMemo(() => {
    const q = filter.trim().toLowerCase();
    const filtered = q ? entries.filter((e) => e.name.toLowerCase().includes(q)) : entries;
    return [...filtered].sort((a, b) => {
      if (a.type !== b.type) return a.type === "dir" ? -1 : 1;
      let cmp = 0;
      switch (sortKey) {
        case "name":
          cmp = a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: "base" });
          break;
        case "size":
          cmp = (a.size ?? 0) - (b.size ?? 0);
          break;
        case "modified":
          cmp = new Date(a.modTime).getTime() - new Date(b.modTime).getTime();
          break;
      }
      return sortDir === "asc" ? cmp : -cmp;
    });
  }, [entries, filter, sortDir, sortKey]);

  const onEntryClick = (entry: DirEntry) => {
    const current = browserPath || activePath;
    const next = joinBrowserPath(current, entry.name, pathMode);
    if (entry.type === "dir") setPath(next);
    else openView(next);
  };

  const toggleSort = (key: SortKey) => {
    if (sortKey === key) {
      setSortDir((d) => (d === "asc" ? "desc" : "asc"));
    } else {
      setSortKey(key);
      setSortDir(key === "name" ? "asc" : "desc");
    }
  };

  const copyText = async (text: string) => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      setTimeout(() => setCopied(false), 1400);
    } catch {}
  };

  return (
    <div className="flex flex-col h-full min-h-full bg-neutral-950 text-neutral-200 relative">
      {showHeader && (
        <header className="flex items-center gap-3 px-4 py-3 border-b border-neutral-800 shrink-0">
          <button
            onClick={handleBack}
            className="text-neutral-400 hover:text-neutral-200"
            aria-label="Back"
          >
            &larr;
          </button>
          {leading}
          <div className="flex-1 min-w-0">
            <div className="font-medium text-sm truncate">{title}</div>
            {subtitle && <div className="text-[11px] text-neutral-500">{subtitle}</div>}
          </div>
          <button
            onClick={() => copyFolderPath && copyText(copyFolderPath)}
            disabled={!copyFolderPath}
            className="text-[11px] px-2 py-1 rounded bg-neutral-900 hover:bg-neutral-800 text-neutral-400 border border-neutral-800 disabled:opacity-40"
            title="Copy absolute path of current folder"
          >
            {copied ? "copied" : "copy path"}
          </button>
        </header>
      )}

      <div className="flex items-center gap-1 px-4 py-2 border-b border-neutral-900 overflow-x-auto shrink-0 text-xs">
        {!showHeader && (
          <button
            onClick={handleBack}
            disabled={!canGoUp}
            className="mr-1 text-neutral-500 hover:text-neutral-300 disabled:opacity-30 disabled:hover:text-neutral-500"
            aria-label="Back"
          >
            &larr;
          </button>
        )}
        {breadcrumbs.map((c, i) => {
          const last = i === breadcrumbs.length - 1;
          return (
            <div key={`${c.path}:${i}`} className="flex items-center gap-1 shrink-0">
              {i > 0 && <span className="text-neutral-700">/</span>}
              <button
                onClick={() => {
                  if (last) return;
                  if (historyDepth !== null && i < historyDepth) {
                    navigate(i - historyDepth);
                    return;
                  }
                  setPath(c.path, { replace: true, historyKind: null });
                }}
                disabled={last}
                className={`flex items-center gap-1 ${
                  last ? "text-neutral-200" : "text-neutral-500 hover:text-neutral-300"
                }`}
              >
                {c.isRoot && <FolderIcon className="w-3.5 h-3.5 text-blue-400" />}
                <span>{c.label}</span>
              </button>
            </div>
          );
        })}
      </div>

      <div className="flex items-center gap-2 px-4 py-2 border-b border-neutral-900 shrink-0">
        <div className="relative flex-1 min-w-0">
          <svg
            xmlns="http://www.w3.org/2000/svg"
            viewBox="0 0 20 20"
            fill="currentColor"
            className="w-4 h-4 absolute left-2.5 top-1/2 -translate-y-1/2 text-neutral-600"
          >
            <path fillRule="evenodd" d="M9 3.5a5.5 5.5 0 100 11 5.5 5.5 0 000-11zM2 9a7 7 0 1112.452 4.391l3.328 3.329a.75.75 0 11-1.06 1.06l-3.329-3.328A7 7 0 012 9z" clipRule="evenodd" />
          </svg>
          <input
            type="text"
            placeholder="Filter..."
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            className="w-full pl-8 pr-2 py-1.5 bg-neutral-900 border border-neutral-800 rounded-lg text-xs focus:outline-none focus:border-neutral-600"
          />
        </div>
        <SortButton label="Name" active={sortKey === "name"} dir={sortDir} onClick={() => toggleSort("name")} />
        <SortButton label="Size" active={sortKey === "size"} dir={sortDir} onClick={() => toggleSort("size")} />
        <SortButton label="Mod" active={sortKey === "modified"} dir={sortDir} onClick={() => toggleSort("modified")} />
        <button
          onClick={() => setShowHidden((h) => !h)}
          className={`text-[11px] px-2 py-1 rounded border ${
            showHidden
              ? "bg-neutral-700/60 text-neutral-200 border-neutral-600"
              : "bg-neutral-900 text-neutral-500 border-neutral-800 hover:text-neutral-300"
          }`}
          title="Toggle hidden files"
        >
          .*
        </button>
      </div>

      <div className="flex-1 overflow-y-auto">
        {error ? (
          <div className="px-4 py-8 text-center text-red-400 text-sm">{error}</div>
        ) : (!ready || loading) && entries.length === 0 ? (
          <div className="px-4 py-16 text-center text-neutral-600 text-sm">Loading...</div>
        ) : sortedEntries.length === 0 ? (
          <div className="px-4 py-16 text-center text-neutral-600 text-sm">
            {filter ? "No matches." : "Empty folder."}
          </div>
        ) : (
          <ul className="divide-y divide-neutral-900">
            {sortedEntries.map((entry) => (
              <EntryRow
                key={entry.name}
                dataSource={dataSource}
                entry={entry}
                parentPath={browserPath}
                pathMode={pathMode}
                onClick={() => onEntryClick(entry)}
              />
            ))}
          </ul>
        )}
      </div>

      {viewer && (
        <FileViewer
          dataSource={dataSource}
          state={viewer}
          onClose={closeViewer}
          onCopyPath={() => {
            const copyPath = viewer.view?.absPath ?? viewer.view?.path;
            if (copyPath) copyText(copyPath);
          }}
          copied={copied}
        />
      )}
    </div>
  );
}

function SortButton({
  label,
  active,
  dir,
  onClick,
}: {
  label: string;
  active: boolean;
  dir: SortDir;
  onClick: () => void;
}) {
  return (
    <button
      onClick={onClick}
      className={`text-[11px] px-2 py-1 rounded border shrink-0 flex items-center gap-1 ${
        active
          ? "bg-neutral-800 text-neutral-200 border-neutral-700"
          : "bg-neutral-900 text-neutral-500 border-neutral-800 hover:text-neutral-300"
      }`}
    >
      {label}
      {active && (
        <svg
          viewBox="0 0 10 10"
          fill="currentColor"
          className={`w-2.5 h-2.5 transition-transform ${dir === "desc" ? "rotate-180" : ""}`}
        >
          <path d="M5 2l3 4H2l3-4z" />
        </svg>
      )}
    </button>
  );
}

function EntryRow({
  dataSource,
  entry,
  parentPath,
  pathMode,
  onClick,
}: {
  dataSource: FileDataSource;
  entry: DirEntry;
  parentPath: string;
  pathMode: PathMode;
  onClick: () => void;
}) {
  const kind = entry.type === "file" ? fileKind(entry.name) : null;
  const style = kind ? KIND_STYLES[kind] : null;
  const showThumb = entry.type === "file" && isImage(entry.name);
  const path = joinBrowserPath(parentPath, entry.name, pathMode);
  // The thumb endpoint only handles png/jpeg/gif/webp. svg/bmp/avif/ico
  // skip the thumb and load the raw directly — usually small anyway,
  // and the resize would only marginally help.
  const ext = entry.name.toLowerCase().split(".").pop() ?? "";
  const thumbSupported = ext === "png" || ext === "jpg" || ext === "jpeg" || ext === "gif" || ext === "webp";
  const thumbSrc = showThumb && thumbSupported && dataSource.thumbUrl
    ? dataSource.thumbUrl(path, 96, entry.modTime)
    : dataSource.rawUrl(path);
  const meta =
    entry.type === "dir"
      ? timeAgo(entry.modTime)
      : `${formatSize(entry.size ?? 0)} · ${timeAgo(entry.modTime)}`;

  return (
    <li>
      <button
        onClick={onClick}
        className="w-full text-left px-4 py-2.5 hover:bg-neutral-900/80 active:bg-neutral-900 flex items-center gap-3 transition-colors"
      >
        <div className={`shrink-0 w-10 h-10 rounded-lg flex items-center justify-center overflow-hidden ${
          entry.type === "dir" ? "bg-blue-500/10" : style?.bg ?? "bg-neutral-800"
        }`}>
          {entry.type === "dir" ? (
            <FolderIcon className="w-6 h-6 text-blue-400" />
          ) : showThumb ? (
            <img
              src={thumbSrc}
              alt=""
              className="w-full h-full object-cover"
              loading="lazy"
              decoding="async"
              onError={(e) => {
                // We only hit thumb for supported extensions, so a
                // failure here is most likely a 413 (too big) or
                // a transient server error. In either case, falling
                // back to the raw is wrong — 413 implies a huge file
                // that would defeat the bandwidth-saving purpose, and
                // for transient errors the list view can just show
                // the placeholder. Hide instead of trying raw.
                (e.currentTarget as HTMLImageElement).style.display = "none";
              }}
            />
          ) : (
            <FileGlyph kind={kind!} className={`w-5 h-5 ${style?.icon}`} />
          )}
        </div>
        <div className="flex-1 min-w-0">
          <div className="text-sm truncate">{entry.name}</div>
          <div className="text-[11px] text-neutral-500">{meta}</div>
        </div>
        <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-4 h-4 text-neutral-700 shrink-0">
          <path fillRule="evenodd" d="M7.21 14.77a.75.75 0 01.02-1.06L11.168 10 7.23 6.29a.75.75 0 111.04-1.08l4.5 4.25a.75.75 0 010 1.08l-4.5 4.25a.75.75 0 01-1.06-.02z" clipRule="evenodd" />
        </svg>
      </button>
    </li>
  );
}

function FileViewer({
  dataSource,
  state,
  onClose,
  onCopyPath,
  copied,
}: {
  dataSource: FileDataSource;
  state: ViewerState;
  onClose: () => void;
  onCopyPath: () => void;
  copied: boolean;
}) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const { view, error, loading, name, path } = state;
  const isMarkdown = view?.type === "text" && view.language === "markdown";
  const [mdView, setMdView] = useState<"rendered" | "raw">("rendered");
  useEffect(() => {
    setMdView("rendered");
  }, [path]);

  return (
    <div className="absolute inset-0 bg-neutral-950 z-10 flex flex-col">
      <header className="flex items-center gap-2 px-4 py-3 border-b border-neutral-800 shrink-0">
        <button onClick={onClose} className="text-neutral-400 hover:text-neutral-200" aria-label="Back">
          &larr;
        </button>
        <div className="flex-1 min-w-0">
          <div className="text-sm truncate">{name}</div>
          {view && (
            <div className="text-[11px] text-neutral-500">
              {formatSize(view.size)}
              {view.language ? ` · ${view.language}` : view.mime ? ` · ${view.mime}` : ""}
            </div>
          )}
        </div>
        {isMarkdown && (
          <div className="flex items-center rounded border border-neutral-800 overflow-hidden shrink-0">
            <button
              onClick={() => setMdView("rendered")}
              className={`text-[11px] px-2 py-1 ${
                mdView === "rendered"
                  ? "bg-neutral-800 text-neutral-200"
                  : "bg-neutral-900 text-neutral-500 hover:text-neutral-300"
              }`}
              title="Render markdown"
            >
              md
            </button>
            <button
              onClick={() => setMdView("raw")}
              className={`text-[11px] px-2 py-1 border-l border-neutral-800 ${
                mdView === "raw"
                  ? "bg-neutral-800 text-neutral-200"
                  : "bg-neutral-900 text-neutral-500 hover:text-neutral-300"
              }`}
              title="Show raw source"
            >
              raw
            </button>
          </div>
        )}
        <button
          onClick={onCopyPath}
          className="text-[11px] px-2 py-1 rounded bg-neutral-900 hover:bg-neutral-800 text-neutral-400 border border-neutral-800"
        >
          {copied ? "copied" : "copy path"}
        </button>
        <a
          href={dataSource.rawUrl(path, true)}
          className="text-[11px] px-2 py-1 rounded bg-neutral-900 hover:bg-neutral-800 text-neutral-400 border border-neutral-800"
          title="Download"
        >
          download
        </a>
      </header>
      <div className="flex-1 overflow-auto">
        {loading ? (
          <div className="px-4 py-16 text-center text-neutral-600 text-sm">Loading...</div>
        ) : error ? (
          <div className="px-4 py-16 text-center text-sm space-y-3">
            <div className="text-neutral-400">{error}</div>
            <a
              href={dataSource.rawUrl(path, true)}
              className="inline-block px-3 py-1.5 text-xs rounded border border-neutral-800 bg-neutral-900 text-neutral-300 hover:bg-neutral-800"
            >
              Download instead
            </a>
          </div>
        ) : isMarkdown && mdView === "rendered" ? (
          <div className="px-4 py-4 text-sm">
            <MarkdownRenderer content={view!.content ?? ""} />
          </div>
        ) : view?.type === "text" ? (
          <pre className="text-[12px] leading-relaxed font-mono p-4 whitespace-pre-wrap break-words text-neutral-200">
            {view.content}
          </pre>
        ) : view?.type === "image" ? (
          <div className="flex items-center justify-center p-4 min-h-full">
            <img
              src={dataSource.rawUrl(path)}
              alt={name}
              className="max-w-full max-h-full object-contain rounded"
            />
          </div>
        ) : null}
      </div>
    </div>
  );
}

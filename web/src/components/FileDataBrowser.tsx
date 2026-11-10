import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useLocation, useNavigate, useSearchParams } from "react-router";
import { isThumbSupported, type DirEntry, type FileView } from "../lib/api";
import { PreconditionFailedError } from "../lib/httpClient";
import { errMsg, formatSize, timeAgo } from "../lib/utils";
import {
  basename,
  buildBreadcrumbs,
  joinBrowserPath,
  normalizePath,
  parentBrowserPath,
  samePath,
  type PathMode,
} from "../lib/browserPath";
import { fileKind, FileGlyph, FolderIcon, isImage, KIND_STYLES } from "./fileIcons";
import { MarkdownRenderer } from "./agent/MarkdownRenderer";
import { useT } from "../lib/i18n";

type SortKey = "name" | "size" | "modified";
type SortDir = "asc" | "desc";
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
  // Optional text save. When present, text files get an "edit" button.
  // `etag` is the one from the view response; a stale one must reject
  // with PreconditionFailedError.
  save?: (path: string, content: string, etag: string) => Promise<FileView>;
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

interface ViewerState {
  path: string;
  name: string;
  view?: FileView;
  error?: string;
  loading: boolean;
}

const VIEW_PARAM = "view";
// Directory and preview navigation both live in URL search params. These
// history markers let the back button pop viewer-local entries instead of
// leaving stale file-browser routes behind the chat screen.
const HISTORY_STATE_KEY = "kojoFileBrowser";
const HISTORY_DEPTH_KEY = "kojoFileBrowserDepth";

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
  const t = useT();
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
  // Bumped after a save (list sizes/mtimes change) and on "load latest"
  // after a save conflict.
  const [listReload, setListReload] = useState(0);
  const [viewReload, setViewReload] = useState(0);

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
        setError(errMsg(e));
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
  }, [activePath, dataSource, ready, showHidden, listReload]);

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
            error: errMsg(err),
            loading: false,
          });
        }
      });
    return () => {
      cancelled = true;
    };
  }, [dataSource, ready, viewPath, viewReload]);

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
    <div className="relative flex h-full min-h-full flex-col bg-app text-ink">
      {showHeader && (
        <header className="flex shrink-0 items-center gap-3 border-b border-hairline px-4 py-3">
          <button
            onClick={handleBack}
            className="-ml-1 flex h-8 w-8 shrink-0 items-center justify-center rounded-[10px] text-ink-dim transition-colors hover:bg-hover hover:text-ink"
            aria-label={t("common.back")}
          >
            <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" className="h-5 w-5">
              <path d="M12.5 15l-5-5 5-5" />
            </svg>
          </button>
          {leading}
          <div className="min-w-0 flex-1">
            <div className="truncate text-sm font-medium text-ink">{title}</div>
            {subtitle && <div className="text-[11px] text-ink-faint">{subtitle}</div>}
          </div>
          <button
            onClick={() => copyFolderPath && copyText(copyFolderPath)}
            disabled={!copyFolderPath}
            className="rounded-[10px] border border-hairline bg-raised px-2 py-1 font-mono text-[11px] text-ink-dim transition-colors hover:bg-hover hover:text-ink disabled:opacity-40"
            title={t("fdb.copyFolderTitle")}
          >
            {copied ? t("fdb.copiedLower") : t("fdb.copyPath")}
          </button>
        </header>
      )}

      <div className="flex shrink-0 items-center gap-1 overflow-x-auto border-b border-hairline px-4 py-2 font-mono text-[12px]">
        {!showHeader && (
          <button
            onClick={handleBack}
            disabled={!canGoUp}
            className="mr-1 flex h-6 w-6 shrink-0 items-center justify-center rounded text-ink-faint transition-colors hover:text-ink-dim disabled:opacity-30 disabled:hover:text-ink-faint"
            aria-label={t("common.back")}
          >
            <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" className="h-4 w-4">
              <path d="M12.5 15l-5-5 5-5" />
            </svg>
          </button>
        )}
        {breadcrumbs.map((c, i) => {
          const last = i === breadcrumbs.length - 1;
          return (
            <div key={`${c.path}:${i}`} className="flex shrink-0 items-center gap-1">
              {i > 0 && <span className="text-ink-faint">/</span>}
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
                  last ? "text-copper" : "text-ink-faint hover:text-ink-dim"
                }`}
              >
                {c.isRoot && <FolderIcon className="h-3.5 w-3.5 text-blue-400" />}
                <span>{c.label}</span>
              </button>
            </div>
          );
        })}
      </div>

      <div className="flex shrink-0 items-center gap-2 border-b border-hairline px-4 py-2">
        <div className="relative min-w-0 flex-1">
          <svg
            xmlns="http://www.w3.org/2000/svg"
            viewBox="0 0 20 20"
            fill="currentColor"
            className="absolute left-2.5 top-1/2 h-4 w-4 -translate-y-1/2 text-ink-faint"
          >
            <path fillRule="evenodd" d="M9 3.5a5.5 5.5 0 100 11 5.5 5.5 0 000-11zM2 9a7 7 0 1112.452 4.391l3.328 3.329a.75.75 0 11-1.06 1.06l-3.329-3.328A7 7 0 012 9z" clipRule="evenodd" />
          </svg>
          <input
            type="text"
            placeholder={t("fdb.filterPlaceholder")}
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            className="w-full rounded-lg border border-hairline bg-raised py-1.5 pl-8 pr-2 text-[12px] text-ink placeholder:text-ink-faint transition-colors focus:border-copper focus:outline-none"
          />
        </div>
        <SortButton label={t("fdb.sortName")} active={sortKey === "name"} dir={sortDir} onClick={() => toggleSort("name")} />
        <SortButton label={t("fdb.sortSize")} active={sortKey === "size"} dir={sortDir} onClick={() => toggleSort("size")} />
        <SortButton label={t("fdb.sortMod")} active={sortKey === "modified"} dir={sortDir} onClick={() => toggleSort("modified")} />
        <button
          onClick={() => setShowHidden((h) => !h)}
          className={`shrink-0 rounded-lg border px-2 py-1 font-mono text-[11px] transition-colors ${
            showHidden
              ? "border-copper/40 bg-copper/10 text-copper"
              : "border-hairline bg-raised text-ink-faint hover:text-ink-dim"
          }`}
          title={t("fdb.toggleHidden")}
        >
          .*
        </button>
      </div>

      <div className="flex-1 overflow-y-auto">
        {error ? (
          <div className="px-4 py-8 text-center text-sm text-lamp-err">{error}</div>
        ) : (!ready || loading) && entries.length === 0 ? (
          <div className="px-4 py-16 text-center text-sm text-ink-faint">{t("fdb.loading")}</div>
        ) : sortedEntries.length === 0 ? (
          <div className="px-4 py-16 text-center text-sm text-ink-faint">
            {filter ? t("fdb.noMatches") : t("fdb.emptyFolder")}
          </div>
        ) : (
          <ul className="divide-y divide-hairline">
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
          onSaved={(v) => {
            setViewer((prev) =>
              prev && prev.path === viewer.path
                ? { ...prev, view: { ...prev.view, ...v, path: prev.view?.path ?? v.path } }
                : prev,
            );
            setListReload((n) => n + 1);
          }}
          onReload={() => setViewReload((n) => n + 1)}
          draftKey={`${location.pathname}\n${viewer.path}`}
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
      className={`flex shrink-0 items-center gap-1 rounded-lg border px-2 py-1 font-mono text-[11px] transition-colors ${
        active
          ? "border-copper/40 bg-copper/10 text-copper"
          : "border-hairline bg-raised text-ink-faint hover:text-ink-dim"
      }`}
    >
      {label}
      {active && (
        <svg
          viewBox="0 0 10 10"
          fill="currentColor"
          className={`h-2.5 w-2.5 transition-transform motion-reduce:transition-none ${dir === "desc" ? "rotate-180" : ""}`}
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
  const thumbSrc = showThumb && isThumbSupported(entry.name) && dataSource.thumbUrl
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
        className="flex w-full items-center gap-3 px-4 py-2.5 text-left transition-colors hover:bg-hover active:bg-hover"
      >
        <div className={`flex h-10 w-10 shrink-0 items-center justify-center overflow-hidden rounded-lg ${
          entry.type === "dir" ? "bg-blue-500/10" : style?.bg ?? "bg-surface"
        }`}>
          {entry.type === "dir" ? (
            <FolderIcon className="h-6 w-6 text-blue-400" />
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
        <div className="min-w-0 flex-1">
          <div className="truncate text-[14px] text-ink">{entry.name}</div>
          <div className="font-mono text-[11px] text-ink-faint">{meta}</div>
        </div>
        <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="h-4 w-4 shrink-0 text-ink-faint">
          <path fillRule="evenodd" d="M7.21 14.77a.75.75 0 01.02-1.06L11.168 10 7.23 6.29a.75.75 0 111.04-1.08l4.5 4.25a.75.75 0 010 1.08l-4.5 4.25a.75.75 0 01-1.06-.02z" clipRule="evenodd" />
        </svg>
      </button>
    </li>
  );
}

// Editor drafts are parked in sessionStorage (per tab) keyed by route +
// file path, so they never outlive the browser tab.
const DRAFT_PREFIX = "kojo.fdb.draft:";
type StoredDraft = { etag: string; content: string };

function readDraft(key: string): StoredDraft | null {
  try {
    const raw = sessionStorage.getItem(DRAFT_PREFIX + key);
    if (!raw) return null;
    const v = JSON.parse(raw) as Partial<StoredDraft>;
    return typeof v.etag === "string" && typeof v.content === "string" ? { etag: v.etag, content: v.content } : null;
  } catch {
    return null;
  }
}

function writeDraft(key: string, draft: StoredDraft) {
  try {
    sessionStorage.setItem(DRAFT_PREFIX + key, JSON.stringify(draft));
  } catch {
    // Quota / private mode: the draft just isn't preserved.
  }
}

function clearDraft(key: string) {
  try {
    sessionStorage.removeItem(DRAFT_PREFIX + key);
  } catch {
    // ignore
  }
}

function FileViewer({
  dataSource,
  state,
  onClose,
  onSaved,
  onReload,
  onCopyPath,
  copied,
  draftKey,
}: {
  dataSource: FileDataSource;
  state: ViewerState;
  draftKey: string;
  onClose: () => void;
  onSaved: (view: FileView) => void;
  onReload: () => void;
  onCopyPath: () => void;
  copied: boolean;
}) {
  const t = useT();
  const { view, error, loading, name, path } = state;
  const isMarkdown = view?.type === "text" && view.language === "markdown";
  const [mdView, setMdView] = useState<"rendered" | "raw">("rendered");

  // ── editor ──
  // `baseEtag` is the version the draft was started from; saves assert it
  // via If-Match so a newer on-disk write is never clobbered silently.
  const canEdit = view?.type === "text" && !!view.etag && !!dataSource.save;
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState("");
  const [baseEtag, setBaseEtag] = useState("");
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [conflict, setConflict] = useState(false);
  const [savedFlash, setSavedFlash] = useState(false);
  const dirty = editing && draft !== (view?.content ?? "");

  useEffect(() => {
    setMdView("rendered");
    setEditing(false);
    setSaveError(null);
    setConflict(false);
  }, [path]);

  // Unsaved drafts survive leaving the viewer through browser history
  // (swipe-back can't be intercepted with BrowserRouter): reopening the
  // file drops straight back into the editor with the draft.
  const restoredFor = useRef<string | null>(null);
  useEffect(() => {
    if (!view?.etag || restoredFor.current === draftKey) return;
    restoredFor.current = draftKey;
    const stored = readDraft(draftKey);
    if (!stored) return;
    setDraft(stored.content);
    setBaseEtag(stored.etag);
    setEditing(true);
  }, [draftKey, view?.etag]);

  // Debounced write; anything still pending is flushed on unmount so a
  // back-swipe right after typing keeps the last keystrokes.
  const pendingDraft = useRef<{ key: string; draft: StoredDraft } | null>(null);
  useEffect(() => {
    if (!editing) return;
    if (!dirty) {
      pendingDraft.current = null;
      clearDraft(draftKey);
      return;
    }
    pendingDraft.current = { key: draftKey, draft: { etag: baseEtag, content: draft } };
    const timer = setTimeout(() => {
      writeDraft(draftKey, { etag: baseEtag, content: draft });
      pendingDraft.current = null;
    }, 300);
    return () => clearTimeout(timer);
  }, [baseEtag, draft, draftKey, dirty, editing]);
  useEffect(() => () => {
    const p = pendingDraft.current;
    if (p) writeDraft(p.key, p.draft);
  }, []);

  const startEdit = () => {
    if (!view?.etag) return;
    setDraft(view.content ?? "");
    setBaseEtag(view.etag);
    setSaveError(null);
    setConflict(false);
    setEditing(true);
  };

  const confirmDiscard = useCallback(
    () => !dirty || window.confirm(t("fdb.discardConfirm")),
    [dirty, t],
  );

  const endEdit = useCallback(() => {
    pendingDraft.current = null;
    clearDraft(draftKey);
    setEditing(false);
    setSaveError(null);
    setConflict(false);
  }, [draftKey]);

  const stopEdit = useCallback(() => {
    if (confirmDiscard()) endEdit();
  }, [confirmDiscard, endEdit]);

  const doSave = useCallback(async (etag: string) => {
    if (!dataSource.save) return;
    setSaving(true);
    setSaveError(null);
    setConflict(false);
    try {
      const saved = await dataSource.save(path, draft, etag);
      pendingDraft.current = null;
      clearDraft(draftKey);
      setBaseEtag(saved.etag ?? "");
      onSaved(saved);
      setSavedFlash(true);
      setTimeout(() => setSavedFlash(false), 1400);
    } catch (e) {
      if (e instanceof PreconditionFailedError) setConflict(true);
      else setSaveError(errMsg(e));
    } finally {
      setSaving(false);
    }
  }, [dataSource, draft, draftKey, onSaved, path]);

  const save = useCallback(() => {
    if (saving || !baseEtag || !dirty) return;
    void doSave(baseEtag);
  }, [baseEtag, dirty, doSave, saving]);

  // Conflict resolution: fetch the current etag and write over it.
  const overwrite = async () => {
    setSaving(true);
    setSaveError(null);
    try {
      const latest = await dataSource.view(path);
      if (latest.type !== "text" || !latest.etag) throw new Error(t("fdb.notEditable"));
      setSaving(false);
      await doSave(latest.etag);
    } catch (e) {
      setSaving(false);
      setSaveError(errMsg(e));
    }
  };

  const loadLatest = () => {
    if (!confirmDiscard()) return;
    endEdit();
    onReload();
  };

  const handleClose = useCallback(() => {
    if (editing) {
      if (!confirmDiscard()) return;
      pendingDraft.current = null;
      clearDraft(draftKey);
    }
    onClose();
  }, [confirmDiscard, draftKey, editing, onClose]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        if (editing) stopEdit();
        else onClose();
        return;
      }
      if (editing && (e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "s") {
        e.preventDefault();
        save();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [editing, onClose, save, stopEdit]);

  useEffect(() => {
    if (!dirty) return;
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      e.preventDefault();
    };
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, [dirty]);

  const btn =
    "shrink-0 rounded-lg border border-hairline bg-raised px-2 py-1 font-mono text-[11px] text-ink-dim transition-colors hover:bg-hover hover:text-ink disabled:opacity-40";

  return (
    <div className="absolute inset-0 z-10 flex flex-col bg-app">
      <header className="flex shrink-0 items-center gap-2 border-b border-hairline px-4 py-3">
        <button
          onClick={handleClose}
          className="-ml-1 flex h-8 w-8 shrink-0 items-center justify-center rounded-[10px] text-ink-dim transition-colors hover:bg-hover hover:text-ink"
          aria-label={t("common.back")}
        >
          <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" className="h-5 w-5">
            <path d="M12.5 15l-5-5 5-5" />
          </svg>
        </button>
        <div className="min-w-0 flex-1">
          <div className="truncate text-sm text-ink">{name}</div>
          {view && (
            <div className="font-mono text-[11px] text-ink-faint">
              {editing
                ? dirty
                  ? t("fdb.unsaved")
                  : savedFlash
                    ? t("fdb.saved")
                    : t("fdb.editing")
                : <>
                    {formatSize(view.size)}
                    {view.language ? ` · ${view.language}` : view.mime ? ` · ${view.mime}` : ""}
                  </>}
            </div>
          )}
        </div>
        {editing ? (
          <>
            <button onClick={stopEdit} disabled={saving} className={btn}>
              {dirty ? t("fdb.cancelEdit") : t("fdb.doneEdit")}
            </button>
            <button
              onClick={save}
              disabled={saving || !dirty}
              className="shrink-0 rounded-lg border border-copper/40 bg-copper/10 px-2 py-1 font-mono text-[11px] text-copper transition-colors hover:bg-copper/20 disabled:opacity-40"
            >
              {saving ? t("fdb.saving") : t("fdb.save")}
            </button>
          </>
        ) : (<>
        {isMarkdown && (
          <div className="flex shrink-0 items-center overflow-hidden rounded-lg border border-hairline">
            <button
              onClick={() => setMdView("rendered")}
              className={`px-2 py-1 font-mono text-[11px] transition-colors ${
                mdView === "rendered"
                  ? "bg-copper/10 text-copper"
                  : "bg-raised text-ink-faint hover:text-ink-dim"
              }`}
              title={t("fdb.renderMd")}
            >
              md
            </button>
            <button
              onClick={() => setMdView("raw")}
              className={`border-l border-hairline px-2 py-1 font-mono text-[11px] transition-colors ${
                mdView === "raw"
                  ? "bg-copper/10 text-copper"
                  : "bg-raised text-ink-faint hover:text-ink-dim"
              }`}
              title={t("fdb.showRawSource")}
            >
              raw
            </button>
          </div>
        )}
        <button
          onClick={onCopyPath}
          className="rounded-lg border border-hairline bg-raised px-2 py-1 font-mono text-[11px] text-ink-dim transition-colors hover:bg-hover hover:text-ink"
        >
          {copied ? t("fdb.copiedLower") : t("fdb.copyPath")}
        </button>
        <a
          href={dataSource.rawUrl(path, true)}
          className="rounded-lg border border-hairline bg-raised px-2 py-1 font-mono text-[11px] text-ink-dim transition-colors hover:bg-hover hover:text-ink"
          title={t("media.download")}
        >
          {t("fdb.downloadLower")}
        </a>
        {canEdit && (
          <button onClick={startEdit} className={btn}>
            {t("fdb.edit")}
          </button>
        )}
        </>)}
      </header>
      {editing && (conflict || saveError) && (
        <div className="shrink-0 space-y-2 border-b border-hairline bg-raised px-4 py-2 text-xs text-ink-dim">
          {saveError && <div className="break-words">{saveError}</div>}
          {conflict && (
            <>
              <div>{t("fdb.conflict")}</div>
              <div className="flex gap-2">
                <button onClick={loadLatest} disabled={saving} className={btn}>
                  {t("fdb.loadLatest")}
                </button>
                <button onClick={overwrite} disabled={saving} className={btn}>
                  {t("fdb.overwrite")}
                </button>
              </div>
            </>
          )}
        </div>
      )}
      {editing ? (
        <textarea
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          spellCheck={false}
          autoCapitalize="off"
          autoCorrect="off"
          autoComplete="off"
          autoFocus
          aria-label={name}
          className="min-h-0 w-full flex-1 resize-none bg-app p-4 font-mono text-base leading-relaxed text-ink outline-none sm:text-[13px]"
        />
      ) : (
      <div className="flex-1 overflow-auto">
        {loading ? (
          <div className="px-4 py-16 text-center text-sm text-ink-faint">{t("fdb.loading")}</div>
        ) : error ? (
          <div className="space-y-3 px-4 py-16 text-center text-sm">
            <div className="text-ink-dim">{error}</div>
            <a
              href={dataSource.rawUrl(path, true)}
              className="inline-block rounded-lg border border-hairline bg-raised px-3 py-1.5 text-xs text-ink-dim transition-colors hover:bg-hover hover:text-ink"
            >
              {t("fdb.downloadInstead")}
            </a>
          </div>
        ) : isMarkdown && mdView === "rendered" ? (
          <div className="px-4 py-4 text-sm">
            <MarkdownRenderer content={view!.content ?? ""} />
          </div>
        ) : view?.type === "text" ? (
          <pre className="whitespace-pre-wrap break-words p-4 font-mono text-[12px] leading-relaxed text-ink">
            {view.content}
          </pre>
        ) : view?.type === "image" ? (
          <div className="flex min-h-full items-center justify-center p-4">
            <img
              src={dataSource.rawUrl(path)}
              alt={name}
              className="max-h-full max-w-full rounded-lg object-contain"
            />
          </div>
        ) : null}
      </div>
      )}
    </div>
  );
}

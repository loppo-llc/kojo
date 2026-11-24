import { useEffect, useMemo, useState } from "react";
import { authHeaders } from "../../lib/auth";
import { useT } from "../../lib/i18n";
import { formatSize } from "../../lib/utils";
import { MarkdownRenderer } from "./MarkdownRenderer";

// Text-ish attachments (kojo-attach .txt / .md / .csv / source / logs)
// open in an inline viewer instead of triggering a download. Only the
// head of the body is fetched so a multi-GB log can't wedge the tab.
export const TEXT_PREVIEW_MAX_BYTES = 2 * 1024 * 1024;
// Rendering huge Markdown synchronously can freeze the tab; past this
// size the viewer opens in Raw and rendering is opt-in.
const MARKDOWN_RENDER_MAX_CHARS = 256 * 1024;
const TABLE_MAX_ROWS = 1000;
const TABLE_MAX_COLS = 100;
const TABLE_MAX_CELLS = 50_000;

const TEXT_EXTS =
  /\.(txt|text|md|markdown|mdx|csv|tsv|log|json|jsonl|ndjson|ya?ml|toml|ini|cfg|conf|properties|xml|html?|css|scss|less|[cm]?js|jsx|tsx?|py|go|rs|rb|php|java|kt|kts|scala|swift|c|h|cc|cpp|cxx|hpp|cs|m|lua|pl|r|sql|sh|bash|zsh|fish|ps1|bat|diff|patch|srt|vtt|tex|rst|org|adoc|env|gradle|graphql|proto)$|(^|[/\\])(Dockerfile|Makefile|\.gitignore)$/i;
const TEXT_MIMES =
  /^(text\/|application\/(json|ld\+json|x-ndjson|xml|x-yaml|yaml|toml|javascript|x-javascript|typescript|x-sh|x-shellscript|sql|x-tex|csv)(\s*;|$))/i;

export type TextFormat = "markdown" | "csv" | "tsv" | "plain";

export function isTextAttachment(name: string, mime: string): boolean {
  return TEXT_MIMES.test(mime) || TEXT_EXTS.test(name);
}

export function textFormat(name: string, mime: string): TextFormat {
  const m = mime.toLowerCase();
  if (/\.(md|markdown|mdx)$/i.test(name) || m.startsWith("text/markdown")) return "markdown";
  if (/\.csv$/i.test(name) || m.startsWith("text/csv") || m.startsWith("application/csv")) return "csv";
  if (/\.tsv$/i.test(name) || m.startsWith("text/tab-separated-values")) return "tsv";
  return "plain";
}

// Drop a multi-byte UTF-8 sequence cut in half by the byte cap so the
// strict decoder doesn't reject an otherwise valid UTF-8 head.
function trimPartialUtf8(b: Uint8Array): Uint8Array {
  let i = b.length - 1;
  while (i >= 0 && b.length - i <= 4 && (b[i] & 0xc0) === 0x80) i--;
  if (i < 0) return b;
  const lead = b[i];
  const need = lead >= 0xf0 ? 4 : lead >= 0xe0 ? 3 : lead >= 0xc0 ? 2 : 1;
  return b.length - i < need ? b.subarray(0, i) : b;
}

function tryDecode(label: string, b: Uint8Array): string | null {
  try {
    return new TextDecoder(label, { fatal: true }).decode(b);
  } catch {
    return null;
  }
}

/**
 * Decode an attachment head. Returns null when the bytes look binary.
 * UTF-8 first, then Shift_JIS (Excel-exported CSVs on Japanese Windows),
 * falling back to lossy UTF-8.
 */
export function decodeTextBytes(bytes: Uint8Array, truncated: boolean): string | null {
  if (bytes[0] === 0xff && bytes[1] === 0xfe) return new TextDecoder("utf-16le").decode(bytes);
  if (bytes[0] === 0xfe && bytes[1] === 0xff) return new TextDecoder("utf-16be").decode(bytes);
  if (bytes.includes(0)) return null;
  const utf8 = truncated ? trimPartialUtf8(bytes) : bytes;
  return (
    tryDecode("utf-8", utf8) ??
    tryDecode("shift_jis", bytes) ??
    (truncated ? tryDecode("shift_jis", bytes.subarray(0, bytes.length - 1)) : null) ??
    new TextDecoder("utf-8").decode(utf8)
  );
}

/**
 * RFC 4180-ish parser: quoted fields, doubled quotes, CRLF/LF.
 * Limits are enforced while parsing so a pathological 2 MiB file can't
 * allocate millions of cells: fields past `maxCols` are dropped and
 * parsing stops once `maxRows` rows or `maxCells` cells are kept.
 * `more` reports any such clipping.
 */
export function parseDelimited(
  text: string,
  sep: string,
  limits: { maxRows: number; maxCols: number; maxCells: number },
): { rows: string[][]; more: boolean } {
  const rows: string[][] = [];
  let row: string[] = [];
  let field = "";
  let quoted = false;
  let more = false;
  let cells = 0;
  const pushField = () => {
    if (row.length < limits.maxCols && cells < limits.maxCells) {
      row.push(field);
      cells++;
    } else {
      more = true;
    }
    field = "";
  };
  const full = () => rows.length >= limits.maxRows || cells >= limits.maxCells;
  let i = 0;
  const n = text.length;
  while (i < n) {
    const c = text[i];
    if (quoted) {
      if (c === '"') {
        if (text[i + 1] === '"') {
          field += '"';
          i += 2;
          continue;
        }
        quoted = false;
      } else {
        field += c;
      }
      i++;
      continue;
    }
    if (c === '"' && field === "") {
      quoted = true;
    } else if (c === sep) {
      pushField();
    } else if (c === "\n" || c === "\r") {
      pushField();
      rows.push(row);
      row = [];
      if (c === "\r" && text[i + 1] === "\n") i++;
      if (full()) return { rows, more: more || i + 1 < n };
    } else {
      field += c;
    }
    i++;
  }
  if (field !== "" || row.length > 0) {
    pushField();
    rows.push(row);
  }
  return { rows, more };
}

async function fetchHead(url: string, cap: number, signal: AbortSignal): Promise<{ bytes: Uint8Array; truncated: boolean }> {
  // Keep the Owner token out of the fetched URL (server/proxy logs);
  // the <img>/<a> style ?token= is only needed where headers can't be set.
  const u = new URL(url, window.location.href);
  u.searchParams.delete("token");
  // No Range header: it disables the blob handler's remote read-through
  // for attachments whose bytes live on another peer. The stream is
  // cancelled once the cap is exceeded instead (overshoot ≤ one chunk).
  const res = await fetch(u.pathname + u.search, { signal, headers: authHeaders() });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  if (!res.body) {
    const all = new Uint8Array(await res.arrayBuffer());
    return { bytes: all.subarray(0, cap), truncated: all.length > cap };
  }
  const reader = res.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  let truncated = false;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    chunks.push(value);
    total += value.length;
    if (total > cap) {
      truncated = true;
      void reader.cancel().catch(() => {});
      break;
    }
  }
  const out = new Uint8Array(Math.min(total, cap));
  let off = 0;
  for (const c of chunks) {
    if (off >= out.length) break;
    const part = c.subarray(0, out.length - off);
    out.set(part, off);
    off += part.length;
  }
  return { bytes: out, truncated };
}

type LoadState =
  | { status: "loading" }
  | { status: "error"; message: string }
  | { status: "binary" }
  | { status: "ok"; text: string; truncated: boolean };

export function TextPreview({
  url,
  downloadUrl,
  name,
  mime,
  onNavigate,
}: {
  url: string;
  downloadUrl: string;
  name: string;
  mime: string;
  /** Prev/next between attachments; shown in the header so the
   * overlay's floating arrows don't cover the text. */
  onNavigate?: (dir: -1 | 1) => void;
}) {
  const t = useT();
  const format = textFormat(name, mime);
  const [state, setState] = useState<LoadState>({ status: "loading" });
  const [mode, setMode] = useState<"rendered" | "raw">("rendered");

  useEffect(() => {
    const ctl = new AbortController();
    setState({ status: "loading" });
    setMode("rendered");
    fetchHead(url, TEXT_PREVIEW_MAX_BYTES, ctl.signal)
      .then(({ bytes, truncated }) => {
        if (ctl.signal.aborted) return;
        const text = decodeTextBytes(bytes, truncated);
        if (text !== null && format === "markdown" && text.length > MARKDOWN_RENDER_MAX_CHARS) setMode("raw");
        setState(text === null ? { status: "binary" } : { status: "ok", text, truncated });
      })
      .catch((e: unknown) => {
        if (ctl.signal.aborted) return;
        setState({ status: "error", message: e instanceof Error ? e.message : String(e) });
      });
    return () => ctl.abort();
  }, [url, format]);

  const table = useMemo(() => {
    if (state.status !== "ok" || (format !== "csv" && format !== "tsv")) return null;
    return parseDelimited(state.text, format === "csv" ? "," : "\t", {
      maxRows: TABLE_MAX_ROWS,
      maxCols: TABLE_MAX_COLS,
      maxCells: TABLE_MAX_CELLS,
    });
  }, [state, format]);

  const canToggle = state.status === "ok" && format !== "plain";
  const rendered = canToggle && mode === "rendered";

  let body: React.ReactNode;
  if (state.status === "loading") {
    body = (
      <div className="flex h-full items-center justify-center py-16">
        <div className="h-5 w-5 animate-spin rounded-full border-2 border-ink-faint border-t-transparent" />
      </div>
    );
  } else if (state.status === "error" || state.status === "binary") {
    body = (
      <div className="px-4 py-16 text-center text-sm text-ink-dim">
        {state.status === "binary" ? t("textPreview.binary") : t("textPreview.loadFailed", { error: state.message })}
      </div>
    );
  } else if (rendered && format === "markdown") {
    body = (
      <div className="px-4 py-4 text-sm">
        <MarkdownRenderer content={state.text} blockRemoteImages />
      </div>
    );
  } else if (rendered && table) {
    const [head, ...rest] = table.rows;
    body = (
      <div className="p-2">
        <table className="border-collapse font-mono text-[12px] text-ink">
          {head && (
            <thead className="sticky top-0 bg-raised">
              <tr>
                {head.map((cell, i) => (
                  <th key={i} className="border border-hairline px-2 py-1 text-left font-semibold whitespace-pre-wrap">
                    {cell}
                  </th>
                ))}
              </tr>
            </thead>
          )}
          <tbody>
            {rest.map((r, ri) => (
              <tr key={ri} className="even:bg-surface/60">
                {r.map((cell, ci) => (
                  <td key={ci} className="border border-hairline px-2 py-1 align-top whitespace-pre-wrap">
                    {cell}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
        {table.more && (
          <div className="px-2 py-2 text-[11px] text-ink-faint">{t("textPreview.tableTruncated")}</div>
        )}
      </div>
    );
  } else {
    body = (
      <pre className="m-0 whitespace-pre-wrap break-words p-4 font-mono text-[12px] leading-relaxed text-ink">{state.text}</pre>
    );
  }

  const btn =
    "rounded-md border border-hairline bg-raised px-2 py-0.5 text-[11px] text-ink-dim transition-colors hover:bg-hover hover:text-ink";

  return (
    <div
      className="flex h-[80vh] w-[min(90vw,56rem)] flex-col overflow-hidden rounded-lg bg-app text-left shadow-2xl"
      data-testid="text-preview"
    >
      <div className="flex shrink-0 items-center gap-2 border-b border-hairline py-2 pl-3 pr-7">
        {onNavigate && (
          <button type="button" className={btn} onClick={() => onNavigate(-1)} aria-label={t("media.prevPreview")} title={t("media.prev")}>
            ‹
          </button>
        )}
        <span className="min-w-0 flex-1 truncate font-mono text-xs text-ink">{name}</span>
        {onNavigate && (
          <button type="button" className={btn} onClick={() => onNavigate(1)} aria-label={t("media.nextPreview")} title={t("media.next")}>
            ›
          </button>
        )}
        {canToggle && (
          <button type="button" className={btn} onClick={() => setMode(mode === "rendered" ? "raw" : "rendered")}>
            {mode === "rendered" ? t("textPreview.raw") : t("textPreview.rendered")}
          </button>
        )}
        <a href={downloadUrl} download={name} className={`${btn} no-underline`}>
          {t("media.download")}
        </a>
      </div>
      {state.status === "ok" && state.truncated && (
        <div className="shrink-0 border-b border-hairline bg-surface px-3 py-1 text-[11px] text-ink-faint">
          {t("textPreview.truncated", { size: formatSize(TEXT_PREVIEW_MAX_BYTES) })}
        </div>
      )}
      <div className="min-h-0 flex-1 overflow-auto">{body}</div>
    </div>
  );
}

import { useCallback, useEffect, useRef, useState } from "react";
import { useParams, useNavigate } from "react-router";
import { api, type SessionInfo, type GitStatus, type GitLogEntry } from "../lib/api";
import { errMsg, timeAgo } from "../lib/utils";
import { Input } from "./ui/Input";
import { Button } from "./ui/Button";

type Tab = "status" | "log" | "diff";

const DANGEROUS = ["push --force", "push -f", "reset --hard", "clean -f", "branch -D"];

// shell-like argument splitting that respects quotes
function parseArgs(input: string): string[] {
  const args: string[] = [];
  let current = "";
  let inQuote: '"' | "'" | null = null;
  for (const ch of input) {
    if (inQuote) {
      if (ch === inQuote) {
        inQuote = null;
      } else {
        current += ch;
      }
    } else if (ch === '"' || ch === "'") {
      inQuote = ch;
    } else if (ch === " " || ch === "\t") {
      if (current) {
        args.push(current);
        current = "";
      }
    } else {
      current += ch;
    }
  }
  if (current) args.push(current);
  return args;
}

interface GitPanelProps {
  embedded?: boolean;
  workDir?: string;
  peerId?: string;
}

export function GitPanel({ embedded, workDir: propWorkDir, peerId }: GitPanelProps = {}) {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const navigateRef = useRef(navigate);
  navigateRef.current = navigate;
  const [session, setSession] = useState<SessionInfo>();
  const [status, setStatus] = useState<GitStatus | null>(null);
  const [commits, setCommits] = useState<GitLogEntry[]>([]);
  const [hasMore, setHasMore] = useState(false);
  const [loadingMore, setLoadingMore] = useState(false);
  const [diff, setDiff] = useState<string | null>(null);
  const [diffLabel, setDiffLabel] = useState("");
  const [tab, setTab] = useState<Tab>("status");
  const [prevTab, setPrevTab] = useState<Tab>("status");
  const [cmdInput, setCmdInput] = useState("");
  const [cmdResult, setCmdResult] = useState<{ exitCode: number; stdout: string; stderr: string } | null>(null);
  const [cmdRunning, setCmdRunning] = useState(false);
  const [error, setError] = useState("");

  const effectiveWorkDir = embedded ? propWorkDir : session?.workDir;

  useEffect(() => {
    if (!embedded && id) {
      api.sessions.get(id, peerId).then(setSession).catch(() => navigateRef.current("/"));
    }
  }, [id, embedded, peerId]);

  const LOG_LIMIT = 10;
  const refreshIdRef = useRef(0);

  const refresh = useCallback(() => {
    if (!effectiveWorkDir) return;
    const rid = ++refreshIdRef.current;
    setError("");
    api.git.status(effectiveWorkDir, peerId).then(setStatus).catch((e) => setError(e.message));
    api.git.log(effectiveWorkDir, LOG_LIMIT, 0, peerId).then((r) => {
      if (rid !== refreshIdRef.current) return;
      setCommits(r.commits);
      setHasMore(r.hasMore);
    }).catch(() => {});
  }, [effectiveWorkDir, peerId]);

  useEffect(() => {
    if (effectiveWorkDir) refresh();
  }, [effectiveWorkDir, refresh]);

  const showDiff = async (ref?: string, label?: string) => {
    if (!effectiveWorkDir) return;
    try {
      const d = await api.git.diff(effectiveWorkDir, ref, peerId);
      setDiff(d);
      setDiffLabel(label ?? ref ?? "working tree");
      setPrevTab(tab);
      setTab("diff");
    } catch (e) {
      setError(errMsg(e));
    }
  };

  const runCmd = async () => {
    if (!effectiveWorkDir || !cmdInput.trim()) return;
    const args = parseArgs(cmdInput.trim());
    if (DANGEROUS.some((p) => cmdInput.includes(p))) {
      if (!window.confirm(`Destructive operation: git ${cmdInput}. Continue?`)) return;
    }
    setCmdRunning(true);
    setCmdResult(null);
    try {
      const result = await api.git.exec(effectiveWorkDir, args, peerId);
      setCmdResult(result);
      setCmdInput("");
      refresh();
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setCmdRunning(false);
    }
  };

  const loadMoreCommits = async () => {
    if (!effectiveWorkDir || loadingMore) return;
    const rid = refreshIdRef.current;
    setLoadingMore(true);
    try {
      const r = await api.git.log(effectiveWorkDir, LOG_LIMIT, commits.length, peerId);
      if (rid !== refreshIdRef.current) return;
      setCommits((prev) => {
        const seen = new Set(prev.map((c) => c.hash));
        const unique = r.commits.filter((c) => !seen.has(c.hash));
        return [...prev, ...unique];
      });
      setHasMore(r.hasMore);
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setLoadingMore(false);
    }
  };

  const quickCmd = (cmd: string) => setCmdInput(cmd);

  return (
    <div className="flex h-full flex-col bg-app text-ink">
      {/* Header (standalone mode only) */}
      {!embedded && (
        <header className="flex h-[52px] shrink-0 items-center gap-2 border-b border-hairline px-3">
          <button
            onClick={() => navigate(`/session/${id}`)}
            aria-label="Back"
            className="-ml-1 flex h-8 w-8 shrink-0 items-center justify-center rounded-[10px] text-ink-dim transition-colors hover:bg-hover hover:text-ink"
          >
            <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" className="h-5 w-5">
              <path d="M12.5 15l-5-5 5-5" />
            </svg>
          </button>
          <span className="font-mono text-[14px] font-semibold text-ink">git</span>
          {status && <span className="font-mono text-[12px] text-ink-dim">{status.branch}</span>}
          {status && (status.ahead > 0 || status.behind > 0) && (
            <span className="font-mono text-[11px] text-ink-faint">
              {status.ahead > 0 && `\u2191${status.ahead}`}
              {status.behind > 0 && `\u2193${status.behind}`}
            </span>
          )}
          <span className="flex-1" />
          <button
            onClick={refresh}
            className="rounded-[10px] border border-hairline bg-raised px-2.5 py-1.5 font-mono text-[11px] text-ink-dim transition-colors hover:bg-hover hover:text-ink"
          >
            Refresh
          </button>
        </header>
      )}

      {/* Embedded header: branch info + refresh */}
      {embedded && (
        <div className="flex shrink-0 items-center gap-2 border-b border-hairline px-3 py-1.5">
          {status && <span className="font-mono text-[12px] text-ink-dim">{status.branch}</span>}
          {status && (status.ahead > 0 || status.behind > 0) && (
            <span className="font-mono text-[11px] text-ink-faint">
              {status.ahead > 0 && `\u2191${status.ahead}`}
              {status.behind > 0 && `\u2193${status.behind}`}
            </span>
          )}
          <span className="flex-1" />
          <button
            onClick={refresh}
            className="rounded-[10px] border border-hairline bg-raised px-2 py-1 font-mono text-[11px] text-ink-dim transition-colors hover:bg-hover hover:text-ink"
          >
            Refresh
          </button>
        </div>
      )}

      {/* Error */}
      {error && (
        <div className="shrink-0 bg-lamp-err/10 px-3 py-2 font-mono text-[11px] text-lamp-err">{error}</div>
      )}

      {/* Tabs */}
      <div className="flex shrink-0 border-b border-hairline">
        {(["status", "log", "diff"] as Tab[]).map((t) => {
          const active = tab === t;
          return (
            <button
              key={t}
              onClick={() => setTab(t)}
              className={`relative flex h-11 flex-1 items-center justify-center font-mono text-[12px] transition-colors ${
                active ? "text-ink" : "text-ink-faint hover:text-ink-dim"
              }`}
            >
              {t.charAt(0).toUpperCase() + t.slice(1)}
              {active && <span className="absolute inset-x-3 bottom-0 h-0.5 rounded-full bg-copper" />}
            </button>
          );
        })}
      </div>

      {/* Tab content */}
      <div className="min-h-0 flex-1 overflow-y-auto">
        {tab === "status" && status && (
          <div className="space-y-4 p-3">
            <FileGroup label="Staged" badge="A" tint="run" files={status.staged} onTap={showDiff} />
            <FileGroup label="Modified" badge="M" tint="warn" files={status.modified} onTap={showDiff} />
            <FileGroup label="Untracked" badge="U" tint="off" files={status.untracked} />
            {status.staged.length === 0 && status.modified.length === 0 && status.untracked.length === 0 && (
              <p className="py-4 text-center text-sm text-ink-faint">Clean working tree</p>
            )}
          </div>
        )}

        {tab === "log" && (
          <div className="divide-y divide-hairline">
            {commits.map((c) => (
              <button
                key={c.hash}
                onClick={() => showDiff(c.hash, `${c.hash.slice(0, 7)} ${c.message}`)}
                className="w-full px-3 py-2.5 text-left transition-colors hover:bg-hover active:bg-hover"
              >
                <div className="flex items-center gap-2">
                  <span className="shrink-0 font-mono text-[11px] text-copper">{c.hash.slice(0, 7)}</span>
                  <span className="flex-1 truncate text-[14px] text-ink">{c.message}</span>
                </div>
                <div className="mt-0.5 font-mono text-[11px] text-ink-faint">
                  {c.author} &middot; {timeAgo(c.date)}
                </div>
              </button>
            ))}
            {commits.length === 0 && (
              <p className="py-4 text-center text-sm text-ink-faint">No commits</p>
            )}
            {hasMore && (
              <button
                onClick={loadMoreCommits}
                disabled={loadingMore}
                className="w-full py-3 font-mono text-[12px] text-ink-faint transition-colors hover:bg-hover hover:text-ink disabled:opacity-40"
              >
                {loadingMore ? "Loading\u2026" : "Load more"}
              </button>
            )}
          </div>
        )}

        {tab === "diff" && (
          <div className="p-3">
            <div className="mb-2 flex items-center gap-2">
              <button
                onClick={() => setTab(prevTab)}
                aria-label="Back"
                className="flex h-6 w-6 shrink-0 items-center justify-center rounded text-ink-dim transition-colors hover:text-ink"
              >
                <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" className="h-4 w-4">
                  <path d="M12.5 15l-5-5 5-5" />
                </svg>
              </button>
              {diffLabel && <span className="truncate font-mono text-[11px] text-ink-faint">{diffLabel}</span>}
            </div>
            {diff ? (
              <pre className="whitespace-pre-wrap break-all font-mono text-xs leading-relaxed">
                {diff.split("\n").map((line, i) => (
                  <span
                    key={i}
                    className={
                      line.startsWith("+") ? "text-lamp-run" :
                      line.startsWith("-") ? "text-lamp-err" :
                      line.startsWith("@@") ? "text-copper" :
                      "text-ink-dim"
                    }
                  >
                    {line}{"\n"}
                  </span>
                ))}
              </pre>
            ) : (
              <p className="py-4 text-center text-sm text-ink-faint">No diff</p>
            )}
          </div>
        )}
      </div>

      {/* Command result */}
      {cmdResult && (
        <div className="max-h-32 shrink-0 overflow-y-auto border-t border-hairline px-3 py-2">
          <pre className="whitespace-pre-wrap font-mono text-xs">
            <span className="text-ink-faint">$ git {cmdInput || "..."}</span>
            {"\n"}
            <span className={cmdResult.exitCode === 0 ? "text-ink-dim" : "text-lamp-err"}>
              {cmdResult.stdout || cmdResult.stderr || `(exit ${cmdResult.exitCode})`}
            </span>
          </pre>
        </div>
      )}

      {/* Quick commands */}
      <div className="flex shrink-0 gap-1.5 overflow-x-auto border-t border-hairline px-2 py-1.5">
        {["add .", "commit -m \"\"", "pull", "push", "stash"].map((cmd) => (
          <button
            key={cmd}
            onClick={() => quickCmd(cmd)}
            className="whitespace-nowrap rounded-[10px] border border-hairline bg-raised px-3 py-2 font-mono text-xs text-ink-dim transition-colors active:bg-hover"
          >
            {cmd}
          </button>
        ))}
      </div>

      {/* Command input */}
      <div className="flex shrink-0 items-center gap-2 border-t border-hairline px-2 py-2">
        <span className="font-mono text-[12px] text-ink-faint">git</span>
        <Input
          mono
          value={cmdInput}
          onChange={(e) => setCmdInput(e.target.value)}
          onKeyDown={(e) => { if (e.key === "Enter" && !e.nativeEvent.isComposing) runCmd(); }}
          placeholder="command\u2026"
          className="flex-1"
        />
        <Button
          variant="primary"
          onClick={runCmd}
          disabled={cmdRunning || !cmdInput.trim()}
        >
          {cmdRunning ? "\u2026" : "Run"}
        </Button>
      </div>
    </div>
  );
}

const BADGE_TINTS: Record<"run" | "warn" | "err" | "off", string> = {
  run: "border-lamp-run/40 bg-lamp-run/10 text-lamp-run",
  warn: "border-lamp-warn/40 bg-lamp-warn/10 text-lamp-warn",
  err: "border-lamp-err/40 bg-lamp-err/10 text-lamp-err",
  off: "border-hairline bg-raised text-ink-faint",
};

function FileGroup({
  label,
  files,
  badge,
  tint,
  onTap,
}: {
  label: string;
  files: string[];
  badge: string;
  tint: "run" | "warn" | "err" | "off";
  onTap?: (ref?: string, label?: string) => void;
}) {
  if (files.length === 0) return null;
  return (
    <div>
      <div className="mb-1 font-mono text-[11px] uppercase tracking-wide text-ink-faint">
        {label} ({files.length})
      </div>
      {files.map((f) => (
        <button
          key={f}
          onClick={() => onTap?.(f, `${label}: ${f}`)}
          className="flex w-full items-center gap-2 rounded-[10px] px-2 py-1.5 text-left transition-colors hover:bg-hover"
        >
          <span
            className={`inline-flex h-[15px] min-w-[15px] items-center justify-center rounded border px-1 font-mono ${BADGE_TINTS[tint]}`}
            style={{ fontSize: "10.5px", lineHeight: 1 }}
          >
            {badge}
          </span>
          <span className="truncate font-mono text-[13px] text-ink">{f}</span>
        </button>
      ))}
    </div>
  );
}


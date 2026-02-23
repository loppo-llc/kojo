import { useEffect, useState } from "react";
import { useParams, useNavigate } from "react-router";
import { api, type SessionInfo, type GitStatus, type GitLogEntry } from "../lib/api";

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

export function GitPanel() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [session, setSession] = useState<SessionInfo>();
  const [status, setStatus] = useState<GitStatus | null>(null);
  const [commits, setCommits] = useState<GitLogEntry[]>([]);
  const [diff, setDiff] = useState<string | null>(null);
  const [diffLabel, setDiffLabel] = useState("");
  const [tab, setTab] = useState<Tab>("status");
  const [cmdInput, setCmdInput] = useState("");
  const [cmdResult, setCmdResult] = useState<{ exitCode: number; stdout: string; stderr: string } | null>(null);
  const [cmdRunning, setCmdRunning] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    api.sessions.get(id!).then(setSession).catch(() => navigate("/"));
  }, [id, navigate]);

  useEffect(() => {
    if (session) refresh();
  }, [session]);

  const refresh = () => {
    if (!session) return;
    setError("");
    api.git.status(session.workDir).then(setStatus).catch((e) => setError(e.message));
    api.git.log(session.workDir, 10).then(setCommits).catch(() => {});
  };

  const showDiff = async (ref?: string, label?: string) => {
    if (!session) return;
    try {
      const d = await api.git.diff(session.workDir, ref);
      setDiff(d);
      setDiffLabel(label ?? ref ?? "working tree");
      setTab("diff");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };

  const runCmd = async () => {
    if (!session || !cmdInput.trim()) return;
    const args = parseArgs(cmdInput.trim());
    if (DANGEROUS.some((p) => cmdInput.includes(p))) {
      if (!window.confirm(`Destructive operation: git ${cmdInput}. Continue?`)) return;
    }
    setCmdRunning(true);
    setCmdResult(null);
    try {
      const result = await api.git.exec(session.workDir, args);
      setCmdResult(result);
      setCmdInput("");
      refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setCmdRunning(false);
    }
  };

  const quickCmd = (cmd: string) => setCmdInput(cmd);

  return (
    <div className="h-full flex flex-col bg-neutral-950 text-neutral-200">
      {/* Header */}
      <header className="flex items-center gap-2 px-3 py-2 border-b border-neutral-800 shrink-0">
        <button onClick={() => navigate(`/session/${id}`)} className="text-neutral-400 hover:text-neutral-200">
          &larr;
        </button>
        <span className="font-mono font-bold">git</span>
        {status && <span className="text-xs text-neutral-400 font-mono">{status.branch}</span>}
        {status && (status.ahead > 0 || status.behind > 0) && (
          <span className="text-xs text-neutral-500">
            {status.ahead > 0 && `\u2191${status.ahead}`}
            {status.behind > 0 && `\u2193${status.behind}`}
          </span>
        )}
        <span className="flex-1" />
        <button
          onClick={refresh}
          className="px-2.5 py-1.5 text-xs bg-neutral-800 hover:bg-neutral-700 text-neutral-400 rounded min-h-[44px] min-w-[44px] flex items-center justify-center"
        >
          Refresh
        </button>
      </header>

      {/* Error */}
      {error && (
        <div className="px-3 py-2 bg-red-950 text-red-400 text-xs">{error}</div>
      )}

      {/* Tabs */}
      <div className="flex border-b border-neutral-800 shrink-0">
        {(["status", "log", "diff"] as Tab[]).map((t) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            className={`flex-1 py-2 text-sm text-center ${
              tab === t ? "text-neutral-200 border-b-2 border-neutral-400" : "text-neutral-500"
            }`}
          >
            {t.charAt(0).toUpperCase() + t.slice(1)}
          </button>
        ))}
      </div>

      {/* Tab content */}
      <div className="flex-1 min-h-0 overflow-y-auto">
        {tab === "status" && status && (
          <div className="p-3 space-y-3">
            <FileGroup label="Staged" files={status.staged} color="text-green-400" prefix="+" onTap={showDiff} />
            <FileGroup label="Modified" files={status.modified} color="text-yellow-400" prefix="~" onTap={showDiff} />
            <FileGroup label="Untracked" files={status.untracked} color="text-neutral-500" prefix="?" />
            {status.staged.length === 0 && status.modified.length === 0 && status.untracked.length === 0 && (
              <p className="text-neutral-500 text-sm text-center py-4">Clean working tree</p>
            )}
          </div>
        )}

        {tab === "log" && (
          <div className="divide-y divide-neutral-800">
            {commits.map((c) => (
              <button
                key={c.hash}
                onClick={() => showDiff(c.hash, `${c.hash.slice(0, 7)} ${c.message}`)}
                className="w-full text-left px-3 py-2 hover:bg-neutral-900 active:bg-neutral-800"
              >
                <div className="flex items-center gap-2">
                  <span className="font-mono text-xs text-neutral-400">{c.hash.slice(0, 7)}</span>
                  <span className="text-sm truncate flex-1">{c.message}</span>
                </div>
                <div className="text-xs text-neutral-500 mt-0.5">
                  {c.author} &middot; {timeAgo(c.date)}
                </div>
              </button>
            ))}
            {commits.length === 0 && (
              <p className="text-neutral-500 text-sm text-center py-4">No commits</p>
            )}
          </div>
        )}

        {tab === "diff" && (
          <div className="p-3">
            {diffLabel && <div className="text-xs text-neutral-500 mb-2 font-mono">{diffLabel}</div>}
            {diff ? (
              <pre className="text-xs font-mono whitespace-pre-wrap break-all leading-relaxed">
                {diff.split("\n").map((line, i) => (
                  <span
                    key={i}
                    className={
                      line.startsWith("+") ? "text-green-400" :
                      line.startsWith("-") ? "text-red-400" :
                      line.startsWith("@@") ? "text-cyan-400" :
                      "text-neutral-400"
                    }
                  >
                    {line}{"\n"}
                  </span>
                ))}
              </pre>
            ) : (
              <p className="text-neutral-500 text-sm text-center py-4">No diff</p>
            )}
          </div>
        )}
      </div>

      {/* Command result */}
      {cmdResult && (
        <div className="px-3 py-2 border-t border-neutral-800 shrink-0 max-h-32 overflow-y-auto">
          <pre className="text-xs font-mono whitespace-pre-wrap">
            <span className="text-neutral-500">$ git {cmdInput || "..."}</span>
            {"\n"}
            <span className={cmdResult.exitCode === 0 ? "text-neutral-400" : "text-red-400"}>
              {cmdResult.stdout || cmdResult.stderr || `(exit ${cmdResult.exitCode})`}
            </span>
          </pre>
        </div>
      )}

      {/* Quick commands */}
      <div className="flex gap-1.5 px-2 py-1.5 border-t border-neutral-800 overflow-x-auto shrink-0">
        {["add .", "commit -m \"\"", "pull", "push", "stash"].map((cmd) => (
          <button
            key={cmd}
            onClick={() => quickCmd(cmd)}
            className="px-3 py-2 text-xs bg-neutral-800 text-neutral-400 rounded whitespace-nowrap active:bg-neutral-600"
          >
            {cmd}
          </button>
        ))}
      </div>

      {/* Command input */}
      <div className="flex items-center gap-2 px-2 py-2 border-t border-neutral-800 shrink-0">
        <span className="text-xs text-neutral-500 font-mono">git</span>
        <input
          value={cmdInput}
          onChange={(e) => setCmdInput(e.target.value)}
          onKeyDown={(e) => { if (e.key === "Enter") runCmd(); }}
          placeholder="command..."
          className="flex-1 px-3 py-1.5 bg-neutral-900 border border-neutral-700 rounded text-sm font-mono focus:outline-none focus:border-neutral-500"
        />
        <button
          onClick={runCmd}
          disabled={cmdRunning || !cmdInput.trim()}
          className="px-3 py-1.5 bg-neutral-700 hover:bg-neutral-600 rounded text-sm disabled:opacity-30"
        >
          {cmdRunning ? "..." : "Run"}
        </button>
      </div>
    </div>
  );
}

function FileGroup({
  label,
  files,
  color,
  prefix,
  onTap,
}: {
  label: string;
  files: string[];
  color: string;
  prefix: string;
  onTap?: (ref?: string, label?: string) => void;
}) {
  if (files.length === 0) return null;
  return (
    <div>
      <div className="text-xs text-neutral-500 mb-1">{label} ({files.length})</div>
      {files.map((f) => (
        <button
          key={f}
          onClick={() => onTap?.(f, `${label}: ${f}`)}
          className="w-full text-left flex items-center gap-2 px-2 py-1.5 hover:bg-neutral-900 rounded text-sm"
        >
          <span className={`font-mono text-xs ${color}`}>{prefix}</span>
          <span className="font-mono truncate">{f}</span>
        </button>
      ))}
    </div>
  );
}

function timeAgo(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime();
  const mins = Math.floor(diff / 60000);
  if (mins < 1) return "just now";
  if (mins < 60) return `${mins}m ago`;
  const hrs = Math.floor(mins / 60);
  if (hrs < 24) return `${hrs}h ago`;
  return `${Math.floor(hrs / 24)}d ago`;
}

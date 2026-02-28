import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router";
import { api, type SessionInfo } from "../lib/api";
import { usePushNotifications } from "../hooks/usePushNotifications";
import { timeAgo } from "../lib/utils";

interface SessionGroup {
  key: string;
  tool: string;
  workDir: string;
  primary: SessionInfo;
  others: SessionInfo[];
}

function groupSessions(sessions: SessionInfo[]): SessionGroup[] {
  const sorted = [...sessions]
    .filter((s) => !s.internal)
    .sort((a, b) => new Date(b.createdAt).getTime() - new Date(a.createdAt).getTime());

  const map = new Map<string, SessionInfo[]>();
  for (const s of sorted) {
    const key = `${s.tool}:${s.workDir}`;
    const list = map.get(key);
    if (list) list.push(s);
    else map.set(key, [s]);
  }

  const groups: SessionGroup[] = [];
  for (const [key, list] of map) {
    // running sessions first, then by createdAt (already sorted)
    const running = list.filter((s) => s.status === "running");
    const rest = list.filter((s) => s.status !== "running");
    const ordered = [...running, ...rest];
    groups.push({
      key,
      tool: ordered[0].tool,
      workDir: ordered[0].workDir,
      primary: ordered[0],
      others: ordered.slice(1),
    });
  }

  // groups with running sessions first, then by most recent
  groups.sort((a, b) => {
    const aRunning = a.primary.status === "running" ? 1 : 0;
    const bRunning = b.primary.status === "running" ? 1 : 0;
    if (aRunning !== bRunning) return bRunning - aRunning;
    return new Date(b.primary.createdAt).getTime() - new Date(a.primary.createdAt).getTime();
  });

  return groups;
}

export function Dashboard() {
  const [sessions, setSessions] = useState<SessionInfo[]>([]);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const navigate = useNavigate();
  const { state: pushState, loading: pushLoading, subscribe: pushSubscribe } = usePushNotifications();

  useEffect(() => {
    const load = () => api.sessions.list().then(setSessions).catch(console.error);
    load();
    const interval = setInterval(load, 3000);
    return () => clearInterval(interval);
  }, []);

  const groups = groupSessions(sessions);
  const hasAny = sessions.some((s) => !s.internal);

  const toggleExpand = (key: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  const deleteGroup = async (g: SessionGroup, e: React.MouseEvent) => {
    e.stopPropagation();
    const all = [g.primary, ...g.others];
    const results = await Promise.allSettled(all.map((s) => api.sessions.delete(s.id)));
    const deletedIds = new Set<string>();
    results.forEach((r, i) => { if (r.status === "fulfilled") deletedIds.add(all[i].id); });
    if (deletedIds.size > 0) {
      setSessions((prev) => prev.filter((s) => !deletedIds.has(s.id)));
    }
  };

  return (
    <div className="min-h-full bg-neutral-950 text-neutral-200">
      <header className="flex items-center justify-between px-4 py-3 border-b border-neutral-800">
        <h1 className="text-lg font-bold">kojo</h1>
        <Link
          to="/new"
          className="px-3 py-1.5 bg-neutral-800 hover:bg-neutral-700 rounded text-sm"
        >
          + New
        </Link>
      </header>
      {pushState === "default" && (
        <div className="mx-4 mt-3 p-3 bg-neutral-900 border border-neutral-800 rounded-lg flex items-center gap-3">
          <span className="text-sm flex-1">Enable notifications when sessions finish?</span>
          <button
            onClick={pushSubscribe}
            disabled={pushLoading}
            className="px-3 py-1.5 bg-neutral-700 hover:bg-neutral-600 rounded text-sm whitespace-nowrap disabled:opacity-40"
          >
            {pushLoading ? "..." : "Enable"}
          </button>
        </div>
      )}
      <main className="p-4 space-y-3">
        {!hasAny && (
          <p className="text-neutral-500 text-center py-12">No sessions</p>
        )}
        {groups.map((g) => {
          const runningOthers = g.others.filter((s) => s.status === "running");
          const stoppedOthers = g.others.filter((s) => s.status !== "running");
          const allExited = g.primary.status !== "running" && runningOthers.length === 0;
          return (
          <div key={g.key} className="bg-neutral-900 rounded-lg border border-neutral-800 relative">
            <button
              onClick={() => navigate(`/session/${g.primary.id}`)}
              className="w-full text-left p-4 hover:bg-neutral-800 rounded-lg"
            >
              <div className="flex items-center gap-2 mb-1">
                <span className="font-mono font-bold">{g.tool}</span>
                {g.primary.toolSessionId && (
                  <span className="text-[10px] text-neutral-600 font-mono truncate">{g.primary.toolSessionId.slice(0, 8)}</span>
                )}
              </div>
              <div className="text-sm text-neutral-400 truncate">{g.workDir}</div>
              <div className="flex items-center gap-2 mt-2 text-xs text-neutral-500">
                <span
                  className={`inline-block w-2 h-2 rounded-full ${
                    g.primary.status === "running" ? "bg-green-500" : "bg-neutral-600"
                  }`}
                />
                <span>{g.primary.status}</span>
                {g.primary.exitCode !== undefined && <span>(exit {g.primary.exitCode})</span>}
                <span className="ml-auto">{timeAgo(g.primary.createdAt)}</span>
              </div>
            </button>
            <div className="absolute top-3 right-3 flex items-center gap-1">
              <button
                onClick={() => navigate(`/new?tool=${encodeURIComponent(g.tool)}&workDir=${encodeURIComponent(g.workDir)}`)}
                className="p-2 text-neutral-600 hover:text-neutral-300 rounded"
                title="New session"
                aria-label="New session"
              >
                <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-5 h-5">
                  <path d="M10.75 4.75a.75.75 0 00-1.5 0v4.5h-4.5a.75.75 0 000 1.5h4.5v4.5a.75.75 0 001.5 0v-4.5h4.5a.75.75 0 000-1.5h-4.5v-4.5z" />
                </svg>
              </button>
              {allExited && (
                <button
                  onClick={(e) => deleteGroup(g, e)}
                  className="p-2 text-neutral-600 hover:text-red-400 rounded"
                  title="Remove"
                  aria-label="Remove"
                >
                  <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-5 h-5">
                    <path fillRule="evenodd" d="M8.75 1A2.75 2.75 0 006 3.75v.443c-.795.077-1.584.176-2.365.298a.75.75 0 10.23 1.482l.149-.022.841 10.518A2.75 2.75 0 007.596 19h4.807a2.75 2.75 0 002.742-2.53l.841-10.519.149.023a.75.75 0 00.23-1.482A41.03 41.03 0 0014 4.193V3.75A2.75 2.75 0 0011.25 1h-2.5zM10 4c.84 0 1.673.025 2.5.075V3.75c0-.69-.56-1.25-1.25-1.25h-2.5c-.69 0-1.25.56-1.25 1.25v.325C8.327 4.025 9.16 4 10 4zM8.58 7.72a.75.75 0 00-1.5.06l.3 7.5a.75.75 0 101.5-.06l-.3-7.5zm4.34.06a.75.75 0 10-1.5-.06l-.3 7.5a.75.75 0 101.5.06l.3-7.5z" clipRule="evenodd" />
                  </svg>
                </button>
              )}
            </div>
            {runningOthers.map((s) => (
              <button
                key={s.id}
                onClick={() => navigate(`/session/${s.id}`)}
                className="w-full text-left px-4 py-2.5 hover:bg-neutral-800 border-t border-neutral-800 flex items-center gap-2 text-xs text-neutral-500"
              >
                <span className="inline-block w-1.5 h-1.5 rounded-full shrink-0 bg-green-500" />
                <span>{s.status}</span>
                {s.toolSessionId && <span className="font-mono text-neutral-600">{s.toolSessionId.slice(0, 8)}</span>}
                <span className="ml-auto">{timeAgo(s.createdAt)}</span>
              </button>
            ))}
            {stoppedOthers.length > 0 && (
              <>
                <button
                  onClick={() => toggleExpand(g.key)}
                  className="w-full px-4 py-2 text-xs text-neutral-500 hover:text-neutral-400 border-t border-neutral-800 text-left"
                >
                  {expanded.has(g.key) ? "Hide" : `+${stoppedOthers.length} more`}
                </button>
                {expanded.has(g.key) && stoppedOthers.map((s) => (
                  <button
                    key={s.id}
                    onClick={() => navigate(`/session/${s.id}`)}
                    className="w-full text-left px-4 py-2.5 hover:bg-neutral-800 border-t border-neutral-800 flex items-center gap-2 text-xs text-neutral-500"
                  >
                    <span className="inline-block w-1.5 h-1.5 rounded-full shrink-0 bg-neutral-600" />
                    <span>{s.status}</span>
                    {s.toolSessionId && <span className="font-mono text-neutral-600">{s.toolSessionId.slice(0, 8)}</span>}
                    {s.exitCode !== undefined && <span>(exit {s.exitCode})</span>}
                    <span className="ml-auto">{timeAgo(s.createdAt)}</span>
                  </button>
                ))}
              </>
            )}
          </div>
          );
        }
        )}
      </main>
    </div>
  );
}


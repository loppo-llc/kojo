import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router";
import { api, type SessionInfo } from "../lib/api";

export function Dashboard() {
  const [sessions, setSessions] = useState<SessionInfo[]>([]);
  const navigate = useNavigate();

  useEffect(() => {
    const load = () => api.sessions.list().then(setSessions).catch(console.error);
    load();
    const interval = setInterval(load, 3000);
    return () => clearInterval(interval);
  }, []);

  const sorted = [...sessions].sort(
    (a, b) => new Date(b.createdAt).getTime() - new Date(a.createdAt).getTime(),
  );
  const running = sorted.filter((s) => s.status === "running");
  const exited = sorted.filter((s) => s.status === "exited");

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
      <main className="p-4 space-y-3">
        {sessions.length === 0 && (
          <p className="text-neutral-500 text-center py-12">No sessions</p>
        )}
        {[...running, ...exited].map((s) => (
          <button
            key={s.id}
            onClick={() => navigate(`/session/${s.id}`)}
            className="w-full text-left p-4 bg-neutral-900 hover:bg-neutral-800 rounded-lg border border-neutral-800"
          >
            <div className="flex items-center gap-2 mb-1">
              <span className="font-mono font-bold">{s.tool}</span>
            </div>
            <div className="text-sm text-neutral-400 truncate">{s.workDir}</div>
            <div className="flex items-center gap-2 mt-2 text-xs text-neutral-500">
              <span
                className={`inline-block w-2 h-2 rounded-full ${
                  s.status === "running" ? "bg-green-500" : "bg-neutral-600"
                }`}
              />
              <span>{s.status}</span>
              {s.exitCode !== undefined && <span>(exit {s.exitCode})</span>}
              <span className="ml-auto">{timeAgo(s.createdAt)}</span>
            </div>
          </button>
        ))}
      </main>
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

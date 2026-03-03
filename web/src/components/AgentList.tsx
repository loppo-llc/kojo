import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router";
import { api, type AgentInfo } from "../lib/api";
import { timeAgo } from "../lib/utils";

export function AgentList() {
  const [agents, setAgents] = useState<AgentInfo[]>([]);
  const navigate = useNavigate();

  useEffect(() => {
    api.agents.list().then(setAgents).catch(console.error);
  }, []);

  return (
    <div className="min-h-full bg-neutral-950 text-neutral-200">
      <header className="flex items-center justify-between px-4 py-3 border-b border-neutral-800">
        <div className="flex items-center gap-3">
          <Link to="/" className="text-neutral-500 hover:text-neutral-300 text-sm">&larr;</Link>
          <h1 className="text-lg font-bold">Agents</h1>
        </div>
        <Link
          to="/agents/new"
          className="px-3 py-1.5 bg-neutral-800 hover:bg-neutral-700 rounded text-sm"
        >
          + New Agent
        </Link>
      </header>
      <main className="p-4 space-y-3">
        {agents.length === 0 && (
          <p className="text-neutral-500 text-center py-12">No agents</p>
        )}
        {agents.map((a) => (
          <button
            key={a.id}
            onClick={() => navigate(`/agents/${a.id}`)}
            className="w-full text-left bg-neutral-900 rounded-lg border border-neutral-800 p-4 hover:bg-neutral-800"
          >
            <div className="flex items-center gap-2 mb-1">
              <span className="font-mono font-bold">{a.name}</span>
              <span className="text-xs text-neutral-500">{a.tool}</span>
              {!a.enabled && (
                <span className="text-xs text-neutral-600 bg-neutral-800 px-1.5 py-0.5 rounded">disabled</span>
              )}
            </div>
            <div className="text-sm text-neutral-400 truncate">{a.workDir}</div>
            <div className="flex items-center gap-2 mt-2 text-xs text-neutral-500">
              {a.schedule && <span className="bg-neutral-800 px-1.5 py-0.5 rounded">{a.schedule}</span>}
              {a.lastRunAt && <span>last run {timeAgo(a.lastRunAt)}</span>}
              <span className="ml-auto">{timeAgo(a.createdAt)}</span>
            </div>
          </button>
        ))}
      </main>
    </div>
  );
}

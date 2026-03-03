import { useEffect, useState } from "react";
import { useNavigate } from "react-router";
import { api, type ServerInfo } from "../lib/api";

export function AgentCreate() {
  const navigate = useNavigate();
  const [info, setInfo] = useState<ServerInfo>();
  const [name, setName] = useState("");
  const [tool, setTool] = useState("claude");
  const [workDir, setWorkDir] = useState("");
  const [schedule, setSchedule] = useState("");
  const [yoloMode, setYoloMode] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    api.info().then((i) => {
      setInfo(i);
      setWorkDir(i.homeDir || "");
      const available = Object.entries(i.tools).find(([, t]) => t.available);
      if (available) setTool(available[0]);
    }).catch(console.error);
  }, []);

  const submit = async () => {
    setError("");
    setLoading(true);
    try {
      const agent = await api.agents.create({ name, tool, workDir, yoloMode, schedule });
      navigate(`/agents/${agent.id}`);
    } catch (e: any) {
      setError(e.message);
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="min-h-full bg-neutral-950 text-neutral-200">
      <header className="flex items-center gap-3 px-4 py-3 border-b border-neutral-800">
        <button onClick={() => navigate("/agents")} className="text-neutral-500 hover:text-neutral-300 text-sm">&larr;</button>
        <h1 className="text-lg font-bold">New Agent</h1>
      </header>
      <main className="p-4 space-y-4 max-w-lg">
        <div>
          <label className="block text-sm text-neutral-400 mb-1">Name</label>
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            className="w-full bg-neutral-900 border border-neutral-700 rounded px-3 py-2 text-sm"
            placeholder="my-agent"
          />
        </div>
        <div>
          <label className="block text-sm text-neutral-400 mb-1">Tool</label>
          <select
            value={tool}
            onChange={(e) => setTool(e.target.value)}
            className="w-full bg-neutral-900 border border-neutral-700 rounded px-3 py-2 text-sm"
          >
            {info && Object.entries(info.tools)
              .filter(([, t]) => t.available)
              .map(([name]) => (
                <option key={name} value={name}>{name}</option>
              ))}
          </select>
        </div>
        <div>
          <label className="block text-sm text-neutral-400 mb-1">Working Directory</label>
          <input
            value={workDir}
            onChange={(e) => setWorkDir(e.target.value)}
            className="w-full bg-neutral-900 border border-neutral-700 rounded px-3 py-2 text-sm font-mono"
          />
        </div>
        <div>
          <label className="block text-sm text-neutral-400 mb-1">Schedule</label>
          <input
            value={schedule}
            onChange={(e) => setSchedule(e.target.value)}
            className="w-full bg-neutral-900 border border-neutral-700 rounded px-3 py-2 text-sm"
            placeholder="hourly / daily 09:00 / every 6h"
          />
        </div>
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={yoloMode}
            onChange={(e) => setYoloMode(e.target.checked)}
            className="rounded"
          />
          YOLO Mode
        </label>
        {error && <p className="text-red-400 text-sm">{error}</p>}
        <button
          onClick={submit}
          disabled={loading || !name || !workDir}
          className="w-full py-2 bg-neutral-800 hover:bg-neutral-700 rounded text-sm disabled:opacity-40"
        >
          {loading ? "Creating..." : "Create Agent"}
        </button>
      </main>
    </div>
  );
}

import { useEffect, useState } from "react";
import { useParams, useNavigate, Link } from "react-router";
import { api, type AgentInfo, type SessionInfo } from "../lib/api";
import { timeAgo } from "../lib/utils";

type Tab = "overview" | "soul" | "memory" | "goals" | "logs" | "sessions";

export function AgentPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [agent, setAgent] = useState<AgentInfo | null>(null);
  const [tab, setTab] = useState<Tab>("overview");
  const [running, setRunning] = useState(false);

  useEffect(() => {
    if (id) api.agents.get(id).then(setAgent).catch(() => navigate("/agents"));
  }, [id, navigate]);

  if (!agent) return null;

  const handleRun = async () => {
    setRunning(true);
    try {
      const sess = await api.agents.run(agent.id);
      navigate(`/session/${sess.id}`);
    } catch (e: any) {
      alert(e.message);
    } finally {
      setRunning(false);
    }
  };

  const handleDelete = async () => {
    if (!confirm("Delete this agent?")) return;
    await api.agents.delete(agent.id);
    navigate("/agents");
  };

  const tabs: { key: Tab; label: string }[] = [
    { key: "overview", label: "Overview" },
    { key: "soul", label: "Soul" },
    { key: "memory", label: "Memory" },
    { key: "goals", label: "Goals" },
    { key: "logs", label: "Logs" },
    { key: "sessions", label: "Sessions" },
  ];

  return (
    <div className="min-h-full bg-neutral-950 text-neutral-200">
      <header className="px-4 py-3 border-b border-neutral-800">
        <div className="flex items-center justify-between mb-2">
          <div className="flex items-center gap-3">
            <Link to="/agents" className="text-neutral-500 hover:text-neutral-300 text-sm">&larr;</Link>
            <h1 className="text-lg font-bold">{agent.name}</h1>
            <span className="text-xs text-neutral-500">{agent.tool}</span>
          </div>
          <div className="flex items-center gap-2">
            <button
              onClick={handleRun}
              disabled={running}
              className="px-3 py-1.5 bg-green-800 hover:bg-green-700 rounded text-sm disabled:opacity-40"
            >
              {running ? "Starting..." : "Run"}
            </button>
            <button
              onClick={handleDelete}
              className="px-3 py-1.5 text-neutral-500 hover:text-red-400 text-sm"
            >
              Delete
            </button>
          </div>
        </div>
        <div className="flex gap-1 overflow-x-auto">
          {tabs.map((t) => (
            <button
              key={t.key}
              onClick={() => setTab(t.key)}
              className={`px-3 py-1.5 text-sm rounded-t ${
                tab === t.key ? "bg-neutral-800 text-neutral-200" : "text-neutral-500 hover:text-neutral-300"
              }`}
            >
              {t.label}
            </button>
          ))}
        </div>
      </header>
      <main className="p-4">
        {tab === "overview" && <OverviewTab agent={agent} />}
        {tab === "soul" && <FileTab agentId={agent.id} file="soul" />}
        {tab === "memory" && <FileTab agentId={agent.id} file="memory" />}
        {tab === "goals" && <FileTab agentId={agent.id} file="goals" />}
        {tab === "logs" && <LogsTab agentId={agent.id} />}
        {tab === "sessions" && <SessionsTab agentId={agent.id} />}
      </main>
    </div>
  );
}

function OverviewTab({ agent }: { agent: AgentInfo }) {
  return (
    <div className="space-y-3 text-sm">
      <div className="grid grid-cols-2 gap-2 text-neutral-400">
        <div>Tool</div><div className="font-mono">{agent.tool}</div>
        <div>WorkDir</div><div className="font-mono truncate">{agent.workDir}</div>
        <div>Schedule</div><div>{agent.schedule || "(manual)"}</div>
        <div>Enabled</div><div>{agent.enabled ? "Yes" : "No"}</div>
        <div>YOLO</div><div>{agent.yoloMode ? "Yes" : "No"}</div>
        <div>Created</div><div>{timeAgo(agent.createdAt)}</div>
        {agent.lastRunAt && <><div>Last Run</div><div>{timeAgo(agent.lastRunAt)}</div></>}
      </div>
    </div>
  );
}

function FileTab({ agentId, file }: { agentId: string; file: "soul" | "memory" | "goals" }) {
  const [content, setContent] = useState("");
  const [saving, setSaving] = useState(false);
  const [loaded, setLoaded] = useState(false);

  useEffect(() => {
    const load = file === "soul" ? api.agents.soul : file === "memory" ? api.agents.memory : api.agents.goals;
    load(agentId).then((c) => { setContent(c); setLoaded(true); }).catch(console.error);
  }, [agentId, file]);

  const save = async () => {
    setSaving(true);
    try {
      const setter = file === "soul" ? api.agents.setSoul : file === "memory" ? api.agents.setMemory : api.agents.setGoals;
      await setter(agentId, content);
    } finally {
      setSaving(false);
    }
  };

  if (!loaded) return null;

  return (
    <div className="space-y-3">
      <textarea
        value={content}
        onChange={(e) => setContent(e.target.value)}
        className="w-full h-80 bg-neutral-900 border border-neutral-700 rounded p-3 text-sm font-mono resize-y"
      />
      <button
        onClick={save}
        disabled={saving}
        className="px-4 py-2 bg-neutral-800 hover:bg-neutral-700 rounded text-sm disabled:opacity-40"
      >
        {saving ? "Saving..." : "Save"}
      </button>
    </div>
  );
}

function LogsTab({ agentId }: { agentId: string }) {
  const [logs, setLogs] = useState<string[]>([]);
  const [selected, setSelected] = useState("");
  const [content, setContent] = useState("");

  useEffect(() => {
    api.agents.logs(agentId).then(setLogs).catch(console.error);
  }, [agentId]);

  const loadLog = async (name: string) => {
    setSelected(name);
    const c = await api.agents.log(agentId, name);
    setContent(c);
  };

  return (
    <div className="space-y-3">
      {logs.length === 0 && <p className="text-neutral-500 text-sm">No logs yet</p>}
      <div className="space-y-1">
        {logs.map((name) => (
          <button
            key={name}
            onClick={() => loadLog(name)}
            className={`block w-full text-left px-3 py-2 rounded text-sm font-mono ${
              selected === name ? "bg-neutral-800" : "hover:bg-neutral-900"
            }`}
          >
            {name}
          </button>
        ))}
      </div>
      {selected && (
        <pre className="bg-neutral-900 border border-neutral-700 rounded p-3 text-sm whitespace-pre-wrap overflow-auto max-h-96">
          {content}
        </pre>
      )}
    </div>
  );
}

function SessionsTab({ agentId }: { agentId: string }) {
  const [sessions, setSessions] = useState<SessionInfo[]>([]);
  const navigate = useNavigate();

  useEffect(() => {
    api.agents.sessions(agentId).then(setSessions).catch(console.error);
  }, [agentId]);

  return (
    <div className="space-y-2">
      {sessions.length === 0 && <p className="text-neutral-500 text-sm">No sessions yet</p>}
      {sessions.map((s) => (
        <button
          key={s.id}
          onClick={() => navigate(`/session/${s.id}`)}
          className="w-full text-left px-3 py-2 bg-neutral-900 rounded hover:bg-neutral-800 flex items-center gap-2 text-sm"
        >
          <span
            className={`inline-block w-2 h-2 rounded-full ${
              s.status === "running" ? "bg-green-500" : "bg-neutral-600"
            }`}
          />
          <span>{s.status}</span>
          <span className="ml-auto text-neutral-500 text-xs">{timeAgo(s.createdAt)}</span>
        </button>
      ))}
    </div>
  );
}

import { useEffect, useState } from "react";
import { agentApi, type NotifySourceConfig, type NotifySourceType } from "../../lib/agentApi";

interface Props {
  agentId: string;
}

export function NotifySourcesEditor({ agentId }: Props) {
  const [sources, setSources] = useState<NotifySourceConfig[]>([]);
  const [sourceTypes, setSourceTypes] = useState<NotifySourceType[]>([]);
  const [adding, setAdding] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    agentApi.notifySources.list(agentId).then(setSources).catch(() => {});
    agentApi.notifySourceTypes().then(setSourceTypes).catch(() => {});
  }, [agentId]);

  const handleAdd = async (type: string) => {
    setAdding(true);
    setError("");
    try {
      const src = await agentApi.notifySources.create(agentId, {
        type,
        intervalMinutes: 10,
      });
      setSources((prev) => [...prev, src]);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setAdding(false);
    }
  };

  const handleToggle = async (source: NotifySourceConfig) => {
    try {
      const updated = await agentApi.notifySources.update(agentId, source.id, {
        enabled: !source.enabled,
      });
      setSources((prev) => prev.map((s) => (s.id === source.id ? updated : s)));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const handleAuth = async (source: NotifySourceConfig) => {
    try {
      const authUrl = await agentApi.notifySources.startAuth(agentId, source.id);
      window.open(authUrl, "_blank", "width=600,height=700");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  // Listen for OAuth callback completion via postMessage
  useEffect(() => {
    const handler = (e: MessageEvent) => {
      if (e.data?.type === "oauth_complete") {
        agentApi.notifySources.list(agentId).then(setSources).catch(() => {});
      }
    };
    window.addEventListener("message", handler);
    return () => window.removeEventListener("message", handler);
  }, [agentId]);

  const handleDelete = async (source: NotifySourceConfig) => {
    if (!confirm(`Remove ${source.type} notification source?`)) return;
    try {
      await agentApi.notifySources.delete(agentId, source.id);
      setSources((prev) => prev.filter((s) => s.id !== source.id));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const handleIntervalChange = async (source: NotifySourceConfig, minutes: number) => {
    try {
      const updated = await agentApi.notifySources.update(agentId, source.id, {
        intervalMinutes: minutes,
      });
      setSources((prev) => prev.map((s) => (s.id === source.id ? updated : s)));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const usedTypes = new Set(sources.map((s) => s.type));
  const availableTypes = sourceTypes.filter((t) => !usedTypes.has(t.type));

  return (
    <div>
      <div className="flex items-center justify-between mb-4">
        <h2 className="text-sm font-semibold text-neutral-300">Notifications</h2>
        <span className="text-xs text-neutral-600">Auto-saved</span>
      </div>

      {sources.length === 0 && availableTypes.length === 0 && (
        <p className="text-xs text-neutral-600">No notification sources available</p>
      )}

      {sources.map((source) => (
        <div
          key={source.id}
          className="flex items-center gap-3 p-3 bg-neutral-900 border border-neutral-800 rounded-lg mb-2"
        >
          {/* Toggle */}
          <button onClick={() => handleToggle(source)} className="shrink-0">
            <span
              className={`relative inline-flex h-3 w-6 items-center rounded-full transition-colors duration-200 ${
                source.enabled ? "bg-emerald-600/60" : "bg-neutral-700"
              }`}
            >
              <span
                className={`inline-block h-2 w-2 rounded-full bg-white transition-transform duration-200 ${
                  source.enabled ? "translate-x-[14px]" : "translate-x-0.5"
                }`}
              />
            </span>
          </button>

          {/* Info */}
          <div className="flex-1 min-w-0">
            <div className="text-sm font-medium capitalize">{source.type}</div>
            <div className="text-xs text-neutral-500">
              {source.enabled ? `Every ${source.intervalMinutes}m` : "Disabled"}
              {source.query ? ` \u00b7 ${source.query}` : ""}
            </div>
          </div>

          {/* Interval */}
          <select
            value={source.intervalMinutes}
            onChange={(e) => handleIntervalChange(source, Number(e.target.value))}
            className="px-2 py-1 bg-neutral-800 border border-neutral-700 rounded text-xs focus:outline-none"
          >
            {[5, 10, 15, 30, 60].map((m) => (
              <option key={m} value={m}>
                {m}m
              </option>
            ))}
          </select>

          {/* Auth button */}
          <button
            onClick={() => handleAuth(source)}
            className="px-2 py-1 bg-neutral-800 hover:bg-neutral-700 rounded text-xs"
            title="Authorize"
          >
            {source.enabled ? "Re-auth" : "Auth"}
          </button>

          {/* Delete */}
          <button
            onClick={() => handleDelete(source)}
            className="text-neutral-600 hover:text-red-400 text-sm"
            title="Remove"
          >
            &times;
          </button>
        </div>
      ))}

      {error && (
        <div className="p-2 bg-red-950 border border-red-800 rounded text-xs text-red-300 mb-2">
          {error}
        </div>
      )}

      {availableTypes.length > 0 && (
        <div className="flex gap-2 mt-2">
          {availableTypes.map((t) => (
            <button
              key={t.type}
              onClick={() => handleAdd(t.type)}
              disabled={adding}
              className="px-3 py-1.5 bg-neutral-800 hover:bg-neutral-700 rounded text-xs disabled:opacity-40"
            >
              + {t.name}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

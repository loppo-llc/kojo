import { useEffect, useState } from "react";
import { agentApi, type NotifySourceConfig, type NotifySourceType } from "../../lib/agentApi";
import { PreconditionFailedError } from "../../lib/httpClient";

interface Props {
  agentId: string;
  // The parent agent's etag at the time AgentSettings loaded the
  // record. Mutations on notify-sources gate on this etag (the rows
  // are JSON entries on the agent record, so the agent etag IS the
  // resource etag for any sub-source write).
  //
  // null when the parent didn't have an etag yet (rare — pre-store
  // legacy fallback). The editor degrades to unconditional writes in
  // that case.
  agentEtag: string | null;
  // Notify the parent when a notify-source mutation bumps the agent
  // etag, so other parts of AgentSettings stay in sync. Optional —
  // when absent, the editor still tracks its own currentEtag locally
  // and re-uses it for chained mutations.
  onAgentEtagChange?: (etag: string | null) => void;
}

export function NotifySourcesEditor({ agentId, agentEtag, onAgentEtagChange }: Props) {
  const [sources, setSources] = useState<NotifySourceConfig[]>([]);
  const [sourceTypes, setSourceTypes] = useState<NotifySourceType[]>([]);
  const [adding, setAdding] = useState(false);
  const [error, setError] = useState("");
  // Tracks the OAuth `state` nonce of the popup currently in flight.
  // The callback HTML postMessages back with state attached; we
  // ignore messages whose state doesn't match so a stale popup from
  // an earlier attempt (same source even) can't overwrite a fresh
  // banner. null when no popup is active. Cleared after we handle
  // the matching message OR when the user opens a new popup.
  const [activeAuthState, setActiveAuthState] = useState<string | null>(null);
  // Local mirror of the parent's etag. Updated after each successful
  // mutation from the response ETag header so two consecutive edits
  // chain without an extra GET. Re-syncs from props if the parent
  // refetches (e.g. agent PATCH from the same form, or the parent's
  // own If-Match-mismatch refetch).
  const [currentEtag, setCurrentEtag] = useState<string | null>(agentEtag);
  useEffect(() => {
    setCurrentEtag(agentEtag);
  }, [agentEtag]);

  const updateEtag = (next: string | null) => {
    if (next === null) return; // server omitted ETag (no v1 store row) — keep last known
    setCurrentEtag(next);
    onAgentEtagChange?.(next);
  };

  useEffect(() => {
    agentApi.notifySources.list(agentId).then(setSources).catch(() => {});
    agentApi.notifySourceTypes().then(setSourceTypes).catch(() => {});
  }, [agentId]);

  // Map an If-Match-related error to a user-facing string and refresh
  // both the sources list AND the parent's agent etag (so a retried
  // click actually carries a fresh precondition). Returns true when
  // the error WAS the stale-state shape and the caller should stop —
  // false when the caller should fall through to its generic error
  // path.
  const handleStale = async (err: unknown): Promise<boolean> => {
    if (!(err instanceof PreconditionFailedError)) return false;
    setError("Notification source changed under us. Reloading…");
    try {
      const fresh = await agentApi.get(agentId);
      setSources(fresh.notifySources ?? []);
      const next = fresh.etag ?? null;
      setCurrentEtag(next);
      onAgentEtagChange?.(next);
    } catch {
      // Refetch failed — leave the editor as-is; the user can retry.
    }
    return true;
  };

  const handleAdd = async (type: string) => {
    setAdding(true);
    setError("");
    try {
      const r = await agentApi.notifySources.create(
        agentId,
        { type, intervalMinutes: 10 },
        currentEtag ?? undefined,
      );
      setSources((prev) => [...prev, r.source]);
      updateEtag(r.agentEtag);
    } catch (err) {
      if (await handleStale(err)) return;
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setAdding(false);
    }
  };

  const handleToggle = async (source: NotifySourceConfig) => {
    setError("");
    try {
      const r = await agentApi.notifySources.update(
        agentId,
        source.id,
        { enabled: !source.enabled },
        currentEtag ?? undefined,
      );
      setSources((prev) => prev.map((s) => (s.id === source.id ? r.source : s)));
      updateEtag(r.agentEtag);
    } catch (err) {
      if (await handleStale(err)) return;
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const handleAuth = async (source: NotifySourceConfig) => {
    setError("");
    try {
      const { authUrl, state } = await agentApi.notifySources.startAuth(
        agentId,
        source.id,
      );
      // Mark THIS popup's state as the active one before opening.
      // If the user double-clicks Auth for the same source, the
      // second click overwrites this and the first popup's late
      // callback (carrying its own state) gets dropped by the
      // listener — closes the "stale same-source popup wins" race
      // that pure sourceId correlation couldn't catch.
      setActiveAuthState(state);
      window.open(authUrl, "_blank", "width=600,height=700");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  // Listen for OAuth callback messages via postMessage:
  //   - oauth_complete: success — server enabled the source under its
  //     own LockPatch, which bumped the agent etag. Refetch the agent
  //     so the editor's currentEtag chains the next mutation; otherwise
  //     the next click would 412.
  //   - oauth_error: failure (agent gone, source deleted mid-flow,
  //     token exchange failed, etc.) — surface the server-supplied
  //     detail so the user knows the popup didn't actually authorize
  //     anything, and refetch sources in case the failure was a
  //     concurrent DELETE we should reflect.
  //
  // Both messages are accepted only when e.origin matches our own
  // origin — a postMessage from a foreign window with a forged
  // {type:"oauth_error"} payload could otherwise overwrite real state
  // or surface a misleading error banner. The OAuth callback runs on
  // the same kojo server, so same-origin is the right policy.
  useEffect(() => {
    const handler = async (e: MessageEvent) => {
      if (e.origin !== window.location.origin) return;
      const data = e.data as {
        type?: string;
        detail?: string;
        sourceId?: string;
        state?: string;
      } | null;
      if (!data?.type) return;
      // state correlation: state-bearing messages MUST match the
      // active popup. Without strict matching, a stale callback
      // arriving after activeAuthState was cleared (e.g. after a
      // successful flow finished and the user reloaded the page,
      // or just after the previous popup completed) could surface
      // its banner over fresh state. Server omits state only for
      // truly state-less failures (missing query param, unknown
      // state) — those bypass correlation so the user still sees
      // "auth was denied at the provider" feedback when nothing
      // else is in flight.
      if (data.state) {
        if (data.state !== activeAuthState) return;
      }
      if (data.type === "oauth_complete") {
        setActiveAuthState(null);
        try {
          const fresh = await agentApi.get(agentId);
          setSources(fresh.notifySources ?? []);
          const next = fresh.etag ?? null;
          setCurrentEtag(next);
          onAgentEtagChange?.(next);
        } catch {
          agentApi.notifySources.list(agentId).then(setSources).catch(() => {});
        }
        return;
      }
      if (data.type === "oauth_error") {
        setActiveAuthState(null);
        setError(data.detail || "Authorization failed.");
        // Re-sync state — a "source_gone" error means a concurrent
        // DELETE landed during auth, and our local list is stale.
        try {
          const fresh = await agentApi.get(agentId);
          setSources(fresh.notifySources ?? []);
          const next = fresh.etag ?? null;
          setCurrentEtag(next);
          onAgentEtagChange?.(next);
        } catch {
          agentApi.notifySources.list(agentId).then(setSources).catch(() => {});
        }
      }
    };
    window.addEventListener("message", handler);
    return () => window.removeEventListener("message", handler);
  }, [agentId, onAgentEtagChange, activeAuthState]);

  const handleDelete = async (source: NotifySourceConfig) => {
    if (!confirm(`Remove ${source.type} notification source?`)) return;
    setError("");
    try {
      const r = await agentApi.notifySources.delete(
        agentId,
        source.id,
        currentEtag ?? undefined,
      );
      setSources((prev) => prev.filter((s) => s.id !== source.id));
      updateEtag(r.agentEtag);
    } catch (err) {
      if (await handleStale(err)) return;
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const handleIntervalChange = async (source: NotifySourceConfig, minutes: number) => {
    setError("");
    try {
      const r = await agentApi.notifySources.update(
        agentId,
        source.id,
        { intervalMinutes: minutes },
        currentEtag ?? undefined,
      );
      setSources((prev) => prev.map((s) => (s.id === source.id ? r.source : s)));
      updateEtag(r.agentEtag);
    } catch (err) {
      if (await handleStale(err)) return;
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
              {source.query ? ` · ${source.query}` : ""}
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

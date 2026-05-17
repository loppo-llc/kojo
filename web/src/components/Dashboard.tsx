import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router";
import { api, type SessionInfo } from "../lib/api";
import { agentApi, type AgentInfo } from "../lib/agentApi";
import { groupdmApi, type GroupDMInfo } from "../lib/groupdmApi";
import { peersApi } from "../lib/peerApi";
import { AgentAvatar } from "./agent/AgentAvatar";
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
    .sort((a, b) => {
      const diff = new Date(b.createdAt).getTime() - new Date(a.createdAt).getTime();
      if (diff !== 0 && !Number.isNaN(diff)) return diff;
      return a.id.localeCompare(b.id);
    });

  const map = new Map<string, SessionInfo[]>();
  for (const s of sorted) {
    // Bucket by (peer, tool, workDir) so a remote-peer claude
    // session in /home/me doesn't merge with a local Hub claude
    // session in the same path — different hosts, different
    // filesystem semantics. Empty peer is the local Hub.
    const key = `${s.peer ?? ""}:${s.tool}:${s.workDir}`;
    const list = map.get(key);
    if (list) list.push(s);
    else map.set(key, [s]);
  }

  const groups: SessionGroup[] = [];
  for (const [key, list] of map) {
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

  groups.sort((a, b) => {
    const aRunning = a.primary.status === "running" ? 1 : 0;
    const bRunning = b.primary.status === "running" ? 1 : 0;
    if (aRunning !== bRunning) return bRunning - aRunning;
    const diff = new Date(b.primary.createdAt).getTime() - new Date(a.primary.createdAt).getTime();
    if (diff !== 0 && !Number.isNaN(diff)) return diff;
    return a.key.localeCompare(b.key);
  });

  return groups;
}

export function Dashboard() {
  const [sessions, setSessions] = useState<SessionInfo[]>([]);
  const [agents, setAgents] = useState<AgentInfo[]>([]);
  const [groupDMs, setGroupDMs] = useState<GroupDMInfo[]>([]);
  const [cronPaused, setCronPaused] = useState(false);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [collapsedAgents, setCollapsedAgents] = useState<Set<string>>(() => {
    try {
      const saved = localStorage.getItem("kojo:collapsed-agents");
      return saved ? new Set(JSON.parse(saved)) : new Set();
    } catch { return new Set(); }
  });
  const [collapsedGroupDMs, setCollapsedGroupDMs] = useState<Set<string>>(() => {
    try {
      const saved = localStorage.getItem("kojo:collapsed-groupdms");
      return saved ? new Set(JSON.parse(saved)) : new Set();
    } catch { return new Set(); }
  });
  const navigate = useNavigate();
  const { state: pushState, loading: pushLoading, subscribe: pushSubscribe } = usePushNotifications();

  useEffect(() => {
    // loadSessions concurrently queries every trusted peer in
    // addition to the local Hub. A session created on peer X with
    // NewSession's peer selector lives in X's manager; Dashboard
    // would otherwise look empty until the user navigated directly.
    // Stamp `peer` on the wire response so subsequent REST + WS
    // calls (delete, restart, terminal, ws) route through the
    // Hub→peer proxy. Failures per peer are swallowed so one
    // unreachable host can't blank the whole list.
    //
    // In-flight guard: peer-routed list calls inherit the 30s
    // proxy timeout. Without the guard a 3s poll would stack up
    // overlapping outbound requests against a slow / offline peer
    // and eventually exhaust the browser's per-host connection
    // pool. The ref skips ticks while one is still resolving;
    // the next interval just runs again.
    let inflight = false;
    const loadSessions = async () => {
      if (inflight) return;
      inflight = true;
      try {
        const local = await api.sessions.list();
        let remote: SessionInfo[] = [];
        try {
          const peers = (await peersApi.list()).items ?? [];
          // Trust direction asymmetry: the local row's `trusted`
          // bit means "this peer may drive privileged ops on THIS
          // host", not "this host may drive ops on the peer".
          // For session-list we need the OTHER direction, which
          // the Hub registry can't observe directly — try every
          // online non-self peer and silently drop 403s / network
          // errors. allSettled keeps one unreachable host from
          // blanking the whole dashboard.
          const remotes = peers.filter((p) => !p.isSelf && p.status === "online");
          const settled = await Promise.allSettled(
            remotes.map((p) =>
              api.sessions.list(p.deviceId).then((rows) =>
                rows.map((r) => ({ ...r, peer: r.peer || p.deviceId })),
              ),
            ),
          );
          remote = settled.flatMap((r) => (r.status === "fulfilled" ? r.value : []));
        } catch {
          /* peer registry unavailable — keep local-only */
        }
        // Dedup by (peer, id) so a Hub-self entry can't collide
        // with a peer-side entry that happens to share an id.
        const merged = new Map<string, SessionInfo>();
        for (const s of [...local, ...remote]) {
          merged.set(`${s.peer ?? ""}::${s.id}`, s);
        }
        setSessions(Array.from(merged.values()));
      } catch (err) {
        console.error(err);
      } finally {
        inflight = false;
      }
    };
    void loadSessions();
    const interval = setInterval(() => void loadSessions(), 3000);
    return () => clearInterval(interval);
  }, []);

  useEffect(() => {
    const loadAgents = () => agentApi.list().then(setAgents).catch(console.error);
    loadAgents();
    const interval = setInterval(loadAgents, 5000);
    return () => clearInterval(interval);
  }, []);

  useEffect(() => {
    const loadGroups = () => groupdmApi.list().then(setGroupDMs).catch(console.error);
    loadGroups();
    const interval = setInterval(loadGroups, 5000);
    return () => clearInterval(interval);
  }, []);

  useEffect(() => {
    const loadPaused = () => agentApi.cronPaused().then(setCronPaused).catch(console.error);
    loadPaused();
    const interval = setInterval(loadPaused, 5000);
    return () => clearInterval(interval);
  }, []);

  const groups = groupSessions(sessions);
  const hasAnySessions = sessions.some((s) => !s.internal);
  // updatedAt is RFC3339 with seconds resolution, so agents touched in the
  // same second tie. Manager.List() iterates a map, so input order is random
  // per request; a 0-return comparator would let that randomness through
  // (Array.sort is stable, but the tie group still re-shuffles each reload).
  // Fall back to id for a deterministic order.
  const sortedAgents = [...agents].sort((a, b) => {
    const diff = new Date(b.updatedAt).getTime() - new Date(a.updatedAt).getTime();
    if (diff !== 0 && !Number.isNaN(diff)) return diff;
    return a.id.localeCompare(b.id);
  });

  const toggleExpand = (key: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  useEffect(() => {
    try { localStorage.setItem("kojo:collapsed-agents", JSON.stringify([...collapsedAgents])); } catch { /* quota / private mode */ }
  }, [collapsedAgents]);

  useEffect(() => {
    try { localStorage.setItem("kojo:collapsed-groupdms", JSON.stringify([...collapsedGroupDMs])); } catch { /* quota / private mode */ }
  }, [collapsedGroupDMs]);

  const toggleCollapseAgent = (agentId: string, e: React.MouseEvent) => {
    e.stopPropagation();
    setCollapsedAgents((prev) => {
      const next = new Set(prev);
      if (next.has(agentId)) next.delete(agentId);
      else next.add(agentId);
      return next;
    });
  };

  const toggleCollapseGroupDM = (groupId: string, e: React.MouseEvent) => {
    e.stopPropagation();
    setCollapsedGroupDMs((prev) => {
      const next = new Set(prev);
      if (next.has(groupId)) next.delete(groupId);
      else next.add(groupId);
      return next;
    });
  };

  // sessionHref preserves the session's home peer so a click on a
  // peer-routed entry lands on the right host. Empty peer → local.
  const sessionHref = (s: SessionInfo) =>
    s.peer ? `/session/${s.id}?peer=${encodeURIComponent(s.peer)}` : `/session/${s.id}`;

  const deleteGroup = async (g: SessionGroup, e: React.MouseEvent) => {
    e.stopPropagation();
    const all = [g.primary, ...g.others];
    const results = await Promise.allSettled(all.map((s) => api.sessions.delete(s.id, s.peer)));
    const deletedKeys = new Set<string>();
    results.forEach((r, i) => {
      if (r.status === "fulfilled") deletedKeys.add(`${all[i].peer ?? ""}::${all[i].id}`);
    });
    if (deletedKeys.size > 0) {
      setSessions((prev) => prev.filter((s) => !deletedKeys.has(`${s.peer ?? ""}::${s.id}`)));
    }
  };

  return (
    <div className="min-h-full bg-neutral-950 text-neutral-200">
      <header className="flex items-center justify-between px-4 py-3 border-b border-neutral-800">
        <h1 className="text-lg font-bold">kojo</h1>
        <div className="flex items-center gap-2">
          <Link
            to="/agents/new"
            className="px-3 py-1.5 bg-neutral-800 hover:bg-neutral-700 rounded text-sm"
          >
            + Agent
          </Link>
          <Link
            to="/new"
            className="px-3 py-1.5 bg-neutral-800 hover:bg-neutral-700 rounded text-sm"
          >
            + Session
          </Link>
          <Link
            to="/settings"
            className="p-1.5 text-neutral-500 hover:text-neutral-300"
            title="Settings"
          >
            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-5 h-5">
              <path fillRule="evenodd" d="M7.84 1.804A1 1 0 018.82 1h2.36a1 1 0 01.98.804l.331 1.652a6.993 6.993 0 011.929 1.115l1.598-.54a1 1 0 011.186.447l1.18 2.044a1 1 0 01-.205 1.251l-1.267 1.113a7.047 7.047 0 010 2.228l1.267 1.113a1 1 0 01.206 1.25l-1.18 2.045a1 1 0 01-1.187.447l-1.598-.54a6.993 6.993 0 01-1.929 1.115l-.33 1.652a1 1 0 01-.98.804H8.82a1 1 0 01-.98-.804l-.331-1.652a6.993 6.993 0 01-1.929-1.115l-1.598.54a1 1 0 01-1.186-.447l-1.18-2.044a1 1 0 01.205-1.251l1.267-1.114a7.05 7.05 0 010-2.227L1.821 7.773a1 1 0 01-.206-1.25l1.18-2.045a1 1 0 011.187-.447l1.598.54A6.993 6.993 0 017.51 3.456l.33-1.652zM10 13a3 3 0 100-6 3 3 0 000 6z" clipRule="evenodd" />
            </svg>
          </Link>
        </div>
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

      <main className="p-4 space-y-6">
        {/* Agents Section */}
        <section>
            <div className="flex items-center justify-between mb-2">
              <h2 className="text-xs font-semibold text-neutral-500 uppercase tracking-wider">Agents</h2>
              {agents.some((a) => (a.cronExpr ?? "") !== "") && (
                <button
                  onClick={() => {
                    const next = !cronPaused;
                    setCronPaused(next);
                    agentApi.setCronPaused(next).catch(() => setCronPaused(!next));
                  }}
                  className="flex items-center gap-1.5"
                >
                  <span className={`relative inline-flex h-3 w-6 shrink-0 items-center rounded-full transition-colors duration-200 ${
                    cronPaused ? "bg-neutral-700" : "bg-emerald-600/60"
                  }`}>
                    <span className={`inline-block h-2 w-2 rounded-full bg-white transition-transform duration-200 ${
                      cronPaused ? "translate-x-0.5" : "translate-x-[14px]"
                    }`} />
                  </span>
                  <span className={`text-[10px] transition-colors ${
                    cronPaused ? "text-neutral-500" : "text-neutral-500"
                  }`}>
                    {cronPaused ? "cron paused" : "cron running"}
                  </span>
                </button>
              )}
            </div>
            {sortedAgents.length === 0 && (
              <p className="text-neutral-500 text-center py-8 text-sm">No agents yet</p>
            )}
            <div className="space-y-2">
              {sortedAgents.map((agent) => {
                const isCollapsed = collapsedAgents.has(agent.id);
                return (
                  <div
                    key={agent.id}
                    className="flex items-center bg-neutral-900 hover:bg-neutral-800 rounded-lg border border-neutral-800 transition-colors"
                  >
                    <button
                      onClick={(e) => toggleCollapseAgent(agent.id, e)}
                      className="p-2 pl-3 text-neutral-600 hover:text-neutral-400 shrink-0 self-stretch flex items-center"
                      title={isCollapsed ? "Expand" : "Collapse"}
                    >
                      <svg
                        className={`w-3 h-3 transition-transform ${isCollapsed ? "" : "rotate-90"}`}
                        fill="none"
                        viewBox="0 0 24 24"
                        stroke="currentColor"
                        strokeWidth={2}
                      >
                        <path strokeLinecap="round" strokeLinejoin="round" d="M9 5l7 7-7 7" />
                      </svg>
                    </button>
                    {agent.holderPeer && (
                      <button
                        onClick={async (e) => {
                          e.stopPropagation();
                          if (!window.confirm(
                            `Force-reclaim "${agent.name}" to this host?\n` +
                            `現在のholder (${agent.holderPeerName || (agent.holderPeer ?? "").slice(0, 8)}) との通信を放棄し、` +
                            `この端末でランタイムを再起動する。`,
                          )) return;
                          try {
                            await agentApi.forceReclaim(agent.id);
                            // List poll picks the new state up on the
                            // next 5s tick; force a refresh for snappy UX.
                            const fresh = await agentApi.list();
                            setAgents(fresh);
                          } catch (err) {
                            console.error("force-reclaim failed", err);
                            window.alert(`Force-reclaim failed: ${(err as Error).message}`);
                          }
                        }}
                        className="px-2 py-1 mr-1 bg-amber-900/60 hover:bg-amber-900 text-amber-200 rounded text-[10px] shrink-0"
                        title="Force-reclaim: rewrite agent_locks back to this host and restart the runtime. Use when device-switch left the agent stuck on an unreachable peer."
                      >
                        強制復帰
                      </button>
                    )}
                    <button
                      onClick={() => navigate(`/agents/${agent.id}`)}
                      className={`flex-1 flex items-center gap-3 text-left min-w-0 ${isCollapsed ? "py-1.5 pr-3 pl-1" : "p-3 pl-1"}`}
                    >
                      {!isCollapsed && (
                        <AgentAvatar agentId={agent.id} name={agent.name} size="lg" cacheBust={agent.avatarHash} />
                      )}
                      {isCollapsed ? (
                        <div className="flex items-center gap-2 flex-1 min-w-0">
                          <span className="font-medium text-sm truncate">{agent.name}</span>
                          {agent.holderPeer && (
                            <span className="text-[10px] text-amber-400/80 shrink-0" title={agent.holderPeer}>
                              転移中 @ {agent.holderPeerName || agent.holderPeer.slice(0, 8)}
                            </span>
                          )}
                          <span className="text-[10px] text-neutral-600 font-mono">{agent.tool}</span>
                          <span className="text-[10px] text-neutral-600 shrink-0 ml-auto">
                            {agent.lastMessage
                              ? timeAgo(agent.lastMessage.timestamp)
                              : timeAgo(agent.createdAt)}
                          </span>
                        </div>
                      ) : (
                        <div className="flex-1 min-w-0">
                          <div className="flex items-center justify-between">
                            <span className="font-medium text-sm truncate">{agent.name}</span>
                            <span className="text-[10px] text-neutral-600 shrink-0 ml-2">
                              {agent.lastMessage
                                ? timeAgo(agent.lastMessage.timestamp)
                                : timeAgo(agent.createdAt)}
                            </span>
                          </div>
                          <div className="text-xs text-neutral-500 truncate mt-0.5">
                            {agent.holderPeer
                              ? `転移中 @ ${agent.holderPeerName || agent.holderPeer.slice(0, 8)} — 最新発言はこの端末では未反映`
                              : agent.lastMessage
                                ? `${agent.lastMessage.role === "user" ? "You: " : ""}${agent.lastMessage.content}`
                                : agent.persona
                                  ? agent.persona.slice(0, 60) + (agent.persona.length > 60 ? "..." : "")
                                  : "No messages yet"}
                          </div>
                          <div className="flex items-center gap-2 mt-1">
                            <span className="text-[10px] text-neutral-600 font-mono">{agent.tool}</span>
                            {agent.model && (
                              <span className="text-[10px] text-neutral-600 font-mono">{agent.model}</span>
                            )}
                            {agent.workDir && (
                              <span className="text-[10px] text-neutral-600 font-mono truncate">{agent.workDir}</span>
                            )}
                          </div>
                        </div>
                      )}
                    </button>
                  </div>
                );
              })}
            </div>
          </section>

        {/* Group DMs Section */}
        <section>
          <div className="flex items-center justify-between mb-2">
            <h2 className="text-xs font-semibold text-neutral-500 uppercase tracking-wider">Group DMs</h2>
          </div>
          {groupDMs.length === 0 && (
            <p className="text-neutral-500 text-center py-8 text-sm">No group DMs</p>
          )}
          <div className="space-y-2">
            {[...groupDMs]
              .sort((a, b) => {
                const diff = new Date(b.updatedAt).getTime() - new Date(a.updatedAt).getTime();
                if (diff !== 0 && !Number.isNaN(diff)) return diff;
                return a.id.localeCompare(b.id);
              })
              .map((g) => {
                const isCollapsed = collapsedGroupDMs.has(g.id);
                return (
                  <div
                    key={g.id}
                    className="flex items-center bg-neutral-900 hover:bg-neutral-800 rounded-lg border border-neutral-800 transition-colors"
                  >
                    <button
                      onClick={(e) => toggleCollapseGroupDM(g.id, e)}
                      className="p-2 pl-3 text-neutral-600 hover:text-neutral-400 shrink-0 self-stretch flex items-center"
                      title={isCollapsed ? "Expand" : "Collapse"}
                    >
                      <svg
                        className={`w-3 h-3 transition-transform ${isCollapsed ? "" : "rotate-90"}`}
                        fill="none"
                        viewBox="0 0 24 24"
                        stroke="currentColor"
                        strokeWidth={2}
                      >
                        <path strokeLinecap="round" strokeLinejoin="round" d="M9 5l7 7-7 7" />
                      </svg>
                    </button>
                    <button
                      onClick={() => navigate(`/groupdms/${g.id}`)}
                      className={`flex-1 flex items-center gap-3 text-left min-w-0 ${isCollapsed ? "py-1.5 pr-3 pl-1" : "p-3 pl-1"}`}
                    >
                      {!isCollapsed && (
                        <div className="flex -space-x-1.5 shrink-0">
                          {g.members.slice(0, 3).map((m) => (
                            <AgentAvatar key={m.agentId} agentId={m.agentId} name={m.agentName} size="sm" />
                          ))}
                        </div>
                      )}
                      {isCollapsed ? (
                        <div className="flex items-center gap-2 flex-1 min-w-0">
                          <span className="font-medium text-sm truncate">{g.name}</span>
                          <span className="text-[10px] text-neutral-600 shrink-0 ml-auto">
                            {timeAgo(g.updatedAt)}
                          </span>
                        </div>
                      ) : (
                        <div className="flex-1 min-w-0">
                          <div className="flex items-center justify-between">
                            <span className="font-medium text-sm truncate">{g.name}</span>
                            <span className="text-[10px] text-neutral-600 shrink-0 ml-2">
                              {timeAgo(g.updatedAt)}
                            </span>
                          </div>
                          <div className="text-xs text-neutral-500 truncate mt-0.5">
                            {g.members.map((m) => m.agentName).join(", ")}
                          </div>
                        </div>
                      )}
                    </button>
                  </div>
                );
              })}
          </div>
        </section>

        {/* Sessions Section */}
        <section>
          <div className="flex items-center justify-between mb-2">
            <h2 className="text-xs font-semibold text-neutral-500 uppercase tracking-wider">Sessions</h2>
          </div>

          {!hasAnySessions && (
            <p className="text-neutral-500 text-center py-8 text-sm">No sessions</p>
          )}

          <div className="space-y-3">
            {groups.map((g) => {
              const runningOthers = g.others.filter((s) => s.status === "running");
              const stoppedOthers = g.others.filter((s) => s.status !== "running");
              const allExited = g.primary.status !== "running" && runningOthers.length === 0;
              return (
                <div key={g.key} className="bg-neutral-900 rounded-lg border border-neutral-800 relative">
                  <button
                    onClick={() => navigate(sessionHref(g.primary))}
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
                      onClick={() => navigate(sessionHref(s))}
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
                          onClick={() => navigate(sessionHref(s))}
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
            })}
          </div>
        </section>
      </main>
    </div>
  );
}

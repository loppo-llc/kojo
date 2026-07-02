import { useEffect, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router";
import { api, type SessionInfo } from "../lib/api";
import { agentApi, type AgentInfo } from "../lib/agentApi";
import { groupdmApi, type GroupDMInfo } from "../lib/groupdmApi";
import { peersApi, type PeerInfo } from "../lib/peerApi";
import { AgentAvatar } from "./agent/AgentAvatar";
import { usePushNotifications } from "../hooks/usePushNotifications";
import { useCollapsedSet } from "../hooks/useCollapsedSet";
import { errMsg } from "../lib/utils";
import { Header } from "./ui/Header";
import { Lamp, type LampState } from "./ui/Lamp";
import { Chip } from "./ui/Chip";
import { RelTime } from "./ui/RelTime";
import { Button } from "./ui/Button";

function sessionLampState(s: SessionInfo): LampState {
  if (s.status === "running") return "run";
  if (s.exitCode !== undefined && s.exitCode !== 0) return "err";
  return "off";
}

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

interface DashboardProps {
  /**
   * "page" (default) is the standalone full-page list. "sidebar" is the
   * persistent left pane of the lg+ two-pane shell: identical markup below
   * lg (so 390px behavior is byte-identical), with lg: overrides for a
   * full-width compact column and an active-row highlight derived from the
   * current route. The active/compact styling is applied only via lg:
   * utilities, so nothing changes below the lg boundary.
   */
  variant?: "page" | "sidebar";
}

export function Dashboard({ variant = "page" }: DashboardProps) {
  const location = useLocation();
  const sidebar = variant === "sidebar";
  // Active row is derived from the URL so it survives remounts and stays in
  // sync with the detail pane. Only meaningful in the sidebar (lg+), and the
  // highlight is applied through lg: classes so mobile markup is untouched.
  const activeAgentId = location.pathname.match(/^\/agents\/([^/]+)/)?.[1] ?? null;
  const activeGroupId = location.pathname.match(/^\/groupdms\/([^/]+)/)?.[1] ?? null;
  const activeSessionId = location.pathname.match(/^\/session\/([^/]+)/)?.[1] ?? null;
  // Reserve a 2px left edge on every sidebar row (transparent when inactive)
  // so switching the active row never shifts row content horizontally.
  const rowEdge = (active: boolean) =>
    sidebar ? (active ? " lg:border-l-2 lg:border-copper lg:bg-hover" : " lg:border-l-2 lg:border-l-transparent") : "";
  const [sessions, setSessions] = useState<SessionInfo[]>([]);
  const [agents, setAgents] = useState<AgentInfo[]>([]);
  const [groupDMs, setGroupDMs] = useState<GroupDMInfo[]>([]);
  const [cronPaused, setCronPaused] = useState(false);
  const [showCreateGroupDialog, setShowCreateGroupDialog] = useState(false);
  const [newGroupName, setNewGroupName] = useState("");
  const [newGroupMemberIds, setNewGroupMemberIds] = useState<Set<string>>(new Set());
  const [newGroupNotifyMembers, setNewGroupNotifyMembers] = useState(false);
  const [creatingGroup, setCreatingGroup] = useState(false);
  const [createGroupError, setCreateGroupError] = useState("");
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [collapsedAgents, toggleCollapseAgent] = useCollapsedSet("kojo:collapsed-agents");
  const [collapsedGroupDMs, toggleCollapseGroupDM] = useCollapsedSet("kojo:collapsed-groupdms");
  const navigate = useNavigate();
  const { state: pushState, loading: pushLoading, subscribe: pushSubscribe } = usePushNotifications();
  const createGroupDialogRef = useRef<HTMLDivElement>(null);
  // peerCacheRef holds the last successful session list per peer.
  // Lives outside the loadSessions effect so deleteGroup can evict
  // entries it just removed — otherwise a delete would visibly
  // "undo" itself on the next poll, repainting the row from cache
  // until the owning peer responds again.
  const peerCacheRef = useRef<Map<string, { rows: SessionInfo[]; updatedAt: number }>>(new Map());
  // tombstonesRef gates deleted session keys against in-flight
  // peer list calls. deleteGroup runs `api.sessions.delete` on the
  // peer, but a concurrent loadSessions tick may have already
  // dispatched the GET against the same peer and will return rows
  // that still contain the about-to-be-deleted session. Without a
  // tombstone the stale rows would land in peerCache and the
  // deleted row would reappear until the next successful poll.
  // Entries expire after tombstoneTTL (>= proxy timeout) so the
  // map can't grow unbounded.
  const tombstonesRef = useRef<Map<string, number>>(new Map());

  useEffect(() => {
    // loadSessions concurrently queries every non-self peer in
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
    // pool. The flag skips ticks while one is still resolving;
    // the next interval just runs again.
    //
    // Stale-while-error: per-peer cache (peerCacheRef) keeps the
    // last successful response for each peer. A rejected fetch
    // (offline peer hitting the proxy timeout) leaves the previous
    // entry in place so its sessions stay visible instead of
    // blanking. We repaint at the top of every tick with
    // (local + cached remote) so a single slow peer can't freeze
    // local-row updates for 30s behind the allSettled barrier.
    //
    // Expiry: cached entries older than peerCacheTTL are dropped.
    // Without this, a peer that was unpaired on the other side
    // (or had its session genuinely deleted while we were unable
    // to reach it) would haunt the dashboard with ghost rows
    // forever — the rejected fetches would keep them pinned. The
    // TTL bounds that staleness to one window.
    const peerCacheTTL = 60_000;
    let inflight = false;
    let cancelled = false;
    const peerCache = peerCacheRef.current;
    const tombstones = tombstonesRef.current;

    const sweepTombstones = () => {
      const now = Date.now();
      for (const [k, exp] of [...tombstones.entries()]) {
        if (exp <= now) tombstones.delete(k);
      }
    };

    const mergeAll = (local: SessionInfo[]) => {
      // Dedup by (peer, id) so a Hub-self entry can't collide
      // with a peer-side entry that happens to share an id.
      sweepTombstones();
      const m = new Map<string, SessionInfo>();
      const keep = (s: SessionInfo) => {
        const k = `${s.peer ?? ""}::${s.id}`;
        if (tombstones.has(k)) return;
        m.set(k, s);
      };
      for (const s of local) keep(s);
      const now = Date.now();
      for (const [k, entry] of [...peerCache.entries()]) {
        if (now - entry.updatedAt > peerCacheTTL) {
          peerCache.delete(k);
          continue;
        }
        for (const s of entry.rows) keep(s);
      }
      return Array.from(m.values());
    };

    const loadSessions = async () => {
      if (inflight) return;
      inflight = true;
      try {
        const local = await api.sessions.list();
        if (cancelled) return;
        // Paint local + last-known remote up front. This is the
        // line that keeps the dashboard responsive when a peer is
        // offline: without it, every tick would block on the
        // proxy timeout below before any setState fired, even
        // though local rows are already in hand.
        setSessions(mergeAll(local));

        let peers: PeerInfo[] = [];
        try {
          peers = (await peersApi.list()).items ?? [];
        } catch {
          // peer registry unavailable: keep local + last cache
          return;
        }
        if (cancelled) return;
        // Don't pre-filter by p.status: a peer marked "offline"
        // in the hub registry might still answer (heartbeat lag,
        // or it just came back). allSettled below absorbs peers
        // that are truly unreachable, so one offline peer never
        // blanks the whole dashboard.
        const remotes = peers.filter((p) => !p.isSelf);
        // Evict cache for peers that vanished from the registry
        // BEFORE awaiting the per-peer list calls. Otherwise an
        // unregister stays masked behind the slowest peer's
        // 30s proxy timeout — the registry already says the peer
        // is gone, so its rows are stale immediately.
        const live = new Set(remotes.map((p) => p.deviceId));
        let evicted = false;
        for (const k of [...peerCache.keys()]) {
          if (!live.has(k)) {
            peerCache.delete(k);
            evicted = true;
          }
        }
        if (evicted) setSessions(mergeAll(local));

        const settled = await Promise.allSettled(
          remotes.map((p) =>
            // Stamp completedAt inside the .then so a fast peer's
            // TTL clock starts when ITS response arrived, not when
            // the slowest peer in this batch finally settled.
            // Otherwise a 60s stale-while-error window can stretch
            // toward 60 + slowestTimeout for the fast peer.
            api.sessions.list(p.deviceId).then((rows) => ({
              rows: rows.map((r) => ({ ...r, peer: r.peer || p.deviceId })),
              completedAt: Date.now(),
            })),
          ),
        );
        if (cancelled) return;
        // Per-peer cache update: fulfilled overwrites, rejected
        // keeps the previous entry (stale-while-error). Filter
        // through tombstones so a row deleted while this fetch
        // was in flight doesn't reappear from a peer that
        // hadn't yet observed the delete.
        remotes.forEach((p, i) => {
          const r = settled[i];
          if (r.status !== "fulfilled") return;
          const filtered = r.value.rows.filter(
            (s) => !tombstones.has(`${s.peer ?? ""}::${s.id}`),
          );
          peerCache.set(p.deviceId, { rows: filtered, updatedAt: r.value.completedAt });
        });
        setSessions(mergeAll(local));
      } catch (err) {
        if (!cancelled) console.error(err);
      } finally {
        inflight = false;
      }
    };
    void loadSessions();
    const interval = setInterval(() => void loadSessions(), 3000);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
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
    if (showCreateGroupDialog) createGroupDialogRef.current?.focus();
  }, [showCreateGroupDialog]);

  useEffect(() => {
    const loadPaused = () => agentApi.cronPaused().then(setCronPaused).catch(console.error);
    loadPaused();
    const interval = setInterval(loadPaused, 5000);
    return () => clearInterval(interval);
  }, []);

  const groups = groupSessions(sessions);
  const hasAnySessions = sessions.some((s) => !s.internal);
  const visibleSessions = sessions.filter((s) => !s.internal);
  const runningCount = visibleSessions.filter((s) => s.status === "running").length;
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
  const selectedNewGroupMembers = sortedAgents.filter((a) => newGroupMemberIds.has(a.id));

  const resetCreateGroupForm = () => {
    setNewGroupName("");
    setNewGroupMemberIds(new Set());
    setNewGroupNotifyMembers(false);
    setCreateGroupError("");
  };

  const toggleNewGroupMember = (agentId: string) => {
    setNewGroupMemberIds((prev) => {
      const next = new Set(prev);
      if (next.has(agentId)) next.delete(agentId);
      else next.add(agentId);
      return next;
    });
  };

  const submitCreateGroup = async (e: React.FormEvent) => {
    e.preventDefault();
    const memberIds = [...newGroupMemberIds];
    if (memberIds.length < 2) {
      setCreateGroupError("Select at least 2 members");
      return;
    }
    setCreatingGroup(true);
    setCreateGroupError("");
    try {
      const created = await groupdmApi.create(newGroupName.trim(), memberIds, {
        notifyMembers: newGroupNotifyMembers,
      });
      setGroupDMs((prev) => [created, ...prev.filter((g) => g.id !== created.id)]);
      resetCreateGroupForm();
      setShowCreateGroupDialog(false);
      navigate(`/groupdms/${created.id}`);
    } catch (err) {
      setCreateGroupError(err instanceof Error ? err.message : "Failed to create group");
    } finally {
      setCreatingGroup(false);
    }
  };

  const toggleExpand = (key: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
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
      // Evict deleted rows from the per-peer cache too. Without
      // this, the next loadSessions tick would repaint the row
      // from cache (because the owning peer might still be on its
      // proxy timeout) and the deletion would visibly flicker
      // back in until the peer responds again.
      for (const [k, entry] of peerCacheRef.current.entries()) {
        const next = entry.rows.filter((s) => !deletedKeys.has(`${s.peer ?? ""}::${s.id}`));
        if (next.length !== entry.rows.length) {
          peerCacheRef.current.set(k, { rows: next, updatedAt: entry.updatedAt });
        }
      }
      // Tombstone the keys for one proxy-timeout window. A
      // loadSessions tick that dispatched its peer GET before
      // this delete landed will still return the row; without
      // a tombstone it would re-enter peerCache and repaint
      // until the next successful poll observed the delete.
      const expiry = Date.now() + 60_000;
      for (const k of deletedKeys) tombstonesRef.current.set(k, expiry);
    }
  };

  return (
    <div className="min-h-full bg-app text-ink">
      <Header />

      <div className={`mx-auto max-w-[720px] px-4${sidebar ? " lg:max-w-none lg:px-3" : ""}`}>
        {/* Fleet summary strip */}
        <div className="flex items-center gap-1.5 pt-3 font-mono text-[12px] text-ink-dim">
          <Lamp state="run" pulse={runningCount > 0} size={7} />
          <span className="truncate">
            {runningCount} running · {sortedAgents.length} agents · {visibleSessions.length} sessions
            {groupDMs.length > 0 ? ` · ${groupDMs.length} DMs` : ""}
          </span>
        </div>

        {pushState === "default" && (
          <div className="mt-3 flex items-center gap-3 rounded-[10px] border border-hairline bg-surface px-3 py-2">
            <span className="flex-1 text-[13px] text-ink-dim">
              Enable notifications when sessions finish?
            </span>
            <button
              onClick={pushSubscribe}
              disabled={pushLoading}
              className="whitespace-nowrap text-[13px] font-medium text-copper transition-colors hover:text-copper-bright disabled:opacity-40"
            >
              {pushLoading ? "..." : "Enable"}
            </button>
          </div>
        )}

        <main className="space-y-6 py-4">
        {/* Agents Section */}
        <section>
          <div className="mb-2 flex items-center justify-between px-0.5">
            <h2 className="font-mono text-[11px] uppercase tracking-wide text-ink-faint">
              Agents · {sortedAgents.length}
            </h2>
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
                  cronPaused ? "bg-hairline" : "bg-lamp-run/50"
                }`}>
                  <span className={`inline-block h-2 w-2 rounded-full bg-ink transition-transform duration-200 ${
                    cronPaused ? "translate-x-0.5" : "translate-x-[14px]"
                  }`} />
                </span>
                <span className="font-mono text-[10px] text-ink-faint">
                  {cronPaused ? "cron paused" : "cron running"}
                </span>
              </button>
            )}
          </div>

          {sortedAgents.length === 0 ? (
            <p className="py-8 text-center text-sm text-ink-faint">No agents yet</p>
          ) : (
            <div className="divide-y divide-hairline overflow-hidden rounded-[10px] border border-hairline bg-surface">
              {sortedAgents.map((agent) => {
                const open = !collapsedAgents.has(agent.id);
                const ts = agent.lastMessage ? agent.lastMessage.timestamp : agent.createdAt;
                const preview = agent.holderPeer
                  ? `転移中 @ ${agent.holderPeerName || agent.holderPeer.slice(0, 8)} — 最新発言はこの端末では未反映`
                  : agent.lastMessage
                    ? `${agent.lastMessage.role === "user" ? "You: " : ""}${agent.lastMessage.content}`
                    : agent.persona
                      ? agent.persona.slice(0, 60) + (agent.persona.length > 60 ? "..." : "")
                      : "No messages yet";
                return (
                  <div key={agent.id} className={`flex items-stretch transition-colors hover:bg-hover${rowEdge(agent.id === activeAgentId)}`}>
                    <button
                      onClick={() => navigate(`/agents/${agent.id}`)}
                      className="flex min-w-0 flex-1 items-center gap-3 py-3 pl-3 pr-1 text-left"
                    >
                      <AgentAvatar agentId={agent.id} name={agent.name} size="xs" cacheBust={agent.avatarHash} />
                      <div className="min-w-0 flex-1">
                        <div className="flex items-baseline gap-2">
                          <span className="min-w-0 truncate text-[15px] font-semibold text-ink">{agent.name}</span>
                          {agent.holderPeer && (
                            <span className="shrink-0 text-[10px] text-copper" title={agent.holderPeer}>
                              転移中 @ {agent.holderPeerName || agent.holderPeer.slice(0, 8)}
                            </span>
                          )}
                          <RelTime value={ts} className="ml-auto" />
                        </div>
                        {open && (
                          <>
                            <div className="mt-0.5 truncate text-[13px] text-ink-dim">{preview}</div>
                            <div className="mt-1 flex min-w-0 items-center gap-1.5 overflow-hidden">
                              <Chip className="shrink-0">{agent.tool}</Chip>
                              {agent.model && <Chip className="min-w-0 max-w-[45%]">{agent.model}</Chip>}
                              {agent.workDir && (
                                <span className="min-w-0 flex-1 truncate font-mono text-[11px] text-ink-faint">{agent.workDir}</span>
                              )}
                            </div>
                          </>
                        )}
                      </div>
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
                            window.alert(`Force-reclaim failed: ${errMsg(err)}`);
                          }
                        }}
                        className="my-2 mr-1 shrink-0 self-start rounded-full border border-lamp-warn/40 px-2 py-1 text-[10px] text-lamp-warn transition-colors hover:bg-lamp-warn/10"
                        title="Force-reclaim: rewrite agent_locks back to this host and restart the runtime. Use when device-switch left the agent stuck on an unreachable peer."
                      >
                        強制復帰
                      </button>
                    )}

                    <button
                      onClick={(e) => toggleCollapseAgent(agent.id, e)}
                      className="flex w-8 shrink-0 items-center justify-center text-ink-faint transition-colors hover:text-ink-dim"
                      title={open ? "Collapse" : "Expand"}
                      aria-label={open ? "Collapse" : "Expand"}
                    >
                      <svg
                        className={`h-3 w-3 transition-transform ${open ? "rotate-90" : ""}`}
                        fill="none"
                        viewBox="0 0 24 24"
                        stroke="currentColor"
                        strokeWidth={2}
                      >
                        <path strokeLinecap="round" strokeLinejoin="round" d="M9 5l7 7-7 7" />
                      </svg>
                    </button>
                  </div>
                );
              })}
            </div>
          )}
        </section>

        {/* Group DMs Section */}
        <section>
          <div className="mb-2 flex items-center justify-between px-0.5">
            <h2 className="font-mono text-[11px] uppercase tracking-wide text-ink-faint">
              Group DMs · {groupDMs.length}
            </h2>
            <button
              onClick={() => {
                resetCreateGroupForm();
                setShowCreateGroupDialog(true);
              }}
              disabled={sortedAgents.length < 2}
              className="rounded-full border border-hairline px-2.5 py-1 font-mono text-[11px] text-ink-dim transition-colors hover:bg-hover hover:text-ink disabled:opacity-40"
            >
              + Group
            </button>
          </div>

          {groupDMs.length === 0 ? (
            <p className="py-8 text-center text-sm text-ink-faint">No group DMs</p>
          ) : (
            <div className="divide-y divide-hairline overflow-hidden rounded-[10px] border border-hairline bg-surface">
              {[...groupDMs]
                .sort((a, b) => {
                  const diff = new Date(b.updatedAt).getTime() - new Date(a.updatedAt).getTime();
                  if (diff !== 0 && !Number.isNaN(diff)) return diff;
                  return a.id.localeCompare(b.id);
                })
                .map((g) => {
                  const open = !collapsedGroupDMs.has(g.id);
                  return (
                    <div key={g.id} className={`flex items-stretch transition-colors hover:bg-hover${rowEdge(g.id === activeGroupId)}`}>
                      <button
                        onClick={() => navigate(`/groupdms/${g.id}`)}
                        className="flex min-w-0 flex-1 items-center gap-3 py-3 pl-3 pr-1 text-left"
                      >
                        <div className="flex shrink-0 -space-x-1.5">
                          {g.members.slice(0, 3).map((m) => (
                            <AgentAvatar key={m.agentId} agentId={m.agentId} name={m.agentName} size="xs" />
                          ))}
                        </div>
                        <div className="min-w-0 flex-1">
                          <div className="flex items-baseline gap-2">
                            <span className="min-w-0 truncate text-[15px] font-semibold text-ink">{g.name}</span>
                            <RelTime value={g.updatedAt} className="ml-auto" />
                          </div>
                          {open && (
                            <div className="mt-0.5 truncate text-[13px] text-ink-dim">
                              {g.members.map((m) => m.agentName).join(", ")}
                            </div>
                          )}
                        </div>
                      </button>

                      <button
                        onClick={(e) => toggleCollapseGroupDM(g.id, e)}
                        className="flex w-8 shrink-0 items-center justify-center text-ink-faint transition-colors hover:text-ink-dim"
                        title={open ? "Collapse" : "Expand"}
                        aria-label={open ? "Collapse" : "Expand"}
                      >
                        <svg
                          className={`h-3 w-3 transition-transform ${open ? "rotate-90" : ""}`}
                          fill="none"
                          viewBox="0 0 24 24"
                          stroke="currentColor"
                          strokeWidth={2}
                        >
                          <path strokeLinecap="round" strokeLinejoin="round" d="M9 5l7 7-7 7" />
                        </svg>
                      </button>
                    </div>
                  );
                })}
            </div>
          )}
        </section>

        {/* Sessions Section */}
        <section>
          <div className="mb-2 flex items-center justify-between px-0.5">
            <h2 className="font-mono text-[11px] uppercase tracking-wide text-ink-faint">
              Sessions · {groups.length}
            </h2>
          </div>

          {!hasAnySessions ? (
            <p className="py-8 text-center text-sm text-ink-faint">No sessions</p>
          ) : (
            <div className="divide-y divide-hairline overflow-hidden rounded-[10px] border border-hairline bg-surface">
              {groups.map((g) => {
                const runningOthers = g.others.filter((s) => s.status === "running");
                const stoppedOthers = g.others.filter((s) => s.status !== "running");
                const allExited = g.primary.status !== "running" && runningOthers.length === 0;
                const groupActive =
                  activeSessionId !== null &&
                  [g.primary, ...g.others].some((s) => s.id === activeSessionId);
                return (
                  <div key={g.key}>
                    <div className={`flex items-stretch transition-colors hover:bg-hover${rowEdge(groupActive)}`}>
                      <button
                        onClick={() => navigate(sessionHref(g.primary))}
                        className="flex min-w-0 flex-1 items-center gap-2.5 px-3 py-3 text-left"
                      >
                        <Lamp
                          state={sessionLampState(g.primary)}
                          pulse={g.primary.status === "running"}
                        />
                        <div className="min-w-0 flex-1">
                          <div className="flex min-w-0 items-center gap-2">
                            <span className="font-mono text-[14px] font-semibold text-ink">{g.tool}</span>
                            {g.primary.toolSessionId && (
                              <span className="truncate font-mono text-[11px] text-ink-faint">
                                {g.primary.toolSessionId.slice(0, 8)}
                              </span>
                            )}
                            {g.primary.exitCode !== undefined && (
                              <span className="shrink-0 font-mono text-[11px] text-ink-faint">
                                exit {g.primary.exitCode}
                              </span>
                            )}
                          </div>
                          <div className="mt-0.5 truncate font-mono text-[12px] text-ink-dim">{g.workDir}</div>
                        </div>
                      </button>
                      <div className="flex shrink-0 items-center gap-0.5 pr-2">
                        <RelTime value={g.primary.createdAt} className="mr-1" />
                        <button
                          onClick={() => navigate(`/new?tool=${encodeURIComponent(g.tool)}&workDir=${encodeURIComponent(g.workDir)}`)}
                          className="rounded-[10px] p-2 text-ink-faint transition-colors hover:bg-hover hover:text-ink"
                          title="New session"
                          aria-label="New session"
                        >
                          <svg viewBox="0 0 20 20" fill="currentColor" className="h-5 w-5">
                            <path d="M10.75 4.75a.75.75 0 00-1.5 0v4.5h-4.5a.75.75 0 000 1.5h4.5v4.5a.75.75 0 001.5 0v-4.5h4.5a.75.75 0 000-1.5h-4.5v-4.5z" />
                          </svg>
                        </button>
                        {allExited && (
                          <button
                            onClick={(e) => deleteGroup(g, e)}
                            className="rounded-[10px] p-2 text-ink-faint transition-colors hover:bg-hover hover:text-lamp-err"
                            title="Remove"
                            aria-label="Remove"
                          >
                            <svg viewBox="0 0 20 20" fill="currentColor" className="h-5 w-5">
                              <path fillRule="evenodd" d="M8.75 1A2.75 2.75 0 006 3.75v.443c-.795.077-1.584.176-2.365.298a.75.75 0 10.23 1.482l.149-.022.841 10.518A2.75 2.75 0 007.596 19h4.807a2.75 2.75 0 002.742-2.53l.841-10.519.149.023a.75.75 0 00.23-1.482A41.03 41.03 0 0014 4.193V3.75A2.75 2.75 0 0011.25 1h-2.5zM10 4c.84 0 1.673.025 2.5.075V3.75c0-.69-.56-1.25-1.25-1.25h-2.5c-.69 0-1.25.56-1.25 1.25v.325C8.327 4.025 9.16 4 10 4zM8.58 7.72a.75.75 0 00-1.5.06l.3 7.5a.75.75 0 101.5-.06l-.3-7.5zm4.34.06a.75.75 0 10-1.5-.06l-.3 7.5a.75.75 0 101.5.06l.3-7.5z" clipRule="evenodd" />
                            </svg>
                          </button>
                        )}
                      </div>
                    </div>
                    {runningOthers.map((s) => (
                      <button
                        key={s.id}
                        onClick={() => navigate(sessionHref(s))}
                        className="flex w-full items-center gap-2 border-t border-hairline px-3 py-2 text-left font-mono text-[11px] text-ink-faint transition-colors hover:bg-hover"
                      >
                        <Lamp state="run" pulse size={6} />
                        <span>{s.status}</span>
                        {s.toolSessionId && <span>{s.toolSessionId.slice(0, 8)}</span>}
                        <RelTime value={s.createdAt} className="ml-auto" />
                      </button>
                    ))}
                    {stoppedOthers.length > 0 && (
                      <>
                        <button
                          onClick={() => toggleExpand(g.key)}
                          className="w-full border-t border-hairline px-3 py-2 text-left font-mono text-[11px] text-ink-faint transition-colors hover:text-ink-dim"
                        >
                          {expanded.has(g.key) ? "Hide" : `+${stoppedOthers.length} more`}
                        </button>
                        {expanded.has(g.key) && stoppedOthers.map((s) => (
                          <button
                            key={s.id}
                            onClick={() => navigate(sessionHref(s))}
                            className="flex w-full items-center gap-2 border-t border-hairline px-3 py-2 text-left font-mono text-[11px] text-ink-faint transition-colors hover:bg-hover"
                          >
                            <Lamp state={sessionLampState(s)} size={6} />
                            <span>{s.status}</span>
                            {s.toolSessionId && <span>{s.toolSessionId.slice(0, 8)}</span>}
                            {s.exitCode !== undefined && <span>(exit {s.exitCode})</span>}
                            <RelTime value={s.createdAt} className="ml-auto" />
                          </button>
                        ))}
                      </>
                    )}
                  </div>
                );
              })}
            </div>
          )}
        </section>
        </main>
      </div>

      {showCreateGroupDialog && (
        <div
          ref={createGroupDialogRef}
          tabIndex={-1}
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4 outline-none"
          onClick={(e) => {
            if (e.target === e.currentTarget && !creatingGroup) {
              setShowCreateGroupDialog(false);
            }
          }}
          onKeyDown={(e) => {
            if (e.key === "Escape" && !creatingGroup) setShowCreateGroupDialog(false);
          }}
        >
          <form
            onSubmit={submitCreateGroup}
            className="flex max-h-[85vh] w-full max-w-[420px] flex-col rounded-[10px] border border-hairline bg-raised shadow-xl shadow-black/50"
          >
            <div className="flex shrink-0 items-center justify-between border-b border-hairline px-4 py-3">
              <h3 className="text-sm font-medium text-ink">New group DM</h3>
              <button
                type="button"
                onClick={() => setShowCreateGroupDialog(false)}
                disabled={creatingGroup}
                className="rounded p-1 text-ink-faint transition-colors hover:text-ink disabled:opacity-40"
                aria-label="Close"
              >
                <svg viewBox="0 0 16 16" fill="currentColor" className="h-4 w-4">
                  <path d="M5.28 4.22a.75.75 0 00-1.06 1.06L6.94 8l-2.72 2.72a.75.75 0 101.06 1.06L8 9.06l2.72 2.72a.75.75 0 101.06-1.06L9.06 8l2.72-2.72a.75.75 0 00-1.06-1.06L8 6.94 5.28 4.22z" />
                </svg>
              </button>
            </div>

            <div className="space-y-4 overflow-y-auto p-4">
              <label className="block">
                <span className="mb-1 block text-xs text-ink-dim">Name</span>
                <input
                  value={newGroupName}
                  onChange={(e) => setNewGroupName(e.target.value)}
                  placeholder={selectedNewGroupMembers.map((a) => a.name).join(", ")}
                  disabled={creatingGroup}
                  className="w-full rounded-[10px] border border-hairline bg-app px-3 py-2 text-sm text-ink placeholder:text-ink-faint focus:border-copper disabled:opacity-50"
                  autoFocus
                />
              </label>

              <label className="flex cursor-pointer select-none items-center gap-2 text-sm text-ink">
                <input
                  type="checkbox"
                  checked={newGroupNotifyMembers}
                  onChange={(e) => setNewGroupNotifyMembers(e.target.checked)}
                  disabled={creatingGroup}
                  className="rounded border-hairline bg-raised accent-[color:var(--color-copper)]"
                />
                Notify members
              </label>

              <div>
                <div className="mb-2 flex items-center justify-between">
                  <span className="text-xs text-ink-dim">Members</span>
                  <span className="text-[10px] text-ink-faint">{newGroupMemberIds.size} selected</span>
                </div>
                <div className="space-y-1.5">
                  {sortedAgents.map((agent) => {
                    const checked = newGroupMemberIds.has(agent.id);
                    return (
                      <label
                        key={agent.id}
                        className={`flex cursor-pointer select-none items-center gap-3 rounded-[10px] border p-2 transition-colors ${
                          checked
                            ? "border-copper/50 bg-copper/10"
                            : "border-hairline bg-app hover:bg-hover"
                        }`}
                      >
                        <input
                          type="checkbox"
                          checked={checked}
                          onChange={() => toggleNewGroupMember(agent.id)}
                          disabled={creatingGroup}
                          className="rounded border-hairline bg-raised accent-[color:var(--color-copper)]"
                        />
                        <AgentAvatar agentId={agent.id} name={agent.name} size="xs" cacheBust={agent.avatarHash} />
                        <div className="min-w-0 flex-1">
                          <div className="truncate text-sm text-ink">{agent.name}</div>
                          <div className="truncate font-mono text-[10px] text-ink-faint">{agent.tool}</div>
                        </div>
                      </label>
                    );
                  })}
                </div>
              </div>

              {createGroupError && (
                <p className="text-xs text-lamp-err">{createGroupError}</p>
              )}
            </div>

            <div className="flex shrink-0 justify-end gap-2 border-t border-hairline px-4 py-3">
              <button
                type="button"
                onClick={() => setShowCreateGroupDialog(false)}
                disabled={creatingGroup}
                className="rounded-[10px] px-3 py-1.5 text-sm text-ink-dim transition-colors hover:text-ink disabled:opacity-50"
              >
                Cancel
              </button>
              <Button type="submit" variant="primary" disabled={creatingGroup || newGroupMemberIds.size < 2}>
                {creatingGroup ? "Creating..." : "Create"}
              </Button>
            </div>
          </form>
        </div>
      )}
    </div>
  );
}

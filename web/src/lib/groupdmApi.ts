import { get, post, del, patch, put } from "./httpClient";
import type { AgentMessageAttachment, ToolUse } from "./agentApi";

export type GroupDMStyle = "efficient" | "expressive";
export type NotifyMode = "realtime" | "digest" | "muted";

/** Physical-setting hint that calibrates how members should speak.
 * "chatroom" (default): closed online chat — text-only, no co-presence.
 * "colocated": same physical space — members can reference shared
 * surroundings, gestures, and use deictic language.
 *
 * Server emits "" (omitempty) for legacy groups created before this
 * feature shipped; treat empty as "chatroom" on read. */
export type GroupDMVenue = "chatroom" | "colocated";
export const DEFAULT_GROUPDM_VENUE: GroupDMVenue = "chatroom";

/** Reserved agentId used for messages posted by the human user (operator). */
export const USER_SENDER_ID = "user";

/** Room kind: "group" = multi-agent room, "dm" = first-class 1:1 room,
 * "thread" = parallel human↔agent thread room (always freshly created). */
export type GroupDMKind = "group" | "dm" | "thread";

/** True when a room is a human↔agent side conversation rather than a room
 * with a roster: kind "thread", or the older kind "dm" shape a thread was
 * created as before "thread" existed.
 *
 * The member-count test is load-bearing. kind "dm" is also what
 * `POST /api/v1/dms` produces for a real 1:1 DM between TWO agents, and
 * classifying that as a thread listed it under Threads on the dashboard and
 * stripped the room affordances (cooldown, hops, style) it actually needs. */
export function isThreadRoom(
  room: { kind?: GroupDMKind; members: unknown[] } | null | undefined,
): boolean {
  if (!room) return false;
  return room.kind === "thread" || (room.kind === "dm" && room.members.length === 1);
}

/** Per-message token usage (agent thread replies only). Mirrors the server's
 * agent.Usage. */
export interface GroupMessageUsage {
  inputTokens: number;
  outputTokens: number;
  cacheReadInputTokens?: number;
  cacheCreationInputTokens?: number;
  costUSD?: number;
}

/** Server default when maxHops is 0/unset. */
export const DEFAULT_MAX_HOPS = 4;
export const MAX_MAX_HOPS = 20;

export interface GroupDMInfo {
  id: string;
  name: string;
  /** Server may omit for legacy rooms; treat undefined as "group". */
  kind?: GroupDMKind;
  members: GroupMember[];
  cooldown: number; // notification cooldown in seconds (0 = default 50s)
  style: GroupDMStyle; // communication style
  /** Physical-setting hint. Server may omit this for legacy groups; UI
   * should fall back to DEFAULT_GROUPDM_VENUE when undefined or empty. */
  venue?: GroupDMVenue;
  /** Agent-to-agent relay hop limit (0 = server default 4, max 20). */
  maxHops?: number;
  /** How kojo agents treat external members' messages (GET single room
   * only). "untrusted" (default) = data, never instructions; "peer" = same
   * as another kojo agent. */
  externalTrust?: ExternalTrust;
  createdAt: string;
  updatedAt: string;
  /** Thread room with a turn in flight (list endpoint only). */
  busy?: boolean;
}

export type ExternalTrust = "untrusted" | "peer";

export interface GroupMember {
  agentId: string;
  agentName: string;
  /** "external" for an external AI agent (agentId "xa_..."); absent for kojo agents. */
  kind?: "external";
  description?: string;
  status?: "online" | "offline" | "busy" | "unknown";
  notifyMode?: NotifyMode;
  digestWindow?: number;
}

export interface GroupMessage {
  id: string;
  agentId: string;
  agentName: string;
  content: string;
  attachments?: AgentMessageAttachment[];
  /** Relay hop count of this message (0 = origin). */
  hop?: number;
  /** Mentioned agent ids; USER_SENDER_ID ("user") = the human operator. */
  mentions?: string[];
  timestamp: string;
  /** Token usage for an agent thread reply (absent otherwise). */
  usage?: GroupMessageUsage;
  /** Extended-thinking text for an agent thread reply (absent otherwise). */
  thinking?: string;
  /** Tool-call trace for an agent thread reply (absent otherwise). */
  toolUses?: ToolUse[];
  /** Model that produced an agent thread reply (absent otherwise). */
  model?: string;
  /** Reasoning effort of an agent thread reply (absent otherwise). */
  effort?: string;
  /** True when the turn ended early (stop/error) — content is the
   * partial output, possibly even an empty string with only thinking or
   * toolUses. */
  interrupted?: boolean;
}

export interface UnreadInfo {
  count: number;
  mentionsUser: boolean;
  hasMore: boolean;
}

/** Live snapshot of a thread room's in-flight turn (GET .../live), polled
 * while the "replying…" indicator is up so thinking / tool calls / partial
 * text can render instead of only a static placeholder. `active: false` means
 * no turn is currently running for the room — the other fields are absent. */
export interface ThreadLive {
  active: boolean;
  status?: string;
  thinking?: string;
  text?: string;
  toolUses?: ToolUse[];
  model?: string;
  effort?: string;
}

/** localStorage key for the last-read message id of a room. */
export const lastReadKey = (roomId: string) => `kojo.groupdm.lastRead.${roomId}`;

export function getLastRead(roomId: string): string | null {
  try {
    return localStorage.getItem(lastReadKey(roomId));
  } catch {
    return null;
  }
}

export function setLastRead(roomId: string, messageId: string): void {
  try {
    localStorage.setItem(lastReadKey(roomId), messageId);
  } catch {
    // storage unavailable (private mode / quota) — badges degrade gracefully
  }
}

/** Drop the marker (e.g. after clearing a room's history, when the
 * referenced message no longer exists). */
export function clearLastRead(roomId: string): void {
  try {
    localStorage.removeItem(lastReadKey(roomId));
  } catch {
    // ignore
  }
}

export const groupdmApi = {
  list: () =>
    get<{ groups: GroupDMInfo[] }>("/api/v1/groupdms").then((r) => r.groups ?? []),

  get: (id: string) => get<GroupDMInfo>(`/api/v1/groupdms/${id}`),

  create: (
    name: string,
    memberIds: string[],
    opts?: {
      style?: GroupDMStyle;
      venue?: GroupDMVenue;
      cooldown?: number;
      notifyMembers?: boolean;
    },
  ) =>
    post<GroupDMInfo>("/api/v1/groupdms", { name, memberIds, ...(opts ?? {}) }),

  rename: (id: string, name: string) =>
    patch<GroupDMInfo>(`/api/v1/groupdms/${id}`, { name }),

  setCooldown: (id: string, cooldown: number) =>
    patch<GroupDMInfo>(`/api/v1/groupdms/${id}`, { cooldown }),

  setStyle: (id: string, style: GroupDMStyle) =>
    patch<GroupDMInfo>(`/api/v1/groupdms/${id}`, { style }),

  setVenue: (id: string, venue: GroupDMVenue) =>
    patch<GroupDMInfo>(`/api/v1/groupdms/${id}`, { venue }),

  setMaxHops: (id: string, maxHops: number) =>
    patch<GroupDMInfo>(`/api/v1/groupdms/${id}`, { maxHops }),

  /** Find-or-create a DM room. Pass {agentId} for a human↔agent DM or
   * {memberIds} for an agent↔agent pair. Server returns the room either way
   * (200 = existing, 201 = created). */
  openDM: (target: { agentId: string } | { memberIds: string[] }) =>
    post<GroupDMInfo>("/api/v1/dms", target),

  /** Create a brand-new parallel thread room for an agent. Always returns a
   * fresh room (201) — no dedup, unlike openDM. */
  createThread: (agentId: string) =>
    post<GroupDMInfo>("/api/v1/threads", { agentId }),

  threadLive: (id: string) => get<ThreadLive>(`/api/v1/groupdms/${id}/live`),

  // stopThread aborts a thread room's in-flight turn. Rejects with an Error
  // whose message starts with "409:" when no turn is in flight — callers
  // should treat that as already-stopped and ignore it.
  stopThread: (id: string) =>
    post<{ stopped: boolean }>(`/api/v1/groupdms/${id}/stop`, {}),

  unread: (id: string, after?: string | null) =>
    get<UnreadInfo>(
      `/api/v1/groupdms/${id}/unread${after ? `?after=${encodeURIComponent(after)}` : ""}`,
    ),

  /** Persist the operator's read cursor server-side (durable across daemon
   * restarts and shared across devices, unlike the localStorage cursor). */
  markRead: (id: string, messageId: string) =>
    post<{ ok: boolean }>(`/api/v1/groupdms/${id}/read`, { messageId }),

  addMember: (id: string, agentId: string, callerAgentId: string) =>
    post<GroupDMInfo>(`/api/v1/groupdms/${id}/members`, { agentId, callerAgentId }),

  setExternalTrust: (id: string, trust: ExternalTrust) =>
    put<unknown>(`/api/v1/groupdms/${id}/external-trust`, { trust }),

  leave: (id: string, agentId: string) =>
    del<{ ok: boolean }>(`/api/v1/groupdms/${id}/members/${agentId}`),

  delete: (id: string, notify = false) =>
    del<{ ok: boolean }>(`/api/v1/groupdms/${id}${notify ? "?notify=true" : ""}`),

  /** Archive a thread room. Alias of delete (thread archive = tombstone, no
   * restore); no member notification. */
  archive: (id: string) => del<{ ok: boolean }>(`/api/v1/groupdms/${id}`),

  clearMessages: (id: string) =>
    del<{ ok: boolean; deleted: number }>(`/api/v1/groupdms/${id}/messages`),

  messages: (id: string, limit = 50, before?: string) => {
    const params = new URLSearchParams({ limit: String(limit) });
    if (before) params.set("before", before);
    return get<{ messages: GroupMessage[]; hasMore: boolean }>(
      `/api/v1/groupdms/${id}/messages?${params}`,
    ).then((r) => ({ messages: r.messages ?? [], hasMore: r.hasMore ?? false }));
  },

  postMessage: (groupId: string, agentId: string, content: string) =>
    post<GroupMessage>(`/api/v1/groupdms/${groupId}/messages`, { agentId, content }),

  postUserMessage: (
    groupId: string,
    content: string,
    attachments?: AgentMessageAttachment[],
  ) =>
    post<GroupMessage>(`/api/v1/groupdms/${groupId}/user-messages`, {
      content,
      ...(attachments && attachments.length > 0 ? { attachments } : {}),
    }),

  // steer injects an operator message into a thread room's currently
  // in-flight reply. Rejects with an Error whose message starts with
  // "409:" when there is no turn in flight, or the backend doesn't
  // support steering — callers should fall back to postUserMessage.
  steer: (groupId: string, content: string) =>
    post<GroupMessage>(`/api/v1/groupdms/${groupId}/steer`, { content }),

  forAgent: (agentId: string) =>
    get<{ groups: GroupDMInfo[] }>(`/api/v1/agents/${agentId}/groups`).then(
      (r) => r.groups ?? [],
    ),
};

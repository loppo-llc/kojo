import { get, post, del, patch } from "./httpClient";
import type { AgentMessageAttachment } from "./agentApi";

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
  createdAt: string;
  updatedAt: string;
}

export interface GroupMember {
  agentId: string;
  agentName: string;
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
}

export interface UnreadInfo {
  count: number;
  mentionsUser: boolean;
  hasMore: boolean;
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

  forAgent: (agentId: string) =>
    get<{ groups: GroupDMInfo[] }>(`/api/v1/agents/${agentId}/groups`).then(
      (r) => r.groups ?? [],
    ),
};

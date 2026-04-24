import { get, post, del, patch } from "./httpClient";

export type GroupDMStyle = "efficient" | "expressive";

/** Reserved agentId used for messages posted by the human user (operator). */
export const USER_SENDER_ID = "user";

export interface GroupDMInfo {
  id: string;
  name: string;
  members: GroupMember[];
  cooldown: number; // notification cooldown in seconds (0 = default 50s)
  style: GroupDMStyle; // communication style
  createdAt: string;
  updatedAt: string;
}

export interface GroupMember {
  agentId: string;
  agentName: string;
}

export interface GroupMessage {
  id: string;
  agentId: string;
  agentName: string;
  content: string;
  timestamp: string;
}

export const groupdmApi = {
  list: () =>
    get<{ groups: GroupDMInfo[] }>("/api/v1/groupdms").then((r) => r.groups ?? []),

  get: (id: string) => get<GroupDMInfo>(`/api/v1/groupdms/${id}`),

  create: (name: string, memberIds: string[]) =>
    post<GroupDMInfo>("/api/v1/groupdms", { name, memberIds }),

  rename: (id: string, name: string) =>
    patch<GroupDMInfo>(`/api/v1/groupdms/${id}`, { name }),

  setCooldown: (id: string, cooldown: number) =>
    patch<GroupDMInfo>(`/api/v1/groupdms/${id}`, { cooldown }),

  setStyle: (id: string, style: GroupDMStyle) =>
    patch<GroupDMInfo>(`/api/v1/groupdms/${id}`, { style }),

  addMember: (id: string, agentId: string, callerAgentId: string) =>
    post<GroupDMInfo>(`/api/v1/groupdms/${id}/members`, { agentId, callerAgentId }),

  leave: (id: string, agentId: string) =>
    del<{ ok: boolean }>(`/api/v1/groupdms/${id}/members/${agentId}`),

  delete: (id: string, notify = false) =>
    del<{ ok: boolean }>(`/api/v1/groupdms/${id}${notify ? "?notify=true" : ""}`),

  messages: (id: string, limit = 50, before?: string) => {
    const params = new URLSearchParams({ limit: String(limit) });
    if (before) params.set("before", before);
    return get<{ messages: GroupMessage[]; hasMore: boolean }>(
      `/api/v1/groupdms/${id}/messages?${params}`,
    ).then((r) => ({ messages: r.messages ?? [], hasMore: r.hasMore ?? false }));
  },

  postMessage: (groupId: string, agentId: string, content: string) =>
    post<GroupMessage>(`/api/v1/groupdms/${groupId}/messages`, { agentId, content }),

  postUserMessage: (groupId: string, content: string) =>
    post<GroupMessage>(`/api/v1/groupdms/${groupId}/user-messages`, { content }),

  forAgent: (agentId: string) =>
    get<{ groups: GroupDMInfo[] }>(`/api/v1/agents/${agentId}/groups`).then(
      (r) => r.groups ?? [],
    ),
};

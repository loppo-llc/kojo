const BASE = "";

export interface AgentInfo {
  id: string;
  name: string;
  persona: string;
  model: string;
  tool: string;
  cronExpr: string;
  createdAt: string;
  updatedAt: string;
  hasAvatar: boolean;
  lastMessage?: {
    content: string;
    role: string;
    timestamp: string;
  };
}

export interface AgentConfig {
  name: string;
  persona: string;
  model?: string;
  tool?: string;
  cronExpr?: string;
}

export interface AgentMessage {
  id: string;
  role: "user" | "assistant" | "system";
  content: string;
  toolUses?: ToolUse[];
  timestamp: string;
  usage?: { inputTokens: number; outputTokens: number };
}

export interface ToolUse {
  name: string;
  input: string;
  output: string;
}

export interface ChatEvent {
  type: "status" | "text" | "tool_use" | "tool_result" | "done" | "error";
  status?: string;
  delta?: string;
  toolName?: string;
  toolInput?: string;
  toolOutput?: string;
  message?: AgentMessage;
  usage?: { inputTokens: number; outputTokens: number };
  errorMessage?: string;
}

async function get<T>(path: string): Promise<T> {
  const res = await fetch(BASE + path);
  if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  return res.json();
}

async function post<T>(path: string, body?: unknown): Promise<T> {
  const res = await fetch(BASE + path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  return res.json();
}

async function del<T>(path: string): Promise<T> {
  const res = await fetch(BASE + path, { method: "DELETE" });
  if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  return res.json();
}

async function patch<T>(path: string, body: unknown): Promise<T> {
  const res = await fetch(BASE + path, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  return res.json();
}

export const agentApi = {
  list: () =>
    get<{ agents: AgentInfo[] }>("/api/v1/agents").then((r) => r.agents ?? []),

  get: (id: string) => get<AgentInfo>(`/api/v1/agents/${id}`),

  create: (cfg: AgentConfig) => post<AgentInfo>("/api/v1/agents", cfg),

  update: (id: string, cfg: Partial<AgentConfig>) =>
    patch<AgentInfo>(`/api/v1/agents/${id}`, cfg),

  delete: (id: string) => del<{ ok: boolean }>(`/api/v1/agents/${id}`),

  avatarUrl: (id: string) => `/api/v1/agents/${id}/avatar`,

  uploadAvatar: async (id: string, file: File) => {
    const form = new FormData();
    form.append("avatar", file);
    const res = await fetch(`${BASE}/api/v1/agents/${id}/avatar`, {
      method: "POST",
      body: form,
    });
    if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
    return res.json() as Promise<{ ok: boolean }>;
  },

  uploadGeneratedAvatar: (id: string, avatarPath: string) =>
    post<{ ok: boolean }>(`/api/v1/agents/${id}/avatar/generated`, { avatarPath }),

  messages: (id: string, limit = 50) =>
    get<{ messages: AgentMessage[] }>(
      `/api/v1/agents/${id}/messages?limit=${limit}`,
    ).then((r) => r.messages ?? []),

  generatePersona: (currentPersona: string, prompt: string) =>
    post<{ persona: string }>("/api/v1/agents/generate-persona", { currentPersona, prompt }),

  generateName: (persona: string, prompt?: string) =>
    post<{ name: string }>("/api/v1/agents/generate-name", { persona, prompt }),

  generateAvatar: (persona: string, name: string, prompt?: string, previousPath?: string) =>
    post<{ avatarPath: string }>("/api/v1/agents/generate-avatar", {
      persona,
      name,
      prompt,
      previousPath,
    }),

  previewAvatarUrl: (path: string) =>
    `${BASE}/api/v1/agents/preview-avatar?path=${encodeURIComponent(path)}`,
};

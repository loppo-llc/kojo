const BASE = "";

export const INTERVAL_PRESETS = [
  { label: "Off", value: 0 },
  { label: "10m", value: 10 },
  { label: "30m", value: 30 },
  { label: "1h", value: 60 },
  { label: "3h", value: 180 },
  { label: "6h", value: 360 },
  { label: "12h", value: 720 },
  { label: "24h", value: 1440 },
] as const;

export interface AgentInfo {
  id: string;
  name: string;
  persona: string;
  model: string;
  tool: string;
  intervalMinutes: number;
  activeStart?: string;
  activeEnd?: string;
  createdAt: string;
  updatedAt: string;
  publicProfile: string;
  publicProfileOverride: boolean;
  hasAvatar: boolean;
  avatarHash?: string;
  notifySources?: NotifySourceConfig[];
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
  intervalMinutes?: number;
  activeStart?: string;
  activeEnd?: string;
}

export interface AgentUpdateParams extends Partial<AgentConfig> {
  publicProfile?: string;
  publicProfileOverride?: boolean;
}

export interface AgentMessageAttachment {
  path: string;
  name: string;
  size: number;
  mime: string;
}

export interface AgentMessage {
  id: string;
  role: "user" | "assistant" | "system";
  content: string;
  thinking?: string;
  toolUses?: ToolUse[];
  attachments?: AgentMessageAttachment[];
  timestamp: string;
  usage?: { inputTokens: number; outputTokens: number };
}

export interface ToolUse {
  name: string;
  input: string;
  output: string;
}

export interface Credential {
  id: string;
  label: string;
  username: string;
  password: string;
  totpSecret?: string;
  totpAlgorithm?: string;
  totpDigits?: number;
  totpPeriod?: number;
  createdAt: string;
  updatedAt: string;
}

export interface NotifySourceConfig {
  id: string;
  type: string;
  enabled: boolean;
  intervalMinutes: number;
  query?: string;
  options?: Record<string, string>;
}

export interface OAuthClientInfo {
  provider: string;
  configured: boolean;
}

export interface NotifySourceType {
  type: string;
  name: string;
  description: string;
  authType: string;
}

export interface OTPEntry {
  label: string;
  issuer: string;
  username: string;
  totpSecret: string;
  algorithm?: string;
  digits?: number;
  period?: number;
}

export interface ChatEvent {
  type: "status" | "text" | "thinking" | "tool_use" | "tool_result" | "done" | "error";
  status?: string;
  delta?: string;
  toolName?: string;
  toolInput?: string;
  toolOutput?: string;
  message?: AgentMessage;
  usage?: { inputTokens: number; outputTokens: number };
  errorMessage?: string;
  startedAt?: string; // RFC3339 timestamp of when processing started
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

async function put<T>(path: string, body: unknown): Promise<T> {
  const res = await fetch(BASE + path, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  return res.json();
}

export const agentApi = {
  list: () =>
    get<{ agents: AgentInfo[] }>("/api/v1/agents").then((r) => r.agents ?? []),

  cronPaused: () =>
    get<{ paused: boolean }>("/api/v1/agents/cron-paused").then((r) => r.paused),

  setCronPaused: (paused: boolean) =>
    put<{ paused: boolean }>("/api/v1/agents/cron-paused", { paused }),

  get: (id: string) => get<AgentInfo>(`/api/v1/agents/${id}`),

  create: (cfg: AgentConfig) => post<AgentInfo>("/api/v1/agents", cfg),

  update: (id: string, cfg: AgentUpdateParams) =>
    patch<AgentInfo>(`/api/v1/agents/${id}`, cfg),

  delete: (id: string) => del<{ ok: boolean }>(`/api/v1/agents/${id}`),

  resetData: (id: string) => post<{ ok: boolean }>(`/api/v1/agents/${id}/reset`),

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

  messages: (id: string, limit = 30, before?: string) => {
    const params = new URLSearchParams({ limit: String(limit) });
    if (before) params.set("before", before);
    return get<{ messages: AgentMessage[]; hasMore: boolean }>(
      `/api/v1/agents/${id}/messages?${params}`,
    ).then((r) => ({ messages: r.messages ?? [], hasMore: r.hasMore ?? false }));
  },

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

  credentials: {
    list: (agentId: string) =>
      get<{ credentials: Credential[] }>(
        `/api/v1/agents/${agentId}/credentials`,
      ).then((r) => r.credentials ?? []),

    add: (
      agentId: string,
      label: string,
      username: string,
      password: string,
      totp?: { secret: string; algorithm?: string; digits?: number; period?: number },
    ) =>
      post<Credential>(`/api/v1/agents/${agentId}/credentials`, {
        label,
        username,
        password,
        ...(totp
          ? {
              totpSecret: totp.secret,
              totpAlgorithm: totp.algorithm || undefined,
              totpDigits: totp.digits || undefined,
              totpPeriod: totp.period || undefined,
            }
          : {}),
      }),

    update: (
      agentId: string,
      credId: string,
      data: Partial<{
        label: string;
        username: string;
        password: string;
        totpSecret: string;
        totpAlgorithm: string;
        totpDigits: number;
        totpPeriod: number;
      }>,
    ) => patch<Credential>(`/api/v1/agents/${agentId}/credentials/${credId}`, data),

    delete: (agentId: string, credId: string) =>
      del<{ ok: boolean }>(`/api/v1/agents/${agentId}/credentials/${credId}`),

    revealPassword: (agentId: string, credId: string) =>
      get<{ password: string }>(
        `/api/v1/agents/${agentId}/credentials/${credId}/password`,
      ).then((r) => r.password),

    getTOTPCode: (agentId: string, credId: string) =>
      get<{ code: string; remaining: number }>(
        `/api/v1/agents/${agentId}/credentials/${credId}/totp`,
      ),

    parseQR: async (agentId: string, file: File) => {
      const form = new FormData();
      form.append("qr", file);
      const res = await fetch(`${BASE}/api/v1/agents/${agentId}/credentials/parse-qr`, {
        method: "POST",
        body: form,
      });
      if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
      return (res.json() as Promise<{ entries: OTPEntry[] }>).then((r) => r.entries ?? []);
    },

    parseOTPURI: (agentId: string, uri: string) =>
      post<{ entries: OTPEntry[] }>(`/api/v1/agents/${agentId}/credentials/parse-uri`, { uri }).then(
        (r) => r.entries ?? [],
      ),
  },

  notifySources: {
    list: (agentId: string) =>
      get<{ sources: NotifySourceConfig[] }>(
        `/api/v1/agents/${agentId}/notify-sources`,
      ).then((r) => r.sources ?? []),

    create: (agentId: string, cfg: { type: string; intervalMinutes?: number; query?: string }) =>
      post<{ source: NotifySourceConfig }>(
        `/api/v1/agents/${agentId}/notify-sources`,
        cfg,
      ).then((r) => r.source),

    update: (agentId: string, sourceId: string, data: Partial<NotifySourceConfig>) =>
      patch<{ source: NotifySourceConfig }>(
        `/api/v1/agents/${agentId}/notify-sources/${sourceId}`,
        data,
      ).then((r) => r.source),

    delete: async (agentId: string, sourceId: string) => {
      const res = await fetch(`${BASE}/api/v1/agents/${agentId}/notify-sources/${sourceId}`, {
        method: "DELETE",
      });
      if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
    },

    startAuth: (agentId: string, sourceId: string) =>
      get<{ authUrl: string }>(
        `/api/v1/agents/${agentId}/notify-sources/${sourceId}/auth`,
      ).then((r) => r.authUrl),
  },

  oauthClients: {
    list: () =>
      get<{ clients: OAuthClientInfo[] }>("/api/v1/oauth-clients").then(
        (r) => r.clients ?? [],
      ),

    set: (provider: string, clientId: string, clientSecret: string) =>
      post<{ ok: boolean }>(`/api/v1/oauth-clients/${provider}`, {
        clientId,
        clientSecret,
      }),

    delete: async (provider: string) => {
      const res = await fetch(`${BASE}/api/v1/oauth-clients/${provider}`, {
        method: "DELETE",
      });
      if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
    },
  },

  notifySourceTypes: () =>
    get<{ types: NotifySourceType[] }>("/api/v1/notify-source-types").then(
      (r) => r.types ?? [],
    ),
};

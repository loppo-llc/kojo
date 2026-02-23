const BASE = "";

export interface SessionInfo {
  id: string;
  tool: string;
  workDir: string;
  args?: string[];
  status: "running" | "exited";
  exitCode?: number;
  yoloMode: boolean;
  createdAt: string;
}

export interface ServerInfo {
  version: string;
  hostname: string;
  homeDir: string;
  tools: Record<string, { available: boolean; path: string }>;
}

export interface DirEntry {
  name: string;
  type: "dir" | "file";
  modTime: string;
}

export interface FileView {
  path: string;
  type: "text" | "image";
  content?: string;
  language?: string;
  mime?: string;
  size: number;
  url?: string;
}

export interface GitStatus {
  branch: string;
  ahead: number;
  behind: number;
  staged: string[];
  modified: string[];
  untracked: string[];
}

export interface GitLogEntry {
  hash: string;
  message: string;
  author: string;
  date: string;
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

export const api = {
  info: () => get<ServerInfo>("/api/v1/info"),
  dirSuggest: (prefix: string) =>
    get<{ dirs: string[] }>(`/api/v1/dirs?prefix=${encodeURIComponent(prefix)}`).then((r) => r.dirs),

  sessions: {
    list: () => get<{ sessions: SessionInfo[] }>("/api/v1/sessions").then((r) => r.sessions),
    get: (id: string) => get<SessionInfo>(`/api/v1/sessions/${id}`),
    create: (body: { tool: string; workDir: string; args?: string[]; yoloMode?: boolean }) =>
      post<SessionInfo>("/api/v1/sessions", body),
    delete: (id: string) => del<{ ok: boolean }>(`/api/v1/sessions/${id}`),
    patch: (id: string, body: { yoloMode?: boolean }) =>
      patch<SessionInfo>(`/api/v1/sessions/${id}`, body),
    restart: (id: string) => post<SessionInfo>(`/api/v1/sessions/${id}/restart`),
  },

  files: {
    list: (path?: string, hidden?: boolean) => {
      const params = new URLSearchParams();
      if (path) params.set("path", path);
      if (hidden) params.set("hidden", "true");
      return get<{ path: string; entries: DirEntry[] }>(`/api/v1/files?${params}`);
    },
    view: (path: string) => get<FileView>(`/api/v1/files/view?path=${encodeURIComponent(path)}`),
  },

  git: {
    status: (workDir: string) =>
      get<GitStatus>(`/api/v1/git/status?workDir=${encodeURIComponent(workDir)}`),
    log: (workDir: string, limit = 20) =>
      get<{ commits: GitLogEntry[] }>(
        `/api/v1/git/log?workDir=${encodeURIComponent(workDir)}&limit=${limit}`,
      ).then((r) => r.commits),
    diff: (workDir: string, ref?: string) => {
      const params = new URLSearchParams({ workDir });
      if (ref) params.set("ref", ref);
      return get<{ diff: string }>(`/api/v1/git/diff?${params}`).then((r) => r.diff);
    },
    exec: (workDir: string, args: string[]) =>
      post<{ exitCode: number; stdout: string; stderr: string }>("/api/v1/git/exec", {
        workDir,
        args,
      }),
  },

  push: {
    vapidKey: () => get<{ publicKey: string }>("/api/v1/push/vapid").then((r) => r.publicKey),
    subscribe: (subscription: PushSubscriptionJSON) =>
      post<{ ok: boolean }>("/api/v1/push/subscribe", subscription),
    unsubscribe: (endpoint: string) =>
      post<{ ok: boolean }>("/api/v1/push/unsubscribe", { endpoint }),
  },

  upload: async (file: File) => {
    const form = new FormData();
    form.append("file", file);
    const res = await fetch(BASE + "/api/v1/upload", { method: "POST", body: form });
    if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
    return res.json() as Promise<{ path: string; name: string; size: number; mime: string }>;
  },
};

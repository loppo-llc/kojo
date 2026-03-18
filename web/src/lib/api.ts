import { get, post, del, patch, upload } from "./httpClient";

export interface SessionInfo {
  id: string;
  tool: string;
  workDir: string;
  args?: string[];
  status: "running" | "exited";
  exitCode?: number;
  yoloMode: boolean;
  internal?: boolean;
  createdAt: string;
  toolSessionId?: string;
  parentId?: string;
  lastOutput?: string; // base64-encoded last terminal output
}

export interface ServerInfo {
  version: string;
  hostname: string;
  homeDir: string;
  tools: Record<string, { available: boolean; path: string }>;
  shellTool: string; // "tmux" on Unix, "shell" on Windows
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

export interface Attachment {
  path: string;
  name: string;
  size: number;
  mime: string;
  modTime: string;
  createdAt: string;
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

export const api = {
  info: () => get<ServerInfo>("/api/v1/info"),
  dirSuggest: (prefix: string) =>
    get<{ dirs: string[] }>(`/api/v1/dirs?prefix=${encodeURIComponent(prefix)}`).then((r) => r.dirs),

  sessions: {
    list: () => get<{ sessions: SessionInfo[] }>("/api/v1/sessions").then((r) => r.sessions),
    get: (id: string) => get<SessionInfo>(`/api/v1/sessions/${id}`),
    create: (body: { tool: string; workDir: string; args?: string[]; yoloMode?: boolean; parentId?: string }) =>
      post<SessionInfo>("/api/v1/sessions", body),
    terminal: (parentId: string) => get<SessionInfo>(`/api/v1/sessions/${parentId}/terminal`),
    delete: (id: string) => del<{ ok: boolean }>(`/api/v1/sessions/${id}`),
    patch: (id: string, body: { yoloMode?: boolean }) =>
      patch<SessionInfo>(`/api/v1/sessions/${id}`, body),
    restart: (id: string) => post<SessionInfo>(`/api/v1/sessions/${id}/restart`),
    tmux: (id: string, body: { action: string }) =>
      post<{ ok: boolean }>(`/api/v1/sessions/${id}/tmux`, body),
    attachments: (id: string) =>
      get<{ attachments: Attachment[] }>(`/api/v1/sessions/${id}/attachments`).then((r) => r.attachments),
    deleteAttachment: (id: string, path: string) =>
      del<{ ok: boolean }>(`/api/v1/sessions/${id}/attachments?path=${encodeURIComponent(path)}`),
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
    log: (workDir: string, limit = 20, skip = 0) =>
      get<{ commits: GitLogEntry[]; hasMore: boolean }>(
        `/api/v1/git/log?workDir=${encodeURIComponent(workDir)}&limit=${limit}&skip=${skip}`,
      ),
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

  upload: (file: File) => {
    const form = new FormData();
    form.append("file", file);
    return upload<{ path: string; name: string; size: number; mime: string }>("/api/v1/upload", form);
  },
};

import { get, post, del, patch, upload } from "./httpClient";
import { appendTokenQuery } from "./auth";

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
  // peer is the deviceID of the host that owns this session. Empty
  // = local. Stamped by the server's create handler so the
  // dashboard can route subsequent calls through the Hub→peer
  // proxy without an extra registry lookup.
  peer?: string;
}

export interface ServerInfo {
  version: string;
  hostname: string;
  homeDir: string;
  tools: Record<string, { available: boolean; path: string }>;
  shellTool: string; // "tmux" on Unix, "shell" on Windows
  agentBackends?: Record<string, boolean>;
}

export interface DirEntry {
  name: string;
  type: "dir" | "file";
  size: number;
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
  absPath?: string; // present on agent-scoped view responses
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

// withPeer appends `?peer=<deviceId>` (or `&peer=<deviceId>` when
// the URL already carries a query string) so the Hub's
// sessionPeerProxyMiddleware forwards the request to the right
// peer. Empty peerId leaves the URL unchanged so local calls stay
// query-clean.
function withPeer(url: string, peerId?: string): string {
  if (!peerId) return url;
  const sep = url.includes("?") ? "&" : "?";
  return `${url}${sep}peer=${encodeURIComponent(peerId)}`;
}

// isThumbSupported reports whether the given filename has an extension
// the server thumb endpoint can decode. The endpoint rejects everything
// else with 415, so the UI should request the raw URL directly for those
// instead of hitting thumb and falling back on error. Keep this in sync
// with internal/thumbnail.IsSupportedExt.
export function isThumbSupported(nameOrPath: string): boolean {
  const m = /\.([^./\\]+)$/.exec(nameOrPath.toLowerCase());
  if (!m) return false;
  switch (m[1]) {
    case "png":
    case "jpg":
    case "jpeg":
    case "gif":
    case "webp":
      return true;
  }
  return false;
}

export const api = {
  info: (peerId?: string) => get<ServerInfo>(withPeer("/api/v1/info", peerId)),
  dirSuggest: (prefix: string, peerId?: string) =>
    get<{ dirs: string[] }>(withPeer(`/api/v1/dirs?prefix=${encodeURIComponent(prefix)}`, peerId)).then((r) => r.dirs),
  customModels: (baseURL: string) =>
    get<{ models: string[] }>(`/api/v1/custom-models?baseURL=${encodeURIComponent(baseURL)}`).then((r) => r.models),

  sessions: {
    list: (peerId?: string) => get<{ sessions: SessionInfo[] }>(withPeer("/api/v1/sessions", peerId)).then((r) => r.sessions),
    // peerId, when present, gets appended as `?peer=<id>` so the
    // Hub's sessionPeerProxyMiddleware forwards the request to the
    // peer that holds the session. Empty / undefined = local.
    get: (id: string, peerId?: string) => get<SessionInfo>(withPeer(`/api/v1/sessions/${id}`, peerId)),
    create: (body: { tool: string; workDir: string; args?: string[]; yoloMode?: boolean; simpleSystemPrompt?: boolean; parentId?: string; peerId?: string }) =>
      post<SessionInfo>("/api/v1/sessions", body),
    terminal: (parentId: string, peerId?: string) =>
      get<SessionInfo>(withPeer(`/api/v1/sessions/${parentId}/terminal`, peerId)),
    delete: (id: string, peerId?: string) =>
      del<{ ok: boolean }>(withPeer(`/api/v1/sessions/${id}`, peerId)),
    patch: (id: string, body: { yoloMode?: boolean }, peerId?: string) =>
      patch<SessionInfo>(withPeer(`/api/v1/sessions/${id}`, peerId), body),
    restart: (id: string, peerId?: string) =>
      post<SessionInfo>(withPeer(`/api/v1/sessions/${id}/restart`, peerId)),
    tmux: (id: string, body: { action: string }, peerId?: string) =>
      post<{ ok: boolean }>(withPeer(`/api/v1/sessions/${id}/tmux`, peerId), body),
    attachments: (id: string, peerId?: string) =>
      get<{ attachments: Attachment[] }>(withPeer(`/api/v1/sessions/${id}/attachments`, peerId)).then((r) => r.attachments),
    deleteAttachment: (id: string, path: string, peerId?: string) =>
      del<{ ok: boolean }>(withPeer(`/api/v1/sessions/${id}/attachments?path=${encodeURIComponent(path)}`, peerId)),
  },

  files: {
    list: (path?: string, hidden?: boolean, peerId?: string) => {
      const params = new URLSearchParams();
      if (path) params.set("path", path);
      if (hidden) params.set("hidden", "true");
      return get<{ path: string; entries: DirEntry[] }>(withPeer(`/api/v1/files?${params}`, peerId));
    },
    view: (path: string, peerId?: string) =>
      get<FileView>(withPeer(`/api/v1/files/view?path=${encodeURIComponent(path)}`, peerId)),
    // rawUrl returns the URL of the raw file with the Owner token
    // appended when one is stored. Use this for `<img src>` /
    // anchor-driven downloads. Fetch-driven callers should prefer
    // rawPath + authHeaders() to keep the token out of URLs / logs.
    rawUrl: (path: string, download = false, peerId?: string) => {
      let base = `/api/v1/files/raw?path=${encodeURIComponent(path)}`;
      if (download) base += "&download=1";
      return appendTokenQuery(withPeer(base, peerId));
    },
    rawPath: (path: string, peerId?: string) =>
      withPeer(`/api/v1/files/raw?path=${encodeURIComponent(path)}`, peerId),
    // thumbUrl returns a low-res JPEG thumbnail URL. Use this for grid
    // tiles / inline message previews — original raws are too heavy for
    // dozens at once over Tailscale. `size` is the longer-edge in pixels;
    // the server clamps to [16, 1024]. `v` is an optional cache-busting
    // string (typically the source's modTime) so an edit produces a
    // fresh URL even though the server caches for a day.
    thumbUrl: (path: string, size = 256, v?: string, peerId?: string) => {
      const q = v ? `&v=${encodeURIComponent(v)}` : "";
      return appendTokenQuery(
        withPeer(`/api/v1/files/thumb?path=${encodeURIComponent(path)}&size=${size}${q}`, peerId),
      );
    },
  },

  // blob serves the native blob store. Used by attachments that
  // originate as kojo:// URIs (agent-generated files, hub-ingested
  // peer pushes). Pass the URI verbatim; the helper parses scope +
  // path and constructs `/api/v1/blob/{scope}/{path}` with the
  // Owner token appended for `<img src>` / anchor download.
  //
  // Returns `null` if the input is not a kojo:// URI so the caller
  // can fall back to the files-raw URL helper for legacy local-path
  // attachments.
  blob: {
    // urlFromKojoURI converts a kojo://<scope>/<path> URI into the
    // hub-side download URL (/api/v1/blob/<scope>/<path>?token=...).
    // Returns null for non-kojo inputs so callers can fall back to
    // the files-raw helper for legacy filesystem-path attachments.
    //
    // The blob handler has no ?download=1 switch — use the anchor's
    // `download` attribute on the link element to force save-as
    // instead of inline display.
    urlFromKojoURI: (uri: string): string | null => {
      if (!uri.startsWith("kojo://")) return null;
      const rest = uri.slice("kojo://".length);
      const slash = rest.indexOf("/");
      if (slash <= 0) return null;
      const scope = rest.slice(0, slash);
      // Path segments are already percent-encoded inside the URI
      // (blob.BuildURI emits the canonical form). Decoding then
      // re-encoding each segment normalises any double encoding
      // (`%2520` → `%20`) that an upstream serializer might have
      // introduced.
      const pathSegs = rest
        .slice(slash + 1)
        .split("/")
        .map((s) => encodeURIComponent(decodeURIComponent(s)));
      const url = `/api/v1/blob/${encodeURIComponent(scope)}/${pathSegs.join("/")}`;
      return appendTokenQuery(url);
    },
    // thumbFromKojoURI is like urlFromKojoURI but appends &thumb=<size>
    // so the blob handler returns a cached JPEG thumbnail instead of
    // the full body. Returns null for non-kojo inputs.
    thumbFromKojoURI: (uri: string, size: number): string | null => {
      if (!uri.startsWith("kojo://")) return null;
      const rest = uri.slice("kojo://".length);
      const slash = rest.indexOf("/");
      if (slash <= 0) return null;
      const scope = rest.slice(0, slash);
      const pathSegs = rest
        .slice(slash + 1)
        .split("/")
        .map((s) => encodeURIComponent(decodeURIComponent(s)));
      const url = `/api/v1/blob/${encodeURIComponent(scope)}/${pathSegs.join("/")}?thumb=${size}`;
      return appendTokenQuery(url);
    },
  },

  git: {
    status: (workDir: string, peerId?: string) =>
      get<GitStatus>(withPeer(`/api/v1/git/status?workDir=${encodeURIComponent(workDir)}`, peerId)),
    log: (workDir: string, limit = 20, skip = 0, peerId?: string) =>
      get<{ commits: GitLogEntry[]; hasMore: boolean }>(
        withPeer(`/api/v1/git/log?workDir=${encodeURIComponent(workDir)}&limit=${limit}&skip=${skip}`, peerId),
      ),
    diff: (workDir: string, ref?: string, peerId?: string) => {
      const params = new URLSearchParams({ workDir });
      if (ref) params.set("ref", ref);
      return get<{ diff: string }>(withPeer(`/api/v1/git/diff?${params}`, peerId)).then((r) => r.diff);
    },
    exec: (workDir: string, args: string[], peerId?: string) =>
      post<{ exitCode: number; stdout: string; stderr: string }>(withPeer("/api/v1/git/exec", peerId), {
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

  upload: (file: File, peerId?: string) => {
    const form = new FormData();
    form.append("file", file);
    return upload<{ path: string; name: string; size: number; mime: string }>(
      withPeer("/api/v1/upload", peerId),
      form,
    );
  },
};

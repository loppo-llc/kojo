import {
  get,
  post,
  del,
  delWithIfMatch,
  patch,
  put,
  upload,
  getWithEtag,
  patchWithIfMatch,
  postWithIfMatch,
} from "./httpClient";
import { appendTokenQuery } from "./auth";
import type { DirEntry, FileView } from "./api";

// SCHEDULE_PRESETS surface the most common cron cadences as one-click chips.
// The cron field uses an "@preset:N" sentinel that the backend expands at
// Save time into a real 5-field expression with a per-agent offset baked in
// (see internal/agent/cron_expr.go). Sending a literal "0 */3 * * *" instead
// would have every agent fire at exactly :00 — the original per-agent
// stagger from the IntervalMinutes era would be lost.
//
// "Daily 09:00" is intentionally a literal expression: a fixed wall-clock
// time is what the user is asking for, so spreading it across agents would
// defeat the point. Off is the empty string (= scheduling disabled).
export const SCHEDULE_PRESETS = [
  { label: "Off", cron: "" },
  { label: "5m", cron: "@preset:5" },
  { label: "10m", cron: "@preset:10" },
  { label: "30m", cron: "@preset:30" },
  { label: "1h", cron: "@preset:60" },
  { label: "3h", cron: "@preset:180" },
  { label: "6h", cron: "@preset:360" },
  { label: "12h", cron: "@preset:720" },
  { label: "Daily 09:00", cron: "0 9 * * *" },
] as const;

export const TIMEOUT_PRESETS = [
  { label: "5m", value: 5 },
  { label: "10m", value: 10 },
  { label: "15m", value: 15 },
  { label: "20m", value: 20 },
  { label: "30m", value: 30 },
  { label: "45m", value: 45 },
  { label: "1h", value: 60 },
] as const;

// RESUME_IDLE_PRESETS: per-agent override for the claude-only idle window
// that protects an over-token-threshold session from being reset out from
// under an active interactive chat. 0 = default (5m, matching Anthropic's
// prompt cache TTL). Smaller = reset more aggressively (fits high-frequency
// agents); larger = keep context across longer pauses.
export const RESUME_IDLE_PRESETS = [
  { label: "default (5m)", value: 0 },
  { label: "1m", value: 1 },
  { label: "3m", value: 3 },
  { label: "5m", value: 5 },
  { label: "10m", value: 10 },
  { label: "15m", value: 15 },
  { label: "30m", value: 30 },
  { label: "1h", value: 60 },
] as const;

export interface AgentInfo {
  id: string;
  name: string;
  persona: string;
  model: string;
  effort: string;
  tool: string;
  customBaseURL?: string;
  workDir: string;
  cronExpr?: string;
  timeoutMinutes: number;
  resumeIdleMinutes?: number;
  silentStart?: string;
  silentEnd?: string;
  notifyDuringSilent?: boolean;
  cronMessage?: string;
  // RFC3339 timestamp of the next scheduled cron run, accounting for silent
  // hours. Empty/absent if cron is disabled, the agent is archived, or no
  // schedule is registered. Server-derived; never sent on update.
  //
  // NOTE: this is the agent's configured next-tick regardless of the
  // global cron-paused toggle — see `cronPausedGlobal` for the live
  // pause indicator. Hiding the time when paused made Agent Settings
  // show "—" on every agent and read as a missing-value bug.
  nextCronAt?: string;
  // True when the Dashboard's global cron toggle is in the paused
  // position. Surfaced on every agent record so Settings can render
  // "(paused)" next to nextCronAt without an extra round-trip to
  // /api/v1/agents/cron-paused. Server-derived; never sent on update.
  cronPausedGlobal?: boolean;
  createdAt: string;
  updatedAt: string;
  publicProfile: string;
  publicProfileOverride: boolean;
  hasAvatar: boolean;
  avatarHash?: string;
  allowedTools?: string[];
  allowProtectedPaths?: string[];
  thinkingMode?: string;
  notifySources?: NotifySourceConfig[];
  lastMessage?: {
    content: string;
    role: string;
    timestamp: string;
  };
  // Archived agents are dormant: runtime activity stopped, hidden from
  // the main list, but all data retained on disk for restore.
  archived?: boolean;
  archivedAt?: string;
  // Privileged grants the agent the ability to delete/reset other
  // agents (NOT to fork or read their full record). Owner-only mutation
  // via POST /api/v1/agents/{id}/privilege.
  privileged?: boolean;
  // Strong HTTP entity tag of the v1 store row for this agent. Carry
  // alongside form state so PATCH /agents/{id} can send it as If-Match;
  // a server-side mismatch surfaces as PreconditionFailedError. Empty
  // when the row has not yet hit the v1 store (legacy paths, brand-new
  // create that has not flushed). Server-populated; do not send on
  // update — use the dedicated `expectedEtag` parameter instead.
  etag?: string;
  tts?: TTSConfig;
}

// TTSConfig mirrors internal/agent.TTSConfig in the Go backend.
// Empty model/voice/stylePrompt are interpreted as "use default" at
// synthesize time.
export interface TTSConfig {
  enabled: boolean;
  model?: string;
  voice?: string;
  stylePrompt?: string;
}

export interface AgentConfig {
  name: string;
  persona: string;
  model?: string;
  effort?: string;
  tool?: string;
  customBaseURL?: string;
  thinkingMode?: string;
  workDir?: string;
  cronExpr?: string;
  timeoutMinutes?: number;
  resumeIdleMinutes?: number;
  silentStart?: string;
  silentEnd?: string;
  notifyDuringSilent?: boolean;
  cronMessage?: string;
}

export interface AgentUpdateParams extends Partial<AgentConfig> {
  publicProfile?: string;
  publicProfileOverride?: boolean;
  allowedTools?: string[];
  allowProtectedPaths?: string[];
  thinkingMode?: string;
  tts?: TTSConfig | null;
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
  // Strong HTTP entity tag of the row backing this message; passed to
  // PATCH /messages/{msgId} as If-Match for optimistic concurrency.
  // Optional because legacy / transitional rows may not have one.
  etag?: string;
}

export interface ToolUse {
  id?: string;
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
  type: "status" | "text" | "thinking" | "tool_use" | "tool_result" | "done" | "error" | "message";
  status?: string;
  delta?: string;
  toolUseId?: string;
  toolName?: string;
  toolInput?: string;
  toolOutput?: string;
  message?: AgentMessage;
  usage?: { inputTokens: number; outputTokens: number };
  errorMessage?: string;
  startedAt?: string; // RFC3339 timestamp of when processing started
}

export interface AgentTask {
  id: string;
  title: string;
  status: "open" | "done";
  createdAt: string;
  updatedAt: string;
}

export interface SlackBotStatus {
  enabled: boolean;
  threadReplies: boolean;
  respondDM: boolean;
  respondMention: boolean;
  respondThread: boolean;
  hasAppToken: boolean;
  hasBotToken: boolean;
  connected: boolean;
}

// TruncateMemoryResult is the response of POST /memory/truncate. Counts are
// best-effort — entries with malformed timestamps or unparseable JSON are
// kept verbatim and not counted, matching the server-side stance of "never
// lose a record just because we couldn't parse it".
export interface TruncateMemoryResult {
  since: string;
  messagesRemoved: number;
  claudeSessionEntriesRemoved: number;
  claudeSessionFilesRemoved: number;
  diaryFilesRemoved: number;
  diaryEntriesRemoved: number;
}

export interface SlackBotSetRequest {
  enabled: boolean;
  appToken?: string;
  botToken?: string;
  threadReplies?: boolean;
  respondDM?: boolean;
  respondMention?: boolean;
  respondThread?: boolean;
}

export const agentApi = {
  // list returns active agents only (archived are excluded by the server).
  list: () =>
    get<{ agents: AgentInfo[] }>("/api/v1/agents").then((r) => r.agents ?? []),

  // listArchived returns archived agents only — used by the Archived
  // section in global Settings.
  listArchived: () =>
    get<{ agents: AgentInfo[] }>("/api/v1/agents?archived=true").then(
      (r) => r.agents ?? [],
    ),

  cronPaused: () =>
    get<{ paused: boolean }>("/api/v1/agents/cron-paused").then((r) => r.paused),

  setCronPaused: (paused: boolean) =>
    put<{ paused: boolean }>("/api/v1/agents/cron-paused", { paused }),

  // GET /api/v1/agents/{id}. The server emits the row's strong ETag
  // both in the HTTP header and in the JSON body's `etag` field. We
  // prefer the body (it's threaded through React state alongside
  // every other field, so a stale form snapshot keeps its original
  // etag rather than picking up a newer one from a shared cache).
  // Fall back to the header for endpoints that have not yet been
  // taught to embed `etag` in the body.
  get: async (id: string): Promise<AgentInfo> => {
    const { value, etag } = await getWithEtag<AgentInfo>(`/api/v1/agents/${id}`);
    if (!value.etag && etag) value.etag = etag;
    return value;
  },

  files: {
    list: (agentId: string, relPath: string, hidden?: boolean) => {
      const params = new URLSearchParams();
      if (relPath) params.set("path", relPath);
      if (hidden) params.set("hidden", "true");
      const qs = params.toString();
      return get<{ path: string; absPath: string; entries: DirEntry[] }>(
        `/api/v1/agents/${agentId}/files${qs ? "?" + qs : ""}`,
      );
    },
    view: (agentId: string, relPath: string) =>
      get<FileView>(
        `/api/v1/agents/${agentId}/files/view?path=${encodeURIComponent(relPath)}`,
      ),
    rawUrl: (agentId: string, relPath: string, download = false) => {
      const base = `/api/v1/agents/${agentId}/files/raw?path=${encodeURIComponent(relPath)}`;
      return appendTokenQuery(download ? `${base}&download=1` : base);
    },
  },

  create: (cfg: AgentConfig) => post<AgentInfo>("/api/v1/agents", cfg),

  // PATCH the agent. expectedEtag is the etag the caller's form
  // snapshot was loaded with — typically taken from the AgentInfo
  // that the matching GET returned. Sending it as If-Match lets the
  // server reject stale writes with 412, surfaced as
  // PreconditionFailedError; the caller should refetch and re-apply
  // the user's edit on the fresh row. Omitting expectedEtag falls
  // back to an unconditional PATCH (legacy callers, test harnesses).
  //
  // The Web UI deliberately threads the etag through React state
  // rather than a global cache: a shared cache would let two
  // simultaneously-open forms accidentally pass each other's writes
  // (the second form's Save would pick up the first's post-write
  // etag and succeed despite holding stale data).
  update: async (
    id: string,
    cfg: AgentUpdateParams,
    expectedEtag?: string,
  ): Promise<AgentInfo> => {
    const path = `/api/v1/agents/${id}`;
    const { value, etag } = await patchWithIfMatch<AgentInfo>(path, cfg, expectedEtag);
    // Prefer the etag baked into the response body (set by
    // buildAgentResponse from the same store read that produced the
    // record); fall back to the header.
    if (!value.etag && etag) value.etag = etag;
    return value;
  },

  delete: (id: string) => del<{ ok: boolean }>(`/api/v1/agents/${id}`),

  // archive: keeps all on-disk data but stops runtime activity. Reversible
  // via unarchive. Hidden from the main list; surfaced in global Settings.
  archive: (id: string) =>
    del<{ ok: boolean }>(`/api/v1/agents/${id}?archive=true`),

  unarchive: (id: string) =>
    post<AgentInfo>(`/api/v1/agents/${id}/unarchive`),

  resetData: (id: string) => post<{ ok: boolean }>(`/api/v1/agents/${id}/reset`),

  resetSession: (id: string) => post<{ ok: boolean }>(`/api/v1/agents/${id}/reset-session`),

  // truncateMemory drops everything recorded at or after the given threshold.
  // Two ways to specify it (matches backend surface):
  //   - { since: RFC3339 }            — absolute datetime
  //   - { fromMessageId: "m_..." }    — use the matched message's timestamp
  // Same auth gate as resetData; same 409 on busy / racing reset; 409 also
  // returned when fromMessageId's pivot gets mutated underneath the call.
  truncateMemory: (
    id: string,
    params: { since: string } | { fromMessageId: string },
  ) => post<TruncateMemoryResult>(`/api/v1/agents/${id}/memory/truncate`, params),

  checkin: (id: string) => post<{ ok: boolean }>(`/api/v1/agents/${id}/checkin`),

  fork: (id: string, params: { name: string; includeTranscript: boolean }) =>
    post<AgentInfo>(`/api/v1/agents/${id}/fork`, params),

  setPrivileged: (id: string, privileged: boolean) =>
    post<{ id: string; privileged: boolean }>(
      `/api/v1/agents/${id}/privilege`,
      { privileged },
    ),

  tasks: {
    list: (agentId: string) =>
      get<{ tasks: AgentTask[] }>(`/api/v1/agents/${agentId}/tasks`).then(
        (r) => r.tasks ?? [],
      ),
    create: (agentId: string, title: string) =>
      post<AgentTask>(`/api/v1/agents/${agentId}/tasks`, { title }),
    update: (agentId: string, taskId: string, data: { title?: string; status?: string }) =>
      patch<AgentTask>(`/api/v1/agents/${agentId}/tasks/${taskId}`, data),
    delete: (agentId: string, taskId: string) =>
      del<{ ok: boolean }>(`/api/v1/agents/${agentId}/tasks/${taskId}`),
  },

  // Avatar / preview / files URLs go straight into <img src> so the
  // token must travel as a query param — Authorization headers can't
  // be set on element-driven fetches.
  avatarUrl: (id: string) => appendTokenQuery(`/api/v1/agents/${id}/avatar`),

  uploadAvatar: (id: string, file: File) => {
    const form = new FormData();
    form.append("avatar", file);
    return upload<{ ok: boolean }>(`/api/v1/agents/${id}/avatar`, form);
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

  // PATCH a single message. The `expectedEtag` is the etag the caller
  // believes is current — typically the value embedded in the
  // AgentMessage object loaded from GET /messages. The server's
  // optimistic-concurrency layer rejects stale writes with 412
  // (PreconditionFailedError); callers should refetch the transcript
  // and re-apply the user's edit on the fresh row rather than
  // blindly retry. expectedEtag may be omitted for backwards-compat
  // (e.g. test harnesses), in which case the write goes through with
  // no precondition — but the regular Web UI should always pass it.
  updateMessage: async (
    agentId: string,
    msgId: string,
    content: string,
    expectedEtag?: string,
  ): Promise<AgentMessage> => {
    const path = `/api/v1/agents/${agentId}/messages/${msgId}`;
    const { value, etag } = await patchWithIfMatch<AgentMessage>(
      path,
      { content },
      expectedEtag,
    );
    // Prefer the body's etag — recordToMessage populates it from the
    // same store.UpdateMessage TX result, so it is guaranteed to
    // describe THIS PATCH's row state. The response header etag,
    // while set by the same handler, could theoretically diverge
    // if a future refactor reads the row a second time. Body wins;
    // header is a last-resort fallback for endpoints that don't
    // embed etag in the body yet.
    if (!value.etag && etag) value.etag = etag;
    return value;
  },

  // expectedEtag enables optimistic locking — pass the etag the UI was
  // looking at when the user clicked delete. Two stale-state shapes can
  // come back; both want a refetch:
  //   - 412 (PreconditionFailedError): the row was edited under us and
  //     its etag has advanced.
  //   - 404 with conditional DELETE: the row already vanished
  //     (SoftDeleteMessage maps non-empty If-Match against a missing
  //     row to ErrNotFound rather than silent success, so a stale
  //     client doesn't think its delete landed). Surfaces as a generic
  //     Error with `404:` prefix; AgentChat.onDelete handles both.
  // Omitting expectedEtag sends an unconditional DELETE (legacy
  // behaviour, still supported).
  deleteMessage: (agentId: string, msgId: string, expectedEtag?: string) =>
    expectedEtag
      ? delWithIfMatch<{ ok: boolean }>(
          `/api/v1/agents/${agentId}/messages/${msgId}`,
          expectedEtag,
        ).then((r) => r.value)
      : del<{ ok: boolean }>(`/api/v1/agents/${agentId}/messages/${msgId}`),

  // expectedEtag preconditions on the *clicked* message's etag (the row
  // the user is regenerating from). 412 → PreconditionFailedError; the
  // UI should refetch and let the user re-confirm rather than auto-retry,
  // because regenerate truncates everything after the click.
  regenerateMessage: (agentId: string, msgId: string, expectedEtag?: string) =>
    expectedEtag
      ? postWithIfMatch<{ ok: boolean }>(
          `/api/v1/agents/${agentId}/messages/${msgId}/regenerate`,
          undefined,
          expectedEtag,
        ).then((r) => r.value)
      : post<{ ok: boolean }>(`/api/v1/agents/${agentId}/messages/${msgId}/regenerate`),

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
    appendTokenQuery(`/api/v1/agents/preview-avatar?path=${encodeURIComponent(path)}`),

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

    parseQR: (agentId: string, file: File) => {
      const form = new FormData();
      form.append("qr", file);
      return upload<{ entries: OTPEntry[] }>(
        `/api/v1/agents/${agentId}/credentials/parse-qr`,
        form,
      ).then((r) => r.entries ?? []);
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

    // create/update/delete accept an optional `agentEtag` (the parent
    // agent's etag) and return the new agent etag from the response
    // ETag header. Callers can chain mutations without an extra GET as
    // long as nothing else races them. 412 → PreconditionFailedError;
    // omit `agentEtag` for unconditional (legacy) writes.
    create: async (
      agentId: string,
      cfg: { type: string; intervalMinutes?: number; query?: string },
      agentEtag?: string,
    ): Promise<{ source: NotifySourceConfig; agentEtag: string | null }> => {
      if (agentEtag) {
        const r = await postWithIfMatch<{ source: NotifySourceConfig }>(
          `/api/v1/agents/${agentId}/notify-sources`,
          cfg,
          agentEtag,
        );
        return { source: r.value.source, agentEtag: r.etag };
      }
      const r = await post<{ source: NotifySourceConfig }>(
        `/api/v1/agents/${agentId}/notify-sources`,
        cfg,
      );
      return { source: r.source, agentEtag: null };
    },

    update: async (
      agentId: string,
      sourceId: string,
      data: Partial<NotifySourceConfig>,
      agentEtag?: string,
    ): Promise<{ source: NotifySourceConfig; agentEtag: string | null }> => {
      if (agentEtag) {
        const r = await patchWithIfMatch<{ source: NotifySourceConfig }>(
          `/api/v1/agents/${agentId}/notify-sources/${sourceId}`,
          data,
          agentEtag,
        );
        return { source: r.value.source, agentEtag: r.etag };
      }
      const r = await patch<{ source: NotifySourceConfig }>(
        `/api/v1/agents/${agentId}/notify-sources/${sourceId}`,
        data,
      );
      return { source: r.source, agentEtag: null };
    },

    delete: async (
      agentId: string,
      sourceId: string,
      agentEtag?: string,
    ): Promise<{ agentEtag: string | null }> => {
      if (agentEtag) {
        const r = await delWithIfMatch<unknown>(
          `/api/v1/agents/${agentId}/notify-sources/${sourceId}`,
          agentEtag,
        );
        return { agentEtag: r.etag };
      }
      await del<unknown>(`/api/v1/agents/${agentId}/notify-sources/${sourceId}`);
      return { agentEtag: null };
    },

    // Returns both authUrl AND the OAuth `state` nonce minted for
    // this popup. The editor tracks state so a same-source double-
    // click race (popup A still in flight when popup B opens) can
    // discriminate which popup's postMessage is current. Without
    // state, both popups would carry the same sourceId and the
    // older one's late callback could overwrite the newer's banner.
    startAuth: (agentId: string, sourceId: string) =>
      get<{ authUrl: string; state: string }>(
        `/api/v1/agents/${agentId}/notify-sources/${sourceId}/auth`,
      ),
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

    delete: (provider: string) =>
      del<unknown>(`/api/v1/oauth-clients/${provider}`),
  },

  apiKeys: {
    get: (provider: string) =>
      get<{ provider: string; configured: boolean; hasFallback?: boolean; embeddingModel?: string }>(`/api/v1/api-keys/${provider}`),

    set: (provider: string, apiKey: string) =>
      put<{ ok: boolean }>(`/api/v1/api-keys/${provider}`, { apiKey }),

    delete: (provider: string) =>
      del<unknown>(`/api/v1/api-keys/${provider}`),
  },

  embeddingModel: {
    set: (model: string) =>
      put<{ ok: boolean; model: string; embeddingsCleared: boolean }>(`/api/v1/embedding-model`, { model }),
    list: () =>
      get<{ models: string[] }>(`/api/v1/embedding-models`).then((r) => r.models ?? []),
  },

  notifySourceTypes: () =>
    get<{ types: NotifySourceType[] }>("/api/v1/notify-source-types").then(
      (r) => r.types ?? [],
    ),

  slackBot: {
    get: (agentId: string) =>
      get<SlackBotStatus>(`/api/v1/agents/${agentId}/slackbot`),

    set: (agentId: string, cfg: SlackBotSetRequest) =>
      put<{ ok: boolean }>(`/api/v1/agents/${agentId}/slackbot`, cfg),

    delete: (agentId: string) =>
      del<{ ok: boolean }>(`/api/v1/agents/${agentId}/slackbot`),

    test: (agentId: string, tokens?: { appToken?: string; botToken?: string }) =>
      post<{ ok: boolean; team: string; botUser: string }>(
        `/api/v1/agents/${agentId}/slackbot/test`,
        tokens,
      ),
  },
};

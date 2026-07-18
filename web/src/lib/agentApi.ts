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
  putWithIfMatch,
} from "./httpClient";
import { appendTokenQuery } from "./auth";
import type { DirEntry, FileView } from "./api";

// INTERVAL_*_OPTIONS drive the simplified "every N unit" schedule editor.
// Minute values are divisors of 60 and hour values divisors of 24, so the
// cadence is always exactly even. Minutes/hours are sent as the "@preset:N"
// sentinel that the backend expands at Save time into a real 5-field
// expression with a per-agent offset baked in (see internal/agent/
// cron_expr.go). Sending a literal "0 */3 * * *" instead would have every
// agent fire at exactly :00. Days are emitted client-side as a literal
// "M H */n * *" with a user-chosen wall-clock time — a fixed time is what
// the user is asking for there, so spreading it across agents would defeat
// the point.
export const INTERVAL_MINUTE_OPTIONS = [5, 10, 15, 20, 30] as const;
export const INTERVAL_HOUR_OPTIONS = [1, 2, 3, 4, 6, 8, 12] as const;
export const INTERVAL_DAY_OPTIONS = [1, 2, 3, 4, 5, 6, 7] as const;

// MAX_SCHEDULE_MINUTES caps the free-form minute inputs (timeoutMinutes,
// resumeIdleMinutes) at one week. Mirrors backend maxScheduleMinutes.
export const MAX_SCHEDULE_MINUTES = 7 * 24 * 60;

// DEFAULT_TIMEOUT_MINUTES mirrors the backend's cronTimeout — what the
// legacy timeoutMinutes=0 sentinel resolves to at runtime. The editor
// renders 0 as this value. (The resume-window default lives only in the
// "sched.resumeDefault" i18n label; the editor keeps 0 = server default.)
export const DEFAULT_TIMEOUT_MINUTES = 10;

// TransferSkip is one session file the §3.7 device-switch transfer
// left behind (see AgentInfo.lastTransferSkips).
export interface TransferSkip {
  path: string;
  reason: string;
  sizeBytes?: number;
}

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
  lastMessage?: {
    content: string;
    role: string;
    timestamp: string;
  };
  // Epoch-millis timestamp of the most recent message. Millisecond
  // precision (unlike lastMessage.timestamp's seconds-resolution RFC3339
  // string) so the dashboard can order agents by most-recent activity
  // without same-second ties reshuffling on reload. Absent/0 = no messages.
  lastMessageAt?: number;
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
  // HolderPeer is set when the agent's runtime currently lives on a remote
  // peer (e.g. via §3.7 device-switch). Empty / absent means this server
  // owns the runtime. The dashboard surfaces this as a "転移中" badge and
  // hides the (necessarily stale) lastMessage preview.
  holderPeer?: string;
  // holderPeerName is the human-friendly label resolved server-side
  // from peer_registry. Falls back to holderPeer (deviceID) when the
  // registry lookup miss races a decommission.
  holderPeerName?: string;
  // holderPeerStatus mirrors peer_registry.status for holderPeer. Used
  // by AgentChat to disable send + show an "offline" banner when the
  // §3.7 device-switch target has gone offline. Empty when holderPeer
  // is empty.
  holderPeerStatus?: "online" | "offline" | "degraded";
  // lastTransferSkips records the session files the most recent
  // inbound §3.7 device-switch transfer skipped (oversized session
  // JSONL, unreadable codex ref, …). Stamped server-side into the
  // agent settings by the sync handler; cleared on a clean transfer.
  // The dashboard shows a "skipped during transfer" notice from it.
  lastTransferSkips?: TransferSkip[];
  // isSwitching is true while a §3.7 device-switch is mid-flight on
  // this peer (between SetSwitching(true) and (false)). Surfaced by
  // the server so the UI can disable mutating controls (credentials
  // add/edit/delete, etc.) that would 409 with agent_busy and show a
  // banner instead.
  isSwitching?: boolean;
  // disabledInjections lists context-injection keys turned OFF for this
  // agent (see CONTEXT_INJECTION_KEYS below). Absent/empty means every
  // injection is enabled — the common case, so the server omits the
  // field entirely rather than sending an empty array on most agents.
  disabledInjections?: string[];
  // autoEffort gates the per-turn dynamic effort classifier. Absent =
  // enabled (the feature is opt-out); the configured effort acts as the
  // ceiling/fallback while enabled. claude / grok tools only.
  autoEffort?: boolean;
  // busy is a runtime-only flag: true while the agent has an in-flight
  // interactive/cron chat (server-side Manager.IsBusy). The dashboard
  // folds it into the "N running" figure so a chatting agent counts even
  // when it has no terminal session. Server-derived; never sent on update.
  busy?: boolean;
  // awaitingAnswer is a runtime-only flag: true while the agent's running
  // turn has an unanswered AskUserQuestion prompt outstanding (server-side
  // Manager.HasPendingQuestion). The dashboard highlights the agent row so
  // a turn blocked on human input is obvious at a glance. Server-derived;
  // never sent on update.
  awaitingAnswer?: boolean;
}

// TURN_ERROR_PREFIX is the marker the server stamps onto the system
// message a failed turn leaves in the transcript (manager.go:
// newSystemMessage("⚠️ Error: " + …)). agent_ws.go relies on the same
// prefix to reconstruct error events on WS resume, so it is a stable
// wire convention, and truncatePreview keeps the first 100 chars so the
// prefix always survives into lastMessage.content.
const TURN_ERROR_PREFIX = "⚠️ Error: ";

// isTurnErrorPreview reports whether an agent's most recent transcript
// message is a turn-failure marker — i.e. the last thing that happened
// to this agent was an error. Used by the dashboard to badge failed
// agents in the list.
export function isTurnErrorPreview(m?: { role: string; content: string }): boolean {
  return !!m && m.role === "system" && m.content.startsWith(TURN_ERROR_PREFIX);
}

// CONTEXT_INJECTION_KEYS mirrors the server-side allowlist for
// disabledInjections. Unknown keys are rejected by the server with 400,
// so this list must stay in sync with the backend's validation.
export const CONTEXT_INJECTION_KEYS = [
  "user_context",
  "memory_md",
  "credentials",
  "groupdm",
  "todo_api",
  "attachments",
  "status",
  "diary_notes",
  "memory_search",
  "recent_conversation",
  "persona_anchor",
] as const;

export type ContextInjectionKey = (typeof CONTEXT_INJECTION_KEYS)[number];

// TTSConfig mirrors internal/agent.TTSConfig in the Go backend.
// Empty model/voice/stylePrompt are interpreted as "use default" at
// synthesize time.
export interface TTSConfig {
  enabled: boolean;
  provider?: "gemini" | "grok";
  model?: string;
  voice?: string;
  stylePrompt?: string;
}

export interface AgentConfig {
  name: string;
  persona: string;
  // Task description from the task-first create flow. Seeded into the
  // new agent's MEMORY.md as a "## Mission" section server-side.
  mission?: string;
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
  disabledInjections?: string[];
  autoEffort?: boolean;
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
  usage?: {
    inputTokens: number;
    outputTokens: number;
    cacheReadInputTokens?: number;
    cacheCreationInputTokens?: number;
    costUSD?: number;
  };
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
  // Narrative text bubble from a subagent (Task tool) turn. Only ever
  // set on entries that live inside `children` (name/input are empty
  // in that case).
  text?: string;
  // Tool calls (and text bubbles) emitted by a subagent spawned via
  // this Task tool call. Populated one level deep even for nested
  // sub-subagents (see backend_claude.go's subagentOwner flattening).
  children?: ToolUse[];
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

export interface OTPEntry {
  label: string;
  issuer: string;
  username: string;
  totpSecret: string;
  algorithm?: string;
  digits?: number;
  period?: number;
}

// UserQuestionOption is one selectable answer for an AskUserQuestion prompt.
export interface UserQuestionOption {
  label: string;
  description?: string;
}

// UserQuestion is one question in an AskUserQuestion control_request.
export interface UserQuestion {
  question: string;
  header?: string;
  options?: UserQuestionOption[];
  multiSelect?: boolean;
}

// RateLimitInfo mirrors the Go agent.RateLimitInfo (the claude CLI's
// rate_limit_event payload).
export interface RateLimitInfo {
  status: string; // "allowed" | "allowed_warning" | "rejected"
  rateLimitType?: string; // "seven_day" | "five_hour"
  resetsAt?: number; // unix seconds
  utilization: number; // 0..1
  isUsingOverage?: boolean;
  surpassedThreshold?: number;
}

// RateLimitSnapshot is a RateLimitInfo plus the unix-seconds time it was
// observed. Returned by GET /api/v1/agents/{id}/ratelimit.
export interface RateLimitSnapshot extends RateLimitInfo {
  observedAt: number;
}

export interface ChatEvent {
  type: "status" | "text" | "thinking" | "tool_use" | "tool_result" | "done" | "error" | "message" | "attachment" | "user_question" | "rate_limit";
  status?: string;
  delta?: string;
  toolUseId?: string;
  toolName?: string;
  toolInput?: string;
  toolOutput?: string;
  message?: AgentMessage;
  attachments?: AgentMessageAttachment[]; // streamed kojo-attach files
  errorMessage?: string;
  // Set on "user_question" events: the control_request id echoed back to the
  // answer endpoint, and the raw AskUserQuestion questions payload.
  requestId?: string;
  questions?: UserQuestion[];
  // Set on "rate_limit" events: the latest usage-window telemetry.
  rateLimit?: RateLimitInfo;
  startedAt?: string; // RFC3339 timestamp of when processing started
  // Set when this event belongs to a subagent (Task tool) turn rather
  // than the main assistant turn. Value is the tool_use ID of the
  // parent Task invocation this event should nest under.
  parentToolUseId?: string;
}

export interface AgentTask {
  id: string;
  title: string;
  status: "open" | "done";
  createdAt: string;
  updatedAt: string;
  // Strong entity tag of the backing store row; echo via If-Match on
  // update/delete for optimistic concurrency. Absent on legacy rows.
  etag?: string;
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
  // Grok session subtrees dropped wholesale ($GROK_HOME/sessions/<encoded(agentDir)>/<uuid>/).
  // Grok's events.jsonl has no kojo-compatible per-record timestamp so any
  // truncate that lands inside a session drops the whole session — the next
  // non-OneShot turn opens a fresh one.
  //
  // Optional because the fields were added after v0.19 — an older server
  // peer (mid-rollout) returns the response without them, and the UI
  // should render "0" instead of "undefined".
  grokSessionsRemoved?: number;
  grokSessionFilesRemoved?: number;
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

// PostAgentMessageResult is the 202 body of POST /agents/{id}/messages.
// Two shapes share it:
//   delivered: {accepted:true, queued:false}
//   queued:    {queued:true, id, agentId, holderPeer, holderPeerName,
//               createdAt, message} — the holder device is offline and the
//               message will be forwarded when it reconnects.
// A full queue (per-agent cap) is a 429 {error:{code:"queue_full"}} and
// surfaces as a thrown Error("429: ...") from httpClient.
export interface PostAgentMessageResult {
  accepted?: boolean;
  queued: boolean;
  id?: string;
  agentId?: string;
  holderPeer?: string;
  holderPeerName?: string;
  // Unix milliseconds (store.NowMillis), not RFC3339.
  createdAt?: number;
  message?: string;
}

// QueuedAgentMessage is one row of GET /agents/{id}/queued-messages.
export interface QueuedAgentMessage {
  id: string;
  agentId: string;
  holderPeer: string;
  content: string;
  // Unix milliseconds (store.NowMillis), not RFC3339.
  createdAt: number;
  status: string;
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

  // forceReclaim rewrites agent_locks back to this host and
  // restarts the local runtime. Owner-only recovery path for
  // agents whose post-device-switch lock points at an
  // unreachable / dead peer. Response carries the new fencing
  // token; UI just needs to await it and refresh the list.
  forceReclaim: (agentId: string) =>
    post<{ agentId: string; holderPeer: string; fencingToken: number; leaseExpiresAt: number }>(
      `/api/v1/agents/${encodeURIComponent(agentId)}/handoff/force-reclaim`,
      {},
    ),

  // GET /api/v1/agents/{id}. The server emits the row's strong ETag
  // both in the HTTP header and in the JSON body's `etag` field. We
  // prefer the body (it's threaded through React state alongside
  // every other field, so a stale form snapshot keeps its original
  // etag rather than picking up a newer one from a shared cache).
  // Fall back to the header for responses that omit `etag` from the body.
  get: async (id: string): Promise<AgentInfo> => {
    const { value, etag } = await getWithEtag<AgentInfo>(`/api/v1/agents/${id}`);
    if (!value.etag && etag) value.etag = etag;
    return value;
  },

  // GET /api/v1/agents/{id}/ratelimit — latest rate-limit snapshot, or null
  // when the backend has never reported one (204). Used to hydrate the badge
  // on mount; live updates arrive over the chat WebSocket as "rate_limit"
  // ChatEvents.
  rateLimit: (id: string): Promise<RateLimitSnapshot | null> =>
    get<RateLimitSnapshot | undefined>(`/api/v1/agents/${id}/ratelimit`).then(
      (v) => v ?? null,
    ),

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
    thumbUrl: (agentId: string, relPath: string, size = 256, v?: string) => {
      const q = v ? `&v=${encodeURIComponent(v)}` : "";
      return appendTokenQuery(
        `/api/v1/agents/${agentId}/files/thumb?path=${encodeURIComponent(relPath)}&size=${size}${q}`,
      );
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

  // user.md workspace file (per-agent notes about the people the agent works
  // with). GET surfaces an in-memory DefaultUserContent template when the
  // file is absent — isDefault=true tells the UI to suppress a no-op PUT on
  // save. PUT writes atomically and returns isDefault=false (the file now
  // exists on disk). The settings UI binds these to a dirty-tracked
  // textarea separate from the persona / cron message fields.
  //
  // Both getter and setter thread the strong etag via getWithEtag /
  // putWithIfMatch so KOJO_REQUIRE_IF_MATCH=1 strict mode accepts our
  // PUTs AND so a concurrent edit from another tab surfaces as a 412
  // (PreconditionFailedError) instead of silently clobbering. The
  // wrapper returns `{ value: { content, isDefault, etag }, etag }` —
  // the inner `etag` mirrors the body field for consistency with the
  // server response, the outer one comes from the ETag header.
  getUserContext: (id: string) =>
    getWithEtag<{ content: string; isDefault: boolean; etag?: string }>(
      `/api/v1/agents/${id}/user-context`,
    ),

  setUserContext: (id: string, content: string, expectedEtag?: string) =>
    putWithIfMatch<{ content: string; isDefault: boolean; etag?: string }>(
      `/api/v1/agents/${id}/user-context`,
      { content },
      expectedEtag,
    ),

  // checkin.md workspace file (per-agent body for cron / manual check-in
  // prompts). Same fallback / no-op-PUT contract as user-context: GET
  // returns DefaultCheckinContent with isDefault=true when checkin.md is
  // absent so the UI shows the body that the cron path would actually run.
  // PUT with an empty body clears the file (server-side trim+remove).
  // Same etag-threading rationale as the user-context endpoints above.
  getCheckinFile: (id: string) =>
    getWithEtag<{ content: string; isDefault: boolean; etag?: string }>(
      `/api/v1/agents/${id}/checkin-file`,
    ),

  putCheckinFile: (id: string, content: string, expectedEtag?: string) =>
    putWithIfMatch<{ content: string; isDefault: boolean; etag?: string }>(
      `/api/v1/agents/${id}/checkin-file`,
      { content },
      expectedEtag,
    ),

  // status.json workspace file — the agent's self-maintained state
  // (mood, energy, sleepiness, ...) injected into its system prompt.
  // Content is a flat JSON object of scalar values; the server rejects
  // nested structures so the settings UI's key-value table always
  // round-trips. Same default-template / etag contract as user-context.
  getAgentStatus: (id: string) =>
    getWithEtag<{ content: string; isDefault: boolean; etag?: string }>(
      `/api/v1/agents/${id}/status`,
    ),

  putAgentStatus: (id: string, content: string, expectedEtag?: string) =>
    putWithIfMatch<{ content: string; isDefault: boolean; etag?: string }>(
      `/api/v1/agents/${id}/status`,
      { content },
      expectedEtag,
    ),

  // anchor.md workspace file — the agent's optional persona anchor: a
  // 2-3 line first-person distillation (pronoun, tone, attitude) appended
  // to the tail of every turn's volatile context so the persona survives
  // long-context drift. Free-form markdown (no JSON validation, unlike
  // status). Empty body clears the file. GET returns an empty template
  // with isDefault=true when anchor.md is absent. Same default-template /
  // etag contract as user-context.
  getAgentAnchor: (id: string) =>
    getWithEtag<{ content: string; isDefault: boolean; etag?: string }>(
      `/api/v1/agents/${id}/anchor`,
    ),

  putAgentAnchor: (id: string, content: string, expectedEtag?: string) =>
    putWithIfMatch<{ content: string; isDefault: boolean; etag?: string }>(
      `/api/v1/agents/${id}/anchor`,
      { content },
      expectedEtag,
    ),

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
    // etag (from AgentTask.etag) is sent as If-Match when provided so a
    // stale row (edited meanwhile by the agent or another device) fails
    // with PreconditionFailedError instead of silently last-write-wins.
    update: (agentId: string, taskId: string, data: { title?: string; status?: string }, etag?: string) =>
      patchWithIfMatch<AgentTask>(`/api/v1/agents/${agentId}/tasks/${taskId}`, data, etag).then(
        (r) => r.value,
      ),
    delete: (agentId: string, taskId: string, etag?: string) =>
      delWithIfMatch<{ ok: boolean }>(`/api/v1/agents/${agentId}/tasks/${taskId}`, etag).then(
        (r) => r.value,
      ),
  },

  // Avatar / preview / files URLs go straight into <img src> so the
  // token must travel as a query param — Authorization headers can't
  // be set on element-driven fetches.
  avatarUrl: (id: string, size?: number) => {
    const base = `/api/v1/agents/${id}/avatar`;
    const sizeQ = size && size > 0 ? `?size=${size}` : "";
    return appendTokenQuery(`${base}${sizeQ}`);
  },

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

  // Queue-and-forward send path for when the agent's holder device is
  // offline: the server either delivers immediately (queued:false) or
  // enqueues for delivery on reconnect (queued:true). 429 queue_full
  // propagates as a thrown Error.
  postAgentMessage: (agentId: string, content: string) =>
    post<PostAgentMessageResult>(`/api/v1/agents/${agentId}/messages`, { content }),

  // steerAgent injects an additional user message into the agent's
  // currently running turn. On success mode is "steer" (merged into the
  // running turn) or "fallback_turn" (the turn had just ended, so the server
  // started a normal follow-up turn with the text — treat like a normal send;
  // its reply streams back over the agent WS). Rejects with an Error whose
  // message starts with "409:" for a non-steerable backend ("unsupported") or
  // a busy agent — callers should restore the text rather than drop it.
  steerAgent: (agentId: string, content: string) =>
    post<{ ok: boolean; mode?: string }>(`/api/v1/agents/${agentId}/steer`, { content }),

  // answerQuestion resolves a pending interactive AskUserQuestion on the
  // agent's running turn. Pass answers (question → chosen answer) to allow, or
  // deny=true to refuse. Rejects with "409:"/"404:" prefixed errors when the
  // turn ended or the question already resolved.
  answerAgentQuestion: (
    agentId: string,
    requestId: string,
    answers: Record<string, string | string[]>,
    deny = false,
    denyMessage = "",
  ) =>
    post<{ ok: boolean }>(`/api/v1/agents/${agentId}/answer`, {
      requestId,
      answers,
      deny,
      denyMessage,
    }),

  getQueuedMessages: (agentId: string) =>
    get<{ messages: QueuedAgentMessage[] }>(
      `/api/v1/agents/${agentId}/queued-messages`,
    ).then((r) => r.messages ?? []),

  cancelQueuedMessage: (agentId: string, qid: string) =>
    del<{ cancelled: boolean; id: string }>(
      `/api/v1/agents/${agentId}/queued-messages/${qid}`,
    ),

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

  apiKeys: {
    get: (provider: string) =>
      get<{ provider: string; configured: boolean; hasFallback?: boolean; embeddingModel?: string }>(`/api/v1/api-keys/${provider}`),

    set: (provider: string, apiKey: string) =>
      put<{ ok: boolean }>(`/api/v1/api-keys/${provider}`, { apiKey }),

    delete: (provider: string) =>
      del<unknown>(`/api/v1/api-keys/${provider}`),
  },

  stt: {
    // Mint a short-lived ephemeral token for the browser to open the xAI
    // streaming Speech-to-Text WebSocket directly.
    token: () =>
      post<{ token: string; expiresAt: number; wsBaseUrl: string }>(`/api/v1/stt/token`, {}),
  },

  embeddingModel: {
    set: (model: string) =>
      put<{ ok: boolean; model: string; embeddingsCleared: boolean }>(`/api/v1/embedding-model`, { model }),
    list: () =>
      get<{ models: string[] }>(`/api/v1/embedding-models`).then((r) => r.models ?? []),
  },

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

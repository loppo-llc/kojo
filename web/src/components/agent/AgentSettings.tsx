import { useEffect, useRef, useState } from "react";
import { useParams, useNavigate } from "react-router";
import { agentApi, type AgentInfo } from "../../lib/agentApi";
import { api } from "../../lib/api";
import { useTTSCapability } from "../../hooks/useTTS";
import { ttsApi, pickBestFormat } from "../../lib/ttsApi";
import { AgentAvatar } from "./AgentAvatar";
import { ScheduleEditor } from "./ScheduleEditor";
import { SlackBotSettings } from "./SlackBotSettings";
import { defaultModelForTool, modelsForTool, effortLevelsForModel, defaultEffortForModel, supportsEffort, type EffortLevel } from "../../lib/toolModels";
import { buildAgentSavePayload } from "./agentSettingsPayload";

export function AgentSettings() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const navigateRef = useRef(navigate);
  navigateRef.current = navigate;
  const [agent, setAgent] = useState<AgentInfo | null>(null);
  const [name, setName] = useState("");
  const [persona, setPersona] = useState("");
  const [publicProfile, setPublicProfile] = useState("");
  const [publicProfileOverride, setPublicProfileOverride] = useState(false);
  const [model, setModel] = useState("");
  const [effort, setEffort] = useState("");
  const [tool, setTool] = useState("");
  const [customBaseURL, setCustomBaseURL] = useState("http://localhost:8080");
  const [customModels, setCustomModels] = useState<string[]>([]);
  const [thinkingMode, setThinkingMode] = useState("");
  const [workDir, setWorkDir] = useState("");
  const [cronExpr, setCronExpr] = useState("");
  const [timeoutMinutes, setTimeoutMinutes] = useState(10);
  const [resumeIdleMinutes, setResumeIdleMinutes] = useState(0);
  const [silentStart, setSilentStart] = useState("");
  const [silentEnd, setSilentEnd] = useState("");
  const [notifyDuringSilent, setNotifyDuringSilent] = useState(false);
  const [cronMessage, setCronMessage] = useState("");
  // checkinIsDefault tracks whether the value currently shown in the
  // cronMessage textarea is the in-memory DefaultCheckinContent template
  // (no checkin.md on disk yet) versus an actual saved file. Save
  // suppresses no-op PUTs when the user hasn't dirtied the default.
  const [checkinIsDefault, setCheckinIsDefault] = useState(false);
  // loadedCheckin pins the exact textarea content the form was
  // hydrated with — works for both the saved-from-disk path AND the
  // default-template path. Dirty detection compares the current
  // textarea against this snapshot directly: if they match, no PUT
  // (covers the "open settings → click Save without editing"
  // no-op case for default templates, which would otherwise persist
  // the unedited template to disk).
  const [loadedCheckin, setLoadedCheckin] = useState("");
  // checkinEtag pins the etag returned by the most recent GET / PUT so
  // the next PUT sends a matching If-Match. Strict-mode servers
  // (KOJO_REQUIRE_IF_MATCH=1) reject PUTs without it; a concurrent
  // edit from another tab surfaces as 412 via PreconditionFailedError.
  // Empty string means "no live row yet" (default-template path) —
  // putWithIfMatch omits the header in that case which is fine: the
  // server's not-yet-existent row has no etag to assert against.
  const [checkinEtag, setCheckinEtag] = useState("");
  // user.md workspace file — separate textarea below the persona / cron
  // sections. Same default-vs-saved tracking as cronMessage so an unedited
  // template doesn't get persisted on Save.
  const [userContext, setUserContext] = useState("");
  const [userContextIsDefault, setUserContextIsDefault] = useState(false);
  const [loadedUser, setLoadedUser] = useState("");
  // Same etag-threading as checkinEtag above, for the user.md workspace file.
  const [userContextEtag, setUserContextEtag] = useState("");
  const [saving, setSaving] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [archiving, setArchiving] = useState(false);
  const [resetting, setResetting] = useState(false);
  const [resettingSession, setResettingSession] = useState(false);
  // Memory truncation. The datetime-local input emits a naive
  // "YYYY-MM-DDTHH:mm" string; we attach the browser's current UTC offset
  // when calling the API so the server interprets it in local time. The
  // result preview stays visible until the next truncation kicks off so
  // operators can see what got removed.
  const [truncating, setTruncating] = useState(false);
  const [truncateSince, setTruncateSince] = useState("");
  const [truncateResult, setTruncateResult] = useState<{
    since: string;
    messagesRemoved: number;
    claudeSessionEntriesRemoved: number;
    claudeSessionFilesRemoved: number;
    // Optional so older servers (pre-grok-truncate rollout) that omit the
    // fields render as "0" via `?? 0` below instead of "undefined".
    grokSessionsRemoved?: number;
    grokSessionFilesRemoved?: number;
    diaryFilesRemoved: number;
    diaryEntriesRemoved: number;
  } | null>(null);
  const [checkingIn, setCheckingIn] = useState(false);
  const [error, setError] = useState("");
  const [success, setSuccess] = useState(false);
  // Distinct from `success` because a manual check-in is fire-and-forget
  // (server returns 202 immediately); reusing the green "Saved" banner
  // would imply persistence rather than a job started in the background.
  const [checkinNotice, setCheckinNotice] = useState("");
  const [avatarToken, setAvatarToken] = useState(() => Date.now());
  const [generatingAvatar, setGeneratingAvatar] = useState(false);
  const [personaPrompt, setPersonaPrompt] = useState("");
  const [generatingPersona, setGeneratingPersona] = useState(false);
  const [allowedTools, setAllowedTools] = useState<string[]>([]);
  const [allowProtectedPaths, setAllowProtectedPaths] = useState<string[]>([]);
  // TTS settings
  const [ttsEnabled, setTTSEnabled] = useState(false);
  const [ttsModel, setTTSModel] = useState("");
  const [ttsVoice, setTTSVoice] = useState("");
  const [ttsStylePrompt, setTTSStylePrompt] = useState("");
  const [ttsPreviewVoice, setTTSPreviewVoice] = useState<string | null>(null);
  const [ttsPreviewError, setTTSPreviewError] = useState("");
  const ttsCapability = useTTSCapability();
  const ttsPreviewAudioRef = useRef<HTMLAudioElement | null>(null);

  // playPreview synthesizes a fixed sample line for the chosen voice
  // and plays it. Concurrent preview clicks supersede the previous one.
  const playPreview = async (voiceName: string) => {
    if (!ttsCapability) return;
    setTTSPreviewError("");
    setTTSPreviewVoice(voiceName);
    // Stop whatever's currently playing before kicking off a new fetch
    // so two clicks don't overlap audibly.
    if (ttsPreviewAudioRef.current) {
      ttsPreviewAudioRef.current.pause();
      ttsPreviewAudioRef.current.src = "";
      ttsPreviewAudioRef.current = null;
    }
    try {
      const fmt = pickBestFormat(ttsCapability.formats);
      const res = await ttsApi.preview(voiceName, {
        model: ttsModel || undefined,
        stylePrompt: ttsStylePrompt.trim() || undefined,
        format: fmt,
      });
      const audio = new Audio(ttsApi.audioUrl(res.url));
      audio.onended = () => {
        if (ttsPreviewAudioRef.current === audio) {
          ttsPreviewAudioRef.current = null;
          setTTSPreviewVoice((cur) => (cur === voiceName ? null : cur));
        }
      };
      audio.onerror = () => {
        if (ttsPreviewAudioRef.current === audio) {
          setTTSPreviewError("Playback error");
          ttsPreviewAudioRef.current = null;
          setTTSPreviewVoice(null);
        }
      };
      ttsPreviewAudioRef.current = audio;
      await audio.play();
    } catch (e) {
      setTTSPreviewError(e instanceof Error ? e.message : String(e));
      setTTSPreviewVoice(null);
    }
  };

  // Stop preview audio when the settings page unmounts.
  useEffect(
    () => () => {
      if (ttsPreviewAudioRef.current) {
        ttsPreviewAudioRef.current.pause();
        ttsPreviewAudioRef.current.src = "";
        ttsPreviewAudioRef.current = null;
      }
    },
    [],
  );
  const [privileged, setPrivileged] = useState(false);
  const [privilegeSaving, setPrivilegeSaving] = useState(false);
  const [showForkDialog, setShowForkDialog] = useState(false);
  const [forkName, setForkName] = useState("");
  const [forkIncludeTranscript, setForkIncludeTranscript] = useState(false);
  const [forking, setForking] = useState(false);
  const [forkError, setForkError] = useState("");
  const fileRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (!id) return;
    // Run agent + workspace-file fetches in parallel via allSettled so a
    // 404 on one endpoint (e.g. /user-context against a server that
    // hasn't been rebuilt) doesn't blank out the rest of the form. The
    // agent record is the gate that determines whether the agent exists
    // at all; any error on its fetch redirects home, but workspace-file
    // failures fall back to the in-memory defaults.
    Promise.allSettled([
      agentApi.get(id),
      agentApi.getCheckinFile(id),
      agentApi.getUserContext(id),
    ]).then(([agentRes, checkinRes, userCtxRes]) => {
      if (agentRes.status !== "fulfilled") {
        navigateRef.current("/");
        return;
      }
      const a = agentRes.value;
      setAgent(a);
      setName(a.name);
      setPersona(a.persona);
      setPublicProfile(a.publicProfile ?? "");
      setPublicProfileOverride(a.publicProfileOverride ?? false);
      setModel(a.model);
      setEffort(a.effort || "");
      setTool(a.tool);
      setCustomBaseURL(a.customBaseURL ?? "http://localhost:8080");
      setThinkingMode(a.thinkingMode ?? "");
      setWorkDir(a.workDir ?? "");
      setCronExpr(a.cronExpr ?? "");
      setTimeoutMinutes(a.timeoutMinutes || 10);
      setResumeIdleMinutes(a.resumeIdleMinutes ?? 0);
      setSilentStart(a.silentStart ?? "");
      setSilentEnd(a.silentEnd ?? "");
      setNotifyDuringSilent(a.notifyDuringSilent ?? true);
      setAllowedTools(a.allowedTools ?? []);
      setAllowProtectedPaths(a.allowProtectedPaths ?? []);
      setPrivileged(a.privileged ?? false);
      setTTSEnabled(a.tts?.enabled ?? false);
      setTTSModel(a.tts?.model ?? "");
      setTTSVoice(a.tts?.voice ?? "");
      setTTSStylePrompt(a.tts?.stylePrompt ?? "");
      // checkin.md: prefer the workspace file. The legacy inline
      // agent.cronMessage is migrated into checkin.md on agent load
      // (see Manager init), so on a current server the endpoint
      // always wins. The fallback only matters during the brief
      // upgrade window when an old server is still in the deployment.
      //
      // getCheckinFile / getUserContext now return EtaggedResponse so
      // we capture the etag alongside the body — the next PUT threads
      // it via If-Match. Default-template responses come back with
      // etag="" (no row yet); we pass that through to the etag state
      // and putWithIfMatch will omit the header in that case.
      if (checkinRes.status === "fulfilled") {
        const v = checkinRes.value.value;
        setCronMessage(v.content);
        setCheckinIsDefault(v.isDefault);
        setLoadedCheckin(v.content);
        setCheckinEtag(checkinRes.value.etag ?? v.etag ?? "");
      } else {
        const fallback = a.cronMessage ?? "";
        setCronMessage(fallback);
        setCheckinIsDefault(!fallback.trim());
        setLoadedCheckin(fallback);
        setCheckinEtag("");
      }
      if (userCtxRes.status === "fulfilled") {
        const v = userCtxRes.value.value;
        setUserContext(v.content);
        setUserContextIsDefault(v.isDefault);
        setLoadedUser(v.content);
        setUserContextEtag(userCtxRes.value.etag ?? v.etag ?? "");
      } else {
        setUserContext("");
        setUserContextIsDefault(true);
        setLoadedUser("");
        setUserContextEtag("");
      }
    });
  }, [id]);

  // Keep nextCronAt fresh. The initial GET captures a snapshot; without
  // this the displayed time drifts into the past (e.g. user leaves the
  // tab open across a check-in) and the "(X ago)" relative label becomes
  // a stale read of a value the server has long since recomputed.
  //
  // Strategy: schedule a single-shot refetch for ~5s after the displayed
  // nextCronAt elapses (small grace so we land *after* the server-side
  // tick fires and updates its own state), and refetch whenever the tab
  // regains visibility (covers laptop-sleep + phone background cases
  // where the timer doesn't fire on time).
  //
  // Merges ONLY the server-derived display fields (nextCronAt,
  // cronPausedGlobal) into local agent state. Crucially does NOT
  // overwrite `etag` — the form's snapshot etag must keep pointing at
  // the version the user last loaded so a subsequent save still gets
  // the 412 (precondition failed) on concurrent edits. Form fields
  // aren't touched either, so unsaved edits survive.
  useEffect(() => {
    if (!id) return;
    const next = agent?.nextCronAt;
    if (!next) return;

    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | null = null;
    // Cap at 1 day so browsers don't silently round huge int32 ms
    // delays (~24.8d) to instant-fire — a monthly cron that schedules
    // further out keeps re-arming each day until the real tick arrives.
    const ONE_DAY_MS = 24 * 60 * 60 * 1000;

    const arm = (dueAt: number) => {
      if (Number.isNaN(dueAt)) return;
      // Clear any prior pending tick — a visibility-triggered refetch
      // that lands while a timer is still armed must not leak the
      // stale handle (the cleanup `clearTimeout(timer)` only sees the
      // most recent assignment).
      if (timer !== null) clearTimeout(timer);
      // 5s grace lets the server's own scheduler advance its entry.Next
      // past the firing tick before we ask. Negative delays (already
      // overdue from a stale fetch) collapse to a near-immediate refetch.
      const raw = dueAt + 5_000 - Date.now();
      const delay = Math.max(0, Math.min(raw, ONE_DAY_MS));
      timer = setTimeout(refetch, delay);
    };

    const refetch = () => {
      if (cancelled) return;
      agentApi.get(id).then((fresh) => {
        // A late-arriving response from a refetch issued before the
        // user navigated / edited could clobber newer state. Drop it.
        if (cancelled) return;
        setAgent((prev) => prev ? {
          ...prev,
          nextCronAt: fresh.nextCronAt,
          cronPausedGlobal: fresh.cronPausedGlobal,
        } : fresh);
        // If the value is unchanged the outer effect won't re-run
        // (deps still equal) — re-arm explicitly so a 1-day-capped
        // timer for a far-future cron keeps making progress.
        if (!cancelled && fresh.nextCronAt && fresh.nextCronAt === next) {
          arm(new Date(fresh.nextCronAt).getTime());
        }
      }).catch(() => { /* keep prior */ });
    };

    const visibilityHandler = () => {
      if (document.visibilityState === "visible") refetch();
    };
    document.addEventListener("visibilitychange", visibilityHandler);

    arm(new Date(next).getTime());

    return () => {
      cancelled = true;
      if (timer !== null) clearTimeout(timer);
      document.removeEventListener("visibilitychange", visibilityHandler);
    };
  }, [id, agent?.nextCronAt]);

  const needsCustomURL = tool === "custom" || tool === "llama.cpp";

  useEffect(() => {
    if (!needsCustomURL) return;
    let cancelled = false;
    const timer = setTimeout(() => {
      api.customModels(customBaseURL).then((models) => {
        if (cancelled) return;
        setCustomModels(models);
        if (models.length > 0) {
          setModel((prev) => models.includes(prev) ? prev : models[0]);
        }
      }).catch(() => { if (!cancelled) setCustomModels([]); });
    }, 300);
    return () => { cancelled = true; clearTimeout(timer); };
  }, [needsCustomURL, customBaseURL]);

  const handleSave = async () => {
    setSaving(true);
    setError("");
    setSuccess(false);
    try {
      // cronMessage and userContext live in separate workspace files
      // (checkin.md, user.md) — they're persisted via dedicated
      // endpoints, NOT the agents PATCH. Dirty detection compares the
      // current textarea against the loaded snapshot directly. This
      // covers all four interesting cases without special-casing the
      // default-template path:
      //   - default + textarea untouched (still equals
      //     DefaultCheckinContent): no PUT.
      //   - default + textarea edited: PUT creates the file on disk.
      //   - saved + textarea cleared: PUT with empty body, server
      //     removes the file via trim+remove.
      //   - saved + body changed: PUT writes the new body.
      const checkinDirty = cronMessage !== loadedCheckin;
      const userDirty = userContext !== loadedUser;

      const updated = await agentApi.update(
        id!,
        buildAgentSavePayload({
          name,
          persona,
          publicProfile,
          publicProfileOverride,
          model,
          effort,
          tool,
          customBaseURL,
          thinkingMode,
          workDir,
          cronExpr,
          timeoutMinutes,
          resumeIdleMinutes,
          silentStart,
          silentEnd,
          notifyDuringSilent,
          // cronMessage is intentionally left blank in the PATCH payload —
          // the agents row no longer drives cron/checkin behaviour. Save
          // continues below via PUT /checkin-file.
          cronMessage: "",
          allowedTools,
          allowProtectedPaths,
          tts: {
            enabled: ttsEnabled,
            model: ttsModel,
            voice: ttsVoice,
            stylePrompt: ttsStylePrompt,
          },
        }),
        // The form's snapshot etag — captured at GET time, NOT a global
        // cache lookup. Without this If-Match the server happily accepts
        // a stale form's overwrite even when another tab has saved newer
        // values; with it, the second writer gets 412 and we surface a
        // "someone else changed this" message below.
        agent?.etag,
      );

      if (checkinDirty) {
        // Thread the etag captured at load time as If-Match. Empty
        // (default-template path) is fine — putWithIfMatch omits the
        // header. After the write the response carries the freshly
        // computed etag, which we re-pin for the next save.
        const wrapped = await agentApi.putCheckinFile(
          id!,
          cronMessage,
          checkinEtag || undefined,
        );
        const res = wrapped.value;
        setCheckinIsDefault(res.isDefault);
        setCronMessage(res.content);
        // Re-pin the loaded snapshot so subsequent edits compare
        // against the value now on disk (or the default template if
        // the body was cleared), not the value the form was first
        // hydrated with.
        setLoadedCheckin(res.content);
        setCheckinEtag(wrapped.etag ?? res.etag ?? "");
      }
      if (userDirty) {
        const wrapped = await agentApi.setUserContext(
          id!,
          userContext,
          userContextEtag || undefined,
        );
        const res = wrapped.value;
        setUserContextIsDefault(res.isDefault);
        setUserContext(res.content);
        setLoadedUser(res.content);
        setUserContextEtag(wrapped.etag ?? res.etag ?? "");
      }

      setAgent(updated);
      setPublicProfile(updated.publicProfile ?? "");
      setPublicProfileOverride(updated.publicProfileOverride ?? false);
      // Re-sync cronExpr to whatever the server actually persisted. Without
      // this, picking a Preset chip ("@preset:30") leaves the local state at
      // the sentinel even though the saved row is the expanded form
      // ("7,37 * * * *"); the dirty diff stays true forever.
      setCronExpr(updated.cronExpr ?? "");
      setSuccess(true);
      setTimeout(() => setSuccess(false), 2000);
    } catch (err) {
      // PreconditionFailedError is the etag-mismatch 412. Re-fetch the
      // agent so the form rebases onto the server's current row before
      // the user re-applies their edit (otherwise the next Save would
      // 412 again with the same stale etag they started with).
      if (err instanceof Error && err.name === "PreconditionFailedError") {
        setError("Someone else updated this agent. Reloading…");
        try {
          const fresh = await agentApi.get(id!);
          setAgent(fresh);
          // Don't blow away the user's in-progress edits — only refresh
          // the etag-bearing record. Form fields stay as-is so the user
          // can decide what to do; clicking Save again will use the new
          // etag and either succeed or 412 again if a third write
          // landed in between.
        } catch {
          /* swallow — primary error is already shown */
        }
        return;
      }
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const handleCheckin = async () => {
    // The server runs the check-in against the persisted agent record, not
    // the in-flight edits in this form. Bail with a notice instead of
    // silently using stale cronMessage/timeoutMinutes — fixing this with
    // an auto-save would mean committing other unrelated dirty fields too.
    // Match the same normalisations the load path applies so an agent that
    // hasn't been touched doesn't read as dirty:
    //   - timeoutMinutes: 0 (server default) is shown as 10 in the UI, so
    //     compare against the same "|| 10" coercion done at load.
    //   - cronMessage: the server trims on save, so a saved value of "x"
    //     stays equal to "x" no matter how many spaces the textarea adds.
    // Dirty detection compares the current textarea against the
    // loaded snapshot — same source of truth Save uses. Hitting the
    // network here would race against an in-flight PUT and surface a
    // false positive whenever the form just saved.
    const savedTimeout = agent ? (agent.timeoutMinutes || 10) : timeoutMinutes;
    if (
      agent &&
      (cronMessage !== loadedCheckin || savedTimeout !== timeoutMinutes)
    ) {
      setCheckinNotice(
        "Save your changes first — manual check-in uses the saved Check-in Message and Timeout.",
      );
      setTimeout(() => setCheckinNotice(""), 5000);
      return;
    }
    setCheckingIn(true);
    setError("");
    setCheckinNotice("");
    try {
      await agentApi.checkin(id!);
      try {
        const a = await agentApi.get(id!);
        setAgent(a);
      } catch {
        // non-fatal — stick with the stale value
      }
      setCheckinNotice(
        "Check-in started — the agent will reply in chat when it finishes.",
      );
      setTimeout(() => setCheckinNotice(""), 4000);
    } catch (err) {
      // Match against the server's typed error code rather than the HTTP
      // status: 409 also covers `code:"archived"` (and any future
      // conflict cases) which the user should NOT see as "already
      // working". Only `code:"busy"` means a chat is in flight, which is
      // the case we want to silently turn into a notice instead of a
      // red error.
      const msg = err instanceof Error ? err.message : String(err);
      if (/"code"\s*:\s*"busy"/.test(msg)) {
        setCheckinNotice(
          "Check-in skipped — the agent is already working on something.",
        );
        setTimeout(() => setCheckinNotice(""), 4000);
      } else {
        setError(msg);
      }
    } finally {
      setCheckingIn(false);
    }
  };

  const handleTogglePrivileged = async (next: boolean) => {
    // Privilege is mutated via a dedicated endpoint (not PATCH /agents/{id}),
    // so this fires its own request rather than waiting on Save Changes.
    setPrivilegeSaving(true);
    setError("");
    try {
      await agentApi.setPrivileged(id!, next);
      setPrivileged(next);
      setAgent((a) => (a ? { ...a, privileged: next } : a));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setPrivilegeSaving(false);
    }
  };

  const handleResetSession = async () => {
    if (!confirm("Reset CLI session? Conversation history and memory are kept, but the AI will start a fresh context window.")) return;
    setResettingSession(true);
    setError("");
    try {
      await agentApi.resetSession(id!);
      setSuccess(true);
      setTimeout(() => setSuccess(false), 2000);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setResettingSession(false);
    }
  };

  // Convert a datetime-local value ("YYYY-MM-DDTHH:mm") into RFC3339 with
  // the browser's local UTC offset. Returns null if the input is empty or
  // doesn't parse — caller surfaces that as a validation error.
  const datetimeLocalToRFC3339 = (value: string): string | null => {
    if (!value) return null;
    const d = new Date(value);
    if (Number.isNaN(d.getTime())) return null;
    const pad = (n: number) => String(n).padStart(2, "0");
    const offMin = -d.getTimezoneOffset();
    const sign = offMin >= 0 ? "+" : "-";
    const offH = pad(Math.floor(Math.abs(offMin) / 60));
    const offM = pad(Math.abs(offMin) % 60);
    return (
      `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}` +
      `T${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}` +
      `${sign}${offH}:${offM}`
    );
  };

  const handleTruncateMemory = async () => {
    const iso = datetimeLocalToRFC3339(truncateSince);
    if (!iso) {
      setError("Pick a date/time to truncate from.");
      return;
    }
    if (
      !confirm(
        `Delete every memory recorded at or after ${iso}? This drops kojo transcript records, Claude --resume session entries (with trailing-turn cleanup), the entire grok --resume session (events.jsonl has no per-record timestamp so partial cuts are not safe — the next turn opens a fresh session), and matching daily diary bullets. Persona, MEMORY.md, project / people / topic notes, and credentials are kept.`,
      )
    ) {
      return;
    }
    setTruncating(true);
    setError("");
    setTruncateResult(null);
    try {
      const res = await agentApi.truncateMemory(id!, { since: iso });
      setTruncateResult(res);
      setSuccess(true);
      setTimeout(() => setSuccess(false), 2000);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setTruncating(false);
    }
  };

  const handleResetData = async () => {
    if (!confirm("Reset conversation logs and memory? Settings, persona, avatar, and credentials will be kept.")) return;
    setResetting(true);
    setError("");
    try {
      await agentApi.resetData(id!);
      setSuccess(true);
      setTimeout(() => setSuccess(false), 2000);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setResetting(false);
    }
  };

  const openForkDialog = () => {
    setForkName(agent ? `${agent.name} (fork)` : "");
    setForkIncludeTranscript(false);
    setForkError("");
    setShowForkDialog(true);
  };

  const handleFork = async () => {
    const trimmed = forkName.trim();
    if (!trimmed) {
      setForkError("Name is required");
      return;
    }
    setForking(true);
    setForkError("");
    try {
      const forked = await agentApi.fork(id!, { name: trimmed, includeTranscript: forkIncludeTranscript });
      setShowForkDialog(false);
      navigate(`/agents/${forked.id}/settings`);
    } catch (err) {
      setForkError(err instanceof Error ? err.message : String(err));
    } finally {
      setForking(false);
    }
  };

  const handleDelete = async () => {
    if (!confirm("Delete this agent? This cannot be undone.")) return;
    setDeleting(true);
    try {
      await agentApi.delete(id!);
      navigate("/", { replace: true });
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setDeleting(false);
    }
  };

  const handleArchive = async () => {
    if (
      !confirm(
        "Archive this agent? Runtime activity stops; data is kept and can be restored from Settings.\n\nThe agent will be removed from all group DMs (2-person groups dissolve), and memberships are NOT restored on unarchive — the agent must be re-invited.",
      )
    )
      return;
    setArchiving(true);
    try {
      await agentApi.archive(id!);
      navigate("/", { replace: true });
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setArchiving(false);
    }
  };

  const handleAvatarUpload = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file || !id) return;
    try {
      await agentApi.uploadAvatar(id, file);
      setAvatarToken(Date.now());
      setAgent((a) => (a ? { ...a, hasAvatar: true } : a));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const handleGeneratePersona = async () => {
    if (!personaPrompt.trim()) return;
    setGeneratingPersona(true);
    setError("");
    try {
      const result = await agentApi.generatePersona(persona.trim(), personaPrompt.trim());
      setPersona(result.persona);
      setPersonaPrompt("");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setGeneratingPersona(false);
    }
  };

  const handleGenerateAvatar = async () => {
    if (!id || !persona.trim()) return;
    setGeneratingAvatar(true);
    setError("");
    try {
      const result = await agentApi.generateAvatar(persona.trim(), name.trim());
      await agentApi.uploadGeneratedAvatar(id, result.avatarPath);
      setAvatarToken(Date.now());
      setAgent((a) => (a ? { ...a, hasAvatar: true } : a));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setGeneratingAvatar(false);
    }
  };

  if (!agent) return null;

  return (
    <div className="min-h-full bg-neutral-950 text-neutral-200">
      <header className="flex items-center gap-2 px-4 py-3 border-b border-neutral-800">
        <button
          onClick={() => navigate(`/agents/${id}`, { replace: true })}
          className="text-neutral-400 hover:text-neutral-200"
        >
          &larr;
        </button>
        <h1 className="text-lg font-bold">Settings</h1>
      </header>

      <main className="p-4 space-y-6 max-w-md mx-auto">
        {/* ── Agent Settings ── */}
        <section className="rounded-xl border border-neutral-800 p-5 space-y-5">
          <h2 className="text-sm font-semibold text-neutral-300">Agent</h2>

        {/* Avatar */}
        <div className="flex items-center gap-4">
          <AgentAvatar agentId={agent.id} name={agent.name} size="xl" cacheBust={avatarToken} />
          <div className="flex gap-2">
            <button
              onClick={() => fileRef.current?.click()}
              className="px-3 py-1.5 bg-neutral-800 hover:bg-neutral-700 rounded text-sm"
            >
              Change Avatar
            </button>
            <button
              onClick={handleGenerateAvatar}
              disabled={generatingAvatar || !persona.trim()}
              className="px-3 py-1.5 bg-neutral-800 hover:bg-neutral-700 rounded text-sm disabled:opacity-40 flex items-center gap-1.5"
            >
              {generatingAvatar ? (
                <><span className="animate-spin">↻</span> Generating...</>
              ) : (
                <>✨ Generate</>
              )}
            </button>
            <input
              ref={fileRef}
              type="file"
              accept="image/*"
              onChange={handleAvatarUpload}
              className="hidden"
            />
          </div>
        </div>

        {/* Name */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">Name</label>
          <input
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
          />
        </div>

        {/* Persona */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">
            Persona
          </label>
          <textarea
            value={persona}
            onChange={(e) => setPersona(e.target.value)}
            rows={6}
            className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm resize-none focus:outline-none focus:border-neutral-500"
          />
          <div className="flex gap-2 mt-2">
            <input
              type="text"
              value={personaPrompt}
              onChange={(e) => setPersonaPrompt(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !e.nativeEvent.isComposing && !e.shiftKey && !generatingPersona) {
                  e.preventDefault();
                  handleGeneratePersona();
                }
              }}
              placeholder="e.g. もっと毒舌にして"
              className="flex-1 px-3 py-1.5 bg-neutral-900 border border-neutral-700 rounded text-xs focus:outline-none focus:border-neutral-500"
            />
            <button
              onClick={handleGeneratePersona}
              disabled={generatingPersona || !personaPrompt.trim()}
              className="px-3 py-1.5 bg-neutral-800 hover:bg-neutral-700 rounded text-xs disabled:opacity-40 flex items-center gap-1"
            >
              {generatingPersona ? (
                <span className="animate-spin">↻</span>
              ) : (
                "✨ AI"
              )}
            </button>
          </div>
        </div>

        {/* User Context (user.md) */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">
            User Context
          </label>
          <textarea
            value={userContext}
            onChange={(e) => {
              setUserContext(e.target.value);
              // First keystroke off the default template — clear the
              // default flag so Save persists the edit instead of
              // treating it as a no-op against the in-memory template.
              if (userContextIsDefault) setUserContextIsDefault(false);
            }}
            rows={6}
            className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm font-mono resize-y focus:outline-none focus:border-neutral-500"
          />
          <p className="mt-1 text-xs text-neutral-600">
            Notes about the people this agent works with — name, timezone, communication preferences, etc. Injected into the system prompt as data (head/tail truncated above 1500 chars). {userContextIsDefault && "Template — not yet saved."}
          </p>
        </div>

        {/* Public Profile */}
        <div>
          <div className="flex items-center justify-between mb-2">
            <label className="text-sm text-neutral-400">
              Public Profile
            </label>
            <label className="flex items-center gap-1.5 text-xs text-neutral-500 cursor-pointer">
              <input
                type="checkbox"
                checked={publicProfileOverride}
                onChange={(e) => setPublicProfileOverride(e.target.checked)}
                className="rounded border-neutral-600"
              />
              Override
            </label>
          </div>
          <textarea
            value={publicProfile}
            onChange={(e) => setPublicProfile(e.target.value)}
            rows={2}
            disabled={!publicProfileOverride}
            placeholder={publicProfileOverride ? "Enter custom public profile" : "Auto-generated from persona"}
            className={`w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm resize-none focus:outline-none focus:border-neutral-500 ${
              !publicProfileOverride ? "opacity-60 cursor-not-allowed" : ""
            }`}
          />
          <p className="mt-1 text-xs text-neutral-600">
            {publicProfileOverride
              ? "Manual override — won't be replaced when persona changes."
              : "Auto-generated from persona. Visible to other agents via directory."}
          </p>
        </div>

        {/* Model */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">Model</label>
          {(() => {
            const models = needsCustomURL ? customModels : modelsForTool(tool);
            return models.length > 0 ? (
              <select
                value={model}
                onChange={(e) => {
                  const m = e.target.value;
                  setModel(m);
                  if (effort && !effortLevelsForModel(m).includes(effort as EffortLevel)) setEffort("");
                }}
                className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
              >
                {model && !models.includes(model) && (
                  <option value={model}>{model}</option>
                )}
                {models.map((m) => (
                  <option key={m} value={m}>
                    {m}
                  </option>
                ))}
              </select>
            ) : (
              <input
                type="text"
                value={model}
                onChange={(e) => {
                  const m = e.target.value;
                  setModel(m);
                  if (effort && !effortLevelsForModel(m).includes(effort as EffortLevel)) setEffort("");
                }}
                placeholder="model name"
                className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
              />
            );
          })()}
        </div>

        {/* Effort */}
        {supportsEffort(tool) && (
          <div>
            <label className="block text-sm text-neutral-400 mb-2">Effort</label>
            <select
              value={effort}
              onChange={(e) => setEffort(e.target.value)}
              className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
            >
              <option value="">default ({defaultEffortForModel(model)})</option>
              {effortLevelsForModel(model).map((e) => (
                <option key={e} value={e}>
                  {e}
                </option>
              ))}
            </select>
          </div>
        )}

        {/* Tool */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">Tool</label>
          <div className="flex flex-wrap gap-2">
            {["claude", "codex", "grok", "custom", "llama.cpp"].map((t) => (
              <button
                key={t}
                onClick={() => {
                  if (t !== tool) {
                    setTool(t);
                    if (t === "custom" || t === "llama.cpp") {
                      setModel("");
                    } else {
                      const m = defaultModelForTool(t);
                      setModel(m);
                      if (effort && !effortLevelsForModel(m).includes(effort as EffortLevel)) setEffort("");
                    }
                  }
                }}
                className={`px-3 py-2 rounded text-sm font-mono ${
                  tool === t
                    ? "bg-neutral-700 border border-neutral-500"
                    : "bg-neutral-900 border border-neutral-800"
                }`}
              >
                {t}
              </button>
            ))}
          </div>
        </div>

        {/* Custom Base URL */}
        {needsCustomURL && (
          <div>
            <label className="block text-sm text-neutral-400 mb-2">Custom Base URL</label>
            <input
              type="text"
              value={customBaseURL}
              onChange={(e) => setCustomBaseURL(e.target.value)}
              placeholder="http://localhost:8080"
              className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm font-mono focus:outline-none focus:border-neutral-500"
            />
            <p className="text-xs text-neutral-600 mt-1">Anthropic Messages API compatible endpoint</p>
          </div>
        )}

        {/* Allowed Tools (custom only) */}
        {tool === "custom" && (
          <div>
            <label className="block text-sm text-neutral-400 mb-2">
              Allowed Tools
              <span className="text-xs text-neutral-600 ml-2">(empty = all)</span>
            </label>
            <div className="grid grid-cols-2 gap-1.5">
              {["Bash", "Read", "Write", "Edit", "Glob", "Grep", "Skill", "WebFetch", "WebSearch", "Agent", "NotebookEdit"].map((t) => (
                <label key={t} className="flex items-center gap-2 px-2 py-1.5 bg-neutral-900 rounded text-xs font-mono cursor-pointer hover:bg-neutral-800">
                  <input
                    type="checkbox"
                    checked={allowedTools.length === 0 || allowedTools.includes(t)}
                    onChange={(e) => {
                      if (allowedTools.length === 0) {
                        // Switching from "all" to explicit: start with all checked except this one
                        if (!e.target.checked) {
                          setAllowedTools(["Bash", "Read", "Write", "Edit", "Glob", "Grep", "Skill", "WebFetch", "WebSearch", "Agent", "NotebookEdit"].filter((x) => x !== t));
                        }
                      } else {
                        if (e.target.checked) {
                          setAllowedTools([...allowedTools, t]);
                        } else {
                          setAllowedTools(allowedTools.filter((x) => x !== t));
                        }
                      }
                    }}
                    className="accent-neutral-400"
                  />
                  {t}
                </label>
              ))}
            </div>
          </div>
        )}

        {/* Protected Path Allow (claude / custom) */}
        {(tool === "claude" || tool === "custom") && (
          <div>
            <label className="block text-sm text-neutral-400 mb-2">
              Allow Edits in Protected Paths
              <span className="text-xs text-neutral-600 ml-2">(bypass claude-code guard)</span>
            </label>
            <div className="grid grid-cols-3 gap-1.5">
              {["claude", "git", "husky"].map((p) => (
                <label key={p} className="flex items-center gap-2 px-2 py-1.5 bg-neutral-900 rounded text-xs font-mono cursor-pointer hover:bg-neutral-800">
                  <input
                    type="checkbox"
                    checked={allowProtectedPaths.includes(p)}
                    onChange={(e) => {
                      if (e.target.checked) {
                        setAllowProtectedPaths([...allowProtectedPaths, p]);
                      } else {
                        setAllowProtectedPaths(allowProtectedPaths.filter((x) => x !== p));
                      }
                    }}
                    className="accent-neutral-400"
                  />
                  .{p}
                </label>
              ))}
            </div>
            <p className="text-xs text-neutral-600 mt-1">
              Recent claude-code versions prompt on Edit/Write to .claude, .git, .husky even with bypassPermissions. Check to suppress.
            </p>
          </div>
        )}

        {/* Thinking Mode (llama.cpp only) */}
        {tool === "llama.cpp" && (
          <div>
            <label className="block text-sm text-neutral-400 mb-2">Thinking</label>
            <select
              value={thinkingMode}
              onChange={(e) => setThinkingMode(e.target.value)}
              className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
            >
              <option value="">auto (server default)</option>
              <option value="on">on</option>
              <option value="off">off</option>
            </select>
          </div>
        )}

        {/* File Storage */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">File Storage</label>
          <input
            type="text"
            value={workDir}
            onChange={(e) => setWorkDir(e.target.value)}
            placeholder="(default: agent data dir)"
            className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm font-mono focus:outline-none focus:border-neutral-500"
          />
          <p className="text-xs text-neutral-600 mt-1">Generated files are saved here.</p>
        </div>

        {/* Schedule */}
        <ScheduleEditor
          cronExpr={cronExpr}
          onCronExprChange={setCronExpr}
          timeoutMinutes={timeoutMinutes}
          onTimeoutChange={setTimeoutMinutes}
          resumeIdleMinutes={resumeIdleMinutes}
          onResumeIdleChange={setResumeIdleMinutes}
          tool={tool}
          silentStart={silentStart}
          silentEnd={silentEnd}
          onSilentStartChange={setSilentStart}
          onSilentEndChange={setSilentEnd}
          cronMessage={cronMessage}
          onCronMessageChange={(v) => {
            setCronMessage(v);
            // First keystroke against the default template — flip the
            // flag so Save persists the edit. checkinIsDefault stays
            // true through the no-op case where the user just opened
            // the form and didn't touch the textarea.
            if (checkinIsDefault) setCheckinIsDefault(false);
          }}
          nextCronAt={agent.nextCronAt}
          cronPausedGlobal={agent.cronPausedGlobal}
          scheduleDirty={
            // Schedule-affecting fields differ from the persisted agent —
            // nextCronAt is computed against the saved schedule so showing
            // it during edits would mislead.
            (agent.cronExpr ?? "") !== cronExpr ||
            (agent.silentStart ?? "") !== silentStart ||
            (agent.silentEnd ?? "") !== silentEnd
          }
          onCheckin={handleCheckin}
          // Keep the button disabled while the notice banner is up so the
          // user doesn't fire repeated 409s in quick succession before the
          // server-side run actually gets going.
          checkingIn={checkingIn || checkinNotice !== ""}
        />

        {/* Notify During Silent Hours */}
        <div>
          <label className="flex items-start gap-3 cursor-pointer select-none">
            <input
              type="checkbox"
              checked={notifyDuringSilent}
              onChange={(e) => setNotifyDuringSilent(e.target.checked)}
              className="mt-1 accent-amber-500"
            />
            <div className="flex-1">
              <div className="text-sm text-neutral-300">Receive DM During Silent Hours</div>
              <p className="text-xs text-neutral-600 mt-0.5">
                When enabled, group DM notifications are delivered even
                during silent hours. When disabled, notifications are
                suppressed (messages remain in the transcript).
              </p>
            </div>
          </label>
        </div>

        {/* Privilege.

            POST /api/v1/agents/{id}/privilege is Owner-only. The web UI
            is only ever served to Owner principals (the public listener
            is OwnerOnlyMiddleware on Tailscale; --local requires the
            Owner Bearer for asset delivery), so the toggle has no
            non-Owner code path to worry about and there is no
            client-side role gate. If the asset gating is ever relaxed
            we'd need to hide this control too — keep that in mind when
            touching index.html / the Bearer bootstrap. */}
        <div>
          <label className="flex items-start gap-3 cursor-pointer select-none">
            <input
              type="checkbox"
              checked={privileged}
              disabled={privilegeSaving}
              onChange={(e) => handleTogglePrivileged(e.target.checked)}
              className="mt-1 accent-amber-500"
            />
            <div className="flex-1">
              <div className="text-sm text-neutral-300">Privileged Agent</div>
              <p className="text-xs text-neutral-600 mt-0.5">
                Allow this agent to delete / reset / archive other agents via
                the API. Cannot fork or read other agents&apos; full record.
              </p>
            </div>
          </label>
        </div>

        {error && (
          <div className="p-3 bg-red-950 border border-red-800 rounded-lg text-sm text-red-300">
            {error}
          </div>
        )}
        {success && (
          <div className="p-3 bg-green-950 border border-green-800 rounded-lg text-sm text-green-300">
            Saved
          </div>
        )}
        {checkinNotice && (
          <div className="p-3 bg-amber-950/40 border border-amber-800/60 rounded-lg text-sm text-amber-200">
            {checkinNotice}
          </div>
        )}

        <button
          onClick={handleSave}
          disabled={saving}
          className="w-full py-3 bg-neutral-800 hover:bg-neutral-700 rounded-lg text-sm font-medium disabled:opacity-40"
        >
          {saving ? "Saving..." : "Save Changes"}
        </button>
        </section>

        {/* ── Text-to-Speech ── */}
        <section className="rounded-xl border border-neutral-800 p-5 space-y-4">
          <div>
            <h2 className="text-sm font-semibold text-neutral-200">Text-to-Speech</h2>
            <p className="text-xs text-neutral-500 mt-1">
              Read assistant replies out loud via Gemini TTS. Manual playback per message; auto playback toggled in the chat header.
            </p>
          </div>

          <label className="flex items-center gap-3 text-sm">
            <input
              type="checkbox"
              checked={ttsEnabled}
              onChange={(e) => setTTSEnabled(e.target.checked)}
              className="w-4 h-4"
            />
            Enable TTS for this agent
          </label>

          {ttsEnabled && (
            <div className="space-y-4 pl-7">
              <div>
                <label className="block text-xs font-medium text-neutral-400 mb-1">Model</label>
                <select
                  value={ttsModel}
                  onChange={(e) => setTTSModel(e.target.value)}
                  className="w-full bg-neutral-900 border border-neutral-700 rounded px-3 py-2 text-sm"
                >
                  <option value="">Default ({ttsCapability?.defaults.model ?? "gemini-3.1-flash-tts-preview"})</option>
                  {(ttsCapability?.models ?? []).map((m) => (
                    <option key={m} value={m}>{m}</option>
                  ))}
                </select>
              </div>

              <div>
                <div className="flex items-center justify-between mb-1">
                  <label className="block text-xs font-medium text-neutral-400">Voice</label>
                  {ttsVoice && (
                    <button
                      type="button"
                      onClick={() => playPreview(ttsVoice)}
                      className="text-[11px] text-blue-400 hover:text-blue-300"
                    >
                      {ttsPreviewVoice === ttsVoice ? "▶ Playing..." : "▶ Preview"}
                    </button>
                  )}
                </div>
                <select
                  value={ttsVoice}
                  onChange={(e) => setTTSVoice(e.target.value)}
                  className="w-full bg-neutral-900 border border-neutral-700 rounded px-3 py-2 text-sm"
                >
                  <option value="">
                    Default ({ttsCapability?.defaults.voice ?? "Kore"})
                  </option>
                  {(ttsCapability?.voiceCatalog ?? []).map((v) => (
                    <option key={v.name} value={v.name}>
                      {v.name} ({v.gender || "?"}) — {v.trait}
                    </option>
                  ))}
                </select>
                <p className="text-[11px] text-neutral-600 mt-1">
                  Gender from Cloud TTS Chirp3-HD mapping. Use{" "}
                  <span className="text-neutral-400">Preview</span> to listen.
                </p>
                {ttsPreviewError && (
                  <p className="text-[11px] text-red-400 mt-1">{ttsPreviewError}</p>
                )}
              </div>

              {/* All-voices preview grid — lets the user audition every voice
                  without leaving the settings page. Each row stays compact
                  so 30 voices fit without dominating the form. */}
              <details className="bg-neutral-900/50 border border-neutral-800 rounded">
                <summary className="px-3 py-2 text-xs text-neutral-400 cursor-pointer select-none">
                  Browse all 30 voices
                </summary>
                <div className="grid grid-cols-2 gap-1 p-2 max-h-64 overflow-y-auto">
                  {(ttsCapability?.voiceCatalog ?? []).map((v) => (
                    <button
                      type="button"
                      key={v.name}
                      onClick={() => {
                        setTTSVoice(v.name);
                        playPreview(v.name);
                      }}
                      className={`flex items-center justify-between px-2 py-1.5 rounded text-left text-xs hover:bg-neutral-800 ${
                        ttsVoice === v.name ? "bg-neutral-800 ring-1 ring-blue-500/40" : ""
                      }`}
                    >
                      <span className="truncate flex items-center gap-1.5">
                        {v.gender && (
                          <span
                            className={`inline-block w-3 text-center text-[10px] font-mono rounded-sm ${
                              v.gender === "F"
                                ? "bg-pink-500/20 text-pink-300"
                                : "bg-sky-500/20 text-sky-300"
                            }`}
                          >
                            {v.gender}
                          </span>
                        )}
                        <span className="text-neutral-200">{v.name}</span>
                        <span className="text-neutral-500"> — {v.trait}</span>
                      </span>
                      <span
                        className={`ml-2 text-[10px] ${
                          ttsPreviewVoice === v.name ? "text-blue-400" : "text-neutral-600"
                        }`}
                      >
                        {ttsPreviewVoice === v.name ? "▶" : "▷"}
                      </span>
                    </button>
                  ))}
                </div>
              </details>

              <div>
                <label className="block text-xs font-medium text-neutral-400 mb-1">Style Prompt</label>
                <textarea
                  value={ttsStylePrompt}
                  onChange={(e) => setTTSStylePrompt(e.target.value)}
                  placeholder={ttsCapability?.defaults.stylePrompt ?? "落ち着いた日本語で、淡々と短く読み上げて。"}
                  rows={3}
                  maxLength={500}
                  className="w-full bg-neutral-900 border border-neutral-700 rounded px-3 py-2 text-sm font-mono resize-y"
                />
                <div className="text-[11px] text-neutral-600 mt-1 space-y-1">
                  <p>
                    Free-form prompt prepended to the text. Audio tags such as{" "}
                    <code className="text-neutral-400">[whispers]</code>,{" "}
                    <code className="text-neutral-400">[excited]</code>,{" "}
                    <code className="text-neutral-400">[laughs]</code> can be embedded inline.
                  </p>
                  <p>
                    Reference:{" "}
                    <a
                      href="https://ai.google.dev/gemini-api/docs/speech-generation"
                      target="_blank"
                      rel="noreferrer"
                      className="text-blue-400 hover:text-blue-300"
                    >
                      Gemini TTS prompt guide
                    </a>
                  </p>
                </div>
              </div>

              {ttsCapability && !ttsCapability.ffmpeg && (
                <div className="text-xs text-amber-400/80 bg-amber-950/20 border border-amber-900/40 rounded px-3 py-2">
                  ffmpeg not detected — only WAV output is available. Install ffmpeg to enable Opus/MP3 (much smaller).
                </div>
              )}
            </div>
          )}

          {/* Local Save button so users editing TTS settings don't have
              to scroll up to the main Save Changes button. handleSave
              already includes the TTS payload, so this just re-uses it. */}
          <button
            onClick={handleSave}
            disabled={saving}
            className="w-full py-2.5 bg-neutral-800 hover:bg-neutral-700 rounded-lg text-sm font-medium disabled:opacity-40"
          >
            {saving ? "Saving..." : "Save TTS Settings"}
          </button>
        </section>

        {/* ── Slack Bot ── */}
        <section className="rounded-xl border border-neutral-800 p-5">
          <SlackBotSettings agentId={id!} />
        </section>

        {/* ── Actions ── */}
        <section className="rounded-xl border border-neutral-800 p-5">
          <button
            onClick={openForkDialog}
            className="w-full py-3 bg-neutral-800 hover:bg-neutral-700 rounded-lg text-sm font-medium"
          >
            Fork Agent
          </button>
          <p className="text-xs text-neutral-600 mt-1">
            Create a copy with persona and memory carried over. Slack, notifications, and credentials are not transferred.
          </p>
        </section>

        {/* ── Danger Zone ── */}
        <section className="rounded-xl border border-red-900/30 bg-red-950/10 p-5 space-y-4">
          <h2 className="text-sm font-semibold text-red-400/80">Danger Zone</h2>
          <div>
            <button
              onClick={handleResetSession}
              disabled={resettingSession}
              className="w-full py-3 bg-neutral-800 hover:bg-neutral-700 border border-neutral-700 rounded-lg text-sm font-medium disabled:opacity-40"
            >
              {resettingSession ? "Resetting..." : "Reset CLI Session"}
            </button>
            <p className="text-xs text-neutral-600 mt-1">
              Force a fresh context window. History and memory are kept, but the AI re-reads everything from scratch.
            </p>
          </div>
          <div>
            <label className="block text-xs text-neutral-400 mb-1">
              Truncate memory since
            </label>
            <input
              type="datetime-local"
              value={truncateSince}
              onChange={(e) => setTruncateSince(e.target.value)}
              className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded-lg text-sm text-neutral-200 mb-2"
            />
            <button
              onClick={handleTruncateMemory}
              disabled={truncating || !truncateSince}
              className="w-full py-3 bg-amber-950 hover:bg-amber-900 border border-amber-800 rounded-lg text-sm font-medium text-amber-300 disabled:opacity-40"
            >
              {truncating ? "Truncating..." : "Truncate Memory From This Time"}
            </button>
            <p className="text-xs text-neutral-600 mt-1">
              Drop transcript records, Claude --resume session entries, the grok --resume session (dropped wholesale — see below), and daily diary bullets recorded at or after this instant. Persona, MEMORY.md, project / people / topic notes, archive, and credentials are kept.
            </p>
            {truncateResult && (
              <div className="mt-2 text-xs text-neutral-400 bg-neutral-900/60 border border-neutral-800 rounded-lg p-2 space-y-0.5">
                <div>Threshold: <span className="text-neutral-300">{truncateResult.since}</span></div>
                <div>
                  Transcript: {truncateResult.messagesRemoved} ·
                  {" "}Claude session: {truncateResult.claudeSessionEntriesRemoved} entries / {truncateResult.claudeSessionFilesRemoved} files ·
                  {" "}Grok session: {truncateResult.grokSessionsRemoved ?? 0} sessions / {truncateResult.grokSessionFilesRemoved ?? 0} files ·
                  {" "}Diary: {truncateResult.diaryEntriesRemoved} entries / {truncateResult.diaryFilesRemoved} files
                </div>
              </div>
            )}
          </div>
          <div>
            <button
              onClick={handleResetData}
              disabled={resetting}
              className="w-full py-3 bg-amber-950 hover:bg-amber-900 border border-amber-800 rounded-lg text-sm font-medium text-amber-300 disabled:opacity-40"
            >
              {resetting ? "Resetting..." : "Reset Data"}
            </button>
            <p className="text-xs text-neutral-600 mt-1">
              Clear conversation logs and memory. Settings, persona, avatar, and credentials are kept.
            </p>
          </div>
          <div>
            <button
              onClick={handleArchive}
              disabled={archiving}
              className="w-full py-3 bg-neutral-900 hover:bg-neutral-800 border border-neutral-700 rounded-lg text-sm font-medium text-neutral-300 disabled:opacity-40"
            >
              {archiving ? "Archiving..." : "Archive Agent"}
            </button>
            <p className="text-xs text-neutral-600 mt-1">
              Hide from the main list and stop runtime activity. Data is kept; restore from Settings. Removes the agent from all group DMs (memberships are NOT restored on unarchive).
            </p>
          </div>
          <button
            onClick={handleDelete}
            disabled={deleting}
            className="w-full py-3 bg-red-950 hover:bg-red-900 border border-red-800 rounded-lg text-sm font-medium text-red-300 disabled:opacity-40"
          >
            {deleting ? "Deleting..." : "Delete Agent"}
          </button>
        </section>

        {/* Info */}
        <div className="text-xs text-neutral-600 space-y-1">
          <div>ID: {agent.id}</div>
          <div>Created: {new Date(agent.createdAt).toLocaleString()}</div>
        </div>
      </main>

      {showForkDialog && (
        <div
          role="dialog"
          aria-modal="true"
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 outline-none"
          onClick={(e) => { if (e.target === e.currentTarget && !forking) setShowForkDialog(false); }}
          onKeyDown={(e) => { if (e.key === "Escape" && !forking) setShowForkDialog(false); }}
        >
          <div className="bg-neutral-900 border border-neutral-700 rounded-lg p-5 w-[22rem] shadow-xl">
            <h3 className="text-sm font-medium text-neutral-200 mb-3">Fork agent</h3>
            <label className="block text-xs text-neutral-400 mb-1">Name</label>
            <input
              type="text"
              value={forkName}
              onChange={(e) => setForkName(e.target.value)}
              disabled={forking}
              autoFocus
              className="w-full px-2 py-1.5 text-sm bg-neutral-800 border border-neutral-700 rounded text-neutral-200 focus:outline-none focus:ring-1 focus:ring-blue-500/50 mb-3"
            />
            <label className="flex items-start gap-2 text-sm text-neutral-400 mb-2 cursor-pointer select-none">
              <input
                type="checkbox"
                checked={forkIncludeTranscript}
                onChange={(e) => setForkIncludeTranscript(e.target.checked)}
                disabled={forking}
                className="mt-0.5 rounded border-neutral-600 bg-neutral-800 text-blue-500 focus:ring-blue-500/30"
              />
              <span>
                Include conversation history
                <span className="block text-xs text-neutral-600">Persona and memory are always copied.</span>
              </span>
            </label>
            <p className="text-xs text-neutral-600 mb-4">
              Slack bot, notification sources, and credentials are not transferred.
            </p>
            {forkError && (
              <p className="text-xs text-red-400 mb-3">{forkError}</p>
            )}
            <div className="flex justify-end gap-2">
              <button
                onClick={() => setShowForkDialog(false)}
                disabled={forking}
                className="px-3 py-1.5 text-xs text-neutral-400 hover:text-neutral-200 rounded disabled:opacity-50"
              >
                Cancel
              </button>
              <button
                onClick={handleFork}
                disabled={forking || !forkName.trim()}
                className="px-3 py-1.5 text-xs bg-blue-600 hover:bg-blue-500 text-white rounded disabled:opacity-50"
              >
                {forking ? "Forking…" : "Fork"}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

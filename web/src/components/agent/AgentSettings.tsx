import { useEffect, useRef, useState } from "react";
import { useParams, useNavigate } from "react-router";
import { agentApi, type AgentInfo, type TruncateMemoryResult } from "../../lib/agentApi";
import { useTTSCapability } from "../../hooks/useTTS";
import { ttsApi, pickBestFormat } from "../../lib/ttsApi";
import { errMsg } from "../../lib/utils";
import { AgentAvatar } from "./AgentAvatar";
import { ScheduleEditor } from "./ScheduleEditor";
import { SlackBotSettings } from "./SlackBotSettings";
import { modelsForTool, type EffortLevel } from "../../lib/toolModels";
import { useCustomModels } from "./fields/useCustomModels";
import { PersonaField } from "./fields/PersonaField";
import { ToolPicker } from "./fields/ToolPicker";
import { ModelPicker } from "./fields/ModelPicker";
import { EffortPicker } from "./fields/EffortPicker";
import { StatusField } from "./fields/StatusField";
import { WorkDirInput } from "./fields/WorkDirInput";
import { buildAgentSavePayload } from "./agentSettingsPayload";
import { PageHeader } from "../ui/PageHeader";
import { SectionCard } from "../ui/SectionCard";
import { Field } from "../ui/Field";
import { Input } from "../ui/Input";
import { Textarea } from "../ui/Textarea";
import { Select } from "../ui/Select";
import { Toggle } from "../ui/Toggle";
import { Banner } from "../ui/Banner";
import { Button } from "../ui/Button";

const SECTIONS = [
  { id: "identity", label: "Identity" },
  { id: "model", label: "Model & Tools" },
  { id: "schedule", label: "Schedule" },
  { id: "voice", label: "Voice" },
  { id: "integrations", label: "Integrations" },
  { id: "memory", label: "Memory" },
  { id: "danger", label: "Danger" },
] as const;

// Stable id list so useScrollSpy's effect deps don't churn every render.
const SECTION_IDS = SECTIONS.map((s) => s.id);

/**
 * Scroll-spy: tracks which SectionCard is currently in the reading zone so
 * the section nav can highlight it. Guarded for jsdom (no IntersectionObserver
 * in the test environment) — the nav simply stays on the first section there.
 */
function useScrollSpy(ids: readonly string[], enabled: boolean): string {
  const [active, setActive] = useState(ids[0]);
  useEffect(() => {
    // Wait until the sections are actually in the DOM. The hook runs once
    // while the agent is still loading (component returns null, no section
    // elements exist); `enabled` flips to true on the first content render
    // and re-runs this effect so the observer binds to real targets.
    if (!enabled || typeof IntersectionObserver === "undefined") return;
    const observer = new IntersectionObserver(
      (entries) => {
        const visible = entries
          .filter((e) => e.isIntersecting)
          .sort((a, b) => a.boundingClientRect.top - b.boundingClientRect.top);
        if (visible[0]) setActive(visible[0].target.id);
      },
      { rootMargin: "-96px 0px -60% 0px", threshold: 0 },
    );
    for (const id of ids) {
      const el = document.getElementById(id);
      if (el) observer.observe(el);
    }
    return () => observer.disconnect();
  }, [ids, enabled]);
  return active;
}

function scrollToSection(id: string) {
  document.getElementById(id)?.scrollIntoView({ behavior: "smooth", block: "start" });
}

/** A labeled toggle row: title + description on the left, Toggle on the right. */
function ToggleRow({
  checked,
  onChange,
  disabled,
  title,
  desc,
}: {
  checked: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
  title: string;
  desc: React.ReactNode;
}) {
  return (
    <div className="flex items-start justify-between gap-3">
      <div className="min-w-0">
        <div className="text-[13px] text-ink">{title}</div>
        <p className="mt-0.5 text-[12px] text-ink-faint">{desc}</p>
      </div>
      <Toggle checked={checked} onChange={onChange} disabled={disabled} aria-label={title} />
    </div>
  );
}

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
  const [effort, setEffort] = useState<EffortLevel | "">("");
  const [tool, setTool] = useState("");
  const [customBaseURL, setCustomBaseURL] = useState("http://localhost:8080");
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
  // status.json workspace file — the agent's self-maintained state,
  // rendered by StatusField as a key-value table. statusLoadGen bumps on
  // every server hydration so StatusField (which owns its rows after
  // mount) remounts with fresh content instead of keeping stale rows.
  const [statusContent, setStatusContent] = useState("");
  const [statusIsDefault, setStatusIsDefault] = useState(false);
  const [loadedStatus, setLoadedStatus] = useState("");
  const [statusEtag, setStatusEtag] = useState("");
  const [statusLoadGen, setStatusLoadGen] = useState(0);
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
  const [truncateResult, setTruncateResult] = useState<TruncateMemoryResult | null>(null);
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
      setTTSPreviewError(errMsg(e));
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
      agentApi.getAgentStatus(id),
    ]).then(([agentRes, checkinRes, userCtxRes, statusRes]) => {
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
      setEffort((a.effort || "") as EffortLevel | "");
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
      if (statusRes.status === "fulfilled") {
        const v = statusRes.value.value;
        setStatusContent(v.content);
        setStatusIsDefault(v.isDefault);
        setLoadedStatus(v.content);
        setStatusEtag(statusRes.value.etag ?? v.etag ?? "");
      } else {
        setStatusContent("");
        setStatusIsDefault(true);
        setLoadedStatus("");
        setStatusEtag("");
      }
      setStatusLoadGen((g) => g + 1);
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

  const { needsCustomURL, customModels } = useCustomModels(tool, customBaseURL, setModel);

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
      // Status uses the same snapshot comparison, with one extra guard:
      // an untouched default template must not be persisted, but the
      // StatusField serializer may reformat identical data (indentation,
      // key spacing), so also skip when it still parses to the template.
      const statusDirty = statusContent !== loadedStatus && !statusIsDefault;

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

      // Commit the PATCH result to local state BEFORE the workspace-file
      // PUTs. If a PUT below throws (e.g. status validation 400), the
      // catch skips the tail of this function — without this early
      // commit the local agent etag would stay stale and the next Save
      // would 412 even though the PATCH landed.
      setAgent(updated);
      setPublicProfile(updated.publicProfile ?? "");
      setPublicProfileOverride(updated.publicProfileOverride ?? false);
      // Re-sync cronExpr to whatever the server actually persisted. Without
      // this, picking a Preset chip ("@preset:30") leaves the local state at
      // the sentinel even though the saved row is the expanded form
      // ("7,37 * * * *"); the dirty diff stays true forever.
      setCronExpr(updated.cronExpr ?? "");

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
      if (statusDirty) {
        const wrapped = await agentApi.putAgentStatus(
          id!,
          statusContent,
          statusEtag || undefined,
        );
        const res = wrapped.value;
        setStatusIsDefault(res.isDefault);
        setStatusContent(res.content);
        setLoadedStatus(res.content);
        setStatusEtag(wrapped.etag ?? res.etag ?? "");
        // Remount StatusField so the table re-hydrates from the
        // server-canonical body (the PUT may have normalized types).
        setStatusLoadGen((g) => g + 1);
      }

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
        // The 412 may equally have come from one of the workspace-file
        // PUTs (checkin / user / status — e.g. the agent rewrote its own
        // status mid-edit). Re-pin JUST their etags so the next Save can
        // succeed; bodies stay as the user typed them.
        try {
          const [ck, uc, st] = await Promise.allSettled([
            agentApi.getCheckinFile(id!),
            agentApi.getUserContext(id!),
            agentApi.getAgentStatus(id!),
          ]);
          if (ck.status === "fulfilled") setCheckinEtag(ck.value.etag ?? ck.value.value.etag ?? "");
          if (uc.status === "fulfilled") setUserContextEtag(uc.value.etag ?? uc.value.value.etag ?? "");
          if (st.status === "fulfilled") setStatusEtag(st.value.etag ?? st.value.value.etag ?? "");
        } catch {
          /* swallow — primary error is already shown */
        }
        return;
      }
      setError(errMsg(err));
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
      const msg = errMsg(err);
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
      setError(errMsg(err));
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
      setError(errMsg(err));
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
      setError(errMsg(err));
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
      setError(errMsg(err));
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
      setForkError(errMsg(err));
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
      setError(errMsg(err));
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
      setError(errMsg(err));
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
      setError(errMsg(err));
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
      setError(errMsg(err));
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
      setError(errMsg(err));
    } finally {
      setGeneratingAvatar(false);
    }
  };

  const activeSection = useScrollSpy(SECTION_IDS, !!agent);

  if (!agent) return null;

  return (
    <div className="min-h-full bg-app text-ink">
      <PageHeader
        title="Settings"
        onBack={() => navigate(`/agents/${id}`, { replace: true })}
        below={
          // Mobile section nav: sticky, horizontally scrollable chip row.
          <nav className="flex gap-1.5 overflow-x-auto border-t border-hairline px-4 py-2 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden lg:hidden">
            {SECTIONS.map((s) => (
              <button
                key={s.id}
                type="button"
                onClick={() => scrollToSection(s.id)}
                className={`shrink-0 whitespace-nowrap rounded-full border px-2.5 py-1 font-mono text-[12px] transition-colors ${
                  activeSection === s.id
                    ? "border-copper bg-copper/15 text-copper-bright"
                    : "border-hairline text-ink-dim hover:text-ink"
                }`}
              >
                {s.label}
              </button>
            ))}
          </nav>
        }
      />

      <div className="mx-auto max-w-[900px] px-4 py-6 lg:grid lg:grid-cols-[180px_1fr] lg:gap-8">
        {/* Desktop sticky section rail */}
        <nav className="hidden lg:block">
          <div className="sticky top-24 space-y-0.5">
            {SECTIONS.map((s) => (
              <button
                key={s.id}
                type="button"
                onClick={() => scrollToSection(s.id)}
                className={`block w-full rounded-md px-2.5 py-1.5 text-left font-mono text-[12px] transition-colors ${
                  activeSection === s.id
                    ? "bg-copper/10 text-copper-bright"
                    : "text-ink-dim hover:bg-hover hover:text-ink"
                }`}
              >
                {s.label}
              </button>
            ))}
          </div>
        </nav>

        <main className="min-w-0 space-y-6">
        {/* ── Identity ── */}
        <SectionCard
          id="identity"
          title="Identity"
          description="Name, persona, and how this agent appears to others."
        >
          {/* Avatar */}
          <div className="mb-4 flex items-center gap-4">
            <AgentAvatar agentId={agent.id} name={agent.name} size="xl" cacheBust={avatarToken} />
            <div className="flex flex-wrap gap-2">
              <Button onClick={() => fileRef.current?.click()}>Change Avatar</Button>
              <Button
                onClick={handleGenerateAvatar}
                disabled={generatingAvatar || !persona.trim()}
                className="flex items-center gap-1.5"
              >
                {generatingAvatar ? (
                  <><span className="animate-spin">↻</span> Generating...</>
                ) : (
                  <>✨ Generate</>
                )}
              </Button>
              <input
                ref={fileRef}
                type="file"
                accept="image/*"
                onChange={handleAvatarUpload}
                className="hidden"
              />
            </div>
          </div>

          <div className="space-y-4">
            <Field label="Name">
              <Input value={name} onChange={(e) => setName(e.target.value)} />
            </Field>

            <PersonaField
              persona={persona}
              setPersona={setPersona}
              textareaRows={6}
              personaPrompt={personaPrompt}
              setPersonaPrompt={setPersonaPrompt}
              promptPlaceholder="e.g. もっと毒舌にして"
              busy={generatingPersona}
              spinning={generatingPersona}
              onGenerate={handleGeneratePersona}
            />

            {/* User Context (user.md) */}
            <Field
              label="User Context"
              help={
                <>
                  Notes about the people this agent works with — name, timezone,
                  communication preferences, etc. Injected into the system prompt as
                  data (head/tail truncated above 1500 chars).{" "}
                  {userContextIsDefault && "Template — not yet saved."}
                </>
              }
            >
              <Textarea
                mono
                value={userContext}
                onChange={(e) => {
                  setUserContext(e.target.value);
                  // First keystroke off the default template — clear the
                  // default flag so Save persists the edit instead of
                  // treating it as a no-op against the in-memory template.
                  if (userContextIsDefault) setUserContextIsDefault(false);
                }}
                rows={6}
              />
            </Field>

            {/* Status (status.json) */}
            <Field
              label="Status"
              help={
                <>
                  The agent&apos;s self-maintained state (mood, energy, sleepiness,
                  ...) injected into its system prompt. The agent updates this on
                  its own as its state drifts; edits here override it.{" "}
                  {statusIsDefault && "Template — not yet saved."}
                </>
              }
            >
              <StatusField
                key={`status-${statusLoadGen}`}
                initialContent={statusContent}
                onChange={(content) => {
                  setStatusContent(content);
                  // First edit off the default template — clear the flag
                  // so Save persists the edit (same contract as user.md).
                  if (statusIsDefault) setStatusIsDefault(false);
                }}
              />
            </Field>

            {/* Public Profile */}
            <Field
              label="Public Profile"
              action={
                <label className="flex cursor-pointer items-center gap-1.5 text-[12px] text-ink-dim">
                  <input
                    type="checkbox"
                    checked={publicProfileOverride}
                    onChange={(e) => setPublicProfileOverride(e.target.checked)}
                    className="h-4 w-4 rounded border-hairline bg-raised accent-[color:var(--color-copper)]"
                  />
                  Override
                </label>
              }
              help={
                publicProfileOverride
                  ? "Manual override — won't be replaced when persona changes."
                  : "Auto-generated from persona. Visible to other agents via directory."
              }
            >
              <Textarea
                value={publicProfile}
                onChange={(e) => setPublicProfile(e.target.value)}
                rows={2}
                disabled={!publicProfileOverride}
                placeholder={publicProfileOverride ? "Enter custom public profile" : "Auto-generated from persona"}
                className={!publicProfileOverride ? "resize-none opacity-60" : "resize-none"}
              />
            </Field>
          </div>
        </SectionCard>

        {/* ── Model & Tools ── */}
        <SectionCard
          id="model"
          title="Model & Tools"
          description="Backend, model, and capability permissions."
        >
          <div className="space-y-4">
            <ToolPicker
              tool={tool}
              setTool={setTool}
              setModel={setModel}
              effort={effort}
              setEffort={setEffort}
            />

            <ModelPicker
              model={model}
              setModel={setModel}
              effort={effort}
              setEffort={setEffort}
              models={needsCustomURL ? customModels : modelsForTool(tool)}
            />

            <EffortPicker tool={tool} effort={effort} setEffort={setEffort} model={model} />

            {needsCustomURL && (
              <Field label="Custom Base URL" help="Anthropic Messages API compatible endpoint">
                <Input
                  mono
                  value={customBaseURL}
                  onChange={(e) => setCustomBaseURL(e.target.value)}
                  placeholder="http://localhost:8080"
                />
              </Field>
            )}

            {/* Allowed Tools (custom only) */}
            {tool === "custom" && (
              <Field
                label={
                  <>
                    Allowed Tools
                    <span className="ml-2 text-ink-faint">(empty = all)</span>
                  </>
                }
              >
                <div className="grid grid-cols-2 gap-1.5">
                  {["Bash", "Read", "Write", "Edit", "Glob", "Grep", "Skill", "WebFetch", "WebSearch", "Agent", "NotebookEdit"].map((t) => (
                    <label key={t} className="flex cursor-pointer items-center gap-2 rounded-lg border border-hairline bg-raised px-2 py-1.5 font-mono text-[12px] text-ink-dim hover:bg-hover">
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
                        className="h-4 w-4 accent-[color:var(--color-copper)]"
                      />
                      {t}
                    </label>
                  ))}
                </div>
              </Field>
            )}

            {/* Protected Path Allow (claude / custom) */}
            {(tool === "claude" || tool === "custom") && (
              <Field
                label={
                  <>
                    Allow Edits in Protected Paths
                    <span className="ml-2 text-ink-faint">(bypass claude-code guard)</span>
                  </>
                }
                help="Recent claude-code versions prompt on Edit/Write to .claude, .git, .husky even with bypassPermissions. Check to suppress."
              >
                <div className="grid grid-cols-3 gap-1.5">
                  {["claude", "git", "husky"].map((p) => (
                    <label key={p} className="flex cursor-pointer items-center gap-2 rounded-lg border border-hairline bg-raised px-2 py-1.5 font-mono text-[12px] text-ink-dim hover:bg-hover">
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
                        className="h-4 w-4 accent-[color:var(--color-copper)]"
                      />
                      .{p}
                    </label>
                  ))}
                </div>
              </Field>
            )}

            {/* Thinking Mode (llama.cpp only) */}
            {tool === "llama.cpp" && (
              <Field label="Thinking">
                <Select value={thinkingMode} onChange={(e) => setThinkingMode(e.target.value)}>
                  <option value="">auto (server default)</option>
                  <option value="on">on</option>
                  <option value="off">off</option>
                </Select>
              </Field>
            )}

            {/* File Storage */}
            <WorkDirInput workDir={workDir} setWorkDir={setWorkDir} />

            {/* Privilege.

                POST /api/v1/agents/{id}/privilege is Owner-only. The web UI
                is only ever served to Owner principals (the public listener
                is OwnerOnlyMiddleware on Tailscale; --local requires the
                Owner Bearer for asset delivery), so the toggle has no
                non-Owner code path to worry about and there is no
                client-side role gate. If the asset gating is ever relaxed
                we'd need to hide this control too — keep that in mind when
                touching index.html / the Bearer bootstrap. */}
            <ToggleRow
              checked={privileged}
              disabled={privilegeSaving}
              onChange={handleTogglePrivileged}
              title="Privileged Agent"
              desc="Allow this agent to delete / reset / archive other agents via the API. Cannot fork or read other agents' full record."
            />
          </div>
        </SectionCard>

        {/* ── Schedule ── */}
        <SectionCard
          id="schedule"
          title="Schedule"
          description="When this agent runs on its own, and when it stays quiet."
        >
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

          <div className="mt-4 border-t border-hairline pt-4">
            <ToggleRow
              checked={notifyDuringSilent}
              onChange={setNotifyDuringSilent}
              title="Receive DM During Silent Hours"
              desc="When enabled, group DM notifications are delivered even during silent hours. When disabled, notifications are suppressed (messages remain in the transcript)."
            />
          </div>
        </SectionCard>

        {/* ── Voice ── */}
        <SectionCard
          id="voice"
          title="Voice"
          description="Read assistant replies out loud via Gemini TTS. Manual playback per message; auto playback toggled in the chat header."
          action={<Toggle checked={ttsEnabled} onChange={setTTSEnabled} aria-label="Enable TTS" />}
        >
          {ttsEnabled && (
            <div className="space-y-4">
              <Field label="Model">
                <Select value={ttsModel} onChange={(e) => setTTSModel(e.target.value)}>
                  <option value="">Default ({ttsCapability?.defaults.model ?? "gemini-3.1-flash-tts-preview"})</option>
                  {(ttsCapability?.models ?? []).map((m) => (
                    <option key={m} value={m}>{m}</option>
                  ))}
                </Select>
              </Field>

              <Field
                label="Voice"
                action={
                  ttsVoice ? (
                    <button
                      type="button"
                      onClick={() => playPreview(ttsVoice)}
                      className="text-[12px] text-copper transition-colors hover:text-copper-bright"
                    >
                      {ttsPreviewVoice === ttsVoice ? "▶ Playing..." : "▶ Preview"}
                    </button>
                  ) : undefined
                }
                help={
                  <>
                    Gender from Cloud TTS Chirp3-HD mapping. Use{" "}
                    <span className="text-ink-dim">Preview</span> to listen.
                  </>
                }
                error={ttsPreviewError || undefined}
              >
                <Select value={ttsVoice} onChange={(e) => setTTSVoice(e.target.value)}>
                  <option value="">
                    Default ({ttsCapability?.defaults.voice ?? "Kore"})
                  </option>
                  {(ttsCapability?.voiceCatalog ?? []).map((v) => (
                    <option key={v.name} value={v.name}>
                      {v.name} ({v.gender || "?"}) — {v.trait}
                    </option>
                  ))}
                </Select>
              </Field>

              {/* All-voices preview grid — lets the user audition every voice
                  without leaving the settings page. Each row stays compact
                  so 30 voices fit without dominating the form. */}
              <details className="overflow-hidden rounded-[10px] border border-hairline bg-raised">
                <summary className="cursor-pointer select-none px-3 py-2 text-[12px] text-ink-dim">
                  Browse all 30 voices
                </summary>
                <div className="grid max-h-64 grid-cols-2 gap-1 overflow-y-auto p-2">
                  {(ttsCapability?.voiceCatalog ?? []).map((v) => (
                    <button
                      type="button"
                      key={v.name}
                      onClick={() => {
                        setTTSVoice(v.name);
                        playPreview(v.name);
                      }}
                      className={`flex items-center justify-between rounded-md px-2 py-1.5 text-left text-[12px] hover:bg-hover ${
                        ttsVoice === v.name ? "bg-hover ring-1 ring-copper/40" : ""
                      }`}
                    >
                      <span className="flex items-center gap-1.5 truncate">
                        {v.gender && (
                          <span
                            className={`inline-block w-3 rounded-sm text-center font-mono text-[10px] ${
                              v.gender === "F"
                                ? "bg-lamp-err/20 text-lamp-err"
                                : "bg-copper/20 text-copper-bright"
                            }`}
                          >
                            {v.gender}
                          </span>
                        )}
                        <span className="text-ink">{v.name}</span>
                        <span className="text-ink-faint"> — {v.trait}</span>
                      </span>
                      <span
                        className={`ml-2 text-[10px] ${
                          ttsPreviewVoice === v.name ? "text-copper" : "text-ink-faint"
                        }`}
                      >
                        {ttsPreviewVoice === v.name ? "▶" : "▷"}
                      </span>
                    </button>
                  ))}
                </div>
              </details>

              <Field
                label="Style Prompt"
                help={
                  <span className="space-y-1">
                    <span className="block">
                      Free-form prompt prepended to the text. Audio tags such as{" "}
                      <code className="text-ink-dim">[whispers]</code>,{" "}
                      <code className="text-ink-dim">[excited]</code>,{" "}
                      <code className="text-ink-dim">[laughs]</code> can be embedded inline.
                    </span>
                    <span className="block">
                      Reference:{" "}
                      <a
                        href="https://ai.google.dev/gemini-api/docs/speech-generation"
                        target="_blank"
                        rel="noreferrer"
                        className="text-copper hover:text-copper-bright"
                      >
                        Gemini TTS prompt guide
                      </a>
                    </span>
                  </span>
                }
              >
                <Textarea
                  mono
                  value={ttsStylePrompt}
                  onChange={(e) => setTTSStylePrompt(e.target.value)}
                  placeholder={ttsCapability?.defaults.stylePrompt ?? "落ち着いた日本語で、淡々と短く読み上げて。"}
                  rows={3}
                  maxLength={500}
                />
              </Field>

              {ttsCapability && !ttsCapability.ffmpeg && (
                <Banner tone="warn">
                  ffmpeg not detected — only WAV output is available. Install ffmpeg to enable Opus/MP3 (much smaller).
                </Banner>
              )}
            </div>
          )}

          {/* Local Save button so users editing TTS settings don't have
              to scroll up to the main Save Changes button. handleSave
              already includes the TTS payload, so this just re-uses it. */}
          <Button
            variant="primary"
            onClick={handleSave}
            disabled={saving}
            className="mt-4 w-full"
          >
            {saving ? "Saving..." : "Save TTS Settings"}
          </Button>
        </SectionCard>

        {/* ── Integrations ── */}
        <SectionCard id="integrations" title="Integrations">
          <SlackBotSettings agentId={id!} />
        </SectionCard>

        {/* ── Memory ── */}
        <SectionCard
          id="memory"
          title="Memory"
          description="Trim stored history. Persona, MEMORY.md, notes, and credentials are always kept."
        >
          <div className="space-y-4">
            <Field
              label="Truncate memory since"
              help="Drop transcript records, Claude --resume session entries, the grok --resume session (dropped wholesale), and daily diary bullets recorded at or after this instant. Persona, MEMORY.md, project / people / topic notes, archive, and credentials are kept."
            >
              <Input
                type="datetime-local"
                value={truncateSince}
                onChange={(e) => setTruncateSince(e.target.value)}
                className="[color-scheme:dark]"
              />
            </Field>
            <Button
              variant="danger"
              onClick={handleTruncateMemory}
              disabled={truncating || !truncateSince}
              className="w-full"
            >
              {truncating ? "Truncating..." : "Truncate Memory From This Time"}
            </Button>
            {truncateResult && (
              <div className="space-y-0.5 rounded-[10px] border border-hairline bg-raised p-2 text-[12px] text-ink-dim">
                <div>Threshold: <span className="text-ink">{truncateResult.since}</span></div>
                <div>
                  Transcript: {truncateResult.messagesRemoved} ·
                  {" "}Claude session: {truncateResult.claudeSessionEntriesRemoved} entries / {truncateResult.claudeSessionFilesRemoved} files ·
                  {" "}Grok session: {truncateResult.grokSessionsRemoved ?? 0} sessions / {truncateResult.grokSessionFilesRemoved ?? 0} files ·
                  {" "}Diary: {truncateResult.diaryEntriesRemoved} entries / {truncateResult.diaryFilesRemoved} files
                </div>
              </div>
            )}
            <div>
              <Button
                variant="danger"
                onClick={handleResetData}
                disabled={resetting}
                className="w-full"
              >
                {resetting ? "Resetting..." : "Reset Data"}
              </Button>
              <p className="mt-1.5 text-[12px] text-ink-faint">
                Clear conversation logs and memory. Settings, persona, avatar, and credentials are kept.
              </p>
            </div>
          </div>
        </SectionCard>

        {/* Banners + primary save (covers every form field via handleSave). */}
        <div className="space-y-3">
          {error && <Banner tone="error">{error}</Banner>}
          {success && <Banner tone="success">Saved</Banner>}
          {checkinNotice && <Banner tone="warn">{checkinNotice}</Banner>}
          <Button
            variant="primary"
            onClick={handleSave}
            disabled={saving}
            className="w-full py-3"
          >
            {saving ? "Saving..." : "Save Changes"}
          </Button>
        </div>

        {/* ── Danger Zone ── */}
        <SectionCard id="danger" title="Danger Zone" danger>
          <div className="space-y-4">
            <div>
              <Button
                onClick={handleResetSession}
                disabled={resettingSession}
                className="w-full"
              >
                {resettingSession ? "Resetting..." : "Reset CLI Session"}
              </Button>
              <p className="mt-1.5 text-[12px] text-ink-faint">
                Force a fresh context window. History and memory are kept, but the AI re-reads everything from scratch.
              </p>
            </div>
            <div>
              <Button onClick={openForkDialog} className="w-full">
                Fork Agent
              </Button>
              <p className="mt-1.5 text-[12px] text-ink-faint">
                Create a copy with persona and memory carried over. Slack, notifications, and credentials are not transferred.
              </p>
            </div>
            <div>
              <Button
                onClick={handleArchive}
                disabled={archiving}
                className="w-full"
              >
                {archiving ? "Archiving..." : "Archive Agent"}
              </Button>
              <p className="mt-1.5 text-[12px] text-ink-faint">
                Hide from the main list and stop runtime activity. Data is kept; restore from Settings. Removes the agent from all group DMs (memberships are NOT restored on unarchive).
              </p>
            </div>
            <Button
              variant="danger"
              onClick={handleDelete}
              disabled={deleting}
              className="w-full border border-lamp-err/40"
            >
              {deleting ? "Deleting..." : "Delete Agent"}
            </Button>
          </div>
        </SectionCard>

        {/* Info */}
        <div className="space-y-1 text-[12px] text-ink-faint">
          <div>ID: {agent.id}</div>
          <div>Created: {new Date(agent.createdAt).toLocaleString()}</div>
        </div>
        </main>
      </div>

      {showForkDialog && (
        <div
          role="dialog"
          aria-modal="true"
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 outline-none"
          onClick={(e) => { if (e.target === e.currentTarget && !forking) setShowForkDialog(false); }}
          onKeyDown={(e) => { if (e.key === "Escape" && !forking) setShowForkDialog(false); }}
        >
          <div className="w-[22rem] max-w-[calc(100vw-2rem)] rounded-[10px] border border-hairline bg-raised p-5 shadow-xl shadow-black/50">
            <h3 className="mb-3 text-[14px] font-semibold text-ink">Fork agent</h3>
            <Field label="Name" className="mb-3">
              <Input
                value={forkName}
                onChange={(e) => setForkName(e.target.value)}
                disabled={forking}
                autoFocus
              />
            </Field>
            <label className="mb-2 flex cursor-pointer select-none items-start gap-2 text-[13px] text-ink-dim">
              <input
                type="checkbox"
                checked={forkIncludeTranscript}
                onChange={(e) => setForkIncludeTranscript(e.target.checked)}
                disabled={forking}
                className="mt-0.5 h-4 w-4 rounded border-hairline bg-raised accent-[color:var(--color-copper)]"
              />
              <span>
                Include conversation history
                <span className="block text-[12px] text-ink-faint">Persona and memory are always copied.</span>
              </span>
            </label>
            <p className="mb-4 text-[12px] text-ink-faint">
              Slack bot, notification sources, and credentials are not transferred.
            </p>
            {forkError && (
              <div className="mb-3">
                <Banner tone="error">{forkError}</Banner>
              </div>
            )}
            <div className="flex justify-end gap-2">
              <Button onClick={() => setShowForkDialog(false)} disabled={forking}>
                Cancel
              </Button>
              <Button
                variant="primary"
                onClick={handleFork}
                disabled={forking || !forkName.trim()}
              >
                {forking ? "Forking…" : "Fork"}
              </Button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

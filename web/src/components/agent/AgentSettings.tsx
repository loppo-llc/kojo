import { useEffect, useRef, useState } from "react";
import { useParams, useNavigate, useLocation } from "react-router";
import {
  agentApi,
  CONTEXT_INJECTION_KEYS,
  type AgentInfo,
  type ContextInjectionKey,
  type TruncateMemoryResult,
} from "../../lib/agentApi";
import { useTTSCapability } from "../../hooks/useTTS";
import { ttsApi, pickBestFormat } from "../../lib/ttsApi";
import { errMsg } from "../../lib/utils";
import { useT } from "../../lib/i18n";
import { AgentAvatar } from "./AgentAvatar";
import { ScheduleEditor } from "./ScheduleEditor";
import { SlackBotSettings } from "./SlackBotSettings";
import { modelsForTool, supportsEffort, type EffortLevel } from "../../lib/toolModels";
import { useCustomModels } from "./fields/useCustomModels";
import { PersonaField } from "./fields/PersonaField";
import { ToolPicker } from "./fields/ToolPicker";
import { ModelPicker } from "./fields/ModelPicker";
import { EffortPicker } from "./fields/EffortPicker";
import { StatusField } from "./fields/StatusField";
import { WorkDirInput } from "./fields/WorkDirInput";
import { buildAgentSavePayload, needsCustomURLFor } from "./agentSettingsPayload";
import { PageHeader } from "../ui/PageHeader";
import { SectionCard } from "../ui/SectionCard";
import { Field } from "../ui/Field";
import { Input } from "../ui/Input";
import { Textarea } from "../ui/Textarea";
import { Select } from "../ui/Select";
import { Toggle } from "../ui/Toggle";
import { Banner } from "../ui/Banner";
import { Button } from "../ui/Button";

// Section ids (also i18n keys under "settings.sec.<id>"). Stable list so
// useScrollSpy's effect deps don't churn every render; labels are resolved
// through t() at render time so they follow the active locale.
const SECTION_IDS = [
  "identity",
  "injections",
  "model",
  "schedule",
  "voice",
  "integrations",
  "memory",
  "danger",
] as const;

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
  const t = useT();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const location = useLocation();
  const navigateRef = useRef(navigate);
  navigateRef.current = navigate;
  const [agent, setAgent] = useState<AgentInfo | null>(null);
  const [name, setName] = useState("");
  const [persona, setPersona] = useState("");
  const [publicProfile, setPublicProfile] = useState("");
  const [publicProfileOverride, setPublicProfileOverride] = useState(false);
  const [model, setModel] = useState("");
  const [effort, setEffort] = useState<EffortLevel | "">("");
  // Per-turn dynamic effort classifier. Absent on the server = enabled
  // (opt-out feature); the Effort selector becomes the ceiling/fallback.
  const [autoEffort, setAutoEffort] = useState(true);
  const [tool, setTool] = useState("");
  const [customBaseURL, setCustomBaseURL] = useState("http://localhost:8080");
  const [thinkingMode, setThinkingMode] = useState("");
  const [workDir, setWorkDir] = useState("");
  const [cronExpr, setCronExpr] = useState("");
  const [timeoutMinutes, setTimeoutMinutes] = useState(-1);
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
  // anchor.md workspace file — the agent's optional persona anchor. Same
  // default-vs-saved tracking / etag threading as user.md above so an
  // unedited empty template doesn't get persisted on Save.
  const [anchorContent, setAnchorContent] = useState("");
  const [anchorIsDefault, setAnchorIsDefault] = useState(false);
  const [loadedAnchor, setLoadedAnchor] = useState("");
  const [anchorEtag, setAnchorEtag] = useState("");
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
  // Context-injection checklist. Checked = enabled = NOT present in this
  // array. Absent/empty on the server means "everything enabled", so an
  // untouched agent loads with this as [].
  const [disabledInjections, setDisabledInjections] = useState<string[]>([]);
  const [allowProtectedPaths, setAllowProtectedPaths] = useState<string[]>([]);
  // TTS settings
  const [ttsEnabled, setTTSEnabled] = useState(false);
  const [ttsProvider, setTTSProvider] = useState<"gemini" | "grok">("gemini");
  const [ttsModel, setTTSModel] = useState("");
  const [ttsVoice, setTTSVoice] = useState("");
  const [ttsStylePrompt, setTTSStylePrompt] = useState("");
  const [ttsPreviewVoice, setTTSPreviewVoice] = useState<string | null>(null);
  const [ttsPreviewError, setTTSPreviewError] = useState("");
  const ttsCapability = useTTSCapability();
  const ttsPreviewAudioRef = useRef<HTMLAudioElement | null>(null);

  // playPreview synthesizes a fixed sample line for the chosen voice
  // browseVoices normalizes the current provider's catalog into a single
  // shape for the "browse all voices" grid (name, display label, F/M
  // gender, optional trait).
  const browseVoices =
    ttsProvider === "grok"
      ? (ttsCapability?.grokVoiceCatalog ?? []).map((v) => ({
          name: v.name,
          display: v.label,
          gender: v.gender === "female" ? "F" : v.gender === "male" ? "M" : "",
          trait: "",
        }))
      : (ttsCapability?.voiceCatalog ?? []).map((v) => ({
          name: v.name,
          display: v.name,
          gender: v.gender ?? "",
          trait: v.trait,
        }));

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
        provider: ttsProvider,
        model: ttsProvider === "grok" ? undefined : ttsModel || undefined,
        stylePrompt:
          ttsProvider === "grok" ? undefined : ttsStylePrompt.trim() || undefined,
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
          setTTSPreviewError(t("settings.playbackError"));
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

  // Sets every agent-row-backed form field from a server row, applying the
  // same normalizations everywhere (initial load, post-save re-sync,
  // discard) so the dirty computation always compares like with like — the
  // server trims name/persona/workDir/etc. on save, and re-hydrating from
  // its response is what lets the sticky bar settle after a save.
  const hydrateFromAgent = (a: AgentInfo) => {
    setName(a.name);
    setPersona(a.persona);
    setPublicProfile(a.publicProfile ?? "");
    setPublicProfileOverride(a.publicProfileOverride ?? false);
    setModel(a.model);
    setEffort((a.effort || "") as EffortLevel | "");
    setAutoEffort(a.autoEffort ?? true);
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
    setDisabledInjections(a.disabledInjections ?? []);
    setAllowProtectedPaths(a.allowProtectedPaths ?? []);
    setPrivileged(a.privileged ?? false);
    setTTSEnabled(a.tts?.enabled ?? false);
    setTTSProvider(a.tts?.provider === "grok" ? "grok" : "gemini");
    setTTSModel(a.tts?.model ?? "");
    setTTSVoice(a.tts?.voice ?? "");
    setTTSStylePrompt(a.tts?.stylePrompt ?? "");
  };
  // Ref so the load effect (deps: [id]) can call the latest hydrate
  // without listing it as a dependency.
  const hydrateRef = useRef(hydrateFromAgent);
  hydrateRef.current = hydrateFromAgent;

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
      agentApi.getAgentAnchor(id),
    ]).then(([agentRes, checkinRes, userCtxRes, statusRes, anchorRes]) => {
      if (agentRes.status !== "fulfilled") {
        navigateRef.current("/");
        return;
      }
      const a = agentRes.value;
      setAgent(a);
      hydrateRef.current(a);
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
      if (anchorRes.status === "fulfilled") {
        const v = anchorRes.value.value;
        setAnchorContent(v.content);
        setAnchorIsDefault(v.isDefault);
        setLoadedAnchor(v.content);
        setAnchorEtag(anchorRes.value.etag ?? v.etag ?? "");
      } else {
        setAnchorContent("");
        setAnchorIsDefault(true);
        setLoadedAnchor("");
        setAnchorEtag("");
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
      const anchorDirty = anchorContent !== loadedAnchor;

      const updated = await agentApi.update(
        id!,
        buildAgentSavePayload({
          name,
          persona,
          publicProfile,
          publicProfileOverride,
          model,
          effort,
          autoEffort,
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
          disabledInjections,
          tts: {
            enabled: ttsEnabled,
            provider: ttsProvider,
            model: ttsProvider === "grok" ? "" : ttsModel,
            voice: ttsVoice,
            stylePrompt: ttsProvider === "grok" ? "" : ttsStylePrompt,
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
      // Re-sync every field to whatever the server actually persisted.
      // The server normalizes on save (trims name/persona/workDir, expands
      // cron preset chips like "@preset:30" to "7,37 * * * *", clears grok
      // TTS model/stylePrompt); without this the dirty diff would compare
      // raw local state against the normalized row and stay true forever.
      hydrateFromAgent(updated);

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
      if (anchorDirty) {
        const wrapped = await agentApi.putAgentAnchor(
          id!,
          anchorContent,
          anchorEtag || undefined,
        );
        const res = wrapped.value;
        setAnchorIsDefault(res.isDefault);
        setAnchorContent(res.content);
        setLoadedAnchor(res.content);
        setAnchorEtag(wrapped.etag ?? res.etag ?? "");
      }

      setSuccess(true);
      setTimeout(() => setSuccess(false), 2000);
    } catch (err) {
      // PreconditionFailedError is the etag-mismatch 412. Re-fetch the
      // agent so the form rebases onto the server's current row before
      // the user re-applies their edit (otherwise the next Save would
      // 412 again with the same stale etag they started with).
      if (err instanceof Error && err.name === "PreconditionFailedError") {
        setError(t("settings.saveConflict"));
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
        // status mid-edit). Re-pin their etags AND loaded snapshots so the
        // next Save can succeed and Discard reverts to the server's current
        // bodies (not the stale ones this form loaded with); the textarea
        // contents — and the isDefault flags guarding untouched templates —
        // stay as the user left them.
        try {
          const [ck, uc, st, an] = await Promise.allSettled([
            agentApi.getCheckinFile(id!),
            agentApi.getUserContext(id!),
            agentApi.getAgentStatus(id!),
            agentApi.getAgentAnchor(id!),
          ]);
          // For each file: re-pin etag + snapshot, and if the user hadn't
          // edited it (field still equals the old snapshot) fast-forward the
          // field too. Otherwise an untouched textarea would read as dirty
          // against the fresh snapshot and the next Save would overwrite the
          // concurrent update with stale content under the new etag.
          if (ck.status === "fulfilled") {
            const fresh = ck.value.value.content;
            if (cronMessage === loadedCheckin) setCronMessage(fresh);
            setLoadedCheckin(fresh);
            setCheckinEtag(ck.value.etag ?? ck.value.value.etag ?? "");
          }
          if (uc.status === "fulfilled") {
            const fresh = uc.value.value.content;
            if (userContext === loadedUser) setUserContext(fresh);
            setLoadedUser(fresh);
            setUserContextEtag(uc.value.etag ?? uc.value.value.etag ?? "");
          }
          if (st.status === "fulfilled") {
            const fresh = st.value.value.content;
            if (statusContent === loadedStatus || statusIsDefault) {
              setStatusContent(fresh);
              // Remount StatusField so its rows rebuild from the fresh body.
              setStatusLoadGen((g) => g + 1);
            }
            setLoadedStatus(fresh);
            setStatusEtag(st.value.etag ?? st.value.value.etag ?? "");
          }
          if (an.status === "fulfilled") {
            const fresh = an.value.value.content;
            if (anchorContent === loadedAnchor) setAnchorContent(fresh);
            setLoadedAnchor(fresh);
            setAnchorEtag(an.value.etag ?? an.value.value.etag ?? "");
          }
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
      setCheckinNotice(t("settings.checkinSaveFirst"));
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
      setCheckinNotice(t("settings.checkinStarted"));
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
        setCheckinNotice(t("settings.checkinSkipped"));
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
    if (!confirm(t("settings.resetSessionConfirm"))) return;
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
      setError(t("settings.pickDate"));
      return;
    }
    if (!confirm(t("settings.truncateConfirm", { iso }))) {
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
    if (!confirm(t("settings.resetDataConfirm"))) return;
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
      setForkError(t("settings.nameRequired"));
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
    if (!confirm(t("settings.deleteConfirm"))) return;
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
    if (!confirm(t("settings.archiveConfirm"))) return;
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

  const toggleInjection = (key: ContextInjectionKey, enabled: boolean) => {
    setDisabledInjections((prev) =>
      enabled ? prev.filter((k) => k !== key) : prev.includes(key) ? prev : [...prev, key],
    );
  };

  const activeSection = useScrollSpy(SECTION_IDS, !!agent);

  // Order-insensitive compare for the checkbox-driven string arrays —
  // unchecking and re-checking an entry must not read as dirty just
  // because it moved to the end of the array.
  const setEq = (a: string[], b: string[]) => {
    if (a.length !== b.length) return false;
    const sa = [...a].sort();
    const sb = [...b].sort();
    return sa.every((v, i) => v === sb[i]);
  };

  // Dirty when a field the save payload actually sends differs from the
  // persisted agent row, or a workspace-file textarea differs from its
  // loaded snapshot. Drives the sticky save bar. Mirrors
  // buildAgentSavePayload: trimmed strings compare trimmed, and fields the
  // payload omits for the current tool / provider (effort, customBaseURL,
  // thinkingMode, allowedTools, allowProtectedPaths, publicProfile body,
  // gemini-only TTS fields) are skipped so hidden inputs can't leave the
  // form un-clearably dirty.
  const dirty =
    !!agent &&
    (name.trim() !== agent.name ||
      persona.trim() !== agent.persona ||
      (publicProfileOverride && publicProfile.trim() !== (agent.publicProfile ?? "")) ||
      publicProfileOverride !== (agent.publicProfileOverride ?? false) ||
      model.trim() !== agent.model ||
      (supportsEffort(tool) && effort !== ((agent.effort || "") as EffortLevel | "")) ||
      autoEffort !== (agent.autoEffort ?? true) ||
      tool.trim() !== agent.tool ||
      (needsCustomURLFor(tool) &&
        customBaseURL.trim() !== (agent.customBaseURL ?? "http://localhost:8080")) ||
      (tool === "llama.cpp" && thinkingMode !== (agent.thinkingMode ?? "")) ||
      workDir.trim() !== (agent.workDir ?? "") ||
      cronExpr !== (agent.cronExpr ?? "") ||
      timeoutMinutes !== (agent.timeoutMinutes || 10) ||
      resumeIdleMinutes !== (agent.resumeIdleMinutes ?? 0) ||
      silentStart !== (agent.silentStart ?? "") ||
      silentEnd !== (agent.silentEnd ?? "") ||
      notifyDuringSilent !== (agent.notifyDuringSilent ?? true) ||
      (tool === "custom" && !setEq(allowedTools, agent.allowedTools ?? [])) ||
      ((tool === "claude" || tool === "custom") &&
        !setEq(allowProtectedPaths, agent.allowProtectedPaths ?? [])) ||
      !setEq(disabledInjections, agent.disabledInjections ?? []) ||
      ttsEnabled !== (agent.tts?.enabled ?? false) ||
      ttsProvider !== (agent.tts?.provider === "grok" ? "grok" : "gemini") ||
      (ttsProvider === "gemini" && ttsModel !== (agent.tts?.model ?? "")) ||
      ttsVoice !== (agent.tts?.voice ?? "") ||
      (ttsProvider === "gemini" && ttsStylePrompt.trim() !== (agent.tts?.stylePrompt ?? "")) ||
      cronMessage !== loadedCheckin ||
      userContext !== loadedUser ||
      (statusContent !== loadedStatus && !statusIsDefault) ||
      anchorContent !== loadedAnchor);

  // Rehydrate every form field from the persisted agent row / loaded
  // workspace-file snapshots, dropping unsaved edits.
  const handleDiscard = () => {
    if (!agent) return;
    hydrateFromAgent(agent);
    setCronMessage(loadedCheckin);
    setUserContext(loadedUser);
    setStatusContent(loadedStatus);
    setAnchorContent(loadedAnchor);
    // Remount StatusField so its internal rows rebuild from the snapshot.
    setStatusLoadGen((g) => g + 1);
    setError("");
  };

  if (!agent) return null;

  return (
    <div className="min-h-full bg-app text-ink">
      <PageHeader
        title={t("common.settings")}
        onBack={() =>
          // Settings is pushed onto the stack from chat, so the UI back
          // button pops that entry (navigate(-1)) rather than pushing a new
          // chat route — otherwise browser back would then land on a
          // duplicate chat. Deep links / refreshes have no chat entry to pop
          // (no fromChat flag), so fall back to a replace-navigate.
          (location.state as { fromChat?: boolean } | null)?.fromChat
            ? navigate(-1)
            : navigate(`/agents/${id}`, { replace: true })
        }
        below={
          // Mobile section nav: sticky, horizontally scrollable chip row.
          <nav className="flex gap-1.5 overflow-x-auto border-t border-hairline px-4 py-2 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden lg:hidden">
            {SECTION_IDS.map((s) => (
              <button
                key={s}
                type="button"
                onClick={() => scrollToSection(s)}
                className={`shrink-0 whitespace-nowrap rounded-full border px-2.5 py-1 font-mono text-[12px] transition-colors ${
                  activeSection === s
                    ? "border-copper bg-copper/15 text-copper-bright"
                    : "border-hairline text-ink-dim hover:text-ink"
                }`}
              >
                {t(`settings.sec.${s}`)}
              </button>
            ))}
          </nav>
        }
      />

      <div className="mx-auto max-w-[900px] px-4 py-6 lg:grid lg:grid-cols-[180px_1fr] lg:gap-8">
        {/* Desktop sticky section rail */}
        <nav className="hidden lg:block">
          <div className="sticky top-24 space-y-0.5">
            {SECTION_IDS.map((s) => (
              <button
                key={s}
                type="button"
                onClick={() => scrollToSection(s)}
                className={`block w-full rounded-md px-2.5 py-1.5 text-left font-mono text-[12px] transition-colors ${
                  activeSection === s
                    ? "bg-copper/10 text-copper-bright"
                    : "text-ink-dim hover:bg-hover hover:text-ink"
                }`}
              >
                {t(`settings.sec.${s}`)}
              </button>
            ))}
          </div>
        </nav>

        <main className="min-w-0 space-y-6">
        {/* ── Identity ── */}
        <SectionCard
          id="identity"
          title={t("settings.sec.identity")}
          description={t("settings.card.identity.desc")}
        >
          {/* Avatar */}
          <div className="mb-4 flex items-center gap-4">
            <AgentAvatar agentId={agent.id} name={agent.name} size="xl" cacheBust={avatarToken} />
            <div className="flex flex-wrap gap-2">
              <Button onClick={() => fileRef.current?.click()}>{t("settings.changeAvatar")}</Button>
              <Button
                onClick={handleGenerateAvatar}
                disabled={generatingAvatar || !persona.trim()}
                className="flex items-center gap-1.5"
              >
                {generatingAvatar ? (
                  <><span className="animate-spin">↻</span> {t("settings.generating")}</>
                ) : (
                  <>✨ {t("settings.generate")}</>
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
            <Field label={t("settings.name")}>
              <Input value={name} onChange={(e) => setName(e.target.value)} />
            </Field>

            <PersonaField
              persona={persona}
              setPersona={setPersona}
              textareaRows={6}
              personaPrompt={personaPrompt}
              setPersonaPrompt={setPersonaPrompt}
              promptPlaceholder={t("settings.personaPromptPlaceholder")}
              busy={generatingPersona}
              spinning={generatingPersona}
              onGenerate={handleGeneratePersona}
            />

            {/* User Context (user.md) */}
            <Field
              label={t("settings.userContextLabel")}
              help={
                <>
                  {t("settings.userContextHelp")}{" "}
                  {userContextIsDefault && t("settings.templateNotSaved")}
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
              label={t("settings.statusLabel")}
              help={
                <>
                  {t("settings.statusHelp")}{" "}
                  {statusIsDefault && t("settings.templateNotSaved")}
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

            {/* Persona Anchor (anchor.md) */}
            <Field
              label={t("settings.anchorLabel")}
              help={
                <>
                  {t("settings.anchorHelp")}{" "}
                  {anchorIsDefault && t("settings.templateNotSaved")}
                </>
              }
            >
              <Textarea
                mono
                value={anchorContent}
                onChange={(e) => {
                  setAnchorContent(e.target.value);
                  // First keystroke off the empty template — clear the
                  // default flag so Save persists the edit.
                  if (anchorIsDefault) setAnchorIsDefault(false);
                }}
                rows={3}
              />
            </Field>

            {/* Public Profile */}
            <Field
              label={t("settings.publicProfile")}
              action={
                <label className="flex cursor-pointer items-center gap-1.5 text-[12px] text-ink-dim">
                  <input
                    type="checkbox"
                    checked={publicProfileOverride}
                    onChange={(e) => setPublicProfileOverride(e.target.checked)}
                    className="h-4 w-4 rounded border-hairline bg-raised accent-[color:var(--color-copper)]"
                  />
                  {t("settings.override")}
                </label>
              }
              help={
                publicProfileOverride
                  ? t("settings.publicProfileHelpOverride")
                  : t("settings.publicProfileHelpAuto")
              }
            >
              <Textarea
                value={publicProfile}
                onChange={(e) => setPublicProfile(e.target.value)}
                rows={2}
                disabled={!publicProfileOverride}
                placeholder={publicProfileOverride ? t("settings.publicProfilePlaceholderOverride") : t("settings.publicProfilePlaceholderAuto")}
                className={!publicProfileOverride ? "resize-none opacity-60" : "resize-none"}
              />
            </Field>
          </div>
        </SectionCard>

        {/* ── Context Injections ── */}
        <SectionCard
          id="injections"
          title={t("settings.sec.injections")}
          description={t("settings.card.injections.desc")}
        >
          <div className="space-y-3">
            {CONTEXT_INJECTION_KEYS.map((key) => {
              const enabled = !disabledInjections.includes(key);
              return (
                <ToggleRow
                  key={key}
                  checked={enabled}
                  onChange={(v) => toggleInjection(key, v)}
                  title={t(`settings.inj.${key}.label`)}
                  desc={t(`settings.inj.${key}.desc`)}
                />
              );
            })}
          </div>
        </SectionCard>

        {/* ── Model & Tools ── */}
        <SectionCard
          id="model"
          title={t("settings.sec.model")}
          description={t("settings.card.model.desc")}
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

            {(tool === "claude" || tool === "grok") && (
              <ToggleRow
                checked={autoEffort}
                onChange={setAutoEffort}
                title={t("settings.autoEffort")}
                desc={t("settings.autoEffortDesc")}
              />
            )}

            {needsCustomURL && (
              <Field label={t("settings.customBaseUrl")} help={t("settings.customBaseUrlHelp")}>
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
                    {t("settings.allowedTools")}
                    <span className="ml-2 text-ink-faint">{t("settings.allEmpty")}</span>
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
                    {t("settings.allowProtectedPaths")}
                    <span className="ml-2 text-ink-faint">{t("settings.bypassGuard")}</span>
                  </>
                }
                help={t("settings.allowProtectedPathsHelp")}
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
              <Field label={t("settings.thinking")}>
                <Select value={thinkingMode} onChange={(e) => setThinkingMode(e.target.value)}>
                  <option value="">{t("settings.thinkingAuto")}</option>
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
              title={t("settings.privileged")}
              desc={t("settings.privilegedDesc")}
            />
          </div>
        </SectionCard>

        {/* ── Schedule ── */}
        <SectionCard
          id="schedule"
          title={t("settings.sec.schedule")}
          description={t("settings.card.schedule.desc")}
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
              title={t("settings.notifyDuringSilent")}
              desc={t("settings.notifyDuringSilentDesc")}
            />
          </div>
        </SectionCard>

        {/* ── Voice ── */}
        <SectionCard
          id="voice"
          title={t("settings.sec.voice")}
          description={t("settings.card.voice.desc")}
          action={<Toggle checked={ttsEnabled} onChange={setTTSEnabled} aria-label={t("settings.enableTts")} />}
        >
          {ttsEnabled && (
            <div className="space-y-4">
              <Field
                label={t("settings.provider")}
                help={t("settings.providerHelp")}
              >
                <Select
                  value={ttsProvider}
                  onChange={(e) => {
                    const p = e.target.value === "grok" ? "grok" : "gemini";
                    setTTSProvider(p);
                    // Voice ids don't cross providers — reset to default.
                    setTTSVoice("");
                    setTTSPreviewError("");
                  }}
                >
                  <option value="gemini">Gemini</option>
                  <option value="grok">Grok (xAI)</option>
                </Select>
              </Field>

              {ttsProvider === "gemini" && (
                <Field label={t("settings.model")}>
                  <Select value={ttsModel} onChange={(e) => setTTSModel(e.target.value)}>
                    <option value="">{t("settings.default")} ({ttsCapability?.defaults.model ?? "gemini-3.1-flash-tts-preview"})</option>
                    {(ttsCapability?.models ?? []).map((m) => (
                      <option key={m} value={m}>{m}</option>
                    ))}
                  </Select>
                </Field>
              )}

              <Field
                label={t("settings.voice")}
                action={
                  ttsVoice ? (
                    <button
                      type="button"
                      onClick={() => playPreview(ttsVoice)}
                      className="text-[12px] text-copper transition-colors hover:text-copper-bright"
                    >
                      {ttsPreviewVoice === ttsVoice ? t("settings.playing") : t("settings.preview")}
                    </button>
                  ) : undefined
                }
                help={
                  <>
                    {t("settings.voiceHelpPre")}<span className="text-ink-dim">{t("settings.voiceHelpLink")}</span>{t("settings.voiceHelpPost")}
                  </>
                }
                error={ttsPreviewError || undefined}
              >
                <Select value={ttsVoice} onChange={(e) => setTTSVoice(e.target.value)}>
                  {ttsProvider === "grok" ? (
                    <>
                      <option value="">
                        {t("settings.default")} ({ttsCapability?.defaults.grokVoice ?? "eve"})
                      </option>
                      {(ttsCapability?.grokVoiceCatalog ?? []).map((v) => (
                        <option key={v.name} value={v.name}>
                          {v.label} ({v.gender || "?"})
                        </option>
                      ))}
                    </>
                  ) : (
                    <>
                      <option value="">
                        {t("settings.default")} ({ttsCapability?.defaults.voice ?? "Kore"})
                      </option>
                      {(ttsCapability?.voiceCatalog ?? []).map((v) => (
                        <option key={v.name} value={v.name}>
                          {v.name} ({v.gender || "?"}) — {v.trait}
                        </option>
                      ))}
                    </>
                  )}
                </Select>
              </Field>

              {/* All-voices preview grid — lets the user audition every voice
                  without leaving the settings page. Each row stays compact
                  so the full catalog fits without dominating the form. */}
              <details className="overflow-hidden rounded-[10px] border border-hairline bg-raised">
                <summary className="cursor-pointer select-none px-3 py-2 text-[12px] text-ink-dim">
                  {t("settings.browseVoices", { count: browseVoices.length })}
                </summary>
                <div className="grid max-h-64 grid-cols-2 gap-1 overflow-y-auto p-2">
                  {browseVoices.map((v) => (
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
                        <span className="text-ink">{v.display}</span>
                        {v.trait && <span className="text-ink-faint"> — {v.trait}</span>}
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

              {ttsProvider === "grok" && (
                <Banner tone="info">
                  {t("settings.grokNoStyle")}
                  <code className="text-ink-dim">[pause]</code>,{" "}
                  <code className="text-ink-dim">[laugh]</code>,{" "}
                  <code className="text-ink-dim">&lt;whisper&gt;…&lt;/whisper&gt;</code>.
                </Banner>
              )}

              {ttsProvider === "gemini" && (
              <Field
                label={t("settings.stylePrompt")}
                help={
                  <span className="space-y-1">
                    <span className="block">
                      {t("settings.stylePromptHelpText")}
                    </span>
                    <span className="block">
                      {t("settings.stylePromptReference")}
                      <a
                        href="https://ai.google.dev/gemini-api/docs/speech-generation"
                        target="_blank"
                        rel="noreferrer"
                        className="text-copper hover:text-copper-bright"
                      >
                        {t("settings.stylePromptGuide")}
                      </a>
                    </span>
                  </span>
                }
              >
                <Textarea
                  mono
                  value={ttsStylePrompt}
                  onChange={(e) => setTTSStylePrompt(e.target.value)}
                  placeholder={ttsCapability?.defaults.stylePrompt ?? t("settings.stylePromptPlaceholder")}
                  rows={3}
                  maxLength={500}
                />
              </Field>
              )}

              {ttsProvider === "gemini" && ttsCapability && !ttsCapability.ffmpeg && (
                <Banner tone="warn">
                  {t("settings.ffmpegWarn")}
                </Banner>
              )}
            </div>
          )}

        </SectionCard>

        {/* ── Integrations ── */}
        <SectionCard id="integrations" title={t("settings.sec.integrations")}>
          <SlackBotSettings agentId={id!} />
        </SectionCard>

        {/* ── Memory ── */}
        <SectionCard
          id="memory"
          title={t("settings.sec.memory")}
          description={t("settings.card.memory.desc")}
        >
          <div className="space-y-4">
            <div>
              <Button
                onClick={handleResetSession}
                disabled={resettingSession}
                className="w-full"
              >
                {resettingSession ? t("settings.resetting") : t("settings.resetCliSession")}
              </Button>
              <p className="mt-1.5 text-[12px] text-ink-faint">
                {t("settings.resetCliSessionHelp")}
              </p>
            </div>
          </div>
        </SectionCard>

        {/* ── Danger Zone ── */}
        <SectionCard id="danger" title={t("settings.card.danger")} danger>
          <div className="space-y-4">
            <Field
              label={t("settings.truncateLabel")}
              help={t("settings.truncateHelp")}
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
              {truncating ? t("settings.truncating") : t("settings.truncateButton")}
            </Button>
            {truncateResult && (
              <div className="space-y-0.5 rounded-[10px] border border-hairline bg-raised p-2 text-[12px] text-ink-dim">
                <div>{t("settings.truncateThreshold")}<span className="text-ink">{truncateResult.since}</span></div>
                <div>
                  {t("settings.truncateResult", {
                    messages: truncateResult.messagesRemoved,
                    claudeEntries: truncateResult.claudeSessionEntriesRemoved,
                    claudeFiles: truncateResult.claudeSessionFilesRemoved,
                    grokSessions: truncateResult.grokSessionsRemoved ?? 0,
                    grokFiles: truncateResult.grokSessionFilesRemoved ?? 0,
                    diaryEntries: truncateResult.diaryEntriesRemoved,
                    diaryFiles: truncateResult.diaryFilesRemoved,
                  })}
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
                {resetting ? t("settings.resetting") : t("settings.resetData")}
              </Button>
              <p className="mt-1.5 text-[12px] text-ink-faint">
                {t("settings.resetDataHelp")}
              </p>
            </div>
            <div>
              <Button onClick={openForkDialog} className="w-full">
                {t("settings.forkAgent")}
              </Button>
              <p className="mt-1.5 text-[12px] text-ink-faint">
                {t("settings.forkAgentHelp")}
              </p>
            </div>
            <div>
              <Button
                onClick={handleArchive}
                disabled={archiving}
                className="w-full"
              >
                {archiving ? t("settings.archiving") : t("settings.archiveAgent")}
              </Button>
              <p className="mt-1.5 text-[12px] text-ink-faint">
                {t("settings.archiveAgentHelp")}
              </p>
            </div>
            <Button
              variant="danger"
              onClick={handleDelete}
              disabled={deleting}
              className="w-full border border-lamp-err/40"
            >
              {deleting ? t("settings.deleting") : t("settings.deleteAgent")}
            </Button>
          </div>
        </SectionCard>

        {/* Info */}
        <div className="space-y-1 text-[12px] text-ink-faint">
          <div>{t("settings.idLabel", { id: agent.id })}</div>
          <div>{t("settings.createdLabel", { date: new Date(agent.createdAt).toLocaleString() })}</div>
        </div>

        {/* Sticky save bar — appears at the bottom of the pane whenever the
            form has unsaved changes (or a banner needs attention) and covers
            every field, TTS included, via handleSave. */}
        {(dirty || saving || error || success || checkinNotice) && (
          <div className="sticky bottom-0 z-10 -mx-4 border-t border-hairline bg-app/95 px-4 py-3 backdrop-blur">
            <div className="space-y-2">
              {error && <Banner tone="error">{error}</Banner>}
              {success && <Banner tone="success">{t("common.saved")}</Banner>}
              {checkinNotice && <Banner tone="warn">{checkinNotice}</Banner>}
              {(dirty || saving) && (
                <div className="flex items-center gap-2">
                  <span className="min-w-0 flex-1 truncate text-[12px] text-ink-faint">
                    {t("settings.unsavedChanges")}
                  </span>
                  <Button onClick={handleDiscard} disabled={saving}>
                    {t("settings.discard")}
                  </Button>
                  <Button variant="primary" onClick={handleSave} disabled={saving}>
                    {saving ? t("settings.saving") : t("settings.saveChanges")}
                  </Button>
                </div>
              )}
            </div>
          </div>
        )}
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
            <h3 className="mb-3 text-[14px] font-semibold text-ink">{t("settings.forkDialogTitle")}</h3>
            <Field label={t("settings.name")} className="mb-3">
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
                {t("settings.forkIncludeHistory")}
                <span className="block text-[12px] text-ink-faint">{t("settings.forkAlwaysCopied")}</span>
              </span>
            </label>
            <p className="mb-4 text-[12px] text-ink-faint">
              {t("settings.forkNotTransferred")}
            </p>
            {forkError && (
              <div className="mb-3">
                <Banner tone="error">{forkError}</Banner>
              </div>
            )}
            <div className="flex justify-end gap-2">
              <Button onClick={() => setShowForkDialog(false)} disabled={forking}>
                {t("common.cancel")}
              </Button>
              <Button
                variant="primary"
                onClick={handleFork}
                disabled={forking || !forkName.trim()}
              >
                {forking ? t("settings.forking") : t("settings.fork")}
              </Button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

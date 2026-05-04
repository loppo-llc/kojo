import { useState, useEffect } from "react";
import { INTERVAL_PRESETS, TIMEOUT_PRESETS, RESUME_IDLE_PRESETS, agentApi } from "../../lib/agentApi";

interface Props {
  agentId?: string;
  intervalMinutes: number;
  onIntervalChange: (v: number) => void;
  timeoutMinutes: number;
  onTimeoutChange: (v: number) => void;
  // claude-only: idle window before kojo abandons --resume on an
  // over-token-threshold session. 0 = use server default (5m). Pass `tool`
  // so we hide the control for non-claude backends where it has no effect.
  resumeIdleMinutes?: number;
  onResumeIdleChange?: (v: number) => void;
  tool?: string;
  silentStart: string;
  silentEnd: string;
  onSilentStartChange: (v: string) => void;
  onSilentEndChange: (v: string) => void;
  // RFC3339 timestamp of the next scheduled run (silent-hours-adjusted).
  // Empty/undefined when scheduling is off or the agent has no schedule.
  nextCronAt?: string;
  // True when interval/silent-hours have been edited but not yet saved —
  // nextCronAt reflects the saved schedule, so we hide the value and
  // prompt the user to save instead of showing a misleading time.
  scheduleDirty?: boolean;
  // Fires a manual check-in. When omitted the button is hidden.
  onCheckin?: () => void;
  checkingIn?: boolean;
}

const DEFAULT_CRON_MESSAGE_HINT =
  "最近の出来事や気づきがあれば memory/{date}.md に記録し、必要なタスクを実行してください。";

/** Parse "HH:MM" to minutes since midnight. */
function toMinutes(hhmm: string): number {
  const [h, m] = hhmm.split(":").map(Number);
  return h * 60 + (m || 0);
}

/** Build CSS gradient showing the silent window on a 24h bar. */
function timelineGradient(start: string, end: string): string {
  const silent = "rgb(38 38 38)"; // neutral-800 — muted/silent
  const active = "rgb(217 119 6 / 0.5)"; // amber-600/50 — active

  if (!start || !end) {
    return active; // no silent hours = always active
  }

  const s = (toMinutes(start) / 1440) * 100;
  const e = (toMinutes(end) / 1440) * 100;

  if (s <= e) {
    // Normal: active | silent | active
    return `linear-gradient(to right, ${active} ${s}%, ${silent} ${s}%, ${silent} ${e}%, ${active} ${e}%)`;
  }
  // Overnight: silent | active | silent
  return `linear-gradient(to right, ${silent} ${e}%, ${active} ${e}%, ${active} ${s}%, ${silent} ${s}%)`;
}

const HOUR_MARKS = [0, 3, 6, 9, 12, 15, 18, 21];

/**
 * Format an ISO timestamp for the "Next check-in" row. The absolute side
 * is rendered in the browser's local time with a `timeZoneName: "short"`
 * suffix so the user can tell at a glance whether their machine and the
 * server's TZ agree (Active Hours is configured against the server clock).
 * The relative side is computed from `now` so a parent can re-render it
 * every minute without remounting; rendering with `Date.now()` directly
 * goes stale until the next prop change. Returns null for empty/invalid
 * input so the caller can hide the row entirely.
 */
function formatNextCron(
  iso: string | undefined,
  now: number,
): { abs: string; rel: string } | null {
  if (!iso) return null;
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return null;
  const abs = t.toLocaleString(undefined, {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    timeZoneName: "short",
  });
  const diffMs = t.getTime() - now;
  const past = diffMs < 0;
  const mins = Math.max(1, Math.round(Math.abs(diffMs) / 60000));
  let amount: string;
  if (mins < 60) amount = `${mins}m`;
  else if (mins < 60 * 24) {
    const h = Math.floor(mins / 60);
    const m = mins % 60;
    amount = m === 0 ? `${h}h` : `${h}h${m}m`;
  } else {
    const d = Math.floor(mins / (60 * 24));
    const h = Math.floor((mins % (60 * 24)) / 60);
    amount = h === 0 ? `${d}d` : `${d}d${h}h`;
  }
  return { abs, rel: past ? `${amount} ago` : `in ${amount}` };
}

export function ScheduleEditor({
  agentId,
  intervalMinutes,
  onIntervalChange,
  timeoutMinutes,
  onTimeoutChange,
  resumeIdleMinutes,
  onResumeIdleChange,
  tool,
  silentStart,
  silentEnd,
  onSilentStartChange,
  onSilentEndChange,
  nextCronAt,
  scheduleDirty,
  onCheckin,
  checkingIn,
}: Props) {
  const showResumeIdle =
    onResumeIdleChange !== undefined && (tool === undefined || tool === "claude");

  // Checkin file state — managed independently via file-based API
  const [cronMessage, setCronMessage] = useState("");
  const [checkinDirty, setCheckinDirty] = useState(false);
  const [checkinSaving, setCheckinSaving] = useState(false);

  useEffect(() => {
    if (!agentId) return;
    agentApi.getCheckinFile(agentId).then(({ content, isDefault }) => {
      setCronMessage(content);
      setCheckinDirty(isDefault);
    }).catch(() => {});
  }, [agentId]);

  const handleCheckinMessageChange = (v: string) => {
    setCronMessage(v);
    setCheckinDirty(true);
  };

  const handleSaveCheckinFile = async () => {
    if (!agentId) return;
    setCheckinSaving(true);
    try {
      const saved = await agentApi.putCheckinFile(agentId, cronMessage);
      setCronMessage(saved);
      setCheckinDirty(false);
    } catch {
      // keep dirty state so user can retry
    } finally {
      setCheckinSaving(false);
    }
  };

  // Re-render every 30s so the "in 12m" / "2h ago" relative label next to
  // the upcoming check-in stays accurate without the user reloading. We
  // only run the timer when there's actually a value to display AND the
  // schedule isn't dirty — a stale value gets hidden behind "save to
  // update", so ticking would just burn renders for an invisible label.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!nextCronAt || scheduleDirty) return;
    const id = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(id);
  }, [nextCronAt, scheduleDirty]);
  // Separate enabled state so toggling off doesn't hide inputs mid-edit
  const [enabled, setEnabled] = useState(silentStart !== "" && silentEnd !== "");

  // Sync enabled state when props change (e.g., on load)
  useEffect(() => {
    setEnabled(silentStart !== "" && silentEnd !== "");
  }, [silentStart, silentEnd]);

  const toggleSilentHours = () => {
    if (enabled) {
      setEnabled(false);
      onSilentStartChange("");
      onSilentEndChange("");
    } else {
      setEnabled(true);
      onSilentStartChange(silentStart || "01:00");
      onSilentEndChange(silentEnd || "07:00");
    }
  };

  const localStart = silentStart || "01:00";
  const localEnd = silentEnd || "07:00";

  return (
    <div className="space-y-4">
      {/* Interval presets */}
      <div>
        <label className="block text-sm text-neutral-400 mb-2">Interval</label>
        <div className="flex gap-1.5 flex-wrap">
          {INTERVAL_PRESETS.map((opt) => (
            <button
              key={opt.value}
              type="button"
              onClick={() => onIntervalChange(opt.value)}
              className={`px-3 py-1.5 rounded text-sm transition-colors ${
                intervalMinutes === opt.value
                  ? "bg-amber-900/60 border border-amber-700/80 text-amber-200"
                  : "bg-neutral-900 border border-neutral-800 text-neutral-400 hover:border-neutral-600"
              }`}
            >
              {opt.label}
            </button>
          ))}
        </div>
        <p className="mt-1.5 text-[11px] text-neutral-600">
          Timing is automatically staggered per agent.
        </p>
      </div>

      {/* Timeout — also surfaced when manual check-in is available, since
          that path uses the same TimeoutMinutes value. Hiding it when
          interval=0 made sense before "Check in now" existed; now the
          setting is meaningful in both modes. */}
      {(intervalMinutes > 0 || onCheckin) && (
        <div>
          <label className="block text-sm text-neutral-400 mb-2">Timeout</label>
          <div className="flex gap-1.5 flex-wrap">
            {TIMEOUT_PRESETS.map((opt) => (
              <button
                key={opt.value}
                type="button"
                onClick={() => onTimeoutChange(opt.value)}
                className={`px-3 py-1.5 rounded text-sm transition-colors ${
                  timeoutMinutes === opt.value
                    ? "bg-amber-900/60 border border-amber-700/80 text-amber-200"
                    : "bg-neutral-900 border border-neutral-800 text-neutral-400 hover:border-neutral-600"
                }`}
              >
                {opt.label}
              </button>
            ))}
          </div>
          <p className="mt-1.5 text-[11px] text-neutral-600">
            Max duration for each scheduled or manual check-in run.
          </p>
        </div>
      )}

      {/* Resume Idle (claude only) */}
      {showResumeIdle && (
        <div>
          <label className="block text-sm text-neutral-400 mb-2">
            Resume Window
            <span className="text-xs text-neutral-600 ml-2">(claude session reset threshold)</span>
          </label>
          <div className="flex gap-1.5 flex-wrap">
            {RESUME_IDLE_PRESETS.map((opt) => (
              <button
                key={opt.value}
                type="button"
                onClick={() => onResumeIdleChange?.(opt.value)}
                className={`px-3 py-1.5 rounded text-sm transition-colors ${
                  (resumeIdleMinutes ?? 0) === opt.value
                    ? "bg-amber-900/60 border border-amber-700/80 text-amber-200"
                    : "bg-neutral-900 border border-neutral-800 text-neutral-400 hover:border-neutral-600"
                }`}
              >
                {opt.label}
              </button>
            ))}
          </div>
          <p className="mt-1.5 text-[11px] text-neutral-600">
            How long an over-context session keeps being resumed after the last
            interactive turn. Smaller resets sooner; larger keeps context across
            longer pauses. Default matches Anthropic's prompt-cache TTL.
          </p>
        </div>
      )}

      {/* Silent Hours */}
      {intervalMinutes > 0 && (
        <div>
          <div className="flex items-center justify-between mb-2">
            <label className="text-sm text-neutral-400">Silent Hours</label>
            <button
              type="button"
              onClick={toggleSilentHours}
              className="flex items-center gap-1.5"
            >
              <span
                className={`relative inline-flex h-4 w-7 shrink-0 items-center rounded-full transition-colors duration-200 ${
                  enabled ? "bg-amber-600/60" : "bg-neutral-700"
                }`}
              >
                <span
                  className={`inline-block h-2.5 w-2.5 rounded-full bg-white transition-transform duration-200 ${
                    enabled ? "translate-x-[14px]" : "translate-x-[3px]"
                  }`}
                />
              </span>
            </button>
          </div>

          {enabled && (
            <div className="space-y-3">
              {/* Time inputs */}
              <div className="flex items-center gap-3">
                <div className="flex-1">
                  <label className="block text-[11px] text-neutral-500 mb-1">From</label>
                  <input
                    type="time"
                    value={localStart}
                    onChange={(e) => onSilentStartChange(e.target.value)}
                    className="w-full px-2.5 py-1.5 bg-neutral-900 border border-neutral-700 rounded text-sm text-neutral-200 focus:outline-none focus:border-amber-700/60 [color-scheme:dark]"
                  />
                </div>
                <span className="text-neutral-600 mt-4">—</span>
                <div className="flex-1">
                  <label className="block text-[11px] text-neutral-500 mb-1">To</label>
                  <input
                    type="time"
                    value={localEnd}
                    onChange={(e) => onSilentEndChange(e.target.value)}
                    className="w-full px-2.5 py-1.5 bg-neutral-900 border border-neutral-700 rounded text-sm text-neutral-200 focus:outline-none focus:border-amber-700/60 [color-scheme:dark]"
                  />
                </div>
              </div>

              {/* Visual timeline */}
              <div>
                <div
                  className="h-3 rounded-full overflow-hidden border border-neutral-800"
                  style={{ background: timelineGradient(localStart, localEnd) }}
                />
                <div className="flex justify-between mt-1 px-0.5">
                  {HOUR_MARKS.map((h) => (
                    <span key={h} className="text-[9px] text-neutral-600 tabular-nums">
                      {h}
                    </span>
                  ))}
                  <span className="text-[9px] text-neutral-600 tabular-nums">24</span>
                </div>
              </div>

              <p className="text-[11px] text-neutral-600">
                {toMinutes(localStart) <= toMinutes(localEnd)
                  ? `Silent ${localStart}–${localEnd}. Paused during this window. (server time)`
                  : `Silent ${localStart}–24:00 & 0:00–${localEnd} (overnight, server time).`}
              </p>
            </div>
          )}

          {!enabled && (
            <p className="text-[11px] text-neutral-600">
              Runs 24/7. Enable to set quiet hours.
            </p>
          )}
        </div>
      )}

      {/* Next check-in / manual check-in trigger */}
      {(intervalMinutes > 0 || onCheckin) && (
        <div className="rounded-md border border-neutral-800 bg-neutral-900/50 p-3 space-y-2">
          {intervalMinutes > 0 && (() => {
            const next = scheduleDirty ? null : formatNextCron(nextCronAt, now);
            return (
              <div className="flex items-center justify-between gap-3">
                <span className="text-xs text-neutral-500">Next check-in</span>
                <span className="text-xs text-neutral-300 tabular-nums">
                  {scheduleDirty ? (
                    <span className="text-neutral-600">save to update</span>
                  ) : next ? (
                    <>
                      {next.abs}
                      <span className="text-neutral-500 ml-1.5">({next.rel})</span>
                    </>
                  ) : (
                    <span className="text-neutral-600">—</span>
                  )}
                </span>
              </div>
            );
          })()}

          {onCheckin && (
            <button
              type="button"
              onClick={onCheckin}
              disabled={checkingIn}
              className="w-full px-3 py-1.5 rounded text-xs bg-amber-900/40 hover:bg-amber-900/60 border border-amber-800/60 text-amber-200 disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
            >
              {checkingIn ? "Checking in…" : "Check in now"}
            </button>
          )}
        </div>
      )}

      {/* Custom Check-in Message — applied to BOTH periodic and manual
          check-ins. Saved independently via file-based API. */}
      <div>
        <label className="block text-sm text-neutral-400 mb-2">
          Check-in Message
        </label>
        <textarea
          value={cronMessage}
          onChange={(e) => handleCheckinMessageChange(e.target.value)}
          rows={5}
          maxLength={4096}
          placeholder={DEFAULT_CRON_MESSAGE_HINT}
          className="w-full px-2.5 py-1.5 bg-neutral-900 border border-neutral-700 rounded text-sm text-neutral-200 resize-none focus:outline-none focus:border-amber-700/60"
        />
        <div className="flex items-center justify-between mt-1.5">
          <p className="text-[11px] text-neutral-600">
            Replaces the trailing instruction in periodic and manual check-in
            prompts. Use <code className="text-neutral-500">{"{date}"}</code>{" "}
            as a placeholder for today (YYYY-MM-DD). Leave blank for the
            default.
          </p>
          {checkinDirty && (
            <button
              type="button"
              onClick={handleSaveCheckinFile}
              disabled={checkinSaving}
              className="ml-3 px-3 py-1 rounded text-xs bg-amber-900/40 hover:bg-amber-900/60 border border-amber-800/60 text-amber-200 disabled:opacity-50 disabled:cursor-not-allowed transition-colors whitespace-nowrap"
            >
              {checkinSaving ? "Saving…" : "Save"}
            </button>
          )}
        </div>
      </div>
    </div>
  );
}

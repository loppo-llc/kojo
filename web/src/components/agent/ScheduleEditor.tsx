import { useEffect, useMemo, useState } from "react";
import { SCHEDULE_PRESETS, TIMEOUT_PRESETS, RESUME_IDLE_PRESETS } from "../../lib/agentApi";
import {
  cronEquivalentToPreset,
  cronFromSimple,
  detectSimpleMode,
  humanizeCron,
  isCronExprSyntaxValid,
  parseCronExpr,
} from "../../lib/cronExpr";

interface Props {
  // 5-field standard cron expression. "" = scheduling disabled.
  cronExpr: string;
  onCronExprChange: (v: string) => void;
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
  cronMessage: string;
  onCronMessageChange: (v: string) => void;
  // RFC3339 timestamp of the next scheduled run (silent-hours-adjusted).
  // Empty/undefined when scheduling is off or the agent has no schedule.
  nextCronAt?: string;
  // True when the global cron toggle is in the paused position. We still
  // render nextCronAt (so the user can see what the schedule would do
  // when they un-pause) but suffix "(paused)" to make it obvious the
  // time is not currently firing.
  cronPausedGlobal?: boolean;
  // True when schedule fields have been edited but not yet saved —
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
  const silent = "rgb(38 38 38)";
  const active = "rgb(217 119 6 / 0.5)";

  if (!start || !end) {
    return active;
  }

  const s = (toMinutes(start) / 1440) * 100;
  const e = (toMinutes(end) / 1440) * 100;

  if (s <= e) {
    return `linear-gradient(to right, ${active} ${s}%, ${silent} ${s}%, ${silent} ${e}%, ${active} ${e}%)`;
  }
  return `linear-gradient(to right, ${silent} ${e}%, ${active} ${e}%, ${active} ${s}%, ${silent} ${s}%)`;
}

const HOUR_MARKS = [0, 3, 6, 9, 12, 15, 18, 21];

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

type TabId = "preset" | "hourly" | "daily" | "weekly" | "advanced";

const TABS: { id: TabId; label: string }[] = [
  { id: "preset", label: "Preset" },
  { id: "hourly", label: "Hourly" },
  { id: "daily", label: "Daily" },
  { id: "weekly", label: "Weekly" },
  { id: "advanced", label: "Advanced" },
];

const DOW_NAMES = ["日", "月", "火", "水", "木", "金", "土"];

/**
 * Pick which tab to surface when the editor mounts (or the value changes
 * out from under us, e.g. after Save). Falls back to Advanced if the
 * expression doesn't fit any of the simple-mode primitives — that's the
 * tab that can faithfully render anything.
 */
function tabForExpr(expr: string): TabId {
  const detected = detectSimpleMode(expr, SCHEDULE_PRESETS);
  if (!detected) return "advanced";
  switch (detected.mode) {
    case "preset":
      return "preset";
    case "everyN":
      // everyN is folded into the preset chips so users see one row of
      // options instead of two near-duplicates. Match by cadence
      // equivalence, NOT string equality, so an already-expanded preset
      // ("7,37 * * * *") that re-loaded from the server still resolves
      // back to the Preset tab.
      return SCHEDULE_PRESETS.some((p) => cronEquivalentToPreset(expr, p.cron))
        ? "preset"
        : "advanced";
    case "hourly":
      return "hourly";
    case "daily":
      return "daily";
    case "weekly":
      return "weekly";
  }
}

export function ScheduleEditor({
  cronExpr,
  onCronExprChange,
  timeoutMinutes,
  onTimeoutChange,
  resumeIdleMinutes,
  onResumeIdleChange,
  tool,
  silentStart,
  silentEnd,
  onSilentStartChange,
  onSilentEndChange,
  cronMessage,
  onCronMessageChange,
  nextCronAt,
  cronPausedGlobal,
  scheduleDirty,
  onCheckin,
  checkingIn,
}: Props) {
  const showResumeIdle =
    onResumeIdleChange !== undefined && (tool === undefined || tool === "claude");

  // Live-tick the relative "in 12m" / "2h ago" label. Skip while dirty —
  // the value is hidden behind a "save to update" notice in that case.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!nextCronAt || scheduleDirty) return;
    const id = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(id);
  }, [nextCronAt, scheduleDirty]);

  // Active tab. Re-sync when cronExpr changes externally (e.g. after Save
  // re-issues a fresh agent record) but otherwise keep whatever the user
  // last clicked so editing doesn't fight the auto-detect.
  const [tab, setTab] = useState<TabId>(() => tabForExpr(cronExpr));
  const [advancedDraft, setAdvancedDraft] = useState(cronExpr);
  useEffect(() => {
    setTab(tabForExpr(cronExpr));
    setAdvancedDraft(cronExpr);
  }, [cronExpr]);

  const detected = useMemo(
    () => detectSimpleMode(cronExpr, SCHEDULE_PRESETS),
    [cronExpr],
  );

  // Parsed cronExpr for the tab editors — these read the structure once
  // and re-emit a freshly-built expression on each input change.
  const parsed = parseCronExpr(cronExpr);

  // ---- Silent hours toggle (unchanged from the old editor) ----
  const [silentEnabled, setSilentEnabled] = useState(silentStart !== "" && silentEnd !== "");
  useEffect(() => {
    setSilentEnabled(silentStart !== "" && silentEnd !== "");
  }, [silentStart, silentEnd]);

  const toggleSilentHours = () => {
    if (silentEnabled) {
      setSilentEnabled(false);
      onSilentStartChange("");
      onSilentEndChange("");
    } else {
      setSilentEnabled(true);
      onSilentStartChange(silentStart || "01:00");
      onSilentEndChange(silentEnd || "07:00");
    }
  };
  const localStart = silentStart || "01:00";
  const localEnd = silentEnd || "07:00";

  const enabled = cronExpr !== "";

  return (
    <div className="space-y-4">
      {/* Mode tabs */}
      <div>
        <label className="block text-sm text-neutral-400 mb-2">Schedule</label>
        <div className="flex gap-1 mb-3 bg-neutral-900 rounded p-1 border border-neutral-800">
          {TABS.map((t) => (
            <button
              key={t.id}
              type="button"
              onClick={() => setTab(t.id)}
              className={`flex-1 px-2 py-1 rounded text-xs transition-colors ${
                tab === t.id
                  ? "bg-amber-900/60 border border-amber-700/80 text-amber-200"
                  : "text-neutral-400 hover:text-neutral-200"
              }`}
            >
              {t.label}
            </button>
          ))}
        </div>

        {tab === "preset" && (
          <div className="flex gap-1.5 flex-wrap">
            {SCHEDULE_PRESETS.map((opt) => {
              // cronEquivalentToPreset (NOT ===) so a Save round-trip
              // that expanded "@preset:30" into "7,37 * * * *" still
              // highlights the original "30m" chip.
              const selected = cronEquivalentToPreset(cronExpr, opt.cron);
              return (
                <button
                  key={opt.label}
                  type="button"
                  onClick={() => onCronExprChange(opt.cron)}
                  className={`px-3 py-1.5 rounded text-sm transition-colors ${
                    selected
                      ? "bg-amber-900/60 border border-amber-700/80 text-amber-200"
                      : "bg-neutral-900 border border-neutral-800 text-neutral-400 hover:border-neutral-600"
                  }`}
                >
                  {opt.label}
                </button>
              );
            })}
          </div>
        )}

        {tab === "hourly" && (
          <HourlyEditor
            initialMinute={
              detected?.mode === "hourly"
                ? detected.hourlyMinute ?? 0
                : parsed && /^\d+$/.test(parsed.minute)
                  ? parseInt(parsed.minute, 10)
                  : 0
            }
            onChange={(m) =>
              onCronExprChange(cronFromSimple("hourly", { minute: m }))
            }
          />
        )}

        {tab === "daily" && (
          <DailyEditor
            hh={detected?.mode === "daily" ? detected.hh ?? 9 : 9}
            mm={detected?.mode === "daily" ? detected.mm ?? 0 : 0}
            onChange={(hh, mm) =>
              onCronExprChange(cronFromSimple("daily", { hh, mm }))
            }
          />
        )}

        {tab === "weekly" && (
          <WeeklyEditor
            hh={detected?.mode === "weekly" ? detected.hh ?? 9 : 9}
            mm={detected?.mode === "weekly" ? detected.mm ?? 0 : 0}
            dows={detected?.mode === "weekly" ? detected.dows ?? [1] : [1]}
            onChange={(hh, mm, dows) =>
              onCronExprChange(cronFromSimple("weekly", { hh, mm, dows }))
            }
          />
        )}

        {tab === "advanced" && (
          <AdvancedEditor
            value={advancedDraft}
            onLocalChange={setAdvancedDraft}
            onCommit={(v) => onCronExprChange(v)}
          />
        )}

        {/* Live human-readable preview — shown across all tabs. */}
        <p className="mt-2 text-[11px] text-neutral-500">
          {humanizeCron(cronExpr)}
        </p>
      </div>

      {/* Timeout */}
      {(enabled || onCheckin) && (
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
      {enabled && (
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
                  silentEnabled ? "bg-amber-600/60" : "bg-neutral-700"
                }`}
              >
                <span
                  className={`inline-block h-2.5 w-2.5 rounded-full bg-white transition-transform duration-200 ${
                    silentEnabled ? "translate-x-[14px]" : "translate-x-[3px]"
                  }`}
                />
              </span>
            </button>
          </div>

          {silentEnabled && (
            <div className="space-y-3">
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

          {!silentEnabled && (
            <p className="text-[11px] text-neutral-600">
              Runs 24/7. Enable to set quiet hours.
            </p>
          )}
        </div>
      )}

      {/* Next check-in / manual check-in trigger */}
      {(enabled || onCheckin) && (
        <div className="rounded-md border border-neutral-800 bg-neutral-900/50 p-3 space-y-2">
          {enabled && (() => {
            const next = scheduleDirty ? null : formatNextCron(nextCronAt, now);
            return (
              <div className="flex items-center justify-between gap-3">
                <span className="text-xs text-neutral-500">
                  Next check-in
                  {cronPausedGlobal && (
                    <span className="ml-1.5 text-amber-500/80">(paused)</span>
                  )}
                </span>
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

      {/* Custom Check-in Message */}
      <div>
        <label className="block text-sm text-neutral-400 mb-2">
          Check-in Message
        </label>
        <textarea
          value={cronMessage}
          onChange={(e) => onCronMessageChange(e.target.value)}
          rows={3}
          maxLength={4096}
          placeholder={DEFAULT_CRON_MESSAGE_HINT}
          className="w-full px-2.5 py-1.5 bg-neutral-900 border border-neutral-700 rounded text-sm text-neutral-200 resize-none focus:outline-none focus:border-amber-700/60"
        />
        <p className="mt-1.5 text-[11px] text-neutral-600">
          Replaces the trailing instruction in periodic and manual check-in
          prompts. Use <code className="text-neutral-500">{"{date}"}</code>{" "}
          as a placeholder for today (YYYY-MM-DD). Leave blank for the
          default.
        </p>
      </div>
    </div>
  );
}

// ---- Sub-editors ----

function HourlyEditor({
  initialMinute,
  onChange,
}: {
  initialMinute: number;
  onChange: (m: number) => void;
}) {
  const [m, setM] = useState(initialMinute);
  useEffect(() => setM(initialMinute), [initialMinute]);
  return (
    <div className="flex items-center gap-3">
      <label className="text-sm text-neutral-400">毎時</label>
      <input
        type="number"
        min={0}
        max={59}
        value={m}
        onChange={(e) => {
          const v = clampInt(e.target.value, 0, 59);
          setM(v);
          onChange(v);
        }}
        className="w-20 px-2 py-1.5 bg-neutral-900 border border-neutral-700 rounded text-sm text-neutral-200 focus:outline-none focus:border-amber-700/60"
      />
      <span className="text-sm text-neutral-500">分</span>
    </div>
  );
}

function DailyEditor({
  hh,
  mm,
  onChange,
}: {
  hh: number;
  mm: number;
  onChange: (hh: number, mm: number) => void;
}) {
  const [h, setH] = useState(hh);
  const [m, setM] = useState(mm);
  useEffect(() => {
    setH(hh);
    setM(mm);
  }, [hh, mm]);
  const value = `${pad(h)}:${pad(m)}`;
  return (
    <div className="flex items-center gap-3">
      <label className="text-sm text-neutral-400">毎日</label>
      <input
        type="time"
        value={value}
        onChange={(e) => {
          const [nh, nm] = e.target.value.split(":").map(Number);
          if (Number.isFinite(nh) && Number.isFinite(nm)) {
            setH(nh);
            setM(nm);
            onChange(nh, nm);
          }
        }}
        className="px-2.5 py-1.5 bg-neutral-900 border border-neutral-700 rounded text-sm text-neutral-200 focus:outline-none focus:border-amber-700/60 [color-scheme:dark]"
      />
    </div>
  );
}

function WeeklyEditor({
  hh,
  mm,
  dows,
  onChange,
}: {
  hh: number;
  mm: number;
  dows: number[];
  onChange: (hh: number, mm: number, dows: number[]) => void;
}) {
  const [h, setH] = useState(hh);
  const [m, setM] = useState(mm);
  const [d, setD] = useState<number[]>(dows);
  useEffect(() => {
    setH(hh);
    setM(mm);
    setD(dows);
  }, [hh, mm, dows]);

  const toggleDow = (n: number) => {
    const next = d.includes(n) ? d.filter((x) => x !== n) : [...d, n].sort((a, b) => a - b);
    setD(next);
    onChange(h, m, next);
  };

  return (
    <div className="space-y-2">
      <div className="flex gap-1.5 flex-wrap">
        {DOW_NAMES.map((label, i) => {
          const selected = d.includes(i);
          return (
            <button
              key={i}
              type="button"
              onClick={() => toggleDow(i)}
              className={`px-2.5 py-1.5 rounded text-sm transition-colors ${
                selected
                  ? "bg-amber-900/60 border border-amber-700/80 text-amber-200"
                  : "bg-neutral-900 border border-neutral-800 text-neutral-400 hover:border-neutral-600"
              }`}
            >
              {label}
            </button>
          );
        })}
      </div>
      <div className="flex items-center gap-3">
        <input
          type="time"
          value={`${pad(h)}:${pad(m)}`}
          onChange={(e) => {
            const [nh, nm] = e.target.value.split(":").map(Number);
            if (Number.isFinite(nh) && Number.isFinite(nm)) {
              setH(nh);
              setM(nm);
              onChange(nh, nm, d);
            }
          }}
          className="px-2.5 py-1.5 bg-neutral-900 border border-neutral-700 rounded text-sm text-neutral-200 focus:outline-none focus:border-amber-700/60 [color-scheme:dark]"
        />
      </div>
      {d.length === 0 && (
        <p className="text-xs text-amber-400/80">
          曜日を 1 つ以上選んで。空のまま保存するとスケジュール無効になる。
        </p>
      )}
    </div>
  );
}

function AdvancedEditor({
  value,
  onLocalChange,
  onCommit,
}: {
  value: string;
  onLocalChange: (v: string) => void;
  onCommit: (v: string) => void;
}) {
  const valid = isCronExprSyntaxValid(value);
  return (
    <div>
      <input
        type="text"
        value={value}
        onChange={(e) => {
          onLocalChange(e.target.value);
        }}
        onBlur={() => {
          if (valid) onCommit(value.trim());
        }}
        onKeyDown={(e) => {
          if (e.key === "Enter") {
            e.preventDefault();
            if (valid) onCommit(value.trim());
          }
        }}
        placeholder="*/15 * * * *"
        className={`w-full px-2.5 py-1.5 bg-neutral-900 border rounded text-sm text-neutral-200 font-mono focus:outline-none ${
          valid
            ? "border-neutral-700 focus:border-amber-700/60"
            : "border-red-700/60 focus:border-red-500/80"
        }`}
      />
      <p className="mt-1 text-[11px] text-neutral-600">
        5-field cron (minute hour day-of-month month day-of-week). Empty = off.
        Press Enter or tab away to apply.
      </p>
      {!valid && (
        <p className="mt-1 text-[11px] text-red-400">
          Invalid syntax — must be 5 whitespace-separated fields.
        </p>
      )}
    </div>
  );
}

function pad(n: number): string {
  return n.toString().padStart(2, "0");
}

function clampInt(s: string, lo: number, hi: number): number {
  const n = parseInt(s, 10);
  if (Number.isNaN(n)) return lo;
  return Math.max(lo, Math.min(hi, n));
}

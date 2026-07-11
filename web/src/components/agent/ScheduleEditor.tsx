import { useEffect, useState } from "react";
import {
  INTERVAL_MINUTE_OPTIONS,
  INTERVAL_HOUR_OPTIONS,
  INTERVAL_DAY_OPTIONS,
  TIMEOUT_PRESETS,
  RESUME_IDLE_PRESETS,
} from "../../lib/agentApi";
import {
  cronFromInterval,
  detectInterval,
  humanizeCron,
  isCronExprSyntaxValid,
  type IntervalSpec,
  type IntervalUnit,
} from "../../lib/cronExpr";
import { Field } from "../ui/Field";
import { Input } from "../ui/Input";
import { Select } from "../ui/Select";
import { Textarea } from "../ui/Textarea";
import { Toggle } from "../ui/Toggle";
import { t as i18nT, useT, type MessageKey } from "../../lib/i18n";

// Shared chip style for the resume-idle preset toggles.
function chipClass(selected: boolean): string {
  return `rounded-lg border px-3 py-1.5 text-[13px] transition-colors ${
    selected
      ? "border-copper bg-copper/15 text-copper-bright"
      : "border-hairline bg-raised text-ink-dim hover:border-ink-faint hover:text-ink"
  }`;
}

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

// CRON_MESSAGE_MAX_LEN matches the server-side workspaceFileBodyCap (1 MiB)
// divided by 4 — UTF-8 worst-case is 4 bytes per code unit, so capping
// the textarea at ~256 KiB code units keeps the encoded body within the
// MaxBytesReader limit on the /checkin-file PUT regardless of the input
// language. Far larger than realistic check-in bodies (a single line is
// typical) but matches the back-end so the UI never produces a request
// the server will reject.
const CRON_MESSAGE_MAX_LEN = (1 << 20) / 4;

/** Parse "HH:MM" to minutes since midnight. */
function toMinutes(hhmm: string): number {
  const [h, m] = hhmm.split(":").map(Number);
  return h * 60 + (m || 0);
}

/** Build CSS gradient showing the silent window on a 24h bar. */
function timelineGradient(start: string, end: string): string {
  const silent = "rgb(38 43 51)"; // hairline
  const active = "rgb(208 139 85 / 0.4)"; // copper

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
  const dt = new Date(iso);
  if (Number.isNaN(dt.getTime())) return null;
  const abs = dt.toLocaleString(undefined, {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    timeZoneName: "short",
  });
  const diffMs = dt.getTime() - now;
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
  return { abs, rel: past ? i18nT("sched.relAgo", { amount }) : i18nT("sched.relIn", { amount }) };
}

type Mode = "off" | "interval" | "cron";

const MODES: { id: Mode; labelKey: MessageKey }[] = [
  { id: "off", labelKey: "sched.modeOff" },
  { id: "interval", labelKey: "sched.modeInterval" },
  { id: "cron", labelKey: "sched.modeCron" },
];

const INTERVAL_OPTS = {
  minutes: INTERVAL_MINUTE_OPTIONS,
  hours: INTERVAL_HOUR_OPTIONS,
  days: INTERVAL_DAY_OPTIONS,
} as const;

const UNIT_KEYS: Record<IntervalUnit, MessageKey> = {
  minute: "sched.unitMinutes",
  hour: "sched.unitHours",
  day: "sched.unitDays",
};

function optionsForUnit(unit: IntervalUnit): ReadonlyArray<number> {
  switch (unit) {
    case "minute":
      return INTERVAL_OPTS.minutes;
    case "hour":
      return INTERVAL_OPTS.hours;
    case "day":
      return INTERVAL_OPTS.days;
  }
}

// Default N when the user switches to a unit whose option list doesn't
// contain the current value: 30 minutes / 1 hour / 1 day.
function defaultNForUnit(unit: IntervalUnit): number {
  return unit === "minute" ? 30 : 1;
}

/**
 * Pick which mode to surface for an expression: "" = off, anything the
 * interval editor can faithfully render = interval, everything else = cron
 * (the tab that can render anything verbatim).
 */
function modeForExpr(expr: string): Mode {
  if (expr === "") return "off";
  return detectInterval(expr, INTERVAL_OPTS) ? "interval" : "cron";
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
  const t = useT();
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

  // Active mode. Re-syncs when cronExpr changes externally (e.g. after Save
  // re-issues a fresh agent record) — except that the cron tab is sticky:
  // it can faithfully render any expression, and jumping the user out of it
  // mid-edit (because they typed something the interval editor happens to
  // recognise) would fight the edit.
  const [mode, setMode] = useState<Mode>(() => modeForExpr(cronExpr));
  const [cronDraft, setCronDraft] = useState(cronExpr);
  // Last-known interval selection; preserved across mode/unit switches so
  // flipping Off → Interval restores what the user had.
  const [spec, setSpec] = useState<IntervalSpec>(
    () => detectInterval(cronExpr, INTERVAL_OPTS) ?? { unit: "minute", n: 30 },
  );
  useEffect(() => {
    setCronDraft(cronExpr);
    const d = detectInterval(cronExpr, INTERVAL_OPTS);
    if (d) setSpec(d);
    setMode((prev) => {
      if (prev === "cron" && cronExpr !== "") return prev;
      return modeForExpr(cronExpr);
    });
  }, [cronExpr]);

  const selectMode = (m: Mode) => {
    if (m === mode) return;
    setMode(m);
    if (m === "off") {
      onCronExprChange("");
    } else if (m === "interval") {
      onCronExprChange(cronFromInterval(spec));
    } else {
      // cron: start editing from whatever is currently set.
      setCronDraft(cronExpr);
    }
  };

  const changeSpec = (next: IntervalSpec) => {
    setSpec(next);
    onCronExprChange(cronFromInterval(next));
  };

  // ---- Silent hours toggle ----
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
      {/* Mode segments — the enclosing SectionCard already titles this
          "スケジュール", so no inner label. */}
      <div>
        <div className="mb-3 flex gap-1 rounded-lg border border-hairline bg-raised p-1">
          {MODES.map((m) => (
            <button
              key={m.id}
              type="button"
              onClick={() => selectMode(m.id)}
              className={`flex-1 rounded-md px-2 py-1 text-[12px] transition-colors ${
                mode === m.id
                  ? "bg-copper/15 text-copper-bright"
                  : "text-ink-dim hover:text-ink"
              }`}
            >
              {t(m.labelKey)}
            </button>
          ))}
        </div>

        {mode === "interval" && <IntervalEditor spec={spec} onChange={changeSpec} />}

        {mode === "cron" && (
          <CronEditor
            value={cronDraft}
            onLocalChange={setCronDraft}
            onCommit={(v) => onCronExprChange(v)}
          />
        )}

        {/* Live human-readable preview — the single readout of what will
            actually fire, shown across all modes. */}
        <p className="mt-2 text-[11px] text-ink-faint">
          {humanizeCron(cronExpr)}
        </p>
      </div>

      {/* Timeout */}
      {(enabled || onCheckin) && (
        <Field label={t("sched.timeout")} help={t("sched.timeoutHelp")}>
          <div className="max-w-[220px]">
            <Select
              value={String(timeoutMinutes)}
              onChange={(e) => onTimeoutChange(parseInt(e.target.value, 10))}
            >
              {/* 0 (= server default) isn't a preset the UI offers, but old
                  agents may still carry it — surface it instead of showing
                  a blank select. */}
              {timeoutMinutes === 0 && (
                <option value="0">{t("sched.timeoutDefault")}</option>
              )}
              {TIMEOUT_PRESETS.map((opt) => (
                <option key={opt.value} value={String(opt.value)}>
                  {opt.value === -1 ? t("sched.timeoutNone") : opt.label}
                </option>
              ))}
            </Select>
          </div>
        </Field>
      )}

      {/* Resume Idle (claude only) */}
      {showResumeIdle && (
        <Field
          label={
            <>
              {t("sched.resumeWindow")}
              <span className="ml-2 text-ink-faint">{t("sched.resumeWindowSub")}</span>
            </>
          }
          help={t("sched.resumeWindowHelp")}
        >
          <div className="flex flex-wrap gap-1.5">
            {RESUME_IDLE_PRESETS.map((opt) => (
              <button
                key={opt.value}
                type="button"
                onClick={() => onResumeIdleChange?.(opt.value)}
                className={chipClass((resumeIdleMinutes ?? 0) === opt.value)}
              >
                {opt.value === 0 ? t("sched.resumeDefault") : opt.label}
              </button>
            ))}
          </div>
        </Field>
      )}

      {/* Silent Hours */}
      {enabled && (
        <div>
          <div className="mb-2 flex items-center justify-between">
            <label className="text-[12px] font-medium text-ink-dim">{t("sched.silentHours")}</label>
            <Toggle checked={silentEnabled} onChange={toggleSilentHours} aria-label={t("sched.silentHours")} />
          </div>

          {silentEnabled && (
            <div className="space-y-3">
              <div className="flex items-center gap-3">
                <Field label={t("sched.from")} className="flex-1">
                  <Input
                    type="time"
                    value={localStart}
                    onChange={(e) => onSilentStartChange(e.target.value)}
                  />
                </Field>
                <span className="mt-6 text-ink-faint">—</span>
                <Field label={t("sched.to")} className="flex-1">
                  <Input
                    type="time"
                    value={localEnd}
                    onChange={(e) => onSilentEndChange(e.target.value)}
                  />
                </Field>
              </div>

              <div>
                <div
                  className="h-3 overflow-hidden rounded-full border border-hairline"
                  style={{ background: timelineGradient(localStart, localEnd) }}
                />
                <div className="mt-1 flex justify-between px-0.5">
                  {HOUR_MARKS.map((h) => (
                    <span key={h} className="text-[9px] tabular-nums text-ink-faint">
                      {h}
                    </span>
                  ))}
                  <span className="text-[9px] tabular-nums text-ink-faint">24</span>
                </div>
              </div>

              <p className="text-[11px] text-ink-faint">
                {toMinutes(localStart) <= toMinutes(localEnd)
                  ? t("sched.silentRange", { start: localStart, end: localEnd })
                  : t("sched.silentRangeOvernight", { start: localStart, end: localEnd })}
              </p>
            </div>
          )}

          {!silentEnabled && (
            <p className="text-[11px] text-ink-faint">
              {t("sched.runs247")}
            </p>
          )}
        </div>
      )}

      {/* Next check-in / manual check-in trigger.
          Both pieces are only meaningful for a persisted agent: nextCronAt
          is server-computed against the saved schedule, and onCheckin fires
          a run against the saved record. AgentCreate passes neither, so
          gating on `onCheckin` (which AgentSettings always provides) keeps
          the whole block hidden in create mode — even though `enabled` is
          true there via the default cron preset. */}
      {onCheckin && (
        <div className="space-y-2 rounded-[10px] border border-hairline bg-raised p-3">
          {enabled && (() => {
            const next = scheduleDirty ? null : formatNextCron(nextCronAt, now);
            return (
              <div className="flex items-center justify-between gap-3">
                <span className="text-[12px] text-ink-dim">
                  {t("sched.nextCheckin")}
                  {cronPausedGlobal && (
                    <span className="ml-1.5 text-lamp-warn">{t("sched.pausedSuffix")}</span>
                  )}
                </span>
                <span className="text-[12px] tabular-nums text-ink">
                  {scheduleDirty ? (
                    <span className="text-ink-faint">{t("sched.saveToUpdate")}</span>
                  ) : next ? (
                    <>
                      {next.abs}
                      <span className="ml-1.5 text-ink-dim">({next.rel})</span>
                    </>
                  ) : (
                    <span className="text-ink-faint">—</span>
                  )}
                </span>
              </div>
            );
          })()}

          {/* Outer guard above already requires onCheckin, so render
              unconditionally here. */}
          <button
            type="button"
            onClick={onCheckin}
            disabled={checkingIn}
            className="w-full rounded-lg border border-copper/50 bg-copper/10 px-3 py-2 text-[13px] text-copper-bright transition-colors hover:bg-copper/20 disabled:cursor-not-allowed disabled:opacity-50"
          >
            {checkingIn ? t("sched.checkingIn") : t("sched.checkinNow")}
          </button>
        </div>
      )}

      {/* Custom Check-in Message */}
      <Field
        label={t("sched.checkinMessage")}
        help={
          <>
            {t("sched.checkinMessageHelpPre")}
            <code className="text-ink-dim">{"{date}"}</code>
            {t("sched.checkinMessageHelpPost")}
          </>
        }
      >
        <Textarea
          value={cronMessage}
          onChange={(e) => onCronMessageChange(e.target.value)}
          rows={3}
          maxLength={CRON_MESSAGE_MAX_LEN}
          placeholder={t("sched.checkinMessageHint")}
        />
      </Field>
    </div>
  );
}

// ---- Sub-editors ----

/**
 * "Every [N] [minutes|hours|days] (at HH:MM)" — the single row that replaces
 * the old preset/hourly/daily/weekly tab set. Word order differs between
 * locales, so the surrounding words come from intervalPrefix/intervalSuffix
 * keys (en: "Every … —", ja: "… おき").
 */
function IntervalEditor({
  spec,
  onChange,
}: {
  spec: IntervalSpec;
  onChange: (spec: IntervalSpec) => void;
}) {
  const t = useT();
  const prefix = t("sched.intervalPrefix");
  const suffix = t("sched.intervalSuffix");

  const changeUnit = (unit: IntervalUnit) => {
    const opts = optionsForUnit(unit);
    const n = opts.includes(spec.n) ? spec.n : defaultNForUnit(unit);
    const next: IntervalSpec = { unit, n };
    if (unit === "day") {
      next.hh = spec.hh ?? 9;
      next.mm = spec.mm ?? 0;
    }
    onChange(next);
  };

  return (
    <div className="flex flex-wrap items-center gap-2">
      {prefix && <span className="text-[13px] text-ink-dim">{prefix}</span>}
      <div className="w-20">
        <Select
          value={String(spec.n)}
          onChange={(e) => onChange({ ...spec, n: parseInt(e.target.value, 10) })}
          aria-label={t("sched.intervalN")}
        >
          {optionsForUnit(spec.unit).map((n) => (
            <option key={n} value={String(n)}>
              {n}
            </option>
          ))}
        </Select>
      </div>
      <div className="w-24">
        <Select
          value={spec.unit}
          onChange={(e) => changeUnit(e.target.value as IntervalUnit)}
          aria-label={t("sched.intervalUnit")}
        >
          {(Object.keys(UNIT_KEYS) as IntervalUnit[]).map((u) => (
            <option key={u} value={u}>
              {t(UNIT_KEYS[u])}
            </option>
          ))}
        </Select>
      </div>
      {suffix && <span className="text-[13px] text-ink-dim">{suffix}</span>}
      {spec.unit === "day" && (
        <Input
          type="time"
          value={`${pad(spec.hh ?? 9)}:${pad(spec.mm ?? 0)}`}
          onChange={(e) => {
            const [nh, nm] = e.target.value.split(":").map(Number);
            if (Number.isFinite(nh) && Number.isFinite(nm)) {
              onChange({ ...spec, hh: nh, mm: nm });
            }
          }}
          className="w-auto"
          aria-label={t("sched.intervalTime")}
        />
      )}
    </div>
  );
}

function CronEditor({
  value,
  onLocalChange,
  onCommit,
}: {
  value: string;
  onLocalChange: (v: string) => void;
  onCommit: (v: string) => void;
}) {
  const t = useT();
  const valid = isCronExprSyntaxValid(value);
  return (
    <Field
      help={t("sched.advancedHelp")}
      error={!valid ? t("sched.advancedInvalid") : undefined}
    >
      <Input
        mono
        invalid={!valid}
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
      />
    </Field>
  );
}

function pad(n: number): string {
  return n.toString().padStart(2, "0");
}

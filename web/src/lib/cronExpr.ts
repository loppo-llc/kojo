// cronExpr.ts — client-side helpers for the strict 5-field cron expressions
// the kojo backend now persists. Mirrors the Go-side ValidateCronExpr surface
// so the editor can give immediate feedback without round-tripping to the
// server. Only matches cron forms that the simple-mode UI can faithfully
// render; anything else falls back to "Advanced" + a verbatim string echo.

import { getLocale } from "./i18n";

export interface CronStruct {
  minute: string;
  hour: string;
  dom: string;
  month: string;
  dow: string;
}

// IntervalSpec describes the one simple-mode primitive the schedule editor
// exposes: "every N minutes/hours/days". Days carry a wall-clock fire time;
// minutes/hours use the per-agent-offset sentinel so no time is stored.
export type IntervalUnit = "minute" | "hour" | "day";

export interface IntervalSpec {
  unit: IntervalUnit;
  n: number;
  // day only: fire time (defaults 09:00 when absent)
  hh?: number;
  mm?: number;
}

// Accepts digits, wildcard, list/range/step separators, AND the case-insensitive
// month/dow word forms (JAN-DEC, MON-SUN) that robfig/cron's standard parser
// also recognises. Anything else gets bounced before the editor lets it through
// — the deep parse still happens on the server so the two stay in lockstep.
const FIELD_RE = /^[\dA-Z*,/\-]+$/i;

// CRON_PRESET_PREFIX mirrors backend cron_expr.go's CronPresetSentinelPrefix.
// Sentinels of the form "@preset:N" are expanded server-side at Save time
// into a real 5-field expression with a per-agent offset baked in (so a
// "30m" preset doesn't fire every agent at :00).
export const CRON_PRESET_PREFIX = "@preset:";

// CRON_PRESET_ALLOWED mirrors backend's legacyAllowedIntervals exactly.
// The cron editor uses this to reject "@preset:7" or any other value
// the server would refuse, so the user gets immediate feedback instead of
// a 400 on Save.
const CRON_PRESET_ALLOWED = new Set([
  5, 10, 15, 20, 30, 60, 120, 180, 240, 360, 480, 720, 1440,
]);

/**
 * Match the "@preset:N" sentinel and return N when N is in the allowed
 * whitelist. Returns null on any non-match (wrong prefix, leading zero,
 * non-integer, out-of-whitelist value). Mirrors backend's
 * parseCronPresetSentinel + ValidateCronExpr's whitelist gate so the two
 * parsers can't drift.
 */
export function parseCronPresetSentinel(expr: string): number | null {
  if (!expr.startsWith(CRON_PRESET_PREFIX)) return null;
  const rest = expr.slice(CRON_PRESET_PREFIX.length);
  if (!/^[1-9]\d*$/.test(rest)) return null;
  const n = parseInt(rest, 10);
  if (!Number.isFinite(n) || n <= 0) return null;
  if (!CRON_PRESET_ALLOWED.has(n)) return null;
  return n;
}

/**
 * Split a 5-field cron expression into its fields. Returns null for any
 * input that isn't exactly 5 whitespace-separated fields whose characters
 * fall inside the safe charset. The backend's strict parser is the source
 * of truth — this is just a fast pre-filter so the editor can flag obvious
 * typos without a network round-trip.
 */
export function parseCronExpr(expr: string): CronStruct | null {
  if (!expr) return null;
  const fields = expr.trim().split(/\s+/);
  if (fields.length !== 5) return null;
  for (const f of fields) {
    if (!FIELD_RE.test(f)) return null;
  }
  return {
    minute: fields[0],
    hour: fields[1],
    dom: fields[2],
    month: fields[3],
    dow: fields[4],
  };
}

/**
 * Quick syntax-only check used by the Advanced tab to flag bad input
 * before save. Catches obvious typos (wrong field count, bad charset,
 * divide-by-zero step like a stray "/0" suffix) but leaves range bounds
 * and day/month combos to the backend so the two parsers can't drift.
 */
export function isCronExprSyntaxValid(expr: string): boolean {
  if (expr === "") return true;
  if (parseCronPresetSentinel(expr) !== null) return true;
  const c = parseCronExpr(expr);
  if (!c) return false;
  // A trailing "/0" (e.g. the splat-form "*/0" or any other Step=0
  // construct) parses as "every zero units" — robfig/cron would error out
  // and the UI shouldn't pretend it's valid.
  for (const f of [c.minute, c.hour, c.dom, c.month, c.dow]) {
    if (/\/0+$/.test(f)) return false;
  }
  return true;
}

const DOW_LABELS_JA = ["日", "月", "火", "水", "木", "金", "土"];
const DOW_LABELS_EN = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];

/**
 * Build a locale-aware (ja/en) summary for the most common cron shapes. Falls
 * back to the raw expression in backticks when nothing matches so the user
 * still sees what's about to be saved.
 */
export function humanizeCron(expr: string): string {
  const ja = getLocale() === "ja";
  const everyNMin = (n: number, offset: boolean) =>
    (ja ? `${n} 分おき` : `every ${n} min`) + (offset ? " (per-agent offset)" : "");
  const everyNHour = (n: number, offset: boolean) =>
    (ja ? `${n} 時間おき` : n === 1 ? "every hour" : `every ${n} hours`) +
    (offset ? " (per-agent offset)" : "");
  if (!expr) return ja ? "スケジュールなし" : "No schedule";
  const presetN = parseCronPresetSentinel(expr);
  if (presetN !== null) {
    if (presetN < 60) return everyNMin(presetN, true);
    if (presetN === 1440) return (ja ? "毎日" : "daily") + " (per-agent offset)";
    if (presetN % 60 === 0 && presetN < 1440) {
      return everyNHour(presetN / 60, true);
    }
    return everyNMin(presetN, true);
  }
  const c = parseCronExpr(expr);
  if (!c) return `cron: \`${expr}\``;

  // Sub-hourly cadence: "*/N * * * *" — also recognise the equivalent
  // offset-list form ("7,37 * * * *") that the legacy intervalToCron
  // migration emits. detectEvenStep returns the step when the list is
  // evenly spaced over a 60-minute period.
  if (c.hour === "*" && c.dom === "*" && c.month === "*" && c.dow === "*") {
    const stepMin = detectStepMinute(c.minute);
    if (stepMin !== null) return everyNMin(stepMin, false);
  }

  // Every-N hours at fixed minute: "M */N * * *" — likewise recognise
  // "M H1,H2,…" with even hour spacing.
  if (/^\d+$/.test(c.minute) && c.dom === "*" && c.month === "*" && c.dow === "*") {
    const stepHour = detectStepHour(c.hour);
    if (stepHour !== null) {
      return ja
        ? `${stepHour} 時間おき (${pad2(parseInt(c.minute, 10))} 分)`
        : `${everyNHour(stepHour, false)} (at :${pad2(parseInt(c.minute, 10))})`;
    }
  }

  // Hourly at minute M: "M * * * *"
  if (/^\d+$/.test(c.minute) && c.hour === "*" && c.dom === "*" && c.month === "*" && c.dow === "*") {
    return ja
      ? `毎時 ${pad2(parseInt(c.minute, 10))} 分`
      : `hourly at :${pad2(parseInt(c.minute, 10))}`;
  }

  // Daily at HH:MM (any DOW pattern restricted to *)
  if (/^\d+$/.test(c.minute) && /^\d+$/.test(c.hour) && c.dom === "*" && c.month === "*" && c.dow === "*") {
    return `${ja ? "毎日" : "daily"} ${pad2(parseInt(c.hour, 10))}:${pad2(parseInt(c.minute, 10))}`;
  }

  // Every-N days at HH:MM: "M H */n * *". The step resets at each month
  // boundary (cron semantics), which the interval editor accepts.
  if (/^\d+$/.test(c.minute) && /^\d+$/.test(c.hour) && c.month === "*" && c.dow === "*") {
    const domStep = c.dom.match(/^\*\/(\d+)$/);
    if (domStep) {
      const n = parseInt(domStep[1], 10);
      const hhmm = `${pad2(parseInt(c.hour, 10))}:${pad2(parseInt(c.minute, 10))}`;
      return ja ? `${n} 日おき ${hhmm}` : `every ${n} days at ${hhmm}`;
    }
  }

  // Weekly at HH:MM on a list/range of DOWs.
  if (/^\d+$/.test(c.minute) && /^\d+$/.test(c.hour) && c.dom === "*" && c.month === "*") {
    const dows = expandDowField(c.dow);
    if (dows && dows.length > 0) {
      const label = formatDowList(dows);
      return `${ja ? "毎週" : "weekly"} ${label} ${pad2(parseInt(c.hour, 10))}:${pad2(parseInt(c.minute, 10))}`;
    }
  }

  return `cron: \`${expr}\``;
}

/**
 * Generate a cron expression from an IntervalSpec. Minutes/hours use the
 * "@preset:N" sentinel so the server bakes in a per-agent offset; days emit
 * a literal expression with the user-chosen fire time ("M H *\/n * *", or
 * plain daily "M H * * *" for n=1). Values outside the editor's option
 * lists are clamped by the caller — this function trusts its input except
 * for the time fields.
 */
export function cronFromInterval(spec: IntervalSpec): string {
  switch (spec.unit) {
    case "minute":
      return `${CRON_PRESET_PREFIX}${spec.n}`;
    case "hour":
      return `${CRON_PRESET_PREFIX}${spec.n * 60}`;
    case "day": {
      const h = clamp(spec.hh ?? 9, 0, 23);
      const m = clamp(spec.mm ?? 0, 0, 59);
      return spec.n === 1 ? `${m} ${h} * * *` : `${m} ${h} */${spec.n} * *`;
    }
  }
}

/**
 * Decide whether an existing cron expression fits the interval editor.
 * Recognises the sentinel form, the "*\/N" step forms, the even offset-list
 * forms that the server's expansion emits ("7,37 * * * *"), plain daily
 * ("M H * * *"), and the day-step form ("M H *\/n * *"). Only steps that
 * appear in the editor's option lists match — anything else returns null so
 * the caller falls through to the cron tab and shows the raw string.
 */
export function detectInterval(
  expr: string,
  opts: {
    minutes: ReadonlyArray<number>;
    hours: ReadonlyArray<number>;
    days: ReadonlyArray<number>;
  },
): IntervalSpec | null {
  const presetN = parseCronPresetSentinel(expr);
  if (presetN !== null) {
    if (presetN < 60 && opts.minutes.includes(presetN)) {
      return { unit: "minute", n: presetN };
    }
    if (presetN % 60 === 0 && opts.hours.includes(presetN / 60)) {
      return { unit: "hour", n: presetN / 60 };
    }
    // e.g. the legacy "@preset:1440" daily sentinel — no fire time to
    // surface, so let the cron tab render it verbatim.
    return null;
  }
  const c = parseCronExpr(expr);
  if (!c || c.month !== "*" || c.dow !== "*") return null;

  // every N minutes: "*/N * * * *" or its even-list equivalent.
  // NOTE: "M * * * *" (hourly at a FIXED minute, the old Hourly tab's
  // output) deliberately does NOT match — IntervalSpec has no slot for the
  // minute phase, so treating it as "every 1 hour" would silently move the
  // firing minute on the next edit. It renders in the cron tab instead.
  if (c.hour === "*" && c.dom === "*") {
    const stepMin = detectStepMinute(c.minute);
    if (stepMin !== null && opts.minutes.includes(stepMin)) {
      return { unit: "minute", n: stepMin };
    }
    return null;
  }

  if (!/^\d+$/.test(c.minute)) return null;

  // every N hours at fixed minute: "M */N * * *" or even hour-list form
  if (c.dom === "*") {
    const stepHour = detectStepHour(c.hour);
    if (stepHour !== null && opts.hours.includes(stepHour)) {
      return { unit: "hour", n: stepHour };
    }
    // daily at HH:MM
    if (/^\d+$/.test(c.hour) && opts.days.includes(1)) {
      return {
        unit: "day",
        n: 1,
        hh: parseInt(c.hour, 10),
        mm: parseInt(c.minute, 10),
      };
    }
    return null;
  }

  // every N days at HH:MM: "M H */n * *"
  if (!/^\d+$/.test(c.hour)) return null;
  const domStep = c.dom.match(/^\*\/(\d+)$/);
  if (!domStep) return null;
  const n = parseInt(domStep[1], 10);
  if (!opts.days.includes(n)) return null;
  return {
    unit: "day",
    n,
    hh: parseInt(c.hour, 10),
    mm: parseInt(c.minute, 10),
  };
}

// ---------- helpers ----------

function pad2(n: number): string {
  return n.toString().padStart(2, "0");
}

/**
 * Detect "every N minutes" cadence in the minute field. Recognises:
 *   - "*\/N"     (the modern UI form)
 *   - "0,N,2N,…" (the offset list form emitted by the legacy
 *                 intervalToCronExpr migration; e.g. "7,37" for every 30m)
 * Returns the step in minutes when the values are evenly distributed across
 * the 0-59 range; returns null otherwise so the caller can fall through.
 */
function detectStepMinute(field: string): number | null {
  const star = field.match(/^\*\/(\d+)$/);
  if (star) {
    const n = parseInt(star[1], 10);
    return n > 0 && n <= 60 ? n : null;
  }
  return detectEvenList(field, 60);
}

/**
 * Detect "every N hours" cadence in the hour field. Mirrors detectStepMinute
 * but with a 24-hour range.
 */
function detectStepHour(field: string): number | null {
  const star = field.match(/^\*\/(\d+)$/);
  if (star) {
    const n = parseInt(star[1], 10);
    return n > 0 && n <= 24 ? n : null;
  }
  return detectEvenList(field, 24);
}

/**
 * If `field` is a comma-separated list of integers that are evenly spaced
 * across the [0, period) range, return the step. Returns null when the list
 * is irregular, single-valued (no step to report), or contains anything
 * other than bare integers.
 */
function detectEvenList(field: string, period: number): number | null {
  const parts = field.split(",");
  if (parts.length < 2) return null;
  const nums: number[] = [];
  for (const p of parts) {
    if (!/^\d+$/.test(p)) return null;
    const n = parseInt(p, 10);
    if (n < 0 || n >= period) return null;
    nums.push(n);
  }
  nums.sort((a, b) => a - b);
  const step = nums[1] - nums[0];
  if (step <= 0) return null;
  for (let i = 2; i < nums.length; i++) {
    if (nums[i] - nums[i - 1] !== step) return null;
  }
  // Wrap-around must also match — the gap from the last entry back to the
  // first (modulo period) has to equal step. Without this `0,15` would be
  // mis-reported as "every 15m" even though it really fires twice an hour.
  const wrap = period - nums[nums.length - 1] + nums[0];
  if (wrap !== step) return null;
  return step;
}

function clamp(n: number, lo: number, hi: number): number {
  if (Number.isNaN(n)) return lo;
  return Math.min(hi, Math.max(lo, n));
}

/**
 * Expand a DOW field into a numeric list. Accepts comma lists ("1,3,5"),
 * ranges ("1-5"), wildcards ("*"), and the union of the two ("1,3-5").
 * Returns null on anything that doesn't fit so the caller can fall back to
 * Advanced mode rather than guess.
 */
function expandDowField(field: string): number[] | null {
  if (field === "*") return [0, 1, 2, 3, 4, 5, 6];
  const out = new Set<number>();
  for (const part of field.split(",")) {
    if (/^\d+$/.test(part)) {
      const n = parseInt(part, 10);
      if (n < 0 || n > 7) return null;
      out.add(n === 7 ? 0 : n); // cron treats 7 as Sunday
      continue;
    }
    const range = part.match(/^(\d+)-(\d+)$/);
    if (!range) return null;
    const lo = parseInt(range[1], 10);
    const hi = parseInt(range[2], 10);
    if (lo > hi || lo < 0 || hi > 7) return null;
    for (let i = lo; i <= hi; i++) {
      out.add(i === 7 ? 0 : i);
    }
  }
  return Array.from(out).sort((a, b) => a - b);
}

/**
 * Render a list of DOWs as a hyphenated range when it's contiguous, or as
 * a comma list otherwise. "0-6" is special-cased to "毎日" so the weekly
 * output doesn't read as "Sun-Sat" when the user actually picked all days.
 */
function formatDowList(dows: number[]): string {
  const ja = getLocale() === "ja";
  const labels = ja ? DOW_LABELS_JA : DOW_LABELS_EN;
  if (dows.length === 7) return ja ? "毎日" : "every day";
  if (dows.length === 0) return "";
  // detect contiguous
  let contiguous = true;
  for (let i = 1; i < dows.length; i++) {
    if (dows[i] !== dows[i - 1] + 1) {
      contiguous = false;
      break;
    }
  }
  if (contiguous && dows.length > 1) {
    return `${labels[dows[0]]}-${labels[dows[dows.length - 1]]}`;
  }
  return dows.map((d) => labels[d]).join(",");
}

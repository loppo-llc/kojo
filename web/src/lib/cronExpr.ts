// cronExpr.ts — client-side helpers for the strict 5-field cron expressions
// the kojo backend now persists. Mirrors the Go-side ValidateCronExpr surface
// so the editor can give immediate feedback without round-tripping to the
// server. Only matches cron forms that the simple-mode UI can faithfully
// render; anything else falls back to "Advanced" + a verbatim string echo.

export interface CronStruct {
  minute: string;
  hour: string;
  dom: string;
  month: string;
  dow: string;
}

export type SimpleMode = "preset" | "everyN" | "hourly" | "daily" | "weekly";

export interface SimpleSpec {
  mode: SimpleMode;
  // preset: matches one of SCHEDULE_PRESETS labels
  presetCron?: string;
  // everyN: minute interval (5/10/30) or hour interval (1/3/6/12)
  everyN?: number;
  everyUnit?: "minute" | "hour";
  // hourly: minute (0..59)
  hourlyMinute?: number;
  // daily: hh / mm
  hh?: number;
  mm?: number;
  // weekly: numeric days-of-week (0=Sun..6=Sat) plus hh/mm
  dows?: number[];
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
// The Advanced editor uses this to reject "@preset:7" or any other value
// the server would refuse, so the user gets immediate feedback instead of
// a 400 on Save.
const CRON_PRESET_ALLOWED = new Set([5, 10, 30, 60, 180, 360, 720, 1440]);

/**
 * Returns true when `currentExpr` represents the same cadence as `presetCron`.
 * Used by the Preset chip strip to highlight the chip the user originally
 * picked even after a Save round-trip — the persisted form is the per-agent-
 * offset expansion (e.g. "7,37 * * * *"), so a literal === comparison would
 * never match the sentinel ("@preset:30") that the chip carries.
 *
 * Match rules:
 *   - empty preset → only matches empty currentExpr
 *   - "@preset:N" sentinel → matches when currentExpr resolves to the same
 *     N via detectStepMinute / detectStepHour (handles both "*\/N" and the
 *     "M1,M2,…" offset-list form intervalToCronExpr emits)
 *   - literal cron (e.g. "0 9 * * *") → exact string equality
 */
export function cronEquivalentToPreset(
  currentExpr: string,
  presetCron: string,
): boolean {
  if (presetCron === "") return currentExpr === "";
  if (currentExpr === presetCron) return true;
  const presetN = parseCronPresetSentinel(presetCron);
  if (presetN === null) return false;
  const c = parseCronExpr(currentExpr);
  if (!c) return false;
  if (presetN < 60) {
    if (c.hour !== "*" || c.dom !== "*" || c.month !== "*" || c.dow !== "*") return false;
    return detectStepMinute(c.minute) === presetN;
  }
  if (presetN === 1440) {
    // Daily: a single fixed M H * * *
    if (!/^\d+$/.test(c.minute) || !/^\d+$/.test(c.hour)) return false;
    return c.dom === "*" && c.month === "*" && c.dow === "*";
  }
  // 60..720: every-N hours at fixed minute
  if (!/^\d+$/.test(c.minute) || c.dom !== "*" || c.month !== "*" || c.dow !== "*") return false;
  if (presetN === 60) {
    // intervalToCronExpr emits "M 0,1,2,...,23 * * *" for the 1h case
    // (hours=1 → hourList covers every hour), so a literal "*" check
    // misses the post-Save form. detectStepHour returns 1 for the
    // expanded list AND for the "*\/1" form, so it covers both shapes.
    return c.hour === "*" || detectStepHour(c.hour) === 1;
  }
  return detectStepHour(c.hour) === presetN / 60;
}

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

const DOW_LABELS = ["日", "月", "火", "水", "木", "金", "土"];

/**
 * Build a Japanese-language summary for the most common cron shapes. Falls
 * back to the raw expression in backticks when nothing matches so the user
 * still sees what's about to be saved.
 */
export function humanizeCron(expr: string): string {
  if (!expr) return "スケジュールなし";
  const presetN = parseCronPresetSentinel(expr);
  if (presetN !== null) {
    if (presetN < 60) return `${presetN} 分おき (per-agent offset)`;
    if (presetN === 1440) return `毎日 (per-agent offset)`;
    if (presetN % 60 === 0 && presetN < 1440) {
      return `${presetN / 60} 時間おき (per-agent offset)`;
    }
    return `${presetN} 分おき (per-agent offset)`;
  }
  const c = parseCronExpr(expr);
  if (!c) return `cron: \`${expr}\``;

  // Sub-hourly cadence: "*/N * * * *" — also recognise the equivalent
  // offset-list form ("7,37 * * * *") that the legacy intervalToCron
  // migration emits. detectEvenStep returns the step when the list is
  // evenly spaced over a 60-minute period.
  if (c.hour === "*" && c.dom === "*" && c.month === "*" && c.dow === "*") {
    const stepMin = detectStepMinute(c.minute);
    if (stepMin !== null) return `${stepMin} 分おき`;
  }

  // Every-N hours at fixed minute: "M */N * * *" — likewise recognise
  // "M H1,H2,…" with even hour spacing.
  if (/^\d+$/.test(c.minute) && c.dom === "*" && c.month === "*" && c.dow === "*") {
    const stepHour = detectStepHour(c.hour);
    if (stepHour !== null) {
      return `${stepHour} 時間おき (${pad2(parseInt(c.minute, 10))} 分)`;
    }
  }

  // Hourly at minute M: "M * * * *"
  if (/^\d+$/.test(c.minute) && c.hour === "*" && c.dom === "*" && c.month === "*" && c.dow === "*") {
    return `毎時 ${pad2(parseInt(c.minute, 10))} 分`;
  }

  // Daily at HH:MM (any DOW pattern restricted to *)
  if (/^\d+$/.test(c.minute) && /^\d+$/.test(c.hour) && c.dom === "*" && c.month === "*" && c.dow === "*") {
    return `毎日 ${pad2(parseInt(c.hour, 10))}:${pad2(parseInt(c.minute, 10))}`;
  }

  // Weekly at HH:MM on a list/range of DOWs.
  if (/^\d+$/.test(c.minute) && /^\d+$/.test(c.hour) && c.dom === "*" && c.month === "*") {
    const dows = expandDowField(c.dow);
    if (dows && dows.length > 0) {
      const label = formatDowList(dows);
      return `毎週 ${label} ${pad2(parseInt(c.hour, 10))}:${pad2(parseInt(c.minute, 10))}`;
    }
  }

  return `cron: \`${expr}\``;
}

/**
 * Generate a cron expression from one of the simple-mode primitives the UI
 * exposes. Used by the tab editor so users never have to type cron syntax
 * for the common cases.
 */
export function cronFromSimple(
  mode: "everyN" | "hourly" | "daily" | "weekly",
  params: {
    everyN?: number;
    everyUnit?: "minute" | "hour";
    minute?: number;
    hh?: number;
    mm?: number;
    dows?: number[];
  },
): string {
  switch (mode) {
    case "everyN": {
      const n = params.everyN ?? 5;
      if (params.everyUnit === "hour") {
        return `0 */${n} * * *`;
      }
      return `*/${n} * * * *`;
    }
    case "hourly": {
      const m = clamp(params.minute ?? 0, 0, 59);
      return `${m} * * * *`;
    }
    case "daily": {
      const h = clamp(params.hh ?? 9, 0, 23);
      const m = clamp(params.mm ?? 0, 0, 59);
      return `${m} ${h} * * *`;
    }
    case "weekly": {
      const h = clamp(params.hh ?? 9, 0, 23);
      const m = clamp(params.mm ?? 0, 0, 59);
      const dows = (params.dows ?? []).filter((d) => d >= 0 && d <= 6);
      // Empty selection used to fall through to "*" — that fired the job
      // every day, the opposite of what "I deselected everything" means.
      // Emit "" (= scheduling disabled) so the user gets the obvious result
      // and the editor can flag it before save.
      if (dows.length === 0) return "";
      const dowField = dows.slice().sort((a, b) => a - b).join(",");
      return `${m} ${h} * * ${dowField}`;
    }
  }
}

/**
 * Look at an existing cron expression and decide which simple-mode tab
 * should pre-select it. Returns null when nothing matches — the caller
 * falls through to the Advanced tab and shows the raw string.
 */
export function detectSimpleMode(
  expr: string,
  presets: ReadonlyArray<{ label: string; cron: string }>,
): SimpleSpec | null {
  if (!expr) {
    return { mode: "preset", presetCron: "" };
  }
  for (const p of presets) {
    if (p.cron === expr) return { mode: "preset", presetCron: expr };
  }
  const c = parseCronExpr(expr);
  if (!c) return null;

  // everyN minutes: "*/N * * * *" or its even-list equivalent ("7,37 * * * *")
  if (c.hour === "*" && c.dom === "*" && c.month === "*" && c.dow === "*") {
    const stepMin = detectStepMinute(c.minute);
    if (stepMin !== null) {
      return { mode: "everyN", everyN: stepMin, everyUnit: "minute" };
    }
  }

  // everyN hours at minute=N: "M */N * * *" or even hour-list equivalent
  if (/^\d+$/.test(c.minute) && c.dom === "*" && c.month === "*" && c.dow === "*") {
    const stepHour = detectStepHour(c.hour);
    if (stepHour !== null) {
      return { mode: "everyN", everyN: stepHour, everyUnit: "hour" };
    }
  }

  // hourly at minute=M: "M * * * *"
  if (/^\d+$/.test(c.minute) && c.hour === "*" && c.dom === "*" && c.month === "*" && c.dow === "*") {
    return { mode: "hourly", hourlyMinute: parseInt(c.minute, 10) };
  }

  // daily at HH:MM
  if (/^\d+$/.test(c.minute) && /^\d+$/.test(c.hour) && c.dom === "*" && c.month === "*" && c.dow === "*") {
    return { mode: "daily", hh: parseInt(c.hour, 10), mm: parseInt(c.minute, 10) };
  }

  // weekly: HH:MM on listed DOWs
  if (/^\d+$/.test(c.minute) && /^\d+$/.test(c.hour) && c.dom === "*" && c.month === "*") {
    const dows = expandDowField(c.dow);
    if (dows && dows.length > 0) {
      return {
        mode: "weekly",
        hh: parseInt(c.hour, 10),
        mm: parseInt(c.minute, 10),
        dows,
      };
    }
  }

  return null;
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
  if (dows.length === 7) return "毎日";
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
    return `${DOW_LABELS[dows[0]]}-${DOW_LABELS[dows[dows.length - 1]]}`;
  }
  return dows.map((d) => DOW_LABELS[d]).join(",");
}

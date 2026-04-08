import { useState, useEffect } from "react";
import { INTERVAL_PRESETS, TIMEOUT_PRESETS } from "../../lib/agentApi";

interface Props {
  intervalMinutes: number;
  onIntervalChange: (v: number) => void;
  timeoutMinutes: number;
  onTimeoutChange: (v: number) => void;
  activeStart: string;
  activeEnd: string;
  onActiveStartChange: (v: string) => void;
  onActiveEndChange: (v: string) => void;
}

/** Parse "HH:MM" to minutes since midnight. */
function toMinutes(hhmm: string): number {
  const [h, m] = hhmm.split(":").map(Number);
  return h * 60 + (m || 0);
}

/** Build CSS gradient showing the active window on a 24h bar. */
function timelineGradient(start: string, end: string): string {
  const active = "rgb(217 119 6 / 0.5)"; // amber-600/50
  const inactive = "rgb(38 38 38)"; // neutral-800

  if (!start || !end) {
    return active; // no restriction = always active
  }

  const s = (toMinutes(start) / 1440) * 100;
  const e = (toMinutes(end) / 1440) * 100;

  if (s <= e) {
    // Normal: inactive | active | inactive
    return `linear-gradient(to right, ${inactive} ${s}%, ${active} ${s}%, ${active} ${e}%, ${inactive} ${e}%)`;
  }
  // Overnight: active | inactive | active
  return `linear-gradient(to right, ${active} ${e}%, ${inactive} ${e}%, ${inactive} ${s}%, ${active} ${s}%)`;
}

const HOUR_MARKS = [0, 3, 6, 9, 12, 15, 18, 21];

export function ScheduleEditor({
  intervalMinutes,
  onIntervalChange,
  timeoutMinutes,
  onTimeoutChange,
  activeStart,
  activeEnd,
  onActiveStartChange,
  onActiveEndChange,
}: Props) {
  // Separate enabled state so toggling off doesn't hide inputs mid-edit
  const [enabled, setEnabled] = useState(activeStart !== "" && activeEnd !== "");

  // Sync enabled state when props change (e.g., on load)
  useEffect(() => {
    setEnabled(activeStart !== "" && activeEnd !== "");
  }, [activeStart, activeEnd]);

  const toggleActiveHours = () => {
    if (enabled) {
      setEnabled(false);
      onActiveStartChange("");
      onActiveEndChange("");
    } else {
      setEnabled(true);
      onActiveStartChange(activeStart || "09:00");
      onActiveEndChange(activeEnd || "23:00");
    }
  };

  const localStart = activeStart || "09:00";
  const localEnd = activeEnd || "23:00";

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

      {/* Timeout */}
      {intervalMinutes > 0 && (
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
            Max duration for each scheduled run.
          </p>
        </div>
      )}

      {/* Active Hours */}
      {intervalMinutes > 0 && (
        <div>
          <div className="flex items-center justify-between mb-2">
            <label className="text-sm text-neutral-400">Active Hours</label>
            <button
              type="button"
              onClick={toggleActiveHours}
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
                    onChange={(e) => onActiveStartChange(e.target.value)}
                    className="w-full px-2.5 py-1.5 bg-neutral-900 border border-neutral-700 rounded text-sm text-neutral-200 focus:outline-none focus:border-amber-700/60 [color-scheme:dark]"
                  />
                </div>
                <span className="text-neutral-600 mt-4">—</span>
                <div className="flex-1">
                  <label className="block text-[11px] text-neutral-500 mb-1">To</label>
                  <input
                    type="time"
                    value={localEnd}
                    onChange={(e) => onActiveEndChange(e.target.value)}
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
                  ? `Runs ${localStart}–${localEnd}, paused overnight. (server time)`
                  : `Runs ${localStart}–24:00 & 0:00–${localEnd} (overnight, server time).`}
              </p>
            </div>
          )}

          {!enabled && (
            <p className="text-[11px] text-neutral-600">
              Runs 24/7. Enable to restrict to specific hours.
            </p>
          )}
        </div>
      )}
    </div>
  );
}

import type { ContextInfo } from "../lib/api";

interface ContextBarProps {
  context: ContextInfo;
}

export function ContextBar({ context }: ContextBarProps) {
  const pct = Math.round(context.usagePercent);
  const threshold = 80;

  const barColor =
    pct >= 95
      ? "bg-red-600"
      : pct >= threshold
        ? "bg-yellow-600"
        : "bg-neutral-600";

  const textColor =
    pct >= 95
      ? "text-red-400"
      : pct >= threshold
        ? "text-yellow-400"
        : "text-neutral-500";

  const isCompacting = pct >= threshold;

  return (
    <div className="flex items-center gap-1.5" title={`Context: ${pct}% (${context.source})`}>
      <div className="w-16 h-1 bg-neutral-800 rounded-full overflow-hidden">
        <div
          className={`h-full rounded-full transition-all duration-300 ${barColor} ${isCompacting ? "animate-pulse" : ""}`}
          style={{ width: `${pct}%` }}
        />
      </div>
      <span className={`text-[10px] tabular-nums ${textColor}`}>{pct}%</span>
    </div>
  );
}

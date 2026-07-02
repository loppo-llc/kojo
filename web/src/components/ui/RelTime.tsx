import { timeAgo } from "../../lib/utils";

interface RelTimeProps {
  /** RFC3339 timestamp. */
  value: string;
  className?: string;
}

/** Right-aligned relative timestamp in mono ink-faint (11px by default). */
export function RelTime({ value, className = "" }: RelTimeProps) {
  return (
    <span className={`shrink-0 whitespace-nowrap font-mono text-[11px] text-ink-faint ${className}`}>
      {timeAgo(value)}
    </span>
  );
}

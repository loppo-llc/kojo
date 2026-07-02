interface ChipProps {
  children: React.ReactNode;
  className?: string;
  title?: string;
}

/**
 * A tiny mono meta pill (tool / model tags). Content never wraps
 * mid-token; when a caller allows shrinking (e.g. min-w-0 max-w-*),
 * the inner text truncates with an ellipsis instead of overflowing.
 * Callers add `shrink-0` when the chip must keep its full width.
 */
export function Chip({ children, className = "", title }: ChipProps) {
  return (
    <span
      title={title}
      className={`inline-flex items-center overflow-hidden whitespace-nowrap rounded-full border border-hairline px-1.5 font-mono text-ink-faint ${className}`}
      style={{ fontSize: "10.5px", lineHeight: 1.4 }}
    >
      <span className="truncate">{children}</span>
    </span>
  );
}

interface WordmarkProps {
  className?: string;
}

/**
 * The kojo wordmark: lowercase "kojo" in IBM Plex Mono 600 followed by
 * a copper block cursor that blinks (static under reduced-motion, which
 * the .animate-cursor-blink utility handles in index.css).
 */
export function Wordmark({ className = "" }: WordmarkProps) {
  return (
    <span className={`font-mono font-semibold lowercase tracking-tight text-ink select-none ${className}`}>
      kojo
      <span className="text-copper animate-cursor-blink" aria-hidden="true">
        ▮
      </span>
    </span>
  );
}

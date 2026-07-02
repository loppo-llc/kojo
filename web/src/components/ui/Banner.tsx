export type BannerTone = "success" | "error" | "warn" | "info";

const toneText: Record<BannerTone, string> = {
  success: "text-lamp-run",
  error: "text-lamp-err",
  warn: "text-lamp-warn",
  info: "text-ink-dim",
};

interface BannerProps {
  tone: BannerTone;
  children: React.ReactNode;
  /** Optional right-aligned action (e.g. a Retry button). */
  action?: React.ReactNode;
  className?: string;
}

/**
 * Thin status row: a surface strip with a hairline border and tone-colored
 * text. Replaces the old saturated red/green banner blocks.
 */
export function Banner({ tone, children, action, className = "" }: BannerProps) {
  return (
    <div
      className={`flex items-center justify-between gap-3 rounded-[10px] border border-hairline bg-surface px-3 py-2 text-[13px] ${toneText[tone]} ${className}`}
    >
      <span className="min-w-0 break-words">{children}</span>
      {action}
    </div>
  );
}

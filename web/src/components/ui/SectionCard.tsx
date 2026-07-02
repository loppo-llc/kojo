interface SectionCardProps {
  /** 13px/600 ink title in the card header. */
  title?: React.ReactNode;
  /** 12px ink-dim description under the title. */
  description?: React.ReactNode;
  /** Anchor target for section nav / scroll-spy. */
  id?: string;
  /** Right-aligned control on the header row (e.g. a toggle or action). */
  action?: React.ReactNode;
  /** Danger styling: lamp-err title + tinted border. */
  danger?: boolean;
  children: React.ReactNode;
  className?: string;
}

/**
 * A titled content card. surface bg, hairline border, rounded-10. Used to
 * group related form fields into a single scannable block. scroll-mt keeps
 * the header clear of the sticky page header when jumped to via anchor.
 */
export function SectionCard({
  title,
  description,
  id,
  action,
  danger = false,
  children,
  className = "",
}: SectionCardProps) {
  return (
    <section
      id={id}
      className={`scroll-mt-28 rounded-[10px] border bg-surface p-4 sm:p-5 ${
        danger ? "border-lamp-err/30" : "border-hairline"
      } ${className}`}
    >
      {(title || action) && (
        <div className="mb-4 flex items-start justify-between gap-3">
          <div className="min-w-0">
            {title && (
              <h2
                className={`text-[13px] font-semibold ${
                  danger ? "text-lamp-err" : "text-ink"
                }`}
              >
                {title}
              </h2>
            )}
            {description && (
              <p className="mt-1 text-[12px] text-ink-dim">{description}</p>
            )}
          </div>
          {action && <div className="shrink-0">{action}</div>}
        </div>
      )}
      {children}
    </section>
  );
}

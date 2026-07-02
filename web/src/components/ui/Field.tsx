interface FieldProps {
  /** 12px/500 ink-dim label above the control. */
  label?: React.ReactNode;
  /** Associates the label with a control id (optional). */
  htmlFor?: string;
  /** 12px ink-faint help text below the control. */
  help?: React.ReactNode;
  /** 12px lamp-err message below the control; also implies an error state. */
  error?: React.ReactNode;
  /** Right-aligned control on the label row (e.g. an override toggle). */
  action?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
}

/**
 * Label + control + help/error wrapper. Labels are always rendered (never
 * placeholder-as-label). The optional `action` slot sits opposite the
 * label for inline toggles.
 */
export function Field({
  label,
  htmlFor,
  help,
  error,
  action,
  children,
  className = "",
}: FieldProps) {
  return (
    <div className={className}>
      {(label || action) && (
        <div className="mb-1.5 flex items-center justify-between gap-2">
          {label ? (
            <label
              htmlFor={htmlFor}
              className="text-[12px] font-medium text-ink-dim"
            >
              {label}
            </label>
          ) : (
            <span />
          )}
          {action}
        </div>
      )}
      {children}
      {error ? (
        <p className="mt-1.5 text-[12px] text-lamp-err">{error}</p>
      ) : help ? (
        <p className="mt-1.5 text-[12px] text-ink-faint">{help}</p>
      ) : null}
    </div>
  );
}

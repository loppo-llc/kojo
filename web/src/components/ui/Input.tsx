import { forwardRef } from "react";

interface InputProps extends React.InputHTMLAttributes<HTMLInputElement> {
  /** Monospace variant for path / URL / cron / token inputs. */
  mono?: boolean;
  /** lamp-err border + focus ring (paired with Field's error message). */
  invalid?: boolean;
}

/**
 * Form text input primitive. raised bg, hairline border, copper focus
 * border (the global :focus-visible ring layers on top). Placeholder is
 * ink-faint — labels live in <Field>, never as placeholders.
 */
export const Input = forwardRef<HTMLInputElement, InputProps>(function Input(
  { mono = false, invalid = false, className = "", ...props },
  ref,
) {
  return (
    <input
      ref={ref}
      className={`w-full rounded-lg border bg-raised px-3 py-2 text-[14px] text-ink placeholder:text-ink-faint transition-colors disabled:opacity-50 ${
        invalid
          ? "border-lamp-err focus:border-lamp-err"
          : "border-hairline focus:border-copper"
      } ${mono ? "font-mono [color-scheme:dark]" : ""} ${className}`}
      {...props}
    />
  );
});

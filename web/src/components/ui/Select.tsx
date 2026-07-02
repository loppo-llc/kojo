import { forwardRef } from "react";

interface SelectProps
  extends React.SelectHTMLAttributes<HTMLSelectElement> {
  mono?: boolean;
}

/**
 * Native <select> styled to match the form primitives. A chevron is drawn
 * via background-image so the control reads as a dropdown without pulling
 * in an extra element. [color-scheme:dark] keeps the popup list dark.
 */
export const Select = forwardRef<HTMLSelectElement, SelectProps>(function Select(
  { mono = false, className = "", children, ...props },
  ref,
) {
  return (
    <select
      ref={ref}
      className={`w-full appearance-none rounded-lg border border-hairline bg-raised bg-[length:1rem] bg-[right_0.65rem_center] bg-no-repeat px-3 py-2 pr-9 text-[14px] text-ink transition-colors focus:border-copper disabled:opacity-50 [color-scheme:dark] ${
        mono ? "font-mono" : ""
      } ${className}`}
      style={{
        backgroundImage:
          "url(\"data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 20 20' fill='%23667080'%3E%3Cpath fill-rule='evenodd' d='M5.23 7.21a.75.75 0 011.06.02L10 11.17l3.71-3.94a.75.75 0 111.08 1.04l-4.25 4.5a.75.75 0 01-1.08 0l-4.25-4.5a.75.75 0 01.02-1.06z' clip-rule='evenodd'/%3E%3C/svg%3E\")",
      }}
      {...props}
    >
      {children}
    </select>
  );
});

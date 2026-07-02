import { forwardRef } from "react";

interface TextareaProps
  extends React.TextareaHTMLAttributes<HTMLTextAreaElement> {
  /** Monospace variant for prompt / template / config bodies. */
  mono?: boolean;
  invalid?: boolean;
}

/** Form textarea primitive — matches <Input> styling, resizes vertically. */
export const Textarea = forwardRef<HTMLTextAreaElement, TextareaProps>(
  function Textarea({ mono = false, invalid = false, className = "", ...props }, ref) {
    return (
      <textarea
        ref={ref}
        className={`w-full resize-y rounded-lg border bg-raised px-3 py-2 text-[14px] text-ink placeholder:text-ink-faint transition-colors disabled:opacity-50 ${
          invalid
            ? "border-lamp-err focus:border-lamp-err"
            : "border-hairline focus:border-copper"
        } ${mono ? "font-mono" : ""} ${className}`}
        {...props}
      />
    );
  },
);

interface ToggleProps {
  checked: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
  id?: string;
  "aria-label"?: string;
}

/**
 * 36×20 track with a 16px knob. Checked = copper fill, otherwise raised
 * with a hairline ring. Rendered as a role="switch" button so it's
 * keyboard-operable and screen-reader friendly.
 */
export function Toggle({
  checked,
  onChange,
  disabled = false,
  id,
  "aria-label": ariaLabel,
}: ToggleProps) {
  return (
    <button
      type="button"
      id={id}
      role="switch"
      aria-checked={checked}
      aria-label={ariaLabel}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={`relative inline-flex h-5 w-9 shrink-0 items-center rounded-full border transition-colors duration-150 after:absolute after:-inset-x-1 after:-inset-y-2.5 after:content-[''] disabled:opacity-40 motion-reduce:transition-none ${
        checked
          ? "border-copper bg-copper"
          : "border-hairline bg-raised"
      }`}
    >
      <span
        className={`inline-block h-4 w-4 rounded-full transition-transform duration-150 motion-reduce:transition-none ${
          checked ? "translate-x-[18px] bg-[#14100b]" : "translate-x-0.5 bg-ink-dim"
        }`}
      />
    </button>
  );
}

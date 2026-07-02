export type ButtonVariant = "primary" | "secondary" | "danger";

const variants: Record<ButtonVariant, string> = {
  // Copper fill with near-black ink at 600.
  primary: "bg-copper text-[#14100b] font-semibold hover:bg-copper-bright",
  secondary: "bg-raised border border-hairline text-ink hover:bg-hover",
  danger: "bg-raised text-lamp-err hover:bg-hover",
};

interface ButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
}

/**
 * Dashboard-scope button primitive. Other screens keep their existing
 * styling until later phases convert them.
 */
export function Button({ variant = "secondary", className = "", ...props }: ButtonProps) {
  return (
    <button
      className={`rounded-[10px] px-3 py-1.5 text-sm transition-colors disabled:pointer-events-none disabled:opacity-40 ${variants[variant]} ${className}`}
      {...props}
    />
  );
}

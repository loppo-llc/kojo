export type LampState = "run" | "warn" | "err" | "off";

const lampColor: Record<LampState, string> = {
  run: "bg-lamp-run",
  warn: "bg-lamp-warn",
  err: "bg-lamp-err",
  off: "bg-lamp-off",
};

interface LampProps {
  state: LampState;
  /** Slow opacity pulse — use for "running". Disabled under reduced motion. */
  pulse?: boolean;
  /** Diameter in px. */
  size?: number;
  className?: string;
}

/** A small status lamp dot. */
export function Lamp({ state, pulse = false, size = 8, className = "" }: LampProps) {
  return (
    <span
      aria-hidden="true"
      className={`inline-block shrink-0 rounded-full ${lampColor[state]} ${
        pulse ? "animate-lamp-pulse" : ""
      } ${className}`}
      style={{ width: size, height: size }}
    />
  );
}

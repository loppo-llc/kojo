import { SPECIAL_KEYS } from "../lib/keys";

interface SpecialKeysBarProps {
  ctrlMode: boolean;
  shiftMode: boolean;
  onKeyPress: (code: string) => void;
}

export function SpecialKeysBar({ ctrlMode, shiftMode, onKeyPress }: SpecialKeysBarProps) {
  return (
    <div className="flex gap-1.5 px-2 py-1.5 border-t border-neutral-800 overflow-x-auto shrink-0">
      {SPECIAL_KEYS.map((key) => (
        <button
          key={key.label}
          onPointerDown={(e) => e.preventDefault()}
          onClick={() => onKeyPress(key.code)}
          className={`px-4 py-2.5 text-sm rounded font-mono whitespace-nowrap ${
            (key.code === "ctrl" && ctrlMode) || (key.code === "shift" && shiftMode)
              ? "bg-blue-900 text-blue-300"
              : "bg-neutral-800 text-neutral-400 active:bg-neutral-600"
          }`}
        >
          {key.label}
        </button>
      ))}
    </div>
  );
}

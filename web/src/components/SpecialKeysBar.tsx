import { SPECIAL_KEYS, CTRL_LETTERS } from "../lib/keys";

interface SpecialKeysBarProps {
  ctrlMode: boolean;
  shiftMode: boolean;
  altMode?: boolean;
  onKeyPress: (code: string) => void;
}

export function SpecialKeysBar({ ctrlMode, shiftMode, altMode = false, onKeyPress }: SpecialKeysBarProps) {
  return (
    <div className="flex shrink-0 flex-col border-t border-hairline bg-app">
      {ctrlMode && (
        <div className="flex gap-1.5 overflow-x-auto px-2 py-1.5">
          {CTRL_LETTERS.map((key) => (
            <button
              key={key.label}
              onPointerDown={(e) => e.preventDefault()}
              onClick={() => onKeyPress(key.code)}
              className="whitespace-nowrap rounded-[10px] border border-copper/40 bg-copper/10 px-4 py-2.5 font-mono text-[13px] text-copper transition-colors active:bg-copper/20"
            >
              ^{key.label}
            </button>
          ))}
        </div>
      )}
      <div className="flex gap-1.5 overflow-x-auto px-2 py-1.5">
        {SPECIAL_KEYS.map((key) => {
          const active =
            (key.code === "ctrl" && ctrlMode) ||
            (key.code === "shift" && shiftMode) ||
            (key.code === "alt" && altMode);
          return (
            <button
              key={key.label}
              onPointerDown={(e) => e.preventDefault()}
              onClick={() => onKeyPress(key.code)}
              className={`whitespace-nowrap rounded-[10px] border px-4 py-2.5 font-mono text-[13px] transition-colors ${
                active
                  ? "border-copper/40 bg-copper/10 text-copper"
                  : "border-hairline bg-raised text-ink-dim active:bg-hover"
              }`}
            >
              {key.label}
            </button>
          );
        })}
      </div>
    </div>
  );
}

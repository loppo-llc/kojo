export interface SpecialKey {
  label: string;
  code: string;
}

export const SPECIAL_KEYS: SpecialKey[] = [
  { label: "Ctrl", code: "ctrl" },
  { label: "Esc", code: "\x1b" },
  { label: "Shift", code: "shift" },
  { label: "Tab", code: "\t" },
  { label: "\u2191", code: "\x1b[A" },
  { label: "\u2193", code: "\x1b[B" },
  { label: "\u2192", code: "\x1b[C" },
  { label: "\u2190", code: "\x1b[D" },
  { label: "Home", code: "\x1b[H" },
  { label: "End", code: "\x1b[F" },
  { label: "PgUp", code: "\x1b[5~" },
  { label: "PgDn", code: "\x1b[6~" },
  { label: "F1", code: "\x1bOP" },
  { label: "F2", code: "\x1bOQ" },
  { label: "F3", code: "\x1bOR" },
  { label: "F4", code: "\x1bOS" },
  { label: "F5", code: "\x1b[15~" },
  { label: "F6", code: "\x1b[17~" },
  { label: "F7", code: "\x1b[18~" },
  { label: "F8", code: "\x1b[19~" },
  { label: "F9", code: "\x1b[20~" },
  { label: "F10", code: "\x1b[21~" },
  { label: "F11", code: "\x1b[23~" },
  { label: "F12", code: "\x1b[24~" },
];

const SHIFT_MAP: Record<string, string> = {
  "\t": "\x1b[Z",
  "\x1b[A": "\x1b[1;2A",
  "\x1b[B": "\x1b[1;2B",
  "\x1b[C": "\x1b[1;2C",
  "\x1b[D": "\x1b[1;2D",
  "\x1b[H": "\x1b[1;2H",
  "\x1b[F": "\x1b[1;2F",
  "\x1b[5~": "\x1b[5;2~",
  "\x1b[6~": "\x1b[6;2~",
  "\x1bOP": "\x1b[1;2P",
  "\x1bOQ": "\x1b[1;2Q",
  "\x1bOR": "\x1b[1;2R",
  "\x1bOS": "\x1b[1;2S",
  "\x1b[15~": "\x1b[15;2~",
  "\x1b[17~": "\x1b[17;2~",
  "\x1b[18~": "\x1b[18;2~",
  "\x1b[19~": "\x1b[19;2~",
  "\x1b[20~": "\x1b[20;2~",
  "\x1b[21~": "\x1b[21;2~",
  "\x1b[23~": "\x1b[23;2~",
  "\x1b[24~": "\x1b[24;2~",
};

/**
 * Resolves a special key press with modifier state into the escape sequence to send.
 * Returns the sequence to send, or null if the key is a modifier toggle (ctrl/shift).
 */
export function resolveKeyPress(
  code: string,
  ctrlMode: boolean,
  shiftMode: boolean,
): string | null {
  if (code === "ctrl" || code === "shift") return null;

  if (ctrlMode) {
    const char = code.charCodeAt(0);
    if (char >= 97 && char <= 122) {
      return String.fromCharCode(char - 96);
    }
    return null;
  }

  if (shiftMode) {
    return SHIFT_MAP[code] ?? code.toUpperCase();
  }

  return code;
}

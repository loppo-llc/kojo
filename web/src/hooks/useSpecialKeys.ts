import { useCallback, useRef, useState } from "react";
import { resolveKeyPress } from "../lib/keys";

/**
 * Shared special-key handling used by SessionPage (CLI tab) and TerminalTab.
 * Manages ctrl/shift modifier state and resolves key codes to terminal sequences.
 */
export function useSpecialKeys(
  sendInput: (data: string) => void,
  autoScrollRef: React.RefObject<boolean>,
) {
  const [ctrlMode, setCtrlMode] = useState(false);
  const [shiftMode, setShiftMode] = useState(false);
  const ctrlModeRef = useRef(false);
  ctrlModeRef.current = ctrlMode;

  const clearModifiers = useCallback(() => {
    setCtrlMode(false);
    setShiftMode(false);
  }, []);

  const handleKeyPress = useCallback(
    (code: string) => {
      autoScrollRef.current = true;
      if (code === "ctrl") {
        setCtrlMode((prev) => {
          setShiftMode(false);
          return !prev;
        });
        return;
      }
      if (code === "shift") {
        setShiftMode((prev) => {
          setCtrlMode(false);
          return !prev;
        });
        return;
      }
      const seq = resolveKeyPress(code, ctrlMode, shiftMode);
      if (seq) sendInput(seq);
      setCtrlMode(false);
      setShiftMode(false);
    },
    [sendInput, autoScrollRef, ctrlMode, shiftMode],
  );

  /** Wraps a raw input handler to apply ctrlMode conversion for xterm onData. */
  const wrapInput = useCallback(
    (data: string): string => {
      if (!ctrlModeRef.current) return data;
      // Only convert single-character input; paste/IME compositions pass through
      if (data.length === 1) {
        const ch = data.toLowerCase().charCodeAt(0);
        if (ch >= 97 && ch <= 122) {
          clearModifiers();
          return String.fromCharCode(ch - 96);
        }
      }
      // Any input clears ctrlMode (one-shot modifier)
      clearModifiers();
      return data;
    },
    [clearModifiers],
  );

  return { ctrlMode, shiftMode, handleKeyPress, clearModifiers, wrapInput };
}

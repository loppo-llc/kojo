import { useCallback, useRef, useState } from "react";
import { resolveKeyPress } from "../lib/keys";

/**
 * Shared special-key handling used by SessionPage (CLI tab) and TerminalTab.
 * Manages ctrl/shift/alt modifier state and resolves key codes to terminal sequences.
 */
export function useSpecialKeys(
  sendInput: (data: string) => void,
  autoScrollRef: React.RefObject<boolean>,
) {
  const [ctrlMode, setCtrlMode] = useState(false);
  const [shiftMode, setShiftMode] = useState(false);
  const [altMode, setAltMode] = useState(false);
  const ctrlModeRef = useRef(false);
  ctrlModeRef.current = ctrlMode;
  const altModeRef = useRef(false);
  altModeRef.current = altMode;

  const clearModifiers = useCallback(() => {
    setCtrlMode(false);
    setShiftMode(false);
    setAltMode(false);
  }, []);

  const handleKeyPress = useCallback(
    (code: string) => {
      autoScrollRef.current = true;
      if (code === "ctrl") {
        setCtrlMode((prev) => {
          setShiftMode(false);
          setAltMode(false);
          return !prev;
        });
        return;
      }
      if (code === "shift") {
        setShiftMode((prev) => {
          setCtrlMode(false);
          setAltMode(false);
          return !prev;
        });
        return;
      }
      if (code === "alt") {
        setAltMode((prev) => {
          setCtrlMode(false);
          setShiftMode(false);
          return !prev;
        });
        return;
      }
      const seq = resolveKeyPress(code, ctrlMode, shiftMode, altMode);
      if (seq) sendInput(seq);
      setCtrlMode(false);
      setShiftMode(false);
      setAltMode(false);
    },
    [sendInput, autoScrollRef, ctrlMode, shiftMode, altMode],
  );

  /** Wraps a raw input handler to apply ctrl/alt mode conversion for xterm onData. */
  const wrapInput = useCallback(
    (data: string): string => {
      if (ctrlModeRef.current) {
        // Only convert single-character input; paste/IME compositions pass through
        if (data.length === 1) {
          const ch = data.toLowerCase().charCodeAt(0);
          if (ch >= 97 && ch <= 122) {
            clearModifiers();
            return String.fromCharCode(ch - 96);
          }
        }
        clearModifiers();
        return data;
      }
      if (altModeRef.current) {
        if (data.length === 1) {
          clearModifiers();
          return "\x1b" + data;
        }
        clearModifiers();
        return data;
      }
      return data;
    },
    [clearModifiers],
  );

  return { ctrlMode, shiftMode, altMode, handleKeyPress, clearModifiers, wrapInput };
}

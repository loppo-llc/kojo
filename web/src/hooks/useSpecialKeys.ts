import { useCallback, useState } from "react";
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

  return { ctrlMode, shiftMode, handleKeyPress, clearModifiers };
}

import { useCallback, useLayoutEffect, useRef } from "react";

/** Max composer height in px before the textarea starts scrolling. */
const MAX_HEIGHT = 150;

/**
 * A textarea ref plus a `resize()` that grows the element to fit its content
 * up to MAX_HEIGHT. Use `resize` as the onInput handler for interactive typing.
 *
 * Pass the bound value so the height also re-fits on programmatic updates —
 * a draft restored from storage when the user navigates back would otherwise
 * render in a collapsed single-row textarea (the DOM value is set without an
 * input event) until the next keystroke. The useLayoutEffect runs the recalc
 * before paint on mount and on every value change, so there's no collapsed
 * flash.
 */
export function useAutoGrowTextarea(value?: string) {
  const textareaRef = useRef<HTMLTextAreaElement>(null);

  const resize = useCallback(() => {
    if (textareaRef.current) {
      textareaRef.current.style.height = "auto";
      textareaRef.current.style.height = Math.min(textareaRef.current.scrollHeight, MAX_HEIGHT) + "px";
    }
  }, []);

  useLayoutEffect(() => {
    resize();
  }, [value, resize]);

  return { textareaRef, resize };
}

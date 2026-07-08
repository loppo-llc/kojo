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
 * input event) until the next keystroke.
 *
 * `textareaRef` is a callback ref (not a plain ref object) on purpose: the
 * composer often mounts LATER than the component that calls this hook — e.g.
 * AgentChat returns null until the agent row loads — so a value-keyed
 * layout effect alone fires while the node is still null and never re-runs
 * when the textarea finally attaches. The callback ref resizes at the exact
 * moment the node appears, and the layout effect covers subsequent value
 * changes.
 */
export function useAutoGrowTextarea(value?: string) {
  const nodeRef = useRef<HTMLTextAreaElement | null>(null);

  const fit = useCallback((el: HTMLTextAreaElement) => {
    el.style.height = "auto";
    el.style.height = Math.min(el.scrollHeight, MAX_HEIGHT) + "px";
  }, []);

  const resize = useCallback(() => {
    if (nodeRef.current) fit(nodeRef.current);
  }, [fit]);

  // Callback ref: fires when the textarea attaches (including late mounts
  // behind loading gates) and re-fits immediately.
  const textareaRef = useCallback(
    (el: HTMLTextAreaElement | null) => {
      nodeRef.current = el;
      if (el) fit(el);
    },
    [fit],
  ) as ((el: HTMLTextAreaElement | null) => void) & { current: HTMLTextAreaElement | null };

  // Keep a `.current` accessor for callers that read the node directly
  // (AgentChat reads/writes textareaRef.current in several handlers).
  Object.defineProperty(textareaRef, "current", {
    get: () => nodeRef.current,
    configurable: true,
  });

  useLayoutEffect(() => {
    resize();
  }, [value, resize]);

  return { textareaRef, resize };
}

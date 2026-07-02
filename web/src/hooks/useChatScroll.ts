import { useLayoutEffect, useRef } from "react";

// How close (px) to the bottom the viewport must be for a new message to
// pull it the rest of the way down. A reader scrolled further up than this
// is left where they are so an arrival (or a background poll) never yanks
// them off the content they're looking at.
const NEAR_BOTTOM_PX = 120;

/**
 * Canonical chat auto-scroll, extracted from AgentChat so both chat surfaces
 * share one implementation.
 *
 * Behavior:
 *   - opening a chat (first populated paint) jumps straight to the bottom;
 *   - a subsequent message change scrolls to the bottom only when the reader
 *     is already near it — otherwise the viewport is left untouched;
 *   - when `suppressAutoScrollRef` + `scrollRestoreRef` are set (pagination /
 *     refetch prepends older rows), the prior viewport offset is restored
 *     instead of scrolling, so the content under the reader stays put.
 *
 * The suppress/restore refs are returned so the pagination hook (and callers
 * that refetch, e.g. AgentChat's holder-peer recovery) can drive the restore
 * branch through the same layout effect.
 *
 * `resetKey` (the conversation id) clears the "first paint" latch so switching
 * conversations re-pins to the bottom.
 */
export function useChatScroll(messages: readonly { id: string }[], resetKey?: string) {
  const messagesEndRef = useRef<HTMLDivElement>(null);
  const scrollContainerRef = useRef<HTMLDivElement>(null);
  const suppressAutoScrollRef = useRef(false);
  const scrollRestoreRef = useRef<{ prevScrollHeight: number; prevScrollTop: number } | null>(null);
  const hasAutoScrolledRef = useRef(false);

  // New conversation → re-arm the initial jump-to-bottom.
  useLayoutEffect(() => {
    hasAutoScrolledRef.current = false;
  }, [resetKey]);

  useLayoutEffect(() => {
    const container = scrollContainerRef.current;

    // Restore branch: older rows were just prepended. Keep the reader anchored
    // to the same content by adding the height the prepend introduced.
    if (suppressAutoScrollRef.current && scrollRestoreRef.current) {
      if (container) {
        const { prevScrollHeight, prevScrollTop } = scrollRestoreRef.current;
        container.scrollTop = prevScrollTop + (container.scrollHeight - prevScrollHeight);
      }
      scrollRestoreRef.current = null;
      suppressAutoScrollRef.current = false;
      return;
    }
    // A caller may hold the suppress flag while it arranges its own restore
    // token (AgentChat's holder-peer refetch). Don't fight it here.
    if (suppressAutoScrollRef.current) return;
    if (!container || messages.length === 0) return;

    // First populated paint: jump (no smooth) so the chat opens at the bottom.
    if (!hasAutoScrolledRef.current) {
      hasAutoScrolledRef.current = true;
      messagesEndRef.current?.scrollIntoView();
      return;
    }

    // Otherwise follow only when the reader is already near the bottom.
    const distance = container.scrollHeight - container.scrollTop - container.clientHeight;
    if (distance <= NEAR_BOTTOM_PX) {
      messagesEndRef.current?.scrollIntoView({ behavior: "smooth" });
    }
  }, [messages]);

  return { messagesEndRef, scrollContainerRef, suppressAutoScrollRef, scrollRestoreRef };
}

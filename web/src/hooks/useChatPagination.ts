import { useCallback, useRef, type RefObject } from "react";

interface OlderPage<M> {
  messages: M[];
  hasMore: boolean;
}

interface UseChatPaginationArgs<M extends { id: string }> {
  /** Falsy (no conversation id) disables loading. */
  enabled: boolean;
  hasMore: boolean;
  messages: M[];
  scrollContainerRef: RefObject<HTMLDivElement | null>;
  suppressAutoScrollRef: RefObject<boolean>;
  scrollRestoreRef: RefObject<{ prevScrollHeight: number; prevScrollTop: number } | null>;
  /** Fetch the page older than `oldestId`. */
  fetchOlder: (oldestId: string) => Promise<OlderPage<M>>;
  setMessages: (updater: (prev: M[]) => M[]) => void;
  setHasMore: (hasMore: boolean) => void;
}

/**
 * Canonical "load older messages" pager, extracted from AgentChat so both
 * chat surfaces share it. Before prepending the fetched page it captures the
 * container's scroll height/top into `scrollRestoreRef` and raises
 * `suppressAutoScrollRef`; useChatScroll's layout effect then re-anchors the
 * viewport to the same content (no visible jump). A single in-flight load is
 * enforced via `loadingMoreRef`.
 */
export function useChatPagination<M extends { id: string }>({
  enabled,
  hasMore,
  messages,
  scrollContainerRef,
  suppressAutoScrollRef,
  scrollRestoreRef,
  fetchOlder,
  setMessages,
  setHasMore,
}: UseChatPaginationArgs<M>) {
  const loadingMoreRef = useRef(false);

  const loadOlderMessages = useCallback(async () => {
    if (!enabled || loadingMoreRef.current || !hasMore || messages.length === 0) return;
    loadingMoreRef.current = true;

    const oldestId = messages[0].id;

    try {
      const r = await fetchOlder(oldestId);
      setHasMore(r.hasMore);
      if (r.messages.length > 0) {
        const container = scrollContainerRef.current;
        suppressAutoScrollRef.current = true;
        scrollRestoreRef.current = {
          prevScrollHeight: container?.scrollHeight ?? 0,
          prevScrollTop: container?.scrollTop ?? 0,
        };
        setMessages((prev) => [...r.messages, ...prev]);
      }
    } catch (e) {
      console.error(e);
    } finally {
      loadingMoreRef.current = false;
    }
  }, [
    enabled,
    hasMore,
    messages,
    fetchOlder,
    setMessages,
    setHasMore,
    scrollContainerRef,
    suppressAutoScrollRef,
    scrollRestoreRef,
  ]);

  return { loadOlderMessages, loadingMoreRef };
}

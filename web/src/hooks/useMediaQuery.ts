import { useSyncExternalStore } from "react";

/**
 * Subscribe to a CSS media query. Uses useSyncExternalStore so the value
 * is read synchronously on first render (no post-mount flash) and stays in
 * sync on viewport changes. Client-only app (no SSR), so the server
 * snapshot simply returns false.
 */
export function useMediaQuery(query: string): boolean {
  return useSyncExternalStore(
    (onChange) => {
      const mql = window.matchMedia(query);
      mql.addEventListener("change", onChange);
      return () => mql.removeEventListener("change", onChange);
    },
    () => window.matchMedia(query).matches,
    () => false,
  );
}

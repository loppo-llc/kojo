import { useSyncExternalStore, useCallback } from "react";

// ── Keys ──

const ENTER_SENDS_KEY = "kojo:enterSends";

// ── Raw accessors ──

export function getEnterSends(): boolean {
  return localStorage.getItem(ENTER_SENDS_KEY) === "true";
}

export function setEnterSends(value: boolean): void {
  localStorage.setItem(ENTER_SENDS_KEY, String(value));
  // Notify all subscribers
  window.dispatchEvent(new StorageEvent("storage", { key: ENTER_SENDS_KEY }));
}

// ── React hook ──

/** Returns [enterSends, toggle] — reactive across all mounted components. */
export function useEnterSends(): [boolean, (v: boolean) => void] {
  const value = useSyncExternalStore(subscribeStorage, getEnterSends, () => false);
  const set = useCallback((v: boolean) => setEnterSends(v), []);
  return [value, set];
}

// ── Internal ──

function subscribeStorage(cb: () => void): () => void {
  const handler = (e: StorageEvent) => {
    if (e.key === ENTER_SENDS_KEY || e.key === null) cb();
  };
  window.addEventListener("storage", handler);
  return () => window.removeEventListener("storage", handler);
}

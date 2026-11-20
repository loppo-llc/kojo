import { useEffect, useSyncExternalStore } from "react";
import { agentApi, type STTConfig } from "./agentApi";

// Shared, reactive copy of the global voice-input (STT) config. The chat
// composer reads it to decide whether to render the mic button; Settings
// writes through setSTTConfig so open chats update without a reload.

let current: STTConfig | null = null;
let inflight: Promise<void> | null = null;
// Bumped on every write so a GET that started before a PUT can't clobber
// the newer value when it resolves late.
let version = 0;
const listeners = new Set<() => void>();

function emit() {
  listeners.forEach((l) => l());
}

function subscribe(cb: () => void): () => void {
  listeners.add(cb);
  return () => listeners.delete(cb);
}

/** Replace the cached config (e.g. with a PUT response) and notify. */
export function setSTTConfig(cfg: STTConfig): void {
  version++;
  current = cfg;
  emit();
}

/** Re-fetch the config from the server. Concurrent calls share one request. */
export function refreshSTTConfig(): Promise<void> {
  if (!inflight) {
    const started = version;
    inflight = agentApi.stt
      .getConfig()
      .then((cfg) => {
        if (version === started) setSTTConfig(cfg);
      })
      .catch(() => {
        // Leave the previous value; null stays null (mic hidden) until a
        // later refresh succeeds.
      })
      .finally(() => {
        inflight = null;
      });
  }
  return inflight;
}

/**
 * Returns the cached STT config (null until the first fetch resolves) and
 * refreshes it on mount so a key added/removed elsewhere is picked up.
 */
export function useSTTConfig(): STTConfig | null {
  const cfg = useSyncExternalStore(subscribe, () => current, () => null);
  useEffect(() => {
    void refreshSTTConfig();
  }, []);
  return cfg;
}

/** Test hook: drop the cached value. */
export function resetSTTConfigForTest(): void {
  current = null;
  inflight = null;
  version++;
  emit();
}

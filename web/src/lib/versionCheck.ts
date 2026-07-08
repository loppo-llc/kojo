// Stale-frontend detection.
//
// The frontend bundle bakes in its own build version (__KOJO_VERSION__,
// injected by Vite from the same `git describe` value the Go binary is
// stamped with — see vite.config.ts + Makefile). The agent chat
// WebSocket sends the running server version in its "connected"
// handshake frame. When a new server deploy lands, the WS reconnects and
// reports a version that no longer matches the loaded bundle — that's the
// exact moment to prompt a reload. No polling.
//
// We never auto-reload: a user may have an unsent draft. We surface a
// dismissible banner with a reload button and show it at most once per
// session per distinct server version.

import { useSyncExternalStore } from "react";

// Baked at build time. Empty/undefined in `vite dev` (no define), which
// (together with import.meta.env.DEV) suppresses the check in dev.
declare const __KOJO_VERSION__: string | undefined;

function clientVersion(): string {
  try {
    return typeof __KOJO_VERSION__ === "string" ? __KOJO_VERSION__ : "";
  } catch {
    return "";
  }
}

/**
 * Pure decision: should we prompt to reload?
 *
 * True only when both versions are non-empty, differ, and this server
 * version has not already been shown this session. Exported for unit
 * testing the mismatch logic without a store or DOM.
 */
export function shouldPromptReload(
  client: string,
  server: string,
  alreadyShown: ReadonlySet<string>,
): boolean {
  if (!client || !server) return false;
  if (client === server) return false;
  if (alreadyShown.has(server)) return false;
  return true;
}

const shown = new Set<string>();
let promptVersion: string | null = null;
const listeners = new Set<() => void>();

function notify(): void {
  for (const l of listeners) l();
}

/**
 * Feed a server version (from the WS "connected" frame) into the check.
 * Skips entirely in dev. Idempotent per server version per session.
 */
export function checkServerVersion(serverVersion: string | undefined): void {
  // import.meta.env is Vite-injected; cast since vite/client types aren't
  // pulled into this tsconfig.
  if ((import.meta as { env?: { DEV?: boolean } }).env?.DEV) return; // never nag in `vite dev`
  const server = serverVersion ?? "";
  const client = clientVersion();
  // A reconnect to a server that now matches our bundle (e.g. a rollback
  // to the build we're running) means the tab is no longer stale — retract
  // any showing prompt.
  if (client && server && client === server) {
    dismissReloadPrompt();
    return;
  }
  if (!shouldPromptReload(client, server, shown)) return;
  shown.add(server);
  promptVersion = server;
  notify();
}

/** Dismiss the banner for the current session (until a new version arrives). */
export function dismissReloadPrompt(): void {
  if (promptVersion === null) return;
  promptVersion = null;
  notify();
}

function subscribe(cb: () => void): () => void {
  listeners.add(cb);
  return () => listeners.delete(cb);
}

function getSnapshot(): string | null {
  return promptVersion;
}

/**
 * Subscribe a component to the reload-prompt state. Returns the server
 * version that triggered the prompt, or null when nothing to show.
 */
export function useReloadPrompt(): string | null {
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot);
}

// GitHub-release self-update prompt.
//
// Polls GET /api/v1/system/update on the local daemon (the daemon itself
// refreshes from GitHub every 6h — this poll never hits GitHub). When a
// newer release is available and the user has not dismissed that version
// this session, surface a banner via useSyncExternalStore. Update applies
// via POST and leaves the banner in a non-interactive "pending" state
// while the daemon drains/restarts; ReloadPrompt handles the post-restart
// bundle skew nudge. While pending, also poll GET /system/restart so an
// aborted drain (15-min timeout) can return the banner to an actionable
// error instead of stuck "updating".

import { useSyncExternalStore } from "react";
import { get, post } from "./httpClient";

export interface UpdateStatusResponse {
  supported: boolean;
  current?: string;
  latest?: string;
  updateAvailable?: boolean;
  notesUrl?: string;
  checkedAt?: string;
}

export type StartUpdateResult =
  | { kind: "pending"; from?: string; to?: string }
  | { kind: "up_to_date" }
  | { kind: "error"; message: string };

export type UpdatePromptPhase = "available" | "pending" | "error";

export interface UpdatePromptState {
  latest: string;
  notesUrl: string;
  supported: boolean;
  phase: UpdatePromptPhase;
  error?: string;
}

/**
 * Pure decision: should we surface the update banner for this latest tag?
 * True when the server reports an update, latest is non-empty, and that
 * version has not been dismissed this session.
 */
export function shouldPromptUpdate(
  updateAvailable: boolean,
  latest: string | undefined,
  dismissed: ReadonlySet<string>,
): boolean {
  if (!updateAvailable) return false;
  if (!latest) return false;
  if (dismissed.has(latest)) return false;
  return true;
}

const dismissed = new Set<string>();
let prompt: UpdatePromptState | null = null;
/** In-flight POST guard — ignore further startUpdate clicks while pending. */
let starting = false;
const listeners = new Set<() => void>();

/** While pending, poll restart status so an aborted drain is recoverable. */
const DRAIN_POLL_MS = 15_000;
let drainPollTimer: ReturnType<typeof setInterval> | null = null;

function notify(): void {
  for (const l of listeners) l();
}

function setPrompt(next: UpdatePromptState | null): void {
  prompt = next;
  if (next?.phase !== "pending") {
    clearDrainPoll();
  }
  notify();
}

function clearDrainPoll(): void {
  if (drainPollTimer !== null) {
    clearInterval(drainPollTimer);
    drainPollTimer = null;
  }
}

/**
 * While the banner is pending, poll GET /api/v1/system/restart. When
 * pending flips to false the drain aborted (success re-execs and this
 * fetch usually fails). Re-check GET /system/update: still available →
 * error so Update is actionable again; fetch failure → keep pending
 * (reconnect/reload path handles a real restart).
 */
async function pollDrainStatus(): Promise<void> {
  if (prompt?.phase !== "pending") {
    clearDrainPoll();
    return;
  }
  try {
    const st = await get<{ pending: boolean }>("/api/v1/system/restart");
    if (st.pending) return;
  } catch {
    // Server may be mid-restart — keep pending.
    return;
  }

  // Drain no longer pending. Confirm whether an update is still offered.
  try {
    const up = await get<UpdateStatusResponse>("/api/v1/system/update");
    if (prompt?.phase !== "pending") return;
    if (up.updateAvailable) {
      setPrompt({
        ...prompt,
        phase: "error",
        error: "restart did not complete; retry",
      });
      return;
    }
    // No longer available — treat as applied / no action needed.
    setPrompt(null);
  } catch {
    // Fetch failed (server actually restarting) — keep pending.
  }
}

function startDrainPoll(): void {
  clearDrainPoll();
  drainPollTimer = setInterval(() => {
    void pollDrainStatus();
  }, DRAIN_POLL_MS);
}

function extractErrorMessage(err: unknown): string {
  if (!(err instanceof Error)) return String(err);
  // httpClient throws `${status}: ${bodyText}`
  const m = err.message.match(/^\d+:\s*(.+)$/s);
  if (m?.[1]) {
    try {
      const j = JSON.parse(m[1]) as { error?: { message?: string } };
      if (j?.error?.message) return j.error.message;
    } catch {
      /* body wasn't JSON — fall through */
    }
    return m[1].trim();
  }
  return err.message;
}

/**
 * GET /api/v1/system/update and update the prompt store.
 * Silent on network/auth failures — the banner is optional chrome.
 */
export async function fetchUpdateStatus(): Promise<void> {
  // Don't clobber an in-progress update UI with a status poll.
  if (prompt?.phase === "pending") return;
  try {
    const st = await get<UpdateStatusResponse>("/api/v1/system/update");
    if (!shouldPromptUpdate(!!st.updateAvailable, st.latest, dismissed)) {
      // Retract when no longer applicable. Keep an error banner for the
      // same latest so the user can read/retry; clear if latest moved on.
      if (prompt?.phase === "error" && st.latest && prompt.latest === st.latest) {
        return;
      }
      if (prompt !== null) setPrompt(null);
      return;
    }
    const latest = st.latest!;
    // Keep error phase for this version (user can retry Update).
    if (prompt?.latest === latest && prompt.phase === "error") return;
    setPrompt({
      latest,
      notesUrl: st.notesUrl ?? "",
      supported: !!st.supported,
      phase: "available",
    });
  } catch {
    /* optional UI — ignore fetch failures */
  }
}

/**
 * POST /api/v1/system/update. Transitions the store for the component:
 * pending keeps the banner non-interactive; up_to_date clears it; error
 * keeps the banner with a message. Concurrent calls while starting or
 * already pending are ignored (returns the current pending result).
 */
export async function startUpdate(): Promise<StartUpdateResult> {
  if (starting || prompt?.phase === "pending") {
    return { kind: "pending" };
  }
  if (!prompt) {
    return { kind: "error", message: "no update available" };
  }
  starting = true;
  try {
    const res = await post<{
      status: string;
      from?: string;
      to?: string;
    }>("/api/v1/system/update", {});
    if (res.status === "up_to_date") {
      dismissed.add(prompt.latest);
      setPrompt(null);
      return { kind: "up_to_date" };
    }
    // pending | already_pending → same UX
    setPrompt({ ...prompt, phase: "pending", error: undefined });
    startDrainPoll();
    return { kind: "pending", from: res.from, to: res.to };
  } catch (err) {
    const message = extractErrorMessage(err);
    setPrompt({ ...prompt, phase: "error", error: message });
    return { kind: "error", message };
  } finally {
    starting = false;
  }
}

/** Dismiss the banner for the current latest version this session. */
export function dismissUpdatePrompt(): void {
  if (prompt === null) return;
  dismissed.add(prompt.latest);
  setPrompt(null);
}

function subscribe(cb: () => void): () => void {
  listeners.add(cb);
  return () => {
    listeners.delete(cb);
    // Unmount of the last subscriber: stop the drain poll.
    if (listeners.size === 0) {
      clearDrainPoll();
    }
  };
}

function getSnapshot(): UpdatePromptState | null {
  return prompt;
}

/**
 * Subscribe a component to the update-prompt state. Returns the prompt
 * payload, or null when nothing to show.
 */
export function useUpdatePrompt(): UpdatePromptState | null {
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot);
}

/** Current prompt snapshot (tests / non-React readers). */
export function getUpdatePromptSnapshot(): UpdatePromptState | null {
  return prompt;
}

/** Reset module state between tests. */
export function __resetUpdateCheckForTests(): void {
  clearDrainPoll();
  dismissed.clear();
  prompt = null;
  starting = false;
  notify();
}

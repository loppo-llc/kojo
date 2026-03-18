import type { Terminal } from "@xterm/xterm";

/**
 * Restore scrollback into a terminal: hide → reset → write → restore visibility.
 * Uses a safety timer to ensure the terminal becomes visible even if write() stalls.
 */
export function restoreScrollback(
  term: Terminal,
  data: Uint8Array,
  autoScrollRef: React.RefObject<boolean>,
): void {
  const el = term.element;
  if (el) el.style.visibility = "hidden";
  term.reset();
  let restored = false;
  const safetyTimer = setTimeout(() => restore(), 500);
  const restore = () => {
    if (restored) return;
    restored = true;
    clearTimeout(safetyTimer);
    autoScrollRef.current = true;
    term.scrollToBottom();
    if (el) el.style.visibility = "";
  };
  term.write(data, restore);
}

/** Decode a standard base64 string to Uint8Array. */
export function base64ToBytes(b64: string): Uint8Array {
  return Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
}

export function toBase64(str: string): string {
  return btoa(
    Array.from(new TextEncoder().encode(str), (b) => String.fromCharCode(b)).join(""),
  );
}

export function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(0)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/** Format a Date as RFC3339 with local timezone offset (e.g. +09:00). */
export function localRFC3339(d: Date = new Date()): string {
  const off = -d.getTimezoneOffset();
  const sign = off >= 0 ? "+" : "-";
  const hh = String(Math.floor(Math.abs(off) / 60)).padStart(2, "0");
  const mm = String(Math.abs(off) % 60).padStart(2, "0");
  const pad = (n: number) => String(n).padStart(2, "0");
  return (
    `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}` +
    `T${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}` +
    `${sign}${hh}:${mm}`
  );
}

export function timeAgo(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime();
  const mins = Math.floor(diff / 60000);
  if (mins < 1) return "just now";
  if (mins < 60) return `${mins}m ago`;
  const hrs = Math.floor(mins / 60);
  if (hrs < 24) return `${hrs}h ago`;
  return `${Math.floor(hrs / 24)}d ago`;
}

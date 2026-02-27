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

export function toBase64(str: string): string {
  return btoa(
    Array.from(new TextEncoder().encode(str), (b) => String.fromCharCode(b)).join(""),
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

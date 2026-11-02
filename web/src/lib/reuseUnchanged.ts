/**
 * Structural-sharing merge for polled message lists.
 *
 * A poll returns freshly deserialized objects every time, so even an
 * unchanged transcript would hand React a brand-new array of brand-new
 * objects — defeating memoized rows and re-running layout-reading effects
 * (auto-scroll) on every tick. This keeps the previous object for every
 * message whose serialized form is unchanged, and returns `prev` itself when
 * nothing changed at all, so a no-op poll is a no-op render.
 */
export function reuseUnchangedMessages<T extends { id: string }>(prev: readonly T[], next: T[]): T[] {
  if (prev.length === 0) return next.length === 0 ? (prev as T[]) : next;
  const byId = new Map<string, T>();
  for (const m of prev) byId.set(m.id, m);
  let identical = prev.length === next.length;
  const out = next.map((m, i) => {
    const old = byId.get(m.id);
    const kept = old !== undefined && (old === m || JSON.stringify(old) === JSON.stringify(m)) ? old : m;
    if (identical && kept !== prev[i]) identical = false;
    return kept;
  });
  return identical ? (prev as T[]) : out;
}

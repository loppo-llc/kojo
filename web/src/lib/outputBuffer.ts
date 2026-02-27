/**
 * RAF-based output buffer that batches Uint8Array chunks into a single write
 * per animation frame. Force-flushes when the buffer exceeds maxBytes to
 * prevent unbounded growth in background tabs where rAF is throttled/paused.
 *
 * Used by useWebSocket (main CLI session) and TerminalTab (tmux session).
 */

const DEFAULT_MAX_BYTES = 256 * 1024;

export interface OutputBuffer {
  /** Push a chunk into the buffer. Schedules or force-flushes as needed. */
  push(chunk: Uint8Array): void;
  /** Cancel any pending rAF, flush remaining data, and reset. */
  dispose(): void;
  /** Cancel pending rAF and clear the buffer without flushing. */
  clear(): void;
}

export function createOutputBuffer(
  onFlush: (data: Uint8Array) => void,
  maxBytes = DEFAULT_MAX_BYTES,
): OutputBuffer {
  let chunks: Uint8Array[] = [];
  let totalBytes = 0;
  let rafId = 0;

  function flush() {
    rafId = 0;
    if (chunks.length === 0) return;
    const pending = chunks;
    chunks = [];
    totalBytes = 0;
    if (pending.length === 1) {
      onFlush(pending[0]);
    } else {
      let total = 0;
      for (const c of pending) total += c.length;
      const merged = new Uint8Array(total);
      let off = 0;
      for (const c of pending) {
        merged.set(c, off);
        off += c.length;
      }
      onFlush(merged);
    }
  }

  function push(chunk: Uint8Array) {
    chunks.push(chunk);
    totalBytes += chunk.length;
    if (totalBytes >= maxBytes) {
      if (rafId) cancelAnimationFrame(rafId);
      flush();
    } else if (!rafId) {
      rafId = requestAnimationFrame(flush);
    }
  }

  function dispose() {
    if (rafId) {
      cancelAnimationFrame(rafId);
      rafId = 0;
    }
    flush();
    chunks = [];
    totalBytes = 0;
  }

  function clear() {
    if (rafId) {
      cancelAnimationFrame(rafId);
      rafId = 0;
    }
    chunks = [];
    totalBytes = 0;
  }

  return { push, dispose, clear };
}

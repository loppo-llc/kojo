import { useEffect, useRef, useCallback, useState } from "react";

interface WSMessage {
  type: string;
  data?: string;
  exitCode?: number;
  cols?: number;
  rows?: number;
  tail?: string;
  live?: boolean;
}

interface UseWebSocketOptions {
  sessionId: string;
  onOutput: (data: Uint8Array) => void;
  onScrollback: (data: Uint8Array) => void;
  onExit: (exitCode: number, live: boolean) => void;
  onYoloDebug?: (tail: string) => void;
}

function toBase64(str: string): string {
  return btoa(
    Array.from(new TextEncoder().encode(str), (b) => String.fromCharCode(b)).join(""),
  );
}

export function useWebSocket({ sessionId, onOutput, onScrollback, onExit, onYoloDebug }: UseWebSocketOptions) {
  const wsRef = useRef<WebSocket | null>(null);
  const [connected, setConnected] = useState(false);
  const reconnectRef = useRef<ReturnType<typeof setTimeout>>(null);
  const backoffRef = useRef(1000);
  const activeRef = useRef(true);

  // Batch output: accumulate chunks within a single animation frame to avoid
  // feeding split ANSI sequences into xterm.js across separate write() calls.
  // Force-flush when buffer exceeds 256KB to prevent unbounded growth in
  // background tabs where rAF is throttled/paused.
  const outputBufRef = useRef<Uint8Array[]>([]);
  const outputBufSizeRef = useRef(0);
  const rafRef = useRef(0);
  const maxBufBytes = 256 * 1024;

  const flushOutput = useCallback(() => {
    rafRef.current = 0;
    const chunks = outputBufRef.current;
    if (chunks.length === 0) return;
    outputBufRef.current = [];
    outputBufSizeRef.current = 0;
    if (chunks.length === 1) {
      onOutput(chunks[0]);
    } else {
      let total = 0;
      for (const c of chunks) total += c.length;
      const merged = new Uint8Array(total);
      let off = 0;
      for (const c of chunks) {
        merged.set(c, off);
        off += c.length;
      }
      onOutput(merged);
    }
  }, [onOutput]);

  const connect = useCallback(() => {
    if (!activeRef.current) return;

    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const url = `${proto}//${location.host}/api/v1/ws?session=${sessionId}`;
    const ws = new WebSocket(url);

    ws.onopen = () => {
      if (wsRef.current !== ws) return; // stale connection
      setConnected(true);
      backoffRef.current = 1000;
    };

    ws.onmessage = (evt) => {
      if (wsRef.current !== ws) return; // stale connection
      let msg: WSMessage;
      try {
        msg = JSON.parse(evt.data);
      } catch {
        return; // ignore invalid frames
      }
      switch (msg.type) {
        case "output":
          if (msg.data) {
            const bytes = Uint8Array.from(atob(msg.data), (c) => c.charCodeAt(0));
            outputBufRef.current.push(bytes);
            outputBufSizeRef.current += bytes.length;
            if (outputBufSizeRef.current >= maxBufBytes) {
              // Force-flush: buffer too large (e.g. background tab with rAF paused)
              if (rafRef.current) {
                cancelAnimationFrame(rafRef.current);
              }
              flushOutput();
            } else if (!rafRef.current) {
              rafRef.current = requestAnimationFrame(flushOutput);
            }
          }
          break;
        case "scrollback":
          if (msg.data) {
            const bytes = Uint8Array.from(atob(msg.data), (c) => c.charCodeAt(0));
            onScrollback(bytes);
          }
          break;
        case "exit":
          activeRef.current = false; // stop reconnecting on exit
          onExit(msg.exitCode ?? 0, msg.live === true);
          break;
        case "yolo_debug":
          if (msg.tail && onYoloDebug) onYoloDebug(msg.tail);
          break;
      }
    };

    ws.onclose = () => {
      if (wsRef.current !== ws) return; // superseded by new connection
      setConnected(false);
      if (!activeRef.current) return; // don't reconnect after unmount or exit
      const delay = Math.min(backoffRef.current, 30000);
      backoffRef.current = delay * 2;
      reconnectRef.current = setTimeout(connect, delay);
    };

    ws.onerror = () => {
      ws.close();
    };

    wsRef.current = ws;
  }, [sessionId, flushOutput, onScrollback, onExit, onYoloDebug]);

  useEffect(() => {
    activeRef.current = true;
    connect();
    return () => {
      activeRef.current = false;
      if (reconnectRef.current) clearTimeout(reconnectRef.current);
      if (rafRef.current) {
        cancelAnimationFrame(rafRef.current);
        rafRef.current = 0;
      }
      // Flush any pending output before teardown
      flushOutput();
      outputBufRef.current = [];
      outputBufSizeRef.current = 0;
      wsRef.current?.close();
    };
  }, [connect]);

  const sendInput = useCallback((data: string) => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify({ type: "input", data: toBase64(data) }));
    }
  }, []);

  const sendResize = useCallback((cols: number, rows: number) => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify({ type: "resize", cols, rows }));
    }
  }, []);

  const reconnect = useCallback(() => {
    if (reconnectRef.current) clearTimeout(reconnectRef.current);
    // Clear pending output from old connection
    if (rafRef.current) {
      cancelAnimationFrame(rafRef.current);
      rafRef.current = 0;
    }
    outputBufRef.current = [];
    outputBufSizeRef.current = 0;
    // Temporarily suppress onclose reconnect while we close the old socket
    activeRef.current = false;
    wsRef.current?.close();
    activeRef.current = true;
    backoffRef.current = 1000;
    connect();
  }, [connect]);

  return { connected, sendInput, sendResize, reconnect };
}

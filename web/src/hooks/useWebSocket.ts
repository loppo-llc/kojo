import { useEffect, useRef, useCallback, useState } from "react";
import { toBase64, base64ToBytes, wsUrl } from "../lib/utils";
import { createOutputBuffer, type OutputBuffer } from "../lib/outputBuffer";
import type { Attachment } from "../lib/api";

export interface WSMessage {
  type: string;
  data?: string;
  exitCode?: number;
  cols?: number;
  rows?: number;
  tail?: string;
  live?: boolean;
  attachments?: Attachment[];
}

interface UseWebSocketOptions {
  sessionId: string;
  // peerId stamps the `?peer=` query param on the WS upgrade so the
  // Hub's session-ws router forwards the upgrade to the right peer
  // (cross-peer session creation, NewSession's peer selector). Empty
  // = this host; the Hub serves the WS locally.
  peerId?: string;
  onOutput: (data: Uint8Array) => void;
  onScrollback: (data: Uint8Array) => void;
  onExit: (exitCode: number, live: boolean) => void;
  onYoloDebug?: (tail: string) => void;
  onAttachment?: (attachments: Attachment[]) => void;
  onConnected?: () => void;
}

export function useWebSocket({ sessionId, peerId, onOutput, onScrollback, onExit, onYoloDebug, onAttachment, onConnected }: UseWebSocketOptions) {
  const wsRef = useRef<WebSocket | null>(null);
  const [connected, setConnected] = useState(false);
  const reconnectRef = useRef<ReturnType<typeof setTimeout>>(null);
  const backoffRef = useRef(1000);
  const activeRef = useRef(true);
  const onConnectedRef = useRef(onConnected);
  onConnectedRef.current = onConnected;

  const bufRef = useRef<OutputBuffer | null>(null);
  if (!bufRef.current) {
    bufRef.current = createOutputBuffer((data) => onOutput(data));
  }

  const connect = useCallback(() => {
    if (!activeRef.current) return;

    const qs = peerId
      ? `session=${encodeURIComponent(sessionId)}&peer=${encodeURIComponent(peerId)}`
      : `session=${encodeURIComponent(sessionId)}`;
    const ws = new WebSocket(wsUrl(`/api/v1/ws?${qs}`));

    ws.onopen = () => {
      if (wsRef.current !== ws) return; // stale connection
      setConnected(true);
      backoffRef.current = 1000;
      onConnectedRef.current?.();
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
            const bytes = base64ToBytes(msg.data);
            bufRef.current!.push(bytes);
          }
          break;
        case "scrollback":
          if (msg.data) {
            const bytes = base64ToBytes(msg.data);
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
        case "attachment":
          if (msg.attachments && onAttachment) onAttachment(msg.attachments);
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
  }, [sessionId, peerId, onScrollback, onExit, onYoloDebug, onAttachment]);

  useEffect(() => {
    activeRef.current = true;
    connect();

    // Zombie-socket recovery: after a tab is backgrounded or the machine
    // sleeps, the underlying TCP connection can die without `onclose`
    // ever firing — the socket sits in readyState OPEN forever and the
    // terminal looks frozen until a manual reload. On wake, reconnect
    // immediately if the socket is already closed/closing/absent, or
    // force a reconnect on a suspect OPEN socket via the existing
    // reconnect() helper, which flips connected down and replaces the
    // socket right away instead of waiting on the dead peer's close
    // handshake (onclose may never fire, or only after a long timeout).
    let hiddenAt: number | null = null;

    const wake = (alwaysSuspect: boolean) => {
      if (!activeRef.current) return;
      const ws = wsRef.current;
      if (ws?.readyState === WebSocket.CONNECTING) return; // connect already in flight
      if (!ws || ws.readyState === WebSocket.CLOSED || ws.readyState === WebSocket.CLOSING) {
        // Route through reconnect() here too so a CLOSING socket whose
        // onclose hasn't fired yet gets the same teardown as a zombie
        // (connected flipped down, buffer cleared, backoff reset) — a
        // bare connect() would replace wsRef and let the stale-socket
        // guard swallow that late onclose without ever flipping state.
        reconnect();
        return;
      }
      if (ws.readyState === WebSocket.OPEN) {
        if (alwaysSuspect) {
          // 'online' fired — the network just changed underneath us
          // (sleep while visible, wifi switch). An OPEN readyState says
          // nothing about the old route still working; reconnect
          // unconditionally. 'online' is rare, so the churn is fine.
          reconnect();
          return;
        }
        const hiddenFor = hiddenAt != null ? Date.now() - hiddenAt : 0;
        // Only force-reconnect after at least one missed server ping
        // interval so a quick alt-tab doesn't churn a healthy connection.
        if (hiddenFor >= 30000) {
          reconnect();
        }
      }
    };

    const onVisibilityChange = () => {
      if (document.visibilityState === "hidden") {
        hiddenAt = Date.now();
      } else {
        wake(false);
        // Only the visible path clears hiddenAt — an 'online' event that
        // fires while still hidden must not erase the hide timestamp, or
        // a long hide would go undetected on the eventual visible flip.
        hiddenAt = null;
      }
    };
    const onOnline = () => wake(true);

    document.addEventListener("visibilitychange", onVisibilityChange);
    window.addEventListener("online", onOnline);

    return () => {
      activeRef.current = false;
      if (reconnectRef.current) clearTimeout(reconnectRef.current);
      document.removeEventListener("visibilitychange", onVisibilityChange);
      window.removeEventListener("online", onOnline);
      bufRef.current?.dispose();
      wsRef.current?.close();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
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
    bufRef.current?.clear();
    // Flip connected down immediately — the old socket may be a dead
    // peer whose close handshake never completes, and its onclose is
    // ignored anyway once connect() replaces wsRef (stale-socket guard).
    setConnected(false);
    // Temporarily suppress onclose reconnect while we close the old socket
    activeRef.current = false;
    wsRef.current?.close();
    activeRef.current = true;
    backoffRef.current = 1000;
    connect();
  }, [connect]);

  return { connected, sendInput, sendResize, reconnect };
}

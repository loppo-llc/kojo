import { useEffect, useRef, useCallback, useState } from "react";
import { toBase64 } from "../lib/utils";
import { createOutputBuffer, type OutputBuffer } from "../lib/outputBuffer";

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
  onConnected?: () => void;
}

export function useWebSocket({ sessionId, onOutput, onScrollback, onExit, onYoloDebug, onConnected }: UseWebSocketOptions) {
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

    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const url = `${proto}//${location.host}/api/v1/ws?session=${sessionId}`;
    const ws = new WebSocket(url);

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
            const bytes = Uint8Array.from(atob(msg.data), (c) => c.charCodeAt(0));
            bufRef.current!.push(bytes);
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
  }, [sessionId, onScrollback, onExit, onYoloDebug]);

  useEffect(() => {
    activeRef.current = true;
    connect();
    return () => {
      activeRef.current = false;
      if (reconnectRef.current) clearTimeout(reconnectRef.current);
      bufRef.current?.dispose();
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
    bufRef.current?.clear();
    // Temporarily suppress onclose reconnect while we close the old socket
    activeRef.current = false;
    wsRef.current?.close();
    activeRef.current = true;
    backoffRef.current = 1000;
    connect();
  }, [connect]);

  return { connected, sendInput, sendResize, reconnect };
}

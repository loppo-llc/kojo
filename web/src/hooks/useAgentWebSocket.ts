import { useEffect, useRef, useCallback, useState } from "react";
import type { AgentMessageAttachment, ChatEvent } from "../lib/agentApi";

interface UseAgentWebSocketOptions {
  agentId: string;
  onEvent: (event: ChatEvent) => void;
  onConnected?: () => void;
  onDisconnect?: () => void;
}

export function useAgentWebSocket({
  agentId,
  onEvent,
  onConnected,
  onDisconnect,
}: UseAgentWebSocketOptions) {
  const wsRef = useRef<WebSocket | null>(null);
  const [connected, setConnected] = useState(false);
  const reconnectRef = useRef<ReturnType<typeof setTimeout>>(null);
  const backoffRef = useRef(1000);
  const activeRef = useRef(true);
  const onEventRef = useRef(onEvent);
  onEventRef.current = onEvent;
  const onConnectedRef = useRef(onConnected);
  onConnectedRef.current = onConnected;
  const onDisconnectRef = useRef(onDisconnect);
  onDisconnectRef.current = onDisconnect;

  const connect = useCallback(() => {
    if (!activeRef.current) return;

    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const url = `${proto}//${location.host}/api/v1/agents/${agentId}/ws`;
    const ws = new WebSocket(url);

    ws.onopen = () => {
      if (wsRef.current !== ws) return;
      setConnected(true);
      backoffRef.current = 1000;
      onConnectedRef.current?.();
    };

    ws.onmessage = (evt) => {
      if (wsRef.current !== ws) return;
      try {
        const event: ChatEvent = JSON.parse(evt.data);
        onEventRef.current(event);
      } catch {
        // ignore
      }
    };

    ws.onclose = () => {
      if (wsRef.current !== ws) return;
      setConnected(false);
      onDisconnectRef.current?.();
      if (!activeRef.current) return;
      const delay = Math.min(backoffRef.current, 30000);
      backoffRef.current = delay * 2;
      reconnectRef.current = setTimeout(connect, delay);
    };

    ws.onerror = () => {
      ws.close();
    };

    wsRef.current = ws;
  }, [agentId]);

  useEffect(() => {
    activeRef.current = true;
    connect();
    return () => {
      activeRef.current = false;
      if (reconnectRef.current) clearTimeout(reconnectRef.current);
      wsRef.current?.close();
    };
  }, [connect]);

  const sendMessage = useCallback((content: string, attachments?: AgentMessageAttachment[]) => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      const msg: Record<string, unknown> = { type: "message", content };
      if (attachments && attachments.length > 0) {
        msg.attachments = attachments;
      }
      wsRef.current.send(JSON.stringify(msg));
    }
  }, []);

  const abort = useCallback(() => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify({ type: "abort" }));
    }
  }, []);

  return { connected, sendMessage, abort };
}

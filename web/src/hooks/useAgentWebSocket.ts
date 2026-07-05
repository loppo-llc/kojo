import { useEffect, useRef, useCallback, useState } from "react";
import type { AgentMessageAttachment, ChatEvent } from "../lib/agentApi";
import { wsUrl } from "../lib/utils";

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

    const ws = new WebSocket(wsUrl(`/api/v1/agents/${agentId}/ws`));

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
      if (!activeRef.current) return;
      setConnected(false);
      onDisconnectRef.current?.();
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

    // Zombie-socket recovery: after a tab is backgrounded or the machine
    // sleeps, the underlying TCP connection can die without the browser
    // ever firing `onclose` — the WebSocket sits in readyState OPEN
    // forever, so the onclose-driven reconnect below never triggers and
    // the chat looks frozen until a manual reload. We track how long the
    // page was hidden and, on wake, either reconnect immediately (socket
    // already closed/closing/absent) or tear down a suspect OPEN socket
    // and reconnect right away.
    let hiddenAt: number | null = null;

    // Don't wait for the dead peer's close handshake — onclose may never
    // fire (or only after a long TCP timeout), and meanwhile `connected`
    // would stay true and sends would silently drop. Flip state down,
    // detach the old socket's handlers so its late events are ignored,
    // fire-and-forget the close, and connect immediately.
    const forceReconnect = () => {
      if (reconnectRef.current) clearTimeout(reconnectRef.current);
      setConnected(false);
      onDisconnectRef.current?.();
      const ws = wsRef.current;
      wsRef.current = null;
      if (ws) {
        ws.onopen = null;
        ws.onmessage = null;
        ws.onclose = null;
        ws.onerror = null;
        ws.close();
      }
      backoffRef.current = 1000;
      connect();
    };

    const wake = (alwaysSuspect: boolean) => {
      if (!activeRef.current) return;
      const ws = wsRef.current;
      if (ws?.readyState === WebSocket.CONNECTING) return; // connect already in flight
      if (!ws || ws.readyState === WebSocket.CLOSED || ws.readyState === WebSocket.CLOSING) {
        // Route through forceReconnect here too: for a CLOSING socket
        // whose onclose hasn't fired yet, a bare connect() would replace
        // wsRef so the stale-socket guard swallows that onclose and
        // onDisconnect (stream reset) never runs — replayed deltas from
        // resumeBackgroundChat would then stack on stale stream state.
        // For an already-CLOSED socket the extra onDisconnect is
        // harmless (resetStream is idempotent).
        forceReconnect();
        return;
      }
      if (ws.readyState === WebSocket.OPEN) {
        if (alwaysSuspect) {
          // 'online' fired — the network just changed underneath us
          // (sleep while visible, wifi switch). An OPEN readyState says
          // nothing about the old route still working; reconnect
          // unconditionally. 'online' is rare, so the churn is fine.
          forceReconnect();
          return;
        }
        const hiddenFor = hiddenAt != null ? Date.now() - hiddenAt : 0;
        // Server pings every 30s (internal/server/agent_ws.go) — only
        // force-reconnect after at least one missed ping interval so a
        // quick alt-tab doesn't churn a perfectly healthy connection.
        if (hiddenFor >= 30000) {
          forceReconnect();
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
      const ws = wsRef.current;
      if (!ws) return;
      ws.onopen = null;
      ws.onmessage = null;
      ws.onclose = null;
      ws.onerror = null;
      if (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING) {
        ws.close(1000, "route change");
      }
      if (wsRef.current === ws) {
        wsRef.current = null;
      }
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

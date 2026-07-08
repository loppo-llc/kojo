import { useEffect, useRef, useCallback, useState } from "react";
import type { AgentMessageAttachment, ChatEvent } from "../lib/agentApi";
import { wsUrl } from "../lib/utils";
import { checkServerVersion } from "../lib/versionCheck";

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
  // Timestamp of the last frame received on the current socket. ANY frame
  // counts (chat events AND the server's ~20s heartbeat), so a healthy but
  // idle connection still refreshes this. The watchdog below reads it to
  // detect a zombie socket the browser never reported closed.
  const lastMsgRef = useRef(Date.now());
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
      lastMsgRef.current = Date.now();
      setConnected(true);
      backoffRef.current = 1000;
      onConnectedRef.current?.();
    };

    ws.onmessage = (evt) => {
      if (wsRef.current !== ws) return;
      // Any inbound frame proves the socket is alive — refresh the
      // watchdog timestamp before anything else (cheap: one ref write).
      lastMsgRef.current = Date.now();
      try {
        const event: ChatEvent = JSON.parse(evt.data);
        // Connection handshake: the server's running version. Emitted
        // once per connection (including every reconnect after a deploy).
        // Feed it to the stale-frontend check and swallow it — nothing to
        // render. Old clients lacking this branch fall through to onEvent,
        // whose switch ignores the unknown "connected" type harmlessly.
        if ((event.type as string) === "connected") {
          checkServerVersion((event as { version?: string }).version);
          return;
        }
        // Server heartbeat: liveness only, nothing to render. Swallow it
        // here so the reducer never sees a synthetic event. (Old clients
        // lacking this branch fall through to onEvent, whose switch has no
        // default case and ignores the unknown "ping" type harmlessly.)
        if ((event.type as string) === "ping") return;
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
        // Server heartbeats every ~20s (internal/server/agent_ws.go) and
        // the watchdog above catches silent zombies within ~45s — only
        // force-reconnect on wake after a hide long enough to have missed
        // a heartbeat, so a quick alt-tab doesn't churn a healthy socket.
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

    // Watchdog for the zombie socket the reported scenario produces: a
    // TLS-terminating proxy (tailscale serve) keeps the browser-side TCP
    // open across a daemon re-exec, so onclose never fires and the
    // OPEN-but-dead socket goes unnoticed by the visibility/online paths
    // (the tab stays foregrounded, the network never changes). The server
    // now emits an application heartbeat every ~20s, so >45s of total
    // silence (2 missed heartbeats + slack) on an OPEN socket means the
    // frames are being black-holed. Force-reconnect and let the existing
    // backoff + onConnected refetch path recover. 10s poll is cheap and
    // reads only a ref timestamp, so idle tabs cost nothing.
    const DEAD_AFTER_MS = 45000;
    const watchdog = setInterval(() => {
      if (!activeRef.current) return;
      const ws = wsRef.current;
      if (ws?.readyState !== WebSocket.OPEN) return; // only OPEN can be a zombie
      if (Date.now() - lastMsgRef.current > DEAD_AFTER_MS) {
        forceReconnect();
      }
    }, 10000);

    document.addEventListener("visibilitychange", onVisibilityChange);
    window.addEventListener("online", onOnline);

    return () => {
      activeRef.current = false;
      if (reconnectRef.current) clearTimeout(reconnectRef.current);
      clearInterval(watchdog);
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

  // Returns true when the frame was actually handed to an OPEN socket —
  // callers that MUST NOT silently drop text (steer fallbacks) check this.
  const sendMessage = useCallback((content: string, attachments?: AgentMessageAttachment[]): boolean => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      const msg: Record<string, unknown> = { type: "message", content };
      if (attachments && attachments.length > 0) {
        msg.attachments = attachments;
      }
      wsRef.current.send(JSON.stringify(msg));
      return true;
    }
    return false;
  }, []);

  const abort = useCallback(() => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify({ type: "abort" }));
    }
  }, []);

  // steer injects an additional user message into a running turn (claude
  // backend only server-side; unsupported backends surface an "error"
  // ChatEvent over the same socket, handled by the caller's onEvent).
  // Returns false if the socket isn't open so callers can fall back to a
  // plain POST.
  const steer = useCallback((content: string) => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify({ type: "steer", content }));
      return true;
    }
    return false;
  }, []);

  return { connected, sendMessage, abort, steer };
}

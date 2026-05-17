import { useCallback, useEffect, useRef, useState } from "react";
import { api, type SessionInfo } from "../lib/api";
import { toBase64, base64ToBytes, restoreScrollback, wsUrl } from "../lib/utils";
import { createOutputBuffer, type OutputBuffer } from "../lib/outputBuffer";
import { useTerminal } from "../hooks/useTerminal";
import { useSpecialKeys } from "../hooks/useSpecialKeys";
import { SpecialKeysBar } from "./SpecialKeysBar";

interface TerminalTabProps {
  parentSessionId: string;
  workDir: string;
  visible: boolean;
  // peerId, when set, forwards every REST + WS call to the peer
  // that hosts the parent session so the tmux terminal lands on
  // the same machine as the CLI session.
  peerId?: string;
}

const TMUX_SHORTCUTS = [
  { label: "+Win", action: "new-window" },
  { label: "\u2190Win", action: "prev-window" },
  { label: "Win\u2192", action: "next-window" },
  { label: "\u2500", action: "split-h" },
  { label: "\u2502", action: "split-v" },
  { label: "Pane", action: "select-pane" },
  { label: "Zoom", action: "resize-pane-z" },
  { label: "List", action: "choose-tree" },
  { label: "Copy", action: "copy-mode" },
  { label: "Kill", action: "kill-pane" },
];

export function TerminalTab({ parentSessionId, workDir, visible, peerId }: TerminalTabProps) {
  const termContainerRef = useRef<HTMLDivElement>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const outputBufRef = useRef<OutputBuffer | null>(null);
  const sessionIdRef = useRef<string | null>(null);
  const initRef = useRef(false);

  const [error, setError] = useState<string | null>(null);
  const [shellTool, setShellTool] = useState<string | null>(null);

  useEffect(() => {
    // shellTool reflects the host the session lives on, not the
    // Hub. Hub-only sessions get tmux/etc. from /info; peer-routed
    // sessions go through the peer proxy so the value comes from
    // the remote machine's PATH probe.
    api.info(peerId).then((info) => setShellTool(info.shellTool)).catch(() => setShellTool("tmux"));
  }, [peerId]);

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

  // wrapInput ref to avoid recreating terminal when ctrlMode changes
  const wrapInputRef = useRef<(data: string) => string>((d) => d);

  const { termRef: xtermRef, autoScrollRef, safeFit, immediateFit } = useTerminal({
    containerRef: termContainerRef,
    onInput: useCallback((data: string) => sendInput(wrapInputRef.current(data)), [sendInput]),
    onResize: sendResize,
    touchMode: "mouse",
    deps: [parentSessionId],
  });

  const { ctrlMode, shiftMode, handleKeyPress, wrapInput } = useSpecialKeys(sendInput, autoScrollRef);
  wrapInputRef.current = wrapInput;

  // Connect WebSocket to a tmux session
  const connectWs = useCallback((tmuxSessionId: string) => {
    const term = xtermRef.current;
    if (!term) return;

    // Close existing connection and clear pending output buffer
    if (wsRef.current) {
      wsRef.current.onclose = null;
      wsRef.current.close();
      wsRef.current = null;
    }
    outputBufRef.current?.clear();
    outputBufRef.current = createOutputBuffer((data) => term.write(data));

    const qs = peerId
      ? `session=${encodeURIComponent(tmuxSessionId)}&peer=${encodeURIComponent(peerId)}`
      : `session=${encodeURIComponent(tmuxSessionId)}`;
    const ws = new WebSocket(wsUrl(`/api/v1/ws?${qs}`));

    ws.onmessage = (evt) => {
      let msg: { type: string; data?: string; exitCode?: number; live?: boolean };
      try {
        msg = JSON.parse(evt.data);
      } catch {
        return;
      }
      switch (msg.type) {
        case "output":
          if (msg.data) {
            const bytes = base64ToBytes(msg.data);
            outputBufRef.current!.push(bytes);
          }
          break;
        case "scrollback":
          if (msg.data) {
            const bytes = base64ToBytes(msg.data);
            restoreScrollback(term, bytes, autoScrollRef);
          }
          break;
        case "exit":
          if (msg.live) {
            term.write(`\r\n\x1b[90m[terminal exited with code ${msg.exitCode ?? 0}]\x1b[0m\r\n`);
          }
          break;
      }
    };

    ws.onopen = () => {
      // Send resize immediately — before server sends scrollback
      immediateFit();
    };

    ws.onclose = () => {
      // No auto-reconnect — reconnection is managed by visibility toggling
    };

    ws.onerror = () => {
      ws.close();
    };

    wsRef.current = ws;
  }, [xtermRef, autoScrollRef, immediateFit, peerId]);

  // Clean up WebSocket and output buffer on unmount
  useEffect(() => {
    return () => {
      outputBufRef.current?.dispose();
      if (wsRef.current) {
        wsRef.current.onclose = null;
        wsRef.current.close();
      }
    };
  }, []);

  // Reset refs when parentSessionId or peerId changes (prevents
  // cross-session contamination AND cross-peer contamination: the
  // tmux child id minted on peer A is meaningless on peer B).
  useEffect(() => {
    sessionIdRef.current = null;
    initRef.current = false;
    setError(null);
    if (wsRef.current) {
      wsRef.current.onclose = null;
      wsRef.current.close();
      wsRef.current = null;
    }
  }, [parentSessionId, peerId]);

  // Manage session + WebSocket based on visibility
  useEffect(() => {
    if (!visible) {
      // Disconnect WebSocket when hidden
      if (wsRef.current) {
        wsRef.current.onclose = null;
        wsRef.current.close();
        wsRef.current = null;
      }
      return;
    }

    // Wait for workDir and shellTool to be resolved before initializing
    if (!workDir || !shellTool) return;

    // Refit when becoming visible
    requestAnimationFrame(() => safeFit());

    // If we already have a session, just reconnect WebSocket
    if (sessionIdRef.current) {
      connectWs(sessionIdRef.current);
      return;
    }

    // Prevent double-init from StrictMode
    if (initRef.current) return;
    initRef.current = true;

    let cancelled = false;

    // Find existing tmux session for this parent via server, or create a new one
    const initSession = async () => {
      // Check server for existing tmux child session
      try {
        const existing = await api.sessions.terminal(parentSessionId, peerId);
        if (cancelled) return;
        if (existing.status === "running") {
          sessionIdRef.current = existing.id;
          connectWs(existing.id);
          return;
        }
        // Exited — restart it (tmux -A will reattach)
        const restarted = await api.sessions.restart(existing.id, peerId);
        if (cancelled) return;
        sessionIdRef.current = restarted.id;
        connectWs(restarted.id);
        return;
      } catch (err) {
        if (cancelled) return;
        // Only create new session on 404; other errors (network, 500) should not trigger creation
        const is404 = err instanceof Error && err.message.startsWith("404");
        if (!is404) {
          setError(err instanceof Error ? err.message : String(err));
          return;
        }
      }

      // Create new terminal session linked to parent
      try {
        const s = await api.sessions.create({ tool: shellTool, workDir, parentId: parentSessionId, peerId });
        if (cancelled) return;
        sessionIdRef.current = s.id;
        connectWs(s.id);
      } catch (err) {
        if (cancelled) return;
        const msg = err instanceof Error ? err.message : String(err);
        if (msg.includes("tool not found")) {
          setError(
            shellTool === "tmux" || shellTool === null
              ? "tmux is not installed.\nInstall: brew install tmux"
              : `${shellTool} is not available.`,
          );
        } else {
          setError(msg);
        }
      }
    };

    initSession();

    return () => {
      cancelled = true;
    };
  }, [visible, parentSessionId, workDir, shellTool, peerId, connectWs, safeFit]);

  if (error) {
    return (
      <div className="flex flex-col items-center justify-center h-full text-neutral-400 gap-2">
        {error.split("\n").map((line, i) => (
          <p key={i} className={i === 0 ? "text-sm" : "text-xs font-mono text-neutral-500"}>
            {line}
          </p>
        ))}
      </div>
    );
  }

  return (
    <div className="flex flex-col h-full">
      {/* Terminal */}
      <div className="flex-1 min-h-0 relative">
        <div ref={termContainerRef} className="absolute inset-0" style={{ touchAction: "none" }} />
      </div>

      {/* tmux shortcuts (only shown for tmux sessions) */}
      {shellTool === "tmux" && (
        <div className="flex gap-1.5 px-2 py-1.5 border-t border-neutral-800 overflow-x-auto shrink-0">
          {TMUX_SHORTCUTS.map((s) => (
            <button
              key={s.action}
              onPointerDown={(e) => e.preventDefault()}
              onClick={() => {
                if (sessionIdRef.current) {
                  api.sessions.tmux(sessionIdRef.current, { action: s.action }, peerId).catch(() => {});
                }
              }}
              className="px-3 py-2.5 text-xs rounded font-mono bg-neutral-800 text-neutral-400 active:bg-neutral-600 whitespace-nowrap"
            >
              {s.label}
            </button>
          ))}
        </div>
      )}

      {/* Special keys */}
      <SpecialKeysBar ctrlMode={ctrlMode} shiftMode={shiftMode} onKeyPress={handleKeyPress} />
    </div>
  );
}

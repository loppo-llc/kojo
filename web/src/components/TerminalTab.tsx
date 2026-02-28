import { useCallback, useEffect, useRef, useState } from "react";
import { api, type SessionInfo } from "../lib/api";
import { toBase64, restoreScrollback } from "../lib/utils";
import { createOutputBuffer, type OutputBuffer } from "../lib/outputBuffer";
import { useTerminal } from "../hooks/useTerminal";
import { useSpecialKeys } from "../hooks/useSpecialKeys";
import { SpecialKeysBar } from "./SpecialKeysBar";

interface TerminalTabProps {
  parentSessionId: string;
  workDir: string;
  visible: boolean;
}

const TMUX_PREFIX = "\x02"; // Ctrl+b

const TMUX_SHORTCUTS = [
  { label: "+Win", seq: TMUX_PREFIX + "c" },
  { label: "\u2190Win", seq: TMUX_PREFIX + "p" },
  { label: "Win\u2192", seq: TMUX_PREFIX + "n" },
  { label: "\u2500", seq: TMUX_PREFIX + '"' },
  { label: "\u2502", seq: TMUX_PREFIX + "%" },
  { label: "Pane", seq: TMUX_PREFIX + "o" },
  { label: "List", seq: TMUX_PREFIX + "w" },
  { label: "Kill", seq: TMUX_PREFIX + ":kill-pane\r" },
];

export function TerminalTab({ parentSessionId, workDir, visible }: TerminalTabProps) {
  const termContainerRef = useRef<HTMLDivElement>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const outputBufRef = useRef<OutputBuffer | null>(null);
  const sessionIdRef = useRef<string | null>(null);
  const initRef = useRef(false);

  const [error, setError] = useState<string | null>(null);

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

    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const url = `${proto}//${location.host}/api/v1/ws?session=${tmuxSessionId}`;
    const ws = new WebSocket(url);

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
            const bytes = Uint8Array.from(atob(msg.data), (c) => c.charCodeAt(0));
            outputBufRef.current!.push(bytes);
          }
          break;
        case "scrollback":
          if (msg.data) {
            const bytes = Uint8Array.from(atob(msg.data), (c) => c.charCodeAt(0));
            restoreScrollback(term, bytes, autoScrollRef);
          }
          break;
        case "exit":
          if (msg.live) {
            term.write(`\r\n\x1b[90m[tmux exited with code ${msg.exitCode ?? 0}]\x1b[0m\r\n`);
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
  }, [xtermRef, autoScrollRef, immediateFit]);

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

  // Reset refs when parentSessionId changes (prevents cross-session contamination)
  useEffect(() => {
    sessionIdRef.current = null;
    initRef.current = false;
    setError(null);
    if (wsRef.current) {
      wsRef.current.onclose = null;
      wsRef.current.close();
      wsRef.current = null;
    }
  }, [parentSessionId]);

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

    // Wait for workDir to be resolved before initializing
    if (!workDir) return;

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
        const existing = await api.sessions.terminal(parentSessionId);
        if (cancelled) return;
        if (existing.status === "running") {
          sessionIdRef.current = existing.id;
          connectWs(existing.id);
          return;
        }
        // Exited — restart it (tmux -A will reattach)
        const restarted = await api.sessions.restart(existing.id);
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

      // Create new tmux session linked to parent
      try {
        const s = await api.sessions.create({ tool: "tmux", workDir, parentId: parentSessionId });
        if (cancelled) return;
        sessionIdRef.current = s.id;
        connectWs(s.id);
      } catch (err) {
        if (cancelled) return;
        const msg = err instanceof Error ? err.message : String(err);
        if (msg.includes("tool not found")) {
          setError("tmux is not installed.\nInstall: brew install tmux");
        } else {
          setError(msg);
        }
      }
    };

    initSession();

    return () => {
      cancelled = true;
    };
  }, [visible, parentSessionId, workDir, connectWs, safeFit]);

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

      {/* tmux shortcuts */}
      <div className="flex gap-1.5 px-2 py-1.5 border-t border-neutral-800 overflow-x-auto shrink-0">
        {TMUX_SHORTCUTS.map((s) => (
          <button
            key={s.label}
            onPointerDown={(e) => e.preventDefault()}
            onClick={() => sendInput(s.seq)}
            className="px-3 py-2.5 text-xs rounded font-mono bg-neutral-800 text-neutral-400 active:bg-neutral-600 whitespace-nowrap"
          >
            {s.label}
          </button>
        ))}
      </div>

      {/* Special keys */}
      <SpecialKeysBar ctrlMode={ctrlMode} shiftMode={shiftMode} onKeyPress={handleKeyPress} />
    </div>
  );
}

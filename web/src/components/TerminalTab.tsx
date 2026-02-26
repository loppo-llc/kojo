import { useCallback, useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { WebLinksAddon } from "@xterm/addon-web-links";
import { api, type SessionInfo } from "../lib/api";

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

const SPECIAL_KEYS = [
  { label: "Esc", code: "\x1b" },
  { label: "Tab", code: "\t" },
  { label: "Ctrl", code: "ctrl" },
  { label: "Opt", code: "opt" },
  { label: "Cmd", code: "cmd" },
  { label: "Shift", code: "shift" },
  { label: "/", code: "/" },
  { label: "\u2191", code: "\x1b[A" },
  { label: "\u2193", code: "\x1b[B" },
  { label: "\u2192", code: "\x1b[C" },
  { label: "\u2190", code: "\x1b[D" },
];

// Filter terminal query responses (DA1/DA2/DA3) that xterm.js auto-generates.
// Without filtering, these get sent to the PTY as input and appear as garbage text.
const DA_RESPONSE_RE = /\x1b\[[\?>=]?[\d;]*c/g;

function toBase64(str: string): string {
  return btoa(
    Array.from(new TextEncoder().encode(str), (b) => String.fromCharCode(b)).join(""),
  );
}

export function TerminalTab({ parentSessionId, workDir, visible }: TerminalTabProps) {
  const termRef = useRef<HTMLDivElement>(null);
  const xtermRef = useRef<Terminal>(null);
  const fitRef = useRef<FitAddon>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const autoScrollRef = useRef(true);
  const fitRafRef = useRef(0);
  const outputBufRef = useRef<Uint8Array[]>([]);
  const outputBufSizeRef = useRef(0);
  const outputRafRef = useRef(0);
  const maxBufBytes = 256 * 1024;
  const sessionIdRef = useRef<string | null>(null);
  const initRef = useRef(false);

  const [error, setError] = useState<string | null>(null);
  const [ctrlMode, setCtrlMode] = useState(false);
  const [optMode, setOptMode] = useState(false);
  const [cmdMode, setCmdMode] = useState(false);
  const [shiftMode, setShiftMode] = useState(false);

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

  const safeFit = useCallback(() => {
    const term = xtermRef.current;
    const fit = fitRef.current;
    const el = termRef.current;
    if (!term || !fit || !el) return;
    if (!el.offsetParent) return;

    cancelAnimationFrame(fitRafRef.current);
    fitRafRef.current = requestAnimationFrame(() => {
      if (!xtermRef.current || !fitRef.current) return;
      const el2 = termRef.current;
      if (!el2 || !el2.offsetParent) return;
      fitRef.current.fit();
      sendResize(xtermRef.current.cols, xtermRef.current.rows);
      if (autoScrollRef.current) {
        xtermRef.current.scrollToBottom();
      }
    });
  }, [sendResize]);

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
    if (outputRafRef.current) {
      cancelAnimationFrame(outputRafRef.current);
      outputRafRef.current = 0;
    }
    outputBufRef.current = [];
    outputBufSizeRef.current = 0;

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
            outputBufRef.current.push(bytes);
            outputBufSizeRef.current += bytes.length;
            const flushNow = () => {
              outputRafRef.current = 0;
              const chunks = outputBufRef.current;
              if (chunks.length === 0) return;
              outputBufRef.current = [];
              outputBufSizeRef.current = 0;
              if (chunks.length === 1) {
                term.write(chunks[0]);
              } else {
                let total = 0;
                for (const c of chunks) total += c.length;
                const merged = new Uint8Array(total);
                let off = 0;
                for (const c of chunks) { merged.set(c, off); off += c.length; }
                term.write(merged);
              }
            };
            if (outputBufSizeRef.current >= maxBufBytes) {
              if (outputRafRef.current) cancelAnimationFrame(outputRafRef.current);
              flushNow();
            } else if (!outputRafRef.current) {
              outputRafRef.current = requestAnimationFrame(flushNow);
            }
          }
          break;
        case "scrollback":
          if (msg.data) {
            const bytes = Uint8Array.from(atob(msg.data), (c) => c.charCodeAt(0));
            const el = term.element;
            if (el) el.style.visibility = "hidden";
            term.reset();
            let restored = false;
            const safetyTimer = setTimeout(() => restoreVis(), 500);
            const restoreVis = () => {
              if (restored) return;
              restored = true;
              clearTimeout(safetyTimer);
              autoScrollRef.current = true;
              term.scrollToBottom();
              if (el) el.style.visibility = "";
            };
            term.write(bytes, restoreVis);
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
      // Fit after connection to send initial size
      requestAnimationFrame(() => safeFit());
    };

    ws.onclose = () => {
      // No auto-reconnect — reconnection is managed by visibility toggling
    };

    ws.onerror = () => {
      ws.close();
    };

    wsRef.current = ws;
  }, [safeFit]);

  // Initialize xterm (once, persistent across visibility changes)
  useEffect(() => {
    if (!termRef.current) return;

    const term = new Terminal({
      cursorBlink: true,
      fontSize: 14,
      fontFamily: "Menlo, Monaco, 'Courier New', monospace",
      theme: {
        background: "#0a0a0a",
        foreground: "#e5e5e5",
        cursor: "#e5e5e5",
      },
    });

    const fit = new FitAddon();
    term.loadAddon(fit);
    term.loadAddon(new WebLinksAddon());

    term.open(termRef.current);
    fit.fit();

    xtermRef.current = term;
    fitRef.current = fit;
    autoScrollRef.current = true;

    const onDataDisposable = term.onData((data) => {
      const filtered = data.replace(DA_RESPONSE_RE, "");
      if (!filtered) return;
      autoScrollRef.current = true;
      sendInput(filtered);
    });

    const onWriteParsedDisposable = term.onWriteParsed(() => {
      if (autoScrollRef.current) term.scrollToBottom();
    });

    const onRenderDisposable = term.onRender(() => {
      if (autoScrollRef.current) term.scrollToBottom();
    });

    const ro = new ResizeObserver(() => safeFit());
    ro.observe(termRef.current);

    // Scroll event listeners
    const el = termRef.current;

    const onWheel = (e: WheelEvent) => {
      if (e.deltaY < 0) {
        autoScrollRef.current = false;
      } else if (e.deltaY > 0) {
        requestAnimationFrame(() => {
          const buf = term.buffer.active;
          if (buf.baseY - buf.viewportY <= 3) {
            autoScrollRef.current = true;
          }
        });
      }
    };
    el.addEventListener("wheel", onWheel, { capture: true, passive: true });

    let touchStartY = 0;
    let accumDelta = 0;
    const lineHeight = 20;
    const onTouchStart = (e: TouchEvent) => {
      touchStartY = e.touches[0].clientY;
      accumDelta = 0;
    };
    const onTouchMove = (e: TouchEvent) => {
      const dy = touchStartY - e.touches[0].clientY;
      touchStartY = e.touches[0].clientY;
      accumDelta += dy;
      const lines = Math.trunc(accumDelta / lineHeight);
      if (lines !== 0) {
        if (lines > 0) {
          term.scrollLines(lines);
          const buf = term.buffer.active;
          if (buf.viewportY >= buf.baseY) autoScrollRef.current = true;
        } else {
          autoScrollRef.current = false;
          term.scrollLines(lines);
        }
        accumDelta -= lines * lineHeight;
      }
      e.preventDefault();
    };
    el.addEventListener("touchstart", onTouchStart, { passive: true, capture: true });
    el.addEventListener("touchmove", onTouchMove, { passive: false, capture: true });

    return () => {
      cancelAnimationFrame(fitRafRef.current);
      if (outputRafRef.current) {
        cancelAnimationFrame(outputRafRef.current);
        outputRafRef.current = 0;
      }
      outputBufRef.current = [];
      outputBufSizeRef.current = 0;
      onDataDisposable.dispose();
      onWriteParsedDisposable.dispose();
      onRenderDisposable.dispose();
      ro.disconnect();
      el.removeEventListener("wheel", onWheel, { capture: true } as EventListenerOptions);
      el.removeEventListener("touchstart", onTouchStart, { capture: true } as EventListenerOptions);
      el.removeEventListener("touchmove", onTouchMove, { capture: true } as EventListenerOptions);
      if (wsRef.current) {
        wsRef.current.onclose = null;
        wsRef.current.close();
      }
      term.dispose();
      xtermRef.current = null;
      fitRef.current = null;
    };
  }, [parentSessionId, sendInput, safeFit]);

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

  const clearModifiers = () => {
    setCtrlMode(false);
    setOptMode(false);
    setCmdMode(false);
    setShiftMode(false);
  };

  const handleKeyPress = (code: string) => {
    autoScrollRef.current = true;
    if (code === "ctrl") {
      clearModifiers();
      setCtrlMode(!ctrlMode);
      return;
    }
    if (code === "opt") {
      clearModifiers();
      setOptMode(!optMode);
      return;
    }
    if (code === "cmd") {
      clearModifiers();
      setCmdMode(!cmdMode);
      return;
    }
    if (code === "shift") {
      clearModifiers();
      setShiftMode(!shiftMode);
      return;
    }
    if (ctrlMode) {
      const char = code.charCodeAt(0);
      if (char >= 97 && char <= 122) {
        sendInput(String.fromCharCode(char - 96));
      }
      clearModifiers();
    } else if (optMode) {
      sendInput("\x1b" + code);
      clearModifiers();
    } else if (cmdMode) {
      sendInput("\x1b" + code);
      clearModifiers();
    } else if (shiftMode) {
      const shiftMap: Record<string, string> = {
        "\x1b[A": "\x1b[1;2A",
        "\x1b[B": "\x1b[1;2B",
        "\x1b[C": "\x1b[1;2C",
        "\x1b[D": "\x1b[1;2D",
        "/": "?",
      };
      sendInput(shiftMap[code] ?? code.toUpperCase());
      clearModifiers();
    } else {
      sendInput(code);
    }
  };

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
        <div ref={termRef} className="absolute inset-0" style={{ touchAction: "none" }} />
      </div>

      {/* tmux shortcuts */}
      <div className="flex gap-1.5 px-2 py-1.5 border-t border-neutral-800 overflow-x-auto shrink-0">
        {TMUX_SHORTCUTS.map((s) => (
          <button
            key={s.label}
            onClick={() => sendInput(s.seq)}
            className="px-3 py-2.5 text-xs rounded font-mono bg-neutral-800 text-neutral-400 active:bg-neutral-600 whitespace-nowrap"
          >
            {s.label}
          </button>
        ))}
      </div>

      {/* Special keys */}
      <div className="flex gap-1.5 px-2 py-1.5 border-t border-neutral-800 overflow-x-auto shrink-0">
        {SPECIAL_KEYS.map((key) => (
          <button
            key={key.label}
            onClick={() => handleKeyPress(key.code)}
            className={`px-4 py-2.5 text-sm rounded font-mono ${
              (key.code === "ctrl" && ctrlMode) || (key.code === "opt" && optMode) || (key.code === "cmd" && cmdMode) || (key.code === "shift" && shiftMode)
                ? "bg-blue-900 text-blue-300"
                : "bg-neutral-800 text-neutral-400 active:bg-neutral-600"
            }`}
          >
            {key.label}
          </button>
        ))}
      </div>
    </div>
  );
}

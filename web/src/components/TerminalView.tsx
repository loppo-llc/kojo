import { useCallback, useEffect, useRef, useState } from "react";
import { useParams, useNavigate } from "react-router";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { WebLinksAddon } from "@xterm/addon-web-links";
import { useWebSocket } from "../hooks/useWebSocket";
import { api, type SessionInfo } from "../lib/api";

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

export function TerminalView() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const termRef = useRef<HTMLDivElement>(null);
  const xtermRef = useRef<Terminal>(null);
  const fitRef = useRef<FitAddon>(null);
  const [session, setSession] = useState<SessionInfo>();
  const [input, setInput] = useState("");
  const [ctrlMode, setCtrlMode] = useState(false);
  const [optMode, setOptMode] = useState(false);
  const [cmdMode, setCmdMode] = useState(false);
  const [shiftMode, setShiftMode] = useState(false);
  const [exited, setExited] = useState(false);
  const yoloTailRef = useRef<string | null>(null);
  const yoloOverlayRef = useRef<HTMLButtonElement>(null);
  const yoloTimerRef = useRef<ReturnType<typeof setTimeout>>(null);

  // Auto-scroll: true = follow output to bottom.
  // Only disabled by explicit user scroll-up (wheel/touch).
  const autoScrollRef = useRef(true);

  const onOutput = useCallback((data: Uint8Array) => {
    xtermRef.current?.write(data);
  }, []);

  const onScrollback = useCallback((data: Uint8Array) => {
    const term = xtermRef.current;
    if (!term) return;
    const el = term.element;
    if (el) el.style.visibility = "hidden";
    term.reset();
    term.write(data, () => {
      autoScrollRef.current = true;
      term.scrollToBottom();
      if (el) el.style.visibility = "";
    });
  }, []);

  const onExit = useCallback((exitCode: number, live: boolean) => {
    setExited(true);
    if (live) {
      xtermRef.current?.write(`\r\n\x1b[90m[Process exited with code ${exitCode}]\x1b[0m\r\n`);
    }
  }, []);

  // fit() debounced into a single rAF to avoid repeated reflows
  const fitRafRef = useRef(0);
  const sendResizeRef = useRef<(cols: number, rows: number) => void>(() => {});
  const safeFit = useCallback(() => {
    const term = xtermRef.current;
    const fit = fitRef.current;
    if (!term || !fit) return;

    cancelAnimationFrame(fitRafRef.current);
    fitRafRef.current = requestAnimationFrame(() => {
      if (!xtermRef.current || !fitRef.current) return;
      fitRef.current.fit();
      sendResizeRef.current(xtermRef.current.cols, xtermRef.current.rows);
      // Let autoScrollRef drive scroll position — no stale delta math
      if (autoScrollRef.current) {
        xtermRef.current.scrollToBottom();
      }
    });
  }, []);

  const onYoloDebug = useCallback((tail: string) => {
    yoloTailRef.current = tail;
    const el = yoloOverlayRef.current;
    if (el) {
      el.style.display = "";
      const textEl = el.querySelector<HTMLSpanElement>("[data-yolo-text]");
      if (textEl) textEl.textContent = tail.slice(-80);
    }
    if (yoloTimerRef.current) clearTimeout(yoloTimerRef.current);
    yoloTimerRef.current = setTimeout(() => {
      yoloTailRef.current = null;
      if (yoloOverlayRef.current) yoloOverlayRef.current.style.display = "none";
    }, 5000);
  }, []);

  const { connected, sendInput, sendResize, reconnect } = useWebSocket({
    sessionId: id!,
    onOutput,
    onScrollback,
    onExit,
    onYoloDebug,
  });
  sendResizeRef.current = sendResize;

  useEffect(() => {
    setExited(false);
    api.sessions.get(id!).then((s) => {
      setSession(s);
      if (s.status === "exited") setExited(true);
    }).catch(() => navigate("/"));
  }, [id, navigate]);

  // track visual viewport for mobile keyboard
  const containerRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const vv = window.visualViewport;
    if (!vv || !containerRef.current) return;
    const updateHeight = () => {
      if (containerRef.current) {
        containerRef.current.style.height = `${vv.height}px`;
      }
    };
    const onResize = () => {
      updateHeight();
      safeFit();
    };
    onResize();
    vv.addEventListener("resize", onResize);
    vv.addEventListener("scroll", updateHeight);
    return () => {
      vv.removeEventListener("resize", onResize);
      vv.removeEventListener("scroll", updateHeight);
    };
  }, []);

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

    // forward terminal keystrokes to PTY
    const onDataDisposable = term.onData((data) => {
      autoScrollRef.current = true;
      sendInput(data);
    });

    // Auto-scroll: after each batch of writes, scroll to bottom if user hasn't scrolled up.
    // onWriteParsed fires immediately after data is parsed (before render) — fast path.
    const onWriteParsedDisposable = term.onWriteParsed(() => {
      if (autoScrollRef.current) {
        term.scrollToBottom();
      }
    });

    // Safety net: onRender fires after ANY rendering (including fit() resize reflow).
    // Catches viewport jumps that onWriteParsed misses (e.g. fit() without new data).
    const onRenderDisposable = term.onRender(() => {
      if (autoScrollRef.current) {
        term.scrollToBottom();
      }
    });

    // Detect user wheel scroll to toggle auto-scroll
    term.attachCustomWheelEventHandler((e: WheelEvent) => {
      if (e.deltaY < 0) {
        // Scrolling up — user wants to read history
        autoScrollRef.current = false;
      } else if (e.deltaY > 0) {
        // Scrolling down — check if close to bottom after xterm processes
        requestAnimationFrame(() => {
          const buf = term.buffer.active;
          if (buf.baseY - buf.viewportY <= 3) {
            autoScrollRef.current = true;
          }
        });
      }
      return true; // let xterm handle the actual scroll
    });

    const ro = new ResizeObserver(() => {
      safeFit();
    });
    ro.observe(termRef.current);

    // touch scroll for mobile
    const el = termRef.current;
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
        // dy>0 = finger up = content scrolls down (toward bottom) = lines>0
        // dy<0 = finger down = content scrolls up (toward history) = lines<0
        if (lines > 0) {
          // Scrolling down toward bottom — check if at bottom after scroll
          term.scrollLines(lines);
          const buf = term.buffer.active;
          if (buf.viewportY >= buf.baseY) {
            autoScrollRef.current = true;
          }
        } else {
          // Scrolling up toward history — disable auto-scroll
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
      onDataDisposable.dispose();
      onWriteParsedDisposable.dispose();
      onRenderDisposable.dispose();
      ro.disconnect();
      el.removeEventListener("touchstart", onTouchStart, { capture: true } as EventListenerOptions);
      el.removeEventListener("touchmove", onTouchMove, { capture: true } as EventListenerOptions);
      term.dispose();
      xtermRef.current = null;
      fitRef.current = null;
    };
  }, [id, sendInput, sendResize]);

  const handleSend = () => {
    autoScrollRef.current = true;
    if (!input || !input.trim()) {
      sendInput("\r");
      return;
    }
    sendInput(input);
    setTimeout(() => sendInput("\r"), 30);
    setInput("");
  };

  const handleResume = async () => {
    if (!id) return;
    try {
      const updated = await api.sessions.restart(id);
      setSession(updated);
      setExited(false);
      reconnect();
    } catch (err) {
      console.error(err);
    }
  };

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
      // Option/Alt: send ESC prefix
      sendInput("\x1b" + code);
      clearModifiers();
    } else if (cmdMode) {
      // Cmd: send ESC prefix (meta)
      sendInput("\x1b" + code);
      clearModifiers();
    } else if (shiftMode) {
      const shiftMap: Record<string, string> = {
        "\x1b[A": "\x1b[1;2A", // Shift+Up
        "\x1b[B": "\x1b[1;2B", // Shift+Down
        "\x1b[C": "\x1b[1;2C", // Shift+Right
        "\x1b[D": "\x1b[1;2D", // Shift+Left
        "/": "?",
      };
      sendInput(shiftMap[code] ?? code.toUpperCase());
      clearModifiers();
    } else {
      sendInput(code);
    }
  };

  const handleStop = async () => {
    if (id) {
      await api.sessions.delete(id);
      setExited(true);
    }
  };

  const handleYoloToggle = async () => {
    if (!id || !session) return;
    const updated = await api.sessions.patch(id, { yoloMode: !session.yoloMode });
    setSession(updated);
  };

  const handleFileAttach = async () => {
    const fileInput = document.createElement("input");
    fileInput.type = "file";
    fileInput.accept = "*/*";
    fileInput.onchange = async () => {
      const file = fileInput.files?.[0];
      if (!file) return;
      const result = await api.upload(file);
      setInput((prev) => (prev ? prev + "\n" : "") + result.path);
    };
    fileInput.click();
  };

  return (
    <div ref={containerRef} className="h-full flex flex-col bg-neutral-950">
      {/* Header */}
      <header className="flex items-center gap-2 px-3 py-2 border-b border-neutral-800 shrink-0">
        <button onClick={() => navigate("/")} className="text-neutral-400 hover:text-neutral-200">
          &larr;
        </button>
        <span className="font-mono font-bold">{session?.tool}</span>
        <span className="text-xs text-neutral-500 truncate flex-1">{session?.workDir}</span>
        <button
          onClick={() => navigate(`/files?path=${encodeURIComponent(session?.workDir ?? '')}`)}
          className="px-2.5 py-1.5 text-xs bg-neutral-800 hover:bg-neutral-700 text-neutral-400 rounded min-h-[44px] min-w-[44px] flex items-center justify-center"
          title="Files"
        >
          &#x1F4C1;
        </button>
        <button
          onClick={() => navigate(`/session/${id}/git`)}
          className="px-2.5 py-1.5 text-xs bg-neutral-800 hover:bg-neutral-700 text-neutral-400 rounded min-h-[44px] min-w-[44px] flex items-center justify-center font-mono"
          title="Git"
        >
          git
        </button>
        <button
          onClick={handleYoloToggle}
          className={`px-2.5 py-1.5 text-xs rounded min-h-[44px] min-w-[44px] flex items-center justify-center ${
            session?.yoloMode
              ? "bg-yellow-900 text-yellow-300"
              : "bg-neutral-800 text-neutral-500"
          }`}
          title="Yolo Mode"
        >
          &#x26A1;
        </button>
        <button
          onClick={handleStop}
          disabled={exited}
          className="px-2.5 py-1.5 text-xs bg-neutral-800 hover:bg-red-900 text-neutral-400 hover:text-red-300 rounded min-h-[44px] min-w-[44px] flex items-center justify-center disabled:opacity-30"
        >
          &#9632;
        </button>
      </header>

      {/* Connection indicator */}
      {!connected && !exited && (
        <div className="px-3 py-1 bg-yellow-950 text-yellow-400 text-xs text-center">
          Reconnecting...
        </div>
      )}

      {/* Terminal (with yolo overlay) */}
      <div className="relative flex-1 min-h-0">
        <div ref={termRef} className="absolute inset-0" style={{ touchAction: "none" }} />
        <button
          ref={yoloOverlayRef}
          style={{ display: "none" }}
          onClick={() => {
            if (yoloTailRef.current) navigator.clipboard.writeText(yoloTailRef.current);
            yoloTailRef.current = null;
            if (yoloOverlayRef.current) yoloOverlayRef.current.style.display = "none";
          }}
          className="absolute top-0 left-0 right-0 z-10 px-3 py-2 bg-purple-950/90 text-purple-300 text-xs text-left font-mono active:bg-purple-900"
        >
          <span className="text-purple-500">yolo tail</span>{" "}
          <span data-yolo-text className="truncate block" />
          <span className="text-purple-600 text-[10px]">tap to copy</span>
        </button>
      </div>

      {/* Auxiliary key bar */}
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

      {/* Input bar */}
      {!exited ? (
        <div className="flex items-end gap-2 px-2 py-2 border-t border-neutral-800 shrink-0">
          <button
            onClick={handleFileAttach}
            className="px-2 py-1.5 text-sm bg-neutral-800 hover:bg-neutral-700 rounded"
            title="Attach file"
          >
            &#x1F4CE;
          </button>
          <textarea
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && e.shiftKey) {
                e.preventDefault();
                handleSend();
              }
            }}
            placeholder="Type here... (Shift+Enter to send)"
            rows={Math.min(input.split("\n").length, 5)}
            className="flex-1 px-3 py-1.5 bg-neutral-900 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500 resize-none"
          />
          <button
            onClick={handleSend}
            className="px-3 py-1.5 bg-neutral-700 hover:bg-neutral-600 rounded text-sm"
          >
            Enter
          </button>
        </div>
      ) : (
        <div className="flex items-center justify-center gap-3 px-2 py-3 border-t border-neutral-800 shrink-0">
          <button
            onClick={handleResume}
            className="px-4 py-2 bg-neutral-800 hover:bg-neutral-700 rounded-lg text-sm font-medium"
          >
            Resume
          </button>
          <button
            onClick={async () => {
              if (!session) return;
              const s = await api.sessions.create({ tool: session.tool, workDir: session.workDir });
              navigate(`/session/${s.id}`);
            }}
            className="px-4 py-2 bg-neutral-800 hover:bg-neutral-700 rounded-lg text-sm font-medium"
          >
            New Session
          </button>
          <button
            onClick={() => navigate("/")}
            className="px-4 py-2 bg-neutral-900 hover:bg-neutral-800 rounded-lg text-sm text-neutral-400"
          >
            Back
          </button>
        </div>
      )}
    </div>
  );
}

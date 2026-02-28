import { useCallback, useEffect, useRef } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { WebLinksAddon } from "@xterm/addon-web-links";

// Filter terminal query responses (DA1/DA2/DA3) that xterm.js auto-generates.
const DA_RESPONSE_RE = /\x1b\[[\?>=]?[\d;]*c/g;

interface UseTerminalOptions {
  /** Container ref for the terminal element */
  containerRef: React.RefObject<HTMLDivElement | null>;
  /** Called when user types into the terminal (after DA response filtering) */
  onInput: (data: string) => void;
  /** Called when terminal is resized */
  onResize: (cols: number, rows: number) => void;
  /**
   * Touch scroll behaviour.
   * - "scroll" (default): scroll the xterm.js scrollback buffer (for CLI tab)
   * - "mouse": convert touch scroll to SGR mouse-wheel escape sequences
   *   sent via onInput so tmux can handle per-pane scrolling (for Terminal tab)
   */
  touchMode?: "scroll" | "mouse";
  /** Dependency array for recreating the terminal (e.g. [sessionId]) */
  deps?: React.DependencyList;
}

interface UseTerminalReturn {
  termRef: React.RefObject<Terminal | null>;
  fitRef: React.RefObject<FitAddon | null>;
  autoScrollRef: React.RefObject<boolean>;
  /** Debounced fit that also sends resize and scrolls to bottom */
  safeFit: () => void;
  /** Immediate (synchronous) fit + resize — no rAF delay */
  immediateFit: () => void;
}

export function useTerminal({
  containerRef,
  onInput,
  onResize,
  touchMode = "scroll",
  deps = [],
}: UseTerminalOptions): UseTerminalReturn {
  const termRef = useRef<Terminal>(null);
  const fitRef = useRef<FitAddon>(null);
  const autoScrollRef = useRef(true);
  const fitRafRef = useRef(0);
  const onResizeRef = useRef(onResize);
  onResizeRef.current = onResize;

  const immediateFit = useCallback(() => {
    const term = termRef.current;
    const fit = fitRef.current;
    const el = containerRef.current;
    if (!term || !fit || !el) return;
    // fit() needs the element visible to measure; skip if hidden
    if (el.offsetParent) {
      fit.fit();
    }
    // Always send current dimensions so the server gets a resize on connect,
    // even if the terminal tab is hidden (uses last known cols/rows).
    onResizeRef.current(term.cols, term.rows);
    if (autoScrollRef.current) {
      term.scrollToBottom();
    }
  }, [containerRef]);

  const safeFit = useCallback(() => {
    const term = termRef.current;
    const fit = fitRef.current;
    const el = containerRef.current;
    if (!term || !fit || !el) return;
    if (!el.offsetParent) return;

    cancelAnimationFrame(fitRafRef.current);
    fitRafRef.current = requestAnimationFrame(() => {
      if (!termRef.current || !fitRef.current) return;
      const el2 = containerRef.current;
      if (!el2 || !el2.offsetParent) return;
      fitRef.current.fit();
      onResizeRef.current(termRef.current.cols, termRef.current.rows);
      if (autoScrollRef.current) {
        termRef.current.scrollToBottom();
      }
    });
  }, [containerRef]);

  useEffect(() => {
    if (!containerRef.current) return;

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

    term.open(containerRef.current);
    fit.fit();

    termRef.current = term;
    fitRef.current = fit;
    autoScrollRef.current = true;

    // Forward terminal keystrokes to PTY
    const onDataDisposable = term.onData((data) => {
      const filtered = data.replace(DA_RESPONSE_RE, "");
      if (!filtered) return;
      autoScrollRef.current = true;
      onInput(filtered);
    });

    // Copy selected text to clipboard when selection stabilises (debounced)
    let selectionTimer = 0;
    const onSelectionDisposable = term.onSelectionChange(() => {
      clearTimeout(selectionTimer);
      selectionTimer = window.setTimeout(() => {
        const sel = term.getSelection();
        if (sel && navigator.clipboard?.writeText) {
          navigator.clipboard.writeText(sel).catch(() => {});
        }
      }, 150);
    });

    const onWriteParsedDisposable = term.onWriteParsed(() => {
      if (autoScrollRef.current) term.scrollToBottom();
    });

    const onRenderDisposable = term.onRender(() => {
      if (autoScrollRef.current) term.scrollToBottom();
    });

    const ro = new ResizeObserver(() => safeFit());
    ro.observe(containerRef.current);

    // Scroll event listeners
    const el = containerRef.current;

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
    let touchDirty = false; // set after multi-touch; resets baseline on next single-touch
    const lineHeight = 20;
    const onTouchStart = (e: TouchEvent) => {
      if (e.touches.length !== 1) { touchDirty = true; return; }
      touchStartY = e.touches[0].clientY;
      accumDelta = 0;
      touchDirty = false;
    };
    const onTouchMove = (e: TouchEvent) => {
      if (e.touches.length !== 1) { touchDirty = true; accumDelta = 0; return; }
      if (touchDirty) {
        // Resuming from multi-touch: reset baseline to avoid jump
        touchStartY = e.touches[0].clientY;
        accumDelta = 0;
        touchDirty = false;
        e.preventDefault();
        return;
      }
      const dy = touchStartY - e.touches[0].clientY;
      touchStartY = e.touches[0].clientY;
      accumDelta += dy;
      const lines = Math.trunc(accumDelta / lineHeight);
      if (lines !== 0) {
        if (touchMode === "mouse") {
          // Convert touch scroll to SGR mouse-wheel escape sequences.
          // tmux intercepts these and scrolls the pane under the cursor.
          const rect = el.getBoundingClientRect();
          if (term.cols > 0 && term.rows > 0 && rect.width > 0 && rect.height > 0) {
            const touch = e.touches[0];
            const cellW = rect.width / term.cols;
            const cellH = rect.height / term.rows;
            const col = Math.min(term.cols, Math.max(1, Math.floor((touch.clientX - rect.left) / cellW) + 1));
            const row = Math.min(term.rows, Math.max(1, Math.floor((touch.clientY - rect.top) / cellH) + 1));
            // SGR encoding: button 64 = scroll up, 65 = scroll down
            const btn = lines > 0 ? 65 : 64;
            const count = Math.abs(lines);
            const seq = `\x1b[<${btn};${col};${row}M`;
            for (let i = 0; i < count; i++) {
              onInput(seq);
            }
          }
        } else {
          if (lines > 0) {
            term.scrollLines(lines);
            const buf = term.buffer.active;
            if (buf.viewportY >= buf.baseY) autoScrollRef.current = true;
          } else {
            autoScrollRef.current = false;
            term.scrollLines(lines);
          }
        }
        accumDelta -= lines * lineHeight;
      }
      e.preventDefault();
    };
    el.addEventListener("touchstart", onTouchStart, { passive: true, capture: true });
    el.addEventListener("touchmove", onTouchMove, { passive: false, capture: true });

    return () => {
      cancelAnimationFrame(fitRafRef.current);
      clearTimeout(selectionTimer);
      onDataDisposable.dispose();
      onSelectionDisposable.dispose();
      onWriteParsedDisposable.dispose();
      onRenderDisposable.dispose();
      ro.disconnect();
      el.removeEventListener("wheel", onWheel, { capture: true } as EventListenerOptions);
      el.removeEventListener("touchstart", onTouchStart, { capture: true } as EventListenerOptions);
      el.removeEventListener("touchmove", onTouchMove, { capture: true } as EventListenerOptions);
      term.dispose();
      termRef.current = null;
      fitRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [containerRef, onInput, safeFit, touchMode, ...deps]);

  return { termRef, fitRef, autoScrollRef, safeFit, immediateFit };
}

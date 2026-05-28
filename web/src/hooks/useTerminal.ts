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
  /**
   * xterm.js scrollback line count. Set to 0 to disable scrollback entirely
   * (fixed-height terminal, no internal scrollbar). Defaults to xterm's 1000.
   */
  scrollback?: number;
  /**
   * When set, mouse-wheel and single-finger touch scrolls are converted to
   * the given key sequences and pushed via `send` instead of moving xterm's
   * scrollback viewport. Used for TUI apps (e.g. grok) that bind their own
   * scroll shortcuts and ignore xterm's internal scrollback. `send` bypasses
   * the modifier-wrapping path used for typed input.
   */
  scrollAsKeys?: { up: string; down: string; send: (key: string) => void };
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
  scrollback,
  scrollAsKeys,
  deps = [],
}: UseTerminalOptions): UseTerminalReturn {
  // Live ref so wheel/touch handlers (bound once on mount) pick up the
  // latest value without recreating the terminal.
  const scrollAsKeysRef = useRef(scrollAsKeys);
  scrollAsKeysRef.current = scrollAsKeys;
  const termRef = useRef<Terminal>(null);
  const fitRef = useRef<FitAddon>(null);
  const autoScrollRef = useRef(true);
  // Tracks the user's intended viewport position (distance from bottom) when
  // autoScroll is off. Used to restore position after xterm.js internally
  // resets the viewport during data writes or buffer reflow.
  const savedDeltaRef = useRef(0);
  const fitRafRef = useRef(0);
  const onResizeRef = useRef(onResize);
  onResizeRef.current = onResize;

  const immediateFit = useCallback(() => {
    const term = termRef.current;
    const fit = fitRef.current;
    const el = containerRef.current;
    if (!term || !fit || !el) return;
    // Save scroll position before fit() which can reflow the buffer
    const buf = term.buffer.active;
    const deltaFromBottom = buf.baseY - buf.viewportY;
    // fit() needs the element visible to measure; skip if hidden
    const visible = el.offsetParent && getComputedStyle(el).visibility !== "hidden";
    if (visible) {
      fit.fit();
    }
    // Always send current dimensions so the server gets a resize on connect,
    // even if the terminal tab is hidden (uses last known cols/rows).
    onResizeRef.current(term.cols, term.rows);
    if (!visible) return;
    if (autoScrollRef.current) {
      term.scrollToBottom();
    } else {
      // Restore relative scroll position after reflow
      const target = term.buffer.active.baseY - deltaFromBottom;
      term.scrollToLine(Math.max(0, target));
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
      if (!el2 || !el2.offsetParent || getComputedStyle(el2).visibility === "hidden") return;
      // Save scroll position before fit() which can reflow the buffer
      const buf = termRef.current.buffer.active;
      const delta = buf.baseY - buf.viewportY;
      fitRef.current.fit();
      onResizeRef.current(termRef.current.cols, termRef.current.rows);
      if (autoScrollRef.current) {
        termRef.current.scrollToBottom();
      } else {
        const target = termRef.current.buffer.active.baseY - delta;
        termRef.current.scrollToLine(Math.max(0, target));
      }
    });
  }, [containerRef]);

  useEffect(() => {
    if (!containerRef.current) return;

    const term = new Terminal({
      cursorBlink: true,
      fontSize: 14,
      fontFamily: "Menlo, Monaco, 'Courier New', monospace",
      ...(scrollback !== undefined ? { scrollback } : {}),
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

    const restoreOrFollow = () => {
      if (autoScrollRef.current) {
        term.scrollToBottom();
      } else if (savedDeltaRef.current >= 0) {
        // xterm.js may internally move the viewport during writes / reflow.
        // Restore the user's intended position (distance from bottom).
        const buf = term.buffer.active;
        const delta = Math.min(savedDeltaRef.current, buf.baseY);
        const target = buf.baseY - delta;
        if (buf.viewportY !== target) {
          term.scrollToLine(target);
        }
      }
    };

    const onWriteParsedDisposable = term.onWriteParsed(restoreOrFollow);

    const onRenderDisposable = term.onRender(restoreOrFollow);

    const ro = new ResizeObserver(() => safeFit());
    ro.observe(containerRef.current);

    // Scroll event listeners
    const el = containerRef.current;

    const onWheel = (e: WheelEvent) => {
      const sk = scrollAsKeysRef.current;
      if (sk) {
        // Suppress xterm's own wheel handling — the TUI manages its own
        // scroll via the forwarded keys, not via xterm scrollback.
        e.preventDefault();
        e.stopPropagation();
        if (e.deltaY < 0) sk.send(sk.up);
        else if (e.deltaY > 0) sk.send(sk.down);
        return;
      }
      if (e.deltaY < 0) {
        autoScrollRef.current = false;
        savedDeltaRef.current = -1; // pending until xterm processes wheel
        requestAnimationFrame(() => {
          if (!autoScrollRef.current) {
            const buf = term.buffer.active;
            savedDeltaRef.current = buf.baseY - buf.viewportY;
          }
        });
      } else if (e.deltaY > 0) {
        requestAnimationFrame(() => {
          const buf = term.buffer.active;
          if (buf.baseY - buf.viewportY <= 3) {
            autoScrollRef.current = true;
          } else if (!autoScrollRef.current) {
            savedDeltaRef.current = buf.baseY - buf.viewportY;
          }
        });
      }
    };
    // passive: false so the scrollAsKeys path can preventDefault and keep the
    // wheel event from reaching xterm's own handler. The default (non-grok)
    // path doesn't preventDefault, so behaviour is unchanged there.
    el.addEventListener("wheel", onWheel, { capture: true, passive: false });

    let touchStartY = 0;
    let accumDelta = 0;
    let touchDirty = false; // set after multi-touch; resets baseline on next single-touch
    const getLineHeight = () => {
      const rect = el.getBoundingClientRect();
      return (term.rows > 0 && rect.height > 0) ? rect.height / term.rows : 20;
    };
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
      // Negate so swipe-up (finger moves up, clientY decreases) yields
      // negative dy → scroll UP (older content), matching mobile terminal
      // apps (direct-manipulation style, not natural/web-page scrolling).
      const dy = e.touches[0].clientY - touchStartY;
      touchStartY = e.touches[0].clientY;
      accumDelta += dy;
      const lineHeight = getLineHeight();
      const lines = Math.trunc(accumDelta / lineHeight);
      if (lines !== 0) {
        const sk = scrollAsKeysRef.current;
        if (sk) {
          // `lines > 0` = finger moved down (matches the dy sign comment
          // above, even though the non-grok branch below uses the opposite
          // convention by handing `lines` straight to xterm.scrollLines).
          // A downward swipe is the user's "show older content" gesture, so
          // it maps to `up` — for grok-builder this fires Ctrl+K.
          const key = lines > 0 ? sk.up : sk.down;
          const count = Math.abs(lines);
          for (let i = 0; i < count; i++) sk.send(key);
        } else if (touchMode === "mouse") {
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
            if (buf.viewportY >= buf.baseY) {
              autoScrollRef.current = true;
            } else {
              savedDeltaRef.current = buf.baseY - buf.viewportY;
            }
          } else {
            autoScrollRef.current = false;
            term.scrollLines(lines);
            // scrollLines is synchronous — capture delta immediately
            const buf = term.buffer.active;
            savedDeltaRef.current = buf.baseY - buf.viewportY;
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

  // Apply scrollback changes at runtime so we don't have to recreate the
  // terminal (which would drop already-written output) when the value is
  // determined after session metadata loads. xterm.js accepts runtime mutation
  // of `options.scrollback`.
  useEffect(() => {
    const term = termRef.current;
    if (!term || scrollback === undefined) return;
    term.options.scrollback = scrollback;
  }, [scrollback]);

  return { termRef, fitRef, autoScrollRef, safeFit, immediateFit };
}

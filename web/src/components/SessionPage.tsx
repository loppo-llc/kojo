import { useCallback, useEffect, useRef, useState } from "react";
import { useParams, useNavigate, useLocation, useSearchParams } from "react-router";
import { useWebSocket } from "../hooks/useWebSocket";
import { useTerminal } from "../hooks/useTerminal";
import { api, type SessionInfo, type Attachment } from "../lib/api";
import { useEnterSends } from "../lib/preferences";
import { restoreScrollback, base64ToBytes } from "../lib/utils";
import { useSpecialKeys } from "../hooks/useSpecialKeys";
import { FileBrowser } from "./FileBrowser";
import { GitPanel } from "./GitPanel";
import { SpecialKeysBar } from "./SpecialKeysBar";
import { TerminalTab } from "./TerminalTab";
import { AttachmentsTab } from "./AttachmentsTab";

type SessionTab = "cli" | "terminal" | "files" | "git" | "attachments";

const TABS: { key: SessionTab; label: string }[] = [
  { key: "cli", label: "CLI" },
  { key: "terminal", label: "Terminal" },
  { key: "files", label: "Files" },
  { key: "git", label: "Git" },
  { key: "attachments", label: "Attach" },
];

export function SessionPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  // BrowserRouter's useNavigate returns an unstable reference (recreated
  // on every location change). Stash in a ref so effects can call it
  // without listing it as a dependency.
  const navigateRef = useRef(navigate);
  navigateRef.current = navigate;
  const location = useLocation();
  const [searchParams] = useSearchParams();
  // peerId, when present, tells the Hub's WS proxy which peer
  // hosts this session. NewSession stamps it into the URL after a
  // peer-targeted create; refreshes preserve it via the query
  // param so the browser bookmark survives a tab restore.
  const peerId = searchParams.get("peer") ?? undefined;
  const termContainerRef = useRef<HTMLDivElement>(null);
  const [session, setSession] = useState<SessionInfo>();
  const [input, setInput] = useState("");
  const [enterSends] = useEnterSends();
  const [exited, setExited] = useState(false);
  const yoloTailRef = useRef<string | null>(null);
  const yoloOverlayRef = useRef<HTMLButtonElement>(null);
  const yoloTimerRef = useRef<ReturnType<typeof setTimeout>>(null);

  // Tab state — derive from URL and keep in sync
  const tabFromPath = (pathname: string): SessionTab =>
    pathname.endsWith("/terminal") ? "terminal" : pathname.endsWith("/files") ? "files" : pathname.endsWith("/git") ? "git" : pathname.endsWith("/attachments") ? "attachments" : "cli";
  const [activeTab, setActiveTab] = useState<SessionTab>(tabFromPath(location.pathname));

  useEffect(() => {
    setActiveTab(tabFromPath(location.pathname));
  }, [location.pathname]);

  const switchTab = (tab: SessionTab) => {
    setActiveTab(tab);
    const base = `/session/${id}`;
    const path = tab === "cli" ? base : `${base}/${tab}`;
    // Preserve `?peer=<id>` across tab switches so the WS + REST
    // routing stays pointed at the peer that owns this session.
    // Without this the user would silently lose the peer route on
    // the first tab change and the next refresh.
    const target = peerId ? `${path}?peer=${encodeURIComponent(peerId)}` : path;
    navigate(target, { replace: true });
  };

  const gotScrollbackRef = useRef(false);

  // Bridge refs: useTerminal and useWebSocket have a circular dependency
  // (callbacks reference terminal, terminal needs sendInput from WS).
  // We use stable refs to break the cycle.
  const sendInputRef = useRef<(data: string) => void>(() => {});
  const sendResizeRef = useRef<(cols: number, rows: number) => void>(() => {});
  const wrapInputRef = useRef<(data: string) => string>((d) => d);

  // Non-claude CLIs (grok build, codex, ...) get a fixed-height terminal with no
  // scrollback — they don't rely on internal scrollback navigation and the
  // visible scrollbar is just noise. Claude keeps the default 1000-line buffer.
  // The scrollback value is applied at runtime in useTerminal (no recreate),
  // so output that arrives before session metadata loads isn't dropped.
  // Gate on `session.id === id`: after navigating between sessions the React
  // state still holds the previous session for a tick, and we must not apply
  // its scrollback to the new terminal — otherwise switching grok → claude
  // could pin scrollback at 0 and drop history before the new metadata loads.
  const sessionMatches = session?.id === id;
  const isClaude = sessionMatches && session?.tool === "claude";
  const isGrok = sessionMatches && session?.tool === "grok";
  // grok-builder binds its own scroll shortcuts (Ctrl+K up, Ctrl+J down) and
  // ignores xterm's internal scrollback. Forward wheel/swipe to those keys so
  // the user can drive the TUI's scroll without a keyboard.
  const scrollAsKeys = isGrok
    ? { up: "\x0b", down: "\x0a", send: (key: string) => sendInputRef.current(key) }
    : undefined;
  const { termRef: xtermRef, autoScrollRef, safeFit, immediateFit } = useTerminal({
    containerRef: termContainerRef,
    onInput: useCallback((data: string) => sendInputRef.current(wrapInputRef.current(data)), []),
    onResize: useCallback((cols: number, rows: number) => sendResizeRef.current(cols, rows), []),
    scrollback: sessionMatches ? (isClaude ? 1000 : 0) : undefined,
    scrollAsKeys,
    deps: [id],
  });

  const onOutput = useCallback((data: Uint8Array) => {
    xtermRef.current?.write(data);
  }, [xtermRef]);

  const onScrollback = useCallback((data: Uint8Array) => {
    const term = xtermRef.current;
    if (!term) return;
    gotScrollbackRef.current = true;
    restoreScrollback(term, data, autoScrollRef);
  }, [xtermRef, autoScrollRef]);

  const onExit = useCallback((exitCode: number, live: boolean) => {
    setExited(true);
    if (live) {
      xtermRef.current?.write(`\r\n\x1b[90m[Process exited with code ${exitCode}]\x1b[0m\r\n`);
    }
  }, [xtermRef]);

  const [attachments, setAttachments] = useState<Attachment[]>([]);

  const mergeAttachments = useCallback((incoming: Attachment[]) => {
    setAttachments((prev) => {
      const existing = new Set(prev.map((a) => a.path));
      const added = incoming.filter((a) => !existing.has(a.path));
      return added.length > 0 ? [...prev, ...added] : prev;
    });
  }, []);

  const onAttachment = mergeAttachments;

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
    peerId,
    onOutput,
    onScrollback,
    onExit,
    onYoloDebug,
    onAttachment,
    onConnected: immediateFit,
  });
  sendInputRef.current = sendInput;
  sendResizeRef.current = sendResize;

  const { ctrlMode, shiftMode, altMode, handleKeyPress, wrapInput } = useSpecialKeys(sendInput, autoScrollRef);
  wrapInputRef.current = wrapInput;

  // Clean up yolo timer on unmount and session switch
  useEffect(() => {
    return () => {
      if (yoloTimerRef.current) clearTimeout(yoloTimerRef.current);
      yoloTailRef.current = null;
      if (yoloOverlayRef.current) yoloOverlayRef.current.style.display = "none";
    };
  }, [id]);

  useEffect(() => {
    setExited(false);
    setAttachments([]);
    gotScrollbackRef.current = false;
    api.sessions.get(id!, peerId).then((s) => {
      setSession(s);
      if (s.status === "exited") setExited(true);
    }).catch(() => navigateRef.current("/"));
    api.sessions.attachments(id!, peerId).then(mergeAttachments).catch(() => {});
  }, [id, peerId]);

  // Show persisted lastOutput for exited sessions when no live scrollback arrived
  useEffect(() => {
    if (!session?.lastOutput || !exited) return;
    if (gotScrollbackRef.current) return;
    const term = xtermRef.current;
    if (!term) return;
    const bytes = base64ToBytes(session.lastOutput);
    term.write(bytes, () => {
      term.scrollToBottom();
    });
  }, [session, exited, xtermRef]);

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
  }, [safeFit]);

  // Refit terminal when switching back to CLI tab
  useEffect(() => {
    if (activeTab === "cli") {
      requestAnimationFrame(() => safeFit());
    }
  }, [activeTab, safeFit]);

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
      const updated = await api.sessions.restart(id, peerId);
      setSession(updated);
      setExited(false);
      reconnect();
    } catch (err) {
      console.error(err);
    }
  };

  const handleStop = async () => {
    if (!id) return;
    try {
      await api.sessions.delete(id, peerId);
      setExited(true);
    } catch (err) {
      console.error("failed to stop session", err);
    }
  };

  const handleYoloToggle = async () => {
    if (!id || !session) return;
    try {
      const updated = await api.sessions.patch(id, { yoloMode: !session.yoloMode }, peerId);
      setSession(updated);
    } catch (err) {
      console.error("failed to toggle yolo mode", err);
    }
  };

  const handleFileAttach = async () => {
    const fileInput = document.createElement("input");
    fileInput.type = "file";
    fileInput.accept = "*/*";
    fileInput.onchange = async () => {
      const file = fileInput.files?.[0];
      if (!file) return;
      const result = await api.upload(file, peerId);
      setInput((prev) => (prev ? prev + "\n" : "") + result.path);
    };
    fileInput.click();
  };

  return (
    <div ref={containerRef} className="h-full flex flex-col bg-neutral-950">
      {/* Header */}
      <header className="flex items-center gap-2 px-3 py-2 border-b border-neutral-800 shrink-0">
        <button
          onClick={() => {
            // React Router stores {idx, key} in history.state. idx is
            // the stack position — stable across replace navigations
            // (tab switches), unlike location.key which changes on
            // every replace. NaN > 0 is false (safe for hash URLs).
            const state = window.history.state as { idx?: number } | null;
            const canGoBack = typeof state?.idx === "number"
              ? state.idx > 0
              : location.key !== "default";
            if (canGoBack) {
              navigate(-1);
            } else {
              navigate("/", { replace: true });
            }
          }}
          className="text-neutral-400 hover:text-neutral-200"
        >
          &larr;
        </button>
        <span className="font-mono font-bold">{session?.tool}</span>
        <span className="text-xs text-neutral-500 truncate flex-1">{session?.workDir}</span>
        {exited ? (
          session?.exitCode !== undefined && (
            <span className="text-xs text-neutral-600">exit {session.exitCode}</span>
          )
        ) : (
          <>
            {isClaude && (
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
            )}
            <button
              onClick={handleStop}
              className="px-2.5 py-1.5 text-xs bg-neutral-800 hover:bg-red-900 text-neutral-400 hover:text-red-300 rounded min-h-[44px] min-w-[44px] flex items-center justify-center"
            >
              &#9632;
            </button>
          </>
        )}
      </header>

      {/* Tab bar — hidden when exited */}
      {!exited && (
        <div className="flex border-b border-neutral-800 shrink-0">
          {TABS.map((t) => (
            <button
              key={t.key}
              onClick={() => switchTab(t.key)}
              className={`flex-1 py-2 text-sm text-center ${
                activeTab === t.key
                  ? "text-neutral-200 border-b-2 border-neutral-400"
                  : "text-neutral-500"
              }`}
            >
              {t.label}
            </button>
          ))}
        </div>
      )}

      {/* Connection indicator */}
      {!exited && !connected && (
        <div className="px-3 py-1 bg-yellow-950 text-yellow-400 text-xs text-center">
          Reconnecting...
        </div>
      )}

      {/* Main content */}
      <div className="flex-1 min-h-0 relative">
        {/* CLI / exited terminal — visible when cli tab or exited */}
        <div
          className="absolute inset-0 flex flex-col"
          inert={(exited || activeTab === "cli") ? undefined : true}
          style={{
            visibility: (exited || activeTab === "cli") ? "visible" : "hidden",
          }}
        >
          <div className="relative flex-1 min-h-0">
            <div ref={termContainerRef} className="absolute inset-0" style={{ touchAction: "none" }} />
            {!exited && isClaude && (
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
            )}
          </div>

          {/* Controls — only when running */}
          {!exited && (
            <>
              <SpecialKeysBar ctrlMode={ctrlMode} shiftMode={shiftMode} altMode={altMode} onKeyPress={handleKeyPress} />
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
                    if (e.key === "Enter" && !e.nativeEvent.isComposing && (enterSends ? !e.shiftKey : e.shiftKey)) {
                      e.preventDefault();
                      handleSend();
                    }
                  }}
                  placeholder={enterSends ? "Type here... (Enter to send)" : "Type here... (Shift+Enter to send)"}
                  rows={Math.min(input.split("\n").length, 5)}
                  className="flex-1 px-3 py-1.5 bg-neutral-900 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500 resize-none"
                />
                <button
                  onPointerDown={(e) => e.preventDefault()}
                  onClick={handleSend}
                  className="px-3 py-1.5 bg-neutral-700 hover:bg-neutral-600 rounded text-sm"
                >
                  Enter
                </button>
              </div>
            </>
          )}
        </div>

        {/* Other tab panels — only when running */}
        {!exited && (
          <>
            <div
              className="absolute inset-0"
              inert={activeTab === "terminal" ? undefined : true}
              style={{
                visibility: activeTab === "terminal" ? "visible" : "hidden",
              }}
            >
              <TerminalTab
                parentSessionId={id!}
                workDir={session?.workDir ?? ""}
                visible={activeTab === "terminal"}
                peerId={peerId}
              />
            </div>
            <div
              className="absolute inset-0 overflow-y-auto"
              inert={activeTab === "files" ? undefined : true}
              style={{
                visibility: activeTab === "files" ? "visible" : "hidden",
              }}
            >
              <FileBrowser embedded initialPath={session?.workDir} peerId={peerId} />
            </div>
            <div
              className="absolute inset-0"
              inert={activeTab === "git" ? undefined : true}
              style={{
                visibility: activeTab === "git" ? "visible" : "hidden",
              }}
            >
              <GitPanel embedded workDir={session?.workDir} peerId={peerId} />
            </div>
            <div
              className="absolute inset-0"
              inert={activeTab === "attachments" ? undefined : true}
              style={{
                visibility: activeTab === "attachments" ? "visible" : "hidden",
              }}
            >
              <AttachmentsTab
                sessionId={id!}
                attachments={attachments}
                peerId={peerId}
                onDelete={(path) => setAttachments((prev) => prev.filter((a) => a.path !== path))}
              />
            </div>
          </>
        )}
      </div>

      {/* Exited action buttons */}
      {exited && (
        <div className="flex flex-col gap-3 px-4 py-4 border-t border-neutral-800 shrink-0">
          <button
            onClick={handleResume}
            className="w-full py-3.5 bg-neutral-800 hover:bg-neutral-700 rounded-lg text-base font-medium"
          >
            Resume
          </button>
          <button
            onClick={async () => {
              if (!session) return;
              const s = await api.sessions.create({ tool: session.tool, workDir: session.workDir, args: session.args, peerId });
              const target = peerId ? `/session/${s.id}?peer=${encodeURIComponent(peerId)}` : `/session/${s.id}`;
              navigate(target, { replace: true });
            }}
            className="w-full py-3.5 bg-neutral-800 hover:bg-neutral-700 rounded-lg text-base font-medium"
          >
            New Session
          </button>
          <button
            onClick={() => {
              const state = window.history.state as { idx?: number } | null;
              if (state && typeof state.idx === "number" && state.idx > 0) {
                navigate(-1);
              } else {
                navigate("/", { replace: true });
              }
            }}
            className="w-full py-2.5 text-sm text-neutral-500 hover:text-neutral-400"
          >
            Back
          </button>
        </div>
      )}
    </div>
  );
}

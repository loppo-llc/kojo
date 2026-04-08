import { useEffect, useLayoutEffect, useRef, useState, useCallback } from "react";
import { useParams, useNavigate } from "react-router";
import { agentApi, type AgentInfo, type AgentMessage, type AgentMessageAttachment, type ChatEvent } from "../../lib/agentApi";
import { api } from "../../lib/api";
import { localRFC3339 } from "../../lib/utils";
import { useAgentWebSocket } from "../../hooks/useAgentWebSocket";
import { ChatMessage, StreamingMessage } from "./ChatMessage";
import { AgentAvatar } from "./AgentAvatar";

const PAGE_SIZE = 30;

export function AgentChat() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [agent, setAgent] = useState<AgentInfo | null>(null);
  const [messages, setMessages] = useState<AgentMessage[]>([]);
  const [input, setInput] = useState(() => sessionStorage.getItem(`agent-draft:${id}`) ?? "");
  const [streaming, setStreaming] = useState(false);
  const [streamText, setStreamText] = useState("");
  const [streamThinking, setStreamThinking] = useState("");
  const [streamTools, setStreamTools] = useState<Array<{ id: string; name: string; input: string; output: string | null }>>([]);
  const [streamStatus, setStreamStatus] = useState("");
  const [streamStartTime, setStreamStartTime] = useState<number>(Date.now());
  const [hasMore, setHasMore] = useState(false);
  const [pendingFiles, setPendingFiles] = useState<AgentMessageAttachment[]>([]);
  const [uploading, setUploading] = useState(false);
  const [uploadError, setUploadError] = useState<string | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const messagesEndRef = useRef<HTMLDivElement>(null);
  const scrollContainerRef = useRef<HTMLDivElement>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const loadingMoreRef = useRef(false);
  const suppressAutoScrollRef = useRef(false);
  const scrollRestoreRef = useRef<{ prevScrollHeight: number; prevScrollTop: number } | null>(null);
  // ID of the synthetic abort message committed by handleAbort.
  // When the server's "done" arrives later, the aborted message can be
  // upgraded to the server's (potentially more complete) version.
  const abortedIdRef = useRef<string | null>(null);
  // Live refs for streaming content — updated synchronously in onEvent
  // so handleAbort always snapshots the latest data (React state lags).
  const liveStreamTextRef = useRef("");
  const liveStreamThinkingRef = useRef("");
  const liveStreamToolsRef = useRef<Array<{ id: string; name: string; input: string; output: string | null }>>([]);

  // Restore draft and textarea height on mount / id change
  useEffect(() => {
    const draft = sessionStorage.getItem(`agent-draft:${id}`) ?? "";
    setInput(draft);
    requestAnimationFrame(() => {
      if (textareaRef.current) {
        textareaRef.current.style.height = "auto";
        textareaRef.current.style.height =
          Math.min(textareaRef.current.scrollHeight, 150) + "px";
      }
    });
  }, [id]);

  // Load agent and initial messages
  useEffect(() => {
    if (!id) return;
    abortedIdRef.current = null; // Clear stale abort state on agent change
    agentApi.get(id).then(setAgent).catch(() => navigate("/"));
    agentApi.messages(id, PAGE_SIZE).then((r) => {
      setMessages(r.messages);
      setHasMore(r.hasMore);
    }).catch(console.error);
  }, [id, navigate]);

  const scrollToBottom = useCallback(() => {
    if (suppressAutoScrollRef.current) return;
    messagesEndRef.current?.scrollIntoView({ behavior: "smooth" });
  }, []);

  useLayoutEffect(() => {
    if (suppressAutoScrollRef.current && scrollRestoreRef.current) {
      const container = scrollContainerRef.current;
      if (container) {
        const { prevScrollHeight, prevScrollTop } = scrollRestoreRef.current;
        const delta = container.scrollHeight - prevScrollHeight;
        container.scrollTop = prevScrollTop + delta;
      }
      scrollRestoreRef.current = null;
      suppressAutoScrollRef.current = false;
      return;
    }
    scrollToBottom();
  }, [messages, scrollToBottom]);

  const loadOlderMessages = useCallback(async () => {
    if (!id || loadingMoreRef.current || !hasMore || messages.length === 0) return;
    loadingMoreRef.current = true;

    const oldestId = messages[0].id;

    try {
      const r = await agentApi.messages(id, PAGE_SIZE, oldestId);
      setHasMore(r.hasMore);
      if (r.messages.length > 0) {
        const container = scrollContainerRef.current;
        suppressAutoScrollRef.current = true;
        scrollRestoreRef.current = {
          prevScrollHeight: container?.scrollHeight ?? 0,
          prevScrollTop: container?.scrollTop ?? 0,
        };
        setMessages((prev) => [...r.messages, ...prev]);
      }
    } catch (e) {
      console.error(e);
    } finally {
      loadingMoreRef.current = false;
    }
  }, [id, hasMore, messages]);

  const resetStream = useCallback(() => {
    setStreaming(false);
    setStreamText("");
    setStreamThinking("");
    setStreamTools([]);
    setStreamStatus("");
    setStreamStartTime(Date.now());
    liveStreamTextRef.current = "";
    liveStreamThinkingRef.current = "";
    liveStreamToolsRef.current = [];
  }, []);

  const onEvent = useCallback(
    (event: ChatEvent) => {
      switch (event.type) {
        case "status":
          setStreamStatus(event.status ?? "");
          if (event.status === "thinking") {
            setStreaming(true);
            if (event.startedAt) {
              setStreamStartTime(new Date(event.startedAt).getTime());
            }
          }
          break;
        case "text":
          liveStreamTextRef.current += event.delta ?? "";
          setStreamText((prev) => prev + (event.delta ?? ""));
          break;
        case "thinking":
          liveStreamThinkingRef.current += event.delta ?? "";
          setStreamThinking((prev) => prev + (event.delta ?? ""));
          break;
        case "tool_use":
          if (event.toolName) {
            const tool = { id: event.toolUseId ?? "", name: event.toolName!, input: event.toolInput ?? "", output: null };
            liveStreamToolsRef.current = [...liveStreamToolsRef.current, tool];
            setStreamTools((prev) => [...prev, tool]);
          }
          break;
        case "tool_result": {
          const matchById = (t: { id: string; name: string; output: string | null }) =>
            event.toolUseId ? t.id === event.toolUseId : t.name === event.toolName && t.output === null;
          const liveTools = [...liveStreamToolsRef.current];
          for (let i = liveTools.length - 1; i >= 0; i--) {
            if (matchById(liveTools[i])) {
              liveTools[i] = { ...liveTools[i], output: event.toolOutput ?? "" };
              break;
            }
          }
          liveStreamToolsRef.current = liveTools;
          setStreamTools((prev) => {
            const copy = [...prev];
            for (let i = copy.length - 1; i >= 0; i--) {
              if (matchById(copy[i])) {
                copy[i] = { ...copy[i], output: event.toolOutput ?? "" };
                break;
              }
            }
            return copy;
          });
          break;
        }
        case "message": {
          if (event.message) {
            setMessages((prev) =>
              prev.some((m) => m.id === event.message!.id)
                ? prev
                : [...prev, event.message!],
            );
          }
          break;
        }
        case "done": {
          const abortedId = abortedIdRef.current;
          abortedIdRef.current = null;

          if (abortedId) {
            // Abort message was already committed by handleAbort.
            // If the server delivered a more complete version, upgrade it.
            if (event.message) {
              setMessages((prev) => {
                // If server's message already exists (e.g. stale synthesized
                // terminal from a prior turn), just remove the synthetic abort.
                if (prev.some((m) => m.id === event.message!.id && m.id !== abortedId)) {
                  return prev.filter((m) => m.id !== abortedId);
                }
                return prev.map((m) => m.id === abortedId ? event.message! : m);
              });
            }
          } else if (event.message) {
            // Normal completion — deduplicate by message ID
            setMessages((prev) =>
              prev.some((m) => m.id === event.message!.id)
                ? prev
                : [...prev, event.message!],
            );
          } else if (id) {
            // Background chat finished — reload recent and merge with older loaded messages
            agentApi.messages(id, PAGE_SIZE).then((r) => {
              setMessages((prev) => {
                const newIds = new Set(r.messages.map((m) => m.id));
                const older = prev.filter((m) => !newIds.has(m.id));
                return [...older, ...r.messages];
              });
              // Don't overwrite hasMore — older pages are already loaded
            }).catch(console.error);
          }
          // Show process error as system message (e.g. auth failures, stderr).
          if (event.errorMessage) {
            const errorContent = `⚠️ Error: ${event.errorMessage}`;
            setMessages((prev) => {
              const last = prev[prev.length - 1];
              if (last?.role === "system" && last.content === errorContent) {
                return prev;
              }
              return [
                ...prev,
                {
                  id: "error_" + Date.now(),
                  role: "system",
                  content: errorContent,
                  timestamp: localRFC3339(),
                },
              ];
            });
          }
          resetStream();
          break;
        }
        case "error": {
          abortedIdRef.current = null; // Clear on every terminal path
          const errorContent = `⚠️ Error: ${event.errorMessage || "An error occurred"}`;
          setMessages((prev) => {
            // Skip if already shown (e.g. loaded from transcript on reconnect)
            const last = prev[prev.length - 1];
            if (last?.role === "system" && last.content === errorContent) {
              return prev;
            }
            return [
              ...prev,
              {
                id: "error_" + Date.now(),
                role: "system",
                content: errorContent,
                timestamp: localRFC3339(),
              },
            ];
          });
          resetStream();
          break;
        }
      }
    },
    [id, resetStream],
  );

  const onDisconnect = useCallback(() => {
    abortedIdRef.current = null; // Finalize pending abort — "done" won't arrive on this connection
    resetStream();
  }, [resetStream]);

  const { connected, sendMessage, abort } = useAgentWebSocket({
    agentId: id!,
    onEvent,
    onDisconnect,
  });

  const handleAbort = useCallback(() => {
    // Commit partial content immediately so it survives even if the
    // server's "done" event is lost (e.g. WebSocket disconnect).
    const text = liveStreamTextRef.current;
    const thinking = liveStreamThinkingRef.current;
    const tools = liveStreamToolsRef.current;
    const hasContent = text || thinking || tools.length > 0;

    if (hasContent) {
      const syntheticId = "aborted_" + Date.now();
      abortedIdRef.current = syntheticId;
      setMessages((prev) => [...prev, {
        id: syntheticId,
        role: "assistant" as const,
        content: text,
        thinking: thinking || undefined,
        toolUses: tools.length > 0
          ? tools.map((t) => ({ id: t.id || undefined, name: t.name, input: t.input, output: t.output ?? "" }))
          : undefined,
        timestamp: localRFC3339(),
      }]);
    }

    resetStream();
    abort();
  }, [abort, resetStream]);

  const handleFileSelect = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const files = e.target.files;
    if (!files || files.length === 0) return;

    setUploading(true);
    setUploadError(null);
    try {
      const results = await Promise.allSettled(
        Array.from(files).map((file) => api.upload(file)),
      );
      const uploaded: AgentMessageAttachment[] = [];
      const failed: string[] = [];
      for (let i = 0; i < results.length; i++) {
        const r = results[i];
        if (r.status === "fulfilled") {
          uploaded.push({ path: r.value.path, name: r.value.name, size: r.value.size, mime: r.value.mime });
        } else {
          failed.push(Array.from(files)[i].name);
        }
      }
      if (uploaded.length > 0) {
        setPendingFiles((prev) => [...prev, ...uploaded]);
      }
      if (failed.length > 0) {
        setUploadError(`Upload failed: ${failed.join(", ")}`);
      }
    } finally {
      setUploading(false);
      if (fileInputRef.current) fileInputRef.current.value = "";
    }
  };

  const removePendingFile = (index: number) => {
    setPendingFiles((prev) => prev.filter((_, i) => i !== index));
  };

  const handleSend = () => {
    const text = input.trim();
    if ((!text && pendingFiles.length === 0) || streaming || !connected) return;
    abortedIdRef.current = null; // Finalize any pending abort — synthetic message stays as-is

    // Add user message immediately
    const userMsg: AgentMessage = {
      id: "pending_" + Date.now(),
      role: "user",
      content: text,
      attachments: pendingFiles.length > 0 ? pendingFiles : undefined,
      timestamp: localRFC3339(),
    };
    setMessages((prev) => [...prev, userMsg]);
    setInput("");
    if (id) sessionStorage.setItem(`agent-draft:${id}`, "");
    setStreaming(true);
    setStreamText("");
    setStreamThinking("");
    setStreamTools([]);
    setStreamStatus("thinking");
    setStreamStartTime(Date.now());
    sendMessage(text, pendingFiles.length > 0 ? pendingFiles : undefined);
    setPendingFiles([]);
    setUploadError(null);

    // Reset textarea height
    if (textareaRef.current) {
      textareaRef.current.style.height = "auto";
    }
  };

  const handleKeyDown = (e: React.KeyboardEvent) => {
    // Shift+Enter to send (matching session input pattern)
    if (e.key === "Enter" && e.shiftKey) {
      e.preventDefault();
      handleSend();
    }
  };

  const handleTextareaInput = () => {
    if (textareaRef.current) {
      textareaRef.current.style.height = "auto";
      textareaRef.current.style.height =
        Math.min(textareaRef.current.scrollHeight, 150) + "px";
    }
  };

  if (!agent) return null;

  return (
    <div className="flex flex-col h-full bg-neutral-950 text-neutral-200">
      {/* Header */}
      <header className="flex items-center gap-3 px-4 py-3 border-b border-neutral-800 shrink-0">
        <button
          onClick={() => navigate("/")}
          className="text-neutral-400 hover:text-neutral-200"
        >
          &larr;
        </button>
        <AgentAvatar agentId={agent.id} name={agent.name} size="md" cacheBust={agent.avatarHash} />
        <div className="flex-1 min-w-0">
          <div className="font-medium text-sm truncate">{agent.name}</div>
          <div className="text-xs text-neutral-500">
            {connected ? (streaming ? "typing..." : "online") : "connecting..."}
          </div>
        </div>
        <button
          onClick={() => navigate(`/agents/${agent.id}/settings`)}
          className="p-2 text-neutral-500 hover:text-neutral-300 rounded"
          title="Settings"
        >
          <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-5 h-5">
            <path fillRule="evenodd" d="M7.84 1.804A1 1 0 018.82 1h2.36a1 1 0 01.98.804l.331 1.652a6.993 6.993 0 011.929 1.115l1.598-.54a1 1 0 011.186.447l1.18 2.044a1 1 0 01-.205 1.251l-1.267 1.113a7.047 7.047 0 010 2.228l1.267 1.113a1 1 0 01.206 1.25l-1.18 2.045a1 1 0 01-1.187.447l-1.598-.54a6.993 6.993 0 01-1.929 1.115l-.33 1.652a1 1 0 01-.98.804H8.82a1 1 0 01-.98-.804l-.331-1.652a6.993 6.993 0 01-1.929-1.115l-1.598.54a1 1 0 01-1.186-.447l-1.18-2.044a1 1 0 01.205-1.251l1.267-1.114a7.05 7.05 0 010-2.227L1.821 7.773a1 1 0 01-.206-1.25l1.18-2.045a1 1 0 011.187-.447l1.598.54A6.993 6.993 0 017.51 3.456l.33-1.652zM10 13a3 3 0 100-6 3 3 0 000 6z" clipRule="evenodd" />
          </svg>
        </button>
      </header>

      {/* Messages */}
      <div ref={scrollContainerRef} className="flex-1 overflow-y-auto px-4 py-4 space-y-3">
        {/* Load more button */}
        {hasMore && (
          <div className="flex justify-center pt-1 pb-3">
            <button
              onClick={loadOlderMessages}
              disabled={loadingMoreRef.current}
              className="group relative px-4 py-1.5 text-xs text-neutral-500 hover:text-neutral-300 transition-colors disabled:opacity-50"
            >
              <span className="absolute inset-x-0 top-1/2 h-px bg-neutral-800" />
              <span className="relative inline-flex items-center gap-1.5 bg-neutral-950 px-3">
                <svg className="w-3 h-3 transition-transform group-hover:-translate-y-0.5" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round">
                  <path d="M8 12V4M4 7l4-4 4 4" />
                </svg>
                older messages
              </span>
            </button>
          </div>
        )}

        {messages.length === 0 && !streaming && (
          <div className="text-center text-neutral-600 py-16">
            <p className="text-lg mb-1">{agent.name}</p>
            <p className="text-sm">Send a message to start chatting</p>
          </div>
        )}
        {messages.map((msg) => (
          <ChatMessage
            key={msg.id}
            message={msg}
            agentName={agent.name}
            agentId={agent.id}
            avatarHash={agent.avatarHash}
          />
        ))}
        {streaming && (
          <StreamingMessage
            text={streamText}
            thinking={streamThinking}
            toolUses={streamTools}
            agentName={agent.name}
            agentId={agent.id}
            status={streamStatus}
            avatarHash={agent.avatarHash}
            startTime={streamStartTime}
          />
        )}
        <div ref={messagesEndRef} />
      </div>

      {/* Input */}
      <div className="border-t border-neutral-800 px-4 py-3 shrink-0">
        {/* Upload error */}
        {uploadError && (
          <div className="flex items-center gap-2 mb-2 px-3 py-1.5 bg-red-950/50 border border-red-900/50 rounded-lg text-xs text-red-300">
            <span className="flex-1">{uploadError}</span>
            <button onClick={() => setUploadError(null)} className="text-red-400 hover:text-red-200">
              <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" className="w-3 h-3">
                <path d="M5.28 4.22a.75.75 0 00-1.06 1.06L6.94 8l-2.72 2.72a.75.75 0 101.06 1.06L8 9.06l2.72 2.72a.75.75 0 101.06-1.06L9.06 8l2.72-2.72a.75.75 0 00-1.06-1.06L8 6.94 5.28 4.22z" />
              </svg>
            </button>
          </div>
        )}
        {/* Pending file attachments */}
        {pendingFiles.length > 0 && (
          <div className="flex flex-wrap gap-2 mb-2">
            {pendingFiles.map((file, i) => (
              <div
                key={file.path}
                className="flex items-center gap-1.5 px-2 py-1 bg-neutral-800 border border-neutral-700 rounded-lg text-xs text-neutral-300"
              >
                {file.mime.startsWith("image/") ? (
                  <img
                    src={`/api/v1/files/raw?path=${encodeURIComponent(file.path)}`}
                    alt={file.name}
                    className="w-6 h-6 rounded object-cover"
                  />
                ) : (
                  <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-4 h-4 text-neutral-500">
                    <path d="M3 3.5A1.5 1.5 0 014.5 2h6.879a1.5 1.5 0 011.06.44l4.122 4.12A1.5 1.5 0 0117 7.622V16.5a1.5 1.5 0 01-1.5 1.5h-11A1.5 1.5 0 013 16.5v-13z" />
                  </svg>
                )}
                <span className="max-w-[120px] truncate">{file.name}</span>
                <button
                  onClick={() => removePendingFile(i)}
                  className="text-neutral-500 hover:text-neutral-300 ml-0.5"
                >
                  <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" className="w-3 h-3">
                    <path d="M5.28 4.22a.75.75 0 00-1.06 1.06L6.94 8l-2.72 2.72a.75.75 0 101.06 1.06L8 9.06l2.72 2.72a.75.75 0 101.06-1.06L9.06 8l2.72-2.72a.75.75 0 00-1.06-1.06L8 6.94 5.28 4.22z" />
                  </svg>
                </button>
              </div>
            ))}
          </div>
        )}
        <div className="flex items-end gap-2">
          <input
            ref={fileInputRef}
            type="file"
            multiple
            onChange={handleFileSelect}
            className="hidden"
          />
          <button
            onClick={() => fileInputRef.current?.click()}
            disabled={uploading || streaming}
            className="p-2 text-neutral-500 hover:text-neutral-300 disabled:opacity-40 shrink-0"
            title="Attach files"
          >
            {uploading ? (
              <svg className="w-5 h-5 animate-spin" xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24">
                <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
                <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z" />
              </svg>
            ) : (
              <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-5 h-5">
                <path fillRule="evenodd" d="M15.621 4.379a3 3 0 00-4.242 0l-7 7a3 3 0 004.241 4.243h.001l.497-.5a.75.75 0 011.064 1.057l-.498.501-.002.002a4.5 4.5 0 01-6.364-6.364l7-7a4.5 4.5 0 016.368 6.36l-3.455 3.553A2.625 2.625 0 119.52 9.52l3.45-3.451a.75.75 0 111.061 1.06l-3.45 3.451a1.125 1.125 0 001.587 1.595l3.454-3.553a3 3 0 000-4.242z" clipRule="evenodd" />
              </svg>
            )}
          </button>
          <textarea
            ref={textareaRef}
            value={input}
            onChange={(e) => {
              setInput(e.target.value);
              if (id) sessionStorage.setItem(`agent-draft:${id}`, e.target.value);
            }}
            onInput={handleTextareaInput}
            onKeyDown={handleKeyDown}
            placeholder="Message..."
            rows={1}
            className="flex-1 px-3 py-2 bg-neutral-900 border border-neutral-700 rounded-xl text-sm resize-none focus:outline-none focus:border-neutral-500 max-h-[150px]"
          />
          {streaming ? (
            <button
              onClick={handleAbort}
              className="px-4 py-2 bg-red-600 hover:bg-red-500 rounded-xl text-sm font-medium shrink-0"
            >
              Stop
            </button>
          ) : (
            <button
              onClick={handleSend}
              disabled={(!input.trim() && pendingFiles.length === 0) || !connected}
              className="px-4 py-2 bg-blue-600 hover:bg-blue-500 rounded-xl text-sm font-medium disabled:opacity-40 shrink-0"
            >
              Send
            </button>
          )}
        </div>
        <div className="text-[10px] text-neutral-600 mt-1 text-center">
          Shift+Enter to send, Enter for newline
        </div>
      </div>
    </div>
  );
}

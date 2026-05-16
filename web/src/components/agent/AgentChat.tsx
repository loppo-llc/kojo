import { useEffect, useLayoutEffect, useRef, useState, useCallback } from "react";
import { useParams, useNavigate } from "react-router";
import { agentApi, type AgentInfo, type AgentMessage, type AgentMessageAttachment, type ChatEvent } from "../../lib/agentApi";
import { api } from "../../lib/api";
import { localRFC3339 } from "../../lib/utils";
import { useEnterSends } from "../../lib/preferences";
import { useAgentWebSocket } from "../../hooks/useAgentWebSocket";
import { useTTSAutoToggle, useTTSPlayer } from "../../hooks/useTTS";
import { ChatMessage, StreamingMessage } from "./ChatMessage";
import { AgentAvatar } from "./AgentAvatar";
import {
  appendSystemErrorIfNew,
  appendUniqueMessage,
  applyDoneMessage,
  applyToolResult,
  newToolFromEvent,
} from "./chatEventReducer";

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
  const [streamViewMode, setStreamViewMode] = useState<"markdown" | "plain">("markdown");
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

  // TTS — auto-play toggle (per agent, persisted in localStorage) and
  // shared player. The player is only "enabled" when both the agent has
  // a TTS config with enabled=true AND the user has the auto toggle on
  // for *manual* play; manual play also requires agent enable but does
  // not require the auto toggle. Manual is the default UX.
  const [ttsAuto, setTTSAuto] = useTTSAutoToggle(id);
  const ttsAgentEnabled = !!agent?.tts?.enabled;
  const tts = useTTSPlayer(id, ttsAgentEnabled);
  // Track which message IDs we've already auto-played in this session
  // so re-renders / message edits don't replay the same audio.
  const autoPlayedRef = useRef<Set<string>>(new Set());
  // onEvent is memoized with a narrow dep set so the WebSocket doesn't
  // tear down on every TTS state flip; the callback reads live values
  // through these refs instead.
  const ttsAutoRef = useRef(ttsAuto);
  const ttsAgentEnabledRef = useRef(ttsAgentEnabled);
  const ttsPlayRef = useRef(tts.play);
  useEffect(() => {
    ttsAutoRef.current = ttsAuto;
  }, [ttsAuto]);
  useEffect(() => {
    ttsAgentEnabledRef.current = ttsAgentEnabled;
  }, [ttsAgentEnabled]);
  useEffect(() => {
    ttsPlayRef.current = tts.play;
  }, [tts.play]);

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
        case "tool_use": {
          const tool = newToolFromEvent(event);
          if (tool) {
            liveStreamToolsRef.current = [...liveStreamToolsRef.current, tool];
            setStreamTools((prev) => [...prev, tool]);
          }
          break;
        }
        case "tool_result": {
          liveStreamToolsRef.current = applyToolResult(liveStreamToolsRef.current, event);
          setStreamTools((prev) => applyToolResult(prev, event));
          break;
        }
        case "message": {
          if (event.message) {
            setMessages((prev) => appendUniqueMessage(prev, event.message!));
          }
          break;
        }
        case "done": {
          const abortedId = abortedIdRef.current;
          abortedIdRef.current = null;

          if (abortedId) {
            // Abort message was already committed by handleAbort.
            // applyDoneMessage handles the upgrade-or-drop branch
            // for the synthetic marker; when event.message is absent
            // it falls through to a copy (the marker stays).
            if (event.message) {
              setMessages((prev) => applyDoneMessage(prev, event, abortedId));
            }
          } else if (event.message) {
            // Normal completion — appendUniqueMessage dedupes by id.
            setMessages((prev) => applyDoneMessage(prev, event, null));
            // Auto-play if both agent has TTS enabled AND the user has
            // the header toggle on. abortedId == null already guarantees
            // this is a real completion, not a synthesized abort marker.
            // Read live values through refs so the memoized onEvent
            // callback (narrow dep list) sees current TTS state.
            if (
              ttsAutoRef.current &&
              ttsAgentEnabledRef.current &&
              event.message.role === "assistant" &&
              event.message.content &&
              !autoPlayedRef.current.has(event.message.id)
            ) {
              autoPlayedRef.current.add(event.message.id);
              ttsPlayRef.current(event.message.id, event.message.content);
            }
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
            setMessages((prev) =>
              appendSystemErrorIfNew(prev, errorContent, Date.now, localRFC3339),
            );
          }
          resetStream();
          break;
        }
        case "error": {
          abortedIdRef.current = null; // Clear on every terminal path
          const errorContent = `⚠️ Error: ${event.errorMessage || "An error occurred"}`;
          setMessages((prev) =>
            appendSystemErrorIfNew(prev, errorContent, Date.now, localRFC3339),
          );
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

  const [enterSends] = useEnterSends();

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Enter" && !e.nativeEvent.isComposing) {
      if (enterSends ? !e.shiftKey : e.shiftKey) {
        e.preventDefault();
        handleSend();
      }
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
        {ttsAgentEnabled && (
          <button
            onClick={() => setTTSAuto((v) => !v)}
            className={`p-2 rounded ${
              ttsAuto
                ? "text-blue-400 hover:text-blue-300"
                : "text-neutral-500 hover:text-neutral-300"
            }`}
            title={ttsAuto ? "Auto TTS: ON" : "Auto TTS: OFF"}
            aria-pressed={ttsAuto}
          >
            {ttsAuto ? (
              <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-5 h-5">
                <path d="M9.383 3.076A1 1 0 0 1 11 4v12a1 1 0 0 1-1.617.781L5.586 13H3a1 1 0 0 1-1-1V8a1 1 0 0 1 1-1h2.586l3.797-3.924z" />
                <path d="M14.657 5.343a1 1 0 0 1 1.414 0 6 6 0 0 1 0 9.314 1 1 0 1 1-1.414-1.414 4 4 0 0 0 0-6.486 1 1 0 0 1 0-1.414z" />
                <path d="M16.95 2.464a1 1 0 0 1 1.414 0A11 11 0 0 1 18.364 17.95a1 1 0 0 1-1.414-1.414 9 9 0 0 0 0-12.728 1 1 0 0 1 0-1.344z" />
              </svg>
            ) : (
              <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-5 h-5">
                <path d="M9.383 3.076A1 1 0 0 1 11 4v12a1 1 0 0 1-1.617.781L5.586 13H3a1 1 0 0 1-1-1V8a1 1 0 0 1 1-1h2.586l3.797-3.924z" />
                <path d="M13.293 7.293a1 1 0 0 1 1.414 0L16 8.586l1.293-1.293a1 1 0 1 1 1.414 1.414L17.414 10l1.293 1.293a1 1 0 0 1-1.414 1.414L16 11.414l-1.293 1.293a1 1 0 0 1-1.414-1.414L14.586 10l-1.293-1.293a1 1 0 0 1 0-1.414z" />
              </svg>
            )}
          </button>
        )}
        {agent.tool === "llama.cpp" && (
          <button
            onClick={async () => {
              const modes = ["", "on", "off"] as const;
              const idx = modes.indexOf((agent.thinkingMode ?? "") as typeof modes[number]);
              const next = modes[(idx + 1) % modes.length];
              try {
                // Pass agent.etag as the optimistic-concurrency token.
                // Without this the toggle would be an unconditional
                // PATCH and could overwrite a settings-page edit that
                // landed since this AgentChat fetched the agent.
                const updated = await agentApi.update(
                  agent.id,
                  { thinkingMode: next },
                  agent.etag,
                );
                setAgent(updated);
              } catch (err) {
                // 412 → refetch and retry once. Any other failure is
                // swallowed (existing behavior); the button visibly
                // doesn't change so the user sees nothing happened.
                if (err instanceof Error && err.name === "PreconditionFailedError") {
                  try {
                    const fresh = await agentApi.get(agent.id);
                    const updated = await agentApi.update(
                      agent.id,
                      { thinkingMode: next },
                      fresh.etag,
                    );
                    setAgent(updated);
                  } catch { /* give up — user can retry */ }
                }
              }
            }}
            className={`px-2 py-1 rounded text-xs font-mono ${
              agent.thinkingMode === "on"
                ? "bg-blue-900/50 text-blue-300 border border-blue-700"
                : agent.thinkingMode === "off"
                  ? "bg-neutral-800 text-neutral-500 border border-neutral-700"
                  : "bg-neutral-800 text-neutral-400 border border-neutral-700"
            }`}
            title={`Thinking: ${agent.thinkingMode || "auto"}`}
          >
            {agent.thinkingMode === "on" ? "think:on" : agent.thinkingMode === "off" ? "think:off" : "think:auto"}
          </button>
        )}
        <button
          onClick={() => navigate(`/agents/${agent.id}/data`, {
            state: { kojoFileBrowser: "root", kojoFileBrowserDepth: 0 },
          })}
          className="p-2 text-neutral-500 hover:text-neutral-300 rounded"
          title="Data folder"
        >
          <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-5 h-5">
            <path d="M3.75 3A1.75 1.75 0 002 4.75v1.5c0 .199.034.39.096.568A1.75 1.75 0 002 8.25v7A1.75 1.75 0 003.75 17h12.5A1.75 1.75 0 0018 15.25v-7a1.75 1.75 0 00-.096-.932c.062-.179.096-.37.096-.568v-1.5A1.75 1.75 0 0016.25 3h-4.086a1.75 1.75 0 01-1.237-.513l-.707-.707A1.75 1.75 0 009.086 1.28L8.914 1.28H3.75z" />
          </svg>
        </button>
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
        {messages.map((msg) => {
          const editable =
            agent.tool === "llama.cpp" &&
            !streaming &&
            !msg.id.startsWith("pending_") &&
            !msg.id.startsWith("error_") &&
            !msg.id.startsWith("aborted_");
          const regeneratable =
            editable && (msg.role === "user" || msg.role === "assistant");
          return (
            <ChatMessage
              key={msg.id}
              message={msg}
              agentName={agent.name}
              agentId={agent.id}
              avatarHash={agent.avatarHash}
              ttsEnabled={ttsAgentEnabled}
              ttsPlayState={tts.state[msg.id]}
              onTTSPlay={ttsAgentEnabled ? tts.play : undefined}
              onEdit={
                editable
                  ? async (msgId, content) => {
                      // Pass the message's current etag so the server can
                      // reject the edit with 412 if someone else changed
                      // the row in the meantime. The msg here came from
                      // the most recent setMessages() so it carries the
                      // freshest etag we have.
                      try {
                        const updated = await agentApi.updateMessage(
                          agent.id,
                          msgId,
                          content,
                          msg.etag,
                        );
                        setMessages((prev) => prev.map((m) => (m.id === msgId ? { ...m, ...updated } : m)));
                      } catch (err) {
                        // 412 means our cached etag is stale — refetch the
                        // transcript so subsequent edits start from the
                        // current row. We rethrow so the ChatMessage's
                        // edit UI can show the error (the form stays open
                        // with the user's draft intact).
                        if (err instanceof Error && err.name === "PreconditionFailedError") {
                          const fresh = await agentApi.messages(agent.id, 30);
                          setMessages(fresh.messages);
                        }
                        throw err;
                      }
                    }
                  : undefined
              }
              onDelete={
                editable
                  ? async (msgId) => {
                      try {
                        await agentApi.deleteMessage(agent.id, msgId, msg.etag);
                        setMessages((prev) => prev.filter((m) => m.id !== msgId));
                      } catch (err) {
                        // Two stale-state shapes both want a refetch:
                        //   - 412: row was edited under us (etag advanced)
                        //   - 404 with conditional delete: row already
                        //     vanished. The store distinguishes this from
                        //     "row never existed" — the conditional
                        //     SoftDelete returns ErrNotFound only for
                        //     tombstoned/missing rows, and the deleteMessage
                        //     pre-read maps unrelated 404s the same way.
                        //     Either way the local view is stale; refetch
                        //     is the safe move. We do NOT auto-retry —
                        //     delete is destructive and the user should
                        //     re-confirm against fresh content.
                        const isStale =
                          err instanceof Error &&
                          (err.name === "PreconditionFailedError" ||
                            (msg.etag && /^404:/.test(err.message)));
                        if (isStale) {
                          const fresh = await agentApi.messages(agent.id, 30);
                          setMessages(fresh.messages);
                        }
                        throw err;
                      }
                    }
                  : undefined
              }
              onRegenerate={
                regeneratable
                  ? async (msgId) => {
                      // Snapshot for rollback — apply optimistic truncation +
                      // streaming state immediately so WS events (which may
                      // arrive before the HTTP response) don't race with the
                      // local slice.
                      const snapshot = messages;
                      const idx = snapshot.findIndex((m) => m.id === msgId);
                      if (idx < 0) return;
                      const keepCount = msg.role === "assistant" ? idx : idx + 1;
                      setMessages(snapshot.slice(0, keepCount));
                      setStreaming(true);
                      setStreamText("");
                      setStreamThinking("");
                      setStreamTools([]);
                      setStreamStatus("thinking");
                      setStreamStartTime(Date.now());
                      try {
                        // Pass msg.etag to catch the case where another
                        // device edited this row between the user clicking
                        // regenerate and the request landing — without the
                        // precondition the server would happily truncate
                        // against a stale view of the conversation.
                        await agentApi.regenerateMessage(agent.id, msgId, msg.etag);
                      } catch (e) {
                        // Server rejected before regen started (400/404/409/412/5xx).
                        // The backend never streamed, so no WS error will
                        // arrive — we own the rollback. On 412 we refetch so
                        // the user sees the fresh transcript before deciding
                        // whether to re-confirm regenerate (no auto-retry:
                        // regen is destructive and may now target a different
                        // row). For everything else, restore the snapshot
                        // and surface an inline error.
                        //
                        // The try/finally ensures resetStream() runs even
                        // if the 412-refetch itself fails — without it we'd
                        // leave the chat stuck in optimistic-truncated +
                        // streaming-spinner state forever.
                        try {
                          if (e instanceof Error && e.name === "PreconditionFailedError") {
                            try {
                              const fresh = await agentApi.messages(agent.id, 30);
                              setMessages(fresh.messages);
                            } catch {
                              // Refetch failed too — fall back to the
                              // snapshot so the user at least sees the
                              // transcript they had before clicking.
                              setMessages(snapshot);
                            }
                            return;
                          }
                          const errorContent = `⚠️ Error: ${e instanceof Error ? e.message : String(e)}`;
                          setMessages([
                            ...snapshot,
                            {
                              id: "error_" + Date.now(),
                              role: "system",
                              content: errorContent,
                              timestamp: localRFC3339(),
                            },
                          ]);
                        } finally {
                          resetStream();
                        }
                      }
                    }
                  : undefined
              }
            />
          );
        })}
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
            viewMode={streamViewMode}
            onViewModeChange={setStreamViewMode}
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
                    src={api.files.rawUrl(file.path)}
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
          {enterSends ? "Enter to send, Shift+Enter for newline" : "Shift+Enter to send, Enter for newline"}
        </div>
      </div>
    </div>
  );
}

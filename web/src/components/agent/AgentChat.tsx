import { useEffect, useRef, useState, useCallback } from "react";
import { useParams, useNavigate, useLocation } from "react-router";
import { agentApi, type AgentInfo, type AgentMessage, type AgentMessageAttachment, type ChatEvent } from "../../lib/agentApi";
import { errMsg, localRFC3339 } from "../../lib/utils";
import { useEnterSends } from "../../lib/preferences";
import { useAgentWebSocket } from "../../hooks/useAgentWebSocket";
import { useTTSAutoToggle, useTTSPlayer } from "../../hooks/useTTS";
import { useFileUpload } from "../../hooks/useFileUpload";
import { useDraftInput } from "../../hooks/useDraftInput";
import { useAutoGrowTextarea } from "../../hooks/useAutoGrowTextarea";
import { useChatScroll } from "../../hooks/useChatScroll";
import { useChatPagination } from "../../hooks/useChatPagination";
import {
  DismissibleError,
  PendingAttachments,
  LoadMoreButton,
  AttachButton,
  SendButton,
  StopButton,
  enterToSend,
} from "../chatComposer";
import { ChatMessage, StreamingMessage } from "./ChatMessage";
import { AgentAvatar } from "./AgentAvatar";
import { Lamp } from "../ui/Lamp";
import {
  appendSystemErrorIfNew,
  appendUniqueMessage,
  applyDoneMessage,
  applyToolResult,
  newToolFromEvent,
  type StreamingTool,
} from "./chatEventReducer";

const PAGE_SIZE = 30;

export function AgentChat() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const location = useLocation();
  // BrowserRouter's useNavigate returns an unstable reference (recreated
  // on every location change). Putting it in useEffect deps would cause
  // spurious re-runs. Stash it in a ref so effects can call it without
  // listing it as a dependency.
  const navigateRef = useRef(navigate);
  navigateRef.current = navigate;
  const [agent, setAgent] = useState<AgentInfo | null>(null);
  const [messages, setMessages] = useState<AgentMessage[]>([]);
  const { input, setInput } = useDraftInput("agent-draft", id);
  const [streaming, setStreaming] = useState(false);
  const [streamText, setStreamText] = useState("");
  const [streamThinking, setStreamThinking] = useState("");
  const [streamTools, setStreamTools] = useState<StreamingTool[]>([]);
  const [streamStatus, setStreamStatus] = useState("");
  const [streamStartTime, setStreamStartTime] = useState<number>(Date.now());
  const [streamViewMode, setStreamViewMode] = useState<"markdown" | "plain">("markdown");
  const [streamAttachments, setStreamAttachments] = useState<AgentMessageAttachment[]>([]);
  const [hasMore, setHasMore] = useState(false);
  const {
    pendingFiles,
    setPendingFiles,
    uploading,
    uploadError,
    setUploadError,
    fileInputRef,
    handleFileSelect,
    removePendingFile,
  } = useFileUpload();
  const { textareaRef, resize: handleTextareaInput } = useAutoGrowTextarea();
  // Canonical auto-scroll + pagination shared with GroupDMChat. The suppress /
  // restore refs are owned by useChatScroll; the holder-peer refetch effect
  // below and the pager both drive them through the same layout effect.
  const { messagesEndRef, scrollContainerRef, suppressAutoScrollRef, scrollRestoreRef } =
    useChatScroll(messages, id);
  const fetchOlder = useCallback(
    (oldestId: string) => agentApi.messages(id!, PAGE_SIZE, oldestId),
    [id],
  );
  const { loadOlderMessages, loadingMoreRef } = useChatPagination<AgentMessage>({
    enabled: !!id,
    hasMore,
    messages,
    scrollContainerRef,
    suppressAutoScrollRef,
    scrollRestoreRef,
    fetchOlder,
    setMessages,
    setHasMore,
  });
  // ID of the synthetic abort message committed by handleAbort.
  // When the server's "done" arrives later, the aborted message can be
  // upgraded to the server's (potentially more complete) version.
  const abortedIdRef = useRef<string | null>(null);
  // Live refs for streaming content — updated synchronously in onEvent
  // so handleAbort always snapshots the latest data (React state lags).
  const liveStreamTextRef = useRef("");
  const liveStreamThinkingRef = useRef("");
  const liveStreamToolsRef = useRef<StreamingTool[]>([]);
  const liveStreamAttachmentsRef = useRef<AgentMessageAttachment[]>([]);

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

  // Restore textarea height on mount / id change. The draft text itself is
  // restored by useDraftInput; here we only re-fit the textarea to it.
  useEffect(() => {
    requestAnimationFrame(handleTextareaInput);
  }, [id, handleTextareaInput]);

  // Monotonic refetch generation shared by every transcript fetch path
  // (initial load, holder-status refetch, onConnected refetch): each
  // fetch increments the seq and only the latest one's response is
  // allowed to commit. Guards against a slow response from one path
  // arriving after another path has already supplied a fresher
  // transcript — without this the stale response would re-order or
  // replace the newer view.
  const refetchSeqRef = useRef(0);

  // Load agent and initial messages
  useEffect(() => {
    if (!id) return;
    abortedIdRef.current = null; // Clear stale abort state on agent change
    // Clear the previous agent's transcript before fetching the new one.
    // The WS reconnects for the new id in parallel, and if its
    // onConnected refetch resolves before this GET, the merge would
    // otherwise run against the old agent's rows and mix the two
    // conversations (the late GET is then seq-discarded, so the mixture
    // would stick).
    setMessages([]);
    setHasMore(false);
    agentApi.get(id).then(setAgent).catch(() => navigateRef.current("/"));
    // Guarded by the shared seq like every other fetch path — the WS may
    // connect (and onConnected refetch+commit) before this GET returns,
    // and an unguarded late commit here would wipe that fresher merge.
    // Replace semantics are kept: this is the initial load.
    const seq = ++refetchSeqRef.current;
    agentApi.messages(id, PAGE_SIZE).then((r) => {
      if (seq !== refetchSeqRef.current) return;
      setMessages(r.messages);
      setHasMore(r.hasMore);
    }).catch(console.error);
  }, [id]);

  // §3.7 device-switch: when the agent's runtime lives on a remote
  // peer that is currently offline, the WS proxy + GET /messages
  // proxy both 502, the transcript shown is whatever stale snapshot
  // Hub has locally, and Send would just queue into the void. We
  // poll the agent record every 5 s to refresh holderPeerStatus
  // without tearing the chat down — when the holder flips back to
  // online we refetch /messages so the most-recent transcript from
  // the holder lands. Stops when the agent has no holder (local)
  // or the page unmounts.
  const holderOffline = !!agent?.holderPeer && agent.holderPeerStatus !== "online";
  // Key the seen status by holderPeer so a holder rotation (peerA →
  // peerB) doesn't silently reuse peerA's last-seen status as the
  // "prev" for peerB. Holder change with both endpoints already
  // "online" also triggers a refetch — the new holder may have
  // accumulated messages while peerA owned the lock.
  const lastHolderRef = useRef<{ peer: string; status?: string } | null>(null);
  useEffect(() => {
    // The no-fetch early returns must NOT bump the shared seq: on first
    // render agent is still null, and a bump here would discard the
    // initial [id] GET's in-flight commit (and any onConnected refetch).
    // Invalidating an in-flight *holder* refetch when the route goes
    // away or the holder returns to local is already handled by the
    // previous run's cleanup below — it's only registered when that run
    // actually issued a fetch.
    if (!id) return;
    if (!agent?.holderPeer) {
      // Agent went back to local. Drop the seen state so a future
      // re-transition to a remote holder starts fresh.
      lastHolderRef.current = null;
      return;
    }
    const prev = lastHolderRef.current;
    const currStatus = agent.holderPeerStatus;
    const currPeer = agent.holderPeer;
    lastHolderRef.current = { peer: currPeer, status: currStatus };

    // Refetch trigger:
    //   (a) holder rotation — peer changed, regardless of status,
    //   (b) status flip away-from-online → online on the SAME holder.
    // Case (a) catches new-holder-with-already-online-status; case
    // (b) catches recovery from a transient offline window.
    const holderChanged = !!prev && prev.peer !== currPeer;
    const cameOnline = !!prev && prev.peer === currPeer && prev.status !== "online" && currStatus === "online";
    if (!holderChanged && !cameOnline) return;

    // Preserve scroll position over the refetch — the user may be
    // mid-read on older content and a jump-to-bottom would be
    // disruptive. Mirrors loadOlderMessages's restore protocol.
    const container = scrollContainerRef.current;
    suppressAutoScrollRef.current = true;
    // Keep the token object's identity so the stale-discard paths below
    // can tell whether these tokens are still ours: a newer holder
    // refetch (or the pager) replaces the object, but an onConnected
    // refetch superseding us never claims it — in that case we must
    // release the tokens ourselves or a later unrelated commit would
    // consume an out-of-date scroll anchor.
    const restoreToken = {
      prevScrollHeight: container?.scrollHeight ?? 0,
      prevScrollTop: container?.scrollTop ?? 0,
    };
    scrollRestoreRef.current = restoreToken;
    const seq = ++refetchSeqRef.current;
    agentApi.messages(id, PAGE_SIZE).then((r) => {
      // Generation guard: if a newer refetch has fired (e.g. holder
      // rotated again, another flip-online, or an onConnected refetch)
      // we drop this response — applying it would either reorder the
      // transcript (older response containing only top-N rows) or
      // overwrite a fresher view with a stale snapshot. Release the
      // scroll-restore tokens if nobody re-claimed them since.
      if (seq !== refetchSeqRef.current) {
        if (scrollRestoreRef.current === restoreToken) {
          suppressAutoScrollRef.current = false;
          scrollRestoreRef.current = null;
        }
        return;
      }
      setMessages((existing) => {
        // Merge: prefer the server's view for the most recent
        // PAGE_SIZE rows, but keep
        //   1. older rows the user already paged in (any non-
        //      synthetic id absent from the fresh page), and
        //   2. local-only synthetic rows (pending_/error_/aborted_)
        //      so an in-flight optimistic message survives.
        // Mirrors the merge in the background-done branch.
        const newIds = new Set(r.messages.map((m) => m.id));
        const olderKept = existing.filter((m) => !newIds.has(m.id) && !/^(pending|error|aborted)_/.test(m.id));
        const localSynthetic = existing.filter((m) => !newIds.has(m.id) && /^(pending|error|aborted)_/.test(m.id));
        return [...olderKept, ...r.messages, ...localSynthetic];
      });
      // Don't shrink hasMore — older pages may already be loaded.
      // Only widen it if the server now reports more.
      if (r.hasMore) setHasMore(true);
    }).catch(() => {
      // Refetch failed — release the scroll-restore tokens so a
      // subsequent normal update isn't suppressed forever. Skip
      // only when someone else has re-claimed the tokens since
      // (newer holder refetch or the pager own them now).
      if (scrollRestoreRef.current !== restoreToken) return;
      suppressAutoScrollRef.current = false;
      scrollRestoreRef.current = null;
    });
    // Cleanup: when the effect re-runs (holder/status/id changed)
    // or the component unmounts, bump the seq so this iteration's
    // pending response can't commit out of order, and release the
    // scroll-restore tokens — the stale response will early-return
    // before clearing them itself, and leaving them set would let an
    // unrelated next message update consume an out-of-date scroll
    // anchor.
    return () => {
      refetchSeqRef.current++;
      suppressAutoScrollRef.current = false;
      scrollRestoreRef.current = null;
    };
  }, [id, agent?.holderPeer, agent?.holderPeerStatus]);

  // Polling refresh for the agent record so holderPeerStatus tracks
  // peer_registry without a page reload. Cheap (one row read) so 5 s
  // matches the dashboard's cadence.
  useEffect(() => {
    if (!id || !agent?.holderPeer) return;
    const t = setInterval(() => {
      agentApi.get(id).then(setAgent).catch(() => {
        // Transient — leave the previous record in place. We don't
        // want a flaky poll to clear holderPeerStatus and flip the
        // UI back to "online" when nothing changed.
      });
    }, 5000);
    return () => clearInterval(t);
  }, [id, agent?.holderPeer]);

  const resetStream = useCallback(() => {
    setStreaming(false);
    setStreamText("");
    setStreamThinking("");
    setStreamTools([]);
    setStreamAttachments([]);
    setStreamStatus("");
    setStreamStartTime(Date.now());
    liveStreamTextRef.current = "";
    liveStreamThinkingRef.current = "";
    liveStreamToolsRef.current = [];
    liveStreamAttachmentsRef.current = [];
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
        case "attachment": {
          if (event.attachments) {
            liveStreamAttachmentsRef.current = [...liveStreamAttachmentsRef.current, ...event.attachments];
            setStreamAttachments((prev) => [...prev, ...event.attachments!]);
          }
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

  // Refetch the latest transcript page on every (re)connect. A reply that
  // completed while we were disconnected is never replayed by the server
  // (resumeBackgroundChat only replays while the chat is still busy), so
  // without this a backgrounded tab wakes up to a frozen transcript until
  // a manual reload. The very first connect also refetches — the mount
  // [id] effect's GET may race the WS connect and miss a reply that lands
  // between the two; the merge below is idempotent so the duplicate
  // request is harmless.
  //
  // Shares refetchSeqRef with the holder-status refetch effect above so
  // the two paths can't overwrite each other with stale out-of-order
  // responses — only the globally latest refetch's response commits.
  const onConnected = useCallback(() => {
    if (!id) return;
    const seq = ++refetchSeqRef.current;
    agentApi.messages(id, PAGE_SIZE).then((r) => {
      if (seq !== refetchSeqRef.current) return;
      setMessages((prev) => {
        const newIds = new Set(r.messages.map((m) => m.id));
        const localSynthetic = prev.filter((m) => !newIds.has(m.id) && /^(pending|error|aborted)_/.test(m.id));
        const missing = prev.filter((m) => !newIds.has(m.id) && !/^(pending|error|aborted)_/.test(m.id));
        // Split rows the fetch didn't return by timestamp against the
        // page's oldest row: paged-in history goes before the page,
        // while WS messages that arrived after the GET snapshot was
        // taken go after it — blindly prepending everything would
        // reorder those fresh rows to the front of the transcript.
        // (Ties and unparseable timestamps fall to the older side —
        // matching the previous prepend-everything behavior — since a
        // fresh WS row is always strictly newer than the page's oldest.)
        const oldestTs = r.messages.length > 0 ? new Date(r.messages[0].timestamp).getTime() : Infinity;
        const isNewer = (m: AgentMessage) => new Date(m.timestamp).getTime() > oldestTs;
        const older = missing.filter((m) => !isNewer(m));
        const newer = missing.filter(isNewer);
        return [...older, ...r.messages, ...newer, ...localSynthetic];
      });
      if (r.hasMore) setHasMore(true);
    }).catch(console.error);
  }, [id]);

  const { connected, sendMessage, abort } = useAgentWebSocket({
    agentId: id!,
    onEvent,
    onConnected,
    onDisconnect,
  });

  const handleAbort = useCallback(() => {
    // Commit partial content immediately so it survives even if the
    // server's "done" event is lost (e.g. WebSocket disconnect).
    const text = liveStreamTextRef.current;
    const thinking = liveStreamThinkingRef.current;
    const tools = liveStreamToolsRef.current;
    const atts = liveStreamAttachmentsRef.current;
    const hasContent = text || thinking || tools.length > 0 || atts.length > 0;

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
        attachments: atts.length > 0 ? atts : undefined,
        timestamp: localRFC3339(),
      }]);
    }

    resetStream();
    abort();
  }, [abort, resetStream]);

  const handleSend = () => {
    const text = input.trim();
    if ((!text && pendingFiles.length === 0) || streaming || !connected) return;
    // Holder peer offline → the WS frame would dead-end at the Hub
    // proxy and the user's message would be lost. Refuse rather than
    // silently swallow.
    if (holderOffline) return;
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

  const handleKeyDown = (e: React.KeyboardEvent) => enterToSend(e, enterSends, handleSend);

  if (!agent) return null;

  return (
    <div className="flex flex-col h-full bg-app text-ink">
      {/* Header */}
      <header className="sticky top-0 z-40 flex h-[52px] shrink-0 items-center gap-2 border-b border-hairline bg-app/85 px-2 backdrop-blur sm:px-3">
        <button
          onClick={() => {
            // navigate(-1) pops the real history entry instead of
            // replacing with "/", which avoids accumulating dead "/"
            // entries after repeated Home → Chat → Back cycles.
            // Fall back to replace when this is the first entry
            // (e.g. opened directly from a bookmark or notification).
            //
            // React Router stores {idx, key} in history.state. idx is
            // the stack position — stable across replace navigations
            // (tab switches, settings→chat). location.key changes on
            // every replace, which would falsely allow navigate(-1)
            // after a direct load + replace. NaN > 0 is false, so
            // NaN-idx (hash URLs) falls through safely.
            const state = window.history.state as { idx?: number } | null;
            const canGoBack = typeof state?.idx === "number"
              ? state.idx > 0
              : location.key !== "default"; // fallback when idx absent
            if (canGoBack) {
              navigate(-1);
            } else {
              navigate("/", { replace: true });
            }
          }}
          className="flex h-8 w-8 shrink-0 items-center justify-center rounded-[10px] text-ink-dim transition-colors hover:bg-hover hover:text-ink lg:hidden"
          aria-label="Back"
        >
          <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" className="h-5 w-5">
            <path d="M12.5 5l-5 5 5 5" />
          </svg>
        </button>
        <AgentAvatar agentId={agent.id} name={agent.name} size="xs" cacheBust={agent.avatarHash} />
        <div className="min-w-0 flex-1">
          <div className="truncate text-[15px] font-semibold text-ink">{agent.name}</div>
          <div className="flex items-center gap-1.5 text-[11px] text-ink-dim">
            <Lamp
              state={holderOffline ? "err" : connected ? (streaming ? "warn" : "run") : "off"}
              pulse={!holderOffline && connected && streaming}
              size={6}
            />
            <span className="truncate">
              {holderOffline
                ? `host offline @ ${agent.holderPeerName || (agent.holderPeer ?? "").slice(0, 8)}`
                : connected
                  ? streaming
                    ? "typing…"
                    : "online"
                  : "connecting…"}
            </span>
          </div>
        </div>
        {ttsAgentEnabled && (
          <button
            onClick={() => setTTSAuto((v) => !v)}
            className={`rounded-[10px] p-2 transition-colors ${
              ttsAuto
                ? "text-copper hover:text-copper-bright"
                : "text-ink-faint hover:text-ink"
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
            className={`rounded-[10px] border px-2 py-1 font-mono text-[11px] transition-colors ${
              agent.thinkingMode === "on"
                ? "border-copper/50 bg-copper/10 text-copper"
                : "border-hairline bg-surface text-ink-faint hover:text-ink"
            }`}
            title={`Thinking: ${agent.thinkingMode || "auto"}`}
          >
            {agent.thinkingMode === "on" ? "think:on" : agent.thinkingMode === "off" ? "think:off" : "think:auto"}
          </button>
        )}
        <button
          onClick={() => navigate(`/agents/${agent.id}/credentials`, { replace: true })}
          className="rounded-[10px] p-2 text-ink-faint transition-colors hover:text-ink"
          title="Credentials"
          aria-label="Credentials"
        >
          <span className="text-base leading-none">🔐</span>
        </button>
        <button
          onClick={() => navigate(`/agents/${agent.id}/data`, {
            replace: true,
            state: { kojoFileBrowser: "root", kojoFileBrowserDepth: 0 },
          })}
          className="rounded-[10px] p-2 text-ink-faint transition-colors hover:text-ink"
          title="Data folder"
          aria-label="Data folder"
        >
          <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-5 h-5">
            <path d="M3.75 3A1.75 1.75 0 002 4.75v1.5c0 .199.034.39.096.568A1.75 1.75 0 002 8.25v7A1.75 1.75 0 003.75 17h12.5A1.75 1.75 0 0018 15.25v-7a1.75 1.75 0 00-.096-.932c.062-.179.096-.37.096-.568v-1.5A1.75 1.75 0 0016.25 3h-4.086a1.75 1.75 0 01-1.237-.513l-.707-.707A1.75 1.75 0 009.086 1.28L8.914 1.28H3.75z" />
          </svg>
        </button>
        <button
          onClick={() => navigate(`/agents/${agent.id}/settings`, { replace: true })}
          className="rounded-[10px] p-2 text-ink-faint transition-colors hover:text-ink"
          title="Settings"
          aria-label="Settings"
        >
          <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-5 h-5">
            <path fillRule="evenodd" d="M7.84 1.804A1 1 0 018.82 1h2.36a1 1 0 01.98.804l.331 1.652a6.993 6.993 0 011.929 1.115l1.598-.54a1 1 0 011.186.447l1.18 2.044a1 1 0 01-.205 1.251l-1.267 1.113a7.047 7.047 0 010 2.228l1.267 1.113a1 1 0 01.206 1.25l-1.18 2.045a1 1 0 01-1.187.447l-1.598-.54a6.993 6.993 0 01-1.929 1.115l-.33 1.652a1 1 0 01-.98.804H8.82a1 1 0 01-.98-.804l-.331-1.652a6.993 6.993 0 01-1.929-1.115l-1.598.54a1 1 0 01-1.186-.447l-1.18-2.044a1 1 0 01.205-1.251l1.267-1.114a7.05 7.05 0 010-2.227L1.821 7.773a1 1 0 01-.206-1.25l1.18-2.045a1 1 0 011.187-.447l1.598.54A6.993 6.993 0 017.51 3.456l.33-1.652zM10 13a3 3 0 100-6 3 3 0 000 6z" clipRule="evenodd" />
          </svg>
        </button>
      </header>

      {/* Messages */}
      <div ref={scrollContainerRef} className="flex-1 overflow-y-auto">
        <div className="mx-auto max-w-[760px] space-y-4 px-4 py-4">
        {/* Load more button */}
        {hasMore && <LoadMoreButton onClick={loadOlderMessages} loading={loadingMoreRef.current} />}

        {messages.length === 0 && !streaming && (
          <div className="py-16 text-center text-ink-faint">
            <p className="mb-1 text-lg text-ink-dim">{agent.name}</p>
            <p className="text-sm">Send a message to start chatting</p>
          </div>
        )}
        {messages.map((msg) => {
          const editable =
            agent.tool === "llama.cpp" &&
            !streaming &&
            !holderOffline &&
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
              // TTS synthesize is an agent sub-route that the remote-
              // agent proxy forwards to the holder peer. Disable
              // play while the holder is offline — otherwise the
              // request just 502s through the proxy.
              onTTSPlay={ttsAgentEnabled && !holderOffline ? tts.play : undefined}
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
                          const errorContent = `⚠️ Error: ${errMsg(e)}`;
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
            attachments={streamAttachments}
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
      </div>

      {/* Composer */}
      <div className="sticky bottom-0 z-30 shrink-0 border-t border-hairline bg-app/92 backdrop-blur">
        <div className="mx-auto max-w-[760px] px-4 py-3">
        {/* Holder offline banner — replaces the live indicator while the
            §3.7 device-switch target is unreachable. The transcript shown
            above this banner is whatever Hub has locally; latest messages
            from the holder will land once it reconnects (see status-flip
            refetch in the holderPeerStatus effect). */}
        {holderOffline && (
          <div className="mb-2 flex items-center gap-2 rounded-[10px] border border-lamp-warn/40 bg-lamp-warn/10 px-3 py-1.5 text-xs text-lamp-warn">
            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="h-3.5 w-3.5 shrink-0">
              <path fillRule="evenodd" d="M8.485 2.495c.673-1.167 2.357-1.167 3.03 0l6.28 10.875c.673 1.167-.17 2.625-1.516 2.625H3.72c-1.347 0-2.189-1.458-1.515-2.625L8.485 2.495zM10 5a.75.75 0 01.75.75v3.5a.75.75 0 01-1.5 0v-3.5A.75.75 0 0110 5zm0 9a1 1 0 100-2 1 1 0 000 2z" clipRule="evenodd" />
            </svg>
            <span className="flex-1">
              ホスト端末 <span className="font-mono">{agent.holderPeerName || (agent.holderPeer ?? "").slice(0, 8)}</span> がオフライン。復帰まで送信不可。
            </span>
          </div>
        )}
        {/* Upload error */}
        {uploadError && <DismissibleError message={uploadError} onDismiss={() => setUploadError(null)} />}
        {/* Pending file attachments */}
        <PendingAttachments files={pendingFiles} onRemove={removePendingFile} thumb />
        <div className="flex items-end gap-2">
          <input
            ref={fileInputRef}
            type="file"
            multiple
            onChange={handleFileSelect}
            className="hidden"
          />
          <AttachButton
            onClick={() => fileInputRef.current?.click()}
            uploading={uploading}
            disabled={uploading || streaming || holderOffline}
            title={holderOffline ? "Holder peer offline" : "Attach files"}
          />
          <div className="min-w-0 flex-1 rounded-xl border border-hairline bg-raised px-1 focus-within:border-copper/50">
            <textarea
              ref={textareaRef}
              value={input}
              onChange={(e) => setInput(e.target.value)}
              onInput={handleTextareaInput}
              onKeyDown={handleKeyDown}
              placeholder={`Message… (${enterSends ? "Enter" : "Ctrl+Enter"} to send)`}
              rows={1}
              className="max-h-[150px] w-full resize-none bg-transparent px-3 py-2 text-[14px] text-ink placeholder:text-ink-faint focus:outline-none"
            />
          </div>
          {streaming ? (
            <StopButton onClick={handleAbort} />
          ) : (
            <SendButton
              onClick={handleSend}
              disabled={(!input.trim() && pendingFiles.length === 0) || !connected || holderOffline}
              title={holderOffline ? `Holder peer is offline — send disabled until @ ${agent.holderPeerName || (agent.holderPeer ?? "").slice(0, 8)} reconnects` : undefined}
            />
          )}
        </div>
        </div>
      </div>
    </div>
  );
}

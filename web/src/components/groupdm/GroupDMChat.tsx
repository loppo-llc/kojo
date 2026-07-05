import { useEffect, useRef, useState, useCallback } from "react";
import { useParams, useNavigate, useSearchParams } from "react-router";
import {
  groupdmApi,
  USER_SENDER_ID,
  type GroupDMInfo,
  type GroupDMStyle,
  type GroupDMVenue,
  DEFAULT_GROUPDM_VENUE,
  DEFAULT_MAX_HOPS,
  MAX_MAX_HOPS,
  setLastRead,
  clearLastRead,
  type GroupMessage,
} from "../../lib/groupdmApi";
import { useEnterSends } from "../../lib/preferences";
import { formatTime } from "../../lib/utils";
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
  enterToSend,
} from "../chatComposer";
import { AgentAvatar } from "../agent/AgentAvatar";
import { AttachmentList, MessageContent, CollapsedToolUses } from "../agent/ChatMessage";
import { ThinkingBlock } from "../agent/StreamingMessage";
import { agentApi } from "../../lib/agentApi";
import { estimateTurnCost } from "../../lib/pricing";

const PAGE_SIZE = 50;

// Mirrors the server-side thread turn cap (notifyTimeout): after this long
// without a reply the "replying…" indicator and the send lockout both clear.
const THREAD_REPLY_TIMEOUT_MS = 10 * 60 * 1000;

export function GroupDMChat() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  // Draft mode: /groupdms/new?agent=<id> renders the chat for a not-yet-created
  // thread. The room is only created (POST /api/v1/threads) when the first
  // message is sent, so a mis-click never leaves an empty thread behind.
  const draftAgentId = searchParams.get("agent");
  const isDraft = !id;
  const [group, setGroup] = useState<GroupDMInfo | null>(null);
  const [messages, setMessages] = useState<GroupMessage[]>([]);
  const [hasMore, setHasMore] = useState(false);
  // Canonical auto-scroll + pagination shared with AgentChat.
  const { messagesEndRef, scrollContainerRef, suppressAutoScrollRef, scrollRestoreRef } =
    useChatScroll(messages, id);
  const fetchOlder = useCallback(
    (oldestId: string) => groupdmApi.messages(id!, PAGE_SIZE, oldestId),
    [id],
  );
  const { loadOlderMessages, loadingMoreRef } = useChatPagination<GroupMessage>({
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

  const [notFound, setNotFound] = useState(false);
  // Model of a thread room's single agent member — used to price the token
  // usage line under agent replies.
  const [threadAgentModel, setThreadAgentModel] = useState<string | undefined>(undefined);
  const [showStyleMenu, setShowStyleMenu] = useState(false);
  const [showVenueMenu, setShowVenueMenu] = useState(false);
  const [editingCooldown, setEditingCooldown] = useState(false);
  const [cooldownInput, setCooldownInput] = useState("");
  const [editingMaxHops, setEditingMaxHops] = useState(false);
  const [maxHopsInput, setMaxHopsInput] = useState("");
  const [editingName, setEditingName] = useState(false);
  const [nameInput, setNameInput] = useState("");
  const nameInputRef = useRef<HTMLInputElement>(null);
  const [showDeleteDialog, setShowDeleteDialog] = useState(false);
  const [deleteNotify, setDeleteNotify] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [deleteError, setDeleteError] = useState("");
  const deleteDialogRef = useRef<HTMLDivElement>(null);
  const [showArchiveDialog, setShowArchiveDialog] = useState(false);
  const [archiving, setArchiving] = useState(false);
  const [archiveError, setArchiveError] = useState("");
  const archiveDialogRef = useRef<HTMLDivElement>(null);
  const [showClearDialog, setShowClearDialog] = useState(false);
  const [clearing, setClearing] = useState(false);
  const [clearError, setClearError] = useState("");
  const clearDialogRef = useRef<HTMLDivElement>(null);

  // User input
  // Draft rooms have no id yet, so key the composer draft on the target agent
  // instead — switching between two draft threads must not leak text between
  // them.
  const { input, setInput, clearDraft } = useDraftInput("groupdm-draft", id ?? draftAgentId ?? undefined);
  // Holds the room created for this draft on the first send (tagged with the
  // agent it belongs to), so a retry after a failed post reuses it instead of
  // littering a second empty thread — and so a room created for agent A can
  // never be reused (or adopted mid-flight) after switching to a draft for
  // agent B.
  const draftRoomRef = useRef<{ agentId: string; id: string } | null>(null);
  // Mirrors the CURRENT draft agent so an in-flight createThread started for a
  // previous draft can detect the switch and discard its result.
  const currentDraftAgentRef = useRef<string | null>(draftAgentId);
  useEffect(() => {
    currentDraftAgentRef.current = draftAgentId;
    return () => {
      // Invalidate the draft generation on unmount (or agent switch) so a
      // slow send that finishes afterwards cannot re-navigate the user to
      // the old thread from wherever they went next.
      currentDraftAgentRef.current = null;
    };
  }, [draftAgentId]);
  const [sending, setSending] = useState(false);
  const [sendError, setSendError] = useState<string | null>(null);
  const { textareaRef, resize: handleTextareaInput } = useAutoGrowTextarea(input);
  const [enterSends] = useEnterSends();
  const pollGenerationRef = useRef(0);

  // File attachments — uploaded to /api/v1/blobs, attached to the user
  // message on send, then cleared on success.
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

  // Reset send-error + textarea height when id changes. The draft text
  // itself is restored by useDraftInput.
  useEffect(() => {
    if (!id) return;
    setSendError(null);
    requestAnimationFrame(handleTextareaInput);
  }, [id, handleTextareaInput]);

  useEffect(() => {
    if (editingName) {
      nameInputRef.current?.focus();
      nameInputRef.current?.select();
    }
  }, [editingName]);

  // Focus delete dialog overlay for Escape key
  useEffect(() => {
    if (showDeleteDialog) deleteDialogRef.current?.focus();
  }, [showDeleteDialog]);

  // Focus archive dialog overlay for Escape key
  useEffect(() => {
    if (showArchiveDialog) archiveDialogRef.current?.focus();
  }, [showArchiveDialog]);

  // Focus clear-history dialog overlay for Escape key
  useEffect(() => {
    if (showClearDialog) clearDialogRef.current?.focus();
  }, [showClearDialog]);

  // Reset state when id changes
  useEffect(() => {
    pollGenerationRef.current += 1;
    setGroup(null);
    setMessages([]);
    setHasMore(false);
    setNotFound(false);
    setEditingName(false);
    setShowDeleteDialog(false);
    setDeleteNotify(false);
    setDeleteError("");
    setShowClearDialog(false);
    setClearing(false);
    setClearError("");
    setPendingFiles([]);
    setUploadError(null);
    draftRoomRef.current = null;
  }, [id, draftAgentId]);

  // Load group info
  useEffect(() => {
    if (!id) return;
    groupdmApi.get(id).then(setGroup).catch((e) => {
      if (e instanceof Error && e.message.startsWith("404")) {
        setNotFound(true);
      }
    });
  }, [id]);

  // Draft mode: synthesize a thread group header from the target agent so the
  // chat renders (agent name, empty transcript, enabled input) without
  // creating a room yet.
  useEffect(() => {
    if (!isDraft) return;
    if (!draftAgentId) {
      setNotFound(true);
      return;
    }
    let cancelled = false;
    agentApi
      .get(draftAgentId)
      .then((a) => {
        if (cancelled) return;
        setGroup({
          id: "",
          name: a.name,
          kind: "thread",
          members: [{ agentId: a.id, agentName: a.name }],
          cooldown: 0,
          style: "efficient",
          createdAt: "",
          updatedAt: "",
        });
        setThreadAgentModel(a.model);
      })
      .catch(() => {
        if (!cancelled) setNotFound(true);
      });
    return () => {
      cancelled = true;
    };
  }, [isDraft, draftAgentId]);

  // Resolve the thread agent's model for pricing the usage line. Only threads
  // (single agent member) carry per-reply usage, so skip group rooms.
  useEffect(() => {
    if (!group) return;
    const threadLike =
      group.kind === "thread" || (group.kind === "dm" && group.members.length === 1);
    if (!threadLike || group.members.length === 0) return;
    let cancelled = false;
    agentApi
      .get(group.members[0].agentId)
      .then((a) => {
        if (!cancelled) setThreadAgentModel(a.model);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [group]);

  // Poll for new messages — merge with already-loaded older messages when
  // the server still reports older pages. A fully-covered page replaces the
  // local transcript so remote clears do not keep stale rows on screen.
  const initialLoadDone = useRef(false);
  const lastHeadRef = useRef<string>("");
  // Tracks the head we've SUCCESSFULLY persisted as read server-side. Kept
  // separate from lastHeadRef (which drives the group-info refresh) so a
  // failed markRead is retried on the next poll instead of being lost — we
  // only advance this on success.
  const lastMarkedReadRef = useRef<string>("");
  useEffect(() => {
    if (!id || notFound) return;
    initialLoadDone.current = false;
    lastHeadRef.current = "";
    lastMarkedReadRef.current = "";
    const load = () => {
      const generation = pollGenerationRef.current;
      groupdmApi.messages(id, PAGE_SIZE).then((r) => {
        if (generation !== pollGenerationRef.current) return;
        // Refresh the group (name/settings) whenever the transcript head
        // advances — the open chat header otherwise never picks up an
        // auto-title/rename that lands between renders. Piggybacks on the
        // message poll so it costs one extra GET only when something changed.
        const head = r.messages.length > 0 ? r.messages[r.messages.length - 1].id : "";
        if (head && head !== lastHeadRef.current) {
          groupdmApi.get(id).then((g) => {
            if (generation !== pollGenerationRef.current) return;
            // Advance only on success so a failed GET is retried on the
            // next poll instead of silently pinning a stale header.
            lastHeadRef.current = head;
            setGroup(g);
          }).catch(() => {});
        }
        // Persist the read cursor server-side so the unread badge survives a
        // daemon restart (the localStorage cursor below is lost when the
        // browser origin/storage changes on restart). Only advance the marker
        // on success so a transient failure is retried on the next poll rather
        // than leaving the server cursor stale.
        if (head && head !== lastMarkedReadRef.current) {
          groupdmApi
            .markRead(id, head)
            .then(() => {
              lastMarkedReadRef.current = head;
            })
            .catch(() => {});
        }
        setMessages((prev) => {
          if (!r.hasMore) return r.messages;
          const newIds = new Set(r.messages.map((m) => m.id));
          const older = prev.filter((m) => !newIds.has(m.id));
          return [...older, ...r.messages];
        });
        // The room is open, so everything fetched counts as read. The
        // room list uses this marker for its unread badges.
        if (r.messages.length > 0) {
          setLastRead(id, r.messages[r.messages.length - 1].id);
        }
        if (!initialLoadDone.current) {
          setHasMore(r.hasMore);
          initialLoadDone.current = true;
        } else if (!r.hasMore) {
          setHasMore(false);
        }
      }).catch(console.error);
    };
    load();
    const interval = setInterval(load, 3000);
    return () => clearInterval(interval);
  }, [id, notFound]);

  // A "thread" room is a human↔agent DM with a single agent member: a
  // temporary Slack-thread-like side conversation. Group-only affordances
  // (cooldown / hops settings) are hidden and Delete becomes Archive.
  const isThread =
    !!group &&
    (group.kind === "thread" || (group.kind === "dm" && group.members.length === 1));

  // In a thread, the newest message being the user's own post means the
  // agent's turn is still running (polling will append the reply and flip
  // this off). Safety: the server-side thread turn is capped at 10 minutes
  // (notifyTimeout), so this also clears once the last user message is older
  // than that — it must not persist forever across reloads or on a rare
  // empty reply. Recomputed on every 3s poll re-render. Drives both the
  // "replying…" indicator and the send lockout: posting mid-turn would queue
  // a second turn that replies without seeing the first answer.
  const lastMsg = messages.length > 0 ? messages[messages.length - 1] : null;
  const awaitingReply =
    isThread &&
    lastMsg !== null &&
    lastMsg.agentId === USER_SENDER_ID &&
    Date.now() - new Date(lastMsg.timestamp).getTime() < THREAD_REPLY_TIMEOUT_MS;

  const handleSend = useCallback(async () => {
    const text = input.trim();
    if ((!text && pendingFiles.length === 0) || sending || awaitingReply) return;
    if (!id && !isDraft) return;
    if (isDraft && !draftAgentId) return;
    setSending(true);
    setSendError(null);
    try {
      // Draft mode: create the thread room now (first message = commit), then
      // post into it and swap the URL to the real room (replace so Back does
      // not return to the throwaway draft route).
      let targetId = id;
      const sendAgentId = draftAgentId;
      if (isDraft) {
        // Reuse a room created by a prior (failed-at-post) attempt for the
        // SAME agent so a retry does not create a second empty thread.
        let roomId =
          draftRoomRef.current?.agentId === sendAgentId ? draftRoomRef.current.id : null;
        if (!roomId) {
          const room = await groupdmApi.createThread(sendAgentId!);
          // The draft may have switched to another agent while the create was
          // in flight — discard the result instead of adopting a room that
          // belongs to the previous draft (posting there would message the
          // wrong agent). Archive the just-created room (best-effort) so the
          // discard doesn't litter an empty thread.
          if (currentDraftAgentRef.current !== sendAgentId) {
            groupdmApi.archive(room.id).catch(() => {});
            return;
          }
          draftRoomRef.current = { agentId: sendAgentId!, id: room.id };
          roomId = room.id;
        }
        targetId = roomId;
      }
      const sent = await groupdmApi.postUserMessage(
        targetId!,
        text,
        pendingFiles.length > 0 ? pendingFiles : undefined,
      );
      // Clear input + draft on success
      clearDraft();
      setPendingFiles([]);
      setUploadError(null);
      if (textareaRef.current) {
        textareaRef.current.style.height = "auto";
      }
      // Own message counts as read immediately — otherwise navigating away
      // before the next poll would badge the room for the user's own post.
      // Persist both the local cursor and the server-side one so the read
      // state holds even on a different device / after a restart.
      setLastRead(targetId!, sent.id);
      groupdmApi.markRead(targetId!, sent.id).catch(() => {});
      if (isDraft) {
        // Only swap the URL if the user is still on this draft — a slow send
        // finishing after they navigated to another draft must not yank them
        // to the old thread. The message itself is already posted.
        if (currentDraftAgentRef.current === sendAgentId) {
          // The real room view mounts on the replaced URL and loads the
          // transcript (including this message) via its own poll.
          navigate(`/groupdms/${targetId}`, { replace: true });
        }
        return;
      }
      // Optimistically append — the polling loop will reconcile via id dedupe.
      setMessages((prev) => {
        if (prev.some((m) => m.id === sent.id)) return prev;
        return [...prev, sent];
      });
    } catch (e) {
      setSendError(e instanceof Error ? e.message : "Failed to send");
    } finally {
      setSending(false);
    }
  }, [id, isDraft, draftAgentId, navigate, input, pendingFiles, sending, awaitingReply, clearDraft, setPendingFiles, setUploadError, textareaRef]);

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLTextAreaElement>) => enterToSend(e, enterSends, () => void handleSend()),
    [enterSends, handleSend],
  );

  // Conditional renders MUST live below every hook above. Moving them up
  // would change the hook count between renders (notFound flips after the
  // initial groupdmApi.get failure) and React would throw.
  if (notFound) {
    return (
      <div className="flex min-h-full flex-col items-center justify-center gap-3 bg-app text-ink">
        <p className="text-ink-faint">Group not found</p>
        <button
          onClick={() => navigate("/")}
          className="rounded-[10px] border border-hairline bg-surface px-4 py-2 text-sm text-ink-dim transition-colors hover:bg-hover hover:text-ink"
        >
          Back
        </button>
      </div>
    );
  }

  if (!group) return null;

  // Build a color map for agents
  const palette = [
    "text-blue-400",
    "text-emerald-400",
    "text-amber-400",
    "text-purple-400",
    "text-rose-400",
    "text-cyan-400",
    "text-orange-400",
    "text-pink-400",
  ];
  const agentColors = new Map<string, string>(
    group.members.map((m, i) => [m.agentId, palette[i % palette.length]]),
  );

  return (
    <div className="flex flex-col h-full bg-app text-ink">
      {/* Header */}
      <header className="sticky top-0 z-40 flex h-[52px] shrink-0 items-center gap-2 border-b border-hairline bg-app/85 px-2 backdrop-blur sm:px-3">
        <button
          onClick={() => navigate("/")}
          className="flex h-8 w-8 shrink-0 items-center justify-center rounded-[10px] text-ink-dim transition-colors hover:bg-hover hover:text-ink lg:hidden"
          aria-label="Back"
        >
          <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" className="h-5 w-5">
            <path d="M12.5 5l-5 5 5 5" />
          </svg>
        </button>
        <div className="flex shrink-0 -space-x-1.5">
          {group.members.slice(0, 3).map((m) => (
            <AgentAvatar key={m.agentId} agentId={m.agentId} name={m.agentName} size="xs" />
          ))}
        </div>
        <div className="min-w-0 flex-1">
          {isDraft ? (
            // Draft threads have no room yet — nothing to rename.
            <div className="w-full truncate text-left text-[15px] font-semibold text-ink">
              {group.name}
            </div>
          ) : editingName ? (
            <input
              ref={nameInputRef}
              type="text"
              value={nameInput}
              onChange={(e) => setNameInput(e.target.value)}
              onBlur={() => {
                const trimmed = nameInput.trim();
                setEditingName(false);
                if (trimmed && trimmed !== group.name) {
                  groupdmApi.rename(group.id, trimmed).then(setGroup).catch((err) =>
                    console.error("Failed to rename group", err),
                  );
                }
              }}
              onKeyDown={(e) => {
                if (e.key === "Escape") {
                  setNameInput(group.name);
                  setEditingName(false);
                } else if (e.key === "Enter" && !e.nativeEvent.isComposing) {
                  e.preventDefault();
                  (e.target as HTMLInputElement).blur();
                }
              }}
              className="w-full rounded-[10px] border border-hairline bg-raised px-1.5 py-0.5 text-[15px] font-semibold text-ink focus:border-copper focus:outline-none"
              aria-label="Group name"
              maxLength={100}
            />
          ) : (
            <button
              type="button"
              className="w-full truncate border-none bg-transparent p-0 text-left text-[15px] font-semibold text-ink transition-colors hover:text-copper"
              onClick={() => {
                setNameInput(group.name);
                setEditingName(true);
              }}
              title="Click to rename"
            >
              {group.name}
            </button>
          )}
          <div className="truncate text-[11px] text-ink-dim">
            {group.members.length <= 3
              ? group.members.map((m) => m.agentName).join(", ")
              : group.members.slice(0, 2).map((m) => m.agentName).join(", ") + `, +${group.members.length - 2}`}
          </div>
        </div>
        {/* Style selector — group-only (threads keep only Archive). */}
        {!isThread && (
        <div className="shrink-0 relative">
          <button
            onClick={() => setShowStyleMenu((v) => !v)}
            className="cursor-pointer rounded-[10px] px-1.5 py-1 text-base leading-none text-ink-faint transition-colors hover:bg-hover hover:text-ink"
            title={`Style: ${group.style || "efficient"}`}
          >
            {(group.style || "efficient") === "efficient" ? "⚡" : "💬"}
          </button>
          {showStyleMenu && (
            <>
              <div className="fixed inset-0 z-10" onClick={() => setShowStyleMenu(false)} />
              <div className="absolute right-0 top-full mt-1 z-20 min-w-[140px] rounded-[10px] border border-hairline bg-raised py-1 shadow-xl shadow-black/40">
                {([["efficient", "⚡ Efficient"], ["expressive", "💬 Expressive"]] as const).map(([value, label]) => (
                  <button
                    key={value}
                    onClick={async () => {
                      setShowStyleMenu(false);
                      if (value === (group.style || "efficient")) return;
                      try {
                        const updated = await groupdmApi.setStyle(group.id, value as GroupDMStyle);
                        setGroup(updated);
                      } catch (err) {
                        console.error("Failed to set style", err);
                      }
                    }}
                    className={`w-full cursor-pointer px-3 py-1.5 text-left text-xs transition-colors hover:bg-hover ${
                      value === (group.style || "efficient") ? "text-ink" : "text-ink-dim"
                    }`}
                  >
                    {label}
                  </button>
                ))}
              </div>
            </>
          )}
        </div>
        )}
        {/* Venue selector — physical-setting hint that calibrates how members
            should speak (text-only chatroom vs. co-located in-person).
            Group-only. */}
        {!isThread && (
        <div className="shrink-0 relative">
          <button
            onClick={() => setShowVenueMenu((v) => !v)}
            className="cursor-pointer rounded-[10px] px-1.5 py-1 text-base leading-none text-ink-faint transition-colors hover:bg-hover hover:text-ink"
            title={`Venue: ${group.venue || DEFAULT_GROUPDM_VENUE}`}
          >
            {(group.venue || DEFAULT_GROUPDM_VENUE) === "colocated" ? "🏠" : "💻"}
          </button>
          {showVenueMenu && (
            <>
              <div className="fixed inset-0 z-10" onClick={() => setShowVenueMenu(false)} />
              <div className="absolute right-0 top-full mt-1 z-20 min-w-[200px] rounded-[10px] border border-hairline bg-raised py-1 shadow-xl shadow-black/40">
                {(
                  [
                    ["chatroom", "💻 Closed chat room", "Text-only, not co-present"],
                    ["colocated", "🏠 Same physical space", "Members are co-present in real space"],
                  ] as const
                ).map(([value, label, hint]) => (
                  <button
                    key={value}
                    onClick={async () => {
                      setShowVenueMenu(false);
                      if (value === (group.venue || DEFAULT_GROUPDM_VENUE)) return;
                      try {
                        const updated = await groupdmApi.setVenue(group.id, value as GroupDMVenue);
                        setGroup(updated);
                      } catch (err) {
                        console.error("Failed to set venue", err);
                      }
                    }}
                    className={`w-full cursor-pointer px-3 py-1.5 text-left text-xs transition-colors hover:bg-hover ${
                      value === (group.venue || DEFAULT_GROUPDM_VENUE) ? "text-ink" : "text-ink-dim"
                    }`}
                  >
                    <div>{label}</div>
                    <div className="text-[10px] text-ink-faint">{hint}</div>
                  </button>
                ))}
              </div>
            </>
          )}
        </div>
        )}
        {/* Cooldown setting — group-only (threads have no notify fan-out) */}
        {!isThread && (
        <div className="shrink-0">
          {editingCooldown ? (
            <form
              className="flex items-center gap-1"
              onSubmit={async (e) => {
                e.preventDefault();
                const val = parseInt(cooldownInput, 10);
                if (isNaN(val) || val < 0) return;
                try {
                  const updated = await groupdmApi.setCooldown(group.id, val);
                  setGroup(updated);
                } catch (err) {
                  console.error("Failed to set cooldown", err);
                }
                setEditingCooldown(false);
              }}
            >
              <input
                type="number"
                min="0"
                value={cooldownInput}
                onChange={(e) => setCooldownInput(e.target.value)}
                className="w-16 rounded-[10px] border border-hairline bg-raised px-1.5 py-0.5 text-center text-xs text-ink focus:border-copper focus:outline-none"
                autoFocus
                onBlur={() => setEditingCooldown(false)}
              />
              <span className="text-[10px] text-ink-faint">s</span>
            </form>
          ) : (
            <button
              onClick={() => {
                setCooldownInput(String(group.cooldown || 50));
                setEditingCooldown(true);
              }}
              className="rounded-[10px] px-1.5 py-0.5 font-mono text-[10px] text-ink-faint transition-colors hover:text-ink"
              title="Notification cooldown (seconds)"
            >
              {group.cooldown || 50}s
            </button>
          )}
        </div>
        )}
        {/* Max hops setting — agent-to-agent relay hop limit (0 = default 4).
            Group-only: threads never relay to other agents. */}
        {!isThread && (
        <div className="shrink-0">
          {editingMaxHops ? (
            <form
              className="flex items-center gap-1"
              onSubmit={async (e) => {
                e.preventDefault();
                const raw = maxHopsInput.trim();
                const val = raw === "" ? 0 : parseInt(raw, 10);
                if (isNaN(val) || val < 0 || val > MAX_MAX_HOPS) return;
                try {
                  const updated = await groupdmApi.setMaxHops(group.id, val);
                  setGroup(updated);
                } catch (err) {
                  console.error("Failed to set max hops", err);
                }
                setEditingMaxHops(false);
              }}
            >
              <input
                type="number"
                min="0"
                max={MAX_MAX_HOPS}
                value={maxHopsInput}
                onChange={(e) => setMaxHopsInput(e.target.value)}
                placeholder={String(DEFAULT_MAX_HOPS)}
                aria-label="Max hops"
                className="w-16 rounded-[10px] border border-hairline bg-raised px-1.5 py-0.5 text-center text-xs text-ink focus:border-copper focus:outline-none"
                autoFocus
                onBlur={() => setEditingMaxHops(false)}
              />
              <span className="text-[10px] text-ink-faint">hops</span>
            </form>
          ) : (
            <button
              onClick={() => {
                setMaxHopsInput(group.maxHops ? String(group.maxHops) : "");
                setEditingMaxHops(true);
              }}
              className="rounded-[10px] px-1.5 py-0.5 font-mono text-[10px] text-ink-faint transition-colors hover:text-ink"
              title="Max relay hops (empty = default 4, max 20)"
            >
              {group.maxHops || DEFAULT_MAX_HOPS}hops
            </button>
          )}
        </div>
        )}
        {!isThread && (
        <button
          onClick={() => {
            setClearError("");
            setShowClearDialog(true);
          }}
          className="rounded-[10px] p-2 text-ink-faint transition-colors hover:text-lamp-warn"
          title="Clear message history"
        >
          <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-5 h-5">
            <path d="M5.25 3A2.25 2.25 0 003 5.25v9.5A2.25 2.25 0 005.25 17h5.378a2.25 2.25 0 001.591-.659l4.122-4.122A2.25 2.25 0 0017 10.628V5.25A2.25 2.25 0 0014.75 3h-9.5zM4.5 5.25c0-.414.336-.75.75-.75h9.5c.414 0 .75.336.75.75v4.5H12.25a2.5 2.5 0 00-2.5 2.5v3.25h-4.5a.75.75 0 01-.75-.75v-9.5zm6.75 10.06v-3.06c0-.552.448-1 1-1h3.06l-4.06 4.06z" />
            <path d="M7.25 6.5a.75.75 0 000 1.5h5.5a.75.75 0 000-1.5h-5.5zM7.25 9.5a.75.75 0 000 1.5h1.5a.75.75 0 000-1.5h-1.5z" />
          </svg>
        </button>
        )}
        {isThread && isDraft ? null : isThread ? (
          <button
            onClick={() => {
              setArchiveError("");
              setShowArchiveDialog(true);
            }}
            className="rounded-[10px] p-2 text-ink-faint transition-colors hover:text-lamp-err"
            title="Archive thread"
            aria-label="Archive thread"
          >
            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-5 h-5">
              <path d="M2 4.75A.75.75 0 012.75 4h14.5a.75.75 0 01.75.75v1.5a.75.75 0 01-.75.75H2.75A.75.75 0 012 6.25v-1.5z" />
              <path fillRule="evenodd" d="M3.5 8.5h13V15a1.5 1.5 0 01-1.5 1.5H5A1.5 1.5 0 013.5 15V8.5zM8 11a.75.75 0 000 1.5h4a.75.75 0 000-1.5H8z" clipRule="evenodd" />
            </svg>
          </button>
        ) : (
          <button
            onClick={() => {
              setDeleteNotify(false);
              setDeleteError("");
              setShowDeleteDialog(true);
            }}
            className="rounded-[10px] p-2 text-ink-faint transition-colors hover:text-lamp-err"
            title="Delete group"
          >
            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-5 h-5">
              <path fillRule="evenodd" d="M8.75 1A2.75 2.75 0 006 3.75v.443c-.795.077-1.584.176-2.365.298a.75.75 0 10.23 1.482l.149-.022.841 10.518A2.75 2.75 0 007.596 19h4.807a2.75 2.75 0 002.742-2.53l.841-10.519.149.023a.75.75 0 00.23-1.482A41.03 41.03 0 0014 4.193V3.75A2.75 2.75 0 0011.25 1h-2.5zM10 4c.84 0 1.673.025 2.5.075V3.75c0-.69-.56-1.25-1.25-1.25h-2.5c-.69 0-1.25.56-1.25 1.25v.325C8.327 4.025 9.16 4 10 4zM8.58 7.72a.75.75 0 00-1.5.06l.3 7.5a.75.75 0 101.5-.06l-.3-7.5zm4.34.06a.75.75 0 10-1.5-.06l-.3 7.5a.75.75 0 101.5.06l.3-7.5z" clipRule="evenodd" />
            </svg>
          </button>
        )}
      </header>

      {/* Messages */}
      <div ref={scrollContainerRef} className="flex-1 overflow-y-auto">
        <div className="mx-auto max-w-[760px] space-y-4 px-4 py-4">
        {hasMore && <LoadMoreButton onClick={loadOlderMessages} loading={loadingMoreRef.current} />}

        {messages.length === 0 && (
          <div className="py-16 text-center text-ink-faint">
            <p className="mb-1 text-lg text-ink-dim">{group.name}</p>
            <p className="text-sm">No messages yet</p>
          </div>
        )}

        {messages.map((msg) => {
          const isUser = msg.agentId === USER_SENDER_ID;
          if (isUser) {
            return (
              <div key={msg.id} className="flex justify-end">
                <div className="min-w-0 max-w-[85%] rounded-2xl rounded-br-md border border-copper/25 bg-raised px-3.5 py-2.5 text-[14px] text-ink lg:max-w-[70%]">
                  {msg.attachments && msg.attachments.length > 0 && (
                    <AttachmentList attachments={msg.attachments} isUser />
                  )}
                  <MessageContent
                    messageId={msg.id}
                    content={msg.content}
                    isUser
                    timestamp={msg.timestamp}
                  />
                </div>
              </div>
            );
          }
          // Subtle highlight when the agent @mentions the human operator.
          const mentionsUser = msg.mentions?.includes(USER_SENDER_ID) ?? false;
          return (
            <div
              key={msg.id}
              className={`flex gap-3${mentionsUser ? " -mx-2 rounded-xl border-l-2 border-copper/60 bg-copper/5 px-2 py-1.5" : ""}`}
            >
              <AgentAvatar
                agentId={msg.agentId}
                name={msg.agentName}
                size="xs"
                className="mt-0.5 shrink-0"
              />
              <div className="min-w-0 flex-1">
                <div className="mb-1 flex items-baseline gap-2">
                  <span className={`text-[13px] font-semibold ${agentColors.get(msg.agentId) ?? "text-ink"}`}>
                    {msg.agentName}
                  </span>
                  <span className="font-mono text-[11px] text-ink-faint">{formatTime(msg.timestamp)}</span>
                  {mentionsUser && (
                    <span
                      className="rounded-full bg-copper/15 px-1.5 font-mono text-[10px] text-copper"
                      title="Mentions you"
                    >
                      @you
                    </span>
                  )}
                </div>
                {msg.thinking && <ThinkingBlock text={msg.thinking} />}
                {msg.attachments && msg.attachments.length > 0 && (
                  <AttachmentList attachments={msg.attachments} isUser={false} />
                )}
                <div className="text-[14px] text-ink">
                  <MessageContent
                    messageId={msg.id}
                    content={msg.content}
                    isUser={false}
                    timestamp={msg.timestamp}
                    showTime={false}
                  />
                </div>
                {msg.toolUses && msg.toolUses.length > 0 && (
                  <CollapsedToolUses toolUses={msg.toolUses} />
                )}
                {msg.usage && msg.usage.inputTokens != null && (
                  <ThreadUsageLine usage={msg.usage} model={threadAgentModel} />
                )}
              </div>
            </div>
          );
        })}
        {awaitingReply && (
          <div className="flex items-center gap-3" aria-live="polite">
            <div className="flex gap-3">
              {group.members.slice(0, 1).map((m) => (
                <AgentAvatar key={m.agentId} agentId={m.agentId} name={m.agentName} size="xs" className="mt-0.5 shrink-0" />
              ))}
            </div>
            <span className="text-[13px] italic text-ink-faint">replying…</span>
          </div>
        )}
        <div ref={messagesEndRef} />
        </div>
      </div>

      {/* Composer */}
      <div className="sticky bottom-0 z-30 shrink-0 border-t border-hairline bg-app/92 backdrop-blur">
        <div className="mx-auto max-w-[760px] px-4 py-3">
        {sendError && <DismissibleError message={sendError} onDismiss={() => setSendError(null)} />}
        {uploadError && <DismissibleError message={uploadError} onDismiss={() => setUploadError(null)} />}
        <PendingAttachments files={pendingFiles} onRemove={removePendingFile} thumb={false} />
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
            disabled={uploading || sending}
          />
          <div className="min-w-0 flex-1 rounded-xl border border-hairline bg-raised px-1 focus-within:border-copper/50">
            <textarea
              ref={textareaRef}
              value={input}
              onChange={(e) => setInput(e.target.value)}
              onInput={handleTextareaInput}
              onKeyDown={handleKeyDown}
              placeholder={`${isThread ? "Message this thread" : "Message the group"}… (${enterSends ? "Enter" : "Ctrl+Enter"} to send)`}
              rows={1}
              className="max-h-[150px] w-full resize-none bg-transparent px-3 py-2 text-[14px] text-ink placeholder:text-ink-faint focus:outline-none"
            />
          </div>
          <SendButton
            onClick={() => void handleSend()}
            disabled={(!input.trim() && pendingFiles.length === 0) || sending || awaitingReply}
          />
        </div>
        </div>
      </div>

      {/* Clear history confirmation dialog */}
      {showClearDialog && (
        <div
          ref={clearDialogRef}
          tabIndex={-1}
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 outline-none"
          onClick={(e) => { if (e.target === e.currentTarget && !clearing) setShowClearDialog(false); }}
          onKeyDown={(e) => { if (e.key === "Escape" && !clearing) setShowClearDialog(false); }}
        >
          <div className="w-80 rounded-[10px] border border-hairline bg-raised p-5 shadow-xl shadow-black/50">
            <h3 className="mb-2 text-sm font-medium text-ink">
              Clear history?
            </h3>
            <p className="mb-4 text-xs text-ink-dim">
              Messages in &ldquo;{group.name}&rdquo; will be deleted. The group stays open.
            </p>
            {clearError && (
              <p className="mb-3 text-xs text-lamp-err">{clearError}</p>
            )}
            <div className="flex justify-end gap-2">
              <button
                onClick={() => setShowClearDialog(false)}
                disabled={clearing}
                className="rounded-[10px] px-3 py-1.5 text-xs text-ink-dim transition-colors hover:text-ink disabled:opacity-50"
              >
                Cancel
              </button>
              <button
                onClick={async () => {
                  setClearing(true);
                  setClearError("");
                  try {
                    await groupdmApi.clearMessages(group.id);
                    // The stored last-read id points at a deleted message;
                    // drop it so unread counting restarts cleanly.
                    clearLastRead(group.id);
                    pollGenerationRef.current += 1;
                    setMessages([]);
                    setHasMore(false);
                    suppressAutoScrollRef.current = false;
                    setShowClearDialog(false);
                  } catch (e) {
                    setClearError(e instanceof Error ? e.message : "Failed to clear history");
                  } finally {
                    setClearing(false);
                  }
                }}
                disabled={clearing}
                className="rounded-[10px] border border-lamp-warn/50 bg-lamp-warn/10 px-3 py-1.5 text-xs text-lamp-warn transition-colors hover:bg-lamp-warn/20 disabled:opacity-50"
              >
                {clearing ? "Clearing…" : "Clear"}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Delete confirmation dialog */}
      {showDeleteDialog && (
        <div
          ref={deleteDialogRef}
          tabIndex={-1}
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 outline-none"
          onClick={(e) => { if (e.target === e.currentTarget && !deleting) setShowDeleteDialog(false); }}
          onKeyDown={(e) => { if (e.key === "Escape" && !deleting) setShowDeleteDialog(false); }}
        >
          <div className="w-80 rounded-[10px] border border-hairline bg-raised p-5 shadow-xl shadow-black/50">
            <h3 className="mb-3 text-sm font-medium text-ink">
              Delete &ldquo;{group.name}&rdquo;?
            </h3>
            <label className="mb-4 flex cursor-pointer select-none items-center gap-2 text-sm text-ink-dim">
              <input
                type="checkbox"
                checked={deleteNotify}
                onChange={(e) => setDeleteNotify(e.target.checked)}
                disabled={deleting}
                className="rounded border-hairline bg-surface accent-[color:var(--color-copper)]"
              />
              Notify members
            </label>
            {deleteError && (
              <p className="mb-3 text-xs text-lamp-err">{deleteError}</p>
            )}
            <div className="flex justify-end gap-2">
              <button
                onClick={() => setShowDeleteDialog(false)}
                disabled={deleting}
                className="rounded-[10px] px-3 py-1.5 text-xs text-ink-dim transition-colors hover:text-ink disabled:opacity-50"
              >
                Cancel
              </button>
              <button
                onClick={async () => {
                  setDeleting(true);
                  setDeleteError("");
                  try {
                    await groupdmApi.delete(group.id, deleteNotify);
                    navigate("/");
                  } catch (e) {
                    setDeleteError(e instanceof Error ? e.message : "Failed to delete");
                  } finally {
                    setDeleting(false);
                  }
                }}
                disabled={deleting}
                className="rounded-[10px] border border-lamp-err/50 bg-lamp-err/10 px-3 py-1.5 text-xs text-lamp-err transition-colors hover:bg-lamp-err/20 disabled:opacity-50"
              >
                {deleting ? "Deleting…" : "Delete"}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Archive confirmation dialog (thread rooms) */}
      {showArchiveDialog && (
        <div
          ref={archiveDialogRef}
          tabIndex={-1}
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 outline-none"
          onClick={(e) => { if (e.target === e.currentTarget && !archiving) setShowArchiveDialog(false); }}
          onKeyDown={(e) => { if (e.key === "Escape" && !archiving) setShowArchiveDialog(false); }}
        >
          <div className="w-80 rounded-[10px] border border-hairline bg-raised p-5 shadow-xl shadow-black/50">
            <h3 className="mb-2 text-sm font-medium text-ink">
              Archive &ldquo;{group.name}&rdquo;?
            </h3>
            <p className="mb-4 text-xs text-ink-dim">
              This permanently closes the thread. It cannot be restored.
            </p>
            {archiveError && (
              <p className="mb-3 text-xs text-lamp-err">{archiveError}</p>
            )}
            <div className="flex justify-end gap-2">
              <button
                onClick={() => setShowArchiveDialog(false)}
                disabled={archiving}
                className="rounded-[10px] px-3 py-1.5 text-xs text-ink-dim transition-colors hover:text-ink disabled:opacity-50"
              >
                Cancel
              </button>
              <button
                onClick={async () => {
                  setArchiving(true);
                  setArchiveError("");
                  try {
                    await groupdmApi.archive(group.id);
                    navigate("/");
                  } catch (e) {
                    setArchiveError(e instanceof Error ? e.message : "Failed to archive");
                  } finally {
                    setArchiving(false);
                  }
                }}
                disabled={archiving}
                className="rounded-[10px] border border-lamp-err/50 bg-lamp-err/10 px-3 py-1.5 text-xs text-lamp-err transition-colors hover:bg-lamp-err/20 disabled:opacity-50"
              >
                {archiving ? "Archiving…" : "Archive"}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

// ThreadUsageLine renders per-reply token counts (input→output, cache read/
// write when present) and an approximate USD cost when the thread agent's
// model is priced. Mirrors ChatMessage's UsageLine (muted mono style).
function ThreadUsageLine({
  usage,
  model,
}: {
  usage: NonNullable<GroupMessage["usage"]>;
  model?: string;
}) {
  const cacheRead = usage.cacheReadInputTokens ?? 0;
  const cacheWrite = usage.cacheCreationInputTokens ?? 0;
  const cost = estimateTurnCost(model, usage);
  return (
    <div className="mt-1 font-mono text-[11px] text-ink-faint">
      {usage.inputTokens.toLocaleString()}&rarr;{usage.outputTokens.toLocaleString()} tokens
      {(cacheRead > 0 || cacheWrite > 0) && (
        <>
          {" "}
          (cache {cacheRead.toLocaleString()}r/{cacheWrite.toLocaleString()}w)
        </>
      )}
      {cost != null && <> &middot; &asymp;${cost.toFixed(4)}</>}
    </div>
  );
}

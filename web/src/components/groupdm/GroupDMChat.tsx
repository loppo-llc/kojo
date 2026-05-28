import { useEffect, useRef, useState, useCallback } from "react";
import { useParams, useNavigate } from "react-router";
import {
  groupdmApi,
  USER_SENDER_ID,
  type GroupDMInfo,
  type GroupDMStyle,
  type GroupDMVenue,
  DEFAULT_GROUPDM_VENUE,
  type GroupMessage,
} from "../../lib/groupdmApi";
import { api } from "../../lib/api";
import type { AgentMessageAttachment } from "../../lib/agentApi";
import { useEnterSends } from "../../lib/preferences";
import { AgentAvatar } from "../agent/AgentAvatar";
import { AttachmentList, MessageContent } from "../agent/ChatMessage";

const PAGE_SIZE = 50;

export function GroupDMChat() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [group, setGroup] = useState<GroupDMInfo | null>(null);
  const [messages, setMessages] = useState<GroupMessage[]>([]);
  const [hasMore, setHasMore] = useState(false);
  const messagesEndRef = useRef<HTMLDivElement>(null);
  const scrollContainerRef = useRef<HTMLDivElement>(null);
  const loadingMoreRef = useRef(false);
  const suppressAutoScrollRef = useRef(false);

  const [notFound, setNotFound] = useState(false);
  const [showStyleMenu, setShowStyleMenu] = useState(false);
  const [showVenueMenu, setShowVenueMenu] = useState(false);
  const [editingCooldown, setEditingCooldown] = useState(false);
  const [cooldownInput, setCooldownInput] = useState("");
  const [showDeleteDialog, setShowDeleteDialog] = useState(false);
  const [deleteNotify, setDeleteNotify] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [deleteError, setDeleteError] = useState("");
  const deleteDialogRef = useRef<HTMLDivElement>(null);

  // User input
  const [input, setInput] = useState(() =>
    id ? sessionStorage.getItem(`groupdm-draft:${id}`) ?? "" : "",
  );
  const [sending, setSending] = useState(false);
  const [sendError, setSendError] = useState<string | null>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const [enterSends] = useEnterSends();

  // File attachments — uploaded to /api/v1/blobs but not yet attached to a
  // posted message. Cleared on successful send.
  const [pendingFiles, setPendingFiles] = useState<AgentMessageAttachment[]>([]);
  const [uploading, setUploading] = useState(false);
  const [uploadError, setUploadError] = useState<string | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  // Restore draft / reset textarea height when id changes
  useEffect(() => {
    if (!id) {
      setInput("");
      return;
    }
    const draft = sessionStorage.getItem(`groupdm-draft:${id}`) ?? "";
    setInput(draft);
    setSendError(null);
    requestAnimationFrame(() => {
      if (textareaRef.current) {
        textareaRef.current.style.height = "auto";
        textareaRef.current.style.height =
          Math.min(textareaRef.current.scrollHeight, 150) + "px";
      }
    });
  }, [id]);

  // Focus delete dialog overlay for Escape key
  useEffect(() => {
    if (showDeleteDialog) deleteDialogRef.current?.focus();
  }, [showDeleteDialog]);

  // Reset state when id changes
  useEffect(() => {
    setGroup(null);
    setMessages([]);
    setHasMore(false);
    setNotFound(false);
    setShowDeleteDialog(false);
    setDeleteNotify(false);
    setDeleteError("");
    setPendingFiles([]);
    setUploadError(null);
  }, [id]);

  // Load group info
  useEffect(() => {
    if (!id) return;
    groupdmApi.get(id).then(setGroup).catch((e) => {
      if (e instanceof Error && e.message.startsWith("404")) {
        setNotFound(true);
      }
    });
  }, [id]);

  // Poll for new messages — merge with already-loaded older messages
  // Only set hasMore on initial load; polling preserves older pages
  const initialLoadDone = useRef(false);
  useEffect(() => {
    if (!id || notFound) return;
    initialLoadDone.current = false;
    const load = () =>
      groupdmApi.messages(id, PAGE_SIZE).then((r) => {
        setMessages((prev) => {
          const newIds = new Set(r.messages.map((m) => m.id));
          const older = prev.filter((m) => !newIds.has(m.id));
          return [...older, ...r.messages];
        });
        if (!initialLoadDone.current) {
          setHasMore(r.hasMore);
          initialLoadDone.current = true;
        }
      }).catch(console.error);
    load();
    const interval = setInterval(load, 3000);
    return () => clearInterval(interval);
  }, [id, notFound]);

  // Auto-scroll only when new messages arrive at the bottom
  const lastMessageIdRef = useRef<string | undefined>(undefined);
  useEffect(() => {
    const lastId = messages[messages.length - 1]?.id;
    if (lastId && lastId !== lastMessageIdRef.current && !suppressAutoScrollRef.current) {
      messagesEndRef.current?.scrollIntoView({ behavior: "smooth" });
    }
    lastMessageIdRef.current = lastId;
  }, [messages]);

  const loadOlderMessages = useCallback(async () => {
    if (!id || loadingMoreRef.current || !hasMore || messages.length === 0) return;
    loadingMoreRef.current = true;

    const oldestId = messages[0].id;
    const container = scrollContainerRef.current;
    const prevScrollHeight = container?.scrollHeight ?? 0;
    const prevScrollTop = container?.scrollTop ?? 0;

    try {
      const r = await groupdmApi.messages(id, PAGE_SIZE, oldestId);
      setHasMore(r.hasMore);
      if (r.messages.length > 0) {
        suppressAutoScrollRef.current = true;
        setMessages((prev) => [...r.messages, ...prev]);
        requestAnimationFrame(() => {
          if (container) {
            const delta = container.scrollHeight - prevScrollHeight;
            container.scrollTop = prevScrollTop + delta;
          }
          suppressAutoScrollRef.current = false;
        });
      }
    } catch (e) {
      console.error(e);
    } finally {
      loadingMoreRef.current = false;
    }
  }, [id, hasMore, messages]);

  const handleFileSelect = useCallback(async (e: React.ChangeEvent<HTMLInputElement>) => {
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
  }, []);

  const removePendingFile = useCallback((index: number) => {
    setPendingFiles((prev) => prev.filter((_, i) => i !== index));
  }, []);

  const handleSend = useCallback(async () => {
    const text = input.trim();
    if ((!text && pendingFiles.length === 0) || sending || !id) return;
    setSending(true);
    setSendError(null);
    try {
      const sent = await groupdmApi.postUserMessage(
        id,
        text,
        pendingFiles.length > 0 ? pendingFiles : undefined,
      );
      // Clear input + draft on success
      setInput("");
      setPendingFiles([]);
      setUploadError(null);
      sessionStorage.removeItem(`groupdm-draft:${id}`);
      if (textareaRef.current) {
        textareaRef.current.style.height = "auto";
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
  }, [id, input, pendingFiles, sending]);

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
      if (e.key === "Enter" && !e.nativeEvent.isComposing) {
        if (enterSends ? !e.shiftKey : e.shiftKey) {
          e.preventDefault();
          void handleSend();
        }
      }
    },
    [enterSends, handleSend],
  );

  const handleTextareaInput = useCallback(() => {
    if (textareaRef.current) {
      textareaRef.current.style.height = "auto";
      textareaRef.current.style.height =
        Math.min(textareaRef.current.scrollHeight, 150) + "px";
    }
  }, []);

  // Conditional renders MUST live below every hook above. Moving them up
  // would change the hook count between renders (notFound flips after the
  // initial groupdmApi.get failure) and React would throw.
  if (notFound) {
    return (
      <div className="min-h-full bg-neutral-950 text-neutral-200 flex flex-col items-center justify-center gap-3">
        <p className="text-neutral-500">Group not found</p>
        <button
          onClick={() => navigate("/")}
          className="px-4 py-2 bg-neutral-800 hover:bg-neutral-700 rounded text-sm"
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
    <div className="flex flex-col h-full bg-neutral-950 text-neutral-200">
      {/* Header */}
      <header className="flex items-center gap-3 px-4 py-3 border-b border-neutral-800 shrink-0">
        <button
          onClick={() => navigate("/")}
          className="text-neutral-400 hover:text-neutral-200"
        >
          &larr;
        </button>
        <div className="flex -space-x-2">
          {group.members.slice(0, 3).map((m) => (
            <AgentAvatar key={m.agentId} agentId={m.agentId} name={m.agentName} size="sm" />
          ))}
        </div>
        <div className="flex-1 min-w-0">
          <div className="font-medium text-sm truncate">{group.name}</div>
          <div className="text-xs text-neutral-500 truncate">
            {group.members.length <= 3
              ? group.members.map((m) => m.agentName).join(", ")
              : group.members.slice(0, 2).map((m) => m.agentName).join(", ") + `, +${group.members.length - 2}`}
          </div>
        </div>
        {/* Style selector */}
        <div className="shrink-0 relative">
          <button
            onClick={() => setShowStyleMenu((v) => !v)}
            className="text-base leading-none px-1 py-0.5 rounded hover:bg-neutral-700 cursor-pointer"
            title={`Style: ${group.style || "efficient"}`}
          >
            {(group.style || "efficient") === "efficient" ? "⚡" : "💬"}
          </button>
          {showStyleMenu && (
            <>
              <div className="fixed inset-0 z-10" onClick={() => setShowStyleMenu(false)} />
              <div className="absolute right-0 top-full mt-1 z-20 bg-neutral-800 border border-neutral-700 rounded shadow-lg py-1 min-w-[140px]">
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
                    className={`w-full text-left px-3 py-1.5 text-xs hover:bg-neutral-700 cursor-pointer ${
                      value === (group.style || "efficient") ? "text-white" : "text-neutral-400"
                    }`}
                  >
                    {label}
                  </button>
                ))}
              </div>
            </>
          )}
        </div>
        {/* Venue selector — physical-setting hint that calibrates how members
            should speak (text-only chatroom vs. co-located in-person). */}
        <div className="shrink-0 relative">
          <button
            onClick={() => setShowVenueMenu((v) => !v)}
            className="text-base leading-none px-1 py-0.5 rounded hover:bg-neutral-700 cursor-pointer"
            title={`Venue: ${group.venue || DEFAULT_GROUPDM_VENUE}`}
          >
            {(group.venue || DEFAULT_GROUPDM_VENUE) === "colocated" ? "🏠" : "💻"}
          </button>
          {showVenueMenu && (
            <>
              <div className="fixed inset-0 z-10" onClick={() => setShowVenueMenu(false)} />
              <div className="absolute right-0 top-full mt-1 z-20 bg-neutral-800 border border-neutral-700 rounded shadow-lg py-1 min-w-[200px]">
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
                    className={`w-full text-left px-3 py-1.5 text-xs hover:bg-neutral-700 cursor-pointer ${
                      value === (group.venue || DEFAULT_GROUPDM_VENUE) ? "text-white" : "text-neutral-400"
                    }`}
                  >
                    <div>{label}</div>
                    <div className="text-[10px] text-neutral-500">{hint}</div>
                  </button>
                ))}
              </div>
            </>
          )}
        </div>
        {/* Cooldown setting */}
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
                className="w-16 px-1.5 py-0.5 text-xs bg-neutral-800 border border-neutral-700 rounded text-neutral-200 text-center"
                autoFocus
                onBlur={() => setEditingCooldown(false)}
              />
              <span className="text-[10px] text-neutral-500">s</span>
            </form>
          ) : (
            <button
              onClick={() => {
                setCooldownInput(String(group.cooldown || 50));
                setEditingCooldown(true);
              }}
              className="text-[10px] text-neutral-600 hover:text-neutral-400 px-1.5 py-0.5 rounded"
              title="Notification cooldown (seconds)"
            >
              {group.cooldown || 50}s
            </button>
          )}
        </div>
        <button
          onClick={() => {
            setDeleteNotify(false);
            setDeleteError("");
            setShowDeleteDialog(true);
          }}
          className="p-2 text-neutral-600 hover:text-red-400 rounded"
          title="Delete group"
        >
          <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-5 h-5">
            <path fillRule="evenodd" d="M8.75 1A2.75 2.75 0 006 3.75v.443c-.795.077-1.584.176-2.365.298a.75.75 0 10.23 1.482l.149-.022.841 10.518A2.75 2.75 0 007.596 19h4.807a2.75 2.75 0 002.742-2.53l.841-10.519.149.023a.75.75 0 00.23-1.482A41.03 41.03 0 0014 4.193V3.75A2.75 2.75 0 0011.25 1h-2.5zM10 4c.84 0 1.673.025 2.5.075V3.75c0-.69-.56-1.25-1.25-1.25h-2.5c-.69 0-1.25.56-1.25 1.25v.325C8.327 4.025 9.16 4 10 4zM8.58 7.72a.75.75 0 00-1.5.06l.3 7.5a.75.75 0 101.5-.06l-.3-7.5zm4.34.06a.75.75 0 10-1.5-.06l-.3 7.5a.75.75 0 101.5.06l.3-7.5z" clipRule="evenodd" />
          </svg>
        </button>
      </header>

      {/* Messages */}
      <div ref={scrollContainerRef} className="flex-1 overflow-y-auto px-4 py-4 space-y-3">
        {hasMore && (
          <div className="flex justify-center pt-1 pb-3">
            <button
              onClick={loadOlderMessages}
              className="group relative px-4 py-1.5 text-xs text-neutral-500 hover:text-neutral-300 transition-colors"
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

        {messages.length === 0 && (
          <div className="text-center text-neutral-600 py-16">
            <p className="text-lg mb-1">{group.name}</p>
            <p className="text-sm">No messages yet</p>
          </div>
        )}

        {messages.map((msg) => {
          const isUser = msg.agentId === USER_SENDER_ID;
          return (
            <div key={msg.id} className={`flex gap-3 ${isUser ? "flex-row-reverse" : "flex-row"}`}>
              {!isUser && (
                <AgentAvatar
                  agentId={msg.agentId}
                  name={msg.agentName}
                  size="sm"
                  className="mt-1 shrink-0"
                />
              )}
              <div
                className={`max-w-[80%] min-w-0 px-3.5 py-2.5 ${
                  isUser
                    ? "bg-blue-600/90 text-white rounded-2xl rounded-tr-sm"
                    : "bg-neutral-800/80 text-neutral-200 rounded-2xl rounded-tl-sm"
                }`}
              >
                {!isUser && (
                  <div
                    className={`mb-1 text-[11px] font-medium ${
                      agentColors.get(msg.agentId) ?? "text-neutral-300"
                    }`}
                  >
                    {msg.agentName}
                  </div>
                )}
                {msg.attachments && msg.attachments.length > 0 && (
                  <AttachmentList attachments={msg.attachments} isUser={isUser} />
                )}
                <MessageContent
                  messageId={msg.id}
                  content={msg.content}
                  isUser={isUser}
                  timestamp={msg.timestamp}
                />
              </div>
            </div>
          );
        })}
        <div ref={messagesEndRef} />
      </div>

      {/* Input */}
      <div className="border-t border-neutral-800 px-4 py-3 shrink-0">
        {sendError && (
          <div className="flex items-center gap-2 mb-2 px-3 py-1.5 bg-red-950/50 border border-red-900/50 rounded-lg text-xs text-red-300">
            <span className="flex-1">{sendError}</span>
            <button
              onClick={() => setSendError(null)}
              className="text-red-400 hover:text-red-200"
            >
              <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" className="w-3 h-3">
                <path d="M5.28 4.22a.75.75 0 00-1.06 1.06L6.94 8l-2.72 2.72a.75.75 0 101.06 1.06L8 9.06l2.72 2.72a.75.75 0 101.06-1.06L9.06 8l2.72-2.72a.75.75 0 00-1.06-1.06L8 6.94 5.28 4.22z" />
              </svg>
            </button>
          </div>
        )}
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
            disabled={uploading || sending}
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
              if (id) sessionStorage.setItem(`groupdm-draft:${id}`, e.target.value);
            }}
            onInput={handleTextareaInput}
            onKeyDown={handleKeyDown}
            placeholder="Message the group..."
            rows={1}
            className="flex-1 px-3 py-2 bg-neutral-900 border border-neutral-700 rounded-xl text-sm resize-none focus:outline-none focus:border-neutral-500 max-h-[150px]"
          />
          <button
            onClick={() => void handleSend()}
            disabled={(!input.trim() && pendingFiles.length === 0) || sending}
            className="px-4 py-2 bg-blue-600 hover:bg-blue-500 rounded-xl text-sm font-medium disabled:opacity-40 shrink-0"
          >
            {sending ? "Sending…" : "Send"}
          </button>
        </div>
        <div className="text-[10px] text-neutral-600 mt-1 text-center">
          {enterSends ? "Enter to send, Shift+Enter for newline" : "Shift+Enter to send, Enter for newline"}
        </div>
      </div>

      {/* Delete confirmation dialog */}
      {showDeleteDialog && (
        <div
          ref={deleteDialogRef}
          tabIndex={-1}
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 outline-none"
          onClick={(e) => { if (e.target === e.currentTarget && !deleting) setShowDeleteDialog(false); }}
          onKeyDown={(e) => { if (e.key === "Escape" && !deleting) setShowDeleteDialog(false); }}
        >
          <div className="bg-neutral-900 border border-neutral-700 rounded-lg p-5 w-80 shadow-xl">
            <h3 className="text-sm font-medium text-neutral-200 mb-3">
              Delete &ldquo;{group.name}&rdquo;?
            </h3>
            <label className="flex items-center gap-2 text-sm text-neutral-400 mb-4 cursor-pointer select-none">
              <input
                type="checkbox"
                checked={deleteNotify}
                onChange={(e) => setDeleteNotify(e.target.checked)}
                disabled={deleting}
                className="rounded border-neutral-600 bg-neutral-800 text-blue-500 focus:ring-blue-500/30"
              />
              Notify members
            </label>
            {deleteError && (
              <p className="text-xs text-red-400 mb-3">{deleteError}</p>
            )}
            <div className="flex justify-end gap-2">
              <button
                onClick={() => setShowDeleteDialog(false)}
                disabled={deleting}
                className="px-3 py-1.5 text-xs text-neutral-400 hover:text-neutral-200 rounded disabled:opacity-50"
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
                className="px-3 py-1.5 text-xs bg-red-600 hover:bg-red-500 text-white rounded disabled:opacity-50"
              >
                {deleting ? "Deleting…" : "Delete"}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

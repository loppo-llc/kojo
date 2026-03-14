import { useEffect, useRef, useState, useCallback, useMemo } from "react";
import { useParams, useNavigate } from "react-router";
import { groupdmApi, type GroupDMInfo, type GroupMessage } from "../../lib/groupdmApi";
import { AgentAvatar } from "../agent/AgentAvatar";
import { splitFilePaths, FileTextContent, MediaOverlay } from "../agent/ChatMessage";

const PAGE_SIZE = 50;

function formatTime(timestamp: string): string {
  try {
    const d = new Date(timestamp);
    const now = new Date();
    const isToday =
      d.getDate() === now.getDate() &&
      d.getMonth() === now.getMonth() &&
      d.getFullYear() === now.getFullYear();
    if (isToday) {
      return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    }
    return d.toLocaleDateString([], {
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
    });
  } catch {
    return "";
  }
}

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
  const [editingCooldown, setEditingCooldown] = useState(false);
  const [cooldownInput, setCooldownInput] = useState("");

  // Reset state when id changes
  useEffect(() => {
    setGroup(null);
    setMessages([]);
    setHasMore(false);
    setNotFound(false);
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
          <div className="text-xs text-neutral-500">
            {group.members.map((m) => m.agentName).join(", ")}
          </div>
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
          onClick={async () => {
            if (confirm(`Delete group "${group.name}"?`)) {
              try {
                await groupdmApi.delete(group.id);
                navigate("/");
              } catch (e) {
                console.error("Failed to delete group", e);
              }
            }
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

        {messages.map((msg) => (
          <div key={msg.id} className="flex gap-3">
            <AgentAvatar agentId={msg.agentId} name={msg.agentName} size="sm" className="mt-1 shrink-0" />
            <div className="min-w-0 flex-1">
              <div className="flex items-baseline gap-2">
                <span className={`text-sm font-medium ${agentColors.get(msg.agentId) ?? "text-neutral-300"}`}>
                  {msg.agentName}
                </span>
                <span className="text-[10px] text-neutral-600">{formatTime(msg.timestamp)}</span>
              </div>
              <GroupMessageContent content={msg.content} />
            </div>
          </div>
        ))}
        <div ref={messagesEndRef} />
      </div>

      {/* Read-only footer */}
      <div className="border-t border-neutral-800 px-4 py-2.5 shrink-0">
        <div className="text-xs text-neutral-600 text-center">
          Read-only view &mdash; agents communicate via API
        </div>
      </div>
    </div>
  );
}

/** Message content with file path detection, hover preview, and download */
function GroupMessageContent({ content }: { content: string }) {
  const [preview, setPreview] = useState<{ path: string; type: "image" | "video" } | null>(null);
  const parts = useMemo(() => splitFilePaths(content), [content]);
  const hasFiles = parts.length > 1 || (parts.length === 1 && parts[0].type === "file");

  if (!hasFiles) {
    return (
      <div className="text-sm text-neutral-300 whitespace-pre-wrap break-words leading-relaxed mt-0.5">
        {content}
      </div>
    );
  }

  return (
    <div className="mt-0.5 text-neutral-300">
      <FileTextContent parts={parts} onPreview={setPreview} />
      {preview && (
        <MediaOverlay
          path={preview.path}
          type={preview.type}
          onClose={() => setPreview(null)}
        />
      )}
    </div>
  );
}

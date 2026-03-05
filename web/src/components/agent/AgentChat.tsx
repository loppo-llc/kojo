import { useEffect, useRef, useState, useCallback } from "react";
import { useParams, useNavigate } from "react-router";
import { agentApi, type AgentInfo, type AgentMessage, type ChatEvent } from "../../lib/agentApi";
import { useAgentWebSocket } from "../../hooks/useAgentWebSocket";
import { ChatMessage, StreamingMessage } from "./ChatMessage";
import { AgentAvatar } from "./AgentAvatar";

export function AgentChat() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [agent, setAgent] = useState<AgentInfo | null>(null);
  const [messages, setMessages] = useState<AgentMessage[]>([]);
  const [input, setInput] = useState("");
  const [streaming, setStreaming] = useState(false);
  const [streamText, setStreamText] = useState("");
  const [streamTools, setStreamTools] = useState<Array<{ name: string; input: string; output: string }>>([]);
  const [streamStatus, setStreamStatus] = useState("");
  const messagesEndRef = useRef<HTMLDivElement>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);

  // Load agent and messages
  useEffect(() => {
    if (!id) return;
    agentApi.get(id).then(setAgent).catch(() => navigate("/agents"));
    agentApi.messages(id, 100).then(setMessages).catch(console.error);
  }, [id, navigate]);

  const scrollToBottom = useCallback(() => {
    messagesEndRef.current?.scrollIntoView({ behavior: "smooth" });
  }, []);

  useEffect(() => {
    scrollToBottom();
  }, [messages, scrollToBottom]);

  const resetStream = useCallback(() => {
    setStreaming(false);
    setStreamText("");
    setStreamTools([]);
    setStreamStatus("");
  }, []);

  const onEvent = useCallback(
    (event: ChatEvent) => {
      switch (event.type) {
        case "status":
          setStreamStatus(event.status ?? "");
          if (event.status === "thinking") {
            setStreaming(true);
          }
          break;
        case "text":
          setStreamText((prev) => prev + (event.delta ?? ""));
          break;
        case "tool_use":
          if (event.toolName) {
            setStreamTools((prev) => [
              ...prev,
              { name: event.toolName!, input: event.toolInput ?? "", output: "" },
            ]);
          }
          break;
        case "tool_result":
          setStreamTools((prev) => {
            const copy = [...prev];
            for (let i = copy.length - 1; i >= 0; i--) {
              if (copy[i].name === event.toolName && !copy[i].output) {
                copy[i] = { ...copy[i], output: event.toolOutput ?? "" };
                break;
              }
            }
            return copy;
          });
          break;
        case "done":
          if (event.message) {
            // Deduplicate by message ID (bgDone may overlap with initial load)
            setMessages((prev) =>
              prev.some((m) => m.id === event.message!.id)
                ? prev
                : [...prev, event.message!],
            );
          } else if (id) {
            // Background chat finished — reload messages from transcript
            agentApi.messages(id, 100).then(setMessages).catch(console.error);
          }
          resetStream();
          break;
        case "error": {
          const errorMsg = event.errorMessage || "An error occurred";
          setMessages((prev) => [
            ...prev,
            {
              id: "error_" + Date.now(),
              role: "system",
              content: `⚠️ Error: ${errorMsg}`,
              timestamp: new Date().toISOString(),
            },
          ]);
          resetStream();
          break;
        }
      }
    },
    [id, resetStream],
  );

  const onDisconnect = useCallback(() => {
    resetStream();
  }, [resetStream]);

  const { connected, sendMessage, abort } = useAgentWebSocket({
    agentId: id!,
    onEvent,
    onDisconnect,
  });

  const handleSend = () => {
    const text = input.trim();
    if (!text || streaming || !connected) return;

    // Add user message immediately
    const userMsg: AgentMessage = {
      id: "pending_" + Date.now(),
      role: "user",
      content: text,
      timestamp: new Date().toISOString(),
    };
    setMessages((prev) => [...prev, userMsg]);
    setInput("");
    setStreaming(true);
    setStreamText("");
    setStreamTools([]);
    setStreamStatus("thinking");
    sendMessage(text);

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
        <AgentAvatar agentId={agent.id} name={agent.name} size="md" />
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
      <div className="flex-1 overflow-y-auto px-4 py-4 space-y-3">
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
          />
        ))}
        {streaming && (
          <StreamingMessage
            text={streamText}
            toolUses={streamTools}
            agentName={agent.name}
            agentId={agent.id}
            status={streamStatus}
          />
        )}
        <div ref={messagesEndRef} />
      </div>

      {/* Input */}
      <div className="border-t border-neutral-800 px-4 py-3 shrink-0">
        <div className="flex items-end gap-2">
          <textarea
            ref={textareaRef}
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onInput={handleTextareaInput}
            onKeyDown={handleKeyDown}
            placeholder="Message..."
            rows={1}
            className="flex-1 px-3 py-2 bg-neutral-900 border border-neutral-700 rounded-xl text-sm resize-none focus:outline-none focus:border-neutral-500 max-h-[150px]"
          />
          {streaming ? (
            <button
              onClick={abort}
              className="px-4 py-2 bg-red-600 hover:bg-red-500 rounded-xl text-sm font-medium shrink-0"
            >
              Stop
            </button>
          ) : (
            <button
              onClick={handleSend}
              disabled={!input.trim() || !connected}
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

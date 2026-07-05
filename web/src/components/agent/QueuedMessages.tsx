import type { QueuedAgentMessage } from "../../lib/agentApi";

const SNIPPET_LEN = 80;

function snippet(content: string): string {
  return content.length > SNIPPET_LEN ? content.slice(0, SNIPPET_LEN) + "…" : content;
}

// createdAt is Unix milliseconds (store.NowMillis).
function enqueuedAt(createdAt: number): string {
  const t = new Date(createdAt);
  return isNaN(t.getTime()) ? "" : t.toLocaleTimeString();
}

// QueuedMessages renders the offline-holder queue panel: one row per
// queued message with a cancel button. Rendered by AgentChat above the
// composer while the holder device is offline or while queued rows
// remain. Presentational — fetching/cancel wiring lives in the parent
// so this stays trivially testable.
export function QueuedMessages({
  messages,
  holderPeerName,
  onCancel,
}: {
  messages: QueuedAgentMessage[];
  holderPeerName: string;
  onCancel: (qid: string) => void;
}) {
  if (messages.length === 0) return null;
  return (
    <div
      className="mb-2 rounded-[10px] border border-lamp-warn/40 bg-lamp-warn/10 px-3 py-2 text-xs text-lamp-warn"
      data-testid="queued-messages"
    >
      <div className="mb-1 font-medium">
        {messages.length === 1
          ? "1 message queued"
          : `${messages.length} messages queued`}
        {" — will deliver when device "}
        <span className="font-mono">{holderPeerName}</span>
        {" reconnects"}
      </div>
      <ul className="space-y-1">
        {messages.map((m) => (
          <li key={m.id} className="flex items-center gap-2">
            <span className="min-w-0 flex-1 truncate" title={m.content}>
              {snippet(m.content)}
            </span>
            <span className="shrink-0 tabular-nums opacity-70">
              {enqueuedAt(m.createdAt)}
            </span>
            <button
              onClick={() => onCancel(m.id)}
              className="shrink-0 rounded-md border border-lamp-warn/40 px-1.5 py-0.5 transition-colors hover:bg-lamp-warn/20"
              aria-label={`Cancel queued message ${m.id}`}
            >
              Cancel
            </button>
          </li>
        ))}
      </ul>
    </div>
  );
}

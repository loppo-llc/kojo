import type { AgentMessage, ChatEvent } from "../../lib/agentApi";

/**
 * Pure helpers extracted from AgentChat's onEvent so the trickier
 * branches (tool_result matching, done/abort handoff, system-error
 * dedupe) can be unit-tested without spinning up React state.
 *
 * The full onEvent callback still lives in AgentChat.tsx — these
 * helpers exist so its bodies become one-liners. They MUST stay
 * pure: no setState, no refs, no Date.now() without an injected
 * clock.
 */

/** Streaming tool block as held in AgentChat's streamTools state. */
export type StreamingTool = {
  id: string;
  name: string;
  input: string;
  output: string | null;
};

/**
 * Tool-match predicate used by `tool_result` events: prefer the
 * server-supplied toolUseId when it exists (Claude); fall back to
 * "last tool with this name and no output yet" for backends that
 * don't emit an id (Codex / older models).
 */
export function matchToolForResult(toolUseId: string, toolName: string | undefined) {
  return (t: { id: string; name: string; output: string | null }) =>
    toolUseId ? t.id === toolUseId : t.name === toolName && t.output === null;
}

/**
 * Apply a `tool_result` event to a streaming-tools list. Walks from
 * the tail so the most recently-issued matching tool_use is the one
 * that gets the output (a Codex multi-call with two `Bash` invocations
 * fills the more recent one first). Returns a new array; the input is
 * never mutated. Non-`tool_result` events and orphan results (no
 * matching tool_use) return the array unchanged.
 */
export function applyToolResult(prev: readonly StreamingTool[], event: ChatEvent): StreamingTool[] {
  if (event.type !== "tool_result") return [...prev];
  const m = matchToolForResult(event.toolUseId ?? "", event.toolName);
  const out = prev.slice();
  for (let i = out.length - 1; i >= 0; i--) {
    if (m(out[i])) {
      out[i] = { ...out[i], output: event.toolOutput ?? "" };
      break;
    }
  }
  return out;
}

/**
 * Build a new StreamingTool entry from a `tool_use` event. Returns
 * null when the event is missing the required toolName (callers
 * silently skip).
 */
export function newToolFromEvent(event: ChatEvent): StreamingTool | null {
  if (event.type !== "tool_use" || !event.toolName) return null;
  return {
    id: event.toolUseId ?? "",
    name: event.toolName,
    input: event.toolInput ?? "",
    output: null,
  };
}

/**
 * Append a message unless an entry with the same id is already
 * present. Used by `message` and the no-abort `done` path. Returns a
 * new array; the input is never mutated.
 */
export function appendUniqueMessage(
  msgs: readonly AgentMessage[],
  m: AgentMessage,
): AgentMessage[] {
  return msgs.some((x) => x.id === m.id) ? msgs.slice() : [...msgs, m];
}

/**
 * Append a system-error entry to msgs unless the trailing entry is
 * already an identical system message. Returns the array unchanged
 * (new copy) when the dedupe fires. id factory + timestamp factory
 * are parameters so tests can pin them.
 */
export function appendSystemErrorIfNew(
  msgs: readonly AgentMessage[],
  errorContent: string,
  nowMs: () => number,
  timestamp: () => string,
): AgentMessage[] {
  const last = msgs[msgs.length - 1];
  if (last && last.role === "system" && last.content === errorContent) {
    return msgs.slice();
  }
  return [
    ...msgs,
    {
      id: "error_" + nowMs(),
      role: "system",
      content: errorContent,
      timestamp: timestamp(),
    },
  ];
}

/**
 * Apply a `done` event's message payload to the transcript, accounting
 * for the abort-marker handoff. Three cases:
 *
 *   1. abortedId set + server delivered a real id that already exists
 *      in the transcript (e.g. a stale synthesized terminal from a
 *      prior turn): drop the synthetic abort marker only.
 *   2. abortedId set + server delivered a fresh id: upgrade the
 *      synthetic abort marker to the server's message in-place.
 *   3. abortedId null + event.message present: standard
 *      appendUniqueMessage.
 *
 * Returns msgs unchanged (copy) when event.message is absent — the
 * background-chat reload path is handled by the caller, not this
 * reducer.
 */
export function applyDoneMessage(
  msgs: readonly AgentMessage[],
  event: ChatEvent,
  abortedId: string | null,
): AgentMessage[] {
  if (event.type !== "done" || !event.message) return msgs.slice();
  const incoming = event.message;
  if (abortedId) {
    if (msgs.some((m) => m.id === incoming.id && m.id !== abortedId)) {
      return msgs.filter((m) => m.id !== abortedId);
    }
    return msgs.map((m) => (m.id === abortedId ? incoming : m));
  }
  return appendUniqueMessage(msgs, incoming);
}

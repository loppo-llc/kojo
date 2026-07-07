import type { AgentMessage, ChatEvent, ToolUse } from "../../lib/agentApi";

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
  /** Narrative text bubble from a subagent turn (name is "" for these). */
  text?: string;
  /** Nested subagent (Task tool) tool calls / text bubbles, keyed by
   *  this tool's id via events carrying a matching parentToolUseId. */
  children?: StreamingTool[];
};

/**
 * Convert a still-streaming StreamingTool (output possibly null) into
 * the persisted ToolUse shape ToolUseCard renders, recursing into
 * subagent children so nested tool chips render the same way whether
 * the turn is live or already persisted.
 */
export function toToolUse(t: StreamingTool): ToolUse {
  return {
    id: t.id,
    name: t.name,
    input: t.input,
    output: t.output ?? "",
    text: t.text,
    children: t.children?.map(toToolUse),
  };
}

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
 * Route a subagent (Task tool) event — one carrying `parentToolUseId` —
 * into the Children of the matching top-level StreamingTool. Mirrors
 * newToolFromEvent / applyToolResult but nests one level instead of
 * appending to the top-level list, so subagent tool calls and narrative
 * text render under their parent Task tool chip rather than mixed into
 * the main turn's stream.
 *
 * Returns the input array unchanged (new copy) when:
 *   - event has no parentToolUseId (not a subagent event), or
 *   - no top-level tool with that id has been seen yet (best-effort
 *     live view only — the persisted message's Children are always
 *     authoritative once `done` arrives).
 */
export function applySubagentEvent(prev: readonly StreamingTool[], event: ChatEvent): StreamingTool[] {
  const parentId = event.parentToolUseId;
  if (!parentId) return [...prev];
  const idx = prev.findIndex((t) => t.id === parentId);
  if (idx === -1) return [...prev];

  const parent = prev[idx];
  const children = parent.children ? parent.children.slice() : [];

  if (event.type === "tool_use" && event.toolName) {
    children.push({ id: event.toolUseId ?? "", name: event.toolName, input: event.toolInput ?? "", output: null });
  } else if (event.type === "tool_result") {
    const m = matchToolForResult(event.toolUseId ?? "", event.toolName);
    for (let i = children.length - 1; i >= 0; i--) {
      if (m(children[i])) {
        children[i] = { ...children[i], output: event.toolOutput ?? "" };
        break;
      }
    }
  } else if (event.type === "text") {
    const last = children[children.length - 1];
    if (last && last.name === "") {
      children[children.length - 1] = { ...last, text: (last.text ?? "") + (event.delta ?? "") };
    } else {
      children.push({ id: "", name: "", input: "", output: null, text: event.delta ?? "" });
    }
  } else {
    return [...prev];
  }

  const out = prev.slice();
  out[idx] = { ...parent, children };
  return out;
}

/**
 * Apply a subagent (Task tool) event to a persisted ToolUse's children list.
 * Mirrors applySubagentEvent but operates on the persisted ToolUse shape
 * (output is "" rather than null) and de-dupes tool_use children by id so a
 * live push that overlaps a transcript reload can't double-insert.
 *
 * Returns the SAME array reference when nothing changed so callers can rely on
 * React's referential fast-path.
 */
function applyEventToToolChildren(children: readonly ToolUse[], event: ChatEvent): ToolUse[] {
  if (event.type === "tool_use" && event.toolName) {
    const id = event.toolUseId ?? "";
    if (id && children.some((c) => c.id === id)) return [...children];
    return [...children, { id, name: event.toolName, input: event.toolInput ?? "", output: "" }];
  }
  if (event.type === "tool_result") {
    const id = event.toolUseId ?? "";
    const out = children.slice();
    for (let i = out.length - 1; i >= 0; i--) {
      if (id && out[i].id === id) {
        out[i] = { ...out[i], output: event.toolOutput ?? "" };
        return out;
      }
    }
    return out;
  }
  if (event.type === "text") {
    const out = children.slice();
    const last = out[out.length - 1];
    if (last && last.name === "") {
      out[out.length - 1] = { ...last, text: (last.text ?? "") + (event.delta ?? "") };
    } else {
      out.push({ id: "", name: "", input: "", output: "", text: event.delta ?? "" });
    }
    return out;
  }
  return [...children];
}

/**
 * Route a subagent event into the timeline's already-persisted messages when
 * its parentToolUseId targets a Task ToolUse living inside a persisted message
 * (rather than the live streamTools of the current turn). This is what makes a
 * BACKGROUND subagent — whose output arrives after the spawning turn finished —
 * nest under its Task chip live, instead of being dropped.
 *
 * Returns the SAME array reference when no persisted message owns the tool_use
 * (the common case: the parent is in the live stream, or nothing matches), so
 * setMessages elides the re-render.
 */
export function applySubagentEventToMessages(
  msgs: AgentMessage[],
  event: ChatEvent,
): AgentMessage[] {
  const parentId = event.parentToolUseId;
  if (!parentId) return msgs;
  let hit = false;
  const next = msgs.map((msg) => {
    if (!msg.toolUses) return msg;
    const idx = msg.toolUses.findIndex((t) => t.id === parentId);
    if (idx === -1) return msg;
    hit = true;
    const parent = msg.toolUses[idx];
    const children = applyEventToToolChildren(parent.children ?? [], event);
    const toolUses = msg.toolUses.slice();
    toolUses[idx] = { ...parent, children };
    return { ...msg, toolUses };
  });
  return hit ? next : msgs;
}

/**
 * Append a message unless an entry with the same id is already
 * present. Used by `message` and the no-abort `done` path.
 *
 * Returns the SAME array reference when the dedupe fires — that
 * matches React's referential-equality fast path so setMessages
 * with this result is a no-op and the parent component does not
 * re-render. The readonly cast in the return type is the caller's
 * problem to widen if needed (every call site treats the result
 * as immutable).
 */
export function appendUniqueMessage(
  msgs: AgentMessage[],
  m: AgentMessage,
): AgentMessage[] {
  return msgs.some((x) => x.id === m.id) ? msgs : [...msgs, m];
}

/**
 * Append a system-error entry to msgs unless the trailing entry is
 * already an identical system message. Returns the SAME array
 * reference on dedupe (same React-fast-path reason as
 * appendUniqueMessage). id factory + timestamp factory are
 * parameters so tests can pin them.
 */
export function appendSystemErrorIfNew(
  msgs: AgentMessage[],
  errorContent: string,
  nowMs: () => number,
  timestamp: () => string,
): AgentMessage[] {
  const last = msgs[msgs.length - 1];
  if (last && last.role === "system" && last.content === errorContent) {
    return msgs;
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
  msgs: AgentMessage[],
  event: ChatEvent,
  abortedId: string | null,
): AgentMessage[] {
  // Non-done / no-message events are a no-op; return the SAME
  // array so React's setState referential fast-path elides the
  // re-render (matches the pre-refactor inline behavior of falling
  // through the switch case).
  if (event.type !== "done" || !event.message) return msgs;
  const incoming = event.message;
  if (abortedId) {
    if (msgs.some((m) => m.id === incoming.id && m.id !== abortedId)) {
      return msgs.filter((m) => m.id !== abortedId);
    }
    return msgs.map((m) => (m.id === abortedId ? incoming : m));
  }
  return appendUniqueMessage(msgs, incoming);
}

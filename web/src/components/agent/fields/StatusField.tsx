import { useState } from "react";
import { Input } from "../../ui/Input";
import { Textarea } from "../../ui/Textarea";

type Entry = {
  key: string;
  value: string;
  // wasString pins the original JSON type so an agent-authored string
  // that merely looks like a number ("3", "true") never gets silently
  // re-typed by a UI round-trip. Rows added in the UI leave it false
  // and get type inference on save.
  wasString: boolean;
  // fromDoc marks rows hydrated from the original document. Only
  // UI-added rows are subject to the abandoned-empty-row drop, so an
  // agent-authored key that happens to be whitespace survives a save.
  fromDoc: boolean;
};

// parseFlat converts a status.json body into table rows. Returns null
// when the content is not a flat JSON object of scalars — the editor
// then falls back to a raw textarea so nothing the agent wrote by hand
// is ever unrenderable or silently rewritten.
function parseFlat(content: string): Entry[] | null {
  try {
    const v: unknown = JSON.parse(content);
    if (!v || typeof v !== "object" || Array.isArray(v)) return null;
    const entries: Entry[] = [];
    for (const [k, val] of Object.entries(v as Record<string, unknown>)) {
      if (val === null || typeof val === "object") return null;
      entries.push({
        key: k,
        value: String(val),
        wasString: typeof val === "string",
        fromDoc: true,
      });
    }
    return entries;
  } catch {
    return null;
  }
}

// isJSONNumber mirrors the JSON number grammar closely enough for
// "should this cell round-trip as a number" — Number() alone accepts
// "0x10" / "Infinity" / whitespace, which JSON.stringify would then
// mangle or reject.
const jsonNumberRe = /^-?(0|[1-9]\d*)(\.\d+)?([eE][+-]?\d+)?$/;

// serialize turns table rows back into the canonical JSON document.
// Cells whose original JSON type was string stay strings no matter what
// they contain; other cells that read exactly like a JSON boolean or
// number are emitted typed so an agent-authored `"fatigue_level": 3`
// survives a UI edit. Blank keys are dropped; duplicate keys
// last-write-wins (matching JSON object semantics).
function serialize(entries: Entry[]): string {
  const obj: Record<string, unknown> = {};
  for (const e of entries) {
    // Drop only abandoned UI-added rows (blank key, never part of the
    // document). Keys are otherwise stored verbatim — agent-authored
    // keys with surrounding or even all-whitespace content survive.
    if (!e.fromDoc && !e.key.trim()) continue;
    const t = e.value.trim();
    if (e.wasString) obj[e.key] = e.value;
    else if (t === "true") obj[e.key] = true;
    else if (t === "false") obj[e.key] = false;
    else if (jsonNumberRe.test(t)) obj[e.key] = Number(t);
    else obj[e.key] = e.value;
  }
  return JSON.stringify(obj, null, 2) + "\n";
}

interface StatusFieldProps {
  /** Initial status.json body. The component owns its editing state
   *  after mount — remount (change the React `key`) to re-hydrate. */
  initialContent: string;
  /** Fired with the full serialized JSON document on every edit. */
  onChange: (content: string) => void;
}

/**
 * Key-value table editor for the agent's status.json. Flat scalar
 * objects render as editable rows with add/remove; anything else
 * (nested JSON, invalid JSON the agent wrote straight to disk) falls
 * back to a raw JSON textarea rather than destroying the document.
 */
export function StatusField({ initialContent, onChange }: StatusFieldProps) {
  const [entries, setEntries] = useState<Entry[] | null>(() =>
    parseFlat(initialContent),
  );
  const [raw, setRaw] = useState(initialContent);

  if (entries === null) {
    return (
      <Textarea
        mono
        rows={6}
        value={raw}
        onChange={(e) => {
          setRaw(e.target.value);
          onChange(e.target.value);
        }}
      />
    );
  }

  const update = (next: Entry[]) => {
    setEntries(next);
    onChange(serialize(next));
  };

  return (
    <div className="flex flex-col gap-1.5">
      {entries.map((e, i) => (
        <div key={i} className="flex items-center gap-1.5">
          <Input
            mono
            value={e.key}
            placeholder="key"
            aria-label={`status key ${i + 1}`}
            className="basis-2/5"
            onChange={(ev) =>
              update(entries.map((x, j) => (j === i ? { ...x, key: ev.target.value } : x)))
            }
          />
          <Input
            value={e.value}
            placeholder="value"
            aria-label={`status value ${i + 1}`}
            className="flex-1"
            onChange={(ev) =>
              update(entries.map((x, j) => (j === i ? { ...x, value: ev.target.value } : x)))
            }
          />
          <button
            type="button"
            aria-label={`remove status row ${i + 1}`}
            className="shrink-0 rounded-md px-2 py-1.5 text-[13px] text-ink-dim transition-colors hover:bg-raised hover:text-lamp-err"
            onClick={() => update(entries.filter((_, j) => j !== i))}
          >
            ✕
          </button>
        </div>
      ))}
      <button
        type="button"
        className="self-start rounded-md px-2 py-1 text-[13px] text-ink-dim transition-colors hover:bg-raised hover:text-ink"
        onClick={() =>
          update([...entries, { key: "", value: "", wasString: false, fromDoc: false }])
        }
      >
        + Add entry
      </button>
    </div>
  );
}

import { Field } from "../../ui/Field";
import { Textarea } from "../../ui/Textarea";
import { Input } from "../../ui/Input";
import { Button } from "../../ui/Button";

/**
 * The "Persona" field: a description textarea plus an inline "AI" prompt row
 * that regenerates/edits the persona. The per-caller differences (textarea
 * rows + placeholder, prompt placeholder, and the busy/spinner flags) are
 * threaded as props so each screen renders exactly as before.
 *
 * `busy` disables the prompt input's Enter shortcut and the button; `spinning`
 * swaps the button label for a spinner. AgentSettings passes the same flag for
 * both; AgentCreate disables on any generation but only spins for persona.
 */
export function PersonaField({
  persona,
  setPersona,
  textareaRows,
  textareaPlaceholder,
  personaPrompt,
  setPersonaPrompt,
  promptPlaceholder,
  busy,
  spinning,
  onGenerate,
}: {
  persona: string;
  setPersona: (v: string) => void;
  textareaRows: number;
  textareaPlaceholder?: string;
  personaPrompt: string;
  setPersonaPrompt: (v: string) => void;
  promptPlaceholder: string;
  busy: boolean;
  spinning: boolean;
  onGenerate: () => void;
}) {
  return (
    <Field label="Persona">
      <Textarea
        value={persona}
        onChange={(e) => setPersona(e.target.value)}
        placeholder={textareaPlaceholder}
        rows={textareaRows}
      />
      <div className="mt-2 flex gap-2">
        <Input
          aria-label="Persona generation prompt"
          value={personaPrompt}
          onChange={(e) => setPersonaPrompt(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.nativeEvent.isComposing && !e.shiftKey && !busy) {
              e.preventDefault();
              onGenerate();
            }
          }}
          placeholder={promptPlaceholder}
          className="flex-1"
        />
        <Button
          onClick={onGenerate}
          disabled={busy || !personaPrompt.trim()}
          className="flex shrink-0 items-center gap-1"
        >
          {spinning ? <span className="animate-spin">↻</span> : "✨ AI"}
        </Button>
      </div>
    </Field>
  );
}

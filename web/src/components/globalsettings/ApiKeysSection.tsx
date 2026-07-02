import type { EmbeddingModelHook } from "./useEmbeddingModel";
import type { GeminiApiKeyHook } from "./useGeminiApiKey";
import { SectionCard } from "../ui/SectionCard";
import { Field } from "../ui/Field";
import { Input } from "../ui/Input";
import { Select } from "../ui/Select";
import { Button } from "../ui/Button";

interface Props {
  gemini: GeminiApiKeyHook;
  embedding: EmbeddingModelHook;
}

/** API Keys section — Gemini API key + embedding model selector. */
export function ApiKeysSection({ gemini, embedding }: Props) {
  return (
    <SectionCard
      title="API Keys"
      description="Encrypted storage for API keys. Used for embedding and image generation."
    >
      <div className="rounded-[10px] border border-hairline bg-raised p-3">
        <div className="flex items-center justify-between gap-2">
          <div className="min-w-0">
            <div className="text-[13px] font-medium text-ink">Gemini API</div>
            <div className="mt-0.5 text-[12px]">
              {gemini.configured ? (
                <span className="text-lamp-run">Configured</span>
              ) : gemini.hasFallback ? (
                <span className="text-lamp-warn">Using fallback</span>
              ) : (
                <span className="text-ink-faint">Not configured</span>
              )}
            </div>
          </div>
          <div className="flex shrink-0 items-center gap-2">
            <Button onClick={gemini.toggleEditing}>
              {gemini.editing ? "Cancel" : gemini.configured ? "Update" : "Configure"}
            </Button>
            {gemini.configured && (
              <button
                onClick={gemini.remove}
                aria-label="Remove Gemini API key"
                className="rounded-md px-1.5 text-ink-faint transition-colors hover:text-lamp-err"
              >
                &times;
              </button>
            )}
          </div>
        </div>

        {gemini.editing && (
          <div className="mt-3 space-y-2 border-t border-hairline pt-3">
            <Input
              mono
              type="password"
              value={gemini.input}
              onChange={(e) => gemini.setInput(e.target.value)}
              placeholder="AIza..."
            />
            <Button
              variant="primary"
              onClick={gemini.save}
              disabled={gemini.saving || !gemini.input.trim()}
              className="w-full"
            >
              {gemini.saving ? "Saving..." : "Save"}
            </Button>
          </div>
        )}

        <div className="mt-3 border-t border-hairline pt-3">
          <Field label="Embedding Model">
            {embedding.loading ? (
              <div className="text-[12px] text-ink-faint">Loading models...</div>
            ) : embedding.available.length > 0 ? (
              <Select
                mono
                value={embedding.model}
                onChange={(e) => embedding.change(e.target.value)}
                disabled={embedding.saving}
              >
                {!embedding.available.includes(embedding.model) && embedding.model && (
                  <option value={embedding.model}>{embedding.model} (unavailable)</option>
                )}
                {embedding.available.map((m) => (
                  <option key={m} value={m}>
                    {m}
                  </option>
                ))}
              </Select>
            ) : (
              <div className="text-[12px] text-ink-faint">
                {gemini.configured ? "Failed to load models" : "Configure API key to see available models"}
              </div>
            )}
          </Field>
        </div>
      </div>
    </SectionCard>
  );
}

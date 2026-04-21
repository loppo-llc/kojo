import type { EmbeddingModelHook } from "./useEmbeddingModel";
import type { GeminiApiKeyHook } from "./useGeminiApiKey";

interface Props {
  gemini: GeminiApiKeyHook;
  embedding: EmbeddingModelHook;
}

/** API Keys section — Gemini API key + embedding model selector. */
export function ApiKeysSection({ gemini, embedding }: Props) {
  return (
    <div>
      <h2 className="text-xs font-semibold text-neutral-500 uppercase tracking-wider mb-3">
        API Keys
      </h2>
      <p className="text-xs text-neutral-600 mb-3">
        Encrypted storage for API keys. Used for embedding and image generation.
      </p>

      <div className="p-3 bg-neutral-900 border border-neutral-800 rounded-lg mb-2">
        <div className="flex items-center justify-between">
          <div>
            <div className="text-sm font-medium">Gemini API</div>
            <div className="text-xs text-neutral-500 mt-0.5">
              {gemini.configured ? (
                <span className="text-emerald-500">Configured</span>
              ) : gemini.hasFallback ? (
                <span className="text-amber-500">Using fallback</span>
              ) : (
                <span className="text-neutral-600">Not configured</span>
              )}
            </div>
          </div>
          <div className="flex gap-2">
            <button
              onClick={gemini.toggleEditing}
              className="px-2 py-1 bg-neutral-800 hover:bg-neutral-700 rounded text-xs"
            >
              {gemini.editing ? "Cancel" : gemini.configured ? "Update" : "Configure"}
            </button>
            {gemini.configured && (
              <button
                onClick={gemini.remove}
                className="text-neutral-600 hover:text-red-400 text-sm"
              >
                &times;
              </button>
            )}
          </div>
        </div>

        {gemini.editing && (
          <div className="mt-3 space-y-2 border-t border-neutral-800 pt-3">
            <input
              type="password"
              value={gemini.input}
              onChange={(e) => gemini.setInput(e.target.value)}
              placeholder="AIza..."
              className="w-full px-3 py-2 bg-neutral-800 border border-neutral-700 rounded text-xs font-mono focus:outline-none focus:border-neutral-500"
            />
            <button
              onClick={gemini.save}
              disabled={gemini.saving || !gemini.input.trim()}
              className="w-full py-2 bg-neutral-700 hover:bg-neutral-600 rounded text-xs font-medium disabled:opacity-40"
            >
              {gemini.saving ? "Saving..." : "Save"}
            </button>
          </div>
        )}

        <div className="mt-3 border-t border-neutral-800 pt-3">
          <div className="text-xs text-neutral-500 mb-1.5">Embedding Model</div>
          {embedding.loading ? (
            <div className="text-xs text-neutral-600">Loading models...</div>
          ) : embedding.available.length > 0 ? (
            <select
              value={embedding.model}
              onChange={(e) => embedding.change(e.target.value)}
              disabled={embedding.saving}
              className="w-full px-3 py-1.5 bg-neutral-800 border border-neutral-700 rounded text-xs font-mono focus:outline-none focus:border-neutral-500 disabled:opacity-40"
            >
              {!embedding.available.includes(embedding.model) && embedding.model && (
                <option value={embedding.model}>{embedding.model} (unavailable)</option>
              )}
              {embedding.available.map((m) => (
                <option key={m} value={m}>
                  {m}
                </option>
              ))}
            </select>
          ) : (
            <div className="text-xs text-neutral-600">
              {gemini.configured ? "Failed to load models" : "Configure API key to see available models"}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

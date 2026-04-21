import { useCallback, useEffect, useState } from "react";
import { agentApi } from "../../lib/agentApi";

/**
 * useEmbeddingModel owns the "currently selected embedding model" + the
 * list of available models. Both depend on the Gemini API key state, so
 * the caller passes `apiKeyConfigured` and `apiKeySaveToken` — the latter
 * flips each time the key is re-saved, letting us re-fetch the model list
 * without needing an imperative callback.
 */
export interface EmbeddingModelHook {
  model: string;
  available: string[];
  loading: boolean;
  saving: boolean;
  change: (next: string) => Promise<void>;
}

export function useEmbeddingModel(
  apiKeyConfigured: boolean,
  apiKeySaveToken: number,
  initialModel: string | null,
  onError: (msg: string) => void,
  onSuccess: () => void,
): EmbeddingModelHook {
  const [model, setModel] = useState("");
  const [available, setAvailable] = useState<string[]>([]);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);

  // Seed the current selection from the initial GET response.
  useEffect(() => {
    if (initialModel != null) {
      setModel(initialModel);
    }
  }, [initialModel]);

  // Fetch the list whenever the API key becomes configured, or whenever the
  // user saves a new key (saveToken bump). Skip when not configured.
  useEffect(() => {
    if (!apiKeyConfigured) return;
    let cancelled = false;
    setLoading(true);
    setAvailable([]);
    agentApi.embeddingModel
      .list()
      .then((models) => {
        if (!cancelled) setAvailable(models);
      })
      .catch(() => {})
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [apiKeyConfigured, apiKeySaveToken]);

  const change = useCallback(
    async (next: string) => {
      if (!next || next === model) return;
      setSaving(true);
      onError("");
      try {
        await agentApi.embeddingModel.set(next);
        setModel(next);
        onSuccess();
      } catch (err) {
        onError(err instanceof Error ? err.message : String(err));
      } finally {
        setSaving(false);
      }
    },
    [model, onError, onSuccess],
  );

  return { model, available, loading, saving, change };
}

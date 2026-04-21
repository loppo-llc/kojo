import { useCallback, useEffect, useState } from "react";
import { agentApi } from "../../lib/agentApi";

/**
 * useGeminiApiKey encapsulates the Gemini API key configured/fallback status
 * and the save/remove flows. Errors and success pulses are surfaced through
 * the caller-supplied callbacks so the owning page can render a single
 * shared banner.
 *
 * `saveToken` increments on every successful Save so downstream hooks
 * (e.g. useEmbeddingModel) can re-run side effects without having to
 * observe imperative events.
 */
export interface GeminiApiKeyHook {
  configured: boolean;
  hasFallback: boolean;
  editing: boolean;
  input: string;
  saving: boolean;
  /** Resolves once the initial GET has completed. */
  loaded: boolean;
  /** Embedding model name reported by the initial GET (null until loaded). */
  initialEmbeddingModel: string | null;
  /** Monotonically-increasing counter; bumps on each successful save. */
  saveToken: number;
  setInput: (v: string) => void;
  toggleEditing: () => void;
  save: () => Promise<void>;
  remove: () => Promise<void>;
}

export function useGeminiApiKey(
  onError: (msg: string) => void,
  onSuccess: () => void,
): GeminiApiKeyHook {
  const [configured, setConfigured] = useState(false);
  const [hasFallback, setHasFallback] = useState(false);
  const [editing, setEditing] = useState(false);
  const [input, setInput] = useState("");
  const [saving, setSaving] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [initialEmbeddingModel, setInitialEmbeddingModel] = useState<string | null>(null);
  const [saveToken, setSaveToken] = useState(0);

  useEffect(() => {
    agentApi.apiKeys
      .get("gemini")
      .then((r: { configured: boolean; hasFallback?: boolean; embeddingModel?: string }) => {
        setConfigured(r.configured);
        setHasFallback(r.hasFallback ?? false);
        setInitialEmbeddingModel(r.embeddingModel ?? null);
      })
      .catch(() => {})
      .finally(() => setLoaded(true));
  }, []);

  const toggleEditing = useCallback(() => {
    setEditing((e) => !e);
    setInput("");
    onError("");
  }, [onError]);

  const save = useCallback(async () => {
    if (!input.trim()) return;
    setSaving(true);
    onError("");
    try {
      await agentApi.apiKeys.set("gemini", input.trim());
      setConfigured(true);
      setEditing(false);
      setInput("");
      setSaveToken((t) => t + 1);
      onSuccess();
    } catch (err) {
      onError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  }, [input, onError, onSuccess]);

  const remove = useCallback(async () => {
    if (!confirm("Remove Gemini API key?")) return;
    try {
      await agentApi.apiKeys.delete("gemini");
      setConfigured(false);
    } catch (err) {
      onError(err instanceof Error ? err.message : String(err));
    }
  }, [onError]);

  return {
    configured,
    hasFallback,
    editing,
    input,
    saving,
    loaded,
    initialEmbeddingModel,
    saveToken,
    setInput,
    toggleEditing,
    save,
    remove,
  };
}

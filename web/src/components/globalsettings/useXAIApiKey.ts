import { useCallback, useEffect, useState } from "react";
import { agentApi } from "../../lib/agentApi";
import { errMsg } from "../../lib/utils";

/**
 * useXAIApiKey encapsulates the xAI (Grok) API key configured status and the
 * save/remove flows. Used for the voice input (speech-to-text) feature: the
 * server mints short-lived ephemeral tokens from this key so the browser
 * never sees the long-lived secret.
 *
 * Mirrors useGeminiApiKey but without the embedding-model coupling.
 */
export interface XAIApiKeyHook {
  configured: boolean;
  hasFallback: boolean;
  editing: boolean;
  input: string;
  saving: boolean;
  loaded: boolean;
  setInput: (v: string) => void;
  toggleEditing: () => void;
  save: () => Promise<void>;
  remove: () => Promise<void>;
}

export function useXAIApiKey(
  onError: (msg: string) => void,
  onSuccess: () => void,
): XAIApiKeyHook {
  const [configured, setConfigured] = useState(false);
  const [hasFallback, setHasFallback] = useState(false);
  const [editing, setEditing] = useState(false);
  const [input, setInput] = useState("");
  const [saving, setSaving] = useState(false);
  const [loaded, setLoaded] = useState(false);

  useEffect(() => {
    agentApi.apiKeys
      .get("xai")
      .then((r: { configured: boolean; hasFallback?: boolean }) => {
        setConfigured(r.configured);
        setHasFallback(r.hasFallback ?? false);
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
      await agentApi.apiKeys.set("xai", input.trim());
      setConfigured(true);
      setEditing(false);
      setInput("");
      onSuccess();
    } catch (err) {
      onError(errMsg(err));
    } finally {
      setSaving(false);
    }
  }, [input, onError, onSuccess]);

  const remove = useCallback(async () => {
    if (!confirm("Remove xAI API key?")) return;
    try {
      await agentApi.apiKeys.delete("xai");
      setConfigured(false);
    } catch (err) {
      onError(errMsg(err));
    }
  }, [onError]);

  return {
    configured,
    hasFallback,
    editing,
    input,
    saving,
    loaded,
    setInput,
    toggleEditing,
    save,
    remove,
  };
}

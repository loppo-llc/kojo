import { useEffect, useState } from "react";
import { useNavigate } from "react-router";
import { agentApi, type OAuthClientInfo } from "../lib/agentApi";
import { useEnterSends } from "../lib/preferences";

export function GlobalSettings() {
  const navigate = useNavigate();
  const [clients, setClients] = useState<OAuthClientInfo[]>([]);
  const [editProvider, setEditProvider] = useState<string | null>(null);
  const [clientId, setClientId] = useState("");
  const [clientSecret, setClientSecret] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [success, setSuccess] = useState(false);

  const [enterSends, setEnterSends] = useEnterSends();

  // API Keys state
  const [geminiKeyConfigured, setGeminiKeyConfigured] = useState(false);
  const [geminiHasFallback, setGeminiHasFallback] = useState(false);
  const [editingGeminiKey, setEditingGeminiKey] = useState(false);
  const [geminiKeyInput, setGeminiKeyInput] = useState("");
  const [savingKey, setSavingKey] = useState(false);

  // Embedding model state
  const [embeddingModel, setEmbeddingModel] = useState("");
  const [availableModels, setAvailableModels] = useState<string[]>([]);
  const [loadingModels, setLoadingModels] = useState(false);
  const [savingModel, setSavingModel] = useState(false);

  useEffect(() => {
    agentApi.oauthClients.list().then(setClients).catch(() => {});
    agentApi.apiKeys.get("gemini").then((r: { configured: boolean; hasFallback?: boolean; embeddingModel?: string }) => {
      setGeminiKeyConfigured(r.configured);
      setGeminiHasFallback(r.hasFallback ?? false);
      if (r.embeddingModel) {
        setEmbeddingModel(r.embeddingModel);
      }
      // Fetch available models if API key is configured
      if (r.configured) {
        setLoadingModels(true);
        agentApi.embeddingModel.list()
          .then(setAvailableModels)
          .catch(() => {})
          .finally(() => setLoadingModels(false));
      }
    }).catch(() => {});
  }, []);

  const handleSave = async (provider: string) => {
    if (!clientId.trim() || !clientSecret.trim()) return;
    setSaving(true);
    setError("");
    try {
      await agentApi.oauthClients.set(provider, clientId.trim(), clientSecret.trim());
      setClients((prev) =>
        prev.map((c) => (c.provider === provider ? { ...c, configured: true } : c)),
      );
      setEditProvider(null);
      setClientId("");
      setClientSecret("");
      setSuccess(true);
      setTimeout(() => setSuccess(false), 2000);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const handleRemove = async (provider: string) => {
    if (!confirm(`Remove OAuth credentials for ${provider}?`)) return;
    try {
      await agentApi.oauthClients.delete(provider);
      setClients((prev) =>
        prev.map((c) => (c.provider === provider ? { ...c, configured: false } : c)),
      );
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const handleSaveGeminiKey = async () => {
    if (!geminiKeyInput.trim()) return;
    setSavingKey(true);
    setError("");
    try {
      await agentApi.apiKeys.set("gemini", geminiKeyInput.trim());
      setGeminiKeyConfigured(true);
      setEditingGeminiKey(false);
      setGeminiKeyInput("");
      setSuccess(true);
      setTimeout(() => setSuccess(false), 2000);

      // The page may have loaded with no key configured, in which case we
      // never fetched the model list on mount. Fetch it now so the
      // Embedding Model dropdown appears without requiring a manual refresh.
      setAvailableModels([]);
      setLoadingModels(true);
      agentApi.embeddingModel
        .list()
        .then(setAvailableModels)
        .catch(() => {})
        .finally(() => setLoadingModels(false));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSavingKey(false);
    }
  };

  const handleChangeEmbeddingModel = async (model: string) => {
    if (!model || model === embeddingModel) return;
    setSavingModel(true);
    setError("");
    try {
      await agentApi.embeddingModel.set(model);
      setEmbeddingModel(model);
      setSuccess(true);
      setTimeout(() => setSuccess(false), 2000);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSavingModel(false);
    }
  };

  const handleRemoveGeminiKey = async () => {
    if (!confirm("Remove Gemini API key?")) return;
    try {
      await agentApi.apiKeys.delete("gemini");
      setGeminiKeyConfigured(false);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const providerLabels: Record<string, string> = {
    gmail: "Google (Gmail)",
  };

  return (
    <div className="min-h-full bg-neutral-950 text-neutral-200">
      <header className="flex items-center gap-2 px-4 py-3 border-b border-neutral-800">
        <button
          onClick={() => navigate("/")}
          className="text-neutral-400 hover:text-neutral-200"
        >
          &larr;
        </button>
        <h1 className="text-lg font-bold">Settings</h1>
      </header>

      <main className="p-4 space-y-5 max-w-md mx-auto">
        {/* API Keys */}
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
                  {geminiKeyConfigured ? (
                    <span className="text-emerald-500">Configured</span>
                  ) : geminiHasFallback ? (
                    <span className="text-amber-500">Using fallback</span>
                  ) : (
                    <span className="text-neutral-600">Not configured</span>
                  )}
                </div>
              </div>
              <div className="flex gap-2">
                <button
                  onClick={() => {
                    setEditingGeminiKey(!editingGeminiKey);
                    setGeminiKeyInput("");
                    setError("");
                  }}
                  className="px-2 py-1 bg-neutral-800 hover:bg-neutral-700 rounded text-xs"
                >
                  {editingGeminiKey ? "Cancel" : geminiKeyConfigured ? "Update" : "Configure"}
                </button>
                {geminiKeyConfigured && (
                  <button
                    onClick={handleRemoveGeminiKey}
                    className="text-neutral-600 hover:text-red-400 text-sm"
                  >
                    &times;
                  </button>
                )}
              </div>
            </div>

            {editingGeminiKey && (
              <div className="mt-3 space-y-2 border-t border-neutral-800 pt-3">
                <input
                  type="password"
                  value={geminiKeyInput}
                  onChange={(e) => setGeminiKeyInput(e.target.value)}
                  placeholder="AIza..."
                  className="w-full px-3 py-2 bg-neutral-800 border border-neutral-700 rounded text-xs font-mono focus:outline-none focus:border-neutral-500"
                />
                <button
                  onClick={handleSaveGeminiKey}
                  disabled={savingKey || !geminiKeyInput.trim()}
                  className="w-full py-2 bg-neutral-700 hover:bg-neutral-600 rounded text-xs font-medium disabled:opacity-40"
                >
                  {savingKey ? "Saving..." : "Save"}
                </button>
              </div>
            )}

            <div className="mt-3 border-t border-neutral-800 pt-3">
              <div className="text-xs text-neutral-500 mb-1.5">Embedding Model</div>
              {loadingModels ? (
                <div className="text-xs text-neutral-600">Loading models...</div>
              ) : availableModels.length > 0 ? (
                <select
                  value={embeddingModel}
                  onChange={(e) => handleChangeEmbeddingModel(e.target.value)}
                  disabled={savingModel}
                  className="w-full px-3 py-1.5 bg-neutral-800 border border-neutral-700 rounded text-xs font-mono focus:outline-none focus:border-neutral-500 disabled:opacity-40"
                >
                  {!availableModels.includes(embeddingModel) && embeddingModel && (
                    <option value={embeddingModel}>{embeddingModel} (unavailable)</option>
                  )}
                  {availableModels.map((m) => (
                    <option key={m} value={m}>{m}</option>
                  ))}
                </select>
              ) : (
                <div className="text-xs text-neutral-600">
                  {geminiKeyConfigured ? "Failed to load models" : "Configure API key to see available models"}
                </div>
              )}
            </div>
          </div>
        </div>

        {/* OAuth Clients */}
        <div>
          <h2 className="text-xs font-semibold text-neutral-500 uppercase tracking-wider mb-3">
            OAuth Clients
          </h2>
          <p className="text-xs text-neutral-600 mb-3">
            Configure OAuth2 credentials for notification sources. These are shared across all agents.
          </p>

          {clients.map((client) => (
            <div
              key={client.provider}
              className="p-3 bg-neutral-900 border border-neutral-800 rounded-lg mb-2"
            >
              <div className="flex items-center justify-between">
                <div>
                  <div className="text-sm font-medium">
                    {providerLabels[client.provider] ?? client.provider}
                  </div>
                  <div className="text-xs text-neutral-500 mt-0.5">
                    {client.configured ? (
                      <span className="text-emerald-500">Configured</span>
                    ) : (
                      <span className="text-neutral-600">Not configured</span>
                    )}
                  </div>
                </div>
                <div className="flex gap-2">
                  <button
                    onClick={() => {
                      setEditProvider(
                        editProvider === client.provider ? null : client.provider,
                      );
                      setClientId("");
                      setClientSecret("");
                      setError("");
                    }}
                    className="px-2 py-1 bg-neutral-800 hover:bg-neutral-700 rounded text-xs"
                  >
                    {editProvider === client.provider ? "Cancel" : client.configured ? "Update" : "Configure"}
                  </button>
                  {client.configured && (
                    <button
                      onClick={() => handleRemove(client.provider)}
                      className="text-neutral-600 hover:text-red-400 text-sm"
                    >
                      &times;
                    </button>
                  )}
                </div>
              </div>

              {editProvider === client.provider && (
                <div className="mt-3 space-y-2 border-t border-neutral-800 pt-3">
                  <input
                    type="text"
                    value={clientId}
                    onChange={(e) => setClientId(e.target.value)}
                    placeholder="Client ID"
                    className="w-full px-3 py-2 bg-neutral-800 border border-neutral-700 rounded text-xs focus:outline-none focus:border-neutral-500"
                  />
                  <input
                    type="password"
                    value={clientSecret}
                    onChange={(e) => setClientSecret(e.target.value)}
                    placeholder="Client Secret"
                    className="w-full px-3 py-2 bg-neutral-800 border border-neutral-700 rounded text-xs focus:outline-none focus:border-neutral-500"
                  />
                  <button
                    onClick={() => handleSave(client.provider)}
                    disabled={saving || !clientId.trim() || !clientSecret.trim()}
                    className="w-full py-2 bg-neutral-700 hover:bg-neutral-600 rounded text-xs font-medium disabled:opacity-40"
                  >
                    {saving ? "Saving..." : "Save"}
                  </button>
                </div>
              )}
            </div>
          ))}
        </div>

        {/* Chat Preferences */}
        <div>
          <h2 className="text-xs font-semibold text-neutral-500 uppercase tracking-wider mb-3">
            Chat
          </h2>
          <div className="p-3 bg-neutral-900 border border-neutral-800 rounded-lg">
            <div className="flex items-center justify-between">
              <div>
                <div className="text-sm font-medium">Send with Enter</div>
                <div className="text-xs text-neutral-500 mt-0.5">
                  {enterSends
                    ? "Enter to send, Shift+Enter for newline"
                    : "Shift+Enter to send, Enter for newline"}
                </div>
              </div>
              <button
                onClick={() => setEnterSends(!enterSends)}
                className={`relative w-10 h-5 rounded-full transition-colors ${enterSends ? "bg-blue-600" : "bg-neutral-700"}`}
                role="switch"
                aria-checked={enterSends}
              >
                <span className={`absolute top-0.5 left-0.5 w-4 h-4 rounded-full bg-white transition-transform ${enterSends ? "translate-x-5" : ""}`} />
              </button>
            </div>
          </div>
        </div>

        {error && (
          <div className="p-3 bg-red-950 border border-red-800 rounded-lg text-sm text-red-300">
            {error}
          </div>
        )}
        {success && (
          <div className="p-3 bg-green-950 border border-green-800 rounded-lg text-sm text-green-300">
            Saved
          </div>
        )}
      </main>
    </div>
  );
}

import { useCallback, useEffect, useState } from "react";
import { agentApi, type OAuthClientInfo } from "../../lib/agentApi";

export interface OAuthClientsHook {
  clients: OAuthClientInfo[];
  editProvider: string | null;
  clientId: string;
  clientSecret: string;
  saving: boolean;
  setClientId: (v: string) => void;
  setClientSecret: (v: string) => void;
  toggleEdit: (provider: string) => void;
  save: (provider: string) => Promise<void>;
  remove: (provider: string) => Promise<void>;
}

/**
 * useOAuthClients manages the list and edit state for OAuth client
 * credentials. Errors and success pulses bubble up via callbacks so the
 * parent can own a single shared banner.
 */
export function useOAuthClients(
  onError: (msg: string) => void,
  onSuccess: () => void,
): OAuthClientsHook {
  const [clients, setClients] = useState<OAuthClientInfo[]>([]);
  const [editProvider, setEditProvider] = useState<string | null>(null);
  const [clientId, setClientId] = useState("");
  const [clientSecret, setClientSecret] = useState("");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    agentApi.oauthClients.list().then(setClients).catch(() => {});
  }, []);

  const toggleEdit = useCallback(
    (provider: string) => {
      setEditProvider((prev) => (prev === provider ? null : provider));
      setClientId("");
      setClientSecret("");
      onError("");
    },
    [onError],
  );

  const save = useCallback(
    async (provider: string) => {
      if (!clientId.trim() || !clientSecret.trim()) return;
      setSaving(true);
      onError("");
      try {
        await agentApi.oauthClients.set(provider, clientId.trim(), clientSecret.trim());
        setClients((prev) =>
          prev.map((c) => (c.provider === provider ? { ...c, configured: true } : c)),
        );
        setEditProvider(null);
        setClientId("");
        setClientSecret("");
        onSuccess();
      } catch (err) {
        onError(err instanceof Error ? err.message : String(err));
      } finally {
        setSaving(false);
      }
    },
    [clientId, clientSecret, onError, onSuccess],
  );

  const remove = useCallback(
    async (provider: string) => {
      if (!confirm(`Remove OAuth credentials for ${provider}?`)) return;
      try {
        await agentApi.oauthClients.delete(provider);
        setClients((prev) =>
          prev.map((c) => (c.provider === provider ? { ...c, configured: false } : c)),
        );
      } catch (err) {
        onError(err instanceof Error ? err.message : String(err));
      }
    },
    [onError],
  );

  return {
    clients,
    editProvider,
    clientId,
    clientSecret,
    saving,
    setClientId,
    setClientSecret,
    toggleEdit,
    save,
    remove,
  };
}

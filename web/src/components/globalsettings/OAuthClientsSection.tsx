import type { OAuthClientsHook } from "./useOAuthClients";

const providerLabels: Record<string, string> = {
  gmail: "Google (Gmail)",
};

interface Props {
  oauth: OAuthClientsHook;
}

/** OAuth Clients section. */
export function OAuthClientsSection({ oauth }: Props) {
  return (
    <div>
      <h2 className="text-xs font-semibold text-neutral-500 uppercase tracking-wider mb-3">
        OAuth Clients
      </h2>
      <p className="text-xs text-neutral-600 mb-3">
        Configure OAuth2 credentials for notification sources. These are shared across all agents.
      </p>

      {oauth.clients.map((client) => (
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
                onClick={() => oauth.toggleEdit(client.provider)}
                className="px-2 py-1 bg-neutral-800 hover:bg-neutral-700 rounded text-xs"
              >
                {oauth.editProvider === client.provider
                  ? "Cancel"
                  : client.configured
                    ? "Update"
                    : "Configure"}
              </button>
              {client.configured && (
                <button
                  onClick={() => oauth.remove(client.provider)}
                  className="text-neutral-600 hover:text-red-400 text-sm"
                >
                  &times;
                </button>
              )}
            </div>
          </div>

          {oauth.editProvider === client.provider && (
            <div className="mt-3 space-y-2 border-t border-neutral-800 pt-3">
              <input
                type="text"
                value={oauth.clientId}
                onChange={(e) => oauth.setClientId(e.target.value)}
                placeholder="Client ID"
                className="w-full px-3 py-2 bg-neutral-800 border border-neutral-700 rounded text-xs focus:outline-none focus:border-neutral-500"
              />
              <input
                type="password"
                value={oauth.clientSecret}
                onChange={(e) => oauth.setClientSecret(e.target.value)}
                placeholder="Client Secret"
                className="w-full px-3 py-2 bg-neutral-800 border border-neutral-700 rounded text-xs focus:outline-none focus:border-neutral-500"
              />
              <button
                onClick={() => oauth.save(client.provider)}
                disabled={oauth.saving || !oauth.clientId.trim() || !oauth.clientSecret.trim()}
                className="w-full py-2 bg-neutral-700 hover:bg-neutral-600 rounded text-xs font-medium disabled:opacity-40"
              >
                {oauth.saving ? "Saving..." : "Save"}
              </button>
            </div>
          )}
        </div>
      ))}
    </div>
  );
}

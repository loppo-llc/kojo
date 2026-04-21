import { useCallback, useState } from "react";
import { useNavigate } from "react-router";
import { ApiKeysSection } from "./globalsettings/ApiKeysSection";
import { ChatPreferencesSection } from "./globalsettings/ChatPreferencesSection";
import { OAuthClientsSection } from "./globalsettings/OAuthClientsSection";
import { useEmbeddingModel } from "./globalsettings/useEmbeddingModel";
import { useGeminiApiKey } from "./globalsettings/useGeminiApiKey";
import { useOAuthClients } from "./globalsettings/useOAuthClients";
import { useEnterSends } from "../lib/preferences";

// How long the "Saved" banner stays visible after a successful mutation.
const SUCCESS_BANNER_MS = 2000;

export function GlobalSettings() {
  const navigate = useNavigate();
  const [error, setError] = useState("");
  const [success, setSuccess] = useState(false);
  const [enterSends, setEnterSends] = useEnterSends();

  const flashSuccess = useCallback(() => {
    setSuccess(true);
    setTimeout(() => setSuccess(false), SUCCESS_BANNER_MS);
  }, []);

  // The Gemini hook owns API key lifecycle; the Embedding hook watches its
  // `configured` flag and `saveToken` so the model list re-fetches on
  // initial load *and* after a subsequent save.
  const gemini = useGeminiApiKey(setError, flashSuccess);
  const embedding = useEmbeddingModel(
    gemini.configured,
    gemini.saveToken,
    gemini.initialEmbeddingModel,
    setError,
    flashSuccess,
  );
  const oauth = useOAuthClients(setError, flashSuccess);

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
        <ApiKeysSection gemini={gemini} embedding={embedding} />
        <OAuthClientsSection oauth={oauth} />
        <ChatPreferencesSection enterSends={enterSends} setEnterSends={setEnterSends} />

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

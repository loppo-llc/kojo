import { useCallback, useState } from "react";
import { useNavigate } from "react-router";
import { ApiKeysSection } from "./globalsettings/ApiKeysSection";
import { ArchivedAgentsSection } from "./globalsettings/ArchivedAgentsSection";
import { ChatPreferencesSection } from "./globalsettings/ChatPreferencesSection";
import { PeersSection } from "./globalsettings/PeersSection";
import { useEmbeddingModel } from "./globalsettings/useEmbeddingModel";
import { useGeminiApiKey } from "./globalsettings/useGeminiApiKey";
import { useEnterSends } from "../lib/preferences";
import { PageHeader } from "./ui/PageHeader";
import { Banner } from "./ui/Banner";

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

  return (
    <div className="min-h-full bg-app text-ink">
      <PageHeader title="Settings" onBack={() => navigate("/")} />

      <main className="mx-auto max-w-[560px] space-y-6 px-4 py-6">
        <ApiKeysSection gemini={gemini} embedding={embedding} />
        <ChatPreferencesSection enterSends={enterSends} setEnterSends={setEnterSends} />
        <PeersSection setError={setError} flashSuccess={flashSuccess} />
        <ArchivedAgentsSection setError={setError} flashSuccess={flashSuccess} />

        {error && <Banner tone="error">{error}</Banner>}
        {success && <Banner tone="success">Saved</Banner>}
      </main>
    </div>
  );
}

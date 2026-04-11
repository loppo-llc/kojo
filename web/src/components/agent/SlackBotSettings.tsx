import { useEffect, useState } from "react";
import { agentApi, type SlackBotStatus } from "../../lib/agentApi";

export function SlackBotSettings({ agentId }: { agentId: string }) {
  const [status, setStatus] = useState<SlackBotStatus | null>(null);
  const [enabled, setEnabled] = useState(false);
  const [appToken, setAppToken] = useState("");
  const [botToken, setBotToken] = useState("");
  const [threadReplies, setThreadReplies] = useState(true);
  const [respondDM, setRespondDM] = useState(true);
  const [respondMention, setRespondMention] = useState(true);
  const [respondThread, setRespondThread] = useState(true);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [error, setError] = useState("");
  const [testResult, setTestResult] = useState("");

  useEffect(() => {
    agentApi.slackBot.get(agentId).then((s) => {
      setStatus(s);
      setEnabled(s.enabled);
      setThreadReplies(s.threadReplies);
      setRespondDM(s.respondDM);
      setRespondMention(s.respondMention);
      setRespondThread(s.respondThread);
    }).catch(() => {});
  }, [agentId]);

  const handleSave = async () => {
    setSaving(true);
    setError("");
    try {
      await agentApi.slackBot.set(agentId, {
        enabled,
        ...(appToken ? { appToken } : {}),
        ...(botToken ? { botToken } : {}),
        threadReplies,
        respondDM,
        respondMention,
        respondThread,
      });
      setAppToken("");
      setBotToken("");
      const s = await agentApi.slackBot.get(agentId);
      setStatus(s);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const handleTest = async () => {
    setTesting(true);
    setTestResult("");
    setError("");
    try {
      const res = await agentApi.slackBot.test(agentId, {
        ...(appToken ? { appToken } : {}),
        ...(botToken ? { botToken } : {}),
      });
      setTestResult(`Connected: team=${res.team}, bot=${res.botUser}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setTesting(false);
    }
  };

  const handleDelete = async () => {
    if (!confirm("Remove Slack bot configuration?")) return;
    setSaving(true);
    setError("");
    try {
      await agentApi.slackBot.delete(agentId);
      const s = await agentApi.slackBot.get(agentId);
      setStatus(s);
      setEnabled(false);
      setAppToken("");
      setBotToken("");
      setTestResult("");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div>
      {/* Header with toggle */}
      <div className="flex items-center justify-between">
        <h2 className="text-sm font-semibold text-neutral-300">Slack Bot</h2>
        <div className="flex items-center gap-2">
          {status?.connected && (
            <span className="text-xs text-green-400">Connected</span>
          )}
          <button
            type="button"
            role="switch"
            aria-checked={enabled}
            onClick={() => setEnabled(!enabled)}
            className={`relative inline-flex h-6 w-11 shrink-0 cursor-pointer rounded-full transition-colors duration-200 ${
              enabled ? "bg-green-600" : "bg-neutral-700"
            }`}
          >
            <span
              className={`pointer-events-none inline-block h-5 w-5 transform rounded-full bg-white shadow ring-0 transition-transform duration-200 mt-0.5 ${
                enabled ? "translate-x-[22px]" : "translate-x-0.5"
              }`}
            />
          </button>
        </div>
      </div>

      {/* Collapsible settings */}
      {enabled && (
        <div className="mt-4 space-y-3">
          {/* App Token */}
          <div>
            <label className="block text-xs text-neutral-500 mb-1">
              App-Level Token (xapp-...)
              {status?.hasAppToken && !appToken && (
                <span className="text-green-500 ml-1">configured</span>
              )}
            </label>
            <input
              type="password"
              value={appToken}
              onChange={(e) => setAppToken(e.target.value)}
              placeholder={status?.hasAppToken ? "••••••••" : "xapp-..."}
              className="w-full px-3 py-1.5 bg-neutral-900 border border-neutral-700 rounded text-xs font-mono focus:outline-none focus:border-neutral-500"
            />
          </div>

          {/* Bot Token */}
          <div>
            <label className="block text-xs text-neutral-500 mb-1">
              Bot Token (xoxb-...)
              {status?.hasBotToken && !botToken && (
                <span className="text-green-500 ml-1">configured</span>
              )}
            </label>
            <input
              type="password"
              value={botToken}
              onChange={(e) => setBotToken(e.target.value)}
              placeholder={status?.hasBotToken ? "••••••••" : "xoxb-..."}
              className="w-full px-3 py-1.5 bg-neutral-900 border border-neutral-700 rounded text-xs font-mono focus:outline-none focus:border-neutral-500"
            />
          </div>

          {/* Thread replies toggle */}
          <label className="flex items-center gap-2 cursor-pointer">
            <input
              type="checkbox"
              checked={threadReplies}
              onChange={(e) => setThreadReplies(e.target.checked)}
              className="rounded border-neutral-600"
            />
            <span className="text-xs text-neutral-400">Always reply in thread</span>
          </label>

          {/* Reaction patterns */}
          <div>
            <div className="text-xs text-neutral-500 mb-2">Respond to:</div>
            <div className="space-y-1.5 ml-1">
              <label className="flex items-center gap-2 cursor-pointer">
                <input
                  type="checkbox"
                  checked={respondDM}
                  onChange={(e) => setRespondDM(e.target.checked)}
                  className="rounded border-neutral-600"
                />
                <span className="text-xs text-neutral-400">Direct messages</span>
              </label>
              <label className="flex items-center gap-2 cursor-pointer">
                <input
                  type="checkbox"
                  checked={respondMention}
                  onChange={(e) => setRespondMention(e.target.checked)}
                  className="rounded border-neutral-600"
                />
                <span className="text-xs text-neutral-400">@mentions in channels</span>
              </label>
              <label className="flex items-center gap-2 cursor-pointer">
                <input
                  type="checkbox"
                  checked={respondThread}
                  onChange={(e) => setRespondThread(e.target.checked)}
                  className="rounded border-neutral-600"
                />
                <span className="text-xs text-neutral-400">Thread follow-ups (auto-reply without mention)</span>
              </label>
            </div>
          </div>

          {error && (
            <div className="p-2 bg-red-950 border border-red-800 rounded text-xs text-red-300">
              {error}
            </div>
          )}
          {testResult && (
            <div className="p-2 bg-green-950 border border-green-800 rounded text-xs text-green-300">
              {testResult}
            </div>
          )}

          <div className="flex gap-2">
            <button
              onClick={handleSave}
              disabled={saving}
              className="flex-1 py-2 bg-neutral-800 hover:bg-neutral-700 rounded text-xs font-medium disabled:opacity-40"
            >
              {saving ? "Saving..." : "Save"}
            </button>
            <button
              onClick={handleTest}
              disabled={testing || (!status?.hasAppToken && !appToken)}
              className="px-4 py-2 bg-neutral-800 hover:bg-neutral-700 rounded text-xs font-medium disabled:opacity-40"
            >
              {testing ? "Testing..." : "Test Connection"}
            </button>
          </div>

          {(status?.hasAppToken || status?.hasBotToken) && (
            <button
              onClick={handleDelete}
              disabled={saving}
              className="w-full py-2 bg-red-950/50 hover:bg-red-950 border border-red-900 rounded text-xs text-red-400 disabled:opacity-40"
            >
              Remove Slack Bot
            </button>
          )}

          <p className="text-xs text-neutral-600">
            Create a Slack App with Socket Mode enabled. Required scopes: chat:write, app_mentions:read, im:history.
            Subscribe to events: message.im, app_mention.
          </p>
        </div>
      )}
    </div>
  );
}

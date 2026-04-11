import { useEffect, useRef, useState } from "react";
import { useParams, useNavigate } from "react-router";
import { agentApi, type AgentInfo } from "../../lib/agentApi";
import { api } from "../../lib/api";
import { AgentAvatar } from "./AgentAvatar";
import { ScheduleEditor } from "./ScheduleEditor";
import { NotifySourcesEditor } from "./NotifySourcesEditor";
import { SlackBotSettings } from "./SlackBotSettings";
import { defaultModelForTool, modelsForTool, effortLevels, supportsEffort } from "../../lib/toolModels";

export function AgentSettings() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [agent, setAgent] = useState<AgentInfo | null>(null);
  const [name, setName] = useState("");
  const [persona, setPersona] = useState("");
  const [publicProfile, setPublicProfile] = useState("");
  const [publicProfileOverride, setPublicProfileOverride] = useState(false);
  const [model, setModel] = useState("");
  const [effort, setEffort] = useState("");
  const [tool, setTool] = useState("");
  const [workDir, setWorkDir] = useState("");
  const [intervalMinutes, setIntervalMinutes] = useState(30);
  const [timeoutMinutes, setTimeoutMinutes] = useState(10);
  const [activeStart, setActiveStart] = useState("");
  const [activeEnd, setActiveEnd] = useState("");
  const [saving, setSaving] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [resetting, setResetting] = useState(false);
  const [resettingSession, setResettingSession] = useState(false);
  const [error, setError] = useState("");
  const [success, setSuccess] = useState(false);
  const [avatarToken, setAvatarToken] = useState(() => Date.now());
  const [generatingAvatar, setGeneratingAvatar] = useState(false);
  const [personaPrompt, setPersonaPrompt] = useState("");
  const [generatingPersona, setGeneratingPersona] = useState(false);
  const [lmsModels, setLmsModels] = useState<string[]>([]);
  const [allowedTools, setAllowedTools] = useState<string[]>([]);
  const fileRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    api.info().then((info) => {
      if (info.lmStudioModels?.length) setLmsModels(info.lmStudioModels);
    }).catch(() => {});
  }, []);

  useEffect(() => {
    if (!id) return;
    agentApi.get(id).then((a) => {
      setAgent(a);
      setName(a.name);
      setPersona(a.persona);
      setPublicProfile(a.publicProfile ?? "");
      setPublicProfileOverride(a.publicProfileOverride ?? false);
      setModel(a.model);
      setEffort(a.effort || "");
      setTool(a.tool);
      setWorkDir(a.workDir ?? "");
      setIntervalMinutes(a.intervalMinutes);
      setTimeoutMinutes(a.timeoutMinutes || 10);
      setActiveStart(a.activeStart ?? "");
      setActiveEnd(a.activeEnd ?? "");
      setAllowedTools(a.allowedTools ?? []);
    }).catch(() => navigate("/"));
  }, [id, navigate]);

  const handleSave = async () => {
    setSaving(true);
    setError("");
    setSuccess(false);
    try {
      const updated = await agentApi.update(id!, {
        name: name.trim(),
        persona: persona.trim(),
        ...(publicProfileOverride ? { publicProfile: publicProfile.trim() } : {}),
        publicProfileOverride,
        model: model.trim(),
        effort: supportsEffort(tool) ? effort : undefined,
        tool: tool.trim(),
        workDir: workDir.trim(),
        intervalMinutes,
        timeoutMinutes,
        activeStart,
        activeEnd,
        allowedTools: tool === "lm-studio" ? allowedTools : undefined,
      });
      setAgent(updated);
      setPublicProfile(updated.publicProfile ?? "");
      setPublicProfileOverride(updated.publicProfileOverride ?? false);
      setSuccess(true);
      setTimeout(() => setSuccess(false), 2000);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const handleResetSession = async () => {
    if (!confirm("Reset CLI session? Conversation history and memory are kept, but the AI will start a fresh context window.")) return;
    setResettingSession(true);
    setError("");
    try {
      await agentApi.resetSession(id!);
      setSuccess(true);
      setTimeout(() => setSuccess(false), 2000);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setResettingSession(false);
    }
  };

  const handleResetData = async () => {
    if (!confirm("Reset conversation logs and memory? Settings, persona, avatar, and credentials will be kept.")) return;
    setResetting(true);
    setError("");
    try {
      await agentApi.resetData(id!);
      setSuccess(true);
      setTimeout(() => setSuccess(false), 2000);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setResetting(false);
    }
  };

  const handleDelete = async () => {
    if (!confirm("Delete this agent? This cannot be undone.")) return;
    setDeleting(true);
    try {
      await agentApi.delete(id!);
      navigate("/", { replace: true });
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setDeleting(false);
    }
  };

  const handleAvatarUpload = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file || !id) return;
    try {
      await agentApi.uploadAvatar(id, file);
      setAvatarToken(Date.now());
      setAgent((a) => (a ? { ...a, hasAvatar: true } : a));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const handleGeneratePersona = async () => {
    if (!personaPrompt.trim()) return;
    setGeneratingPersona(true);
    setError("");
    try {
      const result = await agentApi.generatePersona(persona.trim(), personaPrompt.trim());
      setPersona(result.persona);
      setPersonaPrompt("");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setGeneratingPersona(false);
    }
  };

  const handleGenerateAvatar = async () => {
    if (!id || !persona.trim()) return;
    setGeneratingAvatar(true);
    setError("");
    try {
      const result = await agentApi.generateAvatar(persona.trim(), name.trim());
      await agentApi.uploadGeneratedAvatar(id, result.avatarPath);
      setAvatarToken(Date.now());
      setAgent((a) => (a ? { ...a, hasAvatar: true } : a));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setGeneratingAvatar(false);
    }
  };

  if (!agent) return null;

  return (
    <div className="min-h-full bg-neutral-950 text-neutral-200">
      <header className="flex items-center gap-2 px-4 py-3 border-b border-neutral-800">
        <button
          onClick={() => navigate(`/agents/${id}`)}
          className="text-neutral-400 hover:text-neutral-200"
        >
          &larr;
        </button>
        <h1 className="text-lg font-bold">Settings</h1>
      </header>

      <main className="p-4 space-y-6 max-w-md mx-auto">
        {/* ── Agent Settings ── */}
        <section className="rounded-xl border border-neutral-800 p-5 space-y-5">
          <h2 className="text-sm font-semibold text-neutral-300">Agent</h2>

        {/* Avatar */}
        <div className="flex items-center gap-4">
          <AgentAvatar agentId={agent.id} name={agent.name} size="xl" cacheBust={avatarToken} />
          <div className="flex gap-2">
            <button
              onClick={() => fileRef.current?.click()}
              className="px-3 py-1.5 bg-neutral-800 hover:bg-neutral-700 rounded text-sm"
            >
              Change Avatar
            </button>
            <button
              onClick={handleGenerateAvatar}
              disabled={generatingAvatar || !persona.trim()}
              className="px-3 py-1.5 bg-neutral-800 hover:bg-neutral-700 rounded text-sm disabled:opacity-40 flex items-center gap-1.5"
            >
              {generatingAvatar ? (
                <><span className="animate-spin">↻</span> Generating...</>
              ) : (
                <>✨ Generate</>
              )}
            </button>
            <input
              ref={fileRef}
              type="file"
              accept="image/*"
              onChange={handleAvatarUpload}
              className="hidden"
            />
          </div>
        </div>

        {/* Name */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">Name</label>
          <input
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
          />
        </div>

        {/* Persona */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">
            Persona
          </label>
          <textarea
            value={persona}
            onChange={(e) => setPersona(e.target.value)}
            rows={6}
            className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm resize-none focus:outline-none focus:border-neutral-500"
          />
          <div className="flex gap-2 mt-2">
            <input
              type="text"
              value={personaPrompt}
              onChange={(e) => setPersonaPrompt(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !e.shiftKey && !generatingPersona) {
                  e.preventDefault();
                  handleGeneratePersona();
                }
              }}
              placeholder="e.g. もっと毒舌にして"
              className="flex-1 px-3 py-1.5 bg-neutral-900 border border-neutral-700 rounded text-xs focus:outline-none focus:border-neutral-500"
            />
            <button
              onClick={handleGeneratePersona}
              disabled={generatingPersona || !personaPrompt.trim()}
              className="px-3 py-1.5 bg-neutral-800 hover:bg-neutral-700 rounded text-xs disabled:opacity-40 flex items-center gap-1"
            >
              {generatingPersona ? (
                <span className="animate-spin">↻</span>
              ) : (
                "✨ AI"
              )}
            </button>
          </div>
        </div>

        {/* Public Profile */}
        <div>
          <div className="flex items-center justify-between mb-2">
            <label className="text-sm text-neutral-400">
              Public Profile
            </label>
            <label className="flex items-center gap-1.5 text-xs text-neutral-500 cursor-pointer">
              <input
                type="checkbox"
                checked={publicProfileOverride}
                onChange={(e) => setPublicProfileOverride(e.target.checked)}
                className="rounded border-neutral-600"
              />
              Override
            </label>
          </div>
          <textarea
            value={publicProfile}
            onChange={(e) => setPublicProfile(e.target.value)}
            rows={2}
            disabled={!publicProfileOverride}
            placeholder={publicProfileOverride ? "Enter custom public profile" : "Auto-generated from persona"}
            className={`w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm resize-none focus:outline-none focus:border-neutral-500 ${
              !publicProfileOverride ? "opacity-60 cursor-not-allowed" : ""
            }`}
          />
          <p className="mt-1 text-xs text-neutral-600">
            {publicProfileOverride
              ? "Manual override — won't be replaced when persona changes."
              : "Auto-generated from persona. Visible to other agents via directory."}
          </p>
        </div>

        {/* Model */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">Model</label>
          {(() => {
            const models = tool === "lm-studio" ? lmsModels : modelsForTool(tool);
            return models.length > 0 ? (
              <select
                value={model}
                onChange={(e) => setModel(e.target.value)}
                className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
              >
                {model && !models.includes(model) && (
                  <option value={model}>{model}</option>
                )}
                {models.map((m) => (
                  <option key={m} value={m}>
                    {m}
                  </option>
                ))}
              </select>
            ) : (
              <input
                type="text"
                value={model}
                onChange={(e) => setModel(e.target.value)}
                placeholder="model name"
                className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
              />
            );
          })()}
        </div>

        {/* Effort (claude only) */}
        {supportsEffort(tool) && (
          <div>
            <label className="block text-sm text-neutral-400 mb-2">Effort</label>
            <select
              value={effort}
              onChange={(e) => setEffort(e.target.value)}
              className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
            >
              <option value="">default (high)</option>
              {effortLevels.map((e) => (
                <option key={e} value={e}>
                  {e}
                </option>
              ))}
            </select>
          </div>
        )}

        {/* Tool */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">Tool</label>
          <div className="flex gap-2">
            {["claude", "codex", "gemini", "lm-studio"].map((t) => (
              <button
                key={t}
                onClick={() => {
                  if (t !== tool) {
                    setTool(t);
                    if (t === "lm-studio") {
                      setModel(lmsModels[0] ?? "");
                    } else {
                      setModel(defaultModelForTool(t));
                    }
                  }
                }}
                className={`flex-1 px-3 py-2 rounded text-sm font-mono ${
                  tool === t
                    ? "bg-neutral-700 border border-neutral-500"
                    : "bg-neutral-900 border border-neutral-800"
                }`}
              >
                {t}
              </button>
            ))}
          </div>
        </div>

        {/* Allowed Tools (lm-studio only) */}
        {tool === "lm-studio" && (
          <div>
            <label className="block text-sm text-neutral-400 mb-2">
              Allowed Tools
              <span className="text-xs text-neutral-600 ml-2">(empty = all)</span>
            </label>
            <div className="grid grid-cols-2 gap-1.5">
              {["Bash", "Read", "Write", "Edit", "Glob", "Grep", "Skill", "WebFetch", "WebSearch", "Agent", "NotebookEdit"].map((t) => (
                <label key={t} className="flex items-center gap-2 px-2 py-1.5 bg-neutral-900 rounded text-xs font-mono cursor-pointer hover:bg-neutral-800">
                  <input
                    type="checkbox"
                    checked={allowedTools.length === 0 || allowedTools.includes(t)}
                    onChange={(e) => {
                      if (allowedTools.length === 0) {
                        // Switching from "all" to explicit: start with all checked except this one
                        if (!e.target.checked) {
                          setAllowedTools(["Bash", "Read", "Write", "Edit", "Glob", "Grep", "Skill", "WebFetch", "WebSearch", "Agent", "NotebookEdit"].filter((x) => x !== t));
                        }
                      } else {
                        if (e.target.checked) {
                          setAllowedTools([...allowedTools, t]);
                        } else {
                          setAllowedTools(allowedTools.filter((x) => x !== t));
                        }
                      }
                    }}
                    className="accent-neutral-400"
                  />
                  {t}
                </label>
              ))}
            </div>
          </div>
        )}

        {/* File Storage */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">File Storage</label>
          <input
            type="text"
            value={workDir}
            onChange={(e) => setWorkDir(e.target.value)}
            placeholder="(default: agent data dir)"
            className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm font-mono focus:outline-none focus:border-neutral-500"
          />
          <p className="text-xs text-neutral-600 mt-1">Generated files are saved here.</p>
        </div>

        {/* Schedule */}
        <ScheduleEditor
          intervalMinutes={intervalMinutes}
          onIntervalChange={setIntervalMinutes}
          timeoutMinutes={timeoutMinutes}
          onTimeoutChange={setTimeoutMinutes}
          activeStart={activeStart}
          activeEnd={activeEnd}
          onActiveStartChange={setActiveStart}
          onActiveEndChange={setActiveEnd}
        />

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

        <button
          onClick={handleSave}
          disabled={saving}
          className="w-full py-3 bg-neutral-800 hover:bg-neutral-700 rounded-lg text-sm font-medium disabled:opacity-40"
        >
          {saving ? "Saving..." : "Save Changes"}
        </button>
        </section>

        {/* ── Notifications ── */}
        <section className="rounded-xl border border-neutral-800 p-5">
          <NotifySourcesEditor agentId={id!} />
        </section>

        {/* ── Slack Bot ── */}
        <section className="rounded-xl border border-neutral-800 p-5">
          <SlackBotSettings agentId={id!} />
        </section>

        {/* ── Credentials ── */}
        <section className="rounded-xl border border-neutral-800 p-5">
          <button
            onClick={() => navigate(`/agents/${id}/credentials`)}
            className="w-full py-3 bg-neutral-800 hover:bg-neutral-700 rounded-lg text-sm font-medium"
          >
            Manage Credentials
          </button>
        </section>

        {/* ── Danger Zone ── */}
        <section className="rounded-xl border border-red-900/30 bg-red-950/10 p-5 space-y-4">
          <h2 className="text-sm font-semibold text-red-400/80">Danger Zone</h2>
          <div>
            <button
              onClick={handleResetSession}
              disabled={resettingSession}
              className="w-full py-3 bg-neutral-800 hover:bg-neutral-700 border border-neutral-700 rounded-lg text-sm font-medium disabled:opacity-40"
            >
              {resettingSession ? "Resetting..." : "Reset CLI Session"}
            </button>
            <p className="text-xs text-neutral-600 mt-1">
              Force a fresh context window. History and memory are kept, but the AI re-reads everything from scratch.
            </p>
          </div>
          <div>
            <button
              onClick={handleResetData}
              disabled={resetting}
              className="w-full py-3 bg-amber-950 hover:bg-amber-900 border border-amber-800 rounded-lg text-sm font-medium text-amber-300 disabled:opacity-40"
            >
              {resetting ? "Resetting..." : "Reset Data"}
            </button>
            <p className="text-xs text-neutral-600 mt-1">
              Clear conversation logs and memory. Settings, persona, avatar, and credentials are kept.
            </p>
          </div>
          <button
            onClick={handleDelete}
            disabled={deleting}
            className="w-full py-3 bg-red-950 hover:bg-red-900 border border-red-800 rounded-lg text-sm font-medium text-red-300 disabled:opacity-40"
          >
            {deleting ? "Deleting..." : "Delete Agent"}
          </button>
        </section>

        {/* Info */}
        <div className="text-xs text-neutral-600 space-y-1">
          <div>ID: {agent.id}</div>
          <div>Created: {new Date(agent.createdAt).toLocaleString()}</div>
        </div>
      </main>
    </div>
  );
}

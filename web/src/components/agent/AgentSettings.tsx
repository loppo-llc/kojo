import { useEffect, useRef, useState } from "react";
import { useParams, useNavigate } from "react-router";
import { agentApi, type AgentInfo } from "../../lib/agentApi";
import { AgentAvatar } from "./AgentAvatar";

export function AgentSettings() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [agent, setAgent] = useState<AgentInfo | null>(null);
  const [name, setName] = useState("");
  const [persona, setPersona] = useState("");
  const [model, setModel] = useState("");
  const [tool, setTool] = useState("");
  const [cronExpr, setCronExpr] = useState("");
  const [saving, setSaving] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [error, setError] = useState("");
  const [success, setSuccess] = useState(false);
  const [avatarToken, setAvatarToken] = useState(() => Date.now());
  const [generatingAvatar, setGeneratingAvatar] = useState(false);
  const [personaPrompt, setPersonaPrompt] = useState("");
  const [generatingPersona, setGeneratingPersona] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (!id) return;
    agentApi.get(id).then((a) => {
      setAgent(a);
      setName(a.name);
      setPersona(a.persona);
      setModel(a.model);
      setTool(a.tool);
      setCronExpr(a.cronExpr);
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
        model: model.trim(),
        tool: tool.trim(),
        cronExpr: cronExpr.trim(),
      });
      setAgent(updated);
      setSuccess(true);
      setTimeout(() => setSuccess(false), 2000);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
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

      <main className="p-4 space-y-5 max-w-md mx-auto">
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

        {/* Model */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">Model</label>
          <input
            type="text"
            value={model}
            onChange={(e) => setModel(e.target.value)}
            className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
          />
        </div>

        {/* Tool */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">Tool</label>
          <div className="flex gap-2">
            {["claude", "codex", "gemini"].map((t) => (
              <button
                key={t}
                onClick={() => setTool(t)}
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

        {/* Cron */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">
            Cron Schedule
          </label>
          <input
            type="text"
            value={cronExpr}
            onChange={(e) => setCronExpr(e.target.value)}
            placeholder="0 9 * * *"
            className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
          />
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

        <button
          onClick={handleSave}
          disabled={saving}
          className="w-full py-3 bg-neutral-800 hover:bg-neutral-700 rounded-lg text-sm font-medium disabled:opacity-40"
        >
          {saving ? "Saving..." : "Save Changes"}
        </button>

        <div className="border-t border-neutral-800 pt-5">
          <button
            onClick={handleDelete}
            disabled={deleting}
            className="w-full py-3 bg-red-950 hover:bg-red-900 border border-red-800 rounded-lg text-sm font-medium text-red-300 disabled:opacity-40"
          >
            {deleting ? "Deleting..." : "Delete Agent"}
          </button>
        </div>

        {/* Info */}
        <div className="text-xs text-neutral-600 space-y-1">
          <div>ID: {agent.id}</div>
          <div>Created: {new Date(agent.createdAt).toLocaleString()}</div>
        </div>
      </main>
    </div>
  );
}

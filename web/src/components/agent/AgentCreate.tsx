import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router";
import { agentApi } from "../../lib/agentApi";
import { ScheduleEditor } from "./ScheduleEditor";
import { api, type ServerInfo } from "../../lib/api";
import { defaultModelForTool, modelsForTool, effortLevelsForModel, defaultEffortForModel, supportsEffort, type EffortLevel } from "../../lib/toolModels";

type GenPhase = "idle" | "persona" | "name" | "avatar" | "all-name" | "all-avatar";

export function AgentCreate() {
  const navigate = useNavigate();
  const [info, setInfo] = useState<ServerInfo>();
  const [name, setName] = useState("");
  const [persona, setPersona] = useState("");
  const [model, setModel] = useState("sonnet");
  const [effort, setEffort] = useState("");
  const [tool, setTool] = useState("claude");
  const [customBaseURL, setCustomBaseURL] = useState("http://localhost:8080");
  const [customModels, setCustomModels] = useState<string[]>([]);
  const [thinkingMode, setThinkingMode] = useState("");
  const [workDir, setWorkDir] = useState("");
  const [intervalMinutes, setIntervalMinutes] = useState(30);
  const [timeoutMinutes, setTimeoutMinutes] = useState(10);
  const [resumeIdleMinutes, setResumeIdleMinutes] = useState(0);
  const [silentStart, setSilentStart] = useState("");
  const [silentEnd, setSilentEnd] = useState("");
  const [genPrompt, setGenPrompt] = useState("");
  const [personaPrompt, setPersonaPrompt] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  // Avatar state
  const [avatarTempPath, setAvatarTempPath] = useState("");
  const [avatarPreviewUrl, setAvatarPreviewUrl] = useState("");
  const [avatarFile, setAvatarFile] = useState<File | null>(null);
  const fileRef = useRef<HTMLInputElement>(null);
  const blobUrlRef = useRef("");

  // Generation state — single discriminated phase
  const [genPhase, setGenPhase] = useState<GenPhase>("idle");

  const isGenerating = genPhase !== "idle";

  useEffect(() => {
    api.info().then(setInfo).catch(console.error);
  }, []);

  const needsCustomURL = tool === "custom" || tool === "llama.cpp";

  // Fetch models when custom/llama.cpp tool is selected or base URL changes
  useEffect(() => {
    if (!needsCustomURL) return;
    let cancelled = false;
    const timer = setTimeout(() => {
      api.customModels(customBaseURL).then((models) => {
        if (cancelled) return;
        setCustomModels(models);
        if (models.length > 0) {
          setModel((prev) => models.includes(prev) ? prev : models[0]);
        }
      }).catch(() => { if (!cancelled) setCustomModels([]); });
    }, 300);
    return () => { cancelled = true; clearTimeout(timer); };
  }, [needsCustomURL, customBaseURL]);

  // Revoke blob URL on unmount
  useEffect(() => {
    return () => {
      if (blobUrlRef.current) URL.revokeObjectURL(blobUrlRef.current);
    };
  }, []);

  const revokePreview = () => {
    if (blobUrlRef.current) {
      URL.revokeObjectURL(blobUrlRef.current);
      blobUrlRef.current = "";
    }
  };

  const handleGeneratePersona = async () => {
    if (!personaPrompt.trim()) {
      setError("Enter a prompt to generate persona");
      return;
    }
    setGenPhase("persona");
    setError("");
    try {
      const result = await agentApi.generatePersona(
        persona.trim(),
        personaPrompt.trim(),
      );
      setPersona(result.persona);
      setPersonaPrompt("");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setGenPhase("idle");
    }
  };

  const handleGenerateName = async () => {
    if (!persona.trim()) {
      setError("Write a persona description first");
      return;
    }
    setGenPhase("name");
    setError("");
    try {
      const result = await agentApi.generateName(
        persona.trim(),
        genPrompt.trim(),
      );
      setName(result.name);
      return result.name;
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      return null;
    } finally {
      setGenPhase("idle");
    }
  };

  const handleGenerateAvatar = async (nameOverride?: string) => {
    const n = nameOverride || name.trim();
    if (!persona.trim()) {
      setError("Write a persona description first");
      return;
    }
    setGenPhase("avatar");
    setError("");
    const hadTempPath = !!avatarTempPath;
    try {
      const result = await agentApi.generateAvatar(
        persona.trim(),
        n,
        genPrompt.trim(),
        avatarTempPath || undefined,
      );
      revokePreview();
      setAvatarTempPath(result.avatarPath);
      setAvatarPreviewUrl(agentApi.previewAvatarUrl(result.avatarPath));
      setAvatarFile(null);
    } catch (err) {
      // Only clear avatar state if server deleted the old temp path
      if (hadTempPath) {
        revokePreview();
        setAvatarTempPath("");
        setAvatarPreviewUrl("");
        setAvatarFile(null);
      }
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setGenPhase("idle");
    }
  };

  const handleGenerateAll = async () => {
    if (!persona.trim()) {
      setError("Write a persona description first");
      return;
    }
    setError("");

    // Name
    setGenPhase("all-name");
    let generatedName: string | null = null;
    try {
      const result = await agentApi.generateName(
        persona.trim(),
        genPrompt.trim(),
      );
      generatedName = result.name;
      setName(result.name);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setGenPhase("idle");
      return;
    }

    // Avatar
    setGenPhase("all-avatar");
    const hadTempPath = !!avatarTempPath;
    try {
      const result = await agentApi.generateAvatar(
        persona.trim(),
        generatedName,
        genPrompt.trim(),
        avatarTempPath || undefined,
      );
      revokePreview();
      setAvatarTempPath(result.avatarPath);
      setAvatarPreviewUrl(agentApi.previewAvatarUrl(result.avatarPath));
      setAvatarFile(null);
    } catch (err) {
      if (hadTempPath) {
        revokePreview();
        setAvatarTempPath("");
        setAvatarPreviewUrl("");
        setAvatarFile(null);
      }
      setError(err instanceof Error ? err.message : String(err));
    }
    setGenPhase("idle");
  };

  const handleAvatarClick = () => {
    if (isGenerating) return;
    fileRef.current?.click();
  };

  const handleAvatarFileChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    revokePreview();
    setAvatarFile(file);
    const url = URL.createObjectURL(file);
    blobUrlRef.current = url;
    setAvatarPreviewUrl(url);
    setAvatarTempPath("");
    e.target.value = "";
  };

  const handleCreate = async () => {
    if (!name.trim()) {
      setError("Name is required");
      return;
    }
    setLoading(true);
    setError("");
    try {
      const agent = await agentApi.create({
        name: name.trim(),
        persona: persona.trim(),
        model,
        effort: supportsEffort(tool) && effort ? effort : undefined,
        tool,
        customBaseURL: needsCustomURL ? customBaseURL : undefined,
        thinkingMode: tool === "llama.cpp" && thinkingMode ? thinkingMode : undefined,
        workDir: workDir.trim() || undefined,
        intervalMinutes,
        timeoutMinutes,
        resumeIdleMinutes: resumeIdleMinutes || undefined,
        silentStart: silentStart || undefined,
        silentEnd: silentEnd || undefined,
      });

      // Upload avatar (best-effort — agent is already created)
      try {
        if (avatarTempPath) {
          await agentApi.uploadGeneratedAvatar(agent.id, avatarTempPath);
        } else if (avatarFile) {
          await agentApi.uploadAvatar(agent.id, avatarFile);
        }
      } catch {
        // Avatar upload failed but agent was created — navigate anyway
      }

      navigate(`/agents/${agent.id}`, { replace: true });
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setLoading(false);
    }
  };

  const genStatusText =
    genPhase === "persona"
      ? "Generating persona..."
      : genPhase === "all-name" || genPhase === "name"
        ? "Generating name..."
        : genPhase === "all-avatar" || genPhase === "avatar"
          ? "Generating avatar..."
          : "";

  return (
    <div className="min-h-full bg-neutral-950 text-neutral-200">
      <header className="flex items-center gap-2 px-4 py-3 border-b border-neutral-800">
        <button
          onClick={() => navigate("/")}
          className="text-neutral-400 hover:text-neutral-200"
        >
          &larr;
        </button>
        <h1 className="text-lg font-bold">New Agent</h1>
      </header>

      <main className="p-4 space-y-5 max-w-md mx-auto">
        {/* Persona */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">
            Persona
          </label>
          <textarea
            value={persona}
            onChange={(e) => setPersona(e.target.value)}
            placeholder="Describe the agent's personality, speaking style, interests..."
            rows={5}
            className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm resize-none focus:outline-none focus:border-neutral-500"
          />
          <div className="flex gap-2 mt-2">
            <input
              type="text"
              value={personaPrompt}
              onChange={(e) => setPersonaPrompt(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !e.nativeEvent.isComposing && !e.shiftKey && !isGenerating) {
                  e.preventDefault();
                  handleGeneratePersona();
                }
              }}
              placeholder="e.g. ツンデレな女の子にして"
              className="flex-1 px-3 py-1.5 bg-neutral-900 border border-neutral-700 rounded text-xs focus:outline-none focus:border-neutral-500"
            />
            <button
              onClick={handleGeneratePersona}
              disabled={isGenerating || !personaPrompt.trim()}
              className="px-3 py-1.5 bg-neutral-800 hover:bg-neutral-700 rounded text-xs disabled:opacity-40 flex items-center gap-1"
            >
              {genPhase === "persona" ? (
                <span className="animate-spin">↻</span>
              ) : (
                "✨ AI"
              )}
            </button>
          </div>
        </div>

        {/* Avatar + Name + Hint */}
        <div className="flex gap-4">
          {/* Avatar preview */}
          <div className="relative shrink-0">
            <button
              type="button"
              onClick={handleAvatarClick}
              disabled={isGenerating}
              className="w-24 h-24 rounded-full bg-neutral-800 border border-neutral-700 overflow-hidden flex items-center justify-center hover:border-neutral-500 transition-colors disabled:opacity-60 disabled:cursor-not-allowed"
              title="Click to upload avatar"
            >
              {avatarPreviewUrl ? (
                <img
                  src={avatarPreviewUrl}
                  alt="Avatar"
                  className="w-full h-full object-cover"
                />
              ) : (
                <svg
                  className="w-7 h-7 text-neutral-600"
                  fill="none"
                  viewBox="0 0 24 24"
                  stroke="currentColor"
                  strokeWidth={1.5}
                >
                  <path
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    d="M15.75 6a3.75 3.75 0 1 1-7.5 0 3.75 3.75 0 0 1 7.5 0ZM4.501 20.118a7.5 7.5 0 0 1 14.998 0A17.933 17.933 0 0 1 12 21.75c-2.676 0-5.216-.584-7.499-1.632Z"
                  />
                </svg>
              )}
            </button>
            {/* Re-generate avatar only */}
            {avatarPreviewUrl && (
              <button
                type="button"
                onClick={() => handleGenerateAvatar()}
                disabled={isGenerating || !persona.trim()}
                className="absolute -bottom-1 -right-1 w-6 h-6 rounded-full bg-neutral-700 hover:bg-neutral-600 border border-neutral-600 flex items-center justify-center text-xs disabled:opacity-40"
                title="Regenerate avatar"
              >
                {genPhase === "avatar" ? (
                  <span className="animate-spin">↻</span>
                ) : (
                  "✨"
                )}
              </button>
            )}
            <input
              ref={fileRef}
              type="file"
              accept="image/*"
              onChange={handleAvatarFileChange}
              className="hidden"
            />
          </div>

          {/* Name + Hint */}
          <div className="flex-1 space-y-2">
            <div className="flex gap-2">
              <input
                type="text"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="Name"
                className="flex-1 px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
              />
              <button
                onClick={() => handleGenerateName()}
                disabled={isGenerating || !persona.trim()}
                className="px-2.5 py-2 bg-neutral-800 hover:bg-neutral-700 rounded text-sm disabled:opacity-40"
                title="Generate name from persona"
              >
                {genPhase === "name" ? (
                  <span className="animate-spin inline-block">↻</span>
                ) : (
                  "✨"
                )}
              </button>
            </div>
            <input
              type="text"
              value={genPrompt}
              onChange={(e) => setGenPrompt(e.target.value)}
              placeholder="Generation hint (optional)"
              className="w-full px-3 py-1.5 bg-neutral-900 border border-neutral-700 rounded text-xs focus:outline-none focus:border-neutral-500"
            />
          </div>
        </div>

        {/* Generate All / Avatar only */}
        <div className="flex gap-2">
          <button
            onClick={handleGenerateAll}
            disabled={isGenerating || !persona.trim()}
            className="flex-1 py-2.5 bg-neutral-800 hover:bg-neutral-700 border border-neutral-700 rounded-lg text-sm font-medium disabled:opacity-40 flex items-center justify-center gap-2"
          >
            {genPhase.startsWith("all-") ? (
              <>
                <span className="animate-spin">↻</span>
                {genStatusText}
              </>
            ) : (
              <>✨ Name & Avatar</>
            )}
          </button>
          <button
            onClick={() => handleGenerateAvatar()}
            disabled={isGenerating || !persona.trim() || !name.trim()}
            className="px-4 py-2.5 bg-neutral-800 hover:bg-neutral-700 border border-neutral-700 rounded-lg text-sm font-medium disabled:opacity-40 flex items-center justify-center gap-2"
            title={!name.trim() ? "Set a name first" : "Generate avatar only"}
          >
            {genPhase === "avatar" ? (
              <>
                <span className="animate-spin">↻</span>
                Avatar...
              </>
            ) : (
              <>✨ Avatar</>
            )}
          </button>
        </div>

        {/* Tool */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">Tool</label>
          <div className="flex flex-wrap gap-2">
            {["claude", "codex", "gemini", "custom", "llama.cpp"].map((t) => (
              <button
                key={t}
                onClick={() => {
                  if (t !== tool) {
                    setTool(t);
                    if (t === "custom" || t === "llama.cpp") {
                      setModel("");
                    } else {
                      const m = defaultModelForTool(t);
                      setModel(m);
                      if (effort && !effortLevelsForModel(m).includes(effort as EffortLevel)) setEffort("");
                    }
                  }
                }}
                disabled={info ? !(info.tools[t]?.available || info.agentBackends?.[t]) : false}
                className={`px-3 py-2 rounded text-sm font-mono ${
                  tool === t
                    ? "bg-neutral-700 border border-neutral-500"
                    : "bg-neutral-900 border border-neutral-800"
                } disabled:opacity-30`}
              >
                {t}
              </button>
            ))}
          </div>
        </div>

        {/* Custom Base URL */}
        {needsCustomURL && (
          <div>
            <label className="block text-sm text-neutral-400 mb-2">API Base URL</label>
            <input
              type="text"
              value={customBaseURL}
              onChange={(e) => setCustomBaseURL(e.target.value)}
              placeholder="http://localhost:8080"
              className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm font-mono focus:outline-none focus:border-neutral-500"
            />
          </div>
        )}

        {/* Model */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">Model</label>
          {(() => {
            const models = needsCustomURL ? customModels : modelsForTool(tool);
            return models.length > 0 ? (
              <select
                value={model}
                onChange={(e) => {
                  const m = e.target.value;
                  setModel(m);
                  if (effort && !effortLevelsForModel(m).includes(effort as EffortLevel)) setEffort("");
                }}
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
                onChange={(e) => {
                  const m = e.target.value;
                  setModel(m);
                  if (effort && !effortLevelsForModel(m).includes(effort as EffortLevel)) setEffort("");
                }}
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
              <option value="">default ({defaultEffortForModel(model)})</option>
              {effortLevelsForModel(model).map((e) => (
                <option key={e} value={e}>
                  {e}
                </option>
              ))}
            </select>
          </div>
        )}

        {/* Thinking Mode (llama.cpp only) */}
        {tool === "llama.cpp" && (
          <div>
            <label className="block text-sm text-neutral-400 mb-2">Thinking</label>
            <select
              value={thinkingMode}
              onChange={(e) => setThinkingMode(e.target.value)}
              className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
            >
              <option value="">auto (server default)</option>
              <option value="on">on</option>
              <option value="off">off</option>
            </select>
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
          resumeIdleMinutes={resumeIdleMinutes}
          onResumeIdleChange={setResumeIdleMinutes}
          tool={tool}
          silentStart={silentStart}
          silentEnd={silentEnd}
          onSilentStartChange={setSilentStart}
          onSilentEndChange={setSilentEnd}
        />

        {error && (
          <div className="p-3 bg-red-950 border border-red-800 rounded-lg text-sm text-red-300">
            {error}
          </div>
        )}

        <button
          onClick={handleCreate}
          disabled={loading || !name.trim()}
          className="w-full py-3 bg-neutral-800 hover:bg-neutral-700 rounded-lg text-sm font-medium disabled:opacity-40"
        >
          {loading ? "Creating..." : "Create Agent"}
        </button>
      </main>
    </div>
  );
}

import { useEffect, useRef, useState } from "react";
import { useParams, useNavigate } from "react-router";
import { agentApi, type AgentInfo } from "../../lib/agentApi";
import { api } from "../../lib/api";
import { AgentAvatar } from "./AgentAvatar";
import { ScheduleEditor } from "./ScheduleEditor";
import { NotifySourcesEditor } from "./NotifySourcesEditor";
import { SlackBotSettings } from "./SlackBotSettings";
import { defaultModelForTool, modelsForTool, effortLevelsForModel, defaultEffortForModel, supportsEffort, type EffortLevel } from "../../lib/toolModels";

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
  const [customBaseURL, setCustomBaseURL] = useState("http://localhost:8080");
  const [customModels, setCustomModels] = useState<string[]>([]);
  const [thinkingMode, setThinkingMode] = useState("");
  const [workDir, setWorkDir] = useState("");
  const [intervalMinutes, setIntervalMinutes] = useState(30);
  const [timeoutMinutes, setTimeoutMinutes] = useState(10);
  const [resumeIdleMinutes, setResumeIdleMinutes] = useState(0);
  const [silentStart, setSilentStart] = useState("");
  const [silentEnd, setSilentEnd] = useState("");
  const [notifyDuringSilent, setNotifyDuringSilent] = useState(false);
  const [cronMessage, setCronMessage] = useState("");
  const [saving, setSaving] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [archiving, setArchiving] = useState(false);
  const [resetting, setResetting] = useState(false);
  const [resettingSession, setResettingSession] = useState(false);
  const [checkingIn, setCheckingIn] = useState(false);
  const [error, setError] = useState("");
  const [success, setSuccess] = useState(false);
  // Distinct from `success` because a manual check-in is fire-and-forget
  // (server returns 202 immediately); reusing the green "Saved" banner
  // would imply persistence rather than a job started in the background.
  const [checkinNotice, setCheckinNotice] = useState("");
  const [avatarToken, setAvatarToken] = useState(() => Date.now());
  const [generatingAvatar, setGeneratingAvatar] = useState(false);
  const [personaPrompt, setPersonaPrompt] = useState("");
  const [generatingPersona, setGeneratingPersona] = useState(false);
  const [allowedTools, setAllowedTools] = useState<string[]>([]);
  const [allowProtectedPaths, setAllowProtectedPaths] = useState<string[]>([]);
  const [privileged, setPrivileged] = useState(false);
  const [privilegeSaving, setPrivilegeSaving] = useState(false);
  const [showForkDialog, setShowForkDialog] = useState(false);
  const [forkName, setForkName] = useState("");
  const [forkIncludeTranscript, setForkIncludeTranscript] = useState(false);
  const [forking, setForking] = useState(false);
  const [forkError, setForkError] = useState("");
  const [userContext, setUserContext] = useState("");
  const [userContextDirty, setUserContextDirty] = useState(false);
  const [savingUserContext, setSavingUserContext] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (!id) return;
    Promise.all([
      agentApi.get(id),
      agentApi.userContext.get(id),
    ]).then(([a, uc]) => {
      setAgent(a);
      setName(a.name);
      setPersona(a.persona);
      setPublicProfile(a.publicProfile ?? "");
      setPublicProfileOverride(a.publicProfileOverride ?? false);
      setModel(a.model);
      setEffort(a.effort || "");
      setTool(a.tool);
      setCustomBaseURL(a.customBaseURL ?? "http://localhost:8080");
      setThinkingMode(a.thinkingMode ?? "");
      setWorkDir(a.workDir ?? "");
      setIntervalMinutes(a.intervalMinutes);
      setTimeoutMinutes(a.timeoutMinutes || 10);
      setResumeIdleMinutes(a.resumeIdleMinutes ?? 0);
      setSilentStart(a.silentStart ?? "");
      setSilentEnd(a.silentEnd ?? "");
      setNotifyDuringSilent(a.notifyDuringSilent ?? true);
      setCronMessage(a.cronMessage ?? "");
      setAllowedTools(a.allowedTools ?? []);
      setAllowProtectedPaths(a.allowProtectedPaths ?? []);
      setPrivileged(a.privileged ?? false);
      setUserContext(uc);
    }).catch(() => navigate("/"));
  }, [id, navigate]);

  const needsCustomURL = tool === "custom" || tool === "llama.cpp";

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

  const handleSave = async () => {
    setSaving(true);
    setError("");
    setSuccess(false);
    try {
      const [updated] = await Promise.all([
        agentApi.update(id!, {
          name: name.trim(),
          persona: persona.trim(),
          ...(publicProfileOverride ? { publicProfile: publicProfile.trim() } : {}),
          publicProfileOverride,
          model: model.trim(),
          effort: supportsEffort(tool) ? effort : undefined,
          tool: tool.trim(),
          customBaseURL: needsCustomURL ? customBaseURL.trim() : undefined,
          thinkingMode: tool === "llama.cpp" ? thinkingMode : undefined,
          workDir: workDir.trim(),
          intervalMinutes,
          timeoutMinutes,
          resumeIdleMinutes,
          silentStart,
          silentEnd,
          notifyDuringSilent,
          cronMessage,
          allowedTools: (tool === "custom") ? allowedTools : undefined,
          allowProtectedPaths: (tool === "claude" || tool === "custom") ? allowProtectedPaths : undefined,
        }),
        agentApi.userContext.set(id!, userContext),
      ]);
      setAgent(updated);
      setPublicProfile(updated.publicProfile ?? "");
      setPublicProfileOverride(updated.publicProfileOverride ?? false);
      setUserContextDirty(false);
      setSuccess(true);
      setTimeout(() => setSuccess(false), 2000);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const handleCheckin = async () => {
    // The server runs the check-in against the persisted agent record, not
    // the in-flight edits in this form. Bail with a notice instead of
    // silently using stale cronMessage/timeoutMinutes — fixing this with
    // an auto-save would mean committing other unrelated dirty fields too.
    // Match the same normalisations the load path applies so an agent that
    // hasn't been touched doesn't read as dirty:
    //   - timeoutMinutes: 0 (server default) is shown as 10 in the UI, so
    //     compare against the same "|| 10" coercion done at load.
    //   - cronMessage: the server trims on save, so a saved value of "x"
    //     stays equal to "x" no matter how many spaces the textarea adds.
    const savedTimeout = agent ? (agent.timeoutMinutes || 10) : timeoutMinutes;
    const savedMessage = (agent?.cronMessage ?? "").trim();
    if (
      agent &&
      (cronMessage.trim() !== savedMessage || savedTimeout !== timeoutMinutes)
    ) {
      setCheckinNotice(
        "Save your changes first — manual check-in uses the saved Check-in Message and Timeout.",
      );
      setTimeout(() => setCheckinNotice(""), 5000);
      return;
    }
    setCheckingIn(true);
    setError("");
    setCheckinNotice("");
    try {
      await agentApi.checkin(id!);
      try {
        const a = await agentApi.get(id!);
        setAgent(a);
      } catch {
        // non-fatal — stick with the stale value
      }
      setCheckinNotice(
        "Check-in started — the agent will reply in chat when it finishes.",
      );
      setTimeout(() => setCheckinNotice(""), 4000);
    } catch (err) {
      // Match against the server's typed error code rather than the HTTP
      // status: 409 also covers `code:"archived"` (and any future
      // conflict cases) which the user should NOT see as "already
      // working". Only `code:"busy"` means a chat is in flight, which is
      // the case we want to silently turn into a notice instead of a
      // red error.
      const msg = err instanceof Error ? err.message : String(err);
      if (/"code"\s*:\s*"busy"/.test(msg)) {
        setCheckinNotice(
          "Check-in skipped — the agent is already working on something.",
        );
        setTimeout(() => setCheckinNotice(""), 4000);
      } else {
        setError(msg);
      }
    } finally {
      setCheckingIn(false);
    }
  };

  const handleTogglePrivileged = async (next: boolean) => {
    // Privilege is mutated via a dedicated endpoint (not PATCH /agents/{id}),
    // so this fires its own request rather than waiting on Save Changes.
    setPrivilegeSaving(true);
    setError("");
    try {
      await agentApi.setPrivileged(id!, next);
      setPrivileged(next);
      setAgent((a) => (a ? { ...a, privileged: next } : a));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setPrivilegeSaving(false);
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

  const openForkDialog = () => {
    setForkName(agent ? `${agent.name} (fork)` : "");
    setForkIncludeTranscript(false);
    setForkError("");
    setShowForkDialog(true);
  };

  const handleFork = async () => {
    const trimmed = forkName.trim();
    if (!trimmed) {
      setForkError("Name is required");
      return;
    }
    setForking(true);
    setForkError("");
    try {
      const forked = await agentApi.fork(id!, { name: trimmed, includeTranscript: forkIncludeTranscript });
      setShowForkDialog(false);
      navigate(`/agents/${forked.id}/settings`);
    } catch (err) {
      setForkError(err instanceof Error ? err.message : String(err));
    } finally {
      setForking(false);
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

  const handleArchive = async () => {
    if (
      !confirm(
        "Archive this agent? Runtime activity stops; data is kept and can be restored from Settings.\n\nThe agent will be removed from all group DMs (2-person groups dissolve), and memberships are NOT restored on unarchive — the agent must be re-invited.",
      )
    )
      return;
    setArchiving(true);
    try {
      await agentApi.archive(id!);
      navigate("/", { replace: true });
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setArchiving(false);
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
                if (e.key === "Enter" && !e.nativeEvent.isComposing && !e.shiftKey && !generatingPersona) {
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

        {/* User Context */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">
            User Context
          </label>
          <textarea
            value={userContext}
            onChange={(e) => { setUserContext(e.target.value); setUserContextDirty(true); }}
            rows={6}
            placeholder="Record information about users and collaborators (agents also update this through conversation)"
            className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm resize-none focus:outline-none focus:border-neutral-500"
          />
          <p className="mt-1 text-xs text-neutral-600">
            Injected into system prompt. Agents can also update this via their tools.
          </p>
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
                className={`px-3 py-2 rounded text-sm font-mono ${
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

        {/* Custom Base URL */}
        {needsCustomURL && (
          <div>
            <label className="block text-sm text-neutral-400 mb-2">Custom Base URL</label>
            <input
              type="text"
              value={customBaseURL}
              onChange={(e) => setCustomBaseURL(e.target.value)}
              placeholder="http://localhost:8080"
              className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm font-mono focus:outline-none focus:border-neutral-500"
            />
            <p className="text-xs text-neutral-600 mt-1">Anthropic Messages API compatible endpoint</p>
          </div>
        )}

        {/* Allowed Tools (custom only) */}
        {tool === "custom" && (
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

        {/* Protected Path Allow (claude / custom) */}
        {(tool === "claude" || tool === "custom") && (
          <div>
            <label className="block text-sm text-neutral-400 mb-2">
              Allow Edits in Protected Paths
              <span className="text-xs text-neutral-600 ml-2">(bypass claude-code guard)</span>
            </label>
            <div className="grid grid-cols-3 gap-1.5">
              {["claude", "git", "husky"].map((p) => (
                <label key={p} className="flex items-center gap-2 px-2 py-1.5 bg-neutral-900 rounded text-xs font-mono cursor-pointer hover:bg-neutral-800">
                  <input
                    type="checkbox"
                    checked={allowProtectedPaths.includes(p)}
                    onChange={(e) => {
                      if (e.target.checked) {
                        setAllowProtectedPaths([...allowProtectedPaths, p]);
                      } else {
                        setAllowProtectedPaths(allowProtectedPaths.filter((x) => x !== p));
                      }
                    }}
                    className="accent-neutral-400"
                  />
                  .{p}
                </label>
              ))}
            </div>
            <p className="text-xs text-neutral-600 mt-1">
              Recent claude-code versions prompt on Edit/Write to .claude, .git, .husky even with bypassPermissions. Check to suppress.
            </p>
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
          cronMessage={cronMessage}
          onCronMessageChange={setCronMessage}
          nextCronAt={agent.nextCronAt}
          scheduleDirty={
            // Schedule-affecting fields differ from the persisted agent —
            // nextCronAt is computed against the saved schedule so showing
            // it during edits would mislead.
            agent.intervalMinutes !== intervalMinutes ||
            (agent.silentStart ?? "") !== silentStart ||
            (agent.silentEnd ?? "") !== silentEnd
          }
          onCheckin={handleCheckin}
          // Keep the button disabled while the notice banner is up so the
          // user doesn't fire repeated 409s in quick succession before the
          // server-side run actually gets going.
          checkingIn={checkingIn || checkinNotice !== ""}
        />

        {/* Notify During Silent Hours */}
        <div>
          <label className="flex items-start gap-3 cursor-pointer select-none">
            <input
              type="checkbox"
              checked={notifyDuringSilent}
              onChange={(e) => setNotifyDuringSilent(e.target.checked)}
              className="mt-1 accent-amber-500"
            />
            <div className="flex-1">
              <div className="text-sm text-neutral-300">Receive DM During Silent Hours</div>
              <p className="text-xs text-neutral-600 mt-0.5">
                When enabled, group DM notifications are delivered even
                during silent hours. When disabled, notifications are
                suppressed (messages remain in the transcript).
              </p>
            </div>
          </label>
        </div>

        {/* Privilege.

            POST /api/v1/agents/{id}/privilege is Owner-only. The web UI
            is only ever served to Owner principals (the public listener
            is OwnerOnlyMiddleware on Tailscale; --local requires the
            Owner Bearer for asset delivery), so the toggle has no
            non-Owner code path to worry about and there is no
            client-side role gate. If the asset gating is ever relaxed
            we'd need to hide this control too — keep that in mind when
            touching index.html / the Bearer bootstrap. */}
        <div>
          <label className="flex items-start gap-3 cursor-pointer select-none">
            <input
              type="checkbox"
              checked={privileged}
              disabled={privilegeSaving}
              onChange={(e) => handleTogglePrivileged(e.target.checked)}
              className="mt-1 accent-amber-500"
            />
            <div className="flex-1">
              <div className="text-sm text-neutral-300">Privileged Agent</div>
              <p className="text-xs text-neutral-600 mt-0.5">
                Allow this agent to delete / reset / archive other agents via
                the API. Cannot fork or read other agents&apos; full record.
              </p>
            </div>
          </label>
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
        {checkinNotice && (
          <div className="p-3 bg-amber-950/40 border border-amber-800/60 rounded-lg text-sm text-amber-200">
            {checkinNotice}
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

        {/* ── Actions ── */}
        <section className="rounded-xl border border-neutral-800 p-5">
          <button
            onClick={openForkDialog}
            className="w-full py-3 bg-neutral-800 hover:bg-neutral-700 rounded-lg text-sm font-medium"
          >
            Fork Agent
          </button>
          <p className="text-xs text-neutral-600 mt-1">
            Create a copy with persona and memory carried over. Slack, notifications, and credentials are not transferred.
          </p>
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
          <div>
            <button
              onClick={handleArchive}
              disabled={archiving}
              className="w-full py-3 bg-neutral-900 hover:bg-neutral-800 border border-neutral-700 rounded-lg text-sm font-medium text-neutral-300 disabled:opacity-40"
            >
              {archiving ? "Archiving..." : "Archive Agent"}
            </button>
            <p className="text-xs text-neutral-600 mt-1">
              Hide from the main list and stop runtime activity. Data is kept; restore from Settings. Removes the agent from all group DMs (memberships are NOT restored on unarchive).
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

      {showForkDialog && (
        <div
          role="dialog"
          aria-modal="true"
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 outline-none"
          onClick={(e) => { if (e.target === e.currentTarget && !forking) setShowForkDialog(false); }}
          onKeyDown={(e) => { if (e.key === "Escape" && !forking) setShowForkDialog(false); }}
        >
          <div className="bg-neutral-900 border border-neutral-700 rounded-lg p-5 w-[22rem] shadow-xl">
            <h3 className="text-sm font-medium text-neutral-200 mb-3">Fork agent</h3>
            <label className="block text-xs text-neutral-400 mb-1">Name</label>
            <input
              type="text"
              value={forkName}
              onChange={(e) => setForkName(e.target.value)}
              disabled={forking}
              autoFocus
              className="w-full px-2 py-1.5 text-sm bg-neutral-800 border border-neutral-700 rounded text-neutral-200 focus:outline-none focus:ring-1 focus:ring-blue-500/50 mb-3"
            />
            <label className="flex items-start gap-2 text-sm text-neutral-400 mb-2 cursor-pointer select-none">
              <input
                type="checkbox"
                checked={forkIncludeTranscript}
                onChange={(e) => setForkIncludeTranscript(e.target.checked)}
                disabled={forking}
                className="mt-0.5 rounded border-neutral-600 bg-neutral-800 text-blue-500 focus:ring-blue-500/30"
              />
              <span>
                Include conversation history
                <span className="block text-xs text-neutral-600">Persona and memory are always copied.</span>
              </span>
            </label>
            <p className="text-xs text-neutral-600 mb-4">
              Slack bot, notification sources, and credentials are not transferred.
            </p>
            {forkError && (
              <p className="text-xs text-red-400 mb-3">{forkError}</p>
            )}
            <div className="flex justify-end gap-2">
              <button
                onClick={() => setShowForkDialog(false)}
                disabled={forking}
                className="px-3 py-1.5 text-xs text-neutral-400 hover:text-neutral-200 rounded disabled:opacity-50"
              >
                Cancel
              </button>
              <button
                onClick={handleFork}
                disabled={forking || !forkName.trim()}
                className="px-3 py-1.5 text-xs bg-blue-600 hover:bg-blue-500 text-white rounded disabled:opacity-50"
              >
                {forking ? "Forking…" : "Fork"}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

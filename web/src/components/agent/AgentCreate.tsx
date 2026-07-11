import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router";
import { agentApi } from "../../lib/agentApi";
import { ScheduleEditor } from "./ScheduleEditor";
import { api, type ServerInfo } from "../../lib/api";
import { modelsForTool, supportsEffort, type EffortLevel } from "../../lib/toolModels";
import { errMsg } from "../../lib/utils";
import { useCustomModels } from "./fields/useCustomModels";
import { PersonaField } from "./fields/PersonaField";
import { ToolPicker } from "./fields/ToolPicker";
import { ModelPicker } from "./fields/ModelPicker";
import { EffortPicker } from "./fields/EffortPicker";
import { WorkDirInput } from "./fields/WorkDirInput";
import { PageHeader } from "../ui/PageHeader";
import { SectionCard } from "../ui/SectionCard";
import { Field } from "../ui/Field";
import { Input } from "../ui/Input";
import { Select } from "../ui/Select";
import { Button } from "../ui/Button";
import { Banner } from "../ui/Banner";
import { useT } from "../../lib/i18n";

type GenPhase = "idle" | "persona" | "name" | "avatar" | "all-name" | "all-avatar";

export function AgentCreate() {
  const t = useT();
  const navigate = useNavigate();
  const [info, setInfo] = useState<ServerInfo>();
  const [name, setName] = useState("");
  const [persona, setPersona] = useState("");
  const [model, setModel] = useState("sonnet");
  const [effort, setEffort] = useState<EffortLevel | "">("");
  const [tool, setTool] = useState("claude");
  const [customBaseURL, setCustomBaseURL] = useState("http://localhost:8080");
  const [thinkingMode, setThinkingMode] = useState("");
  const [workDir, setWorkDir] = useState("");
  // cronExpr starts as the default "*/30 * * * *" only for ScheduleEditor's
  // initial visual state. Until the user actually touches the editor we send
  // `cronExpr: undefined` on POST so the server picks the per-agent offset
  // default — without this every newly-created agent would land on :00/:30
  // and bunch up at the same minute.
  const [cronExpr, setCronExpr] = useState("*/30 * * * *");
  const [cronExprDirty, setCronExprDirty] = useState(false);
  // -1 = no timeout: the default for new agents.
  const [timeoutMinutes, setTimeoutMinutes] = useState(-1);
  const [resumeIdleMinutes, setResumeIdleMinutes] = useState(0);
  const [silentStart, setSilentStart] = useState("");
  const [silentEnd, setSilentEnd] = useState("");
  const [cronMessage, setCronMessage] = useState("");
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

  const { needsCustomURL, customModels } = useCustomModels(tool, customBaseURL, setModel);

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
      setError(t("create.personaPromptRequired"));
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
      setError(errMsg(err));
    } finally {
      setGenPhase("idle");
    }
  };

  const handleGenerateName = async () => {
    if (!persona.trim()) {
      setError(t("create.personaRequired"));
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
      setError(errMsg(err));
      return null;
    } finally {
      setGenPhase("idle");
    }
  };

  const handleGenerateAvatar = async (nameOverride?: string) => {
    const n = nameOverride || name.trim();
    if (!persona.trim()) {
      setError(t("create.personaRequired"));
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
      setError(errMsg(err));
    } finally {
      setGenPhase("idle");
    }
  };

  const handleGenerateAll = async () => {
    if (!persona.trim()) {
      setError(t("create.personaRequired"));
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
      setError(errMsg(err));
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
      setError(errMsg(err));
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
      setError(t("settings.nameRequired"));
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
        cronExpr: cronExprDirty ? cronExpr : undefined,
        timeoutMinutes,
        resumeIdleMinutes: resumeIdleMinutes || undefined,
        silentStart: silentStart || undefined,
        silentEnd: silentEnd || undefined,
        cronMessage: cronMessage.trim() || undefined,
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
      setError(errMsg(err));
      setLoading(false);
    }
  };

  const genStatusText =
    genPhase === "persona"
      ? t("create.generatingPersona")
      : genPhase === "all-name" || genPhase === "name"
        ? t("create.generatingName")
        : genPhase === "all-avatar" || genPhase === "avatar"
          ? t("create.generatingAvatar")
          : "";

  return (
    <div className="h-full overflow-y-auto bg-app text-ink">
      <PageHeader title={t("create.newAgent")} onBack={() => navigate("/")} hideBackAtLg />

      <main className="mx-auto max-w-[560px] space-y-6 px-4 py-6">
        {/* ── Identity ── */}
        <SectionCard id="identity" title={t("settings.sec.identity")}>
          <div className="space-y-5">
            <PersonaField
              persona={persona}
              setPersona={setPersona}
              textareaRows={5}
              textareaPlaceholder={t("create.personaPlaceholder")}
              personaPrompt={personaPrompt}
              setPersonaPrompt={setPersonaPrompt}
              promptPlaceholder={t("create.personaPromptPlaceholder")}
              busy={isGenerating}
              spinning={genPhase === "persona"}
              onGenerate={handleGeneratePersona}
            />

            {/* Avatar + Name + Hint */}
            <div className="flex gap-4">
              {/* Avatar preview */}
              <div className="relative shrink-0">
                <button
                  type="button"
                  onClick={handleAvatarClick}
                  disabled={isGenerating}
                  className="flex h-24 w-24 items-center justify-center overflow-hidden rounded-full border border-hairline bg-raised transition-colors hover:border-ink-faint disabled:cursor-not-allowed disabled:opacity-60"
                  title={t("create.uploadAvatarTitle")}
                >
                  {avatarPreviewUrl ? (
                    <img
                      src={avatarPreviewUrl}
                      alt={t("create.avatarAlt")}
                      className="h-full w-full object-cover"
                    />
                  ) : (
                    <svg
                      className="h-7 w-7 text-ink-faint"
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
                    className="absolute -bottom-1 -right-1 flex h-6 w-6 items-center justify-center rounded-full border border-hairline bg-raised text-xs transition-colors hover:bg-hover disabled:opacity-40"
                    title={t("create.regenAvatarTitle")}
                  >
                    {genPhase === "avatar" ? <span className="animate-spin">↻</span> : "✨"}
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
                <Field label={t("dash.name")}>
                  <div className="flex gap-2">
                    <Input
                      value={name}
                      onChange={(e) => setName(e.target.value)}
                      placeholder={t("dash.name")}
                      className="flex-1"
                    />
                    <Button
                      onClick={() => handleGenerateName()}
                      disabled={isGenerating || !persona.trim()}
                      title={t("create.genNameTitle")}
                      className="shrink-0"
                    >
                      {genPhase === "name" ? (
                        <span className="inline-block animate-spin">↻</span>
                      ) : (
                        "✨"
                      )}
                    </Button>
                  </div>
                </Field>
                <Input
                  aria-label={t("create.genHintAria")}
                  value={genPrompt}
                  onChange={(e) => setGenPrompt(e.target.value)}
                  placeholder={t("create.genHintPlaceholder")}
                />
              </div>
            </div>

            {/* Generate All / Avatar only */}
            <div className="flex gap-2">
              <Button
                onClick={handleGenerateAll}
                disabled={isGenerating || !persona.trim()}
                className="flex flex-1 items-center justify-center gap-2"
              >
                {genPhase.startsWith("all-") ? (
                  <>
                    <span className="animate-spin">↻</span>
                    {genStatusText}
                  </>
                ) : (
                  <>✨ {t("create.nameAndAvatar")}</>
                )}
              </Button>
              <Button
                onClick={() => handleGenerateAvatar()}
                disabled={isGenerating || !persona.trim() || !name.trim()}
                className="flex items-center justify-center gap-2"
                title={!name.trim() ? t("create.setNameFirst") : t("create.genAvatarOnly")}
              >
                {genPhase === "avatar" ? (
                  <>
                    <span className="animate-spin">↻</span>
                    {t("create.avatarProgress")}
                  </>
                ) : (
                  <>✨ {t("create.avatar")}</>
                )}
              </Button>
            </div>
          </div>
        </SectionCard>

        {/* ── Model & Tools ── */}
        <SectionCard id="model" title={t("settings.sec.model")}>
          <div className="space-y-4">
            <ToolPicker
              tool={tool}
              setTool={setTool}
              setModel={setModel}
              effort={effort}
              setEffort={setEffort}
              isDisabled={(toolName) => (info ? !(info.tools[toolName]?.available || info.agentBackends?.[toolName]) : false)}
            />

            {needsCustomURL && (
              <Field label={t("create.apiBaseUrl")}>
                <Input
                  mono
                  value={customBaseURL}
                  onChange={(e) => setCustomBaseURL(e.target.value)}
                  placeholder="http://localhost:8080"
                />
              </Field>
            )}

            <ModelPicker
              model={model}
              setModel={setModel}
              effort={effort}
              setEffort={setEffort}
              models={needsCustomURL ? customModels : modelsForTool(tool)}
            />

            <EffortPicker tool={tool} effort={effort} setEffort={setEffort} model={model} />

            {tool === "llama.cpp" && (
              <Field label={t("settings.thinking")}>
                <Select value={thinkingMode} onChange={(e) => setThinkingMode(e.target.value)}>
                  <option value="">{t("settings.thinkingAuto")}</option>
                  <option value="on">on</option>
                  <option value="off">off</option>
                </Select>
              </Field>
            )}

            <WorkDirInput workDir={workDir} setWorkDir={setWorkDir} />
          </div>
        </SectionCard>

        {/* ── Schedule ── */}
        <SectionCard id="schedule" title={t("settings.sec.schedule")}>
          <ScheduleEditor
            cronExpr={cronExpr}
            onCronExprChange={(v) => {
              setCronExpr(v);
              setCronExprDirty(true);
            }}
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
          />
        </SectionCard>

        {error && <Banner tone="error">{error}</Banner>}

        {/* Primary action — sticky above the fold on mobile. */}
        <div className="sticky bottom-0 -mx-4 border-t border-hairline bg-app/90 px-4 py-3 backdrop-blur sm:static sm:mx-0 sm:border-0 sm:bg-transparent sm:p-0 sm:backdrop-blur-none">
          <Button
            variant="primary"
            onClick={handleCreate}
            disabled={loading || !name.trim()}
            className="w-full py-3"
          >
            {loading ? t("dash.creating") : t("create.createAgent")}
          </Button>
        </div>
      </main>
    </div>
  );
}

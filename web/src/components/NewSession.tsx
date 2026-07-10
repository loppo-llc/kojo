import { useEffect, useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router";
import { api, type ServerInfo } from "../lib/api";
import { peersApi, type PeerInfo } from "../lib/peerApi";
import { modelsForTool, sessionEffortLevelsForModel, supportsEffort } from "../lib/toolModels";
import { errMsg } from "../lib/utils";
import { PageHeader } from "./ui/PageHeader";
import { SectionCard } from "./ui/SectionCard";
import { Field } from "./ui/Field";
import { Input } from "./ui/Input";
import { Select } from "./ui/Select";
import { Button } from "./ui/Button";
import { Banner } from "./ui/Banner";
import { Toggle } from "./ui/Toggle";
import { useT } from "../lib/i18n";

export function NewSession() {
  const t = useT();
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const [info, setInfo] = useState<ServerInfo>();
  const [tool, setTool] = useState("claude");
  const [model, setModel] = useState("");
  // Per-session effort. "" = CLI default. Unlike agent settings this may
  // include "ultra" for codex models that advertise it (see
  // sessionEffortLevelsForModel) — ultra is a session-scoped choice only.
  const [effort, setEffort] = useState("");
  const [workDir, setWorkDir] = useState("");
  const [args, setArgs] = useState("");
  const [yoloMode, setYoloMode] = useState(false);
  const [simpleSystemPrompt, setSimpleSystemPrompt] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [suggestions, setSuggestions] = useState<string[]>([]);
  const [showSuggestions, setShowSuggestions] = useState(false);
  const suggestTimer = useRef<ReturnType<typeof setTimeout>>(null);
  const wrapperRef = useRef<HTMLDivElement>(null);
  // Peer selector. Empty selectedPeerId = "this host". The list
  // includes every registry row (including self) so the operator
  // can explicitly confirm "this device" — the dropdown shows
  // human-friendly names, not URLs.
  const [peers, setPeers] = useState<PeerInfo[]>([]);
  const [selfPeerId, setSelfPeerId] = useState("");
  const [selectedPeerId, setSelectedPeerId] = useState("");

  // Peer list is best-effort: a server without peer-identity wiring
  // returns 404 / 503 and we just hide the selector. Runs once.
  useEffect(() => {
    peersApi.list().then((resp) => {
      setPeers(resp.items ?? []);
      setSelfPeerId(resp.selfDeviceId ?? "");
    }).catch(() => { /* peer registry unavailable */ });
  }, []);

  // Fetch server info from the SELECTED host (Hub default, or the
  // remote peer when one is chosen) so tool availability / homeDir
  // reflect what's actually installed on that machine. Re-runs on
  // peer switch — the Hub proxy forwards the info call when
  // ?peer=<id> is present and the peer is paired (registered on
  // both sides via the join-request flow).
  //
  // Concurrency: a rapid peer switch can spawn two info() promises;
  // without a guard the slower (older) reply could overwrite the
  // newer one and leave the form on the wrong host's tools / homeDir.
  // The infoSeq counter discards stale replies. On failure we clear
  // `info` so the Create button (which gates on `tool` + `workDir`)
  // can't fire against the previous host's settings.
  const infoSeq = useRef(0);
  const [infoLoading, setInfoLoading] = useState(false);
  useEffect(() => {
    const peerId = selectedPeerId && selectedPeerId !== selfPeerId ? selectedPeerId : undefined;
    const seq = ++infoSeq.current;
    setError("");
    // Clear the form during the switch so a click on Start lands
    // on whichever host the user picked LAST. Otherwise the
    // previous host's tool / workDir stays selectable while the
    // new info() is still in flight.
    setInfo(undefined);
    setTool("");
    setModel("");
    setEffort("");
    setWorkDir("");
    setInfoLoading(true);
    api.info(peerId).then((info) => {
      if (seq !== infoSeq.current) return;
      setInfo(info);
      const paramTool = searchParams.get("tool");
      const paramDir = searchParams.get("workDir");
      const tools = info.tools ?? {};
      if (paramTool && tools[paramTool]?.available) {
        setTool(paramTool);
      } else {
        const available = Object.entries(tools).find(([, toolInfo]) => toolInfo.available);
        if (available) setTool(available[0]);
        else setTool("");
      }
      setWorkDir(paramDir || info.homeDir || "");
      setInfoLoading(false);
    }).catch((err) => {
      if (seq !== infoSeq.current) return;
      console.error(err);
      setInfoLoading(false);
      if (selectedPeerId && selectedPeerId !== selfPeerId) {
        setError(t("ns.peerInfoUnavailable"));
      }
    });
  }, [searchParams, selectedPeerId, selfPeerId]);

  // close suggestions on outside click
  useEffect(() => {
    const handler = (e: MouseEvent) => {
      if (wrapperRef.current && !wrapperRef.current.contains(e.target as Node)) {
        setShowSuggestions(false);
      }
    };
    document.addEventListener("mousedown", handler);
    return () => document.removeEventListener("mousedown", handler);
  }, []);

  const fetchSuggestions = (value: string) => {
    if (suggestTimer.current) clearTimeout(suggestTimer.current);
    if (!value || value.length < 2) {
      setSuggestions([]);
      setShowSuggestions(false);
      return;
    }
    suggestTimer.current = setTimeout(() => {
      const peerId = selectedPeerId && selectedPeerId !== selfPeerId ? selectedPeerId : undefined;
      api.dirSuggest(value, peerId).then((dirs) => {
        setSuggestions(dirs);
        setShowSuggestions(dirs.length > 0);
      }).catch(console.error);
    }, 150);
  };

  const handleWorkDirChange = (value: string) => {
    setWorkDir(value);
    fetchSuggestions(value);
  };

  const selectSuggestion = (dir: string) => {
    setWorkDir(dir);
    setShowSuggestions(false);
    fetchSuggestions(dir + "/");
  };

  const handleCreate = async () => {
    if (!tool || !workDir) return;
    setLoading(true);
    setError("");
    try {
      let parsedArgs = args.trim() ? args.trim().split(/\s+/) : [];
      // Skip the dropdown model when the user already passed a model
      // flag in Additional Arguments (--model / -m, split or joined
      // form) so we never inject a duplicate the CLI would reject.
      const hasModelArg = parsedArgs.some(
        (a) => a === "--model" || a === "-m" || a.startsWith("--model=") || a.startsWith("-m="),
      );
      if (model && !hasModelArg) {
        parsedArgs = ["--model", model, ...parsedArgs];
      }
      // Effort flag differs per CLI: claude/grok take --effort, codex takes
      // a -c config override. Skip when the user already passed their own,
      // and when a manual --model/-m overrides the dropdown model — the
      // selected effort was validated against the dropdown model's ladder,
      // which may not apply to the manually-typed one.
      if (effort && model && !hasModelArg && supportsEffort(tool)) {
        const hasEffortArg = parsedArgs.some(
          (a) =>
            a === "--effort" ||
            a.startsWith("--effort=") ||
            // any -c / --config form, split or joined
            a.includes("model_reasoning_effort"),
        );
        if (!hasEffortArg) {
          parsedArgs =
            tool === "codex"
              ? ["-c", `model_reasoning_effort=${effort}`, ...parsedArgs]
              : ["--effort", effort, ...parsedArgs];
        }
      }
      const peerId = selectedPeerId && selectedPeerId !== selfPeerId ? selectedPeerId : undefined;
      const session = await api.sessions.create({
        tool,
        workDir,
        args: parsedArgs.length > 0 ? parsedArgs : undefined,
        yoloMode,
        simpleSystemPrompt,
        peerId,
      });
      const target = peerId
        ? `/session/${session.id}?peer=${encodeURIComponent(peerId)}`
        : `/session/${session.id}`;
      navigate(target, { replace: true });
    } catch (err) {
      setError(errMsg(err));
      setLoading(false);
    }
  };

  return (
    <div className="h-full overflow-y-auto bg-app text-ink">
      <PageHeader
        title={t("ns.title")}
        onBack={() => navigate("/", { replace: true })}
        hideBackAtLg
      />

      <main className="mx-auto max-w-[560px] px-4 py-4">
        <SectionCard>
          <div className="space-y-5">
            {/* Peer selection (only when 2+ peers are registered) */}
            {peers.length > 1 && (
              <Field label={t("ns.host")}>
                <Select
                  value={selectedPeerId || selfPeerId}
                  onChange={(e) => setSelectedPeerId(e.target.value)}
                >
                  {peers.map((p) => {
                    const isSelf = p.deviceId === selfPeerId;
                    const offline = p.status !== "online";
                    // A non-self peer needs to be paired (registered on
                    // both sides via the join-request flow). The local
                    // UI can only hint at what we know: status + name.
                    // The Hub-side proxy will 403 if the pairing is
                    // missing on the other side — surface it as a
                    // runtime error rather than disabling here.
                    const disabled = !isSelf && offline;
                    return (
                      <option key={p.deviceId} value={p.deviceId} disabled={disabled}>
                        {p.name}
                        {isSelf ? ` (${t("peers.thisDevice")})` : ""}
                        {offline && !isSelf ? ` — ${t("ns.offline")}` : ""}
                      </option>
                    );
                  })}
                </Select>
              </Field>
            )}

            {/* Tool selection */}
            <Field label={t("field.tool")}>
              <div className="space-y-2">
                {info &&
                  Object.entries(info.tools ?? {}).map(([name, toolInfo]) => (
                    <label
                      key={name}
                      className={`flex cursor-pointer items-center gap-3 rounded-[10px] border p-3 transition-colors ${
                        tool === name
                          ? "border-copper/50 bg-copper/10"
                          : "border-hairline bg-raised hover:bg-hover"
                      } ${!toolInfo.available ? "cursor-not-allowed opacity-40" : ""}`}
                    >
                      <input
                        type="radio"
                        name="tool"
                        value={name}
                        checked={tool === name}
                        disabled={!toolInfo.available}
                        onChange={() => {
                          setTool(name);
                          setModel("");
                          setEffort("");
                        }}
                        className="accent-[color:var(--color-copper)]"
                      />
                      <span className="font-mono text-[14px] text-ink">{name}</span>
                      {!toolInfo.available && (
                        <span className="text-[11px] text-ink-faint">{t("ns.notAvailable")}</span>
                      )}
                    </label>
                  ))}
              </div>
            </Field>

            {/* Model (tools with a known whitelist) */}
            {modelsForTool(tool).length > 0 && (
              <Field label={t("settings.model")}>
                <Select
                  value={model}
                  onChange={(e) => {
                    const m = e.target.value;
                    setModel(m);
                    if (effort && !sessionEffortLevelsForModel(m).includes(effort)) setEffort("");
                  }}
                  mono
                >
                  <option value="">{t("ns.defaultOption")}</option>
                  {modelsForTool(tool).map((m) => (
                    <option key={m} value={m}>
                      {m}
                    </option>
                  ))}
                </Select>
              </Field>
            )}

            {/* Effort (explicit model only — with the CLI-default model the
                valid ladder is unknowable, so both stay CLI default). May
                include "ultra" for codex models that advertise it; ultra is
                intentionally NOT offered in agent settings. */}
            {model && supportsEffort(tool) && (
              <Field label={t("field.effort")}>
                <Select value={effort} onChange={(e) => setEffort(e.target.value)} mono>
                  <option value="">{t("ns.defaultOption")}</option>
                  {sessionEffortLevelsForModel(model).map((lv) => (
                    <option key={lv} value={lv}>
                      {lv}
                    </option>
                  ))}
                </Select>
              </Field>
            )}

            {/* Working directory */}
            <Field label={t("ns.workingDirectory")}>
              <div ref={wrapperRef} className="relative">
                <Input
                  mono
                  type="text"
                  value={workDir}
                  onChange={(e) => handleWorkDirChange(e.target.value)}
                  onFocus={() => suggestions.length > 0 && setShowSuggestions(true)}
                  placeholder="/path/to/your/project"
                />
                {showSuggestions && (
                  <ul className="absolute inset-x-0 z-20 mt-1 max-h-48 overflow-y-auto rounded-[10px] border border-hairline bg-raised py-1 shadow-xl shadow-black/40">
                    {suggestions.map((dir) => (
                      <li key={dir}>
                        <button
                          type="button"
                          onClick={() => selectSuggestion(dir)}
                          className="block w-full truncate px-3 py-2 text-left font-mono text-[13px] text-ink-dim transition-colors hover:bg-hover hover:text-ink focus:bg-hover focus:text-ink focus:outline-none"
                        >
                          {dir}
                        </button>
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            </Field>

            {/* Additional arguments */}
            <Field label={t("ns.additionalArgs")}>
              <Input
                mono
                type="text"
                value={args}
                onChange={(e) => setArgs(e.target.value)}
                placeholder="--model opus"
              />
            </Field>

            {/* Yolo mode */}
            <div
              onClick={() => setYoloMode(!yoloMode)}
              className="flex cursor-pointer items-center justify-between gap-3 rounded-[10px] border border-hairline bg-raised px-3 py-2.5"
            >
              <div className="min-w-0">
                <div className="text-[13px] text-ink">{t("ns.yoloMode")}</div>
                <div className="text-[11px] text-ink-faint">
                  {tool === "claude"
                    ? t("ns.yoloClaude")
                    : tool === "codex"
                      ? t("ns.yoloCodex")
                      : t("ns.yoloOther")}
                </div>
              </div>
              <span onClick={(e) => e.stopPropagation()}>
                <Toggle checked={yoloMode} onChange={setYoloMode} aria-label={t("ns.yoloMode")} />
              </span>
            </div>

            {/* Minimal system prompt (claude / custom only) */}
            {(tool === "claude" || tool === "custom") && (
              <div
                onClick={() => setSimpleSystemPrompt(!simpleSystemPrompt)}
                className="flex cursor-pointer items-center justify-between gap-3 rounded-[10px] border border-hairline bg-raised px-3 py-2.5"
              >
                <div className="min-w-0">
                  <div className="text-[13px] text-ink">{t("ns.minimalPrompt")}</div>
                  <div className="text-[11px] text-ink-faint">
                    {t("ns.minimalPromptHelp")} (<code>--system-prompt</code>)
                  </div>
                </div>
                <span onClick={(e) => e.stopPropagation()}>
                  <Toggle
                    checked={simpleSystemPrompt}
                    onChange={setSimpleSystemPrompt}
                    aria-label={t("ns.minimalPrompt")}
                  />
                </span>
              </div>
            )}

            {error && <Banner tone="error">{error}</Banner>}

            <Button
              variant="primary"
              onClick={handleCreate}
              disabled={loading || infoLoading || !tool || !workDir}
              className="w-full py-2.5"
            >
              {loading ? t("dash.creating") : t("ns.createSession")}
            </Button>
          </div>
        </SectionCard>
      </main>
    </div>
  );
}

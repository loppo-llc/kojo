import { useCallback, useEffect, useState } from "react";
import {
  listExtensions,
  previewExtension,
  installExtension,
  updateExtension,
  setExtensionEnabled,
  setExtensionAutoUpdate,
  checkExtensionUpdates,
  removeExtension,
  getExtensionSchema,
  setExtensionConfig,
  setExtensionAgentBinding,
  getExtensionToken,
  rotateExtensionToken,
  type InstalledExtension,
  type ExtensionPreview,
  type ExtensionServiceStatus,
  type ExtensionUpdateStatus,
  type JSONSchema,
} from "../../lib/extensionsApi";
import { agentApi, type AgentInfo } from "../../lib/agentApi";
import { errMsg } from "../../lib/utils";
import { useT, bootstrapLocales } from "../../lib/i18n";
import { SectionCard } from "../ui/SectionCard";
import { Field } from "../ui/Field";
import { Input } from "../ui/Input";
import { Button } from "../ui/Button";
import { Toggle } from "../ui/Toggle";
import { Banner } from "../ui/Banner";
import { RelTime } from "../ui/RelTime";
import { SchemaForm } from "./SchemaForm";

/**
 * Install and manage kojo extension packages.
 *
 * Flow: paste a git URL → preview (the daemon fetches the repo and
 * parses kojo-package.json) → review the capabilities it asks for →
 * install. Nothing is registered before the operator acknowledges the
 * exact scope list, and the daemon rejects an acknowledgement that does
 * not match the manifest, so this dialog cannot be bypassed by a
 * hand-rolled request that under-reports what a package wants.
 *
 * Per-agent enablement lives on the agent's own settings page; this
 * section owns installation and instance-wide configuration only.
 */
export function ExtensionsSection({
  setError,
  flashSuccess,
}: {
  setError: (msg: string) => void;
  flashSuccess: () => void;
}) {
  const t = useT();
  const [items, setItems] = useState<InstalledExtension[]>([]);
  const [services, setServices] = useState<ExtensionServiceStatus[]>([]);
  const [updates, setUpdates] = useState<
    Record<string, ExtensionUpdateStatus>
  >({});
  const [agents, setAgents] = useState<AgentInfo[]>([]);
  const [url, setUrl] = useState("");
  const [ref, setRef] = useState("");
  const [preview, setPreview] = useState<ExtensionPreview | null>(null);
  const [busy, setBusy] = useState(false);
  const [expanded, setExpanded] = useState<string | null>(null);

  const reload = useCallback(() => {
    listExtensions()
      .then((res) => {
        setItems(res.extensions);
        setServices(res.services);
        setUpdates(res.updates);
      })
      .catch((err) => setError(errMsg(err)));
    // Installing, enabling or removing a package can change which UI
    // languages exist. Re-read them here so the language picker one
    // section up is right without a page reload.
    void bootstrapLocales();
  }, [setError]);

  useEffect(() => {
    reload();
  }, [reload]);

  // The background update pass can swap a checkout while this page sits
  // open in another tab. The web app does not subscribe to server events,
  // so re-read the list when the tab comes back into view instead.
  useEffect(() => {
    const onVisible = () => {
      if (document.visibilityState === "visible") reload();
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => document.removeEventListener("visibilitychange", onVisible);
  }, [reload]);

  // The agent roster drives the per-agent enablement list. Loaded once:
  // binding an extension to an agent created mid-session is a page
  // refresh away, and polling the roster here would be noise.
  useEffect(() => {
    agentApi
      .list()
      .then(setAgents)
      .catch((err) => setError(errMsg(err)));
  }, [setError]);

  const handlePreview = async () => {
    if (!url.trim()) {
      setError(t("gs.extUrlRequired"));
      return;
    }
    setBusy(true);
    setError("");
    try {
      setPreview(await previewExtension(url.trim(), ref.trim()));
    } catch (err) {
      setPreview(null);
      setError(errMsg(err));
    } finally {
      setBusy(false);
    }
  };

  const handleInstall = async () => {
    if (!preview) return;
    setBusy(true);
    setError("");
    try {
      await installExtension(
        url.trim(),
        ref.trim(),
        preview.manifest.scopes ?? [],
        // Pin the commit the consent dialog was rendered from. If the
        // branch moved in between, the install is refused rather than
        // landing code nobody approved.
        preview.commit,
      );
      setPreview(null);
      setUrl("");
      setRef("");
      reload();
      flashSuccess();
    } catch (err) {
      setError(errMsg(err));
    } finally {
      setBusy(false);
    }
  };

  // A handler returns false to mean "the operator backed out" — a
  // cancelled confirm dialog changed nothing, so flashing "saved"
  // would be a lie.
  const withBusy = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    setError("");
    try {
      const changed = await fn();
      reload();
      if (changed !== false) flashSuccess();
    } catch (err) {
      setError(errMsg(err));
      reload();
    } finally {
      setBusy(false);
    }
  };

  const handleUpdate = (ext: InstalledExtension) =>
    withBusy(async () => {
      try {
        await updateExtension(ext.id);
      } catch (err) {
        // A widened scope list is refused until the operator agrees to
        // the new capabilities — surface them instead of retrying.
        if (errMsg(err).includes("scope acknowledgement mismatch")) {
          const fresh = await previewExtension(ext.source, ext.ref ?? "");
          if (
            !window.confirm(
              t("gs.extUpdateScopeConfirm", {
                scopes: (fresh.manifest.scopes ?? []).join(", "),
              }),
            )
          ) {
            return false;
          }
          await updateExtension(ext.id, undefined, fresh.manifest.scopes ?? []);
          return true;
        }
        throw err;
      }
    });

  const handleRemove = (ext: InstalledExtension) => {
    if (!window.confirm(t("gs.extRemoveConfirm", { name: ext.manifest.name })))
      return;
    void withBusy(() => removeExtension(ext.id));
  };

  return (
    <SectionCard
      title={t("gs.extensions")}
      description={t("gs.extensionsHelp")}
    >
      <div className="space-y-5">
        <div className="space-y-3">
          <Field
            label={t("gs.extRepoUrl")}
            htmlFor="ext-url"
            help={t("gs.extRepoUrlHelp")}
          >
            <Input
              id="ext-url"
              mono
              value={url}
              disabled={busy}
              placeholder="https://github.com/owner/kojo-ext-slack.git"
              onChange={(e) => {
                setUrl(e.target.value);
                setPreview(null);
              }}
            />
          </Field>
          <Field
            label={t("gs.extRef")}
            htmlFor="ext-ref"
            help={t("gs.extRefHelp")}
          >
            <Input
              id="ext-ref"
              mono
              value={ref}
              disabled={busy}
              placeholder="v1.0.0"
              onChange={(e) => {
                setRef(e.target.value);
                setPreview(null);
              }}
            />
          </Field>
          <div className="flex gap-2">
            <Button onClick={handlePreview} disabled={busy || !url.trim()}>
              {t("gs.extCheck")}
            </Button>
          </div>
        </div>

        {preview && (
          <ConsentPanel
            preview={preview}
            busy={busy}
            onInstall={handleInstall}
          />
        )}

        <div className="space-y-2">
          <div className="flex items-center justify-between gap-2">
            <h4 className="text-[12px] font-medium text-ink-dim">
              {t("gs.extInstalled")}
            </h4>
            {items.length > 0 && (
              <Button
                onClick={() =>
                  void withBusy(async () => {
                    setUpdates(await checkExtensionUpdates());
                    // Nothing was saved; a "saved" flash would mislead.
                    return false;
                  })
                }
                disabled={busy}
              >
                {t("gs.extCheckUpdates")}
              </Button>
            )}
          </div>
          {items.length === 0 && (
            <p className="text-sm text-ink-faint">{t("gs.extNone")}</p>
          )}
          {items.map((ext) => (
            <ExtensionRow
              key={ext.id}
              ext={ext}
              agents={agents}
              services={services}
              update={updates[ext.id]}
              busy={busy}
              expanded={expanded === ext.id}
              onToggleExpand={() =>
                setExpanded(expanded === ext.id ? null : ext.id)
              }
              onSetEnabled={(v) =>
                void withBusy(() => setExtensionEnabled(ext.id, v))
              }
              onSetAutoUpdate={(v) =>
                void withBusy(() => setExtensionAutoUpdate(ext.id, v))
              }
              onSetAgentBinding={(agentId, enabled) =>
                void withBusy(() =>
                  setExtensionAgentBinding(ext.id, agentId, {
                    enabled,
                    config: ext.agents?.[agentId]?.config,
                  }),
                )
              }
              onUpdate={() => void handleUpdate(ext)}
              onRemove={() => handleRemove(ext)}
              setError={setError}
              flashSuccess={flashSuccess}
            />
          ))}
        </div>
      </div>
    </SectionCard>
  );
}

/** Scope-consent panel shown between preview and install. */
function ConsentPanel({
  preview,
  busy,
  onInstall,
}: {
  preview: ExtensionPreview;
  busy: boolean;
  onInstall: () => void;
}) {
  const t = useT();
  const m = preview.manifest;
  return (
    <div className="space-y-3 rounded-[10px] border border-copper/40 bg-raised p-3">
      <div>
        <div className="text-sm font-semibold text-ink">
          {m.name} <span className="font-normal text-ink-dim">{m.version}</span>
        </div>
        <div className="font-mono text-[12px] text-ink-faint">{m.id}</div>
        {m.description && (
          <p className="mt-1 text-[13px] text-ink-dim">{m.description}</p>
        )}
      </div>

      <div>
        <div className="text-[12px] font-medium text-ink-dim">
          {t("gs.extContributes")}
        </div>
        <ul className="mt-1 space-y-0.5 text-[13px] text-ink-dim">
          {(m.contributes.skills ?? []).length > 0 && (
            <li>skills: {(m.contributes.skills ?? []).join(", ")}</li>
          )}
          {(m.contributes.mcpServers ?? []).length > 0 && (
            <li>
              mcp:{" "}
              {(m.contributes.mcpServers ?? []).map((s) => s.name).join(", ")}
            </li>
          )}
          {m.contributes.service && (
            <li>service ({m.contributes.service.scope})</li>
          )}
          {(m.contributes.locales ?? []).length > 0 && (
            <li>
              locales:{" "}
              {(m.contributes.locales ?? [])
                .map((l) => `${l.name} (${l.tag})`)
                .join(", ")}
            </li>
          )}
          {m.contributes.settings && (
            <li>settings ({m.contributes.settings.scope})</li>
          )}
        </ul>
      </div>

      <div>
        <div className="text-[12px] font-medium text-ink-dim">
          {t("gs.extScopes")}
        </div>
        {preview.scopes.length === 0 ? (
          <p className="text-[13px] text-ink-faint">{t("gs.extScopesNone")}</p>
        ) : (
          <ul className="mt-1 space-y-0.5 text-[13px] text-ink">
            {preview.scopes.map((s) => (
              <li key={s.scope}>
                <span className="font-mono text-[12px] text-ink-dim">
                  {s.scope}
                </span>
                {s.description ? ` — ${s.description}` : ""}
              </li>
            ))}
          </ul>
        )}
      </div>

      {preview.installed ? (
        <Banner tone="warn">{t("gs.extAlreadyInstalled")}</Banner>
      ) : (
        <div className="flex gap-2">
          <Button variant="primary" onClick={onInstall} disabled={busy}>
            {t("gs.extInstall")}
          </Button>
        </div>
      )}
    </div>
  );
}

/** One installed package: status, actions, and global settings form. */
function ExtensionRow({
  ext,
  agents,
  services,
  update,
  busy,
  expanded,
  onToggleExpand,
  onSetEnabled,
  onSetAutoUpdate,
  onSetAgentBinding,
  onUpdate,
  onRemove,
  setError,
  flashSuccess,
}: {
  ext: InstalledExtension;
  agents: AgentInfo[];
  services: ExtensionServiceStatus[];
  update?: ExtensionUpdateStatus;
  busy: boolean;
  expanded: boolean;
  onToggleExpand: () => void;
  onSetEnabled: (v: boolean) => void;
  onSetAutoUpdate: (v: boolean) => void;
  onSetAgentBinding: (agentId: string, enabled: boolean) => void;
  onUpdate: () => void;
  onRemove: () => void;
  setError: (msg: string) => void;
  flashSuccess: () => void;
}) {
  const t = useT();
  const settings = ext.manifest.contributes.settings;
  const hasGlobalSettings = settings?.scope === "global";
  const hasPerAgentSettings = settings?.scope === "per-agent";
  const service = ext.manifest.contributes.service;
  const running = services.filter((s) => s.extensionId === ext.id);
  // A per-agent binding gates skills, MCP servers and services alike,
  // so the list is worth showing whenever the package contributes
  // anything an agent can see.
  const contributesToAgents =
    (ext.manifest.contributes.skills ?? []).length > 0 ||
    (ext.manifest.contributes.mcpServers ?? []).length > 0 ||
    service?.scope === "per-agent" ||
    hasPerAgentSettings;

  return (
    <div className="rounded-[10px] border border-hairline bg-raised p-3">
      <div className="flex items-start justify-between gap-3">
        {/* The whole header (not just the text) toggles the details, and
            the chevron says there is something to open or close. */}
        <button
          type="button"
          className="group flex min-w-0 flex-1 items-start gap-2 text-left"
          onClick={onToggleExpand}
          aria-expanded={expanded}
          title={expanded ? t("dash.collapse") : t("dash.expand")}
        >
          <svg
            aria-hidden
            className={`mt-1 h-3 w-3 shrink-0 text-ink-faint transition-transform group-hover:text-ink-dim motion-reduce:transition-none ${expanded ? "rotate-90" : ""}`}
            fill="none"
            viewBox="0 0 24 24"
            stroke="currentColor"
            strokeWidth={2}
          >
            <path strokeLinecap="round" strokeLinejoin="round" d="M9 5l7 7-7 7" />
          </svg>
          <div className="min-w-0 flex-1">
            <div className="text-sm font-medium text-ink">
              {ext.manifest.name}{" "}
              <span className="font-normal text-ink-dim">
                {ext.manifest.version}
              </span>
              {update?.available && (
                <span
                  className="ml-1.5 inline-flex items-center whitespace-nowrap rounded-full border border-copper/50 px-1.5 align-middle text-[10.5px] font-normal text-copper"
                  title={update.latest}
                >
                  {t("gs.extUpdateAvailable")}
                </span>
              )}
            </div>
            <div className="truncate font-mono text-[12px] text-ink-faint">
              {ext.id} · {ext.commit.slice(0, 7)}
              {ext.ref ? ` · ${ext.ref}` : ""}
            </div>
            {service && (
              <div className="mt-1 flex items-center gap-1.5 text-[12px] text-ink-dim">
                <span
                  aria-hidden
                  className={`inline-block h-1.5 w-1.5 rounded-full ${
                    running.length > 0 ? "bg-emerald-500" : "bg-ink-faint"
                  }`}
                />
                {running.length > 0
                  ? `${t("gs.extServiceRunning")}${running.length > 1 ? ` (${running.length})` : ""}`
                  : t("gs.extServiceStopped")}
              </div>
            )}
          </div>
        </button>
        <Toggle
          checked={ext.enabled}
          disabled={busy}
          aria-label={t("gs.extEnable")}
          onChange={onSetEnabled}
        />
      </div>

      {expanded && (
        <div className="mt-3 space-y-3 border-t border-hairline pt-3">
          <div className="break-all font-mono text-[12px] text-ink-faint">
            {ext.source}
          </div>
          {(ext.grantedScopes ?? []).length > 0 && (
            <div className="text-[12px] text-ink-dim">
              {t("gs.extScopes")}: {(ext.grantedScopes ?? []).join(", ")}
            </div>
          )}
          {hasGlobalSettings && (
            <ExtensionConfigForm
              ext={ext}
              setError={setError}
              flashSuccess={flashSuccess}
            />
          )}
          {contributesToAgents && (
            <AgentBindings
              ext={ext}
              agents={agents}
              busy={busy}
              perAgentSettings={hasPerAgentSettings}
              onSetAgentBinding={onSetAgentBinding}
              setError={setError}
              flashSuccess={flashSuccess}
            />
          )}
          <ExtensionTokenPanel ext={ext} setError={setError} />
          <ExtensionUpdatePanel
            ext={ext}
            update={update}
            busy={busy}
            onSetAutoUpdate={onSetAutoUpdate}
          />
          <div className="flex gap-2">
            <Button onClick={onUpdate} disabled={busy}>
              {t("gs.extUpdate")}
            </Button>
            <Button variant="danger" onClick={onRemove} disabled={busy}>
              {t("gs.extRemove")}
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}

/**
 * Update status and the auto-update opt-in. Auto-update follows the
 * package's ref (branch, tag, or the default branch); a ref pinned to
 * a commit has nothing to follow.
 */
function ExtensionUpdatePanel({
  ext,
  update,
  busy,
  onSetAutoUpdate,
}: {
  ext: InstalledExtension;
  update?: ExtensionUpdateStatus;
  busy: boolean;
  onSetAutoUpdate: (v: boolean) => void;
}) {
  const t = useT();
  let state: string;
  if (!update) state = t("gs.extUpdateNotChecked");
  else if (update.error) state = t("gs.extUpdateCheckFailed");
  else if (update.pinned) state = t("gs.extUpdatePinned");
  else if (update.available)
    state = t("gs.extUpdateAvailableCommit", {
      commit: (update.latest ?? "").slice(0, 7),
    });
  else state = t("gs.extUpdateUpToDate");

  return (
    <div className="space-y-1.5">
      <div className="flex items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="text-[12px] font-medium text-ink-dim">
            {t("gs.extAutoUpdate")}
          </div>
          <p className="text-[12px] text-ink-faint">
            {t("gs.extAutoUpdateHelp")}
          </p>
        </div>
        <Toggle
          checked={ext.autoUpdate === true}
          disabled={busy}
          aria-label={t("gs.extAutoUpdate")}
          onChange={onSetAutoUpdate}
        />
      </div>
      <div className="flex items-center gap-2 text-[12px] text-ink-dim">
        <span>{state}</span>
        {update?.checkedAt && <RelTime value={update.checkedAt} />}
      </div>
      {update?.error && (
        <p className="break-all font-mono text-[11px] text-ink-faint">
          {update.error}
        </p>
      )}
      {ext.autoUpdate && update?.needsConsent && (
        <Banner tone="warn">{t("gs.extAutoUpdateNeedsConsent")}</Banner>
      )}
      {ext.autoUpdate && update?.autoError && !update.needsConsent && (
        <Banner tone="warn">
          {t("gs.extAutoUpdateFailed", { error: update.autoError })}
        </Banner>
      )}
    </div>
  );
}

/**
 * Per-agent enablement. Global enablement alone gives a package
 * nothing agent-shaped: its token resolves to no agents, its skills are
 * copied nowhere and its per-agent service does not start until an
 * agent is ticked here.
 */
function AgentBindings({
  ext,
  agents,
  busy,
  perAgentSettings,
  onSetAgentBinding,
  setError,
  flashSuccess,
}: {
  ext: InstalledExtension;
  agents: AgentInfo[];
  busy: boolean;
  perAgentSettings: boolean;
  onSetAgentBinding: (agentId: string, enabled: boolean) => void;
  setError: (msg: string) => void;
  flashSuccess: () => void;
}) {
  const t = useT();
  const [open, setOpen] = useState<string | null>(null);

  return (
    <div className="space-y-2">
      <div>
        <div className="text-[12px] font-medium text-ink-dim">
          {t("gs.extAgents")}
        </div>
        <p className="text-[12px] text-ink-faint">{t("gs.extAgentsHelp")}</p>
      </div>
      {agents.length === 0 && <p className="text-[13px] text-ink-faint">—</p>}
      {agents.map((a) => {
        const bound = ext.agents?.[a.id]?.enabled === true;
        return (
          <div
            key={a.id}
            className="rounded-[8px] border border-hairline px-2.5 py-2"
          >
            <div className="flex items-center justify-between gap-3">
              <div className="min-w-0">
                <div className="truncate text-[13px] text-ink">{a.name}</div>
                <div className="truncate font-mono text-[11px] text-ink-faint">
                  {a.id}
                </div>
              </div>
              <div className="flex items-center gap-2">
                {bound && perAgentSettings && (
                  <Button
                    onClick={() => setOpen(open === a.id ? null : a.id)}
                    aria-expanded={open === a.id}
                  >
                    {t("gs.extAgentSettings")}
                  </Button>
                )}
                <Toggle
                  checked={bound}
                  disabled={busy}
                  aria-label={`${t("gs.extEnable")} — ${a.name}`}
                  onChange={(v) => onSetAgentBinding(a.id, v)}
                />
              </div>
            </div>
            {bound && perAgentSettings && open === a.id && (
              <div className="mt-2 border-t border-hairline pt-2">
                <ExtensionConfigForm
                  ext={ext}
                  agentId={a.id}
                  setError={setError}
                  flashSuccess={flashSuccess}
                />
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}

/**
 * The package's bearer token. Hidden until asked for, because the whole
 * settings page is otherwise safe to show on a shared screen.
 */
function ExtensionTokenPanel({
  ext,
  setError,
}: {
  ext: InstalledExtension;
  setError: (msg: string) => void;
}) {
  const t = useT();
  const [token, setToken] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // The token belongs to the package, so a different row must never
  // keep showing the previous one.
  useEffect(() => setToken(null), [ext.id]);

  const reveal = async () => {
    setBusy(true);
    setError("");
    try {
      setToken((await getExtensionToken(ext.id)).token);
    } catch (err) {
      setError(errMsg(err));
    } finally {
      setBusy(false);
    }
  };

  const rotate = async () => {
    if (!window.confirm(t("gs.extTokenRotateConfirm"))) return;
    setBusy(true);
    setError("");
    try {
      setToken((await rotateExtensionToken(ext.id)).token);
    } catch (err) {
      setError(errMsg(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-1.5">
      <div className="text-[12px] font-medium text-ink-dim">
        {t("gs.extToken")}
      </div>
      <p className="text-[12px] text-ink-faint">{t("gs.extTokenHelp")}</p>
      {token && (
        <div className="break-all rounded-[8px] bg-sunken px-2 py-1.5 font-mono text-[12px] text-ink">
          {token}
        </div>
      )}
      <div className="flex gap-2">
        <Button
          onClick={() => (token ? setToken(null) : void reveal())}
          disabled={busy}
        >
          {token ? t("gs.extTokenHide") : t("gs.extTokenShow")}
        </Button>
        <Button variant="danger" onClick={() => void rotate()} disabled={busy}>
          {t("gs.extTokenRotate")}
        </Button>
      </div>
    </div>
  );
}

/** Lazily-loaded settings form driven by the package's JSON Schema. */
function ExtensionConfigForm({
  ext,
  agentId,
  setError,
  flashSuccess,
}: {
  ext: InstalledExtension;
  /** Present for a per-agent settings form; absent for the global one. */
  agentId?: string;
  setError: (msg: string) => void;
  flashSuccess: () => void;
}) {
  const t = useT();
  const [schema, setSchema] = useState<JSONSchema | null>(null);
  const [draft, setDraft] = useState<Record<string, unknown>>(
    (agentId ? ext.agents?.[agentId]?.config : ext.config) ?? {},
  );
  const [saving, setSaving] = useState(false);
  // Which secrets the server holds. Tracked here as well as read from
  // the row: the list this section was rendered from is not refetched
  // on save, and without the update a just-saved secret would offer no
  // way to remove it until the page was reloaded.
  const rowSecrets = agentId ? ext.agents?.[agentId]?.secrets : ext.secrets;
  const [storedSecrets, setStoredSecrets] = useState<string[]>(rowSecrets ?? []);
  useEffect(() => {
    setStoredSecrets(rowSecrets ?? []);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [(rowSecrets ?? []).join("\n")]);

  useEffect(() => {
    let live = true;
    getExtensionSchema(ext.id)
      .then((s) => {
        if (live) setSchema(s);
      })
      .catch((err) => setError(errMsg(err)));
    return () => {
      live = false;
    };
  }, [ext.id, setError]);

  if (!schema) return null;

  const save = async () => {
    setSaving(true);
    setError("");
    try {
      // The form shows a schema default for anything left unset, so
      // save what is shown: a package must not get "no value" for a
      // control the operator saw filled in. Secrets have no default.
      const config: Record<string, unknown> = { ...draft };
      for (const [key, prop] of Object.entries(schema.properties ?? {})) {
        if (config[key] === undefined && prop.default !== undefined && prop.format !== "password") {
          config[key] = prop.default;
        }
      }
      let saved: InstalledExtension;
      if (agentId) {
        // The binding carries the config, so it has to be resent with
        // enabled:true — a bare config write would unbind the agent.
        saved = await setExtensionAgentBinding(ext.id, agentId, {
          enabled: true,
          config,
        });
      } else {
        saved = await setExtensionConfig(ext.id, config);
      }
      setStoredSecrets((agentId ? saved.agents?.[agentId]?.secrets : saved.secrets) ?? []);
      // Secrets are write-only: once saved, drop them from the draft so
      // the field goes back to "saved" instead of holding the value.
      setDraft((d) => {
        const next = { ...d };
        for (const [key, prop] of Object.entries(schema.properties ?? {})) {
          if (prop.type === "string" && prop.format === "password") delete next[key];
        }
        return next;
      });
      flashSuccess();
    } catch (err) {
      setError(errMsg(err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="space-y-3">
      <SchemaForm
        schema={schema}
        value={draft}
        onChange={setDraft}
        disabled={saving}
        secretsSet={storedSecrets}
        idPrefix={`ext-${ext.id}${agentId ? `-${agentId}` : ""}`}
      />
      <Button onClick={() => void save()} disabled={saving}>
        {t("gs.extSave")}
      </Button>
    </div>
  );
}

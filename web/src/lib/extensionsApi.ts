// kojo extension packages — client for /api/v1/extensions.
//
// An extension is a git repository containing a kojo-package.json that
// contributes skills, MCP servers, a supervised service process and/or
// a declarative settings form. kojo ships as a single binary with a
// prebuilt web bundle, so a package can never inject code into either;
// everything it adds is data on disk or an out-of-process service.
//
// Installing is deliberately two steps — preview (fetch + parse the
// manifest) then install (with the operator's explicit scope
// acknowledgement) — so nothing is registered before consent.

import { get, post, put, patch, del } from "./httpClient";

/** A capability an extension may request, with its display label. */
export interface ScopeSummary {
  scope: string;
  description: string;
}

export interface ExtensionMCPServer {
  name: string;
  command?: string;
  exec?: Record<string, string>;
  args?: string[];
}

export interface ExtensionService {
  scope: "global" | "per-agent";
  exec: Record<string, string>;
}

export interface ExtensionSettings {
  scope: "global" | "per-agent";
  schema: string;
}

/** A UI language a package adds to the language picker. */
export interface ExtensionLocaleFile {
  tag: string;
  name: string;
  file: string;
}

export interface ExtensionContributes {
  skills?: string[];
  mcpServers?: ExtensionMCPServer[];
  service?: ExtensionService;
  locales?: ExtensionLocaleFile[];
  settings?: ExtensionSettings;
}

export interface ExtensionManifest {
  id: string;
  name: string;
  version: string;
  description?: string;
  homepage?: string;
  kojoVersion?: string;
  scopes?: string[];
  contributes: ExtensionContributes;
}

export interface ExtensionAgentBinding {
  enabled: boolean;
  config?: Record<string, unknown>;
  /** Names of the secret settings stored for this agent. Values are never returned. */
  secrets?: string[];
}

export interface InstalledExtension {
  id: string;
  source: string;
  ref?: string;
  commit: string;
  installedAt: string;
  updatedAt: string;
  enabled: boolean;
  /**
   * Opted into unattended updates: the periodic check applies a new
   * commit on the package's ref by itself, except one that changes the
   * scope set or needs a newer kojo. Off (absent) by default.
   */
  autoUpdate?: boolean;
  grantedScopes?: string[];
  config?: Record<string, unknown>;
  /** Names of the global secret settings that are set. Values are never returned. */
  secrets?: string[];
  agents?: Record<string, ExtensionAgentBinding>;
  manifest: ExtensionManifest;
}

export interface ExtensionPreview {
  manifest: ExtensionManifest;
  scopes: ScopeSummary[];
  /**
   * The commit this manifest was read from. Passed back to
   * installExtension so a branch that moves between the consent dialog
   * and the install is refused instead of quietly landing code the
   * operator never saw.
   */
  commit: string;
  /** true when a package with this id is already installed. */
  installed: boolean;
}

/**
 * A supervised service process that is currently up. Runtime state, not
 * registry state: a package can be enabled and still have a service
 * that keeps crashing, and that difference is what the operator needs
 * to see.
 */
export interface ExtensionServiceStatus {
  extensionId: string;
  agentId?: string;
}

/**
 * What the last upstream check found for one package. The daemon asks
 * each package's remote (git ls-remote) every few hours; a package
 * missing from the map has not been checked since kojo started.
 */
export interface ExtensionUpdateStatus {
  /** Commit the package's ref points at on the remote. */
  latest?: string;
  /** latest differs from the installed commit. */
  available: boolean;
  /** The ref is a commit SHA, so there is nothing to follow. */
  pinned?: boolean;
  /** RFC 3339. */
  checkedAt?: string;
  /** Why the last check failed. */
  error?: string;
  /** Why auto-update did not apply `latest`. */
  autoError?: string;
  /** `latest` changes the scope set; update by hand to consent. */
  needsConsent?: boolean;
}

export interface ExtensionList {
  extensions: InstalledExtension[];
  services: ExtensionServiceStatus[];
  updates: Record<string, ExtensionUpdateStatus>;
}

export async function listExtensions(): Promise<ExtensionList> {
  const res = await get<Partial<ExtensionList>>("/api/v1/extensions");
  return {
    extensions: res.extensions ?? [],
    services: res.services ?? [],
    updates: res.updates ?? {},
  };
}

/** Ask every package's remote now. Checks only; never applies. */
export async function checkExtensionUpdates(): Promise<
  Record<string, ExtensionUpdateStatus>
> {
  const res = await post<{ updates?: Record<string, ExtensionUpdateStatus> }>(
    "/api/v1/extensions/check-updates",
    {},
  );
  return res.updates ?? {};
}

export function previewExtension(
  url: string,
  ref: string,
): Promise<ExtensionPreview> {
  return post<ExtensionPreview>("/api/v1/extensions/preview", { url, ref });
}

// commit is required, not optional: the server refuses an install
// without it, because the commit is what ties the operator's consent to
// the code that actually lands. It always comes from previewExtension.
export function installExtension(
  url: string,
  ref: string,
  ackScopes: string[],
  commit: string,
): Promise<InstalledExtension> {
  return post<InstalledExtension>("/api/v1/extensions", {
    url,
    ref,
    ackScopes,
    commit,
  });
}

export function updateExtension(
  id: string,
  ref?: string,
  ackScopes?: string[],
): Promise<InstalledExtension> {
  return post<InstalledExtension>(
    `/api/v1/extensions/${encodeURIComponent(id)}/update`,
    {
      ref: ref ?? "",
      ackScopes: ackScopes ?? [],
    },
  );
}

export function setExtensionEnabled(
  id: string,
  enabled: boolean,
): Promise<InstalledExtension> {
  return patch<InstalledExtension>(
    `/api/v1/extensions/${encodeURIComponent(id)}`,
    { enabled },
  );
}

export function setExtensionAutoUpdate(
  id: string,
  autoUpdate: boolean,
): Promise<InstalledExtension> {
  return patch<InstalledExtension>(
    `/api/v1/extensions/${encodeURIComponent(id)}`,
    { autoUpdate },
  );
}

export function removeExtension(id: string): Promise<void> {
  return del<void>(`/api/v1/extensions/${encodeURIComponent(id)}`);
}

export function getExtensionSchema(id: string): Promise<JSONSchema> {
  return get<JSONSchema>(`/api/v1/extensions/${encodeURIComponent(id)}/schema`);
}

export function setExtensionConfig(
  id: string,
  config: Record<string, unknown>,
): Promise<InstalledExtension> {
  return put<InstalledExtension>(
    `/api/v1/extensions/${encodeURIComponent(id)}/config`,
    config,
  );
}

export function setExtensionAgentBinding(
  id: string,
  agentId: string,
  binding: ExtensionAgentBinding,
): Promise<InstalledExtension> {
  return put<InstalledExtension>(
    `/api/v1/extensions/${encodeURIComponent(id)}/agents/${encodeURIComponent(agentId)}`,
    binding,
  );
}

export interface ExtensionToken {
  id: string;
  token: string;
  apiBase?: string;
}

/** Reveals the package's bearer token. Owner-only, like every route here. */
export function getExtensionToken(id: string): Promise<ExtensionToken> {
  return get<ExtensionToken>(
    `/api/v1/extensions/${encodeURIComponent(id)}/token`,
  );
}

/**
 * Issues a fresh token. The old one stops working immediately and the
 * daemon restarts the package's service so it picks the new one up.
 */
export function rotateExtensionToken(id: string): Promise<ExtensionToken> {
  return post<ExtensionToken>(
    `/api/v1/extensions/${encodeURIComponent(id)}/token`,
    {},
  );
}

/**
 * The subset of JSON Schema the settings form renders. Anything richer
 * (oneOf, nested objects, arrays of objects) is intentionally out of
 * scope for now: a package needing it should expose its own UI later
 * rather than pushing complexity into this renderer.
 */
export interface JSONSchema {
  type?: string;
  title?: string;
  description?: string;
  required?: string[];
  properties?: Record<string, JSONSchemaProperty>;
}

export interface JSONSchemaProperty {
  type?: "string" | "number" | "integer" | "boolean";
  title?: string;
  description?: string;
  enum?: string[];
  default?: unknown;
  /** Renders a multi-line control instead of a single-line input. */
  multiline?: boolean;
  /** "password" masks the value in the form. */
  format?: string;
}

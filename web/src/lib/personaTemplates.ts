// Persona tone templates for the task-first agent-create flow.
//
// Two sources merge into one picker list:
//   - Built-ins: defined here, localized via i18n, not editable.
//   - Custom: owner-managed rows in the generic kv store under the
//     `persona-templates` namespace (value = JSON {name, persona},
//     type "json", scope "global" so they sync across peers).
//
// The kv wire surface (GET/PUT/DELETE /api/v1/kv/{ns}/{key} + list)
// already exists server-side, so this module is the whole backend
// contract for custom templates.

import { get, putWithIfMatch, putCreateOnly, delWithIfMatch } from "./httpClient";
import { t } from "./i18n";

export interface PersonaTemplate {
  id: string;
  name: string;
  persona: string;
  /** true for kv-backed templates (editable in global settings). */
  custom: boolean;
  /** kv row etag — threaded into If-Match on update/delete. */
  etag?: string;
}

const KV_NAMESPACE = "persona-templates";

/** Sentinel tone choices that are not templates. */
export const TONE_AUTO = "auto"; // derive persona from the task via AI
export const TONE_MODEL_DEFAULT = "default"; // no persona — model's default voice

interface KVListItem {
  key: string;
  value?: string;
  etag?: string;
}

/**
 * Built-in tone templates. A function (not a constant) so the texts
 * re-resolve when the UI locale changes.
 */
export function builtinTemplates(): PersonaTemplate[] {
  return [
    {
      id: "builtin-polite",
      name: t("tone.polite"),
      persona: t("tone.politePersona"),
      custom: false,
    },
    {
      id: "builtin-casual",
      name: t("tone.casual"),
      persona: t("tone.casualPersona"),
      custom: false,
    },
    {
      id: "builtin-taciturn",
      name: t("tone.taciturn"),
      persona: t("tone.taciturnPersona"),
      custom: false,
    },
  ];
}

/**
 * Parses one kv row into a template. Returns null for malformed
 * values (hand-edited kv, older schema) so one bad row doesn't take
 * the whole picker down.
 */
export function parseTemplateRow(
  key: string,
  value: string | undefined,
  etag?: string,
): PersonaTemplate | null {
  if (!value) return null;
  try {
    const parsed: unknown = JSON.parse(value);
    if (typeof parsed !== "object" || parsed === null) return null;
    const rec = parsed as Record<string, unknown>;
    if (typeof rec.name !== "string" || typeof rec.persona !== "string") return null;
    const name = rec.name.trim();
    // A blank persona would silently degrade the tone choice to
    // "model default" — refuse the row instead.
    if (!name || !rec.persona.trim()) return null;
    return { id: key, name, persona: rec.persona, custom: true, etag };
  } catch {
    return null;
  }
}

export async function listCustomTemplates(): Promise<PersonaTemplate[]> {
  const res = await get<{ items: KVListItem[] }>(`/api/v1/kv/${KV_NAMESPACE}`);
  return (res.items ?? [])
    .map((it) => parseTemplateRow(it.key, it.value, it.etag))
    .filter((tpl): tpl is PersonaTemplate => tpl !== null)
    .sort((a, b) => a.name.localeCompare(b.name));
}

/**
 * Creates (id omitted; create-only CAS via If-None-Match: *) or
 * updates (If-Match against the row's etag) a custom template.
 * Throws PreconditionFailedError on a stale etag / key collision —
 * callers should reload the list and let the user retry.
 * Returns the row id.
 */
export async function saveCustomTemplate(
  id: string | undefined,
  name: string,
  persona: string,
  etag?: string,
): Promise<string> {
  const key = id || `tpl-${crypto.randomUUID()}`;
  const body = {
    value: JSON.stringify({ name: name.trim(), persona }),
    type: "json",
    scope: "global",
  };
  const path = `/api/v1/kv/${KV_NAMESPACE}/${encodeURIComponent(key)}`;
  if (id) {
    await putWithIfMatch(path, body, etag || undefined);
  } else {
    await putCreateOnly(path, body);
  }
  return key;
}

export async function deleteCustomTemplate(id: string, etag?: string): Promise<void> {
  await delWithIfMatch(`/api/v1/kv/${KV_NAMESPACE}/${encodeURIComponent(id)}`, etag || undefined);
}

/**
 * Resolves a tone choice to the persona text sent on create.
 *   - TONE_MODEL_DEFAULT → "" (model's default voice, no persona)
 *   - TONE_AUTO → the AI-derived persona the user generated/edited
 *   - anything else → the matching template's persona ("" if the
 *     template vanished, e.g. deleted on another device)
 */
export function resolveTonePersona(
  tone: string,
  autoPersona: string,
  templates: PersonaTemplate[],
): string {
  if (tone === TONE_MODEL_DEFAULT) return "";
  if (tone === TONE_AUTO) return autoPersona;
  return templates.find((tpl) => tpl.id === tone)?.persona ?? "";
}

/**
 * Builds the generate-persona prompt that derives a fitting character
 * from the task description. Japanese on purpose: every server-side
 * generation prompt (agent.GeneratePersona et al.) is Japanese, and
 * this string is prompt material, not UI copy.
 */
export function buildTaskPersonaPrompt(mission: string, hint: string): string {
  let prompt =
    "以下のタスクを専門に担当するエージェントの人物像を作成してください。タスク内容からふさわしい性格・口調・仕事の流儀を逆算すること。\n\n## タスク\n" +
    mission.trim();
  if (hint.trim()) {
    prompt += "\n\n## 追加要望\n" + hint.trim();
  }
  return prompt;
}

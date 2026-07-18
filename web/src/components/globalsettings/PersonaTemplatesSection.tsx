import { useCallback, useEffect, useState } from "react";
import {
  listCustomTemplates,
  saveCustomTemplate,
  deleteCustomTemplate,
  type PersonaTemplate,
} from "../../lib/personaTemplates";
import { errMsg } from "../../lib/utils";
import { useT } from "../../lib/i18n";
import { SectionCard } from "../ui/SectionCard";
import { Field } from "../ui/Field";
import { Input } from "../ui/Input";
import { Textarea } from "../ui/Textarea";
import { Button } from "../ui/Button";

interface EditorState {
  id?: string; // undefined = creating a new template
  name: string;
  persona: string;
  etag?: string; // If-Match precondition for updates
}

/**
 * CRUD for the custom persona templates offered in the task-first
 * AgentCreate tone picker. Built-in templates are code-defined and
 * intentionally absent here. Storage: kv `persona-templates` rows
 * (scope global, synced across peers) — see lib/personaTemplates.ts.
 */
export function PersonaTemplatesSection({
  setError,
  flashSuccess,
}: {
  setError: (msg: string) => void;
  flashSuccess: () => void;
}) {
  const t = useT();
  const [items, setItems] = useState<PersonaTemplate[]>([]);
  const [editor, setEditor] = useState<EditorState | null>(null);
  const [busy, setBusy] = useState(false);

  const reload = useCallback(() => {
    listCustomTemplates()
      .then(setItems)
      .catch((err) => setError(errMsg(err)));
  }, [setError]);

  useEffect(() => {
    reload();
  }, [reload]);

  const handleSave = async () => {
    if (!editor) return;
    if (!editor.name.trim()) {
      setError(t("gs.tplNameRequired"));
      return;
    }
    if (!editor.persona.trim()) {
      setError(t("gs.tplPersonaRequired"));
      return;
    }
    setBusy(true);
    setError("");
    try {
      await saveCustomTemplate(editor.id, editor.name, editor.persona, editor.etag);
      setEditor(null);
      reload();
      flashSuccess();
    } catch (err) {
      // A stale etag (concurrent edit on another device) surfaces as
      // PreconditionFailedError. Refresh the list AND the open
      // editor's etag — keeping the stale one would make every retry
      // fail 412 forever; adopting the fresh etag makes the user's
      // next save an informed overwrite of the concurrent edit.
      setError(errMsg(err));
      try {
        const fresh = await listCustomTemplates();
        setItems(fresh);
        if (editor.id) {
          const row = fresh.find((tpl) => tpl.id === editor.id);
          setEditor((cur) => {
            if (!cur || cur.id !== editor.id) return cur;
            // Row deleted concurrently → drop id/etag so the retry
            // becomes an explicit create (If-None-Match: * under a
            // fresh key) instead of a bare PUT that 428s in strict
            // mode or silently resurrects the deleted key.
            if (!row) return { name: cur.name, persona: cur.persona };
            return { ...cur, etag: row.etag };
          });
        }
      } catch {
        // refresh is best-effort; the original error is already shown
      }
    } finally {
      setBusy(false);
    }
  };

  const handleDelete = async (tpl: PersonaTemplate) => {
    if (!window.confirm(t("gs.tplDeleteConfirm", { name: tpl.name }))) return;
    setBusy(true);
    setError("");
    try {
      await deleteCustomTemplate(tpl.id, tpl.etag);
      if (editor?.id === tpl.id) setEditor(null);
      reload();
      flashSuccess();
    } catch (err) {
      setError(errMsg(err));
      reload();
    } finally {
      setBusy(false);
    }
  };

  return (
    <SectionCard title={t("gs.personaTemplates")}>
      <div className="space-y-4">
        <p className="text-sm text-ink-faint">{t("gs.personaTemplatesHelp")}</p>

        {items.length === 0 && !editor && (
          <p className="text-sm text-ink-faint">{t("gs.tplEmpty")}</p>
        )}

        {items.length > 0 && (
          <ul className="divide-y divide-hairline rounded-lg border border-hairline">
            {items.map((tpl) => (
              <li key={tpl.id} className="flex items-center gap-2 px-3 py-2">
                <span className="min-w-0 flex-1 truncate text-sm">{tpl.name}</span>
                <Button
                  onClick={() =>
                    setEditor({ id: tpl.id, name: tpl.name, persona: tpl.persona, etag: tpl.etag })
                  }
                  disabled={busy}
                  className="shrink-0"
                >
                  {t("gs.tplEdit")}
                </Button>
                <Button onClick={() => handleDelete(tpl)} disabled={busy} className="shrink-0">
                  {t("gs.tplDelete")}
                </Button>
              </li>
            ))}
          </ul>
        )}

        {editor ? (
          <div className="space-y-3 rounded-lg border border-hairline bg-raised p-3">
            <Field label={t("gs.tplName")}>
              <Input
                value={editor.name}
                onChange={(e) => setEditor({ ...editor, name: e.target.value })}
                placeholder={t("gs.tplName")}
              />
            </Field>
            <Field label={t("gs.tplPersona")}>
              <Textarea
                value={editor.persona}
                onChange={(e) => setEditor({ ...editor, persona: e.target.value })}
                placeholder={t("create.personaPlaceholder")}
                rows={6}
              />
            </Field>
            <div className="flex gap-2">
              <Button variant="primary" onClick={handleSave} disabled={busy}>
                {t("gs.tplSave")}
              </Button>
              <Button onClick={() => setEditor(null)} disabled={busy}>
                {t("gs.tplCancel")}
              </Button>
            </div>
          </div>
        ) : (
          <Button onClick={() => setEditor({ name: "", persona: "" })} disabled={busy}>
            {t("gs.tplAdd")}
          </Button>
        )}
      </div>
    </SectionCard>
  );
}

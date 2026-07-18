import { describe, expect, it } from "vitest";
import {
  TONE_AUTO,
  TONE_MODEL_DEFAULT,
  parseTemplateRow,
  resolveTonePersona,
  buildTaskPersonaPrompt,
  builtinTemplates,
  type PersonaTemplate,
} from "./personaTemplates";

describe("parseTemplateRow", () => {
  it("parses a well-formed kv row and threads the etag", () => {
    const tpl = parseTemplateRow("tpl-1", JSON.stringify({ name: "騎士", persona: "- 忠実" }), "e1");
    expect(tpl).toEqual({ id: "tpl-1", name: "騎士", persona: "- 忠実", custom: true, etag: "e1" });
  });

  it("rejects malformed rows instead of throwing", () => {
    expect(parseTemplateRow("k", undefined)).toBeNull(); // secret/absent value
    expect(parseTemplateRow("k", "not json")).toBeNull();
    expect(parseTemplateRow("k", "42")).toBeNull(); // JSON but not an object
    expect(parseTemplateRow("k", "null")).toBeNull();
    expect(parseTemplateRow("k", JSON.stringify({ name: "x" }))).toBeNull(); // missing persona
    expect(parseTemplateRow("k", JSON.stringify({ name: 1, persona: "p" }))).toBeNull();
    expect(parseTemplateRow("k", JSON.stringify({ name: "  ", persona: "p" }))).toBeNull(); // blank name
    expect(parseTemplateRow("k", JSON.stringify({ name: "x", persona: "  " }))).toBeNull(); // blank persona
  });

  it("trims the display name but preserves the persona verbatim", () => {
    const tpl = parseTemplateRow("k", JSON.stringify({ name: " A ", persona: "  keep  " }));
    expect(tpl?.name).toBe("A");
    expect(tpl?.persona).toBe("  keep  ");
  });
});

describe("resolveTonePersona", () => {
  const templates: PersonaTemplate[] = [
    { id: "tpl-a", name: "A", persona: "persona-a", custom: true },
  ];

  it("model default resolves to empty persona", () => {
    expect(resolveTonePersona(TONE_MODEL_DEFAULT, "ignored", templates)).toBe("");
  });

  it("auto resolves to the user-edited derived persona", () => {
    expect(resolveTonePersona(TONE_AUTO, "derived", templates)).toBe("derived");
  });

  it("template id resolves to that template's persona", () => {
    expect(resolveTonePersona("tpl-a", "ignored", templates)).toBe("persona-a");
  });

  it("a vanished template id degrades to empty, not a crash", () => {
    expect(resolveTonePersona("tpl-gone", "ignored", templates)).toBe("");
  });
});

describe("buildTaskPersonaPrompt", () => {
  it("embeds the trimmed task", () => {
    const p = buildTaskPersonaPrompt("  monitor CI  ", "");
    expect(p).toContain("## タスク\nmonitor CI");
    expect(p).not.toContain("## 追加要望");
  });

  it("appends the hint section only when a hint is given", () => {
    const p = buildTaskPersonaPrompt("monitor CI", " 関西弁 ");
    expect(p).toContain("## 追加要望\n関西弁");
  });
});

describe("builtinTemplates", () => {
  it("returns non-custom templates with non-empty personas", () => {
    const list = builtinTemplates();
    expect(list.length).toBeGreaterThanOrEqual(3);
    for (const tpl of list) {
      expect(tpl.custom).toBe(false);
      expect(tpl.name).toBeTruthy();
      expect(tpl.persona).toBeTruthy();
    }
  });
});

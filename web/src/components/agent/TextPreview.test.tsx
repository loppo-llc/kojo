import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { AttachmentList } from "./ChatMessage";
import type { AgentMessageAttachment } from "../../lib/agentApi";
import { decodeTextBytes, isTextAttachment, parseDelimited, textFormat } from "./TextPreview";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function att(name: string, mime: string): AgentMessageAttachment {
  return { path: `kojo://agents/ag_x/attach/${name}`, name, size: 42, mime };
}

describe("text attachment detection", () => {
  it("classifies by mime and extension", () => {
    expect(isTextAttachment("notes.txt", "")).toBe(true);
    expect(isTextAttachment("README", "text/plain; charset=utf-8")).toBe(true);
    expect(isTextAttachment("data.json", "application/json")).toBe(true);
    expect(isTextAttachment("a.zip", "application/zip")).toBe(false);
    expect(isTextAttachment("a.pdf", "application/pdf")).toBe(false);
    expect(isTextAttachment("Dockerfile", "application/octet-stream")).toBe(true);
    expect(textFormat("x.md", "")).toBe("markdown");
    expect(textFormat("x.CSV", "")).toBe("csv");
    expect(textFormat("x", "text/tab-separated-values")).toBe("tsv");
    expect(textFormat("x.log", "text/plain")).toBe("plain");
    expect(textFormat("data", "application/csv")).toBe("csv");
  });
});

describe("decodeTextBytes", () => {
  it("decodes utf-8 and drops a split trailing sequence when truncated", () => {
    const full = new TextEncoder().encode("あい");
    expect(decodeTextBytes(full, false)).toBe("あい");
    expect(decodeTextBytes(full.subarray(0, 4), true)).toBe("あ");
  });
  it("falls back to shift_jis", () => {
    // "テスト" in Shift_JIS
    const sjis = new Uint8Array([0x83, 0x65, 0x83, 0x58, 0x83, 0x67]);
    expect(decodeTextBytes(sjis, false)).toBe("テスト");
  });
  it("rejects binary", () => {
    expect(decodeTextBytes(new Uint8Array([0x50, 0x4b, 0x03, 0x04, 0x00, 0x00]), false)).toBeNull();
  });
});

describe("parseDelimited", () => {
  it("handles quotes, embedded separators, newlines and CRLF", () => {
    const { rows, more } = parseDelimited('a,b\r\n"x,1","he said ""hi""\nok"\n3,\n', ",", { maxRows: 10, maxCols: 10, maxCells: 100 });
    expect(more).toBe(false);
    expect(rows).toEqual([
      ["a", "b"],
      ["x,1", 'he said "hi"\nok'],
      ["3", ""],
    ]);
  });
  it("caps rows, columns and cells while parsing", () => {
    const lim = { maxRows: 2, maxCols: 10, maxCells: 100 };
    expect(parseDelimited("1\n2\n3\n4\n", ",", lim)).toEqual({ rows: [["1"], ["2"]], more: true });
    expect(parseDelimited("1\n2\n", ",", lim)).toEqual({ rows: [["1"], ["2"]], more: false });
    expect(parseDelimited("a,b,c,d\n", ",", { maxRows: 9, maxCols: 2, maxCells: 100 })).toEqual({ rows: [["a", "b"]], more: true });
    expect(parseDelimited("a,b\nc,d\ne,f\n", ",", { maxRows: 9, maxCols: 9, maxCells: 3 }).more).toBe(true);
    expect(parseDelimited("a,b,c,d", ",", { maxRows: 9, maxCols: 9, maxCells: 3 })).toEqual({ rows: [["a", "b", "c"]], more: true });
  });
});

describe("AttachmentList text preview", () => {
  it("opens text attachments in an inline viewer instead of downloading", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("hello\nworld"));
    render(<AttachmentList attachments={[att("notes.txt", "text/plain; charset=utf-8")]} isUser={false} />);

    const chip = screen.getByRole("button", { name: /notes\.txt/ });
    expect(chip.tagName).toBe("BUTTON");
    fireEvent.click(chip);

    const viewer = await screen.findByTestId("text-preview");
    await waitFor(() => expect(viewer.querySelector("pre")?.textContent).toBe("hello\nworld"));
    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/blob/agents/ag_x/attach/notes.txt");
    const dl = screen.getByRole("link", { name: "Download" });
    expect(dl).toHaveAttribute("download", "notes.txt");
  });

  it("renders csv as a table with a raw toggle", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("name,qty\napple,3\n"));
    render(<AttachmentList attachments={[att("items.csv", "text/csv")]} isUser={false} />);
    fireEvent.click(screen.getByRole("button", { name: /items\.csv/ }));

    await waitFor(() => expect(screen.getByRole("columnheader", { name: "name" })).toBeInTheDocument());
    expect(screen.getByRole("cell", { name: "apple" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Raw" }));
    expect(screen.getByTestId("text-preview").querySelector("pre")?.textContent).toBe("name,qty\napple,3\n");
  });

  it("does not load remote images in markdown", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("![px](https://evil.example/p.png)\n"));
    render(<AttachmentList attachments={[att("doc.md", "text/plain")]} isUser={false} />);
    fireEvent.click(screen.getByRole("button", { name: /doc\.md/ }));
    const link = await screen.findByRole("link", { name: "px" });
    expect(link).toHaveAttribute("href", "https://evil.example/p.png");
    expect(screen.getByTestId("text-preview").querySelector("img")).toBeNull();
  });

  it("treats protocol-relative image URLs as remote", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("![bs](//evil.example/p.png)\n"));
    render(<AttachmentList attachments={[att("doc2.md", "text/plain")]} isUser={false} />);
    fireEvent.click(screen.getByRole("button", { name: /doc2\.md/ }));
    await screen.findByRole("link", { name: "bs" });
    expect(screen.getByTestId("text-preview").querySelector("img")).toBeNull();
  });

  it("opens oversized markdown in raw mode", async () => {
    const big = "x\n\n".repeat(100_000);
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(big));
    render(<AttachmentList attachments={[att("big.md", "text/markdown")]} isUser={false} />);
    fireEvent.click(screen.getByRole("button", { name: /big\.md/ }));
    await waitFor(() => expect(screen.getByTestId("text-preview").querySelector("pre")?.textContent).toBe(big));
    expect(screen.getByRole("button", { name: "Rendered" })).toBeInTheDocument();
  });

  it("keeps non-text attachments as download links", () => {
    render(<AttachmentList attachments={[att("a.zip", "application/zip")]} isUser={false} />);
    expect(screen.getByRole("link", { name: /a\.zip/ })).toHaveAttribute("download", "a.zip");
  });
});

import { describe, expect, it, vi } from "vitest";

// api.ts pulls the token from ./auth on every URL build. Pin it so
// the assertions below don't depend on a real localStorage state.
vi.mock("./auth", () => ({
  getOwnerToken: vi.fn(() => "tok"),
  authHeaders: vi.fn(() => ({})),
  // appendTokenQuery is called by api.blob.urlFromKojoURI. Stub it
  // to deterministically append `?token=tok` so the assertions
  // don't depend on URL parsing edge cases inside the real impl.
  appendTokenQuery: vi.fn((url: string) =>
    url.includes("?") ? `${url}&token=tok` : `${url}?token=tok`,
  ),
  bootstrapTokenFromURL: vi.fn(),
  clearOwnerToken: vi.fn(),
}));

import { api } from "./api";

describe("api.blob.urlFromKojoURI", () => {
  it("rewrites a kojo:// URI to /api/v1/blob/<scope>/<path> with token", () => {
    const url = api.blob.urlFromKojoURI(
      "kojo://global/agents/ag_1/attach/m_abc/chart.png",
    );
    expect(url).toMatch(
      /^\/api\/v1\/blob\/global\/agents\/ag_1\/attach\/m_abc\/chart\.png\?token=tok$/,
    );
  });

  it("preserves percent-encoded segments rather than double-encoding", () => {
    // blob.BuildURI emits canonical percent-encoded segments; the
    // helper should round-trip "report%20final.pdf" back to
    // "report%20final.pdf" (NOT "report%2520final.pdf").
    const url = api.blob.urlFromKojoURI(
      "kojo://global/agents/ag_1/attach/m_1/report%20final.pdf",
    );
    expect(url).not.toContain("%2520");
    expect(url).toContain("report%20final.pdf");
  });

  it("returns null for inputs that are not kojo:// URIs", () => {
    expect(api.blob.urlFromKojoURI("/tmp/foo.png")).toBeNull();
    expect(api.blob.urlFromKojoURI("https://example/foo.png")).toBeNull();
    expect(api.blob.urlFromKojoURI("")).toBeNull();
  });

  it("returns null when the path tail is missing the scope/path separator", () => {
    expect(api.blob.urlFromKojoURI("kojo://global")).toBeNull();
    expect(api.blob.urlFromKojoURI("kojo://")).toBeNull();
  });
});

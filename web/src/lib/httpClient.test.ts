import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  PreconditionFailedError,
  delWithIfMatch,
  getWithEtag,
  patchWithIfMatch,
  postWithIfMatch,
} from "./httpClient";

// auth.getOwnerToken is consulted on every withAuth() call. Mock it to
// return an empty string so the Authorization header is never set;
// individual tests that need a token override the mock explicitly.
vi.mock("./auth", () => ({
  getOwnerToken: vi.fn(() => ""),
}));

import { getOwnerToken } from "./auth";

// A typed handle for the fetch-mocked Response factory.
function makeResponse(opts: {
  status?: number;
  body?: unknown;
  etag?: string | null;
  contentLength?: string | null;
}): Response {
  const status = opts.status ?? 200;
  const headers = new Headers();
  if (opts.etag !== undefined && opts.etag !== null) {
    headers.set("ETag", opts.etag);
  }
  if (opts.contentLength !== undefined && opts.contentLength !== null) {
    headers.set("content-length", opts.contentLength);
  }
  const bodyText =
    opts.body === undefined ? "" : typeof opts.body === "string" ? opts.body : JSON.stringify(opts.body);
  return new Response(status === 204 ? null : bodyText, { status, headers });
}

let fetchSpy: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  fetchSpy = vi.spyOn(globalThis, "fetch");
  vi.mocked(getOwnerToken).mockReturnValue("");
});

afterEach(() => {
  fetchSpy.mockRestore();
  vi.clearAllMocks();
});

describe("getWithEtag", () => {
  it("strips RFC 7232 double-quotes from the returned ETag", async () => {
    fetchSpy.mockResolvedValueOnce(
      makeResponse({ body: { ok: true }, etag: '"abc123"' }),
    );
    const got = await getWithEtag<{ ok: boolean }>("/api/v1/x");
    expect(got.etag).toBe("abc123");
    expect(got.value).toEqual({ ok: true });
  });

  it("passes through a wildcard ETag verbatim", async () => {
    fetchSpy.mockResolvedValueOnce(makeResponse({ body: { ok: 1 }, etag: "*" }));
    const got = await getWithEtag<{ ok: number }>("/api/v1/x");
    expect(got.etag).toBe("*");
  });

  it("returns null etag when the header is absent", async () => {
    fetchSpy.mockResolvedValueOnce(makeResponse({ body: { ok: 1 } }));
    const got = await getWithEtag("/api/v1/x");
    expect(got.etag).toBeNull();
  });

  it("returns value=undefined on 204 No Content", async () => {
    fetchSpy.mockResolvedValueOnce(makeResponse({ status: 204, etag: '"v1"' }));
    const got = await getWithEtag("/api/v1/x");
    expect(got.value).toBeUndefined();
    expect(got.etag).toBe("v1");
  });

  it("throws Error with status:body on non-2xx", async () => {
    fetchSpy.mockResolvedValueOnce(
      makeResponse({ status: 500, body: "boom" }),
    );
    await expect(getWithEtag("/api/v1/x")).rejects.toThrow("500: boom");
  });

  it("attaches Authorization when getOwnerToken returns a token", async () => {
    vi.mocked(getOwnerToken).mockReturnValueOnce("secret");
    fetchSpy.mockResolvedValueOnce(makeResponse({ body: { ok: 1 } }));
    await getWithEtag("/api/v1/x");
    const init = fetchSpy.mock.calls[0]![1] as RequestInit;
    const headers = new Headers(init.headers);
    expect(headers.get("Authorization")).toBe("Bearer secret");
  });

  it("omits Authorization when no token is set", async () => {
    fetchSpy.mockResolvedValueOnce(makeResponse({ body: { ok: 1 } }));
    await getWithEtag("/api/v1/x");
    const init = fetchSpy.mock.calls[0]![1] as RequestInit | undefined;
    const headers = new Headers(init?.headers);
    expect(headers.get("Authorization")).toBeNull();
  });
});

describe("patchWithIfMatch", () => {
  it("wraps the caller's unquoted ETag in RFC 7232 double-quotes on the wire", async () => {
    fetchSpy.mockResolvedValueOnce(makeResponse({ body: { ok: 1 }, etag: '"new"' }));
    await patchWithIfMatch("/api/v1/x", { foo: 1 }, "raw-tag");
    const init = fetchSpy.mock.calls[0]![1] as RequestInit;
    const headers = new Headers(init.headers);
    expect(headers.get("If-Match")).toBe('"raw-tag"');
    expect(headers.get("Content-Type")).toBe("application/json");
    expect(init.method).toBe("PATCH");
    expect(init.body).toBe(JSON.stringify({ foo: 1 }));
  });

  it("passes through an already-quoted ETag without double-wrapping", async () => {
    fetchSpy.mockResolvedValueOnce(makeResponse({ body: { ok: 1 } }));
    await patchWithIfMatch("/api/v1/x", {}, '"already"');
    const init = fetchSpy.mock.calls[0]![1] as RequestInit;
    expect(new Headers(init.headers).get("If-Match")).toBe('"already"');
  });

  it("forwards wildcard If-Match: *", async () => {
    fetchSpy.mockResolvedValueOnce(makeResponse({ body: {} }));
    await patchWithIfMatch("/api/v1/x", {}, "*");
    const init = fetchSpy.mock.calls[0]![1] as RequestInit;
    expect(new Headers(init.headers).get("If-Match")).toBe("*");
  });

  it("omits If-Match when ifMatch is undefined", async () => {
    fetchSpy.mockResolvedValueOnce(makeResponse({ body: {} }));
    await patchWithIfMatch("/api/v1/x", { foo: 1 });
    const init = fetchSpy.mock.calls[0]![1] as RequestInit;
    expect(new Headers(init.headers).has("If-Match")).toBe(false);
  });

  it("throws PreconditionFailedError on 412", async () => {
    fetchSpy.mockResolvedValueOnce(makeResponse({ status: 412, body: "stale" }));
    await expect(patchWithIfMatch("/api/v1/x", {}, "tag")).rejects.toBeInstanceOf(
      PreconditionFailedError,
    );
  });

  it("412 carries the server body as the error message", async () => {
    fetchSpy.mockResolvedValueOnce(makeResponse({ status: 412, body: "etag mismatch" }));
    await expect(patchWithIfMatch("/api/v1/x", {}, "tag")).rejects.toThrow(
      "etag mismatch",
    );
  });

  it("412 falls back to a default message when the server body is empty", async () => {
    fetchSpy.mockResolvedValueOnce(makeResponse({ status: 412 }));
    await expect(patchWithIfMatch("/api/v1/x", {})).rejects.toThrow("etag mismatch");
  });

  it("throws a plain Error on non-412 non-2xx", async () => {
    fetchSpy.mockResolvedValueOnce(makeResponse({ status: 500, body: "boom" }));
    let caught: unknown = null;
    try {
      await patchWithIfMatch("/api/v1/x", {});
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(Error);
    expect(caught).not.toBeInstanceOf(PreconditionFailedError);
    expect((caught as Error).message).toBe("500: boom");
  });

  it("returns value=undefined on 204 with the new ETag surfaced", async () => {
    fetchSpy.mockResolvedValueOnce(makeResponse({ status: 204, etag: '"v2"' }));
    const got = await patchWithIfMatch("/api/v1/x", {});
    expect(got.value).toBeUndefined();
    expect(got.etag).toBe("v2");
  });
});

describe("postWithIfMatch", () => {
  it("sets Content-Type only when a body is provided", async () => {
    fetchSpy.mockResolvedValueOnce(makeResponse({ body: { ok: 1 } }));
    await postWithIfMatch("/api/v1/x");
    const init1 = fetchSpy.mock.calls[0]![1] as RequestInit;
    expect(new Headers(init1.headers).has("Content-Type")).toBe(false);
    expect(init1.body).toBeUndefined();

    fetchSpy.mockResolvedValueOnce(makeResponse({ body: { ok: 1 } }));
    await postWithIfMatch("/api/v1/x", { foo: 1 });
    const init2 = fetchSpy.mock.calls[1]![1] as RequestInit;
    expect(new Headers(init2.headers).get("Content-Type")).toBe("application/json");
    expect(init2.body).toBe(JSON.stringify({ foo: 1 }));
  });

  it("412 surfaces as PreconditionFailedError", async () => {
    fetchSpy.mockResolvedValueOnce(makeResponse({ status: 412 }));
    await expect(postWithIfMatch("/api/v1/x", {}, "tag")).rejects.toBeInstanceOf(
      PreconditionFailedError,
    );
  });
});

describe("delWithIfMatch", () => {
  it("forwards If-Match and uses method DELETE", async () => {
    fetchSpy.mockResolvedValueOnce(makeResponse({ status: 204 }));
    await delWithIfMatch("/api/v1/x", "tag");
    const init = fetchSpy.mock.calls[0]![1] as RequestInit;
    expect(init.method).toBe("DELETE");
    expect(new Headers(init.headers).get("If-Match")).toBe('"tag"');
  });

  it("412 surfaces as PreconditionFailedError", async () => {
    fetchSpy.mockResolvedValueOnce(makeResponse({ status: 412 }));
    await expect(delWithIfMatch("/api/v1/x", "tag")).rejects.toBeInstanceOf(
      PreconditionFailedError,
    );
  });
});

describe("PreconditionFailedError", () => {
  it("has status=412 and name='PreconditionFailedError' for typeof discrimination", () => {
    const e = new PreconditionFailedError("oops");
    expect(e.status).toBe(412);
    expect(e.name).toBe("PreconditionFailedError");
    expect(e).toBeInstanceOf(Error);
    expect(e.message).toBe("oops");
  });
});

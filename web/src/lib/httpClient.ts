import { getOwnerToken } from "./auth";

const BASE = "";

/**
 * Thrown when the server rejects a write with 412 Precondition Failed
 * (typically because the client's If-Match etag was stale). Callers
 * should refetch the resource and re-apply the user's edit on the
 * fresh data — blindly retrying would just hit 412 again.
 *
 * Distinguishable from a generic Error so UI code can show a
 * targeted "someone else edited this" message instead of the raw
 * "412: ..." string the catch-all writeError serializer produces.
 */
export class PreconditionFailedError extends Error {
  readonly status = 412;
  constructor(message: string) {
    super(message);
    this.name = "PreconditionFailedError";
  }
}

/**
 * Merge the stashed Owner Bearer token into the request headers when
 * one is available. The Tailscale listener is Owner-trusted by
 * middleware so a missing token is fine; the auth-required (--local)
 * listener requires it on every /api/v1/* call.
 */
function withAuth(init?: RequestInit): RequestInit | undefined {
  const tok = getOwnerToken();
  if (!tok) return init;
  const headers = new Headers(init?.headers);
  if (!headers.has("Authorization")) headers.set("Authorization", `Bearer ${tok}`);
  return { ...init, headers };
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(BASE + path, withAuth(init));
  if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  if (res.status === 204 || res.headers.get("content-length") === "0") {
    return undefined as T;
  }
  return res.json();
}

/**
 * Response wrapper for ETag-aware fetches: the parsed JSON value plus
 * the strong ETag the server returned (or null if the endpoint did
 * not surface one — Phase 7 wires ETags incrementally, so callers
 * must tolerate null until every endpoint catches up).
 */
export type EtaggedResponse<T> = { value: T; etag: string | null };

/**
 * Strip the surrounding double-quotes from a wire-format ETag so the
 * cached / surfaced value is just the inner identifier. Wildcard `*`
 * passes through untouched (RFC 7232 reserves it as a non-quoted
 * sentinel meaning "any current resource"). null/empty stays null.
 */
function unquoteEtag(raw: string | null): string | null {
  if (!raw) return null;
  if (raw === "*") return raw;
  if (raw.length >= 2 && raw[0] === '"' && raw[raw.length - 1] === '"') {
    return raw.slice(1, -1);
  }
  return raw; // server already gave it unquoted (shouldn't happen, but be lenient)
}

/**
 * Wrap a raw etag in the quoted wire format expected by If-Match /
 * If-None-Match per RFC 7232. `*` passes through as-is. Empty/null
 * yields the empty string so callers can detect "no precondition"
 * without a separate undefined check.
 */
function quoteEtag(raw: string | null | undefined): string {
  if (!raw) return "";
  if (raw === "*") return raw;
  if (raw.length >= 2 && raw[0] === '"' && raw[raw.length - 1] === '"') {
    return raw; // already quoted
  }
  return `"${raw}"`;
}

/**
 * GET that surfaces the response ETag header in unquoted form (the
 * canonical inner identifier — easier to thread through JSON-shaped
 * data structures). The wire-format quoting is reapplied internally
 * by patchWithIfMatch when sending If-Match, so callers never have
 * to think about the double-quote convention.
 */
export async function getWithEtag<T>(path: string): Promise<EtaggedResponse<T>> {
  const res = await fetch(BASE + path, withAuth());
  if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  const etag = unquoteEtag(res.headers.get("ETag"));
  if (res.status === 204 || res.headers.get("content-length") === "0") {
    return { value: undefined as T, etag };
  }
  return { value: await res.json(), etag };
}

/**
 * PATCH with optional If-Match precondition. The caller passes the
 * raw (unquoted) etag; this function applies RFC 7232 quoting on the
 * wire so call sites don't have to. When ifMatch is provided the
 * server's optimistic-concurrency layer rejects stale writes with
 * 412 — surfaced as PreconditionFailedError so callers can
 * distinguish it from generic 4xx. Returns the new resource and the
 * new ETag the server computed (unquoted, for symmetry with
 * getWithEtag), so the caller can chain edits without an intervening
 * GET.
 */
export async function patchWithIfMatch<T>(
  path: string,
  body: unknown,
  ifMatch?: string,
): Promise<EtaggedResponse<T>> {
  const headers = new Headers({ "Content-Type": "application/json" });
  if (ifMatch) headers.set("If-Match", quoteEtag(ifMatch));
  const res = await fetch(
    BASE + path,
    withAuth({ method: "PATCH", headers, body: JSON.stringify(body) }),
  );
  if (res.status === 412) {
    // Drain the body so the connection is reusable, but don't try to
    // surface its content — the server's writeError JSON wrapper is
    // useful in dev but ugly in user-facing alerts.
    const text = await res.text().catch(() => "");
    throw new PreconditionFailedError(text || "etag mismatch");
  }
  if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  const etag = unquoteEtag(res.headers.get("ETag"));
  if (res.status === 204 || res.headers.get("content-length") === "0") {
    return { value: undefined as T, etag };
  }
  return { value: await res.json(), etag };
}

const jsonHeaders = { "Content-Type": "application/json" } as const;

export function get<T>(path: string): Promise<T> {
  return request<T>(path);
}

export function post<T>(path: string, body?: unknown): Promise<T> {
  return request<T>(path, {
    method: "POST",
    headers: jsonHeaders,
    body: body ? JSON.stringify(body) : undefined,
  });
}

export function put<T>(path: string, body: unknown): Promise<T> {
  return request<T>(path, {
    method: "PUT",
    headers: jsonHeaders,
    body: JSON.stringify(body),
  });
}

export function patch<T>(path: string, body: unknown): Promise<T> {
  return request<T>(path, {
    method: "PATCH",
    headers: jsonHeaders,
    body: JSON.stringify(body),
  });
}

export function del<T>(path: string): Promise<T> {
  return request<T>(path, { method: "DELETE" });
}

/**
 * POST with optional If-Match precondition. Used for non-PATCH state
 * mutations that still want optimistic locking against a specific row's
 * etag — primarily POST /messages/{msgId}/regenerate, which truncates
 * the transcript and would otherwise race with a concurrent edit.
 * Mirrors the patch/delete variants: 412 → PreconditionFailedError so
 * callers can refetch instead of giving up.
 */
export async function postWithIfMatch<T>(
  path: string,
  body?: unknown,
  ifMatch?: string,
): Promise<EtaggedResponse<T>> {
  const headers = new Headers();
  if (body !== undefined) headers.set("Content-Type", "application/json");
  if (ifMatch) headers.set("If-Match", quoteEtag(ifMatch));
  const res = await fetch(
    BASE + path,
    withAuth({
      method: "POST",
      headers,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    }),
  );
  if (res.status === 412) {
    const text = await res.text().catch(() => "");
    throw new PreconditionFailedError(text || "etag mismatch");
  }
  if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  const etag = unquoteEtag(res.headers.get("ETag"));
  if (res.status === 204 || res.headers.get("content-length") === "0") {
    return { value: undefined as T, etag };
  }
  return { value: await res.json(), etag };
}

/**
 * DELETE with optional If-Match precondition. Mirrors patchWithIfMatch:
 * on 412 throws PreconditionFailedError so callers can distinguish stale
 * deletes from generic 4xx and refetch the row instead of giving up.
 * The server tolerates an empty ifMatch (legacy unconditional delete);
 * pass the raw (unquoted) etag to enable optimistic locking.
 */
export async function delWithIfMatch<T>(
  path: string,
  ifMatch?: string,
): Promise<EtaggedResponse<T>> {
  const headers = new Headers();
  if (ifMatch) headers.set("If-Match", quoteEtag(ifMatch));
  const res = await fetch(BASE + path, withAuth({ method: "DELETE", headers }));
  if (res.status === 412) {
    const text = await res.text().catch(() => "");
    throw new PreconditionFailedError(text || "etag mismatch");
  }
  if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  const etag = unquoteEtag(res.headers.get("ETag"));
  if (res.status === 204 || res.headers.get("content-length") === "0") {
    return { value: undefined as T, etag };
  }
  return { value: await res.json(), etag };
}

export function upload<T>(path: string, form: FormData): Promise<T> {
  return request<T>(path, { method: "POST", body: form });
}

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  __resetUpdateCheckForTests,
  dismissUpdatePrompt,
  fetchUpdateStatus,
  getUpdatePromptSnapshot,
  shouldPromptUpdate,
  startUpdate,
} from "./updateCheck";

// httpClient attaches auth; mock the token so Authorization is deterministic.
vi.mock("./auth", () => ({
  getOwnerToken: vi.fn(() => "tok"),
}));

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

let fetchSpy: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  __resetUpdateCheckForTests();
  fetchSpy = vi.spyOn(globalThis, "fetch");
});

afterEach(() => {
  fetchSpy.mockRestore();
  __resetUpdateCheckForTests();
  vi.clearAllMocks();
});

describe("shouldPromptUpdate", () => {
  const none = new Set<string>();

  it("prompts when updateAvailable and latest is set", () => {
    expect(shouldPromptUpdate(true, "v1.2.0", none)).toBe(true);
  });

  it("does not prompt when updateAvailable is false", () => {
    expect(shouldPromptUpdate(false, "v1.2.0", none)).toBe(false);
  });

  it("does not prompt when latest is empty", () => {
    expect(shouldPromptUpdate(true, "", none)).toBe(false);
    expect(shouldPromptUpdate(true, undefined, none)).toBe(false);
  });

  it("does not prompt for a dismissed version", () => {
    expect(shouldPromptUpdate(true, "v1.2.0", new Set(["v1.2.0"]))).toBe(false);
  });

  it("prompts for a newer version after an earlier dismiss", () => {
    expect(shouldPromptUpdate(true, "v1.3.0", new Set(["v1.2.0"]))).toBe(true);
  });
});

describe("fetchUpdateStatus + dismiss", () => {
  it("exposes prompt when updateAvailable and latest is new", async () => {
    fetchSpy.mockResolvedValueOnce(
      jsonResponse(200, {
        supported: true,
        current: "v1.0.0",
        latest: "v1.1.0",
        updateAvailable: true,
        notesUrl: "https://example.com/r/v1.1.0",
      }),
    );
    await fetchUpdateStatus();
    expect(getUpdatePromptSnapshot()).toEqual({
      latest: "v1.1.0",
      notesUrl: "https://example.com/r/v1.1.0",
      supported: true,
      phase: "available",
    });
  });

  it("shows once per version: dismiss suppresses same latest", async () => {
    const body = {
      supported: true,
      current: "v1.0.0",
      latest: "v1.1.0",
      updateAvailable: true,
      notesUrl: "https://example.com/r/v1.1.0",
    };
    fetchSpy.mockResolvedValueOnce(jsonResponse(200, body));
    await fetchUpdateStatus();
    expect(getUpdatePromptSnapshot()).not.toBeNull();

    dismissUpdatePrompt();
    expect(getUpdatePromptSnapshot()).toBeNull();

    fetchSpy.mockResolvedValueOnce(jsonResponse(200, body));
    await fetchUpdateStatus();
    expect(getUpdatePromptSnapshot()).toBeNull();
    expect(fetchSpy).toHaveBeenCalledTimes(2);
  });

  it("re-prompts when a newer latest arrives after dismiss", async () => {
    fetchSpy.mockResolvedValueOnce(
      jsonResponse(200, {
        supported: true,
        latest: "v1.1.0",
        updateAvailable: true,
        notesUrl: "",
      }),
    );
    await fetchUpdateStatus();
    dismissUpdatePrompt();

    fetchSpy.mockResolvedValueOnce(
      jsonResponse(200, {
        supported: true,
        latest: "v1.2.0",
        updateAvailable: true,
        notesUrl: "https://example.com/r/v1.2.0",
      }),
    );
    await fetchUpdateStatus();
    expect(getUpdatePromptSnapshot()).toMatchObject({
      latest: "v1.2.0",
      phase: "available",
    });
  });

  it("does not show when updateAvailable is false", async () => {
    fetchSpy.mockResolvedValueOnce(
      jsonResponse(200, {
        supported: true,
        latest: "v1.0.0",
        updateAvailable: false,
      }),
    );
    await fetchUpdateStatus();
    expect(getUpdatePromptSnapshot()).toBeNull();
  });

  it("surfaces supported:false so the UI can hide the Update button", async () => {
    fetchSpy.mockResolvedValueOnce(
      jsonResponse(200, {
        supported: false,
        latest: "v1.4.0",
        updateAvailable: true,
        notesUrl: "",
      }),
    );
    await fetchUpdateStatus();
    expect(getUpdatePromptSnapshot()).toMatchObject({
      latest: "v1.4.0",
      supported: false,
      phase: "available",
    });
  });
});

describe("startUpdate transitions", () => {
  async function seedAvailable(supported = true): Promise<void> {
    fetchSpy.mockResolvedValueOnce(
      jsonResponse(200, {
        supported,
        latest: "v2.0.0",
        updateAvailable: true,
        notesUrl: "https://example.com/notes",
      }),
    );
    await fetchUpdateStatus();
  }

  it("pending: keeps banner non-interactive (further POSTs ignored)", async () => {
    await seedAvailable();
    fetchSpy.mockResolvedValueOnce(
      jsonResponse(202, { status: "pending", from: "v1.0.0", to: "v2.0.0" }),
    );
    const first = await startUpdate();
    expect(first.kind).toBe("pending");
    expect(getUpdatePromptSnapshot()?.phase).toBe("pending");

    const second = await startUpdate();
    expect(second.kind).toBe("pending");
    // 1 GET + 1 POST
    expect(fetchSpy).toHaveBeenCalledTimes(2);
  });

  it("already_pending is treated as pending", async () => {
    await seedAvailable();
    fetchSpy.mockResolvedValueOnce(
      jsonResponse(202, {
        status: "already_pending",
        from: "v1.0.0",
        to: "v2.0.0",
      }),
    );
    const result = await startUpdate();
    expect(result.kind).toBe("pending");
    expect(getUpdatePromptSnapshot()?.phase).toBe("pending");
  });

  it("up_to_date: clears the banner", async () => {
    await seedAvailable();
    fetchSpy.mockResolvedValueOnce(
      jsonResponse(200, { status: "up_to_date" }),
    );
    const result = await startUpdate();
    expect(result).toEqual({ kind: "up_to_date" });
    expect(getUpdatePromptSnapshot()).toBeNull();
  });

  it("error: keeps banner and surfaces message", async () => {
    await seedAvailable();
    fetchSpy.mockResolvedValueOnce(
      jsonResponse(502, {
        error: {
          code: "assets_not_ready",
          message: "release has no binary for this platform yet",
        },
      }),
    );
    const result = await startUpdate();
    expect(result).toEqual({
      kind: "error",
      message: "release has no binary for this platform yet",
    });
    expect(getUpdatePromptSnapshot()).toMatchObject({
      phase: "error",
      error: "release has no binary for this platform yet",
      latest: "v2.0.0",
    });

    // Retry after error is allowed.
    fetchSpy.mockResolvedValueOnce(
      jsonResponse(202, { status: "pending", from: "v1.0.0", to: "v2.0.0" }),
    );
    const retry = await startUpdate();
    expect(retry.kind).toBe("pending");
  });

  it("aborted drain: pending → error when restart pending flips false", async () => {
    vi.useFakeTimers();
    try {
      await seedAvailable();
      fetchSpy.mockResolvedValueOnce(
        jsonResponse(202, { status: "pending", from: "v1.0.0", to: "v2.0.0" }),
      );
      await startUpdate();
      expect(getUpdatePromptSnapshot()?.phase).toBe("pending");

      // Drain poll: restart no longer pending, update still available.
      fetchSpy
        .mockResolvedValueOnce(jsonResponse(200, { pending: false }))
        .mockResolvedValueOnce(
          jsonResponse(200, {
            supported: true,
            latest: "v2.0.0",
            updateAvailable: true,
          }),
        );

      await vi.advanceTimersByTimeAsync(15_000);
      // Let the async poll handlers settle.
      await Promise.resolve();
      await Promise.resolve();

      expect(getUpdatePromptSnapshot()).toMatchObject({
        phase: "error",
        error: "restart did not complete; retry",
        latest: "v2.0.0",
      });
    } finally {
      vi.useRealTimers();
    }
  });

  it("aborted-drain poll keeps pending when restart status fetch fails", async () => {
    vi.useFakeTimers();
    try {
      await seedAvailable();
      fetchSpy.mockResolvedValueOnce(
        jsonResponse(202, { status: "pending", from: "v1.0.0", to: "v2.0.0" }),
      );
      await startUpdate();

      fetchSpy.mockRejectedValueOnce(new TypeError("network down"));
      await vi.advanceTimersByTimeAsync(15_000);
      await Promise.resolve();

      expect(getUpdatePromptSnapshot()?.phase).toBe("pending");
    } finally {
      vi.useRealTimers();
    }
  });
});

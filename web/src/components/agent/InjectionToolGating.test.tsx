import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { AgentSettings } from "./AgentSettings";
import {
  CONTEXT_INJECTION_KEYS,
  TOOL_ONLY_INJECTION_KEYS,
  type ContextInjectionKey,
} from "../../lib/agentApi";

const mocks = vi.hoisted(() => ({
  agentGet: vi.fn(),
  checkinFile: vi.fn(),
  userContext: vi.fn(),
  credentialsList: vi.fn(),
  apiCustomModels: vi.fn(),
}));

vi.mock("../../hooks/useTTS", () => ({
  useTTSAutoToggle: () => [false, vi.fn()],
  useTTSPlayer: () => ({ play: vi.fn(), state: {} }),
  useTTSCapability: () => null,
}));

vi.mock("../../lib/api", () => ({
  api: { customModels: mocks.apiCustomModels, upload: vi.fn() },
  isThumbSupported: vi.fn(() => false),
}));

vi.mock("../../lib/agentApi", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/agentApi")>();
  return {
    ...actual,
    agentApi: {
      ...actual.agentApi,
      get: mocks.agentGet,
      getCheckinFile: mocks.checkinFile,
      getUserContext: mocks.userContext,
      avatarUrl: vi.fn((id: string) => `/avatar/${id}`),
      update: vi.fn(),
      credentials: { ...actual.agentApi.credentials, list: mocks.credentialsList },
    },
  };
});


function agentWithTool(tool: string) {
  const now = "2026-06-08T00:00:00Z";
  return {
    id: "demo",
    name: "Demo Agent",
    persona: "Demo persona",
    model: "",
    effort: "",
    tool,
    customBaseURL: "http://127.0.0.1:8080/v1",
    workDir: "/tmp",
    timeoutMinutes: 10,
    createdAt: now,
    updatedAt: now,
    publicProfile: "",
    publicProfileOverride: false,
    hasAvatar: false,
    etag: "agent-etag",
  };
}

function renderSettings() {
  const router = createMemoryRouter(
    [{ path: "/agents/:id/settings", element: <AgentSettings /> }],
    { initialEntries: ["/agents/demo/settings"] },
  );
  render(<RouterProvider router={router} />);
}

beforeEach(() => {
  Element.prototype.scrollIntoView = vi.fn();
  Element.prototype.scrollTo = vi.fn() as unknown as typeof Element.prototype.scrollTo;
  mocks.checkinFile.mockResolvedValue({ value: { content: "", isDefault: true, etag: "" }, etag: "" });
  mocks.userContext.mockResolvedValue({ value: { content: "", isDefault: true, etag: "" }, etag: "" });
  mocks.credentialsList.mockResolvedValue([]);
  mocks.apiCustomModels.mockResolvedValue([]);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// Toggle accessible names come from the injection labels; mirror the en
// strings so the table below covers every key in CONTEXT_INJECTION_KEYS
// and a newly added key fails compilation until it is listed.
const LABELS: Record<ContextInjectionKey, string> = {
  user_context: "User Context",
  memory_md: "MEMORY.md",
  credentials: "Credentials",
  groupdm: "Group DM",
  todo_api: "Todos",
  attachments: "Attachments",
  status: "Status",
  memory_search: "Memory Search",
  recent_conversation: "Recent Conversation",
  persona_anchor: "Persona Anchor",
  call_user: "Call the User",
  model_switch_info: "Model Switch Info",
};

const TOOL_ONLY = new Set<string>(TOOL_ONLY_INJECTION_KEYS);

describe("context injection toggles vs backend", () => {
  it("leaves every toggle live for a tool-capable backend", async () => {
    mocks.agentGet.mockResolvedValue(agentWithTool("claude"));
    renderSettings();

    await screen.findByLabelText(LABELS.credentials);
    for (const key of CONTEXT_INJECTION_KEYS) {
      const toggle = screen.getByLabelText(LABELS[key]);
      expect(toggle, key).toBeEnabled();
      expect(toggle, key).toBeChecked();
    }
  });

  it("greys out exactly the tool-only sections for custom-bare", async () => {
    mocks.agentGet.mockResolvedValue(agentWithTool("custom-bare"));
    renderSettings();

    await screen.findByLabelText(LABELS.credentials);
    for (const key of CONTEXT_INJECTION_KEYS) {
      const toggle = screen.getByLabelText(LABELS[key]);
      if (TOOL_ONLY.has(key)) {
        expect(toggle, key).toBeDisabled();
        // A disabled toggle must also read as off: the section genuinely
        // is not in the prompt, whatever the stored flag says.
        expect(toggle, key).not.toBeChecked();
      } else {
        expect(toggle, key).toBeEnabled();
      }
    }
  });

  it("restores the stored state when an unsaved tool switch is undone", async () => {
    mocks.agentGet.mockResolvedValue({
      ...agentWithTool("claude"),
      // Stored: credentials off (tool-only), status off (always live).
      disabledInjections: ["credentials", "status"],
    });
    renderSettings();

    await screen.findByLabelText(LABELS.attachments);
    expect(screen.getByLabelText(LABELS.attachments)).toBeChecked();
    expect(screen.getByLabelText(LABELS.credentials)).not.toBeChecked();

    // Switching the backend greys the tool-only rows immediately, before
    // anything is saved.
    fireEvent.click(screen.getByRole("button", { name: "custom-bare" }));
    await waitFor(() => expect(screen.getByLabelText(LABELS.attachments)).toBeDisabled());
    expect(screen.getByLabelText(LABELS.status)).toBeEnabled();

    // Switching back must bring the stored values back untouched — the
    // grey-out is display-only and never rewrote disabledInjections.
    fireEvent.click(screen.getByRole("button", { name: "claude" }));
    await waitFor(() => expect(screen.getByLabelText(LABELS.attachments)).toBeEnabled());
    expect(screen.getByLabelText(LABELS.attachments)).toBeChecked();
    expect(screen.getByLabelText(LABELS.credentials)).not.toBeChecked();
    expect(screen.getByLabelText(LABELS.status)).not.toBeChecked();
  });
});

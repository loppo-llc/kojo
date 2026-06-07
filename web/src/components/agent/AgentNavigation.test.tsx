import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { AgentChat } from "./AgentChat";
import { AgentCredentials } from "./AgentCredentials";
import { AgentSettings } from "./AgentSettings";

const mocks = vi.hoisted(() => ({
  agentGet: vi.fn(),
  agentMessages: vi.fn(),
  checkinFile: vi.fn(),
  userContext: vi.fn(),
  credentialsList: vi.fn(),
  apiCustomModels: vi.fn(),
}));

vi.mock("../../hooks/useAgentWebSocket", () => ({
  useAgentWebSocket: () => ({
    connected: true,
    sendMessage: vi.fn(),
    abort: vi.fn(),
  }),
}));

vi.mock("../../hooks/useTTS", () => ({
  useTTSAutoToggle: () => [false, vi.fn()],
  useTTSPlayer: () => ({ play: vi.fn(), state: {} }),
  useTTSCapability: () => null,
}));

vi.mock("../../lib/api", () => ({
  api: {
    customModels: mocks.apiCustomModels,
    upload: vi.fn(),
    files: {
      rawUrl: vi.fn((path: string) => `/raw?path=${encodeURIComponent(path)}`),
      thumbUrl: vi.fn((path: string) => `/thumb?path=${encodeURIComponent(path)}`),
    },
  },
  isThumbSupported: vi.fn(() => false),
}));

vi.mock("../../lib/agentApi", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/agentApi")>();
  return {
    ...actual,
    agentApi: {
      ...actual.agentApi,
      get: mocks.agentGet,
      messages: mocks.agentMessages,
      getCheckinFile: mocks.checkinFile,
      getUserContext: mocks.userContext,
      avatarUrl: vi.fn((id: string) => `/avatar/${id}`),
      update: vi.fn(),
      credentials: {
        ...actual.agentApi.credentials,
        list: mocks.credentialsList,
        add: vi.fn(),
        update: vi.fn(),
        delete: vi.fn(),
        revealPassword: vi.fn(),
        getTOTPCode: vi.fn(),
        parseQR: vi.fn(),
        parseOTPURI: vi.fn(),
      },
    },
  };
});

vi.mock("./SlackBotSettings", () => ({
  SlackBotSettings: () => null,
}));

function demoAgent() {
  const now = "2026-06-08T00:00:00Z";
  return {
    id: "demo",
    name: "Demo Agent",
    persona: "Demo persona",
    model: "claude-sonnet-4",
    effort: "",
    tool: "claude",
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

function homeRoute() {
  return <div>Home</div>;
}

beforeEach(() => {
  Element.prototype.scrollIntoView = vi.fn();
  mocks.agentGet.mockResolvedValue(demoAgent());
  mocks.agentMessages.mockResolvedValue({ messages: [], hasMore: false });
  mocks.checkinFile.mockResolvedValue({
    value: { content: "", isDefault: true, etag: "" },
    etag: "",
  });
  mocks.userContext.mockResolvedValue({
    value: { content: "", isDefault: true, etag: "" },
    etag: "",
  });
  mocks.credentialsList.mockResolvedValue([]);
  mocks.apiCustomModels.mockResolvedValue([]);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("agent route navigation", () => {
  it("moves credential management to the chat header and keeps browser back on home", async () => {
    const router = createMemoryRouter(
      [
        { path: "/", element: homeRoute() },
        { path: "/agents/:id", element: <AgentChat /> },
        { path: "/agents/:id/credentials", element: <AgentCredentials /> },
      ],
      { initialEntries: ["/", "/agents/demo"], initialIndex: 1 },
    );

    render(<RouterProvider router={router} />);

    const credentialsButton = await screen.findByLabelText("Credentials");
    expect(credentialsButton).toHaveTextContent("🔐");

    fireEvent.click(credentialsButton);
    await waitFor(() => expect(router.state.location.pathname).toBe("/agents/demo/credentials"));

    await router.navigate(-1);
    expect(router.state.location.pathname).toBe("/");
  });

  it("replaces chat when opening settings so returning to chat still backs home", async () => {
    const router = createMemoryRouter(
      [
        { path: "/", element: homeRoute() },
        { path: "/agents/:id", element: <AgentChat /> },
        { path: "/agents/:id/settings", element: <AgentSettings /> },
      ],
      { initialEntries: ["/", "/agents/demo"], initialIndex: 1 },
    );

    render(<RouterProvider router={router} />);

    const settingsButton = await screen.findByTitle("Settings");
    fireEvent.click(settingsButton);
    await waitFor(() => expect(router.state.location.pathname).toBe("/agents/demo/settings"));

    expect(await screen.findByText("Settings")).toBeInTheDocument();
    expect(screen.queryByText("Manage Credentials")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "←" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/agents/demo"));

    await router.navigate(-1);
    expect(router.state.location.pathname).toBe("/");
  });

  it("returns from credentials directly to chat with home behind it", async () => {
    const router = createMemoryRouter(
      [
        { path: "/", element: homeRoute() },
        { path: "/agents/:id", element: <AgentChat /> },
        { path: "/agents/:id/credentials", element: <AgentCredentials /> },
      ],
      { initialEntries: ["/", "/agents/demo/credentials"], initialIndex: 1 },
    );

    render(<RouterProvider router={router} />);

    expect(await screen.findByText("Credentials")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "←" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/agents/demo"));

    await router.navigate(-1);
    expect(router.state.location.pathname).toBe("/");
  });
});

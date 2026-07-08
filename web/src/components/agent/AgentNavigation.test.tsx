import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { StrictMode } from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { BrowserRouter, createMemoryRouter, Route, RouterProvider, Routes, useNavigate } from "react-router";
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

function BrowserHomeRoute() {
  const navigate = useNavigate();
  return <button onClick={() => navigate("/agents/demo")}>Open demo</button>;
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
  window.history.replaceState(null, "", "/");
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
    expect(credentialsButton.querySelector("svg")).not.toBeNull();

    fireEvent.click(credentialsButton);
    await waitFor(() => expect(router.state.location.pathname).toBe("/agents/demo/credentials"));

    await router.navigate(-1);
    expect(router.state.location.pathname).toBe("/");
  });

  it("hides the credentials icon when the credentials injection is disabled", async () => {
    mocks.agentGet.mockResolvedValue({
      ...demoAgent(),
      disabledInjections: ["credentials"],
    });
    const router = createMemoryRouter(
      [
        { path: "/", element: homeRoute() },
        { path: "/agents/:id", element: <AgentChat /> },
      ],
      { initialEntries: ["/", "/agents/demo"], initialIndex: 1 },
    );

    render(<RouterProvider router={router} />);

    await screen.findByTitle("Settings");
    expect(screen.queryByLabelText("Credentials")).not.toBeInTheDocument();
  });

  it("pushes settings so browser back returns directly to chat", async () => {
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

    // (a) Browser back directly from settings lands on chat — settings was
    // pushed onto the stack (not replaced), so the prior chat entry is intact.
    await router.navigate(-1);
    expect(router.state.location.pathname).toBe("/agents/demo");

    // ...and chat still has home behind it (no dead entries were inserted).
    await router.navigate(-1);
    expect(router.state.location.pathname).toBe("/");
  });

  it("UI back then browser back does not land on a duplicate chat", async () => {
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

    // (b) In-UI back pops the pushed settings entry (navigate(-1)) rather
    // than pushing/replacing a new chat route.
    fireEvent.click(await screen.findByRole("button", { name: "←" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/agents/demo"));

    // Browser back from there goes straight to home — there is no duplicate
    // chat entry left behind by the UI back button.
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

  it("keeps BrowserRouter in sync after using the chat back button and reopening chat", async () => {
    // idx 0 simulates a direct-load (bookmark / notification) where
    // there is no prior history entry to pop with navigate(-1).
    window.history.replaceState({ idx: 0 }, "", "/agents/demo");

    render(
      <StrictMode>
        <BrowserRouter>
          <Routes>
            <Route path="/" element={<BrowserHomeRoute />} />
            <Route path="/agents/:id" element={<AgentChat />} />
          </Routes>
        </BrowserRouter>
      </StrictMode>,
    );

    await waitFor(() => expect(screen.getAllByText("Demo Agent").length).toBeGreaterThan(0));

    // First back: idx === 0, so the button falls back to
    // navigate("/", { replace: true }).
    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    expect(await screen.findByText("Open demo")).toBeInTheDocument();

    fireEvent.click(screen.getByText("Open demo"));
    await waitFor(() => expect(screen.getAllByText("Demo Agent").length).toBeGreaterThan(0));

    // Second back: idx > 0 (after in-app navigate), so the button
    // uses navigate(-1) to pop the real history entry instead of
    // accumulating dead "/" entries.
    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    expect(await screen.findByText("Open demo")).toBeInTheDocument();
  });
});

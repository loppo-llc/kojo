import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { Dashboard } from "./Dashboard";

const mocks = vi.hoisted(() => ({
  groupList: vi.fn(),
  unread: vi.fn(),
  openDM: vi.fn(),
  createThread: vi.fn(),
  agentList: vi.fn(),
  clearAttention: vi.fn(),
  sessionsList: vi.fn(),
  peersList: vi.fn(),
}));

vi.mock("../lib/groupdmApi", async (importOriginal) => ({
  // The real classifier, not a copy of it — the Threads / Group DMs split is
  // what this suite asserts on, and a stub would keep passing after the real
  // rule regressed.
  isThreadRoom: (await importOriginal<typeof import("../lib/groupdmApi")>()).isThreadRoom,
  groupdmApi: {
    list: mocks.groupList,
    unread: mocks.unread,
    openDM: mocks.openDM,
    createThread: mocks.createThread,
    create: vi.fn(),
  },
  getLastRead: vi.fn((roomId: string) => (roomId === "g1" ? "m9" : null)),
}));

vi.mock("../lib/api", () => ({
  api: { sessions: { list: mocks.sessionsList } },
}));

// Partial mock: keep the real module (pure helpers like
// isTurnErrorPreview) and replace only the network-touching agentApi.
vi.mock("../lib/agentApi", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../lib/agentApi")>()),
  agentApi: {
    list: mocks.agentList,
    cronPaused: vi.fn().mockResolvedValue(false),
    setCronPaused: vi.fn(),
    forceReclaim: vi.fn(),
    clearAttention: mocks.clearAttention,
  },
}));

vi.mock("../lib/peerApi", () => ({
  peersApi: { list: mocks.peersList },
}));

vi.mock("../hooks/usePushNotifications", () => ({
  usePushNotifications: () => ({ state: "granted", loading: false, subscribe: vi.fn() }),
}));

vi.mock("./agent/AgentAvatar", () => ({
  AgentAvatar: ({ name }: { name: string }) => <span data-testid="avatar">{name}</span>,
}));

const room = (over: Record<string, unknown>) => ({
  id: "g1",
  name: "Team",
  kind: "group",
  members: [
    { agentId: "ag_a", agentName: "Alice" },
    { agentId: "ag_b", agentName: "Bob" },
  ],
  cooldown: 0,
  style: "efficient",
  createdAt: "2026-06-15T00:00:00Z",
  updatedAt: "2026-06-15T00:00:00Z",
  ...over,
});

function renderDashboard(initialPath = "/", variant: "page" | "sidebar" = "page") {
  const router = createMemoryRouter(
    [
      { path: "/", element: <Dashboard variant={variant} /> },
      { path: "/agents/:id", element: <Dashboard variant={variant} /> },
      { path: "/groupdms/:id", element: <div>room page</div> },
    ],
    { initialEntries: [initialPath] },
  );
  render(<RouterProvider router={router} />);
  return router;
}

beforeEach(() => {
  mocks.sessionsList.mockResolvedValue([]);
  mocks.peersList.mockResolvedValue({ items: [] });
  mocks.agentList.mockResolvedValue([
    {
      id: "ag_a",
      name: "Alice",
      tool: "claude",
      createdAt: "2026-06-15T00:00:00Z",
      updatedAt: "2026-06-15T00:00:00Z",
    },
  ]);
  mocks.groupList.mockResolvedValue([
    room({ id: "g1", name: "Team" }),
    room({
      id: "d1",
      name: "Alice",
      kind: "dm",
      members: [{ agentId: "ag_a", agentName: "Alice" }],
    }),
  ]);
  mocks.unread.mockImplementation((id: string) =>
    Promise.resolve(
      id === "d1"
        ? { count: 3, mentionsUser: true, hasMore: false }
        : { count: 0, mentionsUser: false, hasMore: false },
    ),
  );
  mocks.openDM.mockResolvedValue(room({ id: "d1", name: "Alice", kind: "dm" }));
  mocks.clearAttention.mockResolvedValue({ attention: false, cleared: true });
  mocks.createThread.mockResolvedValue(
    room({
      id: "t1",
      name: "Alice",
      kind: "thread",
      members: [{ agentId: "ag_a", agentName: "Alice" }],
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  localStorage.clear();
});

describe("Dashboard agent effort", () => {
  function setAgent(over: Record<string, unknown> = {}) {
    mocks.agentList.mockResolvedValue([{
      id: "ag_a",
      name: "Alice",
      tool: "codex",
      model: "gpt-6-astra",
      effort: "xhigh",
      createdAt: "2026-06-15T00:00:00Z",
      updatedAt: "2026-06-15T00:00:00Z",
      ...over,
    }]);
  }

  it.each(["page", "sidebar"] as const)("shows model and configured effort in the %s list", async (variant) => {
    setAgent();
    renderDashboard("/", variant);
    expect(await screen.findByText("gpt-6-astra")).toBeInTheDocument();
    const effort = screen.getByTitle("Configured effort: xhigh");
    expect(effort).toHaveTextContent("xhigh");
    expect(effort).toHaveClass("shrink-0");
    expect(screen.getByTitle("gpt-6-astra").parentElement).toBe(effort.parentElement);
  });

  it.each(["", undefined])("labels unset effort (%s) as default without inventing a level", async (effort) => {
    setAgent({ effort });
    renderDashboard();
    expect(await screen.findByTitle("Configured effort: default (CLI config)")).toHaveTextContent("default");
    expect(screen.queryByText("medium")).not.toBeInTheDocument();
  });

  it("shows explicit effort even when the model is left to the CLI", async () => {
    setAgent({ model: "", effort: "high" });
    renderDashboard();
    expect(await screen.findByTitle("Configured effort: high")).toHaveTextContent("high");
    expect(screen.queryByText("gpt-6-astra")).not.toBeInTheDocument();
  });

  it.each(["claude", "grok"])("explains the configured ceiling/fallback for automatic %s effort", async (tool) => {
    setAgent({ tool, model: tool === "claude" ? "opus" : "grok-4.6", effort: "high" });
    renderDashboard();
    expect(await screen.findByTitle(/Configured effort: high — Pick per-turn effort automatically/)).toHaveTextContent("high");
  });

  it("does not describe fixed effort as automatic", async () => {
    setAgent({ tool: "claude", model: "opus", effort: "max", autoEffort: false });
    renderDashboard();
    expect(await screen.findByTitle("Configured effort: max")).toHaveTextContent("max");
  });

  it.each(["custom-claude", "custom-codex", "custom-bare"])("does not show unused effort for %s", async (tool) => {
    setAgent({ tool, model: "custom-model", effort: "high" });
    renderDashboard();
    expect(await screen.findByText("custom-model")).toBeInTheDocument();
    expect(screen.queryByTitle(/Configured effort:/)).not.toBeInTheDocument();
    expect(screen.queryByText("high")).not.toBeInTheDocument();
  });
});

describe("Dashboard room list", () => {
  it("splits dm rooms into a Threads section separate from Group DMs", async () => {
    renderDashboard();
    expect(await screen.findByText("Threads · 1")).toBeInTheDocument();
    expect(screen.getByText("Group DMs · 1")).toBeInTheDocument();
  });

  // Regression: an agent↔agent 1:1 DM is kind "dm" with TWO members, and
  // splitting on kind alone filed it under Threads.
  it("keeps a two-member dm room under Group DMs, not Threads", async () => {
    mocks.groupList.mockResolvedValue([
      room({ id: "g1", name: "Team" }),
      room({
        id: "t1",
        name: "Alice",
        kind: "thread",
        members: [{ agentId: "ag_a", agentName: "Alice" }],
      }),
      room({
        id: "d2",
        name: "Alice, Bob",
        kind: "dm",
        members: [
          { agentId: "ag_a", agentName: "Alice" },
          { agentId: "ag_b", agentName: "Bob" },
        ],
      }),
    ]);
    renderDashboard();
    expect(await screen.findByText("Threads · 1")).toBeInTheDocument();
    expect(screen.getByText("Group DMs · 2")).toBeInTheDocument();
  });

  it("aborts a pending peer session list on unmount", async () => {
    let peerSignal: AbortSignal | undefined;
    mocks.peersList.mockResolvedValue({
      items: [{ deviceId: "p_off", name: "offline", isSelf: false }],
    });
    mocks.sessionsList.mockImplementation((peerId?: string, signal?: AbortSignal) => {
      if (!peerId) return Promise.resolve([]);
      peerSignal = signal;
      return new Promise(() => {});
    });
    renderDashboard();
    await waitFor(() => expect(peerSignal).toBeDefined());
    expect(peerSignal!.aborted).toBe(false);
    cleanup();
    expect(peerSignal!.aborted).toBe(true);
  });

  it("renders unread and mention badges from the unread endpoint", async () => {
    renderDashboard();
    expect(await screen.findByLabelText("3 unread")).toBeInTheDocument();
    expect(screen.getByLabelText("Mentions you")).toBeInTheDocument();
    await waitFor(() =>
      expect(mocks.unread).toHaveBeenCalledWith("g1", "m9"),
    );
    expect(mocks.unread).toHaveBeenCalledWith("d1", null);
  });

  it("navigates to a draft thread without creating the room (lazy creation)", async () => {
    const router = renderDashboard();
    fireEvent.click(await screen.findByLabelText("New thread with Alice"));
    // Lazy: the button must NOT create a room — it only opens the draft route.
    await waitFor(() => expect(router.state.location.pathname).toBe("/groupdms/new"));
    expect(router.state.location.search).toBe("?agent=ag_a");
    expect(mocks.createThread).not.toHaveBeenCalled();
  });
});

describe("Dashboard agent error badge", () => {
  const erroredAgent = (over: Record<string, unknown> = {}) => ({
    id: "ag_err",
    name: "Broken",
    tool: "claude",
    createdAt: "2026-06-15T00:00:00Z",
    updatedAt: "2026-06-15T00:00:00Z",
    lastMessage: {
      role: "system",
      content: "⚠️ Error: API error (status 402 Payment Required): balance exhausted",
      timestamp: "2026-06-15T01:00:00Z",
    },
    lastMessageAt: 1750000000000,
    ...over,
  });

  it("badges an agent whose last transcript entry is a turn error", async () => {
    mocks.agentList.mockResolvedValue([erroredAgent()]);
    renderDashboard();
    expect(await screen.findByText("Error")).toBeInTheDocument();
  });

  it("suppresses the badge while a retry turn is in flight (busy)", async () => {
    mocks.agentList.mockResolvedValue([erroredAgent({ busy: true })]);
    renderDashboard();
    await screen.findAllByText("Broken");
    expect(screen.queryByText("Error")).not.toBeInTheDocument();
  });

  it("does not badge a healthy last message", async () => {
    mocks.agentList.mockResolvedValue([
      erroredAgent({
        lastMessage: { role: "assistant", content: "done", timestamp: "2026-06-15T01:00:00Z" },
      }),
    ]);
    renderDashboard();
    await screen.findAllByText("Broken");
    expect(screen.queryByText("Error")).not.toBeInTheDocument();
  });

  it("ranks an errored agent above idle agents regardless of recency", async () => {
    mocks.agentList.mockResolvedValue([
      {
        id: "ag_ok",
        name: "Healthy",
        tool: "claude",
        createdAt: "2026-06-15T00:00:00Z",
        updatedAt: "2026-06-15T00:00:00Z",
        lastMessage: { role: "assistant", content: "hi", timestamp: "2026-06-16T00:00:00Z" },
        lastMessageAt: 1760000000000, // newer than the errored agent
      },
      erroredAgent(),
    ]);
    renderDashboard();
    await screen.findByText("Error");
    const names = screen
      .getAllByText(/^(Broken|Healthy)$/)
      .map((el) => el.textContent)
      // AgentAvatar mock also renders the name; keep first occurrence order.
      .filter((v, i, arr) => arr.indexOf(v) === i);
    expect(names).toEqual(["Broken", "Healthy"]);
  });
});

describe("Dashboard agent attention", () => {
  const agent = (over: Record<string, unknown> = {}) => ({
    id: "ag_a",
    name: "Alice",
    tool: "claude",
    createdAt: "2026-06-15T00:00:00Z",
    updatedAt: "2026-06-15T00:00:00Z",
    ...over,
  });

  it("badges a paged agent and shows the reason inline", async () => {
    mocks.agentList.mockResolvedValue([
      agent({ attention: true, attentionReason: "デプロイの承認が要る", attentionAt: 1760000000000 }),
    ]);
    renderDashboard();
    expect(await screen.findByText("Wants you")).toBeInTheDocument();
    expect(screen.getByText("デプロイの承認が要る")).toBeInTheDocument();
  });

  it("shows no badge when the agent has not paged", async () => {
    mocks.agentList.mockResolvedValue([agent()]);
    renderDashboard();
    await screen.findAllByText("Alice");
    expect(screen.queryByText("Wants you")).not.toBeInTheDocument();
  });

  it("lets the blocking awaiting-answer state win over a page", async () => {
    mocks.agentList.mockResolvedValue([
      agent({ awaitingAnswer: true, attention: true, attentionReason: "見て" }),
    ]);
    renderDashboard();
    expect(await screen.findByText("Awaiting answer")).toBeInTheDocument();
    expect(screen.queryByText("Wants you")).not.toBeInTheDocument();
  });

  it("ranks a paged agent above busy and idle, below awaiting answer", async () => {
    mocks.agentList.mockResolvedValue([
      agent({ id: "ag_idle", name: "Idle", lastMessageAt: 1770000000000 }),
      agent({ id: "ag_busy", name: "Busy", busy: true, lastMessageAt: 1770000000000 }),
      agent({ id: "ag_page", name: "Paged", attention: true, lastMessageAt: 1000 }),
      agent({ id: "ag_ask", name: "Asking", awaitingAnswer: true, lastMessageAt: 1000 }),
    ]);
    renderDashboard();
    await screen.findByText("Wants you");
    const names = screen
      .getAllByText(/^(Idle|Busy|Paged|Asking)$/)
      .map((el) => el.textContent)
      .filter((v, i, arr) => arr.indexOf(v) === i);
    expect(names).toEqual(["Asking", "Paged", "Busy", "Idle"]);
  });

  it("clears the page when the operator is looking at that agent's chat", async () => {
    mocks.agentList.mockResolvedValue([
      agent({ id: "ag_page", name: "Paged", attention: true, attentionReason: "見て" }),
    ]);
    renderDashboard("/agents/ag_page");
    await waitFor(() => expect(mocks.clearAttention).toHaveBeenCalledWith("ag_page"));
    // Optimistic local clear: the badge disappears without waiting for a poll.
    await waitFor(() => expect(screen.queryByText("Wants you")).not.toBeInTheDocument());
  });

  it("does not clear a page for an agent the operator is not viewing", async () => {
    mocks.agentList.mockResolvedValue([
      agent({ id: "ag_page", name: "Paged", attention: true }),
      agent({ id: "ag_open", name: "Open" }),
    ]);
    renderDashboard("/agents/ag_open");
    await screen.findByText("Wants you");
    expect(mocks.clearAttention).not.toHaveBeenCalled();
  });
});

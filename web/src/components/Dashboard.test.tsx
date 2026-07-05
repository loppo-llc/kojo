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
}));

vi.mock("../lib/groupdmApi", () => ({
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
  api: { sessions: { list: vi.fn().mockResolvedValue([]) } },
}));

vi.mock("../lib/agentApi", () => ({
  agentApi: {
    list: mocks.agentList,
    cronPaused: vi.fn().mockResolvedValue(false),
    setCronPaused: vi.fn(),
    forceReclaim: vi.fn(),
  },
}));

vi.mock("../lib/peerApi", () => ({
  peersApi: { list: vi.fn().mockResolvedValue({ items: [] }) },
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

function renderDashboard() {
  const router = createMemoryRouter(
    [
      { path: "/", element: <Dashboard /> },
      { path: "/groupdms/:id", element: <div>room page</div> },
    ],
    { initialEntries: ["/"] },
  );
  render(<RouterProvider router={router} />);
  return router;
}

beforeEach(() => {
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

describe("Dashboard room list", () => {
  it("splits dm rooms into a Threads section separate from Group DMs", async () => {
    renderDashboard();
    expect(await screen.findByText("Threads · 1")).toBeInTheDocument();
    expect(screen.getByText("Group DMs · 1")).toBeInTheDocument();
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

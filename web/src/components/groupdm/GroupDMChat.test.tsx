import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { GroupDMChat } from "./GroupDMChat";

const mocks = vi.hoisted(() => ({
  groupGet: vi.fn(),
  groupMessages: vi.fn(),
  clearMessages: vi.fn(),
}));

vi.mock("../../lib/groupdmApi", () => ({
  DEFAULT_GROUPDM_VENUE: "chatroom",
  USER_SENDER_ID: "user",
  groupdmApi: {
    get: mocks.groupGet,
    messages: mocks.groupMessages,
    clearMessages: mocks.clearMessages,
    setStyle: vi.fn(),
    setVenue: vi.fn(),
    setCooldown: vi.fn(),
    postUserMessage: vi.fn(),
    delete: vi.fn(),
  },
}));

vi.mock("../../lib/api", () => ({
  api: {
    upload: vi.fn(),
    files: {
      rawUrl: vi.fn((path: string) => `/raw?path=${encodeURIComponent(path)}`),
    },
  },
}));

vi.mock("../../lib/preferences", () => ({
  useEnterSends: () => [true],
}));

vi.mock("../agent/AgentAvatar", () => ({
  AgentAvatar: ({ name }: { name: string }) => <span data-testid="avatar">{name}</span>,
}));

function renderGroup() {
  const router = createMemoryRouter(
    [
      { path: "/", element: <div>Home</div> },
      { path: "/groupdms/:id", element: <GroupDMChat /> },
    ],
    { initialEntries: ["/groupdms/g1"] },
  );
  render(<RouterProvider router={router} />);
  return router;
}

beforeEach(() => {
  Element.prototype.scrollIntoView = vi.fn();
  mocks.groupGet.mockResolvedValue({
    id: "g1",
    name: "Team",
    members: [
      { agentId: "ag_alice", agentName: "Alice", status: "online" },
      { agentId: "ag_bob", agentName: "Bob", status: "offline" },
    ],
    cooldown: 0,
    style: "efficient",
    venue: "chatroom",
    createdAt: "2026-06-15T00:00:00Z",
    updatedAt: "2026-06-15T00:00:00Z",
  });
  mocks.groupMessages.mockResolvedValue({
    messages: [
      {
        id: "m1",
        agentId: "ag_alice",
        agentName: "Alice",
        content: "hello history",
        timestamp: "2026-06-15T00:00:01Z",
      },
    ],
    hasMore: false,
  });
  mocks.clearMessages.mockResolvedValue({ ok: true, deleted: 1 });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("GroupDMChat clear history", () => {
  it("clears message history from the header action", async () => {
    renderGroup();

    expect(await screen.findByText("hello history")).toBeInTheDocument();

    fireEvent.click(screen.getByTitle("Clear message history"));
    expect(await screen.findByText("Clear history?")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Clear" }));

    await waitFor(() => expect(mocks.clearMessages).toHaveBeenCalledWith("g1"));
    await waitFor(() => {
      expect(screen.queryByText("hello history")).not.toBeInTheDocument();
    });
    expect(screen.getByText("No messages yet")).toBeInTheDocument();
  });
});

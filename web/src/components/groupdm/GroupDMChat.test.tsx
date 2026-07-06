import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { GroupDMChat } from "./GroupDMChat";

const mocks = vi.hoisted(() => ({
  groupGet: vi.fn(),
  groupMessages: vi.fn(),
  clearMessages: vi.fn(),
  setMaxHops: vi.fn(),
  setLastRead: vi.fn(),
  archive: vi.fn(),
  agentGet: vi.fn(),
  createThread: vi.fn(),
  postUserMessage: vi.fn(),
  steer: vi.fn(),
  threadLive: vi.fn(),
}));

vi.mock("../../lib/agentApi", () => ({
  agentApi: { get: mocks.agentGet },
}));

vi.mock("../../lib/groupdmApi", () => ({
  DEFAULT_GROUPDM_VENUE: "chatroom",
  USER_SENDER_ID: "user",
  DEFAULT_MAX_HOPS: 4,
  MAX_MAX_HOPS: 20,
  setLastRead: mocks.setLastRead,
  getLastRead: vi.fn(() => null),
  clearLastRead: vi.fn(),
  groupdmApi: {
    get: mocks.groupGet,
    messages: mocks.groupMessages,
    clearMessages: mocks.clearMessages,
    setStyle: vi.fn(),
    setVenue: vi.fn(),
    setCooldown: vi.fn(),
    setMaxHops: mocks.setMaxHops,
    postUserMessage: mocks.postUserMessage,
    createThread: mocks.createThread,
    markRead: vi.fn(() => Promise.resolve({ ok: true })),
    delete: vi.fn(),
    archive: mocks.archive,
    steer: mocks.steer,
    threadLive: mocks.threadLive,
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
  mocks.agentGet.mockResolvedValue({ id: "ag_alice", name: "Alice", model: "claude-sonnet-4-5" });
  mocks.createThread.mockResolvedValue({
    id: "t1",
    name: "無題のスレッド",
    kind: "thread",
    members: [{ agentId: "ag_alice", agentName: "Alice" }],
    cooldown: 0,
    style: "efficient",
    createdAt: "2026-06-15T00:00:00Z",
    updatedAt: "2026-06-15T00:00:00Z",
  });
  mocks.postUserMessage.mockResolvedValue({
    id: "m_new",
    agentId: "user",
    agentName: "User",
    content: "hi there",
    timestamp: "2026-06-15T00:00:05Z",
  });
  mocks.threadLive.mockResolvedValue({ active: false });
});

function renderDraft() {
  const router = createMemoryRouter(
    [
      { path: "/", element: <div>Home</div> },
      { path: "/groupdms/new", element: <GroupDMChat /> },
      { path: "/groupdms/:id", element: <GroupDMChat /> },
    ],
    { initialEntries: ["/groupdms/new?agent=ag_alice"] },
  );
  render(<RouterProvider router={router} />);
  return router;
}

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

describe("GroupDMChat unread tracking", () => {
  it("marks the latest fetched message as read", async () => {
    renderGroup();
    expect(await screen.findByText("hello history")).toBeInTheDocument();
    await waitFor(() => expect(mocks.setLastRead).toHaveBeenCalledWith("g1", "m1"));
  });
});

describe("GroupDMChat max hops setting", () => {
  it("shows the default and patches maxHops on submit", async () => {
    mocks.setMaxHops.mockResolvedValue({
      id: "g1",
      name: "Team",
      members: [],
      cooldown: 0,
      style: "efficient",
      maxHops: 8,
      createdAt: "2026-06-15T00:00:00Z",
      updatedAt: "2026-06-15T00:00:00Z",
    });
    renderGroup();

    const btn = await screen.findByTitle("Max relay hops (empty = default 4, max 20)");
    expect(btn).toHaveTextContent("4hops");

    fireEvent.click(btn);
    const input = screen.getByLabelText("Max hops");
    fireEvent.change(input, { target: { value: "8" } });
    fireEvent.submit(input.closest("form")!);

    await waitFor(() => expect(mocks.setMaxHops).toHaveBeenCalledWith("g1", 8));
    expect(
      await screen.findByTitle("Max relay hops (empty = default 4, max 20)"),
    ).toHaveTextContent("8hops");
  });
});

describe("GroupDMChat thread room", () => {
  const threadGroup = {
    id: "g1",
    name: "Alice",
    kind: "dm" as const,
    members: [{ agentId: "ag_alice", agentName: "Alice", status: "online" as const }],
    cooldown: 0,
    style: "efficient" as const,
    venue: "chatroom" as const,
    createdAt: "2026-06-15T00:00:00Z",
    updatedAt: "2026-06-15T00:00:00Z",
  };

  it("shows Archive (not group settings) and archives on confirm", async () => {
    mocks.groupGet.mockResolvedValue(threadGroup);
    mocks.archive.mockResolvedValue({ ok: true });
    const router = renderGroup();

    const archiveBtn = await screen.findByTitle("Archive thread");
    expect(archiveBtn).toBeInTheDocument();
    // Group-only affordances are hidden for threads: cooldown, hops, style,
    // venue, and clear-history.
    expect(screen.queryByTitle("Max relay hops (empty = default 4, max 20)")).not.toBeInTheDocument();
    expect(screen.queryByTitle("Notification cooldown (seconds)")).not.toBeInTheDocument();
    expect(screen.queryByTitle(/^Style:/)).not.toBeInTheDocument();
    expect(screen.queryByTitle(/^Venue:/)).not.toBeInTheDocument();
    expect(screen.queryByTitle("Clear message history")).not.toBeInTheDocument();

    fireEvent.click(archiveBtn);
    expect(await screen.findByText("Archive “Alice”?")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Archive" }));

    await waitFor(() => expect(mocks.archive).toHaveBeenCalledWith("g1"));
    await waitFor(() => expect(router.state.location.pathname).toBe("/"));
  });

  it("renders a token usage line under an agent reply", async () => {
    mocks.groupGet.mockResolvedValue(threadGroup);
    mocks.groupMessages.mockResolvedValue({
      messages: [
        {
          id: "m1",
          agentId: "ag_alice",
          agentName: "Alice",
          content: "here you go",
          usage: { inputTokens: 1200, outputTokens: 340 },
          timestamp: "2026-06-15T00:00:01Z",
        },
      ],
      hasMore: false,
    });
    renderGroup();

    expect(await screen.findByText("here you go")).toBeInTheDocument();
    // "1,200→340 tokens" (locale-formatted). Match on the arrow-joined counts.
    expect(await screen.findByText(/1,200.*340 tokens/)).toBeInTheDocument();
  });
});

describe("GroupDMChat steering", () => {
  const threadGroup = {
    id: "g1",
    name: "Alice",
    kind: "dm" as const,
    members: [{ agentId: "ag_alice", agentName: "Alice", status: "online" as const }],
    cooldown: 0,
    style: "efficient" as const,
    venue: "chatroom" as const,
    createdAt: "2026-06-15T00:00:00Z",
    updatedAt: "2026-06-15T00:00:00Z",
  };

  function withAwaitingReply() {
    mocks.groupGet.mockResolvedValue(threadGroup);
    mocks.groupMessages.mockResolvedValue({
      messages: [
        {
          id: "m1",
          agentId: "user",
          agentName: "User",
          content: "hi there",
          timestamp: new Date().toISOString(),
        },
      ],
      hasMore: false,
    });
  }

  it("sends via steer (not postUserMessage) while a reply is in flight, and does not disable the send button", async () => {
    withAwaitingReply();
    mocks.steer.mockResolvedValue({
      id: "m_steer",
      agentId: "user",
      agentName: "User",
      content: "wait, actually",
      timestamp: "2026-06-15T00:00:06Z",
    });
    renderGroup();

    const input = await screen.findByPlaceholderText(/Steer the running reply/);
    fireEvent.change(input, { target: { value: "wait, actually" } });

    const sendBtn = screen.getByRole("button", { name: /send/i });
    expect(sendBtn).not.toBeDisabled();

    fireEvent.click(sendBtn);

    await waitFor(() => expect(mocks.steer).toHaveBeenCalledWith("g1", "wait, actually"));
    expect(mocks.postUserMessage).not.toHaveBeenCalled();
    expect(await screen.findByText("wait, actually")).toBeInTheDocument();
  });

  it("falls back to postUserMessage when steer rejects with 409", async () => {
    withAwaitingReply();
    mocks.steer.mockRejectedValue(new Error("409: no turn in flight"));
    mocks.postUserMessage.mockResolvedValue({
      id: "m_fallback",
      agentId: "user",
      agentName: "User",
      content: "fallback text",
      timestamp: "2026-06-15T00:00:07Z",
    });
    renderGroup();

    const input = await screen.findByPlaceholderText(/Steer the running reply/);
    fireEvent.change(input, { target: { value: "fallback text" } });
    fireEvent.click(screen.getByRole("button", { name: /send/i }));

    await waitFor(() => expect(mocks.steer).toHaveBeenCalled());
    await waitFor(() =>
      expect(mocks.postUserMessage).toHaveBeenCalledWith("g1", "fallback text", undefined),
    );
  });
});

describe("GroupDMChat draft (lazy thread creation)", () => {
  it("does not create the room on mount and shows the thread placeholder", async () => {
    renderDraft();
    // Header renders from the agent, empty transcript, no room created yet.
    expect(await screen.findByText("No messages yet")).toBeInTheDocument();
    expect(mocks.createThread).not.toHaveBeenCalled();
    expect(mocks.groupGet).not.toHaveBeenCalled();
    expect(
      screen.getByPlaceholderText(/Message this thread/),
    ).toBeInTheDocument();
  });

  it("creates the room and posts on the first send, then swaps to the real url", async () => {
    const router = renderDraft();
    const textarea = await screen.findByPlaceholderText(/Message this thread/);
    fireEvent.change(textarea, { target: { value: "hi there" } });
    fireEvent.click(screen.getByLabelText("Send"));

    await waitFor(() => expect(mocks.createThread).toHaveBeenCalledWith("ag_alice"));
    await waitFor(() =>
      expect(mocks.postUserMessage).toHaveBeenCalledWith("t1", "hi there", undefined),
    );
    await waitFor(() => expect(router.state.location.pathname).toBe("/groupdms/t1"));
  });
});

describe("GroupDMChat thread placeholder", () => {
  it("uses thread wording for a thread room", async () => {
    mocks.groupGet.mockResolvedValue({
      id: "g1",
      name: "Alice",
      kind: "thread",
      members: [{ agentId: "ag_alice", agentName: "Alice" }],
      cooldown: 0,
      style: "efficient",
      createdAt: "2026-06-15T00:00:00Z",
      updatedAt: "2026-06-15T00:00:00Z",
    });
    renderGroup();
    expect(
      await screen.findByPlaceholderText(/Message this thread/),
    ).toBeInTheDocument();
  });
});

describe("GroupDMChat mention highlight", () => {
  it("badges messages that mention the user", async () => {
    mocks.groupMessages.mockResolvedValue({
      messages: [
        {
          id: "m1",
          agentId: "ag_alice",
          agentName: "Alice",
          content: "hey @user look at this",
          mentions: ["user"],
          timestamp: "2026-06-15T00:00:01Z",
        },
        {
          id: "m2",
          agentId: "ag_bob",
          agentName: "Bob",
          content: "just chatting",
          timestamp: "2026-06-15T00:00:02Z",
        },
      ],
      hasMore: false,
    });
    renderGroup();

    expect(await screen.findByText("just chatting")).toBeInTheDocument();
    expect(screen.getAllByTitle("Mentions you")).toHaveLength(1);
    // The mentioning message row is visually highlighted; the other is not.
    const mentioned = screen.getByText("hey @user look at this").closest(".border-copper\\/60");
    expect(mentioned).not.toBeNull();
    expect(screen.getByText("just chatting").closest(".border-copper\\/60")).toBeNull();
  });
});

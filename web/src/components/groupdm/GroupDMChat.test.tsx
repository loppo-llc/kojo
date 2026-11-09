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
  stopThread: vi.fn(),
  rewindToMessage: vi.fn(),
}));

vi.mock("../../lib/agentApi", () => ({
  agentApi: { get: mocks.agentGet },
}));

vi.mock("../../lib/groupdmApi", async (importOriginal) => ({
  // The real classifier, not a copy: the thread-vs-room split is what the
  // component branches on, so a stub would make every affordance assertion
  // vacuous the moment the real rule changed.
  isThreadRoom: (await importOriginal<typeof import("../../lib/groupdmApi")>()).isThreadRoom,
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
    stopThread: mocks.stopThread,
    rewindToMessage: mocks.rewindToMessage,
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
  Element.prototype.scrollTo = vi.fn() as unknown as typeof Element.prototype.scrollTo;
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

  it("marks messages from external agents with a badge", async () => {
    mocks.groupMessages.mockResolvedValue({
      messages: [
        { id: "m1", agentId: "ag_alice", agentName: "Alice", content: "from kojo", timestamp: "2026-06-15T00:00:01Z" },
        { id: "m2", agentId: "xa_ext", agentName: "Codex", content: "from outside", timestamp: "2026-06-15T00:00:02Z" },
      ],
      hasMore: false,
    });
    renderGroup();

    expect(await screen.findByText("from outside")).toBeInTheDocument();
    expect(screen.getAllByText(/^(外部|external)$/)).toHaveLength(1);
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

  it("shows a model/effort label and an interrupted chip on an agent reply", async () => {
    mocks.groupGet.mockResolvedValue(threadGroup);
    mocks.groupMessages.mockResolvedValue({
      messages: [
        {
          id: "m1",
          agentId: "ag_alice",
          agentName: "Alice",
          content: "partial answer",
          model: "opus",
          effort: "high",
          interrupted: true,
          timestamp: "2026-06-15T00:00:01Z",
        },
        {
          // Empty-content interrupted reply (stop landed before any text):
          // the chip must still render without a broken empty bubble.
          id: "m2",
          agentId: "ag_alice",
          agentName: "Alice",
          content: "",
          model: "opus",
          interrupted: true,
          timestamp: "2026-06-15T00:00:02Z",
        },
        {
          id: "m3",
          agentId: "user",
          agentName: "User",
          content: "a user message",
          timestamp: "2026-06-15T00:00:03Z",
        },
      ],
      hasMore: false,
    });
    renderGroup();

    expect(await screen.findByText("partial answer")).toBeInTheDocument();
    expect(screen.getByText("opus · high")).toBeInTheDocument();
    expect(screen.getAllByText("interrupted")).toHaveLength(2);
  });

  it("stops the in-flight turn from the composer stop button", async () => {
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
    mocks.stopThread.mockResolvedValue({ stopped: true });
    renderGroup();

    const stopBtn = await screen.findByRole("button", { name: "Stop" });
    fireEvent.click(stopBtn);
    await waitFor(() => expect(mocks.stopThread).toHaveBeenCalledWith("g1"));
  });

  it("keeps an in-flight turn pending past the former 60-minute limit", async () => {
    mocks.groupGet.mockResolvedValue(threadGroup);
    mocks.groupMessages.mockResolvedValue({
      messages: [
        {
          id: "m1",
          agentId: "user",
          agentName: "User",
          content: "long-running task",
          timestamp: new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString(),
        },
      ],
      hasMore: false,
    });
    mocks.threadLive.mockResolvedValue({ active: true, status: "working" });
    renderGroup();

    expect(await screen.findByRole("button", { name: "Stop" })).toBeInTheDocument();
    await waitFor(() => expect(mocks.threadLive).toHaveBeenCalledWith("g1"));
  });

  it("clears pending state when a live turn finishes without a reply message", async () => {
    mocks.groupGet.mockResolvedValue(threadGroup);
    mocks.groupMessages.mockResolvedValue({
      messages: [
        {
          id: "m1",
          agentId: "user",
          agentName: "User",
          content: "question with an empty answer",
          timestamp: new Date().toISOString(),
        },
      ],
      hasMore: false,
    });
    mocks.threadLive
      .mockResolvedValueOnce({ active: true, status: "working" })
      .mockResolvedValue({ active: false });
    renderGroup();

    expect(await screen.findByRole("button", { name: "Stop" })).toBeInTheDocument();
    await waitFor(() => expect(mocks.threadLive.mock.calls.length).toBeGreaterThanOrEqual(2), {
      timeout: 2500,
    });
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Stop" })).not.toBeInTheDocument(),
    );
  });

  it("waits for two inactive polls before treating a never-observed turn as settled", async () => {
    mocks.groupGet.mockResolvedValue(threadGroup);
    mocks.groupMessages.mockResolvedValue({
      messages: [
        {
          id: "m1",
          agentId: "user",
          agentName: "User",
          content: "question",
          timestamp: new Date().toISOString(),
        },
      ],
      hasMore: false,
    });
    let resolveSecond: (value: { active: boolean }) => void = () => {
      throw new Error("second live poll was not started");
    };
    mocks.threadLive
      .mockResolvedValueOnce({ active: false })
      .mockImplementationOnce(
        () => new Promise((resolve) => {
          resolveSecond = resolve;
        }),
      )
      .mockResolvedValue({ active: false });
    renderGroup();

    const stopButton = await screen.findByRole("button", { name: "Stop" });
    await waitFor(() => expect(mocks.threadLive.mock.calls.length).toBeGreaterThanOrEqual(2), {
      timeout: 2500,
    });
    expect(stopButton).toBeInTheDocument();
    resolveSecond({ active: false });
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Stop" })).not.toBeInTheDocument(),
    );
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

  it("clears the draft without retrying when remote steer delivery is uncertain", async () => {
    withAwaitingReply();
    mocks.steer.mockRejectedValue(
      new Error('502: {"error":{"code":"delivery_uncertain","message":"maybe delivered"}}'),
    );
    renderGroup();

    const input = await screen.findByPlaceholderText(/Steer the running reply/);
    fireEvent.change(input, { target: { value: "do not duplicate" } });
    fireEvent.click(screen.getByRole("button", { name: /send/i }));

    await waitFor(() => expect(mocks.steer).toHaveBeenCalledWith("g1", "do not duplicate"));
    await waitFor(() => expect(input).toHaveValue(""));
    expect(mocks.postUserMessage).not.toHaveBeenCalled();
    expect(await screen.findByText(/may have arrived/i)).toBeInTheDocument();
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

describe("GroupDMChat thread rewind", () => {
  const threadGroup = {
    id: "g1",
    name: "Alice",
    kind: "thread" as const,
    members: [{ agentId: "ag_alice", agentName: "Alice", status: "online" as const }],
    cooldown: 0,
    style: "efficient" as const,
    venue: "chatroom" as const,
    createdAt: "2026-06-15T00:00:00Z",
    updatedAt: "2026-06-15T00:00:00Z",
  };
  const u1 = { id: "u1", agentId: "user", agentName: "User", content: "first ask", timestamp: "2026-06-15T00:00:01Z" };
  const a1 = { id: "a1", agentId: "ag_alice", agentName: "Alice", content: "first answer", timestamp: "2026-06-15T00:00:02Z" };
  const u2 = { id: "u2", agentId: "user", agentName: "User", content: "second ask", timestamp: "2026-06-15T00:00:03Z" };
  const a2 = { id: "a2", agentId: "ag_alice", agentName: "Alice", content: "second answer", timestamp: "2026-06-15T00:00:04Z" };

  beforeEach(() => {
    // The composer draft persists in sessionStorage; a restored prompt from
    // one case would otherwise block the restore in the next.
    sessionStorage.clear();
  });
  afterEach(() => {
    vi.restoreAllMocks();
    sessionStorage.clear();
  });

  it("rewinds to a user message and puts its text back in the composer", async () => {
    mocks.groupGet.mockResolvedValue(threadGroup);
    let rewound = false;
    mocks.groupMessages.mockImplementation(() =>
      Promise.resolve({ messages: rewound ? [u1, a1] : [u1, a1, u2, a2], hasMore: false }),
    );
    mocks.rewindToMessage.mockImplementation(() => {
      rewound = true;
      return Promise.resolve({ messagesRemoved: 2 });
    });
    vi.spyOn(window, "confirm").mockReturnValue(true);
    renderGroup();

    expect(await screen.findByText("second answer")).toBeInTheDocument();
    const buttons = screen.getAllByTitle(/^(Undo sending|この発言を送る前に戻す)/);
    expect(buttons).toHaveLength(2); // one per user message
    fireEvent.click(buttons[1]);

    await waitFor(() => expect(mocks.rewindToMessage).toHaveBeenCalledWith("g1", "u2"));
    await waitFor(() => expect(screen.queryByText("second answer")).not.toBeInTheDocument());
    expect(screen.getByText("first answer")).toBeInTheDocument();
    expect((screen.getByRole("textbox") as HTMLTextAreaElement).value).toBe("second ask");
  });

  it("restores the transcript and clears the composer when the rewind fails", async () => {
    mocks.groupGet.mockResolvedValue(threadGroup);
    mocks.groupMessages.mockResolvedValue({ messages: [u1, a1, u2, a2], hasMore: false });
    mocks.rewindToMessage.mockRejectedValue(new Error("busy"));
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const alertSpy = vi.spyOn(window, "alert").mockImplementation(() => {});
    vi.spyOn(console, "error").mockImplementation(() => {});
    renderGroup();

    expect(await screen.findByText("second answer")).toBeInTheDocument();
    fireEvent.click(screen.getAllByTitle(/^(Undo sending|この発言を送る前に戻す)/)[1]);

    await waitFor(() => expect(alertSpy).toHaveBeenCalled());
    expect(screen.getByText("second answer")).toBeInTheDocument();
    expect((screen.getByRole("textbox") as HTMLTextAreaElement).value).toBe("");
  });

  it("offers no rewind while a reply is pending or in a group room", async () => {
    mocks.groupGet.mockResolvedValue(threadGroup);
    mocks.groupMessages.mockResolvedValue({ messages: [u1, a1, u2], hasMore: false });
    mocks.threadLive.mockResolvedValue({ active: true });
    renderGroup();
    expect(await screen.findByText("second ask")).toBeInTheDocument();
    expect(screen.queryAllByTitle(/^(Undo sending|この発言を送る前に戻す)/)).toHaveLength(0);
    cleanup();

    mocks.groupGet.mockResolvedValue({ ...threadGroup, kind: "group" as const, members: [
      { agentId: "ag_alice", agentName: "Alice", status: "online" as const },
      { agentId: "ag_bob", agentName: "Bob", status: "online" as const },
    ] });
    mocks.groupMessages.mockResolvedValue({ messages: [u1, a1], hasMore: false });
    renderGroup();
    expect(await screen.findByText("first answer")).toBeInTheDocument();
    expect(screen.queryAllByTitle(/^(Undo sending|この発言を送る前に戻す)/)).toHaveLength(0);
  });

  it("drops rows another client rewound while the page reports more history", async () => {
    mocks.groupGet.mockResolvedValue(threadGroup);
    const old = Array.from({ length: 3 }, (_, i) => ({
      id: `o${i}`, agentId: "ag_alice", agentName: "Alice", content: `old ${i}`, timestamp: "2026-06-15T00:00:00Z",
    }));
    let page = { messages: [u1, a1, u2, a2], hasMore: true };
    mocks.groupMessages.mockImplementation((_id: string, _limit: number, before?: string) =>
      Promise.resolve(before ? { messages: old, hasMore: false } : page),
    );
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      renderGroup();
      expect(await screen.findByText("second answer")).toBeInTheDocument();
      // Simulate a rewind from elsewhere; the newest page now starts earlier.
      page = { messages: [old[2], u1, a1], hasMore: true };
      await vi.advanceTimersByTimeAsync(3100);
      await waitFor(() => expect(screen.queryByText("second answer")).not.toBeInTheDocument());
      expect(screen.queryByText("second ask")).not.toBeInTheDocument();
      expect(screen.getByText("first answer")).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });
});

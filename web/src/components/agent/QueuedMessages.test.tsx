import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueuedMessages } from "./QueuedMessages";
import type { QueuedAgentMessage } from "../../lib/agentApi";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function queued(id: string, content: string, createdAt = 1783990800000): QueuedAgentMessage {
  return {
    id,
    agentId: "agent-1",
    holderPeer: "peer-abcdef",
    content,
    createdAt,
    status: "queued",
  };
}

describe("QueuedMessages", () => {
  it("renders nothing when the queue is empty", () => {
    const { container } = render(
      <QueuedMessages messages={[]} holderPeerName="macbook" onCancel={() => {}} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("lists queued messages with the reconnect notice", () => {
    render(
      <QueuedMessages
        messages={[queued("q1", "hello there"), queued("q2", "second one")]}
        holderPeerName="macbook"
        onCancel={() => {}}
      />,
    );
    expect(screen.getByText(/2 messages queued/)).toBeInTheDocument();
    expect(screen.getByText(/will deliver when device/)).toBeInTheDocument();
    expect(screen.getByText("macbook")).toBeInTheDocument();
    expect(screen.getByText("hello there")).toBeInTheDocument();
    expect(screen.getByText("second one")).toBeInTheDocument();
  });

  it("uses singular wording for one queued message", () => {
    render(
      <QueuedMessages messages={[queued("q1", "hi")]} holderPeerName="mb" onCancel={() => {}} />,
    );
    expect(screen.getByText(/1 message queued/)).toBeInTheDocument();
  });

  it("truncates long content to a snippet but keeps the full text as title", () => {
    const long = "x".repeat(120);
    render(
      <QueuedMessages messages={[queued("q1", long)]} holderPeerName="mb" onCancel={() => {}} />,
    );
    const row = screen.getByTitle(long);
    expect(row.textContent).toBe("x".repeat(80) + "…");
  });

  it("invokes onCancel with the queued message id", () => {
    const onCancel = vi.fn();
    render(
      <QueuedMessages
        messages={[queued("q1", "a"), queued("q2", "b")]}
        holderPeerName="mb"
        onCancel={onCancel}
      />,
    );
    fireEvent.click(screen.getByLabelText("Cancel queued message q2"));
    expect(onCancel).toHaveBeenCalledTimes(1);
    expect(onCancel).toHaveBeenCalledWith("q2");
  });
});

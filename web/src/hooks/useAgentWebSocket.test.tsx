import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { useAgentWebSocket } from "./useAgentWebSocket";

class FakeWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;

  static instances: FakeWebSocket[] = [];

  readyState = FakeWebSocket.OPEN;
  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onclose: ((event: CloseEvent) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  close = vi.fn();
  send = vi.fn();

  constructor(readonly url: string) {
    FakeWebSocket.instances.push(this);
  }
}

function TestClient({ onDisconnect }: { onDisconnect: () => void }) {
  useAgentWebSocket({
    agentId: "demo",
    onEvent: vi.fn(),
    onDisconnect,
  });
  return null;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  FakeWebSocket.instances = [];
});

describe("useAgentWebSocket", () => {
  it("closes cleanly without running disconnect handlers on unmount", () => {
    vi.stubGlobal("WebSocket", FakeWebSocket);
    const onDisconnect = vi.fn();

    const { unmount } = render(<TestClient onDisconnect={onDisconnect} />);
    const ws = FakeWebSocket.instances[0];

    unmount();

    expect(ws.close).toHaveBeenCalledWith(1000, "route change");
    expect(ws.onclose).toBeNull();
    expect(onDisconnect).not.toHaveBeenCalled();
  });
});

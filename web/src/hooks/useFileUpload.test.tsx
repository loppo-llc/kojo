import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useFileUpload } from "./useFileUpload";

const uploadMock = vi.fn();
vi.mock("../lib/api", () => ({
  api: { upload: (...args: unknown[]) => uploadMock(...args) },
}));

function Harness({ peerId }: { peerId?: string }) {
  const { fileInputRef, handleFileSelect } = useFileUpload(peerId);
  return <input ref={fileInputRef} type="file" onChange={handleFileSelect} />;
}

function BusyHarness({ peerId, scope }: { peerId?: string; scope?: string }) {
  const { fileInputRef, handleFileSelect, uploading, pendingFiles } = useFileUpload(peerId, scope);
  return (
    <>
      <input ref={fileInputRef} type="file" onChange={handleFileSelect} />
      <span data-testid="state">{uploading ? "busy" : "idle"}:{pendingFiles.map((f) => f.name).join(",")}</span>
    </>
  );
}

afterEach(() => {
  uploadMock.mockReset();
});

describe("useFileUpload", () => {
  it("uploads directly to the current remote holder", async () => {
    uploadMock.mockResolvedValue({ path: "/peer/upload/a.txt", name: "a.txt", size: 1, mime: "text/plain" });
    const { container, rerender } = render(<Harness peerId="peer-a" />);
    const input = container.querySelector("input")!;

    fireEvent.change(input, { target: { files: [new File(["a"], "a.txt", { type: "text/plain" })] } });
    await waitFor(() => expect(uploadMock).toHaveBeenCalledWith(expect.any(File), "peer-a"));

    rerender(<Harness peerId="peer-b" />);
    fireEvent.change(input, { target: { files: [new File(["b"], "b.txt", { type: "text/plain" })] } });
    await waitFor(() => expect(uploadMock).toHaveBeenLastCalledWith(expect.any(File), "peer-b"));
  });

  it("stays uploading until every overlapping batch settles", async () => {
    const resolvers: Record<string, () => void> = {};
    uploadMock.mockImplementation((file: File) => new Promise((resolve) => {
      resolvers[file.name] = () => resolve({ path: "/u/" + file.name, name: file.name, size: 1, mime: "text/plain" });
    }));
    const { container } = render(<BusyHarness />);
    const input = container.querySelector("input")!;
    const state = () => screen.getByTestId("state").textContent;

    fireEvent.change(input, { target: { files: [new File(["a"], "a.txt")] } });
    fireEvent.change(input, { target: { files: [new File(["b"], "b.txt")] } });
    await waitFor(() => expect(uploadMock).toHaveBeenCalledTimes(2));
    expect(state()).toBe("busy:");

    await act(async () => resolvers["a.txt"]());
    expect(state()).toBe("busy:a.txt");

    await act(async () => resolvers["b.txt"]());
    expect(state()).toBe("idle:a.txt,b.txt");
  });

  it("does not stay busy for a previous holder's in-flight upload", async () => {
    let resolveOld!: () => void;
    uploadMock.mockImplementation(() => new Promise((resolve) => {
      resolveOld = () => resolve({ path: "/old/a.txt", name: "a.txt", size: 1, mime: "text/plain" });
    }));
    const { container, rerender } = render(<BusyHarness peerId="peer-a" />);
    fireEvent.change(container.querySelector("input")!, { target: { files: [new File(["a"], "a.txt")] } });
    await waitFor(() => expect(screen.getByTestId("state").textContent).toBe("busy:"));

    rerender(<BusyHarness peerId="peer-b" />);
    expect(screen.getByTestId("state").textContent).toBe("idle:");

    await act(async () => resolveOld());
    expect(screen.getByTestId("state").textContent).toBe("idle:");
  });

  it("drops a previous conversation's upload, even after switching back", async () => {
    let resolveOld!: () => void;
    uploadMock.mockImplementation(() => new Promise((resolve) => {
      resolveOld = () => resolve({ path: "/u/a.txt", name: "a.txt", size: 1, mime: "text/plain" });
    }));
    const { container, rerender } = render(<BusyHarness scope="room-1" />);
    fireEvent.change(container.querySelector("input")!, { target: { files: [new File(["a"], "a.txt")] } });
    await waitFor(() => expect(screen.getByTestId("state").textContent).toBe("busy:"));

    rerender(<BusyHarness scope="room-2" />);
    expect(screen.getByTestId("state").textContent).toBe("idle:");
    rerender(<BusyHarness scope="room-1" />);

    await act(async () => resolveOld());
    expect(screen.getByTestId("state").textContent).toBe("idle:");
  });
});

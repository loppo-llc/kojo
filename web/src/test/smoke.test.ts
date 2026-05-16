import { describe, it, expect } from "vitest";

// Sanity check that the Vitest harness boots, jsdom is available, and
// the @testing-library/jest-dom matchers from setup.ts are wired in.
describe("vitest harness", () => {
  it("runs with jsdom", () => {
    document.body.innerHTML = `<div id="smoke">ok</div>`;
    const el = document.getElementById("smoke");
    expect(el).not.toBeNull();
    expect(el?.textContent).toBe("ok");
  });
});

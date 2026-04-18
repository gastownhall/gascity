import { describe, expect, it, vi } from "vitest";

import { showToast } from "./ui";

describe("showToast", () => {
  it("shows visible toast notifications", () => {
    document.body.innerHTML = `<div id="toast-container"></div>`;
    const raf = vi.spyOn(window, "requestAnimationFrame").mockImplementation((cb: FrameRequestCallback) => {
      cb(0);
      return 1;
    });
    const timeout = vi.spyOn(window, "setTimeout").mockImplementation(() => 1 as unknown as number);

    showToast("success", "Sent", "hello");

    const toast = document.querySelector(".toast");
    expect(toast).not.toBeNull();
    expect(toast?.classList.contains("toast-success")).toBe(true);
    expect(toast?.classList.contains("show")).toBe(true);

    raf.mockRestore();
    timeout.mockRestore();
  });
});

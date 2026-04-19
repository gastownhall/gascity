import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

describe("dashboard logger", () => {
  beforeEach(() => {
    vi.resetModules();
    document.body.innerHTML = "";
    window.history.pushState({}, "", "/dashboard?city=mc-city");
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(null, { status: 204 })));
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    window.history.pushState({}, "", "/dashboard");
  });

  it("posts structured client errors to the dashboard server", async () => {
    const { installDashboardLogging, logError } = await import("./logger");

    installDashboardLogging();
    logError("mail", "Compose failed", { reason: "missing recipient" });

    expect(fetch).toHaveBeenCalledWith("/__client-log", expect.objectContaining({
      headers: { "Content-Type": "application/json" },
      keepalive: true,
      method: "POST",
    }));

    const [, request] = vi.mocked(fetch).mock.calls[0] ?? [];
    const parsed = JSON.parse(String(request?.body));
    expect(parsed.scope).toBe("mail");
    expect(parsed.level).toBe("error");
    expect(parsed.city).toBe("mc-city");
    expect(parsed.details.reason).toBe("missing recipient");
  });
});

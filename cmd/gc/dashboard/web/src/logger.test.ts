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

  it("does not emit debug logs by default", async () => {
    const { installDashboardLogging, logDebug } = await import("./logger");

    installDashboardLogging();
    logDebug("api", "Request start", { url: "http://127.0.0.1:8372/v0/cities" });

    expect(fetch).not.toHaveBeenCalled();
  });

  it("does not emit info logs by default", async () => {
    const info = vi.spyOn(console, "info").mockImplementation(() => undefined);
    const { installDashboardLogging, logInfo } = await import("./logger");

    installDashboardLogging();
    logInfo("dashboard", "Boot complete", { city: "mc-city" });

    expect(info).not.toHaveBeenCalled();
    expect(fetch).not.toHaveBeenCalled();
  });

  it("emits debug logs when explicitly enabled", async () => {
    window.history.pushState({}, "", "/dashboard?city=mc-city&debug=1");
    const { installDashboardLogging, logDebug } = await import("./logger");

    installDashboardLogging();
    logDebug("api", "Request start", { url: "http://127.0.0.1:8372/v0/cities" });

    expect(fetch).toHaveBeenCalledWith("/__client-log", expect.objectContaining({
      keepalive: true,
      method: "POST",
    }));
  });

  it("emits info logs when explicitly enabled", async () => {
    window.history.pushState({}, "", "/dashboard?city=mc-city&debug=1");
    const info = vi.spyOn(console, "info").mockImplementation(() => undefined);
    const { installDashboardLogging, logInfo } = await import("./logger");

    installDashboardLogging();
    logInfo("dashboard", "Boot complete", { city: "mc-city" });

    expect(info).toHaveBeenCalledWith("[dashboard][dashboard] Boot complete", { city: "mc-city" });
    expect(fetch).toHaveBeenCalledWith("/__client-log", expect.objectContaining({
      keepalive: true,
      method: "POST",
    }));
  });

  it("still emits warnings by default", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    const { installDashboardLogging, logWarn } = await import("./logger");

    installDashboardLogging();
    logWarn("status", "City status dependency timed out", { city: "mc-city" });

    expect(warn).toHaveBeenCalledWith("[dashboard][status] City status dependency timed out", { city: "mc-city" });
    expect(fetch).toHaveBeenCalledWith("/__client-log", expect.objectContaining({
      keepalive: true,
      method: "POST",
    }));
  });
});

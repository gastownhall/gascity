import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const apiGet = vi.fn();

vi.mock("../api", () => ({
  api: { GET: apiGet },
  cityScope: () => (new URLSearchParams(window.location.search).get("city") ?? "").trim(),
}));

function installStatusDOM(): void {
  document.body.innerHTML = `
    <div class="scope-banner" id="scope-banner">
      <div class="scope-info">
        <span class="scope-title">Selected Scope</span>
        <span id="scope-badge" class="badge badge-muted">Loading</span>
      </div>
      <div id="scope-status"></div>
    </div>
    <div id="status-banner"></div>
  `;
}

function deferred<T>(): {
  promise: Promise<T>;
  resolve: (value: T) => void;
} {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

function ok(data: unknown): { data: unknown } {
  return { data };
}

function flushPromises(): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, 0);
  });
}

function scopeStats(): Record<string, string> {
  const stats: Record<string, string> = {};
  document.querySelectorAll(".scope-stat").forEach((stat) => {
    const label = stat.querySelector(".scope-stat-label")?.textContent ?? "";
    const value = stat.querySelector(".scope-stat-value")?.textContent ?? "";
    stats[label] = value;
  });
  return stats;
}

describe("status panel scope rendering", () => {
  beforeEach(() => {
    vi.resetModules();
    apiGet.mockReset();
    installStatusDOM();
    window.history.pushState({}, "", "/dashboard");
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("does not let a stale city status response overwrite supervisor scope", async () => {
    window.history.pushState({}, "", "/dashboard?city=alpha");
    document.getElementById("scope-badge")!.textContent = "Supervisor";
    document.getElementById("scope-status")!.textContent = "Scope Fleet City Select one";

    const cityStatus = deferred<{ data: unknown }>();
    apiGet.mockImplementation((path: string) => {
      if (path.includes("/status")) return cityStatus.promise;
      if (path.includes("/sessions")) return Promise.resolve(ok({ items: [] }));
      if (path.includes("/beads")) return Promise.resolve(ok({ items: [] }));
      if (path.includes("/convoys")) return Promise.resolve(ok({ items: [] }));
      return Promise.resolve(ok({}));
    });

    const { renderStatus } = await import("./status");
    const render = renderStatus();
    window.history.pushState({}, "", "/dashboard");
    cityStatus.resolve(ok({
      agents: { running: 2 },
      mail: { unread: 0 },
      work: { in_progress: 1, open: 28 },
    }));
    await render;

    expect(document.getElementById("scope-badge")?.textContent).toBe("Supervisor");
    expect(document.getElementById("scope-status")?.textContent).toContain("Fleet");
  });

  it("does not let a stale city session response overwrite supervisor scope", async () => {
    window.history.pushState({}, "", "/dashboard?city=alpha");
    document.getElementById("scope-badge")!.textContent = "Supervisor";
    document.getElementById("scope-status")!.textContent = "Scope Fleet City Select one";

    const sessions = deferred<{ data: unknown }>();
    apiGet.mockImplementation((path: string) => {
      if (path.includes("/status")) {
        return Promise.resolve(ok({
          agents: { running: 2 },
          mail: { unread: 0 },
          work: { in_progress: 1, open: 28 },
        }));
      }
      if (path.includes("/sessions")) return sessions.promise;
      if (path.includes("/beads")) return Promise.resolve(ok({ items: [] }));
      if (path.includes("/convoys")) return Promise.resolve(ok({ items: [] }));
      return Promise.resolve(ok({}));
    });

    const { renderStatus } = await import("./status");
    const render = renderStatus();
    window.history.pushState({}, "", "/dashboard");
    sessions.resolve(ok({
      items: [{
        attached: false,
        configured_named_session: true,
        last_active: new Date().toISOString(),
        running: true,
        template: "control-dispatcher",
      }],
    }));
    await render;

    expect(document.getElementById("scope-badge")?.textContent).toBe("Supervisor");
    expect(document.getElementById("scope-status")?.textContent).toContain("Fleet");
  });

  it("renders city scope as soon as sessions arrive even when status is still pending", async () => {
    window.history.pushState({}, "", "/dashboard?city=alpha");
    const now = new Date().toISOString();
    const cityStatus = deferred<{ data: unknown }>();
    apiGet.mockImplementation((path: string) => {
      if (path.includes("/status")) return cityStatus.promise;
      if (path.includes("/sessions")) {
        return Promise.resolve(ok({
          items: [{
            attached: false,
            configured_named_session: true,
            last_active: now,
            running: true,
            template: "control-dispatcher",
          }],
        }));
      }
      if (path.includes("/beads")) return Promise.resolve(ok({ items: [] }));
      if (path.includes("/convoys")) return Promise.resolve(ok({ items: [] }));
      return Promise.resolve(ok({}));
    });

    const { renderStatus } = await import("./status");
    const render = renderStatus();
    await flushPromises();

    expect(document.getElementById("scope-badge")?.textContent).toBe("City");
    expect(scopeStats()).toMatchObject({
      City: "alpha",
      Session: "control-dispatcher",
      Terminal: "Detached",
      State: "Running",
    });

    cityStatus.resolve(ok({
      agents: { running: 2 },
      mail: { unread: 0 },
      work: { in_progress: 1, open: 28 },
    }));
    await render;
  });

  it("finishes city status render with partial data when the aggregate status request times out", async () => {
    vi.useFakeTimers();
    window.history.pushState({}, "", "/dashboard?city=alpha");
    const now = new Date().toISOString();
    apiGet.mockImplementation((path: string) => {
      if (path.includes("/status")) return new Promise(() => {});
      if (path.includes("/sessions")) {
        return Promise.resolve(ok({
          items: [{
            attached: false,
            configured_named_session: true,
            last_active: now,
            running: true,
            template: "control-dispatcher",
          }],
        }));
      }
      if (path.includes("/beads")) {
        return Promise.resolve(ok({
          items: [{
            assignee: "agent-one",
            id: "bd-1",
            priority: 1,
            status: "open",
          }],
        }));
      }
      if (path.includes("/convoys")) return Promise.resolve(ok({ items: [] }));
      return Promise.resolve(ok({}));
    });

    const { renderStatus } = await import("./status");
    const render = renderStatus();
    await vi.advanceTimersByTimeAsync(1_000);
    await render;

    expect(document.getElementById("scope-badge")?.textContent).toBe("City");
    expect(scopeStats().Terminal).toBe("Detached");
    expect(document.getElementById("status-banner")?.textContent).toContain("Status API slow");
    expect(document.getElementById("status-banner")?.textContent).toContain("1");
  });

  it("renders city scope from session data instead of leaving the placeholder idle", async () => {
    window.history.pushState({}, "", "/dashboard?city=alpha");
    const now = new Date().toISOString();
    apiGet.mockImplementation((path: string) => {
      if (path.includes("/status")) {
        return Promise.resolve(ok({
          agents: { running: 2 },
          mail: { unread: 0 },
          work: { in_progress: 1, open: 28 },
        }));
      }
      if (path.includes("/sessions")) {
        return Promise.resolve(ok({
          items: [{
            attached: false,
            configured_named_session: true,
            last_active: now,
            running: true,
            template: "control-dispatcher",
          }],
        }));
      }
      if (path.includes("/beads")) return Promise.resolve(ok({ items: [] }));
      if (path.includes("/convoys")) return Promise.resolve(ok({ items: [] }));
      return Promise.resolve(ok({}));
    });

    const { renderStatus } = await import("./status");
    await renderStatus();

    expect(document.getElementById("scope-badge")?.textContent).toBe("City");
    expect(scopeStats()).toMatchObject({
      City: "alpha",
      Session: "control-dispatcher",
      Terminal: "Detached",
      State: "Running",
    });
  });

  it("keeps terminal attachment separate from the city scope badge", async () => {
    window.history.pushState({}, "", "/dashboard?city=alpha");
    const now = new Date().toISOString();
    apiGet.mockImplementation((path: string) => {
      if (path.includes("/status")) {
        return Promise.resolve(ok({
          agents: { running: 1 },
          mail: { unread: 0 },
          work: { in_progress: 0, open: 0 },
        }));
      }
      if (path.includes("/sessions")) {
        return Promise.resolve(ok({
          items: [{
            attached: true,
            configured_named_session: true,
            last_active: now,
            running: true,
            template: "control-dispatcher",
          }],
        }));
      }
      if (path.includes("/beads")) return Promise.resolve(ok({ items: [] }));
      if (path.includes("/convoys")) return Promise.resolve(ok({ items: [] }));
      return Promise.resolve(ok({}));
    });

    const { renderStatus } = await import("./status");
    await renderStatus();

    expect(document.getElementById("scope-badge")?.textContent).toBe("City");
    expect(scopeStats()).toMatchObject({
      City: "alpha",
      Session: "control-dispatcher",
      Terminal: "Attached",
      State: "Running",
    });
  });
});

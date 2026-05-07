import { beforeEach, describe, expect, it, vi } from "vitest";

const streamEvents = vi.fn();
const streamSupervisorEvents = vi.fn();

vi.mock("./generated/client.gen", () => ({
  client: {},
}));

vi.mock("./generated/sdk.gen", () => ({
  streamEvents,
  streamSession: vi.fn(),
  streamSupervisorEvents,
}));

vi.mock("./ui", () => ({
  reportUIError: vi.fn(),
}));

async function* quietStream(): AsyncGenerator<never> {
  await new Promise(() => undefined);
}

describe("dashboard SSE status", () => {
  beforeEach(() => {
    vi.resetModules();
    streamEvents.mockReset();
    streamSupervisorEvents.mockReset();
  });

  it("marks a quiet city event stream live after the connection opens", async () => {
    streamEvents.mockResolvedValue({ stream: quietStream() });
    const statuses: string[] = [];

    const { connectCityEvents } = await import("./sse");
    const handle = connectCityEvents("mc-city", () => undefined, {
      onStatus: (status) => statuses.push(status),
    });
    await Promise.resolve();
    await Promise.resolve();

    handle.close();
    expect(statuses).toContain("connecting");
    expect(statuses).toContain("live");
  });

  it("marks a quiet supervisor event stream live after the connection opens", async () => {
    streamSupervisorEvents.mockResolvedValue({ stream: quietStream() });
    const statuses: string[] = [];

    const { connectEvents } = await import("./sse");
    const handle = connectEvents(() => undefined, {
      onStatus: (status) => statuses.push(status),
    });
    await Promise.resolve();
    await Promise.resolve();

    handle.close();
    expect(statuses).toContain("connecting");
    expect(statuses).toContain("live");
  });
});

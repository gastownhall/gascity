import { describe, expect, it, vi } from "vitest";

import { createRefreshScheduler } from "./refresh_scheduler";

describe("refresh scheduler", () => {
  it("coalesces bursts into one refresh per debounce interval", async () => {
    vi.useFakeTimers();
    const run = vi.fn(() => Promise.resolve());
    const scheduler = createRefreshScheduler({
      delayMs: 1_000,
      isPaused: () => false,
      onError: () => undefined,
      run,
    });

    scheduler.schedule();
    scheduler.schedule();
    scheduler.schedule();
    await vi.advanceTimersByTimeAsync(999);
    expect(run).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(1);
    expect(run).toHaveBeenCalledTimes(1);

    vi.useRealTimers();
  });

  it("runs one follow-up refresh when events arrive during an active refresh", async () => {
    vi.useFakeTimers();
    let finishFirst!: () => void;
    const run = vi
      .fn()
      .mockImplementationOnce(() => new Promise<void>((resolve) => {
        finishFirst = resolve;
      }))
      .mockResolvedValue(undefined);
    const scheduler = createRefreshScheduler({
      delayMs: 1_000,
      isPaused: () => false,
      onError: () => undefined,
      run,
    });

    scheduler.schedule();
    await vi.advanceTimersByTimeAsync(1_000);
    expect(run).toHaveBeenCalledTimes(1);

    scheduler.schedule();
    scheduler.schedule();
    finishFirst();
    await Promise.resolve();
    await vi.advanceTimersByTimeAsync(999);
    expect(run).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(1);
    expect(run).toHaveBeenCalledTimes(2);

    vi.useRealTimers();
  });

  it("respects a minimum interval between refresh starts during event storms", async () => {
    vi.useFakeTimers();
    let finishFirst!: () => void;
    const run = vi
      .fn()
      .mockImplementationOnce(() => new Promise<void>((resolve) => {
        finishFirst = resolve;
      }))
      .mockResolvedValue(undefined);
    const scheduler = createRefreshScheduler({
      delayMs: 1_000,
      isPaused: () => false,
      minIntervalMs: 10_000,
      onError: () => undefined,
      run,
    });

    scheduler.schedule();
    await vi.advanceTimersByTimeAsync(1_000);
    expect(run).toHaveBeenCalledTimes(1);

    scheduler.schedule();
    finishFirst();
    await Promise.resolve();

    await vi.advanceTimersByTimeAsync(1_000);
    expect(run).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(8_999);
    expect(run).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(1);
    expect(run).toHaveBeenCalledTimes(2);

    vi.useRealTimers();
  });
});

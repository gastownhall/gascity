export interface RefreshScheduler {
  schedule(): void;
}

interface RefreshSchedulerOptions {
  delayMs: number;
  isPaused: () => boolean;
  minIntervalMs?: number;
  onError: (error: unknown) => void;
  run: () => Promise<void>;
}

export function createRefreshScheduler(options: RefreshSchedulerOptions): RefreshScheduler {
  let timer: ReturnType<typeof setTimeout> | null = null;
  let inFlight = false;
  let lastStartedAt = 0;
  let requestedDuringFlight = false;

  async function flush(): Promise<void> {
    timer = null;
    if (options.isPaused()) return;
    inFlight = true;
    lastStartedAt = Date.now();
    try {
      await options.run();
    } catch (error) {
      options.onError(error);
    } finally {
      inFlight = false;
    }
    if (!requestedDuringFlight || options.isPaused()) {
      requestedDuringFlight = false;
      return;
    }
    requestedDuringFlight = false;
    schedule();
  }

  function schedule(): void {
    if (timer !== null) return;
    if (inFlight) {
      requestedDuringFlight = true;
      return;
    }
    const minIntervalMs = options.minIntervalMs ?? 0;
    const elapsedSinceStart = lastStartedAt > 0 ? Date.now() - lastStartedAt : Number.POSITIVE_INFINITY;
    const intervalDelayMs = minIntervalMs > 0 ? Math.max(0, minIntervalMs - elapsedSinceStart) : 0;
    timer = setTimeout(() => {
      void flush();
    }, Math.max(options.delayMs, intervalDelayMs));
  }

  return { schedule };
}

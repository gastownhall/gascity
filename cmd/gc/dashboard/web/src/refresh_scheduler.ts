export interface RefreshScheduler {
  flushNow(): Promise<void>;
  schedule(): void;
}

interface RefreshSchedulerOptions {
  delayMs: number;
  isPaused: () => boolean;
  onError: (error: unknown) => void;
  run: () => Promise<void>;
}

export function createRefreshScheduler(options: RefreshSchedulerOptions): RefreshScheduler {
  let timer: ReturnType<typeof setTimeout> | null = null;
  let inFlight = false;
  let requestedDuringFlight = false;

  async function flush(): Promise<void> {
    timer = null;
    if (options.isPaused()) return;
    inFlight = true;
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
    timer = setTimeout(() => {
      void flush();
    }, options.delayMs);
  }

  async function flushNow(): Promise<void> {
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    }
    await flush();
  }

  return { flushNow, schedule };
}

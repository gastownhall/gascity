import { SupervisorApiError } from './client';

// A core read is the one supervisor fetch whose failure blanks a whole view
// (the runs list's active-bead read, the run-detail's workflow snapshot). The
// box runs at high load under a slung-pipeline burst, so a single core read
// occasionally crosses an interactive budget. Retry once on a transient
// timeout/5xx before giving up; a sustained failure still propagates after the
// retry is spent (upstream gascity-dashboard#88).
export const CORE_FETCH_RETRIES = 1;
export const CORE_FETCH_RETRY_BACKOFF_MS = 250;

// Run a core read with one bounded retry on a transient failure. The common
// (fast) path never reaches the retry, so first-paint stays prompt; the retry
// only adds latency when a burst already made the first attempt fail. Optional
// enrichment reads keep their own degrade handling and are NOT routed here.
export async function fetchCoreRead<T>(read: () => Promise<T>): Promise<T> {
  let lastError: unknown;
  for (let attempt = 0; attempt <= CORE_FETCH_RETRIES; attempt += 1) {
    try {
      return await read();
    } catch (err) {
      lastError = err;
      if (attempt === CORE_FETCH_RETRIES || !isTransientSupervisorError(err)) throw err;
      await delay(CORE_FETCH_RETRY_BACKOFF_MS);
    }
  }
  throw lastError;
}

// A timeout (status undefined, "timed out after Nms") or a 5xx is transient: the
// supervisor is briefly overloaded, not reporting a stable failure. A 4xx is the
// caller's fault and must not be retried.
export function isTransientSupervisorError(err: unknown): boolean {
  if (!(err instanceof SupervisorApiError)) return false;
  if (err.status === undefined) return /timed out after \d+ms/.test(err.message);
  return err.status >= 500;
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

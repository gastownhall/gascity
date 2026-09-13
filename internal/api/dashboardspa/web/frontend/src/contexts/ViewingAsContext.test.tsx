import { act, cleanup, render } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import { useEffect } from 'react';
import { reportClientError } from '../lib/clientErrorReporting';
import { listSupervisorMail } from '../supervisor/mailReads';
import { OperatorConfigProvider, type OperatorConfig } from './OperatorConfigContext';
import { ViewingAsProvider, useViewingAs, getSessionsRetryDelay } from './ViewingAsContext';

// gascity-dashboard-5gg: bounded retry for supervisor sessions in the alias
// prefetch. Tests cover:
//
//   1. Initial fetch failure flips sessionsUnavailable=true.
//   2. Three retries are scheduled at 30s, 90s, 270s after each prior
//      failure; no retry fires earlier.
//   3. A successful retry flips sessionsUnavailable back to false.
//   4. After the third failed retry, no further attempts are made
//      (sticky failure matches current behaviour).
//   5. Unmounting between retries cancels the pending timer (no late
//      state updates, no leaked timeouts).
//
// We mock supervisor session reads and supervisor mail reads so both
// sources return shaped promises under our control. listMail is not retried
// — only sessions has the bounded retry.

const mockListSupervisorSessions = vi.hoisted(() => vi.fn());
const mockListSupervisorMail = vi.hoisted(() => vi.fn());

vi.mock('../supervisor/sessionReads', () => ({
  listSupervisorSessions: mockListSupervisorSessions,
}));

vi.mock('../supervisor/mailReads', () => ({
  listSupervisorMail: mockListSupervisorMail,
}));

vi.mock('../lib/clientErrorReporting', () => ({
  reportClientError: vi.fn(),
}));

const mockListMail = listSupervisorMail as Mock;
const mockReportClientError = reportClientError as Mock;

interface Probe {
  sessionsUnavailable: boolean;
  aliasesLoading: boolean;
}

function Harness({ onState }: { onState: (p: Probe) => void }) {
  const { sessionsUnavailable, aliasesLoading, loadAliases } = useViewingAs();
  useEffect(() => {
    loadAliases();
  }, [loadAliases]);
  useEffect(() => {
    onState({ sessionsUnavailable, aliasesLoading });
  }, [sessionsUnavailable, aliasesLoading, onState]);
  return null;
}

async function flushPromises() {
  // Vitest fake timers don't drain microtasks; wrap a real-clock yield
  // so awaited `.then()` callbacks on resolved promises run.
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
  });
}

beforeEach(() => {
  vi.useFakeTimers();
  mockListSupervisorSessions.mockReset();
  mockListMail.mockReset();
  mockReportClientError.mockReset();
  // Mail fetch is uninteresting for these tests; resolve to empty.
  mockListMail.mockResolvedValue({ items: [] });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe('ViewingAsProvider — bounded sessions retry', () => {
  it('flips sessionsUnavailable=true on initial supervisor sessions failure', async () => {
    mockListSupervisorSessions.mockRejectedValue(new Error('upstream 504'));
    const states: Probe[] = [];
    render(
      <ViewingAsProvider>
        <Harness onState={(p) => states.push(p)} />
      </ViewingAsProvider>,
    );
    await flushPromises();
    expect(states.at(-1)?.sessionsUnavailable).toBe(true);
    expect(mockReportClientError).toHaveBeenCalledWith({
      component: 'ViewingAsContext',
      operation: 'loadAliases.sessions',
      message: 'upstream 504',
    });
  });

  it('does not retry before the 30s mark', async () => {
    mockListSupervisorSessions.mockRejectedValue(new Error('upstream 504'));
    render(
      <ViewingAsProvider>
        <Harness onState={() => {}} />
      </ViewingAsProvider>,
    );
    await flushPromises();
    expect(mockListSupervisorSessions).toHaveBeenCalledTimes(1);
    // Advance just under the first retry window.
    await act(async () => {
      vi.advanceTimersByTime(29_999);
    });
    expect(mockListSupervisorSessions).toHaveBeenCalledTimes(1);
  });

  it('retries at 30s, 90s, and 270s after successive failures', async () => {
    mockListSupervisorSessions.mockRejectedValue(new Error('upstream 504'));
    render(
      <ViewingAsProvider>
        <Harness onState={() => {}} />
      </ViewingAsProvider>,
    );
    await flushPromises();
    expect(mockListSupervisorSessions).toHaveBeenCalledTimes(1);

    // First retry at 30s.
    await act(async () => {
      vi.advanceTimersByTime(30_000);
    });
    await flushPromises();
    expect(mockListSupervisorSessions).toHaveBeenCalledTimes(2);

    // Second retry at 30s + 90s.
    await act(async () => {
      vi.advanceTimersByTime(90_000);
    });
    await flushPromises();
    expect(mockListSupervisorSessions).toHaveBeenCalledTimes(3);

    // Third retry at 30s + 90s + 270s.
    await act(async () => {
      vi.advanceTimersByTime(270_000);
    });
    await flushPromises();
    expect(mockListSupervisorSessions).toHaveBeenCalledTimes(4);
  });

  it('flips sessionsUnavailable=false when a retry succeeds', async () => {
    mockListSupervisorSessions
      .mockRejectedValueOnce(new Error('upstream 504'))
      .mockResolvedValueOnce({ items: [{ alias: 'mechanic' }] });

    const states: Probe[] = [];
    render(
      <ViewingAsProvider>
        <Harness onState={(p) => states.push(p)} />
      </ViewingAsProvider>,
    );
    await flushPromises();
    expect(states.at(-1)?.sessionsUnavailable).toBe(true);

    // First retry at 30s — succeeds this time.
    await act(async () => {
      vi.advanceTimersByTime(30_000);
    });
    await flushPromises();
    expect(states.at(-1)?.sessionsUnavailable).toBe(false);
  });

  it('stops retrying after the third failed attempt (sticky failure)', async () => {
    mockListSupervisorSessions.mockRejectedValue(new Error('upstream 504'));
    render(
      <ViewingAsProvider>
        <Harness onState={() => {}} />
      </ViewingAsProvider>,
    );
    await flushPromises();
    expect(mockListSupervisorSessions).toHaveBeenCalledTimes(1);

    // Exhaust all three retries.
    await act(async () => {
      vi.advanceTimersByTime(30_000);
    });
    await flushPromises();
    await act(async () => {
      vi.advanceTimersByTime(90_000);
    });
    await flushPromises();
    await act(async () => {
      vi.advanceTimersByTime(270_000);
    });
    await flushPromises();
    expect(mockListSupervisorSessions).toHaveBeenCalledTimes(4); // initial + 3 retries

    // No further calls even after a long quiet period.
    await act(async () => {
      vi.advanceTimersByTime(10 * 60_000);
    });
    await flushPromises();
    expect(mockListSupervisorSessions).toHaveBeenCalledTimes(4);
  });

  it('cancels pending retry on unmount (no late supervisor sessions call)', async () => {
    mockListSupervisorSessions.mockRejectedValue(new Error('upstream 504'));
    const { unmount } = render(
      <ViewingAsProvider>
        <Harness onState={() => {}} />
      </ViewingAsProvider>,
    );
    await flushPromises();
    expect(mockListSupervisorSessions).toHaveBeenCalledTimes(1);

    // Unmount before the 30s retry window elapses.
    unmount();

    await act(async () => {
      vi.advanceTimersByTime(60_000);
    });
    await flushPromises();
    expect(mockListSupervisorSessions).toHaveBeenCalledTimes(1);
  });
});

// gascity-dashboard-7s7: explicit coverage of the two acceptance criteria
// the bead enumerates but the retry-focused suite above does not test
// directly — first-shot sessions success leaves the flag false, and mail
// failure is independent of the sessions-side flag.
describe('ViewingAsProvider — loadAliases initial flag transitions', () => {
  it('leaves sessionsUnavailable=false when initial supervisor sessions succeeds', async () => {
    mockListSupervisorSessions.mockResolvedValue({ items: [{ alias: 'mechanic' }] });
    const states: Probe[] = [];
    render(
      <ViewingAsProvider>
        <Harness onState={(p) => states.push(p)} />
      </ViewingAsProvider>,
    );
    await flushPromises();

    // Flag stays false; success path never sets it.
    expect(states.at(-1)?.sessionsUnavailable).toBe(false);

    // No retry scheduled on success: advancing well past the first retry
    // delay must not trigger a second call.
    await act(async () => {
      vi.advanceTimersByTime(60_000);
    });
    await flushPromises();
    expect(mockListSupervisorSessions).toHaveBeenCalledTimes(1);
  });

  it('leaves sessionsUnavailable=false when mail fetch fails but sessions succeeds', async () => {
    // Mail-side failure must NOT flip the sessions-side flag — the two
    // sources settle independently, and the flag is sessions-scoped.
    mockListSupervisorSessions.mockResolvedValue({ items: [{ alias: 'mechanic' }] });
    mockListMail.mockRejectedValue(new Error('mail corpus 500'));

    const states: Probe[] = [];
    render(
      <ViewingAsProvider>
        <Harness onState={(p) => states.push(p)} />
      </ViewingAsProvider>,
    );
    await flushPromises();

    expect(states.at(-1)?.sessionsUnavailable).toBe(false);
    expect(mockReportClientError).toHaveBeenCalledWith({
      component: 'ViewingAsContext',
      operation: 'loadAliases.mail',
      message: 'mail corpus 500',
    });
    // Mail failure must not schedule a sessions retry either.
    await act(async () => {
      vi.advanceTimersByTime(60_000);
    });
    await flushPromises();
    expect(mockListSupervisorSessions).toHaveBeenCalledTimes(1);
  });
});

// gascity-dashboard-7ky: the sessions retry table is read by index, and
// under `noUncheckedIndexedAccess` the read is typed `number | undefined`.
// If the caller's bounds check ever weakens, `setTimeout(fn, undefined)`
// would fire at 0 ms — a busy-loop retry storm. `getSessionsRetryDelay`
// is the safe accessor that returns `null` for any out-of-bounds index
// instead of leaking `undefined` to a timer.
describe('getSessionsRetryDelay — out-of-bounds guard', () => {
  // Phase-4 H1: getSessionsRetryDelay returns null silently for ALL
  // invalid inputs (negative / non-integer / NaN / out-of-bounds) —
  // frontend production code runs under `no-console: error`. The
  // silent-null contract is what the caller relies on; surfacing
  // a warning is the caller's choice.

  it('returns the scheduled delay for in-range indices', () => {
    expect(getSessionsRetryDelay(0)).toBe(30_000);
    expect(getSessionsRetryDelay(1)).toBe(90_000);
    expect(getSessionsRetryDelay(2)).toBe(270_000);
  });

  it('returns null (never undefined, never a fallback number) for an index just past the end', () => {
    const result = getSessionsRetryDelay(3);
    expect(result).toBeNull();
    // Critically: not undefined and not a number. A `setTimeout` call
    // with this value would no-op via the caller's `null` check rather
    // than fire at 0 ms.
    expect(result).not.toBe(undefined);
    expect(typeof result).not.toBe('number');
  });

  it('returns null for a large out-of-bounds index', () => {
    expect(getSessionsRetryDelay(99)).toBeNull();
  });

  it('returns null for a negative index', () => {
    expect(getSessionsRetryDelay(-1)).toBeNull();
  });

  it('returns null for a non-integer index', () => {
    expect(getSessionsRetryDelay(1.5)).toBeNull();
  });

  it('returns null for NaN', () => {
    expect(getSessionsRetryDelay(Number.NaN)).toBeNull();
  });
});

describe('ViewingAsProvider — operator identity from runtime config (gascity-dashboard-bhvn)', () => {
  const FALLBACK: OperatorConfig = {
    operatorAlias: 'operator',
    operatorWireAlias: 'human',
    decisionLabel: 'needs/operator',
  };
  const RESOLVED: OperatorConfig = {
    operatorAlias: 'stephanie',
    operatorWireAlias: 'human',
    decisionLabel: 'needs/stephanie',
  };

  function IdentityProbe({
    onState,
  }: {
    onState: (s: { alias: string; isOperator: boolean }) => void;
  }) {
    const { viewingAs } = useViewingAs();
    useEffect(() => {
      onState({ alias: viewingAs.alias, isOperator: viewingAs.isOperator });
    }, [viewingAs.alias, viewingAs.isOperator, onState]);
    return null;
  }

  it('re-syncs the viewing alias when /config flips OPERATOR from the fallback to the real alias', async () => {
    sessionStorage.clear();
    const states: Array<{ alias: string; isOperator: boolean }> = [];
    const tree = (operator: OperatorConfig) => (
      <OperatorConfigProvider operator={operator}>
        <ViewingAsProvider>
          <IdentityProbe onState={(s) => states.push(s)} />
        </ViewingAsProvider>
      </OperatorConfigProvider>
    );

    const { rerender } = render(tree(FALLBACK));
    await flushPromises();
    // Pre-config: alias tracks the fallback operator, so the operator is "self".
    expect(states.at(-1)).toEqual({ alias: 'operator', isOperator: true });

    // /config resolves: OPERATOR flips to the real alias. Without the re-sync,
    // alias would latch 'operator' and isOperator would read false.
    rerender(tree(RESOLVED));
    await flushPromises();
    expect(states.at(-1)).toEqual({ alias: 'stephanie', isOperator: true });
  });

  it('does not yank back an explicit impersonation chosen before /config resolves', async () => {
    sessionStorage.clear();
    sessionStorage.setItem('gascity.dashboard.viewingAs', 'mechanic');
    const states: Array<{ alias: string; isOperator: boolean }> = [];
    const tree = (operator: OperatorConfig) => (
      <OperatorConfigProvider operator={operator}>
        <ViewingAsProvider>
          <IdentityProbe onState={(s) => states.push(s)} />
        </ViewingAsProvider>
      </OperatorConfigProvider>
    );

    const { rerender } = render(tree(FALLBACK));
    await flushPromises();
    expect(states.at(-1)).toEqual({ alias: 'mechanic', isOperator: false });

    rerender(tree(RESOLVED));
    await flushPromises();
    // A stored impersonation survives the OPERATOR transition unchanged.
    expect(states.at(-1)).toEqual({ alias: 'mechanic', isOperator: false });
  });
});

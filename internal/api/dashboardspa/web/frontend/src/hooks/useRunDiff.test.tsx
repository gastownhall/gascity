import { act, cleanup, renderHook, waitFor } from '@testing-library/react';
import type { RunDiffResponse } from 'gas-city-dashboard-shared';
import { afterEach, describe, expect, it, vi, type Mock } from 'vitest';
import { invalidate } from '../api/cache';
import { api } from '../api/client';
import { reportClientError } from '../lib/clientErrorReporting';
import { useRunDiff } from './useRunDiff';

vi.mock('../api/client', () => ({
  api: {
    runDiff: vi.fn(),
  },
}));

vi.mock('../lib/clientErrorReporting', () => ({
  reportClientError: vi.fn(() => Promise.resolve({ status: 'reported' })),
}));

const mockRunDiff = api.runDiff as Mock;
const mockReportClientError = reportClientError as Mock;

afterEach(() => {
  cleanup();
  invalidate('');
  vi.clearAllMocks();
});

describe('useRunDiff', () => {
  it('does not fetch or report when no run id is available', async () => {
    const { result } = renderHook(() => useRunDiff(undefined, knownPath()));

    await waitFor(() => expect(result.current.kind).toBe('idle'));

    expect(mockRunDiff).not.toHaveBeenCalled();
    expect(mockReportClientError).not.toHaveBeenCalled();
  });

  it('does not fetch before the supervisor detail resolves an execution path', async () => {
    const { result } = renderHook(() => useRunDiff('wf-1', undefined));

    await waitFor(() => expect(result.current.kind).toBe('idle'));

    expect(mockRunDiff).not.toHaveBeenCalled();
    expect(mockReportClientError).not.toHaveBeenCalled();
  });

  it('returns a real failed state and reports diff load failures', async () => {
    mockRunDiff.mockRejectedValue(new Error('git unavailable'));

    const { result } = renderHook(() => useRunDiff('wf-1', knownPath()));

    await waitFor(() =>
      expect(result.current).toMatchObject({
        kind: 'failed',
        error: 'git unavailable',
      }),
    );

    expect(mockReportClientError).toHaveBeenCalledWith({
      component: 'formula-run-detail',
      operation: 'load diff',
      message: 'wf-1: git unavailable',
    });
  });

  it('returns the loaded diff independently of the run detail resource', async () => {
    const diff = okDiff();
    mockRunDiff.mockResolvedValue(diff);

    const { result } = renderHook(() => useRunDiff('wf-1', knownPath()));

    await waitFor(() => expect(result.current.kind).toBe('ready'));

    if (result.current.kind !== 'ready') throw new Error('diff did not load');
    expect(result.current.diff).toBe(diff);
    expect(result.current.refreshState).toEqual({ kind: 'idle' });
    expect(mockRunDiff).toHaveBeenCalledWith(
      'wf-1',
      {
        executionPath: knownPath(),
      },
      {},
    );
  });

  it('sends refresh=true on an explicit manual refresh (the bypass-lane client contract)', async () => {
    mockRunDiff.mockResolvedValue(okDiff());

    const { result } = renderHook(() => useRunDiff('wf-1', knownPath()));
    await waitFor(() => expect(result.current.kind).toBe('ready'));
    if (result.current.kind !== 'ready') throw new Error('diff did not load');
    mockRunDiff.mockClear();

    await act(async () => {
      await result.current.refresh();
    });

    expect(mockRunDiff).toHaveBeenCalledWith(
      'wf-1',
      { executionPath: knownPath() },
      { refresh: true },
    );
  });

  it('omits refresh on cheapRefresh (the event-driven cheap-lane client contract)', async () => {
    mockRunDiff.mockResolvedValue(okDiff());

    const { result } = renderHook(() => useRunDiff('wf-1', knownPath()));
    await waitFor(() => expect(result.current.kind).toBe('ready'));
    if (result.current.kind !== 'ready') throw new Error('diff did not load');
    mockRunDiff.mockClear();

    await act(async () => {
      await result.current.cheapRefresh();
    });

    // cheapRefresh omits `refresh: true` — the cheap-lane client contract for a
    // future server-side diff cache. It is inert on the wire today (runQuery
    // drops the flag, no cache exists); the live burst protection is the
    // tab-gating + useGcEventRefresh coalescing, not this flag.
    expect(mockRunDiff).toHaveBeenCalledWith('wf-1', { executionPath: knownPath() }, {});
  });
});

function knownPath() {
  return { kind: 'known' as const, path: '/tmp/run' };
}

function okDiff(): RunDiffResponse {
  return {
    kind: 'ok',
    rootPath: { kind: 'known', path: '/tmp/run' },
    comparison: { kind: 'head', reason: 'no_upstream' },
    status: [],
    changedFiles: [],
    patch: '',
    truncated: false,
  };
}

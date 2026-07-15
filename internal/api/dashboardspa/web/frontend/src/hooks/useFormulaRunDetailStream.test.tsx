import { act, cleanup, renderHook, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { FormulaRunDetail } from 'gas-city-dashboard-shared';
import { getCached, invalidate } from '../api/cache';
import { formulaRunDetailCacheKey } from './useFormulaRunDetail';
import { useFormulaRunDetailStream } from './useFormulaRunDetailStream';

vi.mock('../api/cityBase', () => ({
  getActiveCity: () => 'test-city',
  cityPath: (suffix: string) => `/api/city/test-city${suffix}`,
}));

vi.mock('../lib/clientErrorReporting', () => ({
  reportClientError: vi.fn(() => Promise.resolve({ status: 'reported' })),
}));

const eventSources: FakeEventSource[] = [];

function runDetail(overrides: Partial<FormulaRunDetail> = {}): FormulaRunDetail {
  return {
    runId: 'wf-1',
    rootBeadId: 'wf-1',
    rootStoreRef: 'city:test-city',
    resolvedRootStore: 'city:test-city',
    scopeKind: 'city',
    scopeRef: 'test-city',
    title: 'Streamed run',
    formula: { kind: 'known', name: 'mol-test', source: 'metadata' },
    formulaDetail: { kind: 'available', name: 'mol-test', target: 'test-city/codex' },
    executionPath: { kind: 'unavailable', reason: 'missing_cwd_and_rig_root' },
    snapshotVersion: 1,
    snapshotEventSeq: { kind: 'known', seq: 5 },
    completeness: { kind: 'complete' },
    progress: {
      snapshotVersion: 1,
      snapshotEventSeq: { kind: 'known', seq: 5 },
      snapshotPartial: false,
      totalNodeCount: 0,
      visibleNodeCount: 0,
      edgeCount: 0,
      executionInstanceCount: 0,
      sessionLinkCount: 0,
      streamableSessionCount: 0,
      streamableSessionIds: [],
      statusCounts: {},
      allStatusCounts: {},
      terminal: false,
    },
    phase: 'intake',
    stages: [],
    nodes: [],
    edges: [],
    lanes: [],
    ...overrides,
  };
}

describe('useFormulaRunDetailStream', () => {
  beforeEach(() => {
    eventSources.length = 0;
    vi.stubGlobal('EventSource', FakeEventSource);
  });

  afterEach(() => {
    cleanup();
    invalidate('');
    vi.unstubAllGlobals();
    vi.clearAllMocks();
  });

  it('opens the stream at the BFF detail-stream url when enabled', () => {
    renderHook(() => useFormulaRunDetailStream('wf-1', true, undefined));
    expect(eventSources).toHaveLength(1);
    expect(eventSources[0]?.url).toBe('/api/city/test-city/runs/wf-1/detail/stream');
  });

  it('does not open a stream when disabled or when no run id is present', () => {
    const { rerender } = renderHook(
      ({ id, enabled }: { id: string | undefined; enabled: boolean }) =>
        useFormulaRunDetailStream(id, enabled, undefined),
      { initialProps: { id: 'wf-1' as string | undefined, enabled: false } },
    );
    expect(eventSources).toHaveLength(0);
    rerender({ id: undefined, enabled: true });
    expect(eventSources).toHaveLength(0);
  });

  it('adopts a pushed detail frame and warms the cache with zero refetch', async () => {
    const onDetail = vi.fn();
    renderHook(() => useFormulaRunDetailStream('wf-1', true, onDetail));

    const detail = runDetail({ title: 'Pushed title' });
    act(() => eventSources[0]?.open());
    act(() => eventSources[0]?.emit('detail', JSON.stringify(detail)));

    await waitFor(() => expect(onDetail).toHaveBeenCalledTimes(1));
    expect(onDetail).toHaveBeenCalledWith(
      expect.objectContaining({ title: 'Pushed title', runId: 'wf-1' }),
      expect.any(String),
    );
    // The cache is warmed so a remount paints instantly from the pushed frame.
    const cached = getCached(formulaRunDetailCacheKey('wf-1'));
    expect(cached).toMatchObject({ kind: 'loaded', detail: { title: 'Pushed title' } });
  });

  it('tags each pushed frame with the effect-scoped cache key the stream was opened for', async () => {
    // The frame must carry the (runId, scope) key the STREAM was opened for, not
    // the caller's render-time key. During an A→B navigation the callback ref
    // already points at run B while run A's EventSource is still open; without
    // this tag, a late frame from A would be stored under B's key and flash A's
    // detail as B's. Storing the frame under A's key lets the consumer drop it.
    const onDetail = vi.fn();
    renderHook(() => useFormulaRunDetailStream('wf-1', true, onDetail, 'rig', 'app'));

    act(() => eventSources[0]?.open());
    act(() => eventSources[0]?.emit('detail', JSON.stringify(runDetail({ title: 'A' }))));

    await waitFor(() => expect(onDetail).toHaveBeenCalledTimes(1));
    expect(onDetail.mock.calls[0]?.[1]).toBe(formulaRunDetailCacheKey('wf-1', 'rig', 'app'));
  });

  it('reports a stream error via connection state so the caller can fall back', async () => {
    const { result } = renderHook(() => useFormulaRunDetailStream('wf-1', true, undefined));
    expect(result.current).toBe('connecting');
    act(() => eventSources[0]?.open());
    await waitFor(() => expect(result.current).toBe('open'));
    act(() => eventSources[0]?.fail());
    await waitFor(() => expect(result.current).toBe('closed'));
  });

  it('closes the stream on unmount and on run-id change', () => {
    const { rerender, unmount } = renderHook(
      ({ id }: { id: string }) => useFormulaRunDetailStream(id, true, undefined),
      { initialProps: { id: 'wf-1' } },
    );
    const first = eventSources[0];
    expect(first?.closed).toBe(false);
    rerender({ id: 'wf-2' });
    expect(first?.closed).toBe(true);
    expect(eventSources).toHaveLength(2);
    unmount();
    expect(eventSources[1]?.closed).toBe(true);
  });
});

class FakeEventSource {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;

  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  readyState = FakeEventSource.CONNECTING;
  closed = false;
  private readonly listeners = new Map<string, Set<EventListener>>();

  constructor(readonly url: string | URL) {
    eventSources.push(this);
  }

  addEventListener(type: string, listener: EventListener): void {
    const listeners = this.listeners.get(type) ?? new Set<EventListener>();
    listeners.add(listener);
    this.listeners.set(type, listeners);
  }

  removeEventListener(type: string, listener: EventListener): void {
    this.listeners.get(type)?.delete(listener);
  }

  close(): void {
    this.readyState = FakeEventSource.CLOSED;
    this.closed = true;
  }

  open(): void {
    this.readyState = FakeEventSource.OPEN;
    this.onopen?.(new Event('open'));
  }

  fail(): void {
    this.readyState = FakeEventSource.CLOSED;
    this.onerror?.(new Event('error'));
  }

  emit(type: string, data: string): void {
    const event = new MessageEvent<string>(type, { data });
    this.listeners.get(type)?.forEach((listener) => listener(event));
    if (type === 'message') this.onmessage?.(event);
  }
}

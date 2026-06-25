import { act, cleanup, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import { reportClientError } from '../lib/clientErrorReporting';
import type { SessionTranscriptView } from '../supervisor/sessionReads';
import type * as SessionReads from '../supervisor/sessionReads';
import { useSessionStream } from './useSessionStream';

const mockFetchSupervisorSessionTranscript = vi.hoisted(() => vi.fn());

vi.mock('../supervisor/sessionReads', async (importOriginal) => {
  const actual = await importOriginal<typeof SessionReads>();
  return {
    ...actual,
    fetchSupervisorSessionTranscript: mockFetchSupervisorSessionTranscript,
  };
});

vi.mock('../lib/clientErrorReporting', () => ({
  reportClientError: vi.fn(() => Promise.resolve({ status: 'reported' })),
}));

const eventSources: FakeEventSource[] = [];
const mockReportClientError = reportClientError as Mock;

const transcript: SessionTranscriptView = {
  id: 'gc-session-1',
  template: 'mayor',
  provider: 'claude',
  format: 'conversation',
  turns: [{ role: 'assistant', text: 'initial' }],
  total_chars: 7,
  captured_at: '2026-05-27T10:00:00Z',
  truncated: false,
};

describe('useSessionStream', () => {
  beforeEach(() => {
    eventSources.length = 0;
    vi.stubGlobal('EventSource', FakeEventSource);
    mockFetchSupervisorSessionTranscript.mockReset();
    mockReportClientError.mockClear();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it('returns explicit idle state when no session is selected', () => {
    const { result } = renderHook(() => useSessionStream(null, true));

    expect(result.current).toEqual({
      status: 'idle',
      stream: { status: 'idle' },
    });
  });

  it('loads the snapshot, opens the stream, and appends turn frames', async () => {
    mockFetchSupervisorSessionTranscript.mockResolvedValue(transcript);

    const { result } = renderHook(() => useSessionStream('gc-session-1', true));

    expect(result.current).toEqual({
      status: 'loading',
      stream: { status: 'connecting' },
    });

    await flush();
    expect(result.current).toMatchObject({
      status: 'ready',
      result: transcript,
      stream: { status: 'connecting' },
    });
    expect(eventSources[0]?.url).toBe(
      '/gc-supervisor/v0/city/test-city/session/gc-session-1/stream',
    );

    act(() => eventSources[0]?.open());
    expect(result.current).toMatchObject({
      status: 'ready',
      stream: { status: 'open' },
    });

    act(() =>
      eventSources[0]?.emit(
        'message',
        JSON.stringify({
          role: 'assistant',
          text: 'streamed',
        }),
      ),
    );
    expect(result.current.status).toBe('ready');
    if (result.current.status !== 'ready') return;
    expect(result.current.result.turns).toHaveLength(2);
    expect(result.current.result.turns.at(-1)).toEqual({
      role: 'assistant',
      text: 'streamed',
    });
    expect(result.current.result.total_chars).toBe(15);
  });

  it('keeps the transcript visible while surfacing malformed stream frames', async () => {
    mockFetchSupervisorSessionTranscript.mockResolvedValue(transcript);

    const { result } = renderHook(() => useSessionStream('gc-session-1', true));
    await flush();
    act(() => eventSources[0]?.open());
    act(() => eventSources[0]?.emit('message', 'not json'));

    expect(result.current.status).toBe('ready');
    if (result.current.status !== 'ready') return;
    expect(result.current.result).toEqual(transcript);
    expect(result.current.stream).toEqual({
      status: 'degraded',
      error: 'Malformed session stream event.',
    });
    expect(mockReportClientError).toHaveBeenCalledWith({
      component: 'session-stream',
      operation: 'parse stream event',
      message: 'gc-session-1: Malformed session stream event.',
    });
  });

  it('reports initial transcript load failure without nullable result fields', async () => {
    mockFetchSupervisorSessionTranscript.mockRejectedValue(new Error('peek failed'));

    const { result } = renderHook(() => useSessionStream('gc-session-1', true));
    await flush();

    expect(result.current).toEqual({
      status: 'failed',
      error: 'peek failed',
      stream: { status: 'idle' },
    });
    expect(mockReportClientError).toHaveBeenCalledWith({
      component: 'session-stream',
      operation: 'load transcript',
      message: 'gc-session-1: peek failed',
    });
  });
});

async function flush(): Promise<void> {
  await act(async () => {
    await Promise.resolve();
  });
}

class FakeEventSource {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;

  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  readyState = FakeEventSource.CONNECTING;
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
  }

  open(): void {
    this.readyState = FakeEventSource.OPEN;
    this.onopen?.(new Event('open'));
  }

  emit(type: string, data: string): void {
    const event = new MessageEvent<string>(type, { data });
    this.listeners.get(type)?.forEach((listener) => listener(event));
    if (type === 'message') this.onmessage?.(event);
  }
}

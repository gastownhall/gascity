import { act, cleanup, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { DashboardSession } from 'gas-city-dashboard-shared';
import type {
  AgentResponse,
  SessionTranscriptGetResponse,
} from 'gas-city-dashboard-shared/gc-supervisor';
import {
  LiveSessionPeek,
  isAgentStreamable,
  isSessionStreamable,
  streamBadge,
} from './LiveSessionPeek';

// ── Minimal FakeEventSource (mirrors the pattern in WorkflowRunDetail.test) ──
const eventSources: FakeEventSource[] = [];

class FakeEventSource {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;

  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  readyState = FakeEventSource.CONNECTING;
  readonly listeners = new Map<string, Array<(event: MessageEvent<string>) => void>>();

  constructor(readonly url: string) {
    eventSources.push(this);
  }

  open(): void {
    this.readyState = FakeEventSource.OPEN;
    this.onopen?.(new Event('open'));
  }

  addEventListener(type: string, listener: (event: MessageEvent<string>) => void): void {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
  }

  removeEventListener(type: string, listener: (event: MessageEvent<string>) => void): void {
    this.listeners.set(
      type,
      (this.listeners.get(type) ?? []).filter((l) => l !== listener),
    );
  }

  dispatch(type: string, payload: unknown): void {
    const event = new MessageEvent(type, { data: JSON.stringify(payload) });
    for (const listener of this.listeners.get(type) ?? []) listener(event);
  }

  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }
}

function jsonResponse(payload: unknown): Response {
  return new Response(JSON.stringify(payload), {
    status: 200,
    headers: { 'content-type': 'application/json' },
  });
}

const snapshot: SessionTranscriptGetResponse = {
  id: 's1',
  template: 'mayor',
  provider: 'claude',
  format: 'conversation',
  turns: [{ role: 'assistant', text: 'snapshot turn body' }],
};

function session(overrides: Partial<DashboardSession>): DashboardSession {
  return { id: 's1', state: 'active', ...overrides } as DashboardSession;
}

// gascity-dashboard-ay6 / Phase-4 follow-up: minimal AgentResponse factory for
// the isAgentStreamable boundary tests. Matches the shape AgentSchema
// emits (name + state + optional session/running).
function agent(overrides: Partial<AgentResponse> & { sessionPresent?: boolean }): AgentResponse {
  const { sessionPresent, ...rest } = overrides;
  // Build the session field conditionally so exactOptionalPropertyTypes
  // doesn't complain about a `session: undefined` assignment when no
  // session is requested. Spreading is the idiom that satisfies the
  // strict optionality contract.
  const sessionField =
    sessionPresent && !('session' in rest)
      ? { session: { name: 's1', attached: false, last_activity: '2026-05-29T00:00:00Z' } }
      : {};
  return {
    name: 'a1',
    available: true,
    state: 'asleep',
    running: false,
    suspended: false,
    ...rest,
    ...sessionField,
  };
}

describe('streamBadge', () => {
  it('maps each connection state to a tone + label', () => {
    expect(streamBadge('open')).toEqual({ tone: 'ok', label: 'live' });
    expect(streamBadge('connecting')).toEqual({ tone: 'warn', label: 'connecting' });
    expect(streamBadge('closed')).toEqual({ tone: 'stuck', label: 'offline' });
    expect(streamBadge('idle')).toEqual({ tone: 'neutral', label: 'snapshot' });
  });
});

describe('isSessionStreamable', () => {
  it('is false for null', () => {
    expect(isSessionStreamable(null)).toBe(false);
  });
  it('is true when the process is running', () => {
    expect(isSessionStreamable(session({ state: 'asleep', running: true }))).toBe(true);
  });
  it('is true when the gc state is active', () => {
    expect(isSessionStreamable(session({ state: 'active', running: false }))).toBe(true);
  });
  it("is true when the gc state is 'running' (aligns with isRunningAgent)", () => {
    expect(isSessionStreamable(session({ state: 'running', running: false }))).toBe(true);
  });
  it('is false for a non-running, non-active session', () => {
    expect(isSessionStreamable(session({ state: 'asleep', running: false }))).toBe(false);
  });
});

describe('isAgentStreamable', () => {
  it('is false for null', () => {
    expect(isAgentStreamable(null)).toBe(false);
  });
  it('is false for an orphan agent (no session) even when state/running say active', () => {
    // The session guard is load-bearing: even a fully-active agent with
    // no session has nothing to stream from. This is the H2/M2 invariant
    // the Phase-4 review flagged as untested.
    expect(isAgentStreamable(agent({ state: 'active', running: true }))).toBe(false);
  });
  it('is true when the agent has a session AND running=true', () => {
    expect(isAgentStreamable(agent({ state: 'asleep', running: true, sessionPresent: true }))).toBe(
      true,
    );
  });
  it('is true when the agent has a session AND gc state is active', () => {
    expect(
      isAgentStreamable(agent({ state: 'active', running: false, sessionPresent: true })),
    ).toBe(true);
  });
  it("is true when the agent has a session AND gc state is 'running'", () => {
    expect(
      isAgentStreamable(agent({ state: 'running', running: false, sessionPresent: true })),
    ).toBe(true);
  });
  it('is false for a resting agent with a session (asleep + not running)', () => {
    expect(
      isAgentStreamable(agent({ state: 'asleep', running: false, sessionPresent: true })),
    ).toBe(false);
  });
});

describe('LiveSessionPeek (streaming)', () => {
  beforeEach(() => {
    eventSources.length = 0;
    vi.stubGlobal('EventSource', FakeEventSource);
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = requestUrl(input);
        if (url === '/gc-supervisor/v0/city/test-city/session/s1/transcript?format=conversation') {
          return jsonResponse(snapshot);
        }
        throw new Error(`unexpected fetch: ${url}`);
      }),
    );
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it('renders the snapshot, then a live turn appended over the stream, with a live badge', async () => {
    render(<LiveSessionPeek sessionId="s1" stream showBadge showCaption />);

    // Snapshot fetched and rendered.
    expect(await screen.findByText('snapshot turn body')).toBeTruthy();
    // While the stream opens, the badge reads "connecting".
    expect(screen.getByText('connecting')).toBeTruthy();

    // Open the stream -> badge flips to "live".
    const es = eventSources[0];
    expect(es).toBeDefined();
    act(() => es!.open());
    expect(await screen.findByText('live')).toBeTruthy();

    // A streamed 'turn' event appends without replacing the snapshot.
    act(() => es!.dispatch('turn', { role: 'assistant', text: 'streamed turn body' }));
    expect(await screen.findByText('streamed turn body')).toBeTruthy();
    expect(screen.getByText('snapshot turn body')).toBeTruthy();
  });

  it('shows a snapshot badge and opens no EventSource when stream is false', async () => {
    render(<LiveSessionPeek sessionId="s1" stream={false} showBadge showCaption />);
    expect(await screen.findByText('snapshot turn body')).toBeTruthy();
    expect(screen.getByText('snapshot')).toBeTruthy();
    expect(eventSources).toHaveLength(0);
  });

  it('forwards the caption (turn count + captured age) into the TranscriptBox header', async () => {
    // Guards the one-line caption spread in SessionPeekContent: showCaption must
    // surface the turn-count / captured-age string in the transcript header, not
    // drop it silently.
    render(<LiveSessionPeek sessionId="s1" stream={false} showBadge showCaption />);
    await screen.findByText('snapshot turn body');
    const caption = screen.getByText(/turn\(s\)/);
    expect(caption.textContent).toMatch(/1 turn\(s\)/);
    expect(caption.textContent).toMatch(/captured/);
  });
});

function requestUrl(input: RequestInfo | URL): string {
  const url =
    input instanceof Request ? input.url : input instanceof URL ? input.toString() : String(input);
  return stripSameOrigin(url);
}

function stripSameOrigin(url: string): string {
  const origin = window.location.origin;
  return url.startsWith(origin) ? url.slice(origin.length) : url;
}

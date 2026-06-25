import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setActiveCity } from '../api/cityBase';
import { invalidate } from '../api/cache';
import { AttentionProvider } from '../attention/context';
import { createAttentionContributors } from '../attention/registry';
import type { TypedEventStreamEnvelope } from 'gas-city-dashboard-shared/gc-supervisor';
import { ActivityPage } from './Activity';

interface FetchCall {
  path: string;
  query: URLSearchParams;
}

const fetchCalls: FetchCall[] = [];
let eventFetchMode: 'ok' | 'partial' | 'fail' | 'duplicate-audit' = 'ok';

beforeEach(() => {
  setActiveCity('test-city');
  fetchCalls.length = 0;
  eventFetchMode = 'ok';
  invalidate('activity:bundle:test-city');
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(
        input instanceof Request ? input.url : String(input),
        'http://dashboard.local',
      );
      fetchCalls.push({ path: url.pathname, query: url.searchParams });

      if (url.pathname === '/gc-supervisor/v0/city/test-city/events') {
        if (eventFetchMode === 'fail') {
          return new Response(JSON.stringify({ error: 'event store offline' }), {
            status: 503,
            headers: { 'content-type': 'application/json' },
          });
        }
        if (eventFetchMode === 'duplicate-audit') {
          // Dashboard audit records forwarded into the supervisor event log
          // carry no sequence number, so several arrive with seq 0
          // (gascity-dashboard-q89b).
          return jsonResponse({
            items: [
              supervisorEvent({
                actor: 'dashboard',
                message: 'audit fetch one',
                seq: 0,
                subject: 'GET /api/health/system',
                type: 'dashboard.fetch',
              }),
              supervisorEvent({
                actor: 'dashboard',
                message: 'audit fetch two',
                seq: 0,
                subject: 'GET /api/health/local-tools',
                type: 'dashboard.fetch',
              }),
            ],
            partial: false,
            total: 2,
          });
        }
        return jsonResponse({
          items: [
            supervisorEvent({
              actor: 'maintainer',
              message: 'session crashed while applying patch',
              seq: 42,
              subject: 'gc-session-1',
              type: 'session.crashed',
            }),
            supervisorEvent({
              actor: 'supervisor',
              message: 'event archive rotated',
              seq: 41,
              subject: 'events',
              type: 'events.rotated',
            }),
            supervisorEvent({
              actor: 'rig-runner',
              message: 'session suspended for operator review',
              seq: 40,
              subject: 'gc-session-2',
              type: 'session.suspended',
            }),
          ],
          partial: eventFetchMode === 'partial',
          partial_errors:
            eventFetchMode === 'partial' ? ['events truncated at archive boundary'] : undefined,
          total: 2,
        });
      }
      if (url.pathname === '/api/builds') {
        return jsonResponse({
          failed_marker: true,
          source: '/tmp/.dev-deploy.log',
          items: [
            {
              at: '2026-06-01T10:00:00.000Z',
              detail: 'stage: frontend',
              status: 'failed',
            },
          ],
        });
      }
      if (url.pathname === '/api/git/commits') {
        return jsonResponse({
          view: url.searchParams.get('view') ?? 'recent-main',
          items: [
            {
              author: 'Chris',
              date: '2026-06-01T09:30:00.000Z',
              sha: 'abcdef123456',
              short_sha: 'abcdef1',
              subject: 'wire activity route',
            },
          ],
        });
      }
      throw new Error(`unexpected fetch: ${url.pathname}${url.search}`);
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('ActivityPage', () => {
  it('loads supervisor events through the generated-client proxy and honors type deep links', async () => {
    renderPage('/activity?mode=events&type=session.crashed');

    const table = await screen.findByRole('table', { name: /supervisor events/i });

    expect(await within(table).findByText('session.crashed')).toBeTruthy();
    expect(await within(table).findByText('session crashed while applying patch')).toBeTruthy();
    expect(within(table).queryByText('events.rotated')).toBeNull();

    const eventFetch = fetchCalls.find(
      (call) => call.path === '/gc-supervisor/v0/city/test-city/events',
    );
    expect(eventFetch).toBeDefined();
    expect(eventFetch?.query.get('type')).toBe('session.crashed');
    expect(eventFetch?.query.get('since')).toBe('24h');
    expect(fetchCalls.some((call) => call.path === '/api/city/test-city/events')).toBe(false);
  });

  it('renders seq-less dashboard audit events without React key collisions (gascity-dashboard-q89b)', async () => {
    eventFetchMode = 'duplicate-audit';
    const consoleError = vi.spyOn(console, 'error');

    renderPage('/activity?mode=events');

    const table = await screen.findByRole('table', { name: /supervisor events/i });
    expect(await within(table).findByText('audit fetch one')).toBeTruthy();
    expect(within(table).getByText('audit fetch two')).toBeTruthy();
    const keyWarnings = consoleError.mock.calls.filter((call) =>
      String(call[0]).includes('same key'),
    );
    expect(keyWarnings).toHaveLength(0);
  });

  it('renders deploy and commit activity without using supervisor mirrors', async () => {
    renderPage('/activity');

    expect(await screen.findByText('stage: frontend')).toBeTruthy();
    expect(screen.getByText('wire activity route')).toBeTruthy();
    expect(fetchCalls.some((call) => call.path === '/api/builds')).toBe(true);
    expect(fetchCalls.some((call) => call.path === '/api/git/commits')).toBe(true);
    expect(fetchCalls.some((call) => call.path === '/api/city/test-city/snapshot')).toBe(false);
  });

  it('marks event rows that match composed Activity attention', async () => {
    renderPage('/activity?mode=events', {
      eventId: 'activity:event:42:session.crashed',
    });

    const eventText = await screen.findByText('session.crashed');
    const row = eventText.closest('tr');
    expect(row?.getAttribute('data-attention-severity')).toBe('attention');
  });

  it('offers event window, type, and text filters and forwards them to supervisor reads', async () => {
    renderPage('/activity?mode=events&since=7d&type=session.crashed&q=patch');

    const table = await screen.findByRole('table', { name: /supervisor events/i });

    expect((screen.getByLabelText('Event window') as HTMLSelectElement).value).toBe('7d');
    expect((screen.getByLabelText('Event type') as HTMLInputElement).value).toBe('session.crashed');
    expect((screen.getByLabelText('Search activity') as HTMLInputElement).value).toBe('patch');
    expect(await within(table).findByText('session.crashed')).toBeTruthy();
    expect(within(table).queryByText('events.rotated')).toBeNull();

    const eventFetch = fetchCalls.find(
      (call) => call.path === '/gc-supervisor/v0/city/test-city/events',
    );
    expect(eventFetch?.query.get('type')).toBe('session.crashed');
    expect(eventFetch?.query.get('since')).toBe('7d');

    fireEvent.change(screen.getByLabelText('Search activity'), {
      target: { value: 'archive' },
    });
    expect(
      await within(table).findByText('No supervisor events match these filters.'),
    ).toBeTruthy();
  });

  it('offers actor and severity filters while routing only supervisor-supported filters through the generated client', async () => {
    renderPage('/activity?mode=events&actor=rig-runner&signal=watch');

    const table = await screen.findByRole('table', { name: /supervisor events/i });

    expect((screen.getByLabelText('Event actor') as HTMLInputElement).value).toBe('rig-runner');
    expect((screen.getByLabelText('Signal severity') as HTMLSelectElement).value).toBe('watch');
    expect(await within(table).findByText('session.suspended')).toBeTruthy();
    expect(within(table).queryByText('session.crashed')).toBeNull();
    expect(within(table).queryByText('events.rotated')).toBeNull();

    const eventFetch = fetchCalls.find(
      (call) => call.path === '/gc-supervisor/v0/city/test-city/events',
    );
    expect(eventFetch?.query.get('actor')).toBe('rig-runner');
    expect(eventFetch?.query.has('signal')).toBe(false);
  });

  it('marks deploy rows that match composed Activity attention', async () => {
    renderPage('/activity?mode=deploys', {
      deploys: true,
    });

    const deployText = await screen.findByText('stage: frontend');
    const row = deployText.closest('tr');
    expect(row?.getAttribute('data-attention-severity')).toBe('attention');
  });

  it('surfaces partial event history details on the Activity route', async () => {
    eventFetchMode = 'partial';

    renderPage('/activity?mode=events');

    expect(await screen.findByText(/event history incomplete/i)).toBeTruthy();
    expect(screen.getByText(/events truncated at archive boundary/i)).toBeTruthy();
  });

  it('keeps deploys and commits visible when supervisor events fail', async () => {
    eventFetchMode = 'fail';

    renderPage('/activity');

    expect(await screen.findByText('stage: frontend')).toBeTruthy();
    expect(screen.getByText('wire activity route')).toBeTruthy();
    expect(screen.getAllByText(/event history unavailable/i).length).toBeGreaterThan(0);
  });
});

function renderPage(path: string, attention: { eventId?: string; deploys?: boolean } = {}) {
  const facts = {
    ...(attention.eventId === undefined
      ? {}
      : {
          events: [
            supervisorEvent({
              seq: 42,
              type: 'session.crashed',
            }),
          ],
        }),
    ...(attention.deploys === true
      ? {
          deploys: {
            failed_marker: true,
            source: '/tmp/.dev-deploy.log',
            items: [
              {
                at: '2026-06-01T10:00:00.000Z',
                detail: 'stage: frontend',
                status: 'failed' as const,
              },
            ],
          },
        }
      : {}),
  };
  return render(
    <MemoryRouter
      initialEntries={[path]}
      future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
    >
      <AttentionProvider
        contributors={createAttentionContributors(
          Object.keys(facts).length === 0 ? {} : { activity: facts },
        )}
      >
        <ActivityPage />
      </AttentionProvider>
    </MemoryRouter>,
  );
}

function supervisorEvent(overrides: {
  actor?: string;
  message?: string;
  seq?: number;
  subject?: string;
  type?: string;
}): TypedEventStreamEnvelope {
  const type = overrides.type ?? 'session.crashed';
  return {
    actor: overrides.actor ?? 'supervisor',
    message: overrides.message ?? 'event message',
    payload:
      type === 'events.rotated'
        ? {
            prior_archive: '/tmp/events-1.jsonl',
            prior_first_seq: 1,
            prior_last_seq: 40,
          }
        : type === 'session.suspended'
          ? {}
          : {
              reason: 'panic',
              session_id: 'gc-session-1',
              template: 'mayor',
            },
    seq: overrides.seq ?? 1,
    subject: overrides.subject ?? 'subject',
    ts: '2026-06-01T10:10:00.000Z',
    type,
  } as TypedEventStreamEnvelope;
}

function jsonResponse(payload: unknown): Response {
  return new Response(JSON.stringify(payload), {
    status: 200,
    headers: { 'content-type': 'application/json' },
  });
}

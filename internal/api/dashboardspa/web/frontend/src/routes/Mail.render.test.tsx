import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { invalidate } from '../api/cache';
import { AttentionProvider } from '../attention/context';
import type { AttentionContributor } from '../attention/compose';
import { NowProvider } from '../contexts/NowContext';
import { ReadOnlyProvider } from '../contexts/ReadOnlyContext';
import { MailPage } from './Mail';

interface FetchCall {
  method: string;
  url: string;
  body: unknown;
  gcRequest: string | null;
}

const fetchCalls: FetchCall[] = [];

// Stateful read-state overrides so a mark-read/unread POST is reflected by the
// next mailbox GET — lets the bulk-mark tests assert the needs-you count (the
// same selectOperatorActionableUnread the nav badge reads) actually updates.
const markedRead = new Map<string, boolean>();

function applyReadState(item: Record<string, unknown>): Record<string, unknown> {
  const id = item.id as string;
  return markedRead.has(id) ? { ...item, read: markedRead.get(id) } : item;
}

vi.mock('../contexts/ViewingAsContext', () => ({
  useViewingAs: () => ({
    viewingAs: { alias: 'stephanie', isOperator: true },
    setAlias: vi.fn(),
    resetToOperator: vi.fn(),
    aliasBuckets: [],
    aliasesLoading: false,
    sessionsUnavailable: false,
    loadAliases: vi.fn(),
  }),
}));

// Operator identity from /config (gascity-dashboard-bhvn): display alias
// `stephanie` maps to the gc wire alias `human` for mailbox filtering.
vi.mock('../contexts/OperatorConfigContext', () => ({
  useOperatorConfig: () => ({
    operatorAlias: 'stephanie',
    operatorWireAlias: 'human',
    decisionLabel: 'needs/stephanie',
  }),
}));

function stubFetch() {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      const method = requestMethod(input, init);
      fetchCalls.push({
        method,
        url,
        body: await requestBody(input, init),
        gcRequest: requestHeader(input, init, 'X-GC-Request'),
      });
      if (url === '/v0/city/test-city/mail?limit=100') {
        return jsonResponse({
          items: [
            mail({
              id: 'mail-inbox',
              from: 'mayor',
              to: 'human',
              subject: 'direct supervisor inbox',
              body: 'inbox preview',
              created_at: '2026-06-01T10:00:00Z',
              thread_id: 'thread-direct',
            }),
            // Unread and addressed to the operator, but pool-worker firehose: it
            // counts as inbox unread yet must NOT count toward "needs you"
            // (gascity-dashboard-2j8e.5).
            mail({
              id: 'mail-firehose',
              from: '/home/ds/gascity/polecat-2',
              to: 'human',
              subject: 'worker firehose noise',
              body: 'status update preview',
              created_at: '2026-06-01T09:59:00Z',
              thread_id: 'thread-firehose',
            }),
            mail({
              id: 'mail-sent',
              from: 'human',
              to: 'mechanic',
              subject: 'operator sent only',
              body: 'sent preview',
              created_at: '2026-06-01T10:01:00Z',
              read: true,
              thread_id: 'thread-direct',
            }),
            mail({
              id: 'mail-other',
              from: 'mechanic',
              to: 'mayor',
              subject: 'other inbox',
              body: 'other preview',
              created_at: '2026-06-01T10:02:00Z',
              thread_id: 'thread-other',
            }),
          ].map(applyReadState),
          total: 4,
        });
      }
      if (url === '/v0/city/test-city/mail?limit=1000') {
        return jsonResponse({
          items: [
            mail({
              id: 'mail-inbox',
              from: 'mayor',
              to: 'human',
              subject: 'direct supervisor inbox',
              body: 'inbox preview',
              created_at: '2026-06-01T10:00:00Z',
              thread_id: 'thread-direct',
            }),
            mail({
              id: 'mail-sent',
              from: 'human',
              to: 'mechanic',
              subject: 'operator sent only',
              body: 'sent preview',
              created_at: '2026-06-01T10:01:00Z',
              read: true,
              thread_id: 'thread-direct',
            }),
            mail({
              id: 'mail-other',
              from: 'mechanic',
              to: 'mayor',
              subject: 'other inbox',
              body: 'other preview',
              created_at: '2026-06-01T10:02:00Z',
              thread_id: 'thread-other',
            }),
            mail({
              id: 'mail-older',
              from: 'mayor',
              to: 'human',
              subject: 'expanded history item',
              body: 'older preview',
              created_at: '2026-05-30T10:02:00Z',
              read: true,
              thread_id: 'thread-older',
            }),
          ],
          total: 4,
        });
      }
      if (url === '/v0/city/test-city/mail/thread/thread-direct') {
        return jsonResponse({
          items: [
            mail({
              id: 'thread-new',
              from: 'human',
              to: 'mayor',
              subject: 'direct supervisor inbox',
              body: 'newest in thread',
              created_at: '2026-06-01T10:02:00Z',
              thread_id: 'thread-direct',
            }),
            mail({
              id: 'thread-old',
              from: 'mayor',
              to: 'human',
              subject: 'direct supervisor inbox',
              body: 'oldest in thread',
              created_at: '2026-06-01T10:00:00Z',
              thread_id: 'thread-direct',
            }),
          ],
          total: 2,
        });
      }
      if (url === '/v0/city/test-city/mail' && method === 'POST') {
        return jsonResponse(
          {
            id: 'mail-new',
            from: 'human',
            to: 'mayor',
            subject: 'status',
            body: 'all green',
            created_at: '2026-06-01T10:03:00Z',
            read: false,
            thread_id: 'thread-new',
          },
          201,
        );
      }
      const readMatch = url.match(/\/mail\/([^/]+)\/read$/);
      if (readMatch?.[1] !== undefined && method === 'POST') {
        markedRead.set(readMatch[1], true);
        return jsonResponse({ status: 'ok' });
      }
      const unreadMatch = url.match(/\/mail\/([^/]+)\/mark-unread$/);
      if (unreadMatch?.[1] !== undefined && method === 'POST') {
        markedRead.set(unreadMatch[1], false);
        return jsonResponse({ status: 'ok' });
      }
      if (url === '/v0/city/test-city/mail/mail-inbox/archive' && method === 'POST') {
        return jsonResponse({ status: 'ok' });
      }
      if (url === '/v0/city/test-city/mail/mail-inbox/reply' && method === 'POST') {
        return jsonResponse(
          {
            id: 'mail-reply',
            from: 'human',
            to: 'mayor',
            subject: 'direct supervisor inbox',
            body: 'got it',
            created_at: '2026-06-01T10:04:00Z',
            read: false,
            thread_id: 'thread-direct',
          },
          201,
        );
      }
      throw new Error(`unexpected fetch: ${url}`);
    }),
  );
}

function requestUrl(input: RequestInfo | URL): string {
  const url =
    input instanceof Request ? input.url : input instanceof URL ? input.toString() : String(input);
  return stripSameOrigin(url);
}

function stripSameOrigin(url: string): string {
  const origin = window.location.origin;
  return url.startsWith(origin) ? url.slice(origin.length) : url;
}

function requestMethod(input: RequestInfo | URL, init: RequestInit | undefined): string {
  if (init?.method !== undefined) return init.method;
  if (input instanceof Request) return input.method;
  return 'GET';
}

async function requestBody(
  input: RequestInfo | URL,
  init: RequestInit | undefined,
): Promise<unknown> {
  const raw = init?.body ?? (input instanceof Request ? await input.clone().text() : undefined);
  if (typeof raw !== 'string' || raw.length === 0) return undefined;
  return JSON.parse(raw) as unknown;
}

function requestHeader(
  input: RequestInfo | URL,
  init: RequestInit | undefined,
  name: string,
): string | null {
  const headers = new Headers(input instanceof Request ? input.headers : init?.headers);
  return headers.get(name);
}

function jsonResponse(payload: unknown, status = 200): Response {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

function mail(overrides: Record<string, unknown>): Record<string, unknown> {
  return {
    id: 'mail-1',
    from: 'mayor',
    to: 'human',
    subject: 'subject',
    body: 'body',
    created_at: '2026-06-01T10:00:00Z',
    read: false,
    ...overrides,
  };
}

beforeEach(() => {
  fetchCalls.length = 0;
  markedRead.clear();
  invalidate('mail');
  stubFetch();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('MailPage supervisor reads', () => {
  it('loads the operator inbox from the supervisor API instead of the dashboard GET mirror', async () => {
    renderMailPage();

    await screen.findByText('direct supervisor inbox');

    expect(fetchCalls.map((call) => call.url)).toContain('/v0/city/test-city/mail?limit=100');
    expect(fetchCalls.some((call) => call.url.startsWith('/api/city/test-city/mail'))).toBe(false);
    expect(screen.queryByText('operator sent only')).toBeNull();
    expect(screen.queryByText('other inbox')).toBeNull();
  });

  it('foregrounds the operator needs-you count, folding the pool-worker firehose (gascity-dashboard-2j8e.5)', async () => {
    renderMailPage();

    await screen.findByText('direct supervisor inbox');
    // Two unread to:human (mayor + a polecat firehose message), but only the
    // mayor escalation needs the operator — the same count the nav badge shows.
    expect(screen.getByText(/1 need you of 2 unread/)).toBeTruthy();
  });

  it('the needs-you chip surfaces operator escalations and hides the firehose (gascity-dashboard-2j8e.5)', async () => {
    renderMailPage();

    await screen.findByText('direct supervisor inbox');
    expect(screen.getByText('worker firehose noise')).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: 'needs you' }));

    expect(screen.getByText('direct supervisor inbox')).toBeTruthy();
    expect(screen.queryByText('worker firehose noise')).toBeNull();
  });

  it('expands mailbox history through the generated supervisor limit query', async () => {
    renderMailPage();

    await screen.findByText('direct supervisor inbox');
    fireEvent.change(screen.getByLabelText('Mail history limit'), {
      target: { value: '1000' },
    });

    expect(await screen.findByText('expanded history item')).toBeTruthy();
    expect(fetchCalls.map((call) => call.url)).toContain('/v0/city/test-city/mail?limit=1000');
    expect((screen.getByLabelText('Mail history limit') as HTMLSelectElement).value).toBe('1000');
  });

  it('exposes a clock window filter without changing the generated supervisor history query', async () => {
    renderMailPage();

    await screen.findByText('direct supervisor inbox');
    expect((screen.getByLabelText('Mail time window') as HTMLSelectElement).value).toBe('all');

    fireEvent.change(screen.getByLabelText('Mail time window'), {
      target: { value: '7d' },
    });

    expect((screen.getByLabelText('Mail time window') as HTMLSelectElement).value).toBe('7d');
    expect(fetchCalls.map((call) => call.url)).toContain('/v0/city/test-city/mail?limit=100');
    expect(fetchCalls.some((call) => call.url.includes('since='))).toBe(false);
  });

  it('loads thread messages from the supervisor thread endpoint', async () => {
    renderMailPage();

    fireEvent.click(await screen.findByText('direct supervisor inbox'));

    await screen.findByText('oldest in thread');
    expect(screen.getByText('newest in thread')).toBeTruthy();
    expect(fetchCalls.map((call) => call.url)).toContain(
      '/v0/city/test-city/mail/thread/thread-direct',
    );
    expect(fetchCalls.some((call) => call.url.startsWith('/api/city/test-city/mail/threads'))).toBe(
      false,
    );
  });

  it('opens a message thread from the message query parameter', async () => {
    renderMailPage('/mail?message=mail-inbox');

    await screen.findByText('oldest in thread');

    expect(screen.getByText('newest in thread')).toBeTruthy();
    expect(fetchCalls.map((call) => call.url)).toContain('/v0/city/test-city/mail?limit=1000');
    expect(fetchCalls.map((call) => call.url)).toContain(
      '/v0/city/test-city/mail/thread/thread-direct',
    );
  });

  it('sends mail through the supervisor API instead of the dashboard write mirror', async () => {
    renderMailPage();

    fireEvent.click(await screen.findByRole('button', { name: 'Compose' }));
    fireEvent.change(screen.getByLabelText('To (alias)'), { target: { value: 'mayor' } });
    fireEvent.change(screen.getByLabelText('Subject'), { target: { value: 'status' } });
    fireEvent.change(screen.getByLabelText('Body'), { target: { value: 'all green' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));

    await screen.findByText('direct supervisor inbox');
    expect(fetchCalls).toContainEqual({
      method: 'POST',
      url: '/v0/city/test-city/mail',
      body: {
        to: 'mayor',
        subject: 'status',
        body: 'all green',
        from: 'human',
      },
      gcRequest: 'dashboard',
    });
    expect(fetchCalls.some((call) => call.url === '/api/city/test-city/mail-send')).toBe(false);
  });

  it('offers an all-traffic mailbox mode without filtering out sent or non-operator mail', async () => {
    renderMailPage();

    fireEvent.click(await screen.findByRole('button', { name: 'All' }));

    await screen.findByText('direct supervisor inbox');
    expect(screen.getByText('operator sent only')).toBeTruthy();
    expect(screen.getByText('other inbox')).toBeTruthy();
  });

  it('marks unread inbox mail read through the supervisor API', async () => {
    renderMailPage();

    fireEvent.click(await screen.findByText('direct supervisor inbox'));
    fireEvent.click(await screen.findByRole('button', { name: 'Mark read' }));

    await waitFor(() => {
      expect(fetchCalls).toContainEqual({
        method: 'POST',
        url: '/v0/city/test-city/mail/mail-inbox/read',
        body: undefined,
        gcRequest: 'dashboard',
      });
    });
  });

  it('marks read mail unread through the supervisor API', async () => {
    renderMailPage();

    fireEvent.click(await screen.findByRole('button', { name: 'All' }));
    fireEvent.click(await screen.findByText('operator sent only'));
    fireEvent.click(await screen.findByRole('button', { name: 'Mark unread' }));

    await screen.findByRole('button', { name: 'Mark read' });
    await waitFor(() => {
      expect(fetchCalls).toContainEqual({
        method: 'POST',
        url: '/v0/city/test-city/mail/mail-sent/mark-unread',
        body: undefined,
        gcRequest: 'dashboard',
      });
    });
  });

  it('archives mail through the supervisor API', async () => {
    renderMailPage();

    fireEvent.click(await screen.findByText('direct supervisor inbox'));
    fireEvent.click(await screen.findByRole('button', { name: 'Archive' }));

    await waitFor(() => {
      expect(fetchCalls).toContainEqual({
        method: 'POST',
        url: '/v0/city/test-city/mail/mail-inbox/archive',
        body: undefined,
        gcRequest: 'dashboard',
      });
    });
  });

  it('replies to a thread through the supervisor API as the operator', async () => {
    renderMailPage();

    fireEvent.click(await screen.findByText('direct supervisor inbox'));
    fireEvent.change(await screen.findByLabelText('Reply'), {
      target: { value: 'got it' },
    });
    fireEvent.click(await screen.findByRole('button', { name: 'Reply' }));

    await waitFor(() => {
      expect(fetchCalls).toContainEqual({
        method: 'POST',
        url: '/v0/city/test-city/mail/mail-inbox/reply',
        body: {
          body: 'got it',
          from: 'human',
        },
        gcRequest: 'dashboard',
      });
    });
  });

  it('disables Compose and surfaces a read-only affordance in read-only mode', async () => {
    renderMailPage('/mail', { readOnly: true });

    await screen.findByText('direct supervisor inbox');

    const compose = screen.getByRole('button', { name: 'Compose' }) as HTMLButtonElement;
    expect(compose.disabled).toBe(true);
    expect(compose.getAttribute('title')).toBe('Read-only mode: mutations are disabled');
    // The affordance carries words, not just a dimmed control (DESIGN.md §States).
    expect(screen.getByText('Read-only')).toBeTruthy();
  });

  it('disables the thread mark/archive/reply actions in read-only mode', async () => {
    renderMailPage('/mail', { readOnly: true });

    fireEvent.click(await screen.findByText('direct supervisor inbox'));

    expect(
      (await screen.findByRole('button', { name: 'Mark read' })) as HTMLButtonElement,
    ).toHaveProperty('disabled', true);
    expect((screen.getByRole('button', { name: 'Archive' }) as HTMLButtonElement).disabled).toBe(
      true,
    );
    expect((screen.getByRole('button', { name: 'Reply' }) as HTMLButtonElement).disabled).toBe(
      true,
    );
  });

  it('keeps Compose active when the dashboard is writable', async () => {
    renderMailPage();

    await screen.findByText('direct supervisor inbox');

    expect((screen.getByRole('button', { name: 'Compose' }) as HTMLButtonElement).disabled).toBe(
      false,
    );
    expect(screen.queryByText('Read-only')).toBeNull();
  });

  it('marks attention mail rows without filtering out non-attention mail', async () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <NowProvider intervalMs={1_000_000}>
          <AttentionProvider
            contributors={[
              contributor('mail', [
                {
                  id: 'mail:mail-inbox:unread',
                  domain: 'mail',
                  severity: 'attention',
                  title: 'direct supervisor inbox',
                },
              ]),
            ]}
          >
            <MailPage />
          </AttentionProvider>
        </NowProvider>
      </MemoryRouter>,
    );

    const inboxRow = (await screen.findByText('direct supervisor inbox')).closest('tr');
    expect(inboxRow?.getAttribute('data-attention-severity')).toBe('attention');
    expect(screen.queryByText('operator sent only')).toBeNull();
  });

  it('marks attention messages inside an opened thread', async () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <NowProvider intervalMs={1_000_000}>
          <AttentionProvider
            contributors={[
              contributor('mail', [
                {
                  id: 'mail:thread-old:unread',
                  domain: 'mail',
                  severity: 'attention',
                  title: 'direct supervisor inbox',
                },
              ]),
            ]}
          >
            <MailPage />
          </AttentionProvider>
        </NowProvider>
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByText('direct supervisor inbox'));

    const threadArticle = (await screen.findByText('oldest in thread')).closest('article');
    expect(threadArticle?.getAttribute('data-attention-severity')).toBe('attention');
  });
});

describe('MailPage bulk read-state selection (gascity-dashboard-mp3g)', () => {
  it('select-all selects the whole visible set, and rows toggle independently', async () => {
    renderMailPage();
    await screen.findByText('direct supervisor inbox');

    const selectAll = screen.getByRole('checkbox', { name: 'select all mail' });
    fireEvent.click(selectAll);
    // Both visible inbox rows (mayor escalation + polecat firehose) selected.
    expect(screen.getByText('2 selected')).toBeTruthy();

    fireEvent.click(selectAll);
    expect(screen.queryByText(/selected/)).toBeNull();

    fireEvent.click(screen.getByRole('checkbox', { name: /select mail: direct supervisor inbox/ }));
    expect(screen.getByText('1 selected')).toBeTruthy();
    // Toggling a row checkbox must not open the thread modal (no Reply field).
    expect(screen.queryByLabelText('Reply')).toBeNull();
  });

  it('bulk-marks the selected inbox mail read through the supervisor API', async () => {
    renderMailPage();
    await screen.findByText('direct supervisor inbox');

    fireEvent.click(screen.getByRole('checkbox', { name: 'select all mail' }));
    fireEvent.click(screen.getByRole('button', { name: 'Mark read' }));

    await waitFor(() => {
      expect(fetchCalls).toContainEqual({
        method: 'POST',
        url: '/v0/city/test-city/mail/mail-inbox/read',
        body: undefined,
        gcRequest: 'dashboard',
      });
    });
    expect(fetchCalls).toContainEqual({
      method: 'POST',
      url: '/v0/city/test-city/mail/mail-firehose/read',
      body: undefined,
      gcRequest: 'dashboard',
    });
  });

  it('bulk-marks the selected read mail unread, skipping already-unread rows (no second mark)', async () => {
    renderMailPage();
    fireEvent.click(await screen.findByRole('button', { name: 'All' }));
    await screen.findByText('operator sent only');

    fireEvent.click(screen.getByRole('checkbox', { name: 'select all mail' }));
    fireEvent.click(screen.getByRole('button', { name: 'Mark unread' }));

    await waitFor(() => {
      expect(fetchCalls).toContainEqual({
        method: 'POST',
        url: '/v0/city/test-city/mail/mail-sent/mark-unread',
        body: undefined,
        gcRequest: 'dashboard',
      });
    });
    // mail-inbox / mail-other are already unread — bulk mark-unread must not
    // re-issue redundant writes for them.
    expect(fetchCalls.some((c) => c.url === '/v0/city/test-city/mail/mail-inbox/mark-unread')).toBe(
      false,
    );
    expect(fetchCalls.some((c) => c.url === '/v0/city/test-city/mail/mail-other/mark-unread')).toBe(
      false,
    );
  });

  it('updates the needs-you count after a bulk mark-read (same selector as the nav badge)', async () => {
    renderMailPage();
    await screen.findByText(/1 need you of 2 unread/);

    fireEvent.click(screen.getByRole('checkbox', { name: 'select all mail' }));
    fireEvent.click(screen.getByRole('button', { name: 'Mark read' }));

    // After both inbox rows are marked read, the re-fetched mailbox has no
    // unread — the needs-you synopsis collapses to the all-read line.
    expect(await screen.findByText(/all read/)).toBeTruthy();
  });

  it('offers no bulk selection in the sent box, which has no read-state concept', async () => {
    renderMailPage();
    fireEvent.click(await screen.findByRole('button', { name: 'Sent' }));
    await waitFor(() => {
      expect(screen.queryByRole('checkbox', { name: 'select all mail' })).toBeNull();
    });
  });

  it('disables the bulk mark actions in read-only mode', async () => {
    renderMailPage('/mail', { readOnly: true });
    await screen.findByText('direct supervisor inbox');

    fireEvent.click(screen.getByRole('checkbox', { name: 'select all mail' }));
    expect((screen.getByRole('button', { name: 'Mark read' }) as HTMLButtonElement).disabled).toBe(
      true,
    );
    expect(
      (screen.getByRole('button', { name: 'Mark unread' }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });
});

function renderMailPage(initialEntry = '/mail', options: { readOnly?: boolean } = {}) {
  render(
    <MemoryRouter
      initialEntries={[initialEntry]}
      future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
    >
      <NowProvider intervalMs={1_000_000}>
        <ReadOnlyProvider readOnly={options.readOnly ?? false}>
          <MailPage />
        </ReadOnlyProvider>
      </NowProvider>
    </MemoryRouter>,
  );
}

function contributor(
  domain: 'mail',
  items: ReturnType<AttentionContributor['getItems']>,
): AttentionContributor {
  return {
    id: `${domain}:test`,
    domain,
    getItems: () => items,
  };
}

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AgentsPage } from './Agents';
import { AttentionProvider } from '../attention/context';
import type { AttentionContributor } from '../attention/compose';
import { invalidate } from '../api/cache';
import { NowProvider } from '../contexts/NowContext';
import { ReadOnlyProvider } from '../contexts/ReadOnlyContext';
import { resetSupervisorApiForTests } from '../supervisor/client';

// Regression tests for two bugs that shipped with the ay6 Agents-view rewrite
// (PR #45, surfaced post-deploy):
//
// 1. Peek button errored with "invalid session id" because the modal passed
//    `agent.session.name` (a friendly alias like "mayor") to a route that
//    validates against SESSION_ID_RE (`gc-XXX` format). The fix maps
//    agent.session.name -> session.id through the sessions cache.
//
// 2. The agent name column rendered `display_name ?? name`, so the
//    Orchestration group showed "Claude (Account 5)" instead of "mayor".
//    The fix uses `name` (alias) as primary and pushes `display_name` to a
//    secondary line.

interface FetchCall {
  url: string;
  method: string;
  gcRequest: string | null;
  body: unknown;
}

const fetchCalls: FetchCall[] = [];

const fetchUrls = () => fetchCalls.map((call) => call.url);

interface StubFetchOptions {
  agentsPayload?: unknown;
  agentsStatus?: number;
}

// Minimal fetch stub that mimics the surface the AgentsPage hits: dashboard
// local agents plus direct supervisor sessions/transcript reads.
function stubFetch(options: StubFetchOptions = {}) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      const method = requestMethod(input, init);
      const gcRequest = requestHeader(input, init, 'X-GC-Request');
      const body = await requestBody(input, init);
      fetchCalls.push({ url, method, gcRequest, body });
      if (url === '/api/city/test-city/agents') {
        throw new Error('old dashboard agents roster route should not be called');
      }
      if (url === '/v0/city/test-city/agents' && method === 'GET') {
        return jsonResponse(
          options.agentsPayload ?? {
            items: [
              {
                name: 'mayor',
                available: true,
                running: true,
                suspended: false,
                state: 'idle',
                display_name: 'Claude (Account 5)',
                provider: 'claude-5',
                session: {
                  name: 'mayor',
                  attached: true,
                  last_activity: '2026-05-30T00:56:31Z',
                },
              },
              // ay6.2: orphan agent — configured roster entry with no
              // bound live session. AgentDetail will show "no session
              // matches" for these; the name-link in this view must
              // pre-empt the confusion with a distinct title tooltip.
              {
                name: 'control-dispatcher',
                available: true,
                running: false,
                suspended: false,
                state: 'asleep',
                display_name: 'Claude (Account 7)',
                provider: 'claude-5',
              },
            ],
            total: 2,
          },
          options.agentsStatus === undefined ? undefined : { status: options.agentsStatus },
        );
      }
      if (url === '/v0/city/test-city/sessions?limit=1000' && method === 'GET') {
        return jsonResponse({
          items: [
            {
              id: 'gc-2568',
              session_name: 'mayor',
              state: 'active',
              template: 'mayor',
              alias: 'mayor',
              provider: 'claude-5',
              running: true,
              attached: true,
              created_at: '2026-05-30T00:00:00Z',
              title: 'mayor',
            },
          ],
          total: 1,
        });
      }
      if (url === '/v0/city/test-city/session/gc-2568/pending' && method === 'GET') {
        return jsonResponse({
          supported: true,
          pending: {
            kind: 'tool_approval',
            prompt: 'Approve deployment?',
            request_id: 'req-1',
          },
        });
      }
      // The "Workers active" section reads sessions (+ beads as best-effort
      // context). None of the post-ay6 regression cases assert on it; the stub
      // sessions list carries only orchestration, so it renders its calm
      // "No workers active right now." empty state.
      if (url.startsWith('/v0/city/test-city/beads') && method === 'GET') {
        return jsonResponse({ items: [], total: 0 });
      }
      if (url === '/v0/city/test-city/session/gc-2568/respond' && method === 'POST') {
        return jsonResponse({ id: 'gc-2568', status: 'accepted' }, { status: 202 });
      }
      if (
        url === '/v0/city/test-city/session/gc-2568/transcript?format=conversation' &&
        method === 'GET'
      ) {
        return jsonResponse({
          id: 'gc-2568',
          template: 'mayor',
          provider: 'claude-5',
          format: 'conversation',
          turns: [{ role: 'assistant', text: 'mayor transcript snapshot' }],
        });
      }
      throw new Error(`unexpected fetch: ${url}`);
    }),
  );
  resetSupervisorApiForTests();
}

function requestUrl(input: RequestInfo | URL): string {
  const url =
    input instanceof Request ? input.url : input instanceof URL ? input.toString() : String(input);
  return stripSameOrigin(url);
}

function requestMethod(input: RequestInfo | URL, init: RequestInit | undefined): string {
  if (init?.method !== undefined) return init.method.toUpperCase();
  if (input instanceof Request) return input.method.toUpperCase();
  return 'GET';
}

function requestHeader(
  input: RequestInfo | URL,
  init: RequestInit | undefined,
  name: string,
): string | null {
  const initHeaders = init?.headers === undefined ? null : new Headers(init.headers);
  const inputHeaders = input instanceof Request ? input.headers : null;
  return initHeaders?.get(name) ?? inputHeaders?.get(name) ?? null;
}

async function requestBody(
  input: RequestInfo | URL,
  init: RequestInit | undefined,
): Promise<unknown> {
  if (init?.body !== undefined) return parseBody(init.body);
  if (input instanceof Request && input.method.toUpperCase() !== 'GET') {
    return input.clone().json();
  }
  return undefined;
}

function parseBody(body: BodyInit | null): unknown {
  if (typeof body === 'string') return JSON.parse(body);
  return body;
}

function stripSameOrigin(url: string): string {
  const origin = window.location.origin;
  return url.startsWith(origin) ? url.slice(origin.length) : url;
}

function jsonResponse(payload: unknown, init?: ResponseInit): Response {
  return new Response(JSON.stringify(payload), {
    status: init?.status ?? 200,
    headers: { 'content-type': 'application/json' },
  });
}

beforeEach(() => {
  fetchCalls.length = 0;
  invalidate('agents');
  invalidate('sessions');
  invalidate('beads:in-flight');
  stubFetch();
});

afterEach(() => {
  cleanup();
  resetSupervisorApiForTests();
  vi.unstubAllGlobals();
});

describe('AgentsPage (post-ay6 regressions)', () => {
  it('renders the alias as primary label and display_name as secondary (Orchestration shows "mayor", not "Claude (Account 5)")', async () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <NowProvider intervalMs={1_000_000}>
          <AgentsPage />
        </NowProvider>
      </MemoryRouter>,
    );

    // mayor has no rig (cross-rig orchestration) → the label is just the
    // alias, rendered as a Link.
    const mayorLink = await screen.findByRole('link', { name: /mayor/i });
    expect(mayorLink).toBeDefined();
    expect(mayorLink.textContent).toBe('mayor');
    expect(fetchUrls()).toContain('/v0/city/test-city/agents');
    expect(fetchUrls()).not.toContain('/api/city/test-city/agents');
    // display_name appears as secondary muted text — present but not the link.
    expect(screen.getByText('Claude (Account 5)')).toBeDefined();
  });

  it('boots with the running checkbox checked and renders in-rig agents as "rig · agent"', async () => {
    // gascity-dashboard-fgzf: the reverted simple view defaults to the
    // actively-running agents (running checkbox ON) and restores the
    // 'rig · agent' label format lost in the table refactor.
    vi.unstubAllGlobals();
    stubFetch({
      agentsPayload: {
        items: [
          {
            name: 'polecat-1',
            available: true,
            running: true,
            suspended: false,
            state: 'active',
            rig: 'gascity-packs',
            display_name: 'Claude (Account 5)',
            provider: 'claude-5',
            session: {
              name: 'polecat-1',
              attached: false,
              last_activity: '2026-05-30T00:56:31Z',
            },
          },
          // asleep, in-rig — hidden while the running checkbox is checked.
          {
            name: 'polecat-2',
            available: true,
            running: false,
            suspended: false,
            state: 'asleep',
            rig: 'gascity-packs',
            provider: 'claude-5',
          },
        ],
        total: 2,
      },
    });

    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <NowProvider intervalMs={1_000_000}>
          <AgentsPage />
        </NowProvider>
      </MemoryRouter>,
    );

    // The running checkbox is the only filter control, and it boots checked.
    const runningCheckbox = await screen.findByRole<HTMLInputElement>('checkbox', {
      name: /running/i,
    });
    expect(runningCheckbox.checked).toBe(true);

    // The running agent is shown with the restored 'rig · agent' label.
    const runningLink = await screen.findByRole('link', { name: /polecat-1/i });
    expect(runningLink.textContent).toBe('gascity-packs · polecat-1');

    // The asleep agent is hidden by the default-on running filter.
    expect(screen.queryByRole('link', { name: /polecat-2/i })).toBeNull();

    // Unchecking the box reveals the full roster (asleep agent appears).
    fireEvent.click(runningCheckbox);
    const sleepingLink = await screen.findByRole('link', { name: /polecat-2/i });
    expect(sleepingLink.textContent).toBe('gascity-packs · polecat-2');
  });

  it('renders a genuine supervisor agents failure as operator-safe unavailable copy', async () => {
    vi.unstubAllGlobals();
    stubFetch({
      agentsStatus: 503,
      agentsPayload: {
        title: 'Service Unavailable',
        status: 503,
        detail: 'supervisor unavailable',
      },
    });

    const { container } = render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <NowProvider intervalMs={1_000_000}>
          <AgentsPage />
        </NowProvider>
      </MemoryRouter>,
    );

    // Fail-safe invariant (R6/R15/R16): a genuine fetch failure must surface an
    // explicit unavailable state, never a false all-clear empty roster.
    expect(await screen.findByRole('alert')).toBeTruthy();
    expect(container.textContent).toContain('Agent roster unavailable.');
    expect(container.textContent).not.toContain('No agents configured.');
  });

  it('Peek resolves agent.session.name -> session.id via direct supervisor sessions and fetches the right transcript', async () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <NowProvider intervalMs={1_000_000}>
          <AgentsPage />
        </NowProvider>
      </MemoryRouter>,
    );

    // Wait for the row to load.
    await screen.findByRole('link', { name: /mayor/i });

    const peekButton = await screen.findByRole('button', { name: /peek/i });
    fireEvent.click(peekButton);

    // The peek modal must hit supervisor transcript for gc-2568 — NOT
    // dashboard mirror peek for mayor (the pre-fix bug). We wait for it
    // because the resolution is async (sessions cache).
    await waitFor(() => {
      expect(fetchUrls()).toContain(
        '/v0/city/test-city/session/gc-2568/transcript?format=conversation',
      );
    });
    // Belt-and-suspenders: assert the buggy URL was NEVER attempted.
    expect(fetchUrls()).toContain('/v0/city/test-city/sessions?limit=1000');
    expect(fetchUrls()).not.toContain('/api/city/test-city/sessions');
    expect(fetchUrls()).not.toContain('/api/city/test-city/sessions/gc-2568/peek');
    expect(fetchUrls()).not.toContain('/api/city/test-city/sessions/mayor/peek');
  });

  it('surfaces pending interactions and copies the attach command for the agent', async () => {
    const writeText = vi.fn(async () => undefined);
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    });

    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <NowProvider intervalMs={1_000_000}>
          <AgentsPage />
        </NowProvider>
      </MemoryRouter>,
    );

    expect(await screen.findByText('needs you')).toBeTruthy();
    // The prompt shows in both the "Needs you" section and the roster row.
    expect(within(screen.getByRole('table')).getByText('Approve deployment?')).toBeTruthy();
    expect(fetchUrls()).toContain('/v0/city/test-city/session/gc-2568/pending');

    fireEvent.click(screen.getByRole('button', { name: /copy attach/i }));

    await waitFor(() => {
      expect(writeText).toHaveBeenCalledWith('gc agent attach mayor');
    });
  });

  it('responds to pending interactions through the generated supervisor respond endpoint', async () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <NowProvider intervalMs={1_000_000}>
          <AgentsPage />
        </NowProvider>
      </MemoryRouter>,
    );

    expect(await screen.findByText('needs you')).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: /deny/i }));

    await screen.findByText('responded to mayor');

    expect(fetchCalls).toContainEqual({
      url: '/v0/city/test-city/session/gc-2568/respond',
      method: 'POST',
      gcRequest: 'dashboard',
      body: {
        action: 'deny',
        request_id: 'req-1',
      },
    });
    expect(fetchUrls()).not.toContain('/api/city/test-city/sessions/gc-2568/respond');
  });

  it('disables the pending Approve/Deny controls in read-only mode (gascity-dashboard-uzhr)', async () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <NowProvider intervalMs={1_000_000}>
          <ReadOnlyProvider readOnly={true}>
            <AgentsPage />
          </ReadOnlyProvider>
        </NowProvider>
      </MemoryRouter>,
    );

    const table = await screen.findByRole('table');
    expect(within(table).getByText('Approve deployment?')).toBeTruthy();

    const approve = screen.getByRole('button', { name: /approve/i }) as HTMLButtonElement;
    const deny = screen.getByRole('button', { name: /deny/i }) as HTMLButtonElement;
    expect(approve.disabled).toBe(true);
    expect(deny.disabled).toBe(true);
    expect(approve.getAttribute('title')).toBe('Read-only mode: mutations are disabled');
    // The affordance carries words, not just a dimmed control (DESIGN.md §States).
    expect(screen.getByText('Read-only')).toBeTruthy();

    // The disabled control must not reach the supervisor respond mutation.
    fireEvent.click(deny);
    expect(fetchUrls()).not.toContain('/v0/city/test-city/session/gc-2568/respond');
  });

  it('orphan agent name-link carries a different title tooltip than a session-bound one (ay6.2)', async () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <NowProvider intervalMs={1_000_000}>
          <AgentsPage />
        </NowProvider>
      </MemoryRouter>,
    );

    // The Agents view defaults to the 'running' chip, so the asleep
    // control-dispatcher orphan is hidden until we toggle it off to reveal
    // the full supervisor roster. Both rows then render their alias as a
    // Link, but the title must differ — session-bound agents promise a
    // real drilldown; orphan agents warn the detail page shows no session.
    await showAllAgents();
    // mayor also appears in the "Needs you" section (it has a pending ask), so
    // scope the roster-row tooltip assertions to the table.
    const roster = within(await screen.findByRole('table'));
    const mayorLink = await roster.findByRole('link', { name: /mayor/i });
    const orphanLink = await roster.findByRole('link', { name: /control-dispatcher/i });

    const mayorTitle = mayorLink.getAttribute('title') ?? '';
    const orphanTitle = orphanLink.getAttribute('title') ?? '';

    expect(mayorTitle.length).toBeGreaterThan(0);
    expect(orphanTitle.length).toBeGreaterThan(0);
    expect(orphanTitle).not.toBe(mayorTitle);
    // The orphan title must mention the missing-session situation so
    // the operator understands why the drilldown will be empty.
    expect(orphanTitle.toLowerCase()).toMatch(/not running|no live session|configured/);
  });

  it('marks rows that match composed agent attention without hiding other agents', async () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <NowProvider intervalMs={1_000_000}>
          <AttentionProvider
            contributors={[
              contributor('agents', [
                {
                  id: 'agents:control-dispatcher:idle',
                  domain: 'agents',
                  severity: 'watch',
                  title: 'control-dispatcher idle',
                },
              ]),
            ]}
          >
            <AgentsPage />
          </AttentionProvider>
        </NowProvider>
      </MemoryRouter>,
    );

    // Toggle off the default 'running' chip so the asleep orphan shows.
    await showAllAgents();
    // mayor also appears in the "Needs you" section; the row-marking assertion
    // is about the roster <tr>, so scope the lookup to the table.
    const roster = within(await screen.findByRole('table'));
    const orphanLink = await roster.findByRole('link', { name: /control-dispatcher/i });
    const mayorLink = await roster.findByRole('link', { name: /mayor/i });

    expect(orphanLink.closest('tr')?.getAttribute('data-attention-severity')).toBe('watch');
    expect(mayorLink.closest('tr')?.getAttribute('data-attention-severity')).toBeNull();
  });

  it('lists agents needing the operator with reason and next step, excluding calm agents', async () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <NowProvider intervalMs={1_000_000}>
          <AgentsPage />
        </NowProvider>
      </MemoryRouter>,
    );

    const section = within(await screen.findByRole('region', { name: /agents needing you/i }));
    // The header count is the same selectAgentsNeedingYou the nav badge counts;
    // only mayor (a pending ask) needs the operator here.
    expect(section.getByRole('heading', { name: /needs you \(1\)/i })).toBeTruthy();
    // Reason word (survives the greyscale test), the prompt, and the next step.
    expect(section.getByText('awaiting input')).toBeTruthy();
    expect(section.getByText('Approve deployment?')).toBeTruthy();
    expect(section.getByText('Respond to its prompt.')).toBeTruthy();
    // The asleep orphan is calm; it must never appear in the needs-you list.
    expect(section.queryByText(/control-dispatcher/i)).toBeNull();
  });
});

// The Agents view boots with the 'running' checkbox checked. Tests that
// assert on non-running agents (orphans/asleep) uncheck it to reveal the
// full roster, mirroring the operator toggling the checkbox.
async function showAllAgents(): Promise<void> {
  const runningCheckbox = await screen.findByRole('checkbox', { name: /running/i });
  fireEvent.click(runningCheckbox);
}

function contributor(
  domain: 'agents',
  items: ReturnType<AttentionContributor['getItems']>,
): AttentionContributor {
  return {
    id: `${domain}:test`,
    domain,
    getItems: () => items,
  };
}

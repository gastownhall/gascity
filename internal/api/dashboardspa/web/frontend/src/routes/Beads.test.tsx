import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { BeadsPage } from './Beads';
import { setActiveCity } from '../api/cityBase';
import { invalidate } from '../api/cache';
import { NowProvider } from '../contexts/NowContext';
import { OperatorConfigProvider } from '../contexts/OperatorConfigContext';
import type { SupervisorBead } from '../supervisor/beadReads';

const PROJECT = 'gascity';
const beadQueries: URLSearchParams[] = [];
const supervisorWrites: Array<{
  method: string;
  path: string;
  body?: unknown;
}> = [];

beforeEach(() => {
  setActiveCity('test-city');
  beadQueries.length = 0;
  supervisorWrites.length = 0;
  invalidate('beads:board:');
  invalidate('sessions');
  invalidate('agents');
  invalidate('rigs');
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = parsedUrl(input);
      const method = requestMethod(input, init);
      if (url.pathname === '/gc-supervisor/v0/city/test-city/beads' && method === 'GET') {
        beadQueries.push(url.searchParams);
        return jsonResponse(beadListPayload(url.searchParams.has('type') ? [] : [sampleBead()]));
      }
      if (url.pathname === '/gc-supervisor/v0/city/test-city/beads' && method === 'POST') {
        supervisorWrites.push({
          method,
          path: url.pathname,
          body: await requestJson(input, init),
        });
        return jsonResponse(
          {
            id: `${PROJECT}-0002`,
            title: 'Route failing work',
            status: 'open',
            priority: 0,
            issue_type: 'task',
            labels: [],
            created_at: '2026-01-01T01:00:00Z',
          },
          { status: 201 },
        );
      }
      if (url.pathname === '/gc-supervisor/v0/city/test-city/sling' && method === 'POST') {
        supervisorWrites.push({
          method,
          path: url.pathname,
          body: await requestJson(input, init),
        });
        return jsonResponse({ status: 'ok', bead: `${PROJECT}-0002`, target: 'mayor' });
      }
      if (
        url.pathname === '/gc-supervisor/v0/city/test-city/bead/gascity-0001' &&
        method === 'GET'
      ) {
        // The detail modal fetches the bead by id when it opens before the
        // board's full row is in state. Serve the full bead so the modal
        // renders deterministically regardless of load ordering.
        return jsonResponse(sampleBead());
      }
      if (
        url.pathname === '/gc-supervisor/v0/city/test-city/bead/gascity-0001' &&
        method === 'PATCH'
      ) {
        supervisorWrites.push({
          method,
          path: url.pathname,
          body: await requestJson(input, init),
        });
        return jsonResponse({ status: 'ok' });
      }
      if (
        url.pathname === '/gc-supervisor/v0/city/test-city/bead/gascity-0001/close' &&
        method === 'POST'
      ) {
        supervisorWrites.push({
          method,
          path: url.pathname,
          body: await requestJson(input, init),
        });
        return jsonResponse({ status: 'closed' });
      }
      if (
        url.pathname === '/gc-supervisor/v0/city/test-city/agent/mayor/nudge' &&
        method === 'POST'
      ) {
        supervisorWrites.push({ method, path: url.pathname });
        return jsonResponse({ status: 'ok' });
      }
      if (url.pathname === '/gc-supervisor/v0/city/test-city/sessions') {
        return jsonResponse({ items: [], total: 0 });
      }
      if (url.pathname === '/gc-supervisor/v0/city/test-city/agents') {
        // Agents report `rig` as filesystem PATHS (not names), and one runs in
        // a directory (gascity-main) that is not a registered rig — exactly the
        // shapes the dropdown must canonicalize away.
        return jsonResponse({
          items: [
            agent('mayor', '/home/ds/east'),
            agent('west/mechanic', '/home/ds/west'),
            agent('janitor', '/home/ds/gascity-main'),
          ],
          total: 3,
        });
      }
      if (url.pathname === '/gc-supervisor/v0/city/test-city/rigs') {
        return jsonResponse({
          items: [
            {
              name: 'east',
              path: '/home/ds/east',
              agent_count: 1,
              running_count: 1,
              suspended: false,
            },
            {
              name: 'west',
              path: '/home/ds/west',
              agent_count: 1,
              running_count: 1,
              suspended: false,
            },
          ],
          total: 2,
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

describe('BeadsPage', () => {
  it('renders the kanban board by default with no board/list compatibility switch', async () => {
    renderPage();

    await screen.findByRole('heading', { name: /^beads$/i });
    const board = await screen.findByRole('region', { name: PROJECT });

    expect(board).not.toBeNull();
    expect(screen.queryByRole('table')).toBeNull();
    expect(screen.queryByRole('radiogroup', { name: /view/i })).toBeNull();
    expect(screen.getByRole('button', { name: /new bead/i })).toBeTruthy();
  });

  it('requests direct supervisor current engineering beads without type fan-out or closed history', async () => {
    renderPage();

    await screen.findByText('Sample bead');

    expect(beadQueries.length).toBe(1);
    const [query] = beadQueries;
    expect(query?.get('limit')).toBe('1000');
    expect(query?.has('all')).toBe(false);
    expect(query?.has('type')).toBe(false);
    expect(query?.has('showAll')).toBe(false);
  });

  it('refetches with all=true when the operator activates the closed status control', async () => {
    renderPage();

    await screen.findByText('Sample bead');
    expect(beadQueries.length).toBe(1);
    for (const query of beadQueries) {
      expect(query.has('all')).toBe(false);
    }

    fireEvent.click(screen.getByRole('button', { name: /^closed$/i }));

    // Activating `closed` flips the data scope, forcing exactly one fresh
    // read that now carries all=true (closed beads included).
    await waitFor(() => expect(beadQueries.length).toBe(2));
    const closedQuery = beadQueries.at(-1);
    expect(closedQuery?.get('all')).toBe('true');
    expect(closedQuery?.has('type')).toBe(false);

    // Deactivating `closed` must revert to an open-only fetch — otherwise the
    // chip would read inactive while the board silently keeps scanning closed
    // history (the showClosed/chip desync this dual-state wiring must avoid).
    fireEvent.click(screen.getByRole('button', { name: /^closed$/i }));
    await waitFor(() => expect(beadQueries.length).toBe(3));
    const reopenedQuery = beadQueries.at(-1);
    expect(reopenedQuery?.has('all')).toBe(false);
    expect(reopenedQuery?.has('type')).toBe(false);
  });

  it('passes the selected rig to the generated supervisor bead query', async () => {
    renderPage();

    await screen.findByText('Sample bead');
    fireEvent.change(screen.getByLabelText(/rig filter/i), {
      target: { value: 'east' },
    });

    await waitFor(() => expect(beadQueries.length).toBe(2));
    const latestQuery = beadQueries.at(-1);
    expect(latestQuery?.get('rig')).toBe('east');
    expect(latestQuery?.has('all')).toBe(false);
    expect(latestQuery?.has('type')).toBe(false);
  });

  it('lists only real rig names in the rig dropdown — no filesystem paths or non-rig dirs', async () => {
    renderPage();

    await screen.findByText('Sample bead');
    const select = (await screen.findByLabelText(/rig filter/i)) as HTMLSelectElement;
    const optionLabels = Array.from(select.options).map((option) => option.textContent ?? '');

    expect(optionLabels).toEqual(['all rigs', 'east', 'west']);
    for (const label of optionLabels) {
      expect(label).not.toContain('/home/ds');
      expect(label).not.toContain('gascity-main');
    }
    // The option *values* (what the supervisor query receives) are rig names too.
    const optionValues = Array.from(select.options)
      .map((option) => option.value)
      .filter((value) => value.length > 0);
    expect(optionValues).toEqual(['east', 'west']);
  });

  it('deep-links to and selects a bead from the bead query param', async () => {
    renderPage('/beads?bead=gascity-0001');

    const selected = await screen.findByTitle('Select gascity-0001');
    expect(selected.getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByRole('heading', { name: 'Sample bead' })).toBeTruthy();
    expect(within(screen.getByRole('dialog')).getByText('gascity-0001')).toBeTruthy();
  });

  it('creates and slings a bead directly through the supervisor API', async () => {
    renderPage();

    await screen.findByText('Sample bead');
    fireEvent.click(screen.getByRole('button', { name: /new bead/i }));

    const dialog = await screen.findByRole('dialog');
    fireEvent.change(within(dialog).getByLabelText(/title/i), {
      target: { value: 'Route failing work' },
    });
    fireEvent.change(within(dialog).getByLabelText(/body/i), {
      target: { value: 'Please investigate the failed deployment.' },
    });
    fireEvent.click(within(dialog).getByRole('button', { name: /create and sling/i }));

    expect(await screen.findByText(/created gascity-0002 and slung to mayor/i)).toBeTruthy();
    expect(supervisorWrites).toEqual([
      {
        method: 'POST',
        path: '/gc-supervisor/v0/city/test-city/beads',
        body: {
          title: 'Route failing work',
          description: 'Please investigate the failed deployment.',
        },
      },
      {
        method: 'POST',
        path: '/gc-supervisor/v0/city/test-city/sling',
        body: {
          bead: 'gascity-0002',
          rig: 'east',
          target: 'mayor',
        },
      },
    ]);
  });

  it('closes and nudges beads directly through the supervisor API', async () => {
    renderPage('/beads?bead=gascity-0001');

    const detailDialog = await screen.findByRole('dialog');
    // No operator Claim affordance: the human is never a bead assignee
    // (gascity-dashboard-2j8e.8).
    expect(within(detailDialog).queryByRole('button', { name: /^claim$/i })).toBeNull();

    fireEvent.click(within(detailDialog).getByRole('button', { name: /^nudge$/i }));
    await screen.findByText(/nudged mayor/i);

    const closeButton = within(detailDialog)
      .getAllByRole('button', { name: /^close$/i })
      .find((button) => button.textContent?.trim() === 'Close');
    expect(closeButton).toBeTruthy();
    fireEvent.click(closeButton as HTMLButtonElement);

    const closeDialog = await screen.findByRole('heading', { name: /close gascity-0001/i });
    const modal = closeDialog.closest('[role="dialog"]');
    expect(modal).toBeTruthy();
    fireEvent.change(within(modal as HTMLElement).getByLabelText(/reason/i), {
      target: { value: '  verified done  ' },
    });
    fireEvent.click(within(modal as HTMLElement).getByRole('button', { name: /close bead/i }));

    await screen.findByText(/closed gascity-0001/i);
    await waitFor(() => {
      expect(supervisorWrites).toEqual([
        {
          method: 'POST',
          path: '/gc-supervisor/v0/city/test-city/agent/mayor/nudge',
        },
        {
          method: 'POST',
          path: '/gc-supervisor/v0/city/test-city/bead/gascity-0001/close',
          body: {
            reason: 'verified done',
          },
        },
      ]);
    });
  });
});

function renderPage(path = '/beads') {
  return render(
    <MemoryRouter
      initialEntries={[path]}
      future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
    >
      <NowProvider intervalMs={1_000_000}>
        <OperatorConfigProvider
          operator={{
            operatorAlias: 'stephanie',
            operatorWireAlias: 'human',
            decisionLabel: 'needs/stephanie',
          }}
        >
          <BeadsPage />
        </OperatorConfigProvider>
      </NowProvider>
    </MemoryRouter>,
  );
}

function jsonResponse(payload: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(payload), {
    status: init.status ?? 200,
    headers: { 'content-type': 'application/json' },
  });
}

function beadListPayload(items: ReadonlyArray<SupervisorBead>): {
  items: ReadonlyArray<SupervisorBead>;
  total: number;
} {
  return { items, total: items.length };
}

function sampleBead(): SupervisorBead {
  return {
    id: `${PROJECT}-0001`,
    title: 'Sample bead',
    // The board fetch returns full beads (description included). Carrying it
    // here lets the detail modal render from the pre-loaded row and skip the
    // by-id detail fetch (useBeadDetail's `description` freshness signal),
    // matching the production path.
    description: 'Sample bead description.',
    status: 'open',
    priority: 0,
    issue_type: 'task',
    assignee: 'mayor',
    labels: [],
    created_at: '2026-01-01T00:00:00Z',
  };
}

function agent(name: string, rig: string): unknown {
  return {
    name,
    display_name: name,
    rig,
    available: true,
    running: true,
    state: 'active',
    suspended: false,
  };
}

function parsedUrl(input: RequestInfo | URL): URL {
  const value =
    input instanceof Request ? input.url : input instanceof URL ? input.toString() : String(input);
  return new URL(value, window.location.origin);
}

function requestMethod(input: RequestInfo | URL, init?: RequestInit): string {
  if (input instanceof Request) return input.method;
  return init?.method ?? 'GET';
}

async function requestJson(input: RequestInfo | URL, init?: RequestInit): Promise<unknown> {
  if (input instanceof Request) {
    return input.clone().json() as Promise<unknown>;
  }
  const body = init?.body;
  if (typeof body !== 'string') return undefined;
  return JSON.parse(body) as unknown;
}

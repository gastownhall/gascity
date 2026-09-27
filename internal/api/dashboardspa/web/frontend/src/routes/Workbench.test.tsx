import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { WorkbenchPage } from './Workbench';
import { setActiveCity } from '../api/cityBase';
import { invalidate } from '../api/cache';
import { NowProvider } from '../contexts/NowContext';
import type { SupervisorBead } from '../supervisor/beadReads';

const PROJECT = 'gascity';

type StubMode =
  | { kind: 'ok'; beads: SupervisorBead[] }
  | { kind: 'list-error' }
  | { kind: 'pending' };

let stubMode: StubMode = { kind: 'ok', beads: [sampleBead()] };
let updateMode: 'ok' | 'reject' = 'ok';
let stubSessions: Array<Record<string, unknown>> = [];
const supervisorWrites: Array<{ method: string; path: string; body?: unknown }> = [];

function setStub(mode: StubMode) {
  stubMode = mode;
}

beforeEach(() => {
  setActiveCity('test-city');
  supervisorWrites.length = 0;
  updateMode = 'ok';
  stubSessions = [];
  setStub({ kind: 'ok', beads: [sampleBead()] });
  invalidate('workbench:queue:');
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = parsedUrl(input);
      const method = requestMethod(input, init);
      const beadMatch = /^\/v0\/city\/test-city\/bead\/([^/]+)$/.exec(url.pathname);
      if (method !== 'GET') {
        let capturedBody: unknown;
        if (input instanceof Request) {
          try {
            capturedBody = JSON.parse(await input.clone().text());
          } catch {
            capturedBody = undefined;
          }
        } else {
          capturedBody = parseBody(init?.body);
        }
        supervisorWrites.push({ method, path: url.pathname, body: capturedBody });
        if (/\/mail$/.test(url.pathname)) return jsonResponse({ id: 'm-1' });
        if (/\/sling$/.test(url.pathname)) return jsonResponse({ ok: true });
        if (beadMatch && updateMode === 'ok') return jsonResponse({ ok: true });
        if (beadMatch && updateMode === 'reject') {
          return jsonResponse({ error: 'update rejected' }, { status: 409 });
        }
        return jsonResponse({ error: 'unexpected write' }, { status: 405 });
      }
      if (url.pathname === '/v0/city/test-city/beads' && method === 'GET') {
        if (stubMode.kind === 'pending') return new Promise<Response>(() => {});
        if (stubMode.kind === 'list-error') {
          return jsonResponse({ error: 'store unavailable' }, { status: 500 });
        }
        return jsonResponse(beadListPayload(stubMode.beads));
      }
      if (url.pathname === '/v0/city/test-city/sessions' && method === 'GET') {
        return jsonResponse({ items: stubSessions, total: stubSessions.length });
      }
      if (/\/attempts\/diff$/.test(url.pathname) && method === 'GET') {
        return jsonResponse({
          body: {
            worktree: '/wt/s-active',
            state: 'ok',
            text: 'diff --git a/a.txt b/a.txt\n-old\n+new\n',
            truncated: false,
            binary: false,
            bytes: 42,
          },
        });
      }
      if (beadMatch) {
        const id = decodeURIComponent(beadMatch[1] ?? '');
        const bead =
          stubMode.kind === 'ok'
            ? stubMode.beads.find((candidate) => candidate.id === id)
            : undefined;
        if (bead) return jsonResponse(bead);
        return jsonResponse({ error: 'not found' }, { status: 404 });
      }
      throw new Error(`unexpected fetch: ${url.pathname}${url.search}`);
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('WorkbenchPage', () => {
  it('renders the work queue from real beads through the typed supervisor API', async () => {
    renderPage();

    await screen.findByRole('heading', { name: /^workbench$/i });
    expect(await screen.findByRole('list', { name: /work queue/i })).toBeTruthy();
    expect(await screen.findByText('Sample bead')).toBeTruthy();
  });

  it('keeps bead details and attempt controls together without covering them in a modal', async () => {
    renderPage();

    await screen.findByText('Sample bead');
    fireEvent.click(screen.getByTitle('Open gascity-0001'));

    const detail = await screen.findByRole('region', { name: /selected bead/i });
    expect(within(detail).getByText(`${PROJECT}-0001`)).toBeTruthy();
    expect(within(detail).getByText('Sample bead description.')).toBeTruthy();
    expect(within(detail).getByLabelText('Execution attempt')).toBeTruthy();
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('is read-only: renders no create, close, or claim controls and performs no writes', async () => {
    renderPage('/workbench?bead=gascity-0001');

    expect((await screen.findAllByText('Sample bead')).length).toBeGreaterThan(0);
    const detail = await screen.findByRole('region', { name: /selected bead/i });

    expect(screen.queryByRole('button', { name: /new bead/i })).toBeNull();
    expect(within(detail).queryByRole('button', { name: /^close$/i })).toBeNull();
    expect(within(detail).queryByRole('button', { name: /^claim$/i })).toBeNull();
    expect(supervisorWrites).toEqual([]);
  });

  it('covers the loading state while the queue fetch is in flight', async () => {
    setStub({ kind: 'pending' });
    renderPage();

    expect((await screen.findAllByText('Loading work queue.')).length).toBeGreaterThan(0);
  });

  it('covers the unavailable state when the bead store cannot be reached', async () => {
    setStub({ kind: 'list-error' });
    renderPage();

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toMatch(/unavailable/i);
    expect(screen.getByRole('button', { name: /retry/i })).toBeTruthy();
  });

  it('covers the unknown-bead state for a deep link to a bead that no longer exists', async () => {
    setStub({ kind: 'ok', beads: [] });
    renderPage('/workbench?bead=gascity-9999');

    const detail = await screen.findByRole('region', { name: /selected bead/i });
    expect(await within(detail).findByText(/resolved or removed/i)).toBeTruthy();
  });

  it('filters and orders the queue without changing the shared bead read', async () => {
    setStub({
      kind: 'ok',
      beads: [
        sampleBead(),
        {
          ...sampleBead(),
          id: 'gascity-0002',
          title: 'Other bead',
          priority: 0,
          status: 'closed',
        } as SupervisorBead,
      ],
    });
    renderPage();
    await screen.findByText('Other bead');
    fireEvent.change(screen.getByRole('searchbox', { name: /search workbench/i }), {
      target: { value: 'Sample' },
    });
    const queue = screen.getByRole('list', { name: /work queue/i });
    expect(within(queue).getByText('Sample bead')).toBeTruthy();
    expect(within(queue).queryByText('Other bead')).toBeNull();
    fireEvent.change(screen.getByRole('searchbox', { name: /search workbench/i }), {
      target: { value: '' },
    });
    fireEvent.change(screen.getByLabelText('Filter status'), { target: { value: 'closed' } });
    expect(within(queue).getByText('Other bead')).toBeTruthy();
    expect(within(queue).queryByText('Sample bead')).toBeNull();
  });

  it('groups work by the rig of its linked session without inventing an owner', async () => {
    setStub({
      kind: 'ok',
      beads: [sampleBead(), { ...sampleBead(), id: 'gascity-0002', title: 'Unlinked bead' }],
    });
    stubSessions = [
      {
        id: 's-old',
        session_name: 'worker-old',
        title: 'worker-old',
        template: 'worker',
        state: 'stopped',
        running: false,
        attached: false,
        provider: 'opencode',
        created_at: '2026-01-01T00:00:00Z',
        active_bead: 'gascity-0001',
        rig: 'legacy',
      },
      {
        id: 's-1',
        session_name: 'worker-1',
        title: 'worker-1',
        template: 'worker',
        state: 'active',
        running: true,
        attached: false,
        provider: 'opencode',
        created_at: '2026-01-02T00:00:00Z',
        active_bead: 'gascity-0001',
        rig: 'frontend',
      },
    ];
    renderPage();
    await screen.findByText('Sample bead');
    fireEvent.click(screen.getByRole('button', { name: /^rigs$/i }));
    expect(
      within(screen.getByRole('region', { name: 'Rig frontend' })).getByText('Sample bead'),
    ).toBeTruthy();
    expect(screen.queryByRole('region', { name: 'Rig legacy' })).toBeNull();
    expect(
      within(screen.getByRole('region', { name: 'No rig-linked session' })).getByText(
        'Unlinked bead',
      ),
    ).toBeTruthy();
  });

  it('shows only blocked or stale-session beads in Needs attention, respecting shared search', async () => {
    setStub({
      kind: 'ok',
      beads: [
        sampleBead(),
        { ...sampleBead(), id: 'gascity-0002', title: 'Blocked bead', status: 'blocked' },
        {
          ...sampleBead(),
          id: 'gascity-0003',
          title: 'Stale bead',
          metadata: { 'gc.session_id': 'missing' },
        },
      ],
    });
    renderPage();
    await screen.findByText('Sample bead');
    fireEvent.click(screen.getByRole('button', { name: /needs attention/i }));
    const attention = screen.getByRole('list', { name: /needs attention/i });
    expect(within(attention).getByText('Blocked bead')).toBeTruthy();
    expect(within(attention).getByText('Stale bead')).toBeTruthy();
    expect(within(attention).queryByText('Sample bead')).toBeNull();
    fireEvent.change(screen.getByRole('searchbox', { name: /search workbench/i }), {
      target: { value: 'Stale' },
    });
    expect(within(attention).queryByText('Blocked bead')).toBeNull();
    expect(within(attention).getByText('Stale bead')).toBeTruthy();
  });

  it('offers a keyboard and touch-friendly move control for board cards', async () => {
    renderPage();
    await screen.findByText('Sample bead');
    fireEvent.click(screen.getByRole('button', { name: /kanban/i }));
    fireEvent.change(screen.getByLabelText('Move gascity-0001 to status'), {
      target: { value: 'in_progress' },
    });
    await waitFor(() => expect(supervisorWrites[0]?.body).toEqual({ status: 'in_progress' }));
  });

  it('Kanban drag changes only the supported Bead status', async () => {
    renderPage();
    await screen.findByText('Sample bead');
    fireEvent.click(screen.getByRole('button', { name: /kanban/i }));

    const lane = await screen.findByTestId('lane-status:in_progress');
    fireEvent.drop(lane, { dataTransfer: dt(`${PROJECT}-0001`) });

    await waitFor(() => expect(supervisorWrites).toHaveLength(1));
    expect(supervisorWrites[0]?.method).not.toBe('GET');
    expect(supervisorWrites[0]?.path).toContain(`${PROJECT}-0001`);
    expect(supervisorWrites[0]?.body).toEqual({ status: 'in_progress' });
  });

  it('Priority drag changes only the supported Bead priority', async () => {
    renderPage();
    await screen.findByText('Sample bead');
    fireEvent.click(screen.getByRole('button', { name: /priority/i }));

    const lane = await screen.findByTestId('lane-priority:3');
    fireEvent.drop(lane, { dataTransfer: dt(`${PROJECT}-0001`) });

    await waitFor(() => expect(supervisorWrites).toHaveLength(1));
    expect(supervisorWrites[0]?.body).toEqual({ priority: 3 });
  });

  it('closing requires confirmation and cancellation leaves the Bead unchanged', async () => {
    renderPage();
    await screen.findByText('Sample bead');
    fireEvent.click(screen.getByRole('button', { name: /kanban/i }));

    fireEvent.click(await screen.findByRole('button', { name: /^close$/i }));
    const dialog = await screen.findByRole('alertdialog');
    expect(dialog).toBeTruthy();

    fireEvent.click(within(dialog).getByRole('button', { name: /cancel/i }));
    expect(supervisorWrites).toEqual([]);

    fireEvent.click(screen.getByRole('button', { name: /^close$/i }));
    fireEvent.click(
      within(await screen.findByRole('alertdialog')).getByRole('button', { name: /close bead/i }),
    );
    await waitFor(() => expect(supervisorWrites).toHaveLength(1));
    expect(supervisorWrites[0]?.body).toEqual({ status: 'closed' });
  });

  it('shows the active Execution Attempt terminal for the selected Bead', async () => {
    stubSessions = [
      {
        id: 's-active',
        template: 'worker',
        session_name: 'worker-1',
        title: 'worker-1',
        state: 'active',
        running: true,
        attached: false,
        provider: 'opencode',
        created_at: '2026-01-02T00:00:00Z',
        active_bead: `${PROJECT}-0001`,
        work_dir: '/wt/s-active',
      },
    ];
    renderPage('/workbench?bead=gascity-0001');

    expect(await screen.findByText(/current attempt/i)).toBeTruthy();
    expect(screen.getByText('worker-1')).toBeTruthy();
  });

  it('shows the read-only worktree diff for the attempt (gp-466)', async () => {
    stubSessions = [
      {
        id: 's-active',
        template: 'worker',
        session_name: 'worker-1',
        title: 'worker-1',
        state: 'active',
        running: true,
        attached: false,
        provider: 'opencode',
        created_at: '2026-01-02T00:00:00Z',
        active_bead: `${PROJECT}-0001`,
        work_dir: '/wt/s-active',
      },
    ];
    renderPage('/workbench?bead=gascity-0001');

    const diff = await screen.findByLabelText('Attempt diff');
    expect(diff.textContent).toContain('+new');
    // Viewing the diff mutates nothing: no bead/worktree write is issued.
    expect(supervisorWrites.filter((w) => w.path.includes('/bead/'))).toEqual([]);
  });

  it('lets the operator inspect a prior attempt without offering live controls on it', async () => {
    stubSessions = [
      {
        id: 's-active',
        session_name: 'worker-1',
        state: 'active',
        running: true,
        active_bead: 'gascity-0001',
        created_at: '2026-01-02T00:00:00Z',
      },
      {
        id: 's-old',
        session_name: 'worker-old',
        state: 'completed',
        running: false,
        active_bead: 'gascity-0001',
        created_at: '2026-01-01T00:00:00Z',
      },
    ];
    renderPage('/workbench?bead=gascity-0001');
    fireEvent.click(await screen.findByRole('button', { name: /inspect worker-old/i }));
    const panel = screen.getByLabelText('Execution attempt');
    expect(within(panel).getAllByText(/worker-old/).length).toBeGreaterThan(0);
    expect(within(panel).queryByRole('button', { name: /send/i })).toBeNull();
  });

  it('does not offer PR actions without a Gas City queue, policy and conflict verdict', async () => {
    stubSessions = [
      {
        id: 's-active',
        session_name: 'worker-1',
        state: 'active',
        running: true,
        active_bead: 'gascity-0001',
        created_at: '2026-01-02T00:00:00Z',
      },
    ];
    renderPage('/workbench?bead=gascity-0001');
    const actions = await screen.findByLabelText('Pull request actions');
    expect(actions.textContent).toMatch(/verdicts/i);
    expect(within(actions).queryByRole('button', { name: /prepare pr|queue pr/i })).toBeNull();
    expect(supervisorWrites.some((write) => write.path.endsWith('/mail'))).toBe(false);
  });

  it('does not claim session delivery merely because mail was accepted (gp-bod)', async () => {
    stubSessions = [
      {
        id: 's-active',
        template: 'worker',
        session_name: 'worker-1',
        title: 'worker-1',
        state: 'active',
        running: true,
        attached: false,
        provider: 'opencode',
        created_at: '2026-01-02T00:00:00Z',
        active_bead: `${PROJECT}-0001`,
        work_dir: '/wt/s-active',
      },
    ];
    renderPage('/workbench?bead=gascity-0001');

    const input = await screen.findByLabelText('Message');
    fireEvent.change(input, { target: { value: 'please continue' } });
    fireEvent.click(screen.getByRole('button', { name: /send/i }));

    const queue = await screen.findByLabelText('Queued messages');
    expect(queue.textContent).toContain('please continue');
    await waitFor(() => expect(queue.textContent).toContain('awaiting session acknowledgement'));
    expect(queue.textContent).not.toContain('delivered');
  });

  it('offers an explicit start action when a Bead has no active attempt (gp-w3q)', async () => {
    setStub({
      kind: 'ok',
      beads: [
        {
          ...sampleBead(),
          status: 'open',
          metadata: { 'gc.routed_to': 'worker' },
        } as unknown as SupervisorBead,
      ],
    });
    renderPage('/workbench?bead=gascity-0001');

    const start = await screen.findByRole('button', { name: /start new attempt/i });
    fireEvent.click(start);
    fireEvent.click(start); // second click while in flight is ignored (idempotent)
    await waitFor(() => expect(supervisorWrites.some((w) => w.path.endsWith('/sling'))).toBe(true));
    await waitFor(() =>
      expect(supervisorWrites.filter((w) => w.path.endsWith('/sling'))).toHaveLength(1),
    );
  });

  it('reverts the optimistic move and surfaces a server rejection', async () => {
    updateMode = 'reject';
    renderPage();
    await screen.findByText('Sample bead');
    fireEvent.click(screen.getByRole('button', { name: /kanban/i }));

    const lane = await screen.findByTestId('lane-status:blocked');
    fireEvent.drop(lane, { dataTransfer: dt(`${PROJECT}-0001`) });

    expect(await screen.findByText(/update rejected/i)).toBeTruthy();
    // The Bead reconverges on the authoritative read: it stays in its open lane.
    await waitFor(() =>
      expect(screen.getByTestId('lane-status:open').textContent).toContain('Sample bead'),
    );
  });
});

function dt(id: string): DataTransfer {
  const store = new Map<string, string>([['text/bead-id', id]]);
  return {
    getData: (key: string) => store.get(key) ?? '',
    setData: (key: string, value: string) => {
      store.set(key, value);
    },
  } as unknown as DataTransfer;
}

function parseBody(body: BodyInit | null | undefined): unknown {
  if (typeof body !== 'string') return undefined;
  try {
    return JSON.parse(body);
  } catch {
    return body;
  }
}

function renderPage(path = '/workbench') {
  return render(
    <MemoryRouter
      initialEntries={[path]}
      future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
    >
      <NowProvider intervalMs={1_000_000}>
        <WorkbenchPage />
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
    description: 'Sample bead description.',
    status: 'open',
    priority: 0,
    issue_type: 'task',
    assignee: 'mayor',
    labels: [],
    created_at: '2026-01-01T00:00:00Z',
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

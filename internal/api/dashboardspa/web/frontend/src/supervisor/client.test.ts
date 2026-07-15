import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  GC_MUTATION_HEADERS,
  SUPERVISOR_PROXY_BASE_URL,
  SUPERVISOR_REQUEST_TIMEOUT_MS,
  createSupervisorApi,
  resetSupervisorApiForTests,
  setSupervisorApiForTests,
  supervisorApi,
  supervisorApiForRequestBudget,
} from './client';

describe('supervisor client wrapper', () => {
  afterEach(() => {
    resetSupervisorApiForTests();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it('defaults to the current origin under the same-origin contract', () => {
    // Same-origin deploy (gascity-dashboard-soo): the supervisor serves the SPA
    // and the typed API on one listener, so the default base is location.origin
    // (the jsdom test origin here), not the retired `/gc-supervisor` proxy
    // prefix. SUPERVISOR_PROXY_BASE_URL is now the empty SSR/no-DOM fallback.
    expect(createSupervisorApi().baseUrl).toBe(globalThis.location.origin);
    expect(SUPERVISOR_PROXY_BASE_URL).toBe('');
  });

  it('keeps the default request timeout long enough for slow workflow snapshots', () => {
    expect(SUPERVISOR_REQUEST_TIMEOUT_MS).toBe(60_000);
  });

  it('caches generated supervisor clients by explicit request budget', () => {
    const fast = supervisorApiForRequestBudget(2_500);
    expect(supervisorApiForRequestBudget(2_500)).toBe(fast);
    expect(supervisorApiForRequestBudget(5_000)).not.toBe(fast);
  });

  it('honors the test supervisor override for explicit request budgets', () => {
    const injected = createSupervisorApi({ baseUrl: 'http://gc-supervisor.test' });
    setSupervisorApiForTests(injected);

    expect(supervisorApiForRequestBudget(2_500)).toBe(injected);
  });

  it('calls supervisor health through the generated SDK', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            cities_running: 1,
            cities_total: 2,
            status: 'ok',
            uptime_sec: 42,
            version: '1.4.2',
          }),
          {
            status: 200,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });

    await expect(api.health()).resolves.toMatchObject({
      status: 'ok',
      cities_total: 2,
    });
    expect(requestedUrl(fetchSpy.mock.calls[0]?.[0])).toBe('http://gc-supervisor.test/health');
  });

  it('calls supervisor cities through the generated SDK without dashboard DTO stripping', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            items: [{ name: 'test-city', path: '/srv/gc/test-city', running: true }],
            total: 1,
          }),
          {
            status: 200,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });

    await expect(api.listCities()).resolves.toMatchObject({
      items: [{ name: 'test-city', path: '/srv/gc/test-city', running: true }],
      total: 1,
    });
    expect(requestedUrl(fetchSpy.mock.calls[0]?.[0])).toBe('http://gc-supervisor.test/v0/cities');
  });

  it('calls city-scoped supervisor health through the generated SDK', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            city: 'test-city',
            status: 'ok',
            uptime_sec: 42,
            version: '1.4.2',
          }),
          {
            status: 200,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });

    await expect(api.cityHealth('test-city')).resolves.toMatchObject({
      city: 'test-city',
      status: 'ok',
    });
    expect(requestedUrl(fetchSpy.mock.calls[0]?.[0])).toBe(
      'http://gc-supervisor.test/v0/city/test-city/health',
    );
  });

  it('calls city-scoped supervisor status through the generated SDK', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            agent_count: 1,
            agents: { quarantined: 0, running: 1, suspended: 0, total: 1 },
            mail: { total: 3, unread: 1 },
            name: 'test-city',
            path: '/srv/gc/test-city',
            rig_count: 1,
            rigs: { suspended: 0, total: 1 },
            running: 1,
            suspended: false,
            uptime_sec: 42,
            version: '1.4.2',
            work: { open: 5, ready: 2, in_progress: 1 },
          }),
          {
            status: 200,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });

    await expect(api.cityStatus('test-city')).resolves.toMatchObject({
      name: 'test-city',
      version: '1.4.2',
    });
    expect(requestedUrl(fetchSpy.mock.calls[0]?.[0])).toBe(
      'http://gc-supervisor.test/v0/city/test-city/status',
    );
  });

  it('calls city-scoped usage and canonical runs through the generated SDK', async () => {
    const fetchSpy = vi.fn(async (input: RequestInfo | URL) => {
      const url = requestedUrl(input);
      if (new URL(url).pathname.endsWith('/usage')) {
        return new Response(
          JSON.stringify({
            available: true,
            recording: true,
            source: 'local_estimate',
            today: { invocations: 4, input_tokens: 100, output_tokens: 20 },
            recent: { invocations: 1, input_tokens: 25, output_tokens: 5 },
            recent_window_secs: 300,
            updated_at: '2026-07-14T12:00:00Z',
          }),
          { status: 200, headers: { 'content-type': 'application/json' } },
        );
      }
      return new Response(
        JSON.stringify({
          status_counts: {
            pending: 0,
            active: 1,
            waiting: 0,
            canceling: 0,
            completed: 0,
            failed: 0,
            canceled: 0,
            skipped: 0,
          },
        }),
        { status: 200, headers: { 'content-type': 'application/json' } },
      );
    });
    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });

    await expect(api.cityUsage('test-city')).resolves.toMatchObject({
      available: true,
      today: { invocations: 4 },
    });
    await expect(api.runCensus('test-city')).resolves.toMatchObject({
      status_counts: { active: 1 },
    });
    expect(fetchSpy.mock.calls.map((call) => requestedUrl(call[0]))).toEqual([
      'http://gc-supervisor.test/v0/city/test-city/usage?aggregate_only=true',
      'http://gc-supervisor.test/v0/city/test-city/runs/census',
    ]);
  });

  it('calls supervisor sessions through the generated SDK without dashboard DTO stripping', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            items: [
              {
                id: 'gc-session-1',
                template: 'mayor',
                title: 'mayor',
                provider: 'claude',
                session_name: 'mayor',
                state: 'active',
                created_at: '2026-06-01T00:00:00Z',
                attached: false,
                running: true,
                alias: 'mayor',
              },
            ],
            total: 1,
          }),
          {
            status: 200,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });

    await expect(api.listSessions('test-city')).resolves.toMatchObject({
      items: [{ id: 'gc-session-1', session_name: 'mayor' }],
      total: 1,
    });
    expect(requestedUrl(fetchSpy.mock.calls[0]?.[0])).toBe(
      'http://gc-supervisor.test/v0/city/test-city/sessions',
    );
  });

  it('calls supervisor session pending interaction through the generated SDK', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            supported: true,
            pending: {
              kind: 'tool_approval',
              prompt: 'Approve deployment?',
              request_id: 'req-1',
            },
          }),
          {
            status: 200,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });

    await expect(api.sessionPending('test-city', 'gc-2568')).resolves.toMatchObject({
      supported: true,
      pending: { request_id: 'req-1' },
    });
    expect(requestedUrl(fetchSpy.mock.calls[0]?.[0])).toBe(
      'http://gc-supervisor.test/v0/city/test-city/session/gc-2568/pending',
    );
  });

  it('responds to supervisor session pending interactions through the generated SDK with mutation headers', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            id: 'gc-2568',
            status: 'accepted',
          }),
          {
            status: 202,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    }) as ReturnType<typeof createSupervisorApi> & {
      respondSession(
        cityName: string,
        sessionId: string,
        body: { action: string; request_id?: string },
      ): Promise<unknown>;
    };

    await expect(
      api.respondSession('test-city', 'gc-2568', {
        action: 'deny',
        request_id: 'req-1',
      }),
    ).resolves.toMatchObject({ id: 'gc-2568', status: 'accepted' });

    const req = fetchSpy.mock.calls[0]?.[0];
    expect(requestedUrl(req)).toBe(
      'http://gc-supervisor.test/v0/city/test-city/session/gc-2568/respond',
    );
    expect(req).toBeInstanceOf(Request);
    const request = req as Request;
    expect(request.method).toBe('POST');
    expect(request.headers.get('X-GC-Request')).toBe('dashboard');
    await expect(request.json()).resolves.toEqual({
      action: 'deny',
      request_id: 'req-1',
    });
  });

  it('calls supervisor agents through the generated SDK without dashboard DTO stripping', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            items: [
              {
                name: 'mayor',
                available: true,
                running: true,
                suspended: false,
                state: 'idle',
                provider: 'claude',
                session: {
                  name: 'mayor',
                  attached: true,
                  last_activity: '2026-06-01T00:00:00Z',
                },
              },
            ],
            total: 1,
          }),
          {
            status: 200,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });
    const agentApi = api as typeof api & {
      listAgents(cityName: string): Promise<unknown>;
    };

    await expect(agentApi.listAgents('test-city')).resolves.toMatchObject({
      items: [{ name: 'mayor', session: { name: 'mayor' } }],
      total: 1,
    });
    expect(requestedUrl(fetchSpy.mock.calls[0]?.[0])).toBe(
      'http://gc-supervisor.test/v0/city/test-city/agents',
    );
  });

  it('accepts RFC3339 offset datetimes from supervisor agent sessions', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            items: [
              {
                name: 'control-dispatcher',
                available: true,
                running: true,
                suspended: false,
                state: 'idle',
                provider: 'codex',
                session: {
                  name: 'control-dispatcher',
                  attached: false,
                  last_activity: '2026-05-24T16:41:12-07:00',
                },
              },
            ],
            total: 1,
          }),
          {
            status: 200,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });

    await expect(api.listAgents('test-city')).resolves.toMatchObject({
      items: [
        {
          name: 'control-dispatcher',
          session: { last_activity: '2026-05-24T16:41:12-07:00' },
        },
      ],
      total: 1,
    });
  });

  it('calls supervisor beads through the generated SDK without dashboard DTO stripping', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            items: [
              {
                id: 'td-bead-abc123',
                title: 'wire beads directly',
                status: 'open',
                issue_type: 'task',
                created_at: '2026-06-01T00:00:00Z',
                labels: ['needs-review'],
              },
            ],
            total: 1,
          }),
          {
            status: 200,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });
    const beadApi = api as typeof api & {
      listBeads(cityName: string, params?: { limit?: number }): Promise<unknown>;
    };

    await expect(beadApi.listBeads('test-city', { limit: 1000 })).resolves.toMatchObject({
      items: [{ id: 'td-bead-abc123', title: 'wire beads directly' }],
      total: 1,
    });
    expect(requestedUrl(fetchSpy.mock.calls[0]?.[0])).toBe(
      'http://gc-supervisor.test/v0/city/test-city/beads?limit=1000',
    );
  });

  it('calls supervisor bead detail through the generated SDK without dashboard DTO stripping', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            id: 'td-bead-abc123',
            title: 'wire bead detail directly',
            status: 'open',
            issue_type: 'task',
            created_at: '2026-06-01T00:00:00Z',
            priority: 1,
          }),
          {
            status: 200,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });
    const beadApi = api as typeof api & {
      getBead(cityName: string, id: string): Promise<unknown>;
    };

    await expect(beadApi.getBead('test-city', 'td-bead-abc123')).resolves.toMatchObject({
      id: 'td-bead-abc123',
      title: 'wire bead detail directly',
    });
    expect(requestedUrl(fetchSpy.mock.calls[0]?.[0])).toBe(
      'http://gc-supervisor.test/v0/city/test-city/bead/td-bead-abc123',
    );
  });

  it('updates supervisor beads through the generated SDK with mutation headers', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(JSON.stringify({ status: 'ok' }), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        }),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });
    const writeApi = api as typeof api & {
      updateBead(
        cityName: string,
        id: string,
        body: { status?: string; assignee?: string },
      ): Promise<unknown>;
    };

    await expect(
      writeApi.updateBead('test-city', 'td-bead-abc123', {
        status: 'in_progress',
        assignee: 'stephanie',
      }),
    ).resolves.toMatchObject({ status: 'ok' });

    const req = fetchSpy.mock.calls[0]?.[0];
    expect(requestedUrl(req)).toBe(
      'http://gc-supervisor.test/v0/city/test-city/bead/td-bead-abc123',
    );
    expect(req).toBeInstanceOf(Request);
    const request = req as Request;
    expect(request.method).toBe('PATCH');
    expect(request.headers.get('X-GC-Request')).toBe('dashboard');
    await expect(request.json()).resolves.toEqual({
      status: 'in_progress',
      assignee: 'stephanie',
    });
  });

  it('closes supervisor beads through the generated SDK with mutation headers', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(JSON.stringify({ status: 'closed' }), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        }),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });

    await expect(api.closeBead('test-city', 'td-bead-abc123')).resolves.toMatchObject({
      status: 'closed',
    });

    const req = fetchSpy.mock.calls[0]?.[0];
    expect(requestedUrl(req)).toBe(
      'http://gc-supervisor.test/v0/city/test-city/bead/td-bead-abc123/close',
    );
    expect(req).toBeInstanceOf(Request);
    const request = req as Request;
    expect(request.method).toBe('POST');
    expect(request.headers.get('X-GC-Request')).toBe('dashboard');
    await expect(request.text()).resolves.toBe('');
  });

  it('creates supervisor beads through the generated SDK with mutation headers', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            id: 'td-new-1',
            title: 'Route failing work',
            status: 'open',
            issue_type: 'task',
            created_at: '2026-06-01T00:00:00Z',
          }),
          {
            status: 201,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });

    await expect(
      api.createBead('test-city', {
        title: 'Route failing work',
        description: 'Please investigate the failed deployment.',
      }),
    ).resolves.toMatchObject({ id: 'td-new-1' });

    const req = fetchSpy.mock.calls[0]?.[0];
    expect(requestedUrl(req)).toBe('http://gc-supervisor.test/v0/city/test-city/beads');
    expect(req).toBeInstanceOf(Request);
    const request = req as Request;
    expect(request.method).toBe('POST');
    expect(request.headers.get('X-GC-Request')).toBe('dashboard');
    await expect(request.json()).resolves.toEqual({
      title: 'Route failing work',
      description: 'Please investigate the failed deployment.',
    });
  });

  it('slings supervisor beads through the generated SDK with mutation headers', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            status: 'ok',
            bead: 'td-new-1',
            target: 'mayor',
          }),
          {
            status: 200,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });

    await expect(
      api.sling('test-city', {
        bead: 'td-new-1',
        rig: 'east',
        target: 'mayor',
      }),
    ).resolves.toMatchObject({ status: 'ok', target: 'mayor' });

    const req = fetchSpy.mock.calls[0]?.[0];
    expect(requestedUrl(req)).toBe('http://gc-supervisor.test/v0/city/test-city/sling');
    expect(req).toBeInstanceOf(Request);
    const request = req as Request;
    expect(request.method).toBe('POST');
    expect(request.headers.get('X-GC-Request')).toBe('dashboard');
    await expect(request.json()).resolves.toEqual({
      bead: 'td-new-1',
      rig: 'east',
      target: 'mayor',
    });
  });

  it('calls supervisor mail through the generated SDK without dashboard DTO stripping', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            items: [
              {
                id: 'mail-1',
                from: 'mayor',
                to: 'human',
                subject: 'direct mail reads',
                body: 'body',
                created_at: '2026-06-01T00:00:00Z',
                read: false,
                thread_id: 'thread-1',
              },
            ],
            total: 1,
          }),
          {
            status: 200,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });
    const mailApi = api as typeof api & {
      listMail(cityName: string, params?: { limit?: number }): Promise<unknown>;
    };

    await expect(mailApi.listMail('test-city', { limit: 1000 })).resolves.toMatchObject({
      items: [{ id: 'mail-1', subject: 'direct mail reads' }],
      total: 1,
    });
    expect(requestedUrl(fetchSpy.mock.calls[0]?.[0])).toBe(
      'http://gc-supervisor.test/v0/city/test-city/mail?limit=1000',
    );
  });

  it('sends supervisor mail through the generated SDK with mutation headers', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            id: 'mail-new',
            from: 'human',
            to: 'mayor',
            subject: 'status',
            body: 'all green',
            created_at: '2026-06-01T00:00:00Z',
            read: false,
            thread_id: 'thread-new',
          }),
          {
            status: 201,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });
    const mailApi = api as typeof api & {
      sendMail(
        cityName: string,
        body: { to: string; subject: string; body: string; from: string },
      ): Promise<unknown>;
    };

    await expect(
      mailApi.sendMail('test-city', {
        to: 'mayor',
        subject: 'status',
        body: 'all green',
        from: 'human',
      }),
    ).resolves.toMatchObject({ id: 'mail-new' });

    const req = fetchSpy.mock.calls[0]?.[0];
    expect(requestedUrl(req)).toBe('http://gc-supervisor.test/v0/city/test-city/mail');
    expect(req).toBeInstanceOf(Request);
    const request = req as Request;
    expect(request.method).toBe('POST');
    expect(request.headers.get('X-GC-Request')).toBe('dashboard');
    await expect(request.json()).resolves.toEqual({
      to: 'mayor',
      subject: 'status',
      body: 'all green',
      from: 'human',
    });
  });

  it('calls supervisor mail thread through the generated SDK without dashboard DTO stripping', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            items: [
              {
                id: 'mail-1',
                from: 'mayor',
                to: 'human',
                subject: 'thread read',
                body: 'body',
                created_at: '2026-06-01T00:00:00Z',
                read: false,
                thread_id: 'thread-1',
              },
            ],
            total: 1,
          }),
          {
            status: 200,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });
    const mailApi = api as typeof api & {
      mailThread(cityName: string, threadId: string): Promise<unknown>;
    };

    await expect(mailApi.mailThread('test-city', 'thread-1')).resolves.toMatchObject({
      items: [{ id: 'mail-1', subject: 'thread read' }],
      total: 1,
    });
    expect(requestedUrl(fetchSpy.mock.calls[0]?.[0])).toBe(
      'http://gc-supervisor.test/v0/city/test-city/mail/thread/thread-1',
    );
  });

  it('builds direct supervisor city event stream URLs', () => {
    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: vi.fn() as typeof fetch,
    });
    const eventsApi = api as typeof api & {
      cityEventStreamUrl(cityName: string): string;
    };

    expect(eventsApi.cityEventStreamUrl('test-city')).toBe(
      'http://gc-supervisor.test/v0/city/test-city/events/stream',
    );
  });

  it('calls supervisor event history through the generated SDK', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            items: [
              {
                actor: 'supervisor',
                message: 'session crashed',
                payload: {
                  reason: 'panic',
                  session_id: 'gc-session-1',
                  template: 'mayor',
                },
                seq: 42,
                subject: 'gc-session-1',
                ts: '2026-06-01T00:00:00Z',
                type: 'session.crashed',
              },
            ],
            total: 1,
          }),
          {
            status: 200,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );
    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });
    const eventsApi = api as typeof api & {
      listEvents(cityName: string, query?: { limit?: number; since?: string }): Promise<unknown>;
    };

    await expect(
      eventsApi.listEvents('test-city', { limit: 100, since: '24h' }),
    ).resolves.toMatchObject({
      items: [{ type: 'session.crashed', seq: 42 }],
    });
    expect(requestedUrl(fetchSpy.mock.calls[0]?.[0])).toBe(
      'http://gc-supervisor.test/v0/city/test-city/events?limit=100&since=24h',
    );
  });

  it('builds direct supervisor session stream URLs', () => {
    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: vi.fn() as typeof fetch,
    });
    const streamApi = api as typeof api & {
      sessionStreamUrl(cityName: string, sessionId: string): string;
    };

    expect(streamApi.sessionStreamUrl('test-city', 'gc-session-1')).toBe(
      'http://gc-supervisor.test/v0/city/test-city/session/gc-session-1/stream',
    );
  });

  it('calls supervisor session transcripts through the generated SDK', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            id: 'gc-session-1',
            template: 'mayor',
            provider: 'claude',
            format: 'conversation',
            turns: [{ role: 'assistant', text: 'hello' }],
          }),
          {
            status: 200,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });

    await expect(api.sessionTranscript('test-city', 'gc-session-1')).resolves.toMatchObject({
      id: 'gc-session-1',
      turns: [{ role: 'assistant', text: 'hello' }],
    });
    expect(requestedUrl(fetchSpy.mock.calls[0]?.[0])).toBe(
      'http://gc-supervisor.test/v0/city/test-city/session/gc-session-1/transcript?format=conversation',
    );
  });

  it('calls supervisor workflow snapshots through the generated SDK with scope query params', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            workflow_id: 'gc-run-1',
            root_bead_id: 'gc-run-1',
            root_store_ref: 'city:test-city',
            resolved_root_store: 'city:test-city',
            scope_kind: 'city',
            scope_ref: 'test-city',
            snapshot_version: 3,
            snapshot_event_seq: 7,
            partial: false,
            stores_scanned: ['city:test-city'],
            beads: [
              {
                id: 'gc-run-1',
                title: 'direct workflow run',
                status: 'in_progress',
                kind: 'workflow',
                metadata: {
                  'gc.kind': 'workflow',
                  'gc.formula_contract': 'graph.v2',
                  'gc.formula': 'mol-direct',
                  'gc.run_target': 'test-city/codex',
                },
              },
            ],
            deps: [],
            logical_nodes: [],
            logical_edges: [],
            scope_groups: [],
          }),
          {
            status: 200,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });

    await expect(
      api.workflowRun('test-city', 'gc-run-1', {
        scope_kind: 'city',
        scope_ref: 'test-city',
      }),
    ).resolves.toMatchObject({
      workflow_id: 'gc-run-1',
      root_bead_id: 'gc-run-1',
      snapshot_version: 3,
    });
    expect(requestedUrl(fetchSpy.mock.calls[0]?.[0])).toBe(
      'http://gc-supervisor.test/v0/city/test-city/workflow/gc-run-1?scope_kind=city&scope_ref=test-city',
    );
  });

  it('bounds generated supervisor calls when the upstream request never resolves', async () => {
    vi.useFakeTimers();
    const fetchSpy = vi.fn((_input: Request) => new Promise<Response>(() => undefined));
    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
      timeoutMs: 25,
    });

    const request = api.workflowRun('test-city', 'gc-run-1');
    const expectation = expect(request).rejects.toMatchObject({
      message: 'gc supervisor request timed out after 25ms',
      status: undefined,
    });
    await vi.advanceTimersByTimeAsync(25);

    await expectation;
    const fetchRequest = fetchSpy.mock.calls[0]?.[0] as Request | undefined;
    expect(fetchRequest?.signal.aborted).toBe(true);
  });

  it('calls supervisor formula detail through the generated SDK with target and scope query params', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            name: 'mol-direct',
            description: 'direct formula detail',
            version: 'v1',
            preview: {
              nodes: [{ id: 'root', title: 'Root', kind: 'step' }],
              edges: [],
            },
            steps: [],
            deps: [],
            var_defs: [],
          }),
          {
            status: 200,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });

    await expect(
      api.formulaDetail('test-city', 'mol-direct', {
        target: 'test-city/codex',
        scope_kind: 'city',
        scope_ref: 'test-city',
      }),
    ).resolves.toMatchObject({
      name: 'mol-direct',
      preview: { nodes: [{ id: 'root' }] },
    });
    expect(requestedUrl(fetchSpy.mock.calls[0]?.[0])).toBe(
      'http://gc-supervisor.test/v0/city/test-city/formulas/mol-direct?target=test-city%2Fcodex&scope_kind=city&scope_ref=test-city',
    );
  });

  it('normalizes supervisor error responses', async () => {
    const fetchSpy = vi.fn(
      async () =>
        new Response(JSON.stringify({ error: 'supervisor unavailable', kind: 'upstream' }), {
          status: 503,
          statusText: 'Service Unavailable',
          headers: {
            'content-type': 'application/json',
            'x-gc-request-id': 'req-42',
          },
        }),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });

    await expect(api.health()).rejects.toMatchObject({
      name: 'SupervisorApiError',
      status: 503,
      message: 'supervisor unavailable',
      requestId: 'req-42',
    });
  });

  it('accepts supervisor responses whose shapes exceed the OpenAPI snapshot (r43k)', async () => {
    // Regression for r43k: the dashboard must not re-validate and reject the
    // supervisor's own valid output. Here `last_activity` is not an RFC3339
    // datetime and the agent carries a field absent from the snapshot — both
    // previously tripped strict client-side response validation and blanked
    // the roster. The supervisor is the source of truth; we accept its frame.
    const fetchSpy = vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            items: [
              {
                name: 'mayor',
                available: true,
                running: true,
                suspended: false,
                state: 'idle',
                provider: 'claude',
                unmodeled_field: 'emitted by a newer supervisor',
                session: {
                  name: 'mayor',
                  attached: true,
                  last_activity: 'not-a-date',
                },
              },
            ],
            total: 1,
          }),
          {
            status: 200,
            headers: { 'content-type': 'application/json' },
          },
        ),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });

    const result = await api.listAgents('test-city');
    expect(result.items?.[0]?.name).toBe('mayor');
    expect(result.total).toBe(1);
  });

  it('accepts an events frame with an event type absent from the OpenAPI snapshot (r43k)', async () => {
    // Regression for r43k: the live supervisor emits event types (e.g.
    // `bead.deleted`, `session.reset_stalled`) added after the snapshot was
    // captured. A closed discriminated-union validator rejected the entire
    // frame, killing the Activity tab whenever such an event landed in the
    // window. The dashboard must accept unknown event types.
    const frame = {
      items: [
        {
          seq: 1397867,
          type: 'bead.updated',
          ts: '2026-06-04T14:12:30.892324421-04:00',
          actor: 'controller',
          subject: 'gpk-wisp',
          payload: {
            bead: {
              id: 'gpk-wisp',
              title: 'order',
              status: 'open',
              issue_type: 'task',
              created_at: '2026-06-04T18:12:31Z',
            },
          },
        },
        {
          seq: 1397868,
          type: 'bead.deleted',
          ts: '2026-06-04T14:12:31.000000000-04:00',
          actor: 'controller',
          subject: 'gpk-wisp',
          payload: {
            bead: {
              id: 'gpk-wisp',
              title: 'order',
              status: 'closed',
              issue_type: 'task',
              created_at: '2026-06-04T18:12:31Z',
            },
          },
        },
      ],
      total: 2,
    };
    const fetchSpy = vi.fn(
      async () =>
        new Response(JSON.stringify(frame), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        }),
    );

    const api = createSupervisorApi({
      baseUrl: 'http://gc-supervisor.test',
      fetch: fetchSpy as typeof fetch,
    });

    const result = await api.listEvents('test-city');
    expect(result.items?.length).toBe(2);
    expect(result.items?.map((event) => event.type)).toContain('bead.deleted');
  });

  it('publishes mutation headers required by the supervisor', () => {
    expect(createSupervisorApi().mutationHeaders()).toEqual(GC_MUTATION_HEADERS);
  });

  it('supports test injection without importing the dashboard api client', async () => {
    const fake = {
      baseUrl: 'test://supervisor',
      getBead: vi.fn(),
      cityHealth: vi.fn(),
      cityStatus: vi.fn(),
      cityUsage: vi.fn(),
      runCensus: vi.fn(),
      health: vi.fn(),
      listAgents: vi.fn(),
      listRigs: vi.fn(),
      listBeads: vi.fn(),
      listEvents: vi.fn(),
      listCities: vi.fn(),
      formulaFeed: vi.fn(),
      listMail: vi.fn(),
      markMailRead: vi.fn(),
      markMailUnread: vi.fn(),
      archiveMail: vi.fn(),
      replyMail: vi.fn(),
      listSessions: vi.fn(),
      sessionPending: vi.fn(),
      respondSession: vi.fn(),
      mailThread: vi.fn(),
      sendMail: vi.fn(),
      createBead: vi.fn(),
      updateBead: vi.fn(),
      closeBead: vi.fn(),
      sling: vi.fn(),
      cityEventStreamUrl: vi.fn(() => '/gc-supervisor/v0/city/test-city/events/stream'),
      sessionStreamUrl: vi.fn(() => '/gc-supervisor/v0/city/test-city/session/gc-session-1/stream'),
      mutationHeaders: vi.fn(() => GC_MUTATION_HEADERS),
      sessionTranscript: vi.fn(),
      workflowRun: vi.fn(),
      formulaDetail: vi.fn(),
    };

    setSupervisorApiForTests(fake);

    expect(supervisorApi()).toBe(fake);
    await supervisorApi().health();
    expect(fake.health).toHaveBeenCalledOnce();
  });
});

function requestedUrl(input: RequestInfo | URL | undefined): string {
  if (input instanceof Request) return input.url;
  if (input instanceof URL) return input.toString();
  return String(input);
}

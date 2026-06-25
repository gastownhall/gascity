import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, ApiClientError } from './client';

describe('api client error handling', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('surfaces non-JSON error bodies instead of replacing them with status text', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response('plain upstream failure', {
            status: 502,
            statusText: 'Bad Gateway',
          }),
      ),
    );

    await expect(api.config()).rejects.toMatchObject({
      status: 502,
      message: 'plain upstream failure',
    });
  });

  it('preserves structured API error kind and message', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(JSON.stringify({ error: 'bad scope', kind: 'validation' }), {
            status: 400,
            statusText: 'Bad Request',
            headers: { 'content-type': 'application/json' },
          }),
      ),
    );

    await expect(api.config()).rejects.toBeInstanceOf(ApiClientError);
    await expect(api.config()).rejects.toMatchObject({
      status: 400,
      message: 'bad scope',
      kind: 'validation',
    });
  });

  it('rejects malformed successful response bodies at the frontend API edge', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(JSON.stringify({ cityName: 'demo-city' }), {
            status: 200,
            headers: { 'content-type': 'application/json' },
          }),
      ),
    );

    await expect(api.config()).rejects.toMatchObject({
      name: 'ApiResponseDecodeError',
      message: expect.stringContaining('config.cityRoot must be a string'),
    });
  });

  it('rejects a config body missing the read-only posture at the edge', async () => {
    // readOnly drives whether the SPA disables its mutating controls
    // (gascity-dashboard-uzhr). A body that omits it must fail at the edge
    // rather than default-coerce a missing flag to a writable dashboard.
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({
              cityName: 'demo-city',
              cityRoot: '/srv/gc/demo',
              useFixtures: false,
              enabledModules: null,
              defaultView: null,
            }),
            {
              status: 200,
              headers: { 'content-type': 'application/json' },
            },
          ),
      ),
    );

    await expect(api.config()).rejects.toMatchObject({
      name: 'ApiResponseDecodeError',
      message: expect.stringContaining('config.readOnly must be a boolean'),
    });
  });

  it('decodes a well-formed config body carrying the read-only posture', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({
              cityName: 'demo-city',
              cityRoot: '/srv/gc/demo',
              useFixtures: false,
              readOnly: true,
              operatorAlias: 'operator',
              operatorWireAlias: 'human',
              decisionLabel: 'needs/operator',
              enabledModules: null,
              defaultView: null,
            }),
            {
              status: 200,
              headers: { 'content-type': 'application/json' },
            },
          ),
      ),
    );

    await expect(api.config()).resolves.toMatchObject({ readOnly: true });
  });

  it('rejects a local-tools body whose installed status is absent at the edge', async () => {
    // The Health renderer branches on each tool's `status`; a tool object that
    // omits it would mis-render silently, so the decoder rejects it up front.
    const tool = { status: 'available', version: '2.1.2', source: 'local probe: dolt version' };
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({
              gc: tool,
              beads: tool,
              dolt: { version: '2.1.2' },
            }),
            { status: 200, headers: { 'content-type': 'application/json' } },
          ),
      ),
    );

    await expect(api.localToolVersions()).rejects.toMatchObject({
      name: 'ApiResponseDecodeError',
      message: expect.stringContaining('dolt.status must be a string'),
    });
  });

  it('decodes a cached supervisor-status report at the edge', async () => {
    // gascity-dashboard-4bol: the Health status widgets read the dashboard
    // backend's cached /supervisor-status snapshot; the report envelope is
    // validated at the API edge before the page consumes it.
    const report = {
      available: true,
      sampledAt: '2026-06-07T00:00:00.000Z',
      status: { name: 'demo-city', work: { open: 1, ready: 2, in_progress: 3 } },
    };
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(JSON.stringify(report), {
            status: 200,
            headers: { 'content-type': 'application/json' },
          }),
      ),
    );

    await expect(api.supervisorStatus()).resolves.toMatchObject({
      available: true,
      sampledAt: '2026-06-07T00:00:00.000Z',
    });
  });

  it('rejects a supervisor-status body missing the availability discriminant', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(JSON.stringify({ status: null }), {
            status: 200,
            headers: { 'content-type': 'application/json' },
          }),
      ),
    );

    await expect(api.supervisorStatus()).rejects.toMatchObject({
      name: 'ApiResponseDecodeError',
      message: expect.stringContaining('supervisor status.available must be a boolean'),
    });
  });

  it('rejects an available supervisor-status report whose status payload is missing', async () => {
    // The Health widgets dereference status fields; an available report with no
    // status object must fail at the edge rather than crash at render.
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(JSON.stringify({ available: true, sampledAt: '2026-06-07T00:00:00.000Z' }), {
            status: 200,
            headers: { 'content-type': 'application/json' },
          }),
      ),
    );

    await expect(api.supervisorStatus()).rejects.toMatchObject({
      name: 'ApiResponseDecodeError',
      message: expect.stringContaining('supervisor status.status must be an object'),
    });
  });

  it('rejects an available supervisor-status report missing sampledAt', async () => {
    // The available branch's contract requires sampledAt; absence must fail at
    // the edge so the decoded value does not lie about its type.
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({
              available: true,
              status: { name: 'demo-city', work: { open: 1, ready: 2, in_progress: 3 } },
            }),
            { status: 200, headers: { 'content-type': 'application/json' } },
          ),
      ),
    );

    await expect(api.supervisorStatus()).rejects.toMatchObject({
      name: 'ApiResponseDecodeError',
      message: expect.stringContaining('supervisor status.sampledAt must be a string'),
    });
  });

  it('rejects a supervisor-status report whose status.work is missing', async () => {
    // The Beads usage widget dereferences status.work.{open,ready,in_progress};
    // a status object without work must fail at the edge, not crash at render.
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({
              available: true,
              sampledAt: '2026-06-07T00:00:00.000Z',
              status: { name: 'demo-city' },
            }),
            { status: 200, headers: { 'content-type': 'application/json' } },
          ),
      ),
    );

    await expect(api.supervisorStatus()).rejects.toMatchObject({
      name: 'ApiResponseDecodeError',
      message: expect.stringContaining('supervisor status.status.work must be an object'),
    });
  });
});

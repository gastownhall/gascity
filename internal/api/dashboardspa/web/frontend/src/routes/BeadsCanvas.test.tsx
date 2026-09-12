import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setActiveCity } from '../api/cityBase';
import { invalidate } from '../api/cache';
import { NowProvider } from '../contexts/NowContext';
import type { SupervisorBead } from '../supervisor/beadReads';
import { BeadsCanvasPage } from './BeadsCanvas';

const beadQueries: URLSearchParams[] = [];

beforeEach(() => {
  setActiveCity('test-city');
  beadQueries.length = 0;
  invalidate('beads:canvas:');
  invalidate('rigs:canvas:');
  localStorage.clear();
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(
        input instanceof Request ? input.url : input instanceof URL ? input : String(input),
        window.location.origin,
      );
      if (url.pathname === '/v0/city/test-city/beads') {
        beadQueries.push(url.searchParams);
        return json({ items: sampleBeads(), total: sampleBeads().length });
      }
      if (url.pathname === '/v0/city/test-city/rigs') {
        return json({
          items: [{ name: 'inktree', path: '/repo/inktree', agent_count: 2, running_count: 2 }],
          total: 1,
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

describe('BeadsCanvasPage', () => {
  it('renders live beads as accessible draggable bubbles with dependency edges', async () => {
    renderPage();

    expect(await screen.findByRole('heading', { name: 'Beads Canvas' })).toBeTruthy();
    expect(await screen.findByRole('button', { name: 'ga-root: Ship the canvas' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'ga-child: Verify the canvas' })).toBeTruthy();
    expect(screen.getByLabelText('1 dependency edges')).toBeTruthy();
    expect(screen.getByText('ga-root blocks ga-child')).toBeTruthy();
    expect(screen.getByRole('application', { name: /canvas minimap/i })).toBeTruthy();
  });

  it('switches from type clusters to ordered priority lanes', async () => {
    renderPage();
    await screen.findByRole('button', { name: 'ga-root: Ship the canvas' });

    fireEvent.click(screen.getByRole('button', { name: 'Priority lanes' }));

    expect(screen.getByText('P0 · critical · 1')).toBeTruthy();
    expect(screen.getByText('P4 · backlog · 0')).toBeTruthy();
  });

  it('widens the live query only when closed beads are requested', async () => {
    renderPage();
    await screen.findByRole('button', { name: 'ga-root: Ship the canvas' });
    expect(beadQueries).toHaveLength(1);
    expect(beadQueries[0]?.has('all')).toBe(false);

    fireEvent.click(screen.getByRole('button', { name: 'closed' }));

    await waitFor(() => expect(beadQueries).toHaveLength(2));
    expect(beadQueries[1]?.get('all')).toBe('true');
  });
});

function renderPage() {
  return render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <NowProvider intervalMs={1_000_000}>
        <BeadsCanvasPage />
      </NowProvider>
    </MemoryRouter>,
  );
}

function sampleBeads(): SupervisorBead[] {
  return [
    {
      id: 'ga-root',
      title: 'Ship the canvas',
      description: 'Port the Bubbly Canvas.',
      status: 'in_progress',
      priority: 0,
      issue_type: 'feature',
      labels: ['inktree'],
      created_at: '2026-01-01T00:00:00Z',
    },
    {
      id: 'ga-child',
      title: 'Verify the canvas',
      description: 'Exercise the live graph.',
      status: 'open',
      priority: 2,
      issue_type: 'task',
      needs: ['ga-root'],
      created_at: '2026-01-01T00:00:00Z',
    },
  ];
}

function json(payload: unknown): Response {
  return new Response(JSON.stringify(payload), {
    status: 200,
    headers: { 'content-type': 'application/json' },
  });
}

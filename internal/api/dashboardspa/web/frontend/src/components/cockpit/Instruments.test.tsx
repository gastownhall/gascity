import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { ActivityTrace, Gauge, Odometer, PipelineBar, RunRings, StatusLamps } from './Instruments';

describe('cockpit instruments', () => {
  afterEach(() => cleanup());

  it('exposes the odometer reading in the accessibility tree', () => {
    render(<Odometer label="model calls today" value={42} note="$1.20 estimated" />);
    expect(screen.getByRole('status', { name: 'model calls today: 42' })).toBeTruthy();
  });

  it('keeps the full gauge scale inside its SVG viewport', () => {
    const { container } = render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <Gauge label="active sessions" value={2} max={10} formatted="2" href="/agents" />
      </MemoryRouter>,
    );
    const svg = container.querySelector('svg');
    expect(svg).not.toBeNull();
    const [, , width = 0, height = 0] = (svg?.getAttribute('viewBox') ?? '').split(' ').map(Number);
    for (const line of container.querySelectorAll('svg line')) {
      for (const [axis, bound] of [
        ['x1', width],
        ['x2', width],
        ['y1', height],
        ['y2', height],
      ] as const) {
        const coordinate = Number(line.getAttribute(axis));
        expect(coordinate, `${axis} should be inside the gauge viewport`).toBeGreaterThanOrEqual(0);
        expect(coordinate, `${axis} should be inside the gauge viewport`).toBeLessThanOrEqual(
          bound,
        );
      }
    }
  });

  it('keeps one comfortably-sized pipeline control per state', () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <PipelineBar
          segments={[
            { key: 'pending', label: 'queued', count: 2, href: '/runs' },
            { key: 'active', label: 'running', count: 1, href: '/runs' },
          ]}
        />
      </MemoryRouter>,
    );
    expect(screen.getAllByTestId('pipeline-track-segment')).toHaveLength(2);
    expect(screen.getAllByRole('link')).toHaveLength(2);
    expect(screen.getByRole('link', { name: 'queued: 2' }).className).toContain('min-h-6');
  });

  it('announces unavailable traces and pipeline counts without inventing zero readings', () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <ActivityTrace samples={[]} available={false} note="usage unavailable" />
        <PipelineBar
          available={false}
          segments={[
            { key: 'pending', label: 'queued', count: 0, href: '/runs' },
            { key: 'active', label: 'running', count: 0, href: '/runs' },
          ]}
        />
      </MemoryRouter>,
    );

    expect(
      screen.getByRole('figure', { name: /recent model activity: unavailable/i }),
    ).toBeTruthy();
    expect(screen.getByRole('link', { name: 'queued: unavailable' }).textContent).toContain('—');
  });

  it('keeps a partial activity reading available while announcing its provenance', () => {
    render(<ActivityTrace samples={[2]} available note="usage estimate is partial" />);

    expect(
      screen.getByRole('figure', {
        name: /recent model activity: 2 invocations in the current window; usage estimate is partial/i,
      }),
    ).toBeTruthy();
  });

  it('announces variable stage totals and retries', () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <RunRings
          runs={[
            {
              id: 'r1',
              label: 'Deploy',
              stage: 7,
              totalStages: 7,
              stageWord: 'publish',
              attempt: 3,
              href: '/runs/r1',
            },
          ]}
        />
      </MemoryRouter>,
    );
    expect(
      screen.getByRole('link', { name: 'Deploy: stage 7 of 7, retry attempt 3' }),
    ).toBeTruthy();
  });

  it('announces lamp health without relying on color', () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <StatusLamps
          lamps={[
            {
              key: 'store',
              label: 'store',
              value: 'maintenance overdue',
              state: 'warning',
              href: '/health',
            },
            {
              key: 'feed',
              label: 'live feed',
              value: 'disconnected',
              state: 'unknown',
              href: '/activity',
            },
          ]}
        />
      </MemoryRouter>,
    );
    expect(screen.getByRole('link', { name: 'store: warning, maintenance overdue' })).toBeTruthy();
    expect(screen.getByRole('link', { name: 'live feed: unknown, disconnected' })).toBeTruthy();
  });
});

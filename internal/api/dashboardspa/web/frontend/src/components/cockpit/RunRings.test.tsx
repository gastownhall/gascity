import { cleanup, render } from '@testing-library/react';
import type { ReactNode } from 'react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it } from 'vitest';

import { RING_CIRC, RunRings, type RunRing, ringDashoffset } from './RunRings';

function router(node: ReactNode) {
  return (
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      {node}
    </MemoryRouter>
  );
}

describe('ringDashoffset', () => {
  it('leaves 60% of the circumference for a 2-of-5 ring', () => {
    expect(ringDashoffset(2, 5)).toBeCloseTo(RING_CIRC * 0.6, 6);
  });

  it('is zero for a completed ring', () => {
    expect(ringDashoffset(5, 5)).toBeCloseTo(0, 6);
  });

  it('is the full circumference for a not-started ring', () => {
    expect(ringDashoffset(0, 5)).toBeCloseTo(RING_CIRC, 6);
  });
});

describe('RunRings', () => {
  afterEach(() => {
    cleanup();
  });

  it('sets the progress dashoffset and rotation for the stage fraction', () => {
    const runs: RunRing[] = [
      { id: 'r1', label: 'build', stage: 2, stageWord: 'testing', href: '/runs/r1' },
    ];
    const { container } = render(router(<RunRings runs={runs} totalStages={5} />));
    const progress = container.querySelector('[data-testid="ring-progress"]') as SVGCircleElement;
    expect(Number(progress.getAttribute('stroke-dashoffset'))).toBeCloseTo(RING_CIRC * 0.6, 2);
    expect(progress.getAttribute('transform')).toBe('rotate(-90 38 38)');
    expect(container.querySelector('[data-testid="ring-count"]')?.textContent).toBe('2/5');
  });

  it('closes the ring at the final stage', () => {
    const runs: RunRing[] = [
      { id: 'r1', label: 'build', stage: 5, stageWord: 'done', href: '/runs/r1' },
    ];
    const { container } = render(router(<RunRings runs={runs} totalStages={5} />));
    const progress = container.querySelector('[data-testid="ring-progress"]') as SVGCircleElement;
    expect(Number(progress.getAttribute('stroke-dashoffset'))).toBeCloseTo(0, 2);
  });

  it('swaps the stage word for a warn-toned attempt marker when retrying', () => {
    const { container } = render(
      router(
        <RunRings
          runs={[
            { id: 'r1', label: 'build', stage: 2, stageWord: 'testing', attempt: 2, href: '/r1' },
          ]}
        />,
      ),
    );
    const attempt = container.querySelector('[data-testid="ring-attempt"]');
    expect(attempt?.textContent).toContain('attempt 2');
    expect(attempt?.className).toContain('text-warn');
    expect(attempt?.className).not.toContain('text-accent');
    expect(container.querySelector('[data-testid="ring-stageword"]')).toBeNull();
  });

  it('shows the stage word (not the attempt marker) on the first attempt', () => {
    const { container } = render(
      router(
        <RunRings
          runs={[
            { id: 'r1', label: 'build', stage: 2, stageWord: 'testing', attempt: 1, href: '/r1' },
          ]}
        />,
      ),
    );
    expect(container.querySelector('[data-testid="ring-stageword"]')?.textContent).toBe('testing');
    expect(container.querySelector('[data-testid="ring-attempt"]')).toBeNull();
  });

  it('deep-links each ring', () => {
    const { container } = render(
      router(
        <RunRings
          runs={[{ id: 'r1', label: 'build', stage: 2, stageWord: 'testing', href: '/runs/r1' }]}
        />,
      ),
    );
    const link = container.querySelector('[data-id="r1"]') as HTMLElement;
    expect(link.getAttribute('href')).toBe('/runs/r1');
    expect(link.textContent).toContain('build');
  });
});

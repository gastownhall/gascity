import { cleanup, render } from '@testing-library/react';
import type { ReactNode } from 'react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it } from 'vitest';

import { PipelineBar, type PipelineSegment, pipelineWidths } from './PipelineBar';

function router(node: ReactNode) {
  return (
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      {node}
    </MemoryRouter>
  );
}

const SEGMENTS: PipelineSegment[] = [
  { key: 'ready', label: 'Ready', count: 3, href: '/beads?state=ready' },
  { key: 'active', label: 'Active', count: 1, href: '/beads?state=active' },
];

describe('pipelineWidths', () => {
  it('is proportional to count over the total', () => {
    expect(pipelineWidths([3, 1])).toEqual([75, 25]);
  });

  it('floors empty segments at 2% so they stay visible', () => {
    expect(pipelineWidths([100, 0])).toEqual([100, 2]);
  });

  it('renders an even split when the total is zero', () => {
    expect(pipelineWidths([0, 0, 0])).toEqual([100 / 3, 100 / 3, 100 / 3]);
  });
});

describe('PipelineBar', () => {
  afterEach(() => {
    cleanup();
  });

  it('sizes segments by the width math', () => {
    const { container } = render(router(<PipelineBar segments={SEGMENTS} />));
    const segs = container.querySelectorAll('[data-testid="pipeline-segment"]');
    expect((segs[0] as HTMLElement).style.width).toBe('75%');
    expect((segs[1] as HTMLElement).style.width).toBe('25%');
  });

  it('keeps an empty segment visible via the 2% floor', () => {
    const { container } = render(
      router(
        <PipelineBar
          segments={[
            { key: 'a', label: 'A', count: 100, href: '/a' },
            { key: 'b', label: 'B', count: 0, href: '/b' },
          ]}
        />,
      ),
    );
    const segs = container.querySelectorAll('[data-testid="pipeline-segment"]');
    expect((segs[1] as HTMLElement).style.width).toBe('2%');
  });

  it('paints the alpha ladder by segment index', () => {
    const { container } = render(
      router(
        <PipelineBar
          segments={[
            { key: 'a', label: 'A', count: 1, href: '/a' },
            { key: 'b', label: 'B', count: 1, href: '/b' },
            { key: 'c', label: 'C', count: 1, href: '/c' },
            { key: 'd', label: 'D', count: 1, href: '/d' },
          ]}
        />,
      ),
    );
    const segs = container.querySelectorAll('[data-testid="pipeline-segment"]');
    expect((segs[0] as HTMLElement).style.backgroundColor).toBe('oklch(var(--fg) / 0.18)');
    expect((segs[1] as HTMLElement).style.backgroundColor).toBe('oklch(var(--fg) / 0.36)');
    expect((segs[2] as HTMLElement).style.backgroundColor).toBe('oklch(var(--fg) / 0.58)');
    expect((segs[3] as HTMLElement).style.backgroundColor).toBe('oklch(var(--fg) / 0.8)');
  });

  it('lists each segment in the legend with its count', () => {
    const { container } = render(router(<PipelineBar segments={SEGMENTS} />));
    const entries = container.querySelectorAll('[data-testid="pipeline-legend-entry"]');
    expect(entries).toHaveLength(2);
    expect(entries[0]!.textContent).toContain('Ready');
    expect(entries[0]!.textContent).toContain('3');
    expect(entries[1]!.textContent).toContain('Active');
    expect(entries[1]!.textContent).toContain('1');
  });

  it('deep-links both the segment and its legend entry', () => {
    const { container } = render(router(<PipelineBar segments={SEGMENTS} />));
    const segs = container.querySelectorAll('[data-testid="pipeline-segment"]');
    const entries = container.querySelectorAll('[data-testid="pipeline-legend-entry"]');
    expect(segs[0]!.getAttribute('href')).toBe('/beads?state=ready');
    expect(entries[0]!.getAttribute('href')).toBe('/beads?state=ready');
  });
});

import { cleanup, render, screen, within } from '@testing-library/react';
import type { ReactNode } from 'react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it } from 'vitest';

import { Odometer, odometerColumns } from './Odometer';

function router(node: ReactNode) {
  return (
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      {node}
    </MemoryRouter>
  );
}

describe('odometerColumns', () => {
  it('zero-pads to the configured digit count', () => {
    expect(odometerColumns(181, 4)).toEqual([0, 1, 8, 1]);
  });

  it('widens past the configured width when the value is longer', () => {
    expect(odometerColumns(12345, 4)).toEqual([1, 2, 3, 4, 5]);
  });

  it('floors and clamps non-integer or negative input to zero', () => {
    expect(odometerColumns(-7, 2)).toEqual([0, 0]);
    expect(odometerColumns(4.9, 2)).toEqual([0, 4]);
  });
});

describe('Odometer', () => {
  afterEach(() => {
    cleanup();
  });

  it('renders one column per decomposed digit', () => {
    const { container } = render(
      router(<Odometer value={181} digits={4} label="events today" href="/activity" />),
    );
    const columns = container.querySelectorAll('[data-testid="odometer-column"]');
    expect([...columns].map((c) => c.getAttribute('data-digit'))).toEqual(['0', '1', '8', '1']);
  });

  it('translates each digit stack by one cell height per unit', () => {
    const { container } = render(
      router(<Odometer value={181} digits={4} label="events today" href="/activity" />),
    );
    const stacks = container.querySelectorAll('[data-testid="odometer-stack"]');
    // Columns are [0, 1, 8, 1].
    expect((stacks[1] as HTMLElement).style.transform).toBe('translateY(-1.1em)');
    expect((stacks[2] as HTMLElement).style.transform).toBe('translateY(-8.8em)');
  });

  it('grows past the configured digit width instead of truncating', () => {
    const { container } = render(
      router(<Odometer value={12345} digits={4} label="events today" href="/activity" />),
    );
    expect(container.querySelectorAll('[data-testid="odometer-column"]')).toHaveLength(5);
  });

  it('deep-links the instrument and prints label and sub caption', () => {
    render(
      router(
        <Odometer value={181} digits={4} label="events today" sub="since boot" href="/activity" />,
      ),
    );
    const link = screen.getByRole('link', { name: 'events today: 181' });
    expect(link.getAttribute('href')).toBe('/activity');
    expect(screen.getByText('events today')).toBeTruthy();
    expect(screen.getByText('since boot')).toBeTruthy();
  });

  it('defaults to a 4-column readout when digits is omitted', () => {
    const { container } = render(router(<Odometer value={7} label="beads closed today" />));
    const columns = container.querySelectorAll('[data-testid="odometer-column"]');
    expect([...columns].map((c) => c.getAttribute('data-digit'))).toEqual(['0', '0', '0', '7']);
  });

  it('renders an unlinked labelled block when no href is given', () => {
    const { container } = render(
      router(<Odometer value={181} digits={4} label="beads closed today" sub="$1.00 est today" />),
    );
    // No Link wrapper — the block is a plain labelled region.
    expect(container.querySelector('a')).toBeNull();
    const block = screen.getByLabelText('beads closed today: 181');
    expect(block).toBeTruthy();
    expect(within(block).getByText('beads closed today')).toBeTruthy();
    expect(within(block).getByText('$1.00 est today')).toBeTruthy();
  });
});

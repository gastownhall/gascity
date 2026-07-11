import { cleanup, render } from '@testing-library/react';
import type { ReactNode } from 'react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it } from 'vitest';

import { VUBank, type VUMeter, fillPct } from './VUBank';

function router(node: ReactNode) {
  return (
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      {node}
    </MemoryRouter>
  );
}

function pct(value: string): number {
  return Number(value.replace('%', ''));
}

describe('fillPct', () => {
  it('is the value as a percentage of max', () => {
    expect(fillPct(50, 100)).toBe(50);
  });

  it('clamps to 100 when the value exceeds max', () => {
    expect(fillPct(200, 100)).toBe(100);
  });

  it('clamps to 0 for negatives and a non-positive max', () => {
    expect(fillPct(-5, 100)).toBe(0);
    expect(fillPct(10, 0)).toBe(0);
  });
});

describe('VUBank', () => {
  afterEach(() => {
    cleanup();
  });

  it('renders tall columns (58w × 130h) in a bottom-aligned bank', () => {
    const meters: VUMeter[] = [{ id: 's1', name: 'sess-1', value: 50, href: '/s1' }];
    const { container } = render(router(<VUBank meters={meters} max={100} />));
    const well = container.querySelector('[data-testid="vu-well"]') as HTMLElement;
    expect(well.className).toContain('w-[58px]');
    expect(well.className).toContain('h-[130px]');
    const bank = container.querySelector('[data-testid="vu-bank"]') as HTMLElement;
    expect(bank.className).toContain('items-end');
    expect(bank.className).toContain('min-h-[190px]');
  });

  it('grows the fill to the value fraction and clamps at 100%', () => {
    const meters: VUMeter[] = [{ id: 's1', name: 'sess-1', value: 200, href: '/s1' }];
    const { container } = render(router(<VUBank meters={meters} max={100} />));
    expect((container.querySelector('[data-testid="vu-fill"]') as HTMLElement).style.height).toBe(
      '100%',
    );
  });

  it('ratchets the peak-hold line and keeps it above a dropped value', () => {
    const { container, rerender } = render(
      router(<VUBank meters={[{ id: 's1', name: 'sess-1', value: 90, href: '/s1' }]} max={100} />),
    );
    const peakHigh = pct(
      (container.querySelector('[data-testid="vu-peak"]') as HTMLElement).style.bottom,
    );
    expect(peakHigh).toBeGreaterThan(80);

    rerender(
      router(<VUBank meters={[{ id: 's1', name: 'sess-1', value: 10, href: '/s1' }]} max={100} />),
    );
    const fill = pct((container.querySelector('[data-testid="vu-fill"]') as HTMLElement).style.height);
    const peakLow = pct(
      (container.querySelector('[data-testid="vu-peak"]') as HTMLElement).style.bottom,
    );
    expect(fill).toBeCloseTo(10, 5);
    expect(peakLow).toBeGreaterThan(fill);
    expect(peakLow).toBeGreaterThan(70);
  });

  it('holds each session peak across a reorder because it is keyed by id', () => {
    const { container, rerender } = render(
      router(
        <VUBank
          meters={[
            { id: 'a', name: 'AAA', value: 90, href: '/a' },
            { id: 'b', name: 'BBB', value: 10, href: '/b' },
          ]}
          max={100}
        />,
      ),
    );
    rerender(
      router(
        <VUBank
          meters={[
            { id: 'b', name: 'BBB', value: 10, href: '/b' },
            { id: 'a', name: 'AAA', value: 10, href: '/a' },
          ]}
          max={100}
        />,
      ),
    );
    const a = container.querySelector('[data-id="a"]') as HTMLElement;
    expect(a.getAttribute('aria-label')).toContain('AAA');
    const aPeak = pct((a.querySelector('[data-testid="vu-peak"]') as HTMLElement).style.bottom);
    expect(aPeak).toBeGreaterThan(70);
  });

  it('shows the compact value and deep-links each meter', () => {
    const meters: VUMeter[] = [{ id: 's1', name: 'sess-1', value: 1500, href: '/sessions/s1' }];
    const { container } = render(router(<VUBank meters={meters} max={4000} />));
    const meter = container.querySelector('[data-testid="vu-meter"]') as HTMLElement;
    expect(meter.getAttribute('href')).toBe('/sessions/s1');
    expect(meter.textContent).toContain('1.5K');
  });
});

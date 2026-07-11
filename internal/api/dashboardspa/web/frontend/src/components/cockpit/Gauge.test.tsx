import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it } from 'vitest';

import { GAUGE_END_DEG, gaugeNeedleDeg } from './arc';
import { Gauge } from './Gauge';

function renderGauge(value: number) {
  return render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <Gauge
        label="events / min"
        value={value}
        max={150}
        warnFrom={120}
        format={(v) => `${Math.round(v)}`}
        tickFormat={(v) => `${Math.round(v)}`}
        href="/activity?mode=events"
      />
    </MemoryRouter>,
  );
}

describe('Gauge', () => {
  afterEach(() => {
    cleanup();
  });

  it('deep-links the whole instrument and labels it accessibly', () => {
    renderGauge(137);
    const link = screen.getByRole('link', { name: 'events / min: 137' });
    expect(link.getAttribute('href')).toBe('/activity?mode=events');
    expect(screen.getByTestId('gauge-value').textContent).toBe('137');
  });

  it('rotates the needle to the value fraction', () => {
    renderGauge(75);
    const needle = screen.getByTestId('gauge-needle');
    expect(needle.style.transform).toBe(`rotate(${gaugeNeedleDeg(75 / 150)}deg)`);
  });

  it('pegs the needle just past full scale when the value overshoots', () => {
    renderGauge(9000);
    const needle = screen.getByTestId('gauge-needle');
    const deg = Number(/rotate\((-?[\d.]+)deg\)/.exec(needle.style.transform)?.[1]);
    expect(deg).toBeGreaterThan(GAUGE_END_DEG);
    expect(deg).toBeLessThan(GAUGE_END_DEG + 15);
  });

  it('draws the warn zone arc outside the dial (R+3) with a 2.5 stroke', () => {
    renderGauge(10);
    const warn = screen.getByTestId('gauge-warn-zone');
    expect(warn.getAttribute('stroke-width')).toBe('2.5');
    // arcPath renders the radius twice in the elliptical-arc command.
    expect(warn.getAttribute('d')).toContain('A 65 65');
  });

  it('draws the track arc at the dial radius with a hairline 1.5 stroke', () => {
    renderGauge(10);
    const track = screen.getByTestId('gauge-track');
    expect(track.getAttribute('stroke-width')).toBe('1.5');
    expect(track.getAttribute('d')).toContain('A 62 62');
  });

  it('formats the major-tick labels with the supplied tickFormat', () => {
    renderGauge(10);
    // 11 ticks, majors at i=0,5,10 → 0, 75, 150 for a 150-max integer format.
    expect(screen.getByText('0')).toBeTruthy();
    expect(screen.getByText('75')).toBeTruthy();
    expect(screen.getByText('150')).toBeTruthy();
  });
});

import { describe, expect, it } from 'vitest';

import {
  GAUGE_END_DEG,
  GAUGE_OVERSHOOT,
  GAUGE_START_DEG,
  arcPath,
  gaugeNeedleDeg,
  polarPoint,
} from './arc';

describe('polarPoint', () => {
  it('puts 0° at 12 o’clock and 90° at 3 o’clock', () => {
    const top = polarPoint(0, 0, 10, 0);
    expect(top.x).toBeCloseTo(0);
    expect(top.y).toBeCloseTo(-10);

    const right = polarPoint(0, 0, 10, 90);
    expect(right.x).toBeCloseTo(10);
    expect(right.y).toBeCloseTo(0);
  });
});

describe('arcPath', () => {
  it('sets the large-arc flag only for sweeps over 180°', () => {
    expect(arcPath(0, 0, 10, 0, 90)).toContain(' 0 0 1 ');
    expect(arcPath(0, 0, 10, GAUGE_START_DEG, GAUGE_END_DEG)).toContain(' 0 1 1 ');
  });
});

describe('gaugeNeedleDeg', () => {
  it('maps 0 to the sweep start and 1 to the sweep end', () => {
    expect(gaugeNeedleDeg(0)).toBe(GAUGE_START_DEG);
    expect(gaugeNeedleDeg(1)).toBe(GAUGE_END_DEG);
  });

  it('clamps below zero and pegs at the overshoot ceiling', () => {
    expect(gaugeNeedleDeg(-1)).toBe(GAUGE_START_DEG);
    expect(gaugeNeedleDeg(5)).toBe(gaugeNeedleDeg(GAUGE_OVERSHOOT));
    expect(gaugeNeedleDeg(5)).toBeGreaterThan(GAUGE_END_DEG);
  });
});

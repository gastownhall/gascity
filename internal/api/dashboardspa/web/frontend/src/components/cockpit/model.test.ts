import { describe, expect, it } from 'vitest';
import type { RunLane } from 'gas-city-dashboard-shared';
import {
  burnPerHour,
  laneToRing,
  pipelineSegments,
  pipelineWidths,
  tokensPerMinute,
} from './model';

describe('cockpit telemetry derivation', () => {
  it('normalizes segment floors to exactly 100 percent', () => {
    const widths = pipelineWidths([100, 0, Number.NaN, -4]);
    expect(widths.reduce((sum, width) => sum + width, 0)).toBeCloseTo(100, 8);
    expect(widths.every((width) => Number.isFinite(width) && width >= 0)).toBe(true);
    expect(pipelineWidths([0, 0, 0, 0])).toEqual([25, 25, 25, 25]);
  });

  it('uses only canonical nonterminal run states', () => {
    expect(
      pipelineSegments({
        pending: 2,
        active: 3,
        waiting: 1,
        canceling: 4,
        completed: 99,
        failed: 8,
        canceled: 7,
        skipped: 6,
      }).map(({ key, count }) => [key, count]),
    ).toEqual([
      ['pending', 2],
      ['active', 3],
      ['waiting', 1],
      ['canceling', 4],
    ]);
  });

  it('derives bounded rates and rejects invalid inputs', () => {
    const totals = {
      invocations: 1,
      compute_facts: 0,
      input_tokens: 100,
      output_tokens: 20,
      cache_read_tokens: 30,
      cache_creation_tokens: 10,
      wall_seconds: 0,
      cost_usd_estimate: 0.5,
      unpriced: 0,
    };
    expect(tokensPerMinute(totals, 300)).toBe(32);
    expect(burnPerHour(totals, 300)).toBe(6);
    expect(tokensPerMinute({ ...totals, input_tokens: Number.NaN }, 0)).toBeNull();
    expect(burnPerHour({ ...totals, cost_usd_estimate: Number.MAX_VALUE }, 1)).toBeNull();
    expect(
      tokensPerMinute(
        {
          ...totals,
          input_tokens: Number.MAX_VALUE,
          output_tokens: 0,
          cache_read_tokens: 0,
          cache_creation_tokens: 0,
        },
        Number.MIN_VALUE,
      ),
    ).toBeNull();
  });

  it('carries each lane real stage total and retry provenance', () => {
    const lane = {
      id: 'run-1',
      title: 'Seven-stage run',
      formula: { status: 'known', name: 'deploy' },
      scope: { status: 'unavailable', error: 'not resolved' },
      phase: 'active',
      phaseLabel: 'publish',
      stages: Array.from({ length: 7 }, (_, index) => ({ key: `s${index}`, label: `S${index}` })),
      progress: {
        status: 'active_step',
        stage: { status: 'available', index: 6, key: 's6', label: 'S6' },
        attempt: { status: 'available', value: 2 },
      },
    } as unknown as RunLane;
    expect(laneToRing(lane)).toMatchObject({ stage: 7, totalStages: 7, attempt: 2 });
  });
});

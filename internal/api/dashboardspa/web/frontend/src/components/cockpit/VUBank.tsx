import { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';

import { fmtCompact } from './format';

// Peak-hold decay per update tick, as a fraction of full scale. The peak
// ratchets up instantly to a new high, then bleeds down toward the live value
// by this much each time the meters change — the classic VU peak-hold feel.
const PEAK_DECAY = 0.05;

/**
 * fillPct maps a meter value to a bar height percentage, clamped to [0, 100].
 * A non-positive max has no scale to read against, so it reads as empty rather
 * than dividing by zero.
 */
export function fillPct(value: number, max: number): number {
  if (max <= 0) return 0;
  return Math.min(Math.max(value / max, 0), 1) * 100;
}

export interface VUMeter {
  /** Stable identity; peak-hold state is keyed by this. */
  id: string;
  /** Session label under the meter. */
  name: string;
  /** Current reading (raw units; the fill shows value/max). */
  value: number;
  /** Deep link to the session. */
  href: string;
}

export interface VUBankProps {
  meters: VUMeter[];
  /** Full-scale reading at the top of every meter. */
  max: number;
}

/**
 * VUBank is a row of per-session VU meters: each a bottom-up fill plus a
 * peak-hold line that ratchets up to new highs and decays slowly back toward
 * the live value. Peak state is held per session id, so reordering the row
 * preserves each session's history. Fill uses the healthy-sage tone, never
 * accent.
 */
export function VUBank({ meters, max }: VUBankProps) {
  const [peaks, setPeaks] = useState<Record<string, number>>({});

  useEffect(() => {
    setPeaks((prev) => {
      const next: Record<string, number> = {};
      for (const meter of meters) {
        const ratio = fillPct(meter.value, max) / 100;
        const prevPeak = prev[meter.id] ?? ratio;
        next[meter.id] = Math.max(ratio, prevPeak - PEAK_DECAY);
      }
      return next;
    });
  }, [meters, max]);

  return (
    <div className="flex flex-wrap items-end gap-3 min-h-[190px]" data-testid="vu-bank">
      {meters.map((meter) => {
        const height = fillPct(meter.value, max);
        const peak = (peaks[meter.id] ?? fillPct(meter.value, max) / 100) * 100;
        return (
          <Link
            key={meter.id}
            to={meter.href}
            data-id={meter.id}
            data-testid="vu-meter"
            aria-label={`${meter.name}: ${fmtCompact(meter.value)}`}
            className="focus-mark block w-[58px] no-underline"
          >
            <div
              className="relative h-[130px] w-[58px] overflow-hidden rounded-sm border border-rule"
              data-testid="vu-well"
            >
              <div
                data-testid="vu-fill"
                className="absolute inset-x-0 bottom-0 bg-ok/55 transition-[height] duration-300 ease-out-quart motion-reduce:transition-none"
                style={{ height: `${height}%` }}
              />
              <div
                data-testid="vu-peak"
                className="absolute inset-x-0 h-[2px] bg-ok transition-[bottom] duration-[900ms] ease-out-quart motion-reduce:transition-none"
                style={{ bottom: `${peak}%` }}
              />
            </div>
            <div className="mt-1 w-[58px] text-center">
              <div className="truncate text-label uppercase tracking-wider text-fg-faint">
                {meter.name}
              </div>
              <div className="text-label tnum text-fg-muted">{fmtCompact(meter.value)}</div>
            </div>
          </Link>
        );
      })}
    </div>
  );
}

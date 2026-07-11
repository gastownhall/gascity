// Shared polar/arc math for the cockpit instruments (gauges, rings).
//
// Angle convention: degrees, 0° at 12 o'clock, positive clockwise — the
// convention the cockpit mockup uses for its 240° dial sweep. SVG's y-axis
// points down, so the conversion offsets by -90° and flips sin/cos
// accordingly.

/** A point on the SVG plane. */
export interface Point {
  x: number;
  y: number;
}

/** polarPoint returns the point at `deg` on the circle (cx, cy, r). */
export function polarPoint(cx: number, cy: number, r: number, deg: number): Point {
  const rad = ((deg - 90) * Math.PI) / 180;
  return { x: cx + r * Math.cos(rad), y: cy + r * Math.sin(rad) };
}

/**
 * arcPath builds an SVG path `d` for the circular arc from startDeg to
 * endDeg (clockwise). The large-arc flag flips when the sweep exceeds 180°.
 */
export function arcPath(
  cx: number,
  cy: number,
  r: number,
  startDeg: number,
  endDeg: number,
): string {
  const start = polarPoint(cx, cy, r, startDeg);
  const end = polarPoint(cx, cy, r, endDeg);
  const largeArc = endDeg - startDeg > 180 ? 1 : 0;
  return `M ${start.x.toFixed(2)} ${start.y.toFixed(2)} A ${r} ${r} 0 ${largeArc} 1 ${end.x.toFixed(2)} ${end.y.toFixed(2)}`;
}

/** The dial sweep shared by every cockpit needle gauge: −120° → +120°. */
export const GAUGE_START_DEG = -120;
export const GAUGE_END_DEG = 120;
export const GAUGE_SWEEP_DEG = GAUGE_END_DEG - GAUGE_START_DEG;

/**
 * Needles may overshoot the dial slightly (the mockup allows 4%) so a
 * pegged gauge reads as "pinned", not parked exactly on the max tick.
 */
export const GAUGE_OVERSHOOT = 1.04;

/**
 * gaugeNeedleDeg maps a value fraction (value/max) to a needle rotation in
 * degrees, clamped to [0, GAUGE_OVERSHOOT].
 */
export function gaugeNeedleDeg(frac: number): number {
  const clamped = Math.min(Math.max(frac, 0), GAUGE_OVERSHOOT);
  return GAUGE_START_DEG + clamped * GAUGE_SWEEP_DEG;
}

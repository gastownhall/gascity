import { useEffect, useRef } from 'react';

import { createScopeBuffer } from './scopeBuffer';
import { useInkPalette, type Ink } from './useInkPalette';

// Four minutes of one-second bins scrolling right-to-left. The trace fills the
// card width; its height is pinned, so the backing-store math needs only width.
const BINS = 240;
const SHIFT_MS = 1000;
const REDUCED_DRAW_MS = 250; // ~4Hz fallback when the operator prefers reduced motion
const HEIGHT = 110;
const MIN_PEAK = 6; // floor the vertical scale so a quiet stream still reads as a line
const HEAD_RADIUS = 2.5;
const DPR_CAP = 2;
const STROKE_WIDTH = 1.2;
// The trace rides above a resting baseline hairline: a zero bin sits at
// BASELINE_BOTTOM and a full bin climbs TRACE_RANGE toward the top, leaving the
// hairline (BASELINE_Y) just below the zero line rather than under the trace.
const BASELINE_Y = HEIGHT - 12.5;
const BASELINE_BOTTOM = HEIGHT - 14;
const TRACE_RANGE = HEIGHT - 24;

export interface OscilloscopeProps {
  /**
   * Registers a callback fired once per incoming event and returns an
   * unsubscribe. Passing this in keeps the scope transport-agnostic (and
   * testable) — the parent owns the SSE wiring.
   */
  subscribe: (onEvent: () => void) => () => void;
  /** Caption rendered under the trace. */
  label?: string;
  /** Freezes the trace (draw loop + window shift stop); events still accumulate. */
  paused?: boolean;
}

/**
 * Oscilloscope is a cockpit event-stream trace: a scrolling canvas polyline of
 * event arrivals over the last four minutes, with a phosphor head dot at the
 * newest bin. It draws in a requestAnimationFrame loop (or a ~4Hz timer under
 * prefers-reduced-motion) and never calls setState per frame — React renders
 * the shell once and the ref-held canvas carries the animation.
 */
export function Oscilloscope({
  subscribe,
  label = 'event stream · 4 min',
  paused = false,
}: OscilloscopeProps) {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const bufferRef = useRef(createScopeBuffer(BINS));
  const ink = useInkPalette();
  const inkRef = useRef<Ink>(ink);

  // Let the draw loop read the latest theme colors without being torn down and
  // re-subscribed every time the palette identity changes.
  useEffect(() => {
    inkRef.current = ink;
  }, [ink]);

  // Bumps are independent of `paused` so a frozen trace still accumulates
  // arrivals; unfreezing reveals them in place.
  useEffect(() => {
    const unsubscribe = subscribe(() => {
      bufferRef.current.bump();
    });
    return unsubscribe;
  }, [subscribe]);

  useEffect(() => {
    if (paused) return;
    const canvas = canvasRef.current;
    if (canvas === null) return;
    const ctx = canvas.getContext('2d');
    if (ctx === null) return;

    const buffer = bufferRef.current;
    const shiftTimer = setInterval(() => {
      buffer.shift();
    }, SHIFT_MS);

    const render = () => draw(ctx, canvas, buffer.snapshot(), inkRef.current);
    const reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

    let rafId = 0;
    let drawTimer: ReturnType<typeof setInterval> | undefined;

    if (reduceMotion) {
      render(); // paint once immediately, then step on a slow timer
      drawTimer = setInterval(render, REDUCED_DRAW_MS);
    } else {
      const loop = () => {
        render();
        rafId = requestAnimationFrame(loop);
      };
      rafId = requestAnimationFrame(loop);
    }

    return () => {
      clearInterval(shiftTimer);
      if (drawTimer !== undefined) clearInterval(drawTimer);
      if (rafId !== 0) cancelAnimationFrame(rafId);
    };
  }, [paused]);

  return (
    <figure className="relative w-full">
      <canvas
        ref={canvasRef}
        role="img"
        aria-label={label}
        data-testid="oscilloscope-canvas"
        className="block w-full"
        style={{ height: HEIGHT }}
      />
      <figcaption className="absolute right-0 top-0 text-label uppercase tracking-wider text-fg-faint">
        {label}
      </figcaption>
    </figure>
  );
}

/**
 * draw renders one frame: it right-sizes the DPR-scaled backing store, then
 * strokes the polyline across every bin and marks the newest point with a
 * phosphor head dot. Normalized to max(MIN_PEAK, peak) so a busy window fills
 * the height while a quiet one stays a low, legible line.
 */
function draw(
  ctx: CanvasRenderingContext2D,
  canvas: HTMLCanvasElement,
  bins: number[],
  ink: Ink,
): void {
  const dpr = Math.min(window.devicePixelRatio || 1, DPR_CAP);
  const cssWidth = canvas.clientWidth;
  const desiredW = Math.max(0, Math.round(cssWidth * dpr));
  const desiredH = Math.round(HEIGHT * dpr);
  if (canvas.width !== desiredW) canvas.width = desiredW;
  if (canvas.height !== desiredH) canvas.height = desiredH;

  // setTransform (rather than a cumulative scale) keeps the DPR mapping exact
  // across frames even when the backing store is not resized.
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, cssWidth, HEIGHT);

  const n = bins.length;
  const peak = Math.max(MIN_PEAK, ...bins);
  const stepX = n > 1 ? cssWidth / (n - 1) : 0;
  const yOf = (v: number) => BASELINE_BOTTOM - (v / peak) * TRACE_RANGE;

  // Resting baseline hairline: the ground the trace stands on.
  ctx.beginPath();
  ctx.lineWidth = 1;
  ctx.strokeStyle = ink('rule');
  ctx.moveTo(0, BASELINE_Y);
  ctx.lineTo(cssWidth, BASELINE_Y);
  ctx.stroke();

  ctx.beginPath();
  ctx.lineWidth = STROKE_WIDTH;
  ctx.lineJoin = 'round';
  ctx.strokeStyle = ink('fg-muted');
  for (let i = 0; i < n; i++) {
    const px = i * stepX;
    const py = yOf(bins[i] ?? 0);
    if (i === 0) ctx.moveTo(px, py);
    else ctx.lineTo(px, py);
  }
  ctx.stroke();

  ctx.beginPath();
  ctx.fillStyle = ink('ok');
  ctx.arc((n - 1) * stepX, yOf(bins[n - 1] ?? 0), HEAD_RADIUS, 0, Math.PI * 2);
  ctx.fill();
}

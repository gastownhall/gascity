import { cleanup, render } from '@testing-library/react';
import type { ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { ThemeProvider } from '../../contexts/ThemeContext';
import { Oscilloscope } from './Oscilloscope';

// The component draws through the ring buffer; mock it so the tests can observe
// bump/shift directly. scopeBuffer's own behavior is covered in scopeBuffer.test.ts.
const { bufferMock } = vi.hoisted(() => ({
  bufferMock: {
    bump: vi.fn(),
    shift: vi.fn(),
    snapshot: vi.fn((): number[] => new Array<number>(240).fill(0)),
  },
}));
vi.mock('./scopeBuffer', () => ({
  createScopeBuffer: vi.fn(() => bufferMock),
}));

// jsdom has no canvas rendering; capture the 2D-context draw calls with a stub.
function makeCtxStub() {
  return {
    setTransform: vi.fn(),
    clearRect: vi.fn(),
    beginPath: vi.fn(),
    moveTo: vi.fn(),
    lineTo: vi.fn(),
    stroke: vi.fn(),
    arc: vi.fn(),
    fill: vi.fn(),
    strokeStyle: '',
    fillStyle: '',
    lineWidth: 0,
    lineJoin: 'round' as CanvasLineJoin,
  };
}

// ThemeProvider (via useInkPalette → useTheme) and the component's
// reduced-motion check both need matchMedia, which jsdom lacks.
let reduceMotion = false;
function installMatchMedia(): void {
  window.matchMedia = ((query: string) => ({
    matches: query.includes('prefers-reduced-motion: reduce') ? reduceMotion : false,
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  })) as unknown as typeof window.matchMedia;
}

function wrapper({ children }: { children: ReactNode }) {
  return <ThemeProvider>{children}</ThemeProvider>;
}

const REDUCED_DRAW_MS = 250;

describe('Oscilloscope', () => {
  let ctxStub: ReturnType<typeof makeCtxStub>;
  let getContextSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    reduceMotion = false;
    installMatchMedia();
    bufferMock.bump.mockClear();
    bufferMock.shift.mockClear();
    bufferMock.snapshot.mockClear();
    vi.useFakeTimers({
      toFake: ['setInterval', 'clearInterval', 'requestAnimationFrame', 'cancelAnimationFrame'],
    });
    ctxStub = makeCtxStub();
    getContextSpy = vi
      .spyOn(HTMLCanvasElement.prototype, 'getContext')
      .mockReturnValue(ctxStub as unknown as CanvasRenderingContext2D);
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
    getContextSpy.mockRestore();
    localStorage.clear();
  });

  it('subscribes once and unsubscribes on unmount', () => {
    const unsubscribe = vi.fn();
    const subscribe = vi.fn(() => unsubscribe);
    const { unmount } = render(<Oscilloscope subscribe={subscribe} />, { wrapper });

    expect(subscribe).toHaveBeenCalledTimes(1);
    expect(unsubscribe).not.toHaveBeenCalled();

    unmount();
    expect(unsubscribe).toHaveBeenCalledTimes(1);
  });

  it('bumps the buffer for every subscribed event', () => {
    let onEvent: (() => void) | undefined;
    const subscribe = vi.fn((cb: () => void) => {
      onEvent = cb;
      return vi.fn();
    });
    render(<Oscilloscope subscribe={subscribe} />, { wrapper });

    expect(subscribe).toHaveBeenCalledTimes(1);
    onEvent?.();
    onEvent?.();
    expect(bufferMock.bump).toHaveBeenCalledTimes(2);
  });

  it('draws the trace and shifts the window on the timer cadence', () => {
    const subscribe = vi.fn(() => vi.fn());
    render(<Oscilloscope subscribe={subscribe} />, { wrapper });

    // Nothing is drawn until the first animation frame fires.
    expect(ctxStub.moveTo).not.toHaveBeenCalled();

    vi.advanceTimersByTime(1000);

    expect(bufferMock.shift).toHaveBeenCalledTimes(1);
    expect(ctxStub.moveTo.mock.calls.length).toBeGreaterThan(0);
    expect(ctxStub.lineTo.mock.calls.length).toBeGreaterThan(0);
    // Two strokes per frame: the resting baseline hairline plus the trace.
    expect(ctxStub.stroke.mock.calls.length).toBeGreaterThanOrEqual(2);
    expect(ctxStub.arc).toHaveBeenCalled(); // phosphor head dot
  });

  it('strokes the resting baseline hairline near the bottom of the scope', () => {
    const subscribe = vi.fn(() => vi.fn());
    render(<Oscilloscope subscribe={subscribe} />, { wrapper });
    vi.advanceTimersByTime(1000);
    // The baseline is a flat rule spanning the full width at y = HEIGHT - 12.5.
    const baselineMoves = ctxStub.moveTo.mock.calls.filter(([, y]) => y === 110 - 12.5);
    expect(baselineMoves.length).toBeGreaterThan(0);
  });

  it('positions the caption absolutely at the top-right of the scope', () => {
    const subscribe = vi.fn(() => vi.fn());
    const { container } = render(<Oscilloscope subscribe={subscribe} />, { wrapper });
    const caption = container.querySelector('figcaption') as HTMLElement;
    expect(caption.className).toContain('absolute');
    expect(caption.className).toContain('right-0');
    expect(caption.className).toContain('top-0');
  });

  it('paused freezes drawing and shifting but keeps accumulating events', () => {
    let onEvent: (() => void) | undefined;
    const subscribe = vi.fn((cb: () => void) => {
      onEvent = cb;
      return vi.fn();
    });
    render(<Oscilloscope subscribe={subscribe} paused />, { wrapper });

    vi.advanceTimersByTime(2000);
    expect(bufferMock.shift).not.toHaveBeenCalled();
    expect(ctxStub.moveTo).not.toHaveBeenCalled();

    // Bumps still land while the trace is frozen.
    onEvent?.();
    expect(bufferMock.bump).toHaveBeenCalledTimes(1);
  });

  it('draws on a slow timer under prefers-reduced-motion instead of rAF', () => {
    reduceMotion = true;
    const rafSpy = vi.spyOn(globalThis, 'requestAnimationFrame');
    const subscribe = vi.fn(() => vi.fn());
    render(<Oscilloscope subscribe={subscribe} />, { wrapper });

    // Reduced-motion paints immediately on mount, then on a slow interval.
    expect(ctxStub.moveTo.mock.calls.length).toBeGreaterThan(0);
    const afterMount = ctxStub.moveTo.mock.calls.length;

    vi.advanceTimersByTime(REDUCED_DRAW_MS);
    expect(ctxStub.moveTo.mock.calls.length).toBeGreaterThan(afterMount);

    expect(rafSpy).not.toHaveBeenCalled();
    rafSpy.mockRestore();
  });
});

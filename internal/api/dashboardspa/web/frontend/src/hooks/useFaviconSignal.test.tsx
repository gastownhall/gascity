import { renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useFaviconSignal, FAVICON_ALERT_HREF, FAVICON_CALM_HREF } from './useFaviconSignal';

// gascity-dashboard-kb3 R8: favicon hysteresis is the trust instrument
// of last resort — alarm fatigue is fatal, so the swap requires the
// failing-predicate to hold for TWO consecutive snapshot cycles before
// flipping, and the same in reverse before flipping back. Single-cycle
// flips would let a transient phantom flick the tab title.

const FAVICON_ID = 'favicon';

function setupFaviconLink() {
  const link = document.createElement('link');
  link.id = FAVICON_ID;
  link.rel = 'icon';
  link.href = FAVICON_CALM_HREF;
  document.head.appendChild(link);
  return link;
}

function currentHref(): string {
  const link = document.getElementById(FAVICON_ID) as HTMLLinkElement | null;
  if (link === null) return '';
  // Tests assert against the pathname only; the prod hook appends
  // ?v=<timestamp> to defeat Safari's favicon cache (Phase 1 architect
  // finding H6) — strip the cachebust query.
  return link.href.split('?')[0] ?? '';
}

describe('useFaviconSignal', () => {
  beforeEach(() => {
    setupFaviconLink();
  });

  afterEach(() => {
    document.head.querySelectorAll(`#${FAVICON_ID}`).forEach((n) => n.remove());
    vi.useRealTimers();
  });

  it('starts calm and stays calm across cycles when failing stays 0', () => {
    const { rerender } = renderHook(
      ({ failing, cycleKey }) => useFaviconSignal({ failing, cycleKey }),
      { initialProps: { failing: 0, cycleKey: 0 } },
    );
    expect(currentHref()).toContain(FAVICON_CALM_HREF);

    rerender({ failing: 0, cycleKey: 1 });
    rerender({ failing: 0, cycleKey: 2 });
    rerender({ failing: 0, cycleKey: 3 });
    expect(currentHref()).toContain(FAVICON_CALM_HREF);
  });

  it('does NOT flip after a single failing cycle (alarm-fatigue gate)', () => {
    // Phase 1 R8 spec: failing predicate must hold TWO consecutive
    // cycles before swap. A solitary failing>0 from a flaky snapshot is
    // exactly the kind of phantom the gate exists to absorb.
    const { rerender } = renderHook(
      ({ failing, cycleKey }) => useFaviconSignal({ failing, cycleKey }),
      { initialProps: { failing: 0, cycleKey: 0 } },
    );
    rerender({ failing: 1, cycleKey: 1 });
    expect(currentHref()).toContain(FAVICON_CALM_HREF);

    rerender({ failing: 0, cycleKey: 2 });
    expect(currentHref()).toContain(FAVICON_CALM_HREF);
  });

  it('flips to alert after TWO consecutive failing cycles', () => {
    const { rerender } = renderHook(
      ({ failing, cycleKey }) => useFaviconSignal({ failing, cycleKey }),
      { initialProps: { failing: 0, cycleKey: 0 } },
    );
    rerender({ failing: 1, cycleKey: 1 });
    rerender({ failing: 1, cycleKey: 2 });
    expect(currentHref()).toContain(FAVICON_ALERT_HREF);
  });

  it('does NOT flip back after a single calm cycle (symmetric hysteresis)', () => {
    const { rerender } = renderHook(
      ({ failing, cycleKey }) => useFaviconSignal({ failing, cycleKey }),
      { initialProps: { failing: 0, cycleKey: 0 } },
    );
    rerender({ failing: 1, cycleKey: 1 });
    rerender({ failing: 1, cycleKey: 2 });
    expect(currentHref()).toContain(FAVICON_ALERT_HREF);

    // Single calm cycle is not enough.
    rerender({ failing: 0, cycleKey: 3 });
    expect(currentHref()).toContain(FAVICON_ALERT_HREF);
  });

  it('flips back to calm after TWO consecutive calm cycles', () => {
    const { rerender } = renderHook(
      ({ failing, cycleKey }) => useFaviconSignal({ failing, cycleKey }),
      { initialProps: { failing: 0, cycleKey: 0 } },
    );
    rerender({ failing: 1, cycleKey: 1 });
    rerender({ failing: 1, cycleKey: 2 });
    rerender({ failing: 0, cycleKey: 3 });
    rerender({ failing: 0, cycleKey: 4 });
    expect(currentHref()).toContain(FAVICON_CALM_HREF);
  });

  it('appends a ?v=<timestamp> cachebust on every swap (Safari favicon cache mitigation)', () => {
    vi.useFakeTimers().setSystemTime(new Date(1_700_000_000_000));
    const { rerender } = renderHook(
      ({ failing, cycleKey }) => useFaviconSignal({ failing, cycleKey }),
      { initialProps: { failing: 0, cycleKey: 0 } },
    );
    rerender({ failing: 1, cycleKey: 1 });
    rerender({ failing: 1, cycleKey: 2 });
    const link = document.getElementById(FAVICON_ID) as HTMLLinkElement;
    expect(link.href).toMatch(/\?v=\d+$/);
  });

  it('does NOT advance hysteresis on consumer re-renders that share a cycleKey (NowContext 1s ticks)', () => {
    // The whole reason cycleKey exists. The consumer re-renders every
    // second from the NowContext tick — without this gate, the favicon
    // would flip after 2 seconds of failing instead of 2 snapshots.
    const { rerender } = renderHook(
      ({ failing, cycleKey }) => useFaviconSignal({ failing, cycleKey }),
      { initialProps: { failing: 0, cycleKey: 0 } },
    );
    // Same cycle, repeated re-renders — must not advance the hysteresis.
    rerender({ failing: 1, cycleKey: 1 });
    rerender({ failing: 1, cycleKey: 1 });
    rerender({ failing: 1, cycleKey: 1 });
    rerender({ failing: 1, cycleKey: 1 });
    expect(currentHref()).toContain(FAVICON_CALM_HREF);
  });

  it('is inert when no #favicon link element exists in the document', () => {
    // The hook is lifted to App-level so it runs on every route; if a
    // future test renders App without setting up the favicon link, the
    // hook should degrade silently rather than throw.
    document.head.querySelectorAll(`#${FAVICON_ID}`).forEach((n) => n.remove());
    expect(() => {
      const r = renderHook(({ failing, cycleKey }) => useFaviconSignal({ failing, cycleKey }), {
        initialProps: { failing: 1, cycleKey: 0 },
      });
      r.rerender({ failing: 1, cycleKey: 1 });
      r.rerender({ failing: 1, cycleKey: 2 });
    }).not.toThrow();
  });
});

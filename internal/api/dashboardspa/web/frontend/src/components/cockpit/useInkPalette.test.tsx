import { act, renderHook } from '@testing-library/react';
import type { ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { ThemeProvider, useTheme } from '../../contexts/ThemeContext';
import { useInkPalette } from './useInkPalette';

// jsdom does not implement matchMedia, which ThemeProvider needs. Install a
// controllable stub so the tests can drive the real ThemeContext (per spec:
// use the real provider when practical).
let systemDark = false;
function installMatchMedia(): void {
  window.matchMedia = ((query: string) => ({
    matches: query.includes('prefers-color-scheme: dark') ? systemDark : false,
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  })) as unknown as typeof window.matchMedia;
}

const SEEDED_VARS: Record<string, string> = {
  '--fg': '18% 0.012 75',
  '--fg-muted': '42% 0.014 75',
  '--ok': '50% 0.085 150',
};

function wrapper({ children }: { children: ReactNode }) {
  return <ThemeProvider>{children}</ThemeProvider>;
}

describe('useInkPalette', () => {
  beforeEach(() => {
    systemDark = false;
    installMatchMedia();
    for (const [name, value] of Object.entries(SEEDED_VARS)) {
      document.documentElement.style.setProperty(name, value);
    }
  });

  afterEach(() => {
    document.documentElement.removeAttribute('data-theme');
    for (const name of Object.keys(SEEDED_VARS)) {
      document.documentElement.style.removeProperty(name);
    }
    localStorage.clear();
  });

  it('renders oklch strings from the token triplets', () => {
    const { result } = renderHook(() => useInkPalette(), { wrapper });
    expect(result.current('fg-muted')).toBe('oklch(42% 0.014 75)');
  });

  it('appends the alpha channel when given', () => {
    const { result } = renderHook(() => useInkPalette(), { wrapper });
    expect(result.current('ok', 0.8)).toBe('oklch(50% 0.085 150 / 0.8)');
  });

  it('keeps a stable identity across re-renders at the same theme', () => {
    const { result, rerender } = renderHook(() => useInkPalette(), { wrapper });
    const first = result.current;
    rerender();
    expect(result.current).toBe(first);
  });

  it('re-snapshots (new identity + new values) when the theme flips', () => {
    const { result } = renderHook(() => ({ ink: useInkPalette(), theme: useTheme() }), {
      wrapper,
    });
    const lightInk = result.current.ink;
    expect(result.current.ink('fg')).toBe('oklch(18% 0.012 75)');

    act(() => {
      // Simulate the stylesheet swapping the triplet under [data-theme="dark"]
      // while the operator pins dark through the real ThemeContext.
      document.documentElement.style.setProperty('--fg', '92% 0.006 75');
      result.current.theme.set('dark');
    });

    expect(result.current.ink).not.toBe(lightInk);
    expect(result.current.ink('fg')).toBe('oklch(92% 0.006 75)');
  });
});

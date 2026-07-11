import { useMemo } from 'react';

import { useTheme } from '../../contexts/ThemeContext';

// The cockpit instruments draw to <canvas>, which cannot resolve the SPA's
// OKLCH theme tokens (they live as `L% C H` triplets in CSS custom properties).
// This hook snapshots the computed triplets once per theme and hands the canvas
// a color function that composes them back into `oklch(...)` strings.

/** The semantic color tokens defined in styles/index.css. */
export const INK_TOKENS = [
  'surface',
  'surface-tint',
  'fg',
  'fg-muted',
  'fg-faint',
  'rule',
  'accent',
  'ok',
  'warn',
] as const;

export type InkToken = (typeof INK_TOKENS)[number];

/** Ink resolves a theme token (and optional alpha) to a canvas color string. */
export type Ink = (token: InkToken, alpha?: number) => string;

function snapshotTriplets(): Record<InkToken, string> {
  const style = getComputedStyle(document.documentElement);
  const out = {} as Record<InkToken, string>;
  for (const token of INK_TOKENS) {
    out[token] = style.getPropertyValue(`--${token}`).trim();
  }
  return out;
}

/**
 * useInkPalette snapshots the current theme's token triplets and returns a
 * stable `ink(token, alpha)` function for canvas drawing. It re-snapshots — and
 * returns a fresh function identity — only when the resolved theme changes,
 * subscribing through ThemeContext rather than a MutationObserver.
 */
export function useInkPalette(): Ink {
  const { resolved } = useTheme();
  return useMemo<Ink>(() => {
    const triplets = snapshotTriplets();
    return (token, alpha) =>
      alpha === undefined
        ? `oklch(${triplets[token]})`
        : `oklch(${triplets[token]} / ${alpha})`;
    // `resolved` is the snapshot key: a new theme means new computed triplets.
  }, [resolved]);
}

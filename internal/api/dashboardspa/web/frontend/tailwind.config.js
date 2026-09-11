// Tokens for the ds-research dashboard.
//
// All semantic colors resolve from CSS custom properties defined in
// styles/index.css under :root (light) and :root[data-theme="dark"]
// (or prefers-color-scheme: dark when the operator has not pinned a
// theme). The values are OKLCH triplets ("L% C H") so Tailwind's
// alpha-value substitution works via `oklch(var(--token) / <alpha>)`.

/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  // Dark mode is selector-driven on <html data-theme="dark">. The CSS
  // also reacts to prefers-color-scheme directly so SSR/no-JS isn't
  // stuck in the wrong theme.
  darkMode: ['selector', '[data-theme="dark"]'],
  theme: {
    extend: {
      colors: {
        surface: 'oklch(var(--surface) / <alpha-value>)',
        'surface-tint': 'oklch(var(--surface-tint) / <alpha-value>)',
        fg: 'oklch(var(--fg) / <alpha-value>)',
        'fg-muted': 'oklch(var(--fg-muted) / <alpha-value>)',
        'fg-faint': 'oklch(var(--fg-faint) / <alpha-value>)',
        rule: 'oklch(var(--rule) / <alpha-value>)',
        accent: 'oklch(var(--accent) / <alpha-value>)',
        ok: 'oklch(var(--ok) / <alpha-value>)',
        warn: 'oklch(var(--warn) / <alpha-value>)',
      },
      fontFamily: {
        sans: [
          '"Inter Variable"',
          'Inter',
          '-apple-system',
          'BlinkMacSystemFont',
          '"Segoe UI"',
          'system-ui',
          'sans-serif',
        ],
      },
      fontSize: {
        // Five-step scale. Body and Title share a measure; the larger
        // steps jump ratio to ~1.5 so headings carry weight in the
        // page rhythm rather than crowd the body.
        label: ['0.75rem', { lineHeight: '1.2', letterSpacing: '0.04em' }],
        body: ['0.9375rem', { lineHeight: '1.55' }],
        title: ['1rem', { lineHeight: '1.35' }],
        headline: ['1.5rem', { lineHeight: '1.15', letterSpacing: '-0.01em' }],
        display: ['2.5rem', { lineHeight: '1.05', letterSpacing: '-0.02em' }],
      },
      letterSpacing: {
        tightest: '-0.03em',
        tighter: '-0.02em',
        tight: '-0.01em',
        normal: '0',
        wide: '0.02em',
        wider: '0.04em',
        widest: '0.08em',
      },
      borderRadius: {
        none: '0',
        sm: '2px',
        DEFAULT: '4px',
        md: '6px',
        lg: '8px',
      },
      maxWidth: {
        dashboard: '1280px',
        prose: '70ch',
        reading: '65ch',
      },
      transitionTimingFunction: {
        'out-quart': 'cubic-bezier(0.25, 1, 0.5, 1)',
      },
    },
  },
  plugins: [],
};

import { expect } from 'vitest';

// Shared assertion for DESIGN.md "The One Mark Rule": the maroon
// (Stuck Maroon / `text-accent`) class appears at most once per
// visible viewport. Every route-level test that renders a page or
// subtree the operator sees in one go should call this once after
// the render settles.
//
// Mechanism: counts elements carrying Tailwind's `text-accent`
// utility. That's the only signature jsdom can resolve reliably —
// it does not run the Tailwind/PostCSS pipeline, so computed-style
// inspection of `oklch(var(--accent))` won't work. Every current
// call site in the codebase paints the mark via `text-accent`, so
// this is also a complete check. If a future component introduces
// a semantic alias (e.g. a class that compiles down to text-accent
// without using the name), this helper would miss it — at that
// point extend the selector here rather than re-inlining the count.
//
// Convention: new routes/components that can paint the mark must
// have a vitest test that calls `assertAtMostOneMark(container)`
// in the same commit as the route — the project's mechanical
// One Mark Rule gate. The manual "Greyscale Test" (snap.mjs
// desaturated review) stays human-judged per DESIGN.md and is
// not mechanized here.

export function assertAtMostOneMark(container: Element): void {
  const marks = container.querySelectorAll('.text-accent');
  expect(
    marks.length,
    `One Mark Rule violated: expected at most 1 .text-accent in container, found ${marks.length}.`,
  ).toBeLessThanOrEqual(1);
}

// Strict-equality sibling of assertAtMostOneMark for call sites whose
// contract is "exactly one maroon mark" — e.g. an aggregate badge whose
// presence is the test's load-bearing claim. Use this when 0 marks would
// be a real regression (a broken aggregator painting nothing), not a
// calm-state expectation. Same detection mechanism and jsdom caveat as
// assertAtMostOneMark above.
export function assertExactlyOneMark(container: Element): void {
  const marks = container.querySelectorAll('.text-accent');
  expect(
    marks.length,
    `One Mark Rule violated: expected exactly 1 .text-accent in container, found ${marks.length}.`,
  ).toBe(1);
}

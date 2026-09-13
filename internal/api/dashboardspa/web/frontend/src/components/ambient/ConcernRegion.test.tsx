import { cleanup, render } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { ConcernRegion } from './ConcernRegion';

// dw8 — focused render assertion that the concern region carries the
// `id="needs-you"` fragment anchor R13 mandates
// (specs/plans/workflow-observability-prd.md:386-388). The rest of the
// concern-region behaviour (row links, reason labels, opacity-on-absence)
// is covered by routes/AmbientHome.test.tsx.

describe('ConcernRegion — needs-you fragment anchor (dw8)', () => {
  afterEach(() => {
    cleanup();
  });

  it('wraps the list in <section id="needs-you"> for /#needs-you scroll', () => {
    const { container } = render(<ConcernRegion rows={[]} />);
    const section = container.querySelector('section#needs-you');
    expect(section).not.toBeNull();
    // The opacity-managed list lives INSIDE the anchor wrapper so the
    // existing concern-region testid keeps working for the empty-state
    // assertion at routes/AmbientHome.test.tsx.
    const region = section?.querySelector('[data-testid="concern-region"]');
    expect(region).not.toBeNull();
  });
});

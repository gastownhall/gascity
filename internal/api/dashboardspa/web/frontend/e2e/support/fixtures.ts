import { test as base } from '@playwright/test';
import { assertNoErrorBoundary, watchClientErrors } from './renderGuards';

/**
 * The render smoke's two negative guards — NO React error boundary and NO
 * client-error POST — run automatically after EVERY test via this auto fixture,
 * not inline at the end of each spec body.
 *
 * Attaching the client-error watch in fixture SETUP (before the test body
 * navigates) means no report is missed. Asserting in fixture TEARDOWN, after a
 * short settle, also catches LATE async reporters: an SSE decode error or a
 * deferred render that posts to /api/client-errors after the last positive
 * assertion already resolved would slip past an inline end-of-body check, but not
 * past a teardown check that first lets the microtask/network queue drain.
 *
 * Each spec therefore asserts only positive seeded content; the crash guards are
 * enforced here uniformly for all specs.
 */
export const test = base.extend<{ renderGuards: void }>({
  renderGuards: [
    async ({ page }, use) => {
      const watch = watchClientErrors(page);
      await use();
      // Let any late async client-error reporter flush before asserting.
      await page.waitForTimeout(500);
      await assertNoErrorBoundary(page);
      watch.assertClean();
    },
    { auto: true },
  ],
});

export { expect } from '@playwright/test';

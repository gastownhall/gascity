import { expect, type Page } from '@playwright/test';
import { CLIENT_ERROR_ENDPOINT, ERROR_BOUNDARY_TEXT } from '../fixtures/expected';

/**
 * ClientErrorWatch records any client-error POSTs the SPA fired while a spec
 * drove a route. The dashboard posts to /api/client-errors on a render failure
 * (lib/clientErrorReporting.ts, via components/ErrorBoundary.tsx), so a
 * non-empty list means a view threw — the exact failure Layer B exists to catch.
 */
export interface ClientErrorWatch {
  /** Request URLs (paths) the SPA POSTed to the client-error endpoint. */
  readonly hits: readonly string[];
  /** Assert no client-error POST fired. */
  assertClean(): void;
}

/**
 * watchClientErrors attaches a passive request listener (page.on('request'))
 * that records every POST to the client-error endpoint. It does NOT intercept or
 * mock the request — the seeded plane serves /api/client-errors itself; the
 * listener only observes. Call it BEFORE navigating so no report is missed. A
 * recorded hit means the SPA caught a render error and reported it, which
 * assertClean() then fails on.
 */
export function watchClientErrors(page: Page): ClientErrorWatch {
  const hits: string[] = [];
  page.on('request', (req) => {
    if (req.method() === 'POST' && new URL(req.url()).pathname === CLIENT_ERROR_ENDPOINT) {
      hits.push(new URL(req.url()).pathname);
    }
  });
  return {
    hits,
    assertClean() {
      expect(
        hits,
        `SPA POSTed ${hits.length} client-error report(s) to ${CLIENT_ERROR_ENDPOINT}; a view crashed`,
      ).toEqual([]);
    },
  };
}

/**
 * assertNoErrorBoundary fails if the React error-boundary crash fallback
 * (components/ErrorBoundary.tsx: "Dashboard view failed.") is showing. The
 * boundary renders with role="alert", but the heading text is the stable,
 * user-visible signal.
 */
export async function assertNoErrorBoundary(page: Page): Promise<void> {
  await expect(
    page.getByText(ERROR_BOUNDARY_TEXT),
    'the React error boundary crash fallback is showing — a view threw during render',
  ).toHaveCount(0);
}

/**
 * gotoCityRoute navigates to a city-scoped client route and waits for the SPA to
 * resolve the active city and mount the router (CityBootstrap fetches /v0/cities
 * then mounts under the /city/{name} basename). `suffix` is the in-app path
 * (e.g. '/runs', '/runs/run-anchor', '' for home). It leaves a trailing check
 * that the bootstrap "Resolving city…" shell is gone.
 */
export async function gotoCityRoute(page: Page, cityBase: string, suffix: string): Promise<void> {
  // Trailing slash on the bare base so CityBootstrap parses the city segment
  // and mounts the router rather than treating it as a bare-"/" first-city
  // redirect.
  const path = suffix === '' ? `${cityBase}/` : `${cityBase}${suffix}`;
  await page.goto(path);
  await expect(page.getByText('Resolving city…')).toHaveCount(0, { timeout: 15_000 });
}

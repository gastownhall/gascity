import type { Page } from '@playwright/test';

import { ANCHOR_FORMULA, ANCHOR_RUN_ID, CITY_BASE, CITY_NAME } from './fixtures/expected';
import { gotoCityRoute } from './support/renderGuards';
import { expect, test } from './support/fixtures';

// Client-side navigation smoke. render-smoke.spec.ts deep-links every route with
// page.goto — one full document load per route — which proves each route
// resolves, but would also pass if the router stopped intercepting clicks and
// every navigation fell back to a native page load (the supervisor serves the
// SPA shell at every path). These specs drive the router itself under the
// production <BrowserRouter basename="/city/{name}">: header <NavLink>s, in-content
// <Link>s with encoded path params and query strings, useNavigate,
// useSearchParams' setter, back/forward (popstate), and the catch-all route.
//
// Each spec marks the document after its first load and asserts the mark
// survives every navigation: a full document load would reset it.

type MarkedWindow = Window & { __gcNavigationMark?: boolean };

async function markDocument(page: Page): Promise<void> {
  await page.evaluate(() => {
    (window as MarkedWindow).__gcNavigationMark = true;
  });
}

async function expectSameDocument(page: Page): Promise<void> {
  const marked = await page.evaluate(() => (window as MarkedWindow).__gcNavigationMark === true);
  expect(marked, 'navigation reloaded the document instead of routing client-side').toBe(true);
}

function inCity(path: string): (url: URL) => boolean {
  return (url) => url.pathname === `${CITY_BASE}${path}`;
}

// Header nav entries in render order. Names are prefix regexes: a nav link's
// accessible name also carries its attention badge count (e.g. "Beads3").
const NAV_ROUTES = [
  { name: /^Agents/, path: '/agents', heading: 'Agents' },
  { name: /^Beads/, path: '/beads', heading: 'Beads' },
  { name: /^Runs/, path: '/runs', heading: 'Runs' },
  { name: /^Mail/, path: '/mail', heading: 'Mail' },
  { name: /^Activity/, path: '/activity', heading: 'Activity' },
  { name: /^Health/, path: '/health', heading: 'Health' },
] as const;

test.describe('dashboard client-side navigation', () => {
  test('header nav links route client-side under the city basename', async ({ page }) => {
    const routerWarnings: string[] = [];
    page.on('console', (message) => {
      if (/react router|no routes matched|<Router>/i.test(message.text())) {
        routerWarnings.push(message.text());
      }
    });

    await gotoCityRoute(page, CITY_BASE, '');
    await expect(page.getByRole('heading', { name: 'Home', level: 1 })).toBeVisible();
    await markDocument(page);

    const nav = page.locator('header nav');
    const home = nav.getByRole('link', { name: 'Home', exact: true });
    await expect(home).toHaveAttribute('aria-current', 'page');

    for (const route of NAV_ROUTES) {
      const link = nav.getByRole('link', { name: route.name });
      // The basename prefixes every rendered href.
      await expect(link).toHaveAttribute('href', `${CITY_BASE}${route.path}`);
      await link.click();
      await expect(page).toHaveURL(inCity(route.path));
      await expect(page.getByRole('heading', { name: route.heading, level: 1 })).toBeVisible();
      await expect(link).toHaveAttribute('aria-current', 'page');
      // `end` keeps Home inactive on every nested route.
      await expect(home).not.toHaveAttribute('aria-current', 'page');
      await expectSameDocument(page);
    }

    // Back and forward replay history entries through popstate.
    await page.goBack();
    await expect(page).toHaveURL(inCity('/activity'));
    await expect(page.getByRole('heading', { name: 'Activity', level: 1 })).toBeVisible();
    await page.goForward();
    await expect(page).toHaveURL(inCity('/health'));
    await expect(page.getByRole('heading', { name: 'Health', level: 1 })).toBeVisible();
    await expectSameDocument(page);

    await home.click();
    await expect(page.getByRole('heading', { name: 'Home', level: 1 })).toBeVisible();
    await expect(home).toHaveAttribute('aria-current', 'page');
    await expectSameDocument(page);

    expect(routerWarnings).toEqual([]);
  });

  test('agent links carry an encoded slug and useNavigate returns to the list', async ({
    page,
  }) => {
    await gotoCityRoute(page, CITY_BASE, '/agents');
    await expect(page.getByRole('heading', { name: 'Agents', level: 1 })).toBeVisible();
    await markDocument(page);
    // The seeded pool members are idle; the default running-only filter hides them.
    await page.getByRole('checkbox', { name: 'running' }).uncheck();

    // Pool members are rig-qualified ("demo/builder-1"): the link encodes the
    // slash into the :slug segment and the detail page reads it back decoded.
    const member = page.locator('main').getByRole('link', { name: /builder-1/ });
    await expect(member).toHaveAttribute('href', `${CITY_BASE}/agents/demo%2Fbuilder-1`);
    await member.click();
    await expect(page).toHaveURL(inCity('/agents/demo%2Fbuilder-1'));
    // An idle pool member has no live session, so the detail page renders its
    // no-session state, which echoes the decoded slug.
    await expect(page.getByText('No session matches demo/builder-1.')).toBeVisible();
    await expectSameDocument(page);

    // The no-session state's back button navigates with useNavigate.
    await page.getByRole('button', { name: '← Agents' }).click();
    await expect(page).toHaveURL(inCity('/agents'));
    await expect(page.getByRole('heading', { name: 'Agents', level: 1 })).toBeVisible();
    await expectSameDocument(page);
  });

  test('run links keep their query string and the history toggle sets search params', async ({
    page,
  }) => {
    await gotoCityRoute(page, CITY_BASE, '/runs');
    await expect(page.getByRole('heading', { name: 'Runs', level: 1 })).toBeVisible();
    await markDocument(page);

    // useSearchParams' setter adds ?history=1; back removes it again.
    await page.getByRole('button', { name: /^Show \d+ completed formula runs\.$/ }).click();
    await expect(page).toHaveURL(
      (url) => url.pathname === `${CITY_BASE}/runs` && url.searchParams.get('history') === '1',
    );
    await expect(page.getByRole('region', { name: 'Historical runs' })).toBeVisible();
    await page.goBack();
    await expect(page).toHaveURL(
      (url) => url.pathname === `${CITY_BASE}/runs` && !url.searchParams.has('history'),
    );
    await expect(page.getByRole('region', { name: 'Historical runs' })).toHaveCount(0);
    await expectSameDocument(page);

    // A lane links to its run detail with the run's scope in the query string.
    await page.locator('main').getByRole('link', { name: ANCHOR_FORMULA }).first().click();
    await expect(page).toHaveURL(
      (url) =>
        url.pathname === `${CITY_BASE}/runs/${ANCHOR_RUN_ID}` &&
        url.searchParams.get('scope_kind') === 'city' &&
        url.searchParams.get('scope_ref') === CITY_NAME,
    );
    await expect(page.getByRole('heading', { name: ANCHOR_FORMULA, level: 1 })).toBeVisible();
    await expectSameDocument(page);

    // The detail page's breadcrumb returns to the list.
    await page.locator('main').getByRole('link', { name: 'Runs', exact: true }).click();
    await expect(page).toHaveURL(inCity('/runs'));
    await expect(page.getByRole('heading', { name: 'Runs', level: 1 })).toBeVisible();
    await expectSameDocument(page);
  });

  test('an unknown in-app path renders the catch-all route and the nav recovers', async ({
    page,
  }) => {
    await gotoCityRoute(page, CITY_BASE, '/no-such-route');
    await expect(page.getByRole('heading', { name: 'Page not found', level: 1 })).toBeVisible();
    await markDocument(page);

    await page
      .locator('header nav')
      .getByRole('link', { name: /^Agents/ })
      .click();
    await expect(page).toHaveURL(inCity('/agents'));
    await expect(page.getByRole('heading', { name: 'Agents', level: 1 })).toBeVisible();
    await expectSameDocument(page);
  });
});

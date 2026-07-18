import { expect, test, type Page } from '@playwright/test';
import {
  AGENT_NAME,
  ANCHOR_FORMULA,
  ANCHOR_RUN_ID,
  CITY_BASE,
  CITY_NAME,
  COMPLETED_FORMULA,
  COMPLETED_PHASE_LABEL,
  COMPLETED_RUN_ID,
  COMPLETED_STEP_APPROVE,
  MAIL_SUBJECT,
  WORK_BEAD_ID,
} from './fixtures/expected';
import { assertNoErrorBoundary, gotoCityRoute, watchClientErrors } from './support/renderGuards';

// Layer B render smoke (.dashport-plan/04-e2e.md): drive Chromium to each
// dashboard route against the seeded fake supervisor (test/dashport/cmd/
// fakesupervisor over the shared testdata/dashport corpus) and assert three
// things per route:
//   (a) seeded content renders (not a spinner, not an empty state),
//   (b) NO React error boundary is shown (components/ErrorBoundary.tsx), and
//   (c) NO client-error POST fires (lib/clientErrorReporting.ts → /api/client-errors).
// The three together are the render-truth backstop for the run-view break class:
// a projection that decodes wrong throws in render, trips the boundary, and
// posts a client error — all three assertions fail.
//
// Every spec opens the client-error watch BEFORE navigating so no report is
// missed, then asserts positive seeded content, then the two negative guards.

/** Run the shared no-crash guards after a route's positive content asserted. */
async function assertHealthy(page: Page, watch: ReturnType<typeof watchClientErrors>) {
  await assertNoErrorBoundary(page);
  watch.assertClean();
}

test.describe('dashboard render smoke over the seeded corpus', () => {
  test('ambient home renders with seeded status', async ({ page }) => {
    const watch = watchClientErrors(page);
    await gotoCityRoute(page, CITY_BASE, '');
    // AmbientHome's PageHeader title is "Home" (routes/AmbientHome.tsx).
    await expect(page.getByRole('heading', { name: 'Home', level: 1 })).toBeVisible();
    await assertHealthy(page, watch);
  });

  test('runs list renders the seeded run', async ({ page }) => {
    const watch = watchClientErrors(page);
    await gotoCityRoute(page, CITY_BASE, '/runs');
    await expect(page.getByRole('heading', { name: 'Runs', level: 1 })).toBeVisible();
    // The seeded run's formula name labels its lane (runs/summary title).
    await expect(page.getByText(ANCHOR_FORMULA).first()).toBeVisible();
    await assertHealthy(page, watch);
  });

  test('run detail (the regression view) renders the seeded lanes/nodes', async ({ page }) => {
    const watch = watchClientErrors(page);
    await gotoCityRoute(page, CITY_BASE, `/runs/${ANCHOR_RUN_ID}`);
    // FormulaRunDetail's PageHeader title is the run's formula name
    // (routes/FormulaRunDetail.tsx: title={detail?.title}). A projection break
    // on /workflow/{id} or /runs/{id}/detail leaves this on the skeleton title
    // or trips the boundary.
    await expect(page.getByRole('heading', { name: ANCHOR_FORMULA, level: 1 })).toBeVisible();
    await assertHealthy(page, watch);
  });

  test('agents renders the seeded agent/rig', async ({ page }) => {
    const watch = watchClientErrors(page);
    await gotoCityRoute(page, CITY_BASE, '/agents');
    await expect(page.getByRole('heading', { name: 'Agents', level: 1 })).toBeVisible();
    // The seeded pool agents are idle (state=stopped), and the view defaults to
    // a running-only filter (routes/Agents.tsx). Turn it off so the seeded rows
    // render, then assert the seeded agent name (pool member "<rig>/<agent>-N")
    // appears — proof the roster projected, not just the summary count.
    await page.getByRole('checkbox', { name: 'running' }).uncheck();
    await expect(page.getByText(AGENT_NAME).first()).toBeVisible();
    await assertHealthy(page, watch);
  });

  test('beads renders the seeded work bead', async ({ page }) => {
    const watch = watchClientErrors(page);
    await gotoCityRoute(page, CITY_BASE, '/beads');
    await expect(page.getByRole('heading', { name: 'Beads', level: 1 })).toBeVisible();
    await expect(page.getByText(WORK_BEAD_ID).first()).toBeVisible();
    await assertHealthy(page, watch);
  });

  test('mail renders the seeded message', async ({ page }) => {
    const watch = watchClientErrors(page);
    await gotoCityRoute(page, CITY_BASE, '/mail');
    await expect(page.getByRole('heading', { name: 'Mail', level: 1 })).toBeVisible();
    // The seeded message is addressed builder→reviewer, so the default Inbox
    // (scoped to the operator alias) hides it. Switch to the "All" box, which
    // lists every message, then assert the seeded subject row renders.
    await page.getByRole('button', { name: 'All', exact: true }).click();
    await expect(page.getByText(MAIL_SUBJECT).first()).toBeVisible();
    await assertHealthy(page, watch);
  });

  test('activity renders the seeded event stream', async ({ page }) => {
    const watch = watchClientErrors(page);
    await gotoCityRoute(page, CITY_BASE, '/activity');
    await expect(page.getByRole('heading', { name: 'Activity', level: 1 })).toBeVisible();
    // The corpus loader stamps the seeded events with the current time so they
    // land inside the Activity default window; the supervisor-events table then
    // renders real rows. Assert a seeded event row: the anchor run's subject and
    // the bead.created event type both come straight from the seeded event log,
    // so this fails if the events feed / projection stops rendering.
    const eventsTable = page.getByRole('table').first();
    await expect(eventsTable.getByText(ANCHOR_RUN_ID).first()).toBeVisible();
    await expect(eventsTable.getByText('bead.created').first()).toBeVisible();
    await assertHealthy(page, watch);
  });

  test('health renders the system/local-tools widgets', async ({ page }) => {
    const watch = watchClientErrors(page);
    await gotoCityRoute(page, CITY_BASE, '/health');
    await expect(page.getByRole('heading', { name: 'Health', level: 1 })).toBeVisible();
    // The synopsis is derived from the seeded city's /health projection
    // ("Supervisor healthy on <city>, uptime ..."), so the seeded city name in
    // it proves the health read wired through — a static header would not carry
    // it. The "Tool versions" section is a real widget the local-tools plane
    // fills, confirming the BFF health plane rendered too.
    await expect(
      page.getByText(`Supervisor healthy on ${CITY_NAME}`, { exact: false }),
    ).toBeVisible();
    await expect(page.getByText('Tool versions', { exact: false }).first()).toBeVisible();
    await assertHealthy(page, watch);
  });

  // Close-side scenario (the completed run "run-done"): the corpus seeds a
  // SECOND run whose root and both steps are all closed, capped by a
  // molecule.resolved event. These four specs assert the close-side data renders
  // populated on every surface it reaches — the historical runs list, the
  // terminal run detail, the closed beads view, and the close-edge activity feed
  // — the render-truth half of Layer A's TestCompletedRunProjection.

  test('runs list history reveals the completed run as terminal', async ({ page }) => {
    const watch = watchClientErrors(page);
    // history=1 reveals the historical section directly; completed runs are
    // hidden from the default active view by design (routes/Runs.tsx).
    await gotoCityRoute(page, CITY_BASE, '/runs?history=1');
    await expect(page.getByRole('heading', { name: 'Runs', level: 1 })).toBeVisible();
    // The completed run lives ONLY in the Historical region — scope every
    // assertion to it so a leak into the active lanes cannot satisfy the spec.
    const history = page.getByRole('region', { name: 'Historical runs' });
    // The lane renders the run root id, its formula title, and a terminal phase
    // label ("complete"); the active anchor run carries none of these here.
    await expect(history.getByText(COMPLETED_RUN_ID).first()).toBeVisible();
    await expect(history.getByText(COMPLETED_FORMULA).first()).toBeVisible();
    await expect(history.getByText(COMPLETED_PHASE_LABEL, { exact: true }).first()).toBeVisible();
    await assertHealthy(page, watch);
  });

  test('completed run detail renders terminal lanes/nodes', async ({ page }) => {
    const watch = watchClientErrors(page);
    await gotoCityRoute(page, CITY_BASE, `/runs/${COMPLETED_RUN_ID}`);
    // The detail h1 is the completed run's formula name — distinct from the
    // active run's, so this addresses the completed run unambiguously.
    await expect(page.getByRole('heading', { name: COMPLETED_FORMULA, level: 1 })).toBeVisible();
    // Its closed step nodes render with a terminal "done" status: the step node
    // title proves the lanes/nodes projected (not a skeleton/empty state) and the
    // "done" status label proves they read terminal, not in-progress.
    await expect(page.getByText('approve').first()).toBeVisible();
    await expect(page.getByText('done').first()).toBeVisible();
    await assertHealthy(page, watch);
  });

  test('beads reveals the completed run closed step', async ({ page }) => {
    const watch = watchClientErrors(page);
    await gotoCityRoute(page, CITY_BASE, '/beads');
    await expect(page.getByRole('heading', { name: 'Beads', level: 1 })).toBeVisible();
    // Closed beads are hidden by default; the "closed" status chip widens the
    // fetch (all=true) and narrows the board to closed rows. The completed run
    // surfaces via its closed TASK step — its molecule root is filtered out of
    // the engineering-types board (routes/Beads.tsx, supervisor/beadReads.ts).
    await page.getByRole('button', { name: 'closed' }).click();
    await expect(page.getByText(COMPLETED_STEP_APPROVE).first()).toBeVisible();
    await assertHealthy(page, watch);
  });

  test('activity renders the completed run close edges', async ({ page }) => {
    const watch = watchClientErrors(page);
    await gotoCityRoute(page, CITY_BASE, '/activity');
    await expect(page.getByRole('heading', { name: 'Activity', level: 1 })).toBeVisible();
    // The completed run's close-side events project as raw rows in the events
    // table (routes/Activity.tsx renders event.type verbatim): a bead.closed
    // close edge and the molecule.resolved resolution, both keyed to run-done.
    const eventsTable = page.getByRole('table').first();
    await expect(eventsTable.getByText('bead.closed').first()).toBeVisible();
    await expect(eventsTable.getByText('molecule.resolved').first()).toBeVisible();
    await expect(eventsTable.getByText(COMPLETED_RUN_ID).first()).toBeVisible();
    await assertHealthy(page, watch);
  });
});

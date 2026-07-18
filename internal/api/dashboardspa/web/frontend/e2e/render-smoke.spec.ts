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
  RIG_NAME,
  WORK_BEAD_ID,
  WORK_BEAD_TITLE,
} from './fixtures/expected';
import { gotoCityRoute } from './support/renderGuards';
import { expect, test } from './support/fixtures';

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
// Guards (b) and (c) run automatically after every test via the auto renderGuards
// fixture (support/fixtures.ts), so each spec below asserts only POSITIVE seeded
// content. Every positive assertion is written to fail if its component renders
// empty — a bare heading or a title that also renders over an empty view is not
// enough; specs anchor on seeded-data-derived content and scope id/status matches
// so a stray substring elsewhere in the DOM cannot satisfy them.

test.describe('dashboard render smoke over the seeded corpus', () => {
  test('ambient home renders with seeded status', async ({ page }) => {
    await gotoCityRoute(page, CITY_BASE, '');
    await expect(page.getByRole('heading', { name: 'Home', level: 1 })).toBeVisible();
    // The h1 "Home" renders identically in the loading, error, and
    // runs-source-error branches, so assert seeded synopsis content that appears
    // ONLY once the home data loaded: the city name + the census-derived active
    // run count ("1 running" = the one in-progress anchor run), and the
    // runs-in-flight tile carrying the same count.
    await expect(
      page.getByText('dashport-city · 0 active sessions · 1 running', { exact: false }),
    ).toBeVisible();
    await expect(
      page
        .getByRole('region', { name: 'runs in flight · canonical state' })
        .getByRole('link', { name: 'running: 1' }),
    ).toBeVisible();
    // A healthy home shows no alert; the error branches render one.
    await expect(page.getByRole('alert')).toHaveCount(0);
  });

  test('runs list renders the seeded run', async ({ page }) => {
    await gotoCityRoute(page, CITY_BASE, '/runs');
    await expect(page.getByRole('heading', { name: 'Runs', level: 1 })).toBeVisible();
    // The seeded run's formula name labels its lane (runs/summary title).
    await expect(page.getByText(ANCHOR_FORMULA).first()).toBeVisible();
  });

  test('run detail (the regression view) renders the seeded lanes/nodes', async ({ page }) => {
    await gotoCityRoute(page, CITY_BASE, `/runs/${ANCHOR_RUN_ID}`);
    // FormulaRunDetail's PageHeader title is the run's formula name
    // (routes/FormulaRunDetail.tsx: title={detail?.title}).
    await expect(page.getByRole('heading', { name: ANCHOR_FORMULA, level: 1 })).toBeVisible();
    // The h1 renders identically over an EMPTY diagram, so assert seeded node
    // content: the synopsis reports the projected node count, and the Formula
    // Graph renders one button per node ("<step> step <status>"). A projection
    // break on /workflow/{id} or /runs/{id}/detail drops these even though the
    // title still resolves.
    await expect(page.getByText('3 nodes.', { exact: false })).toBeVisible();
    const graph = page.getByRole('region', { name: 'Formula run graph' });
    await expect(graph.getByRole('button', { name: /preflight step/ })).toBeVisible();
    await expect(graph.getByRole('button', { name: /review step/ })).toBeVisible();
  });

  test('agents renders the seeded agent/rig', async ({ page }) => {
    await gotoCityRoute(page, CITY_BASE, '/agents');
    await expect(page.getByRole('heading', { name: 'Agents', level: 1 })).toBeVisible();
    // The seeded pool agents are idle (state=stopped), and the view defaults to
    // a running-only filter (routes/Agents.tsx). Turn it off so the seeded rows
    // render, then assert the seeded agent name and rig name (pool members render
    // as "<rig>/<agent>-N") — proof the roster projected, not just a count.
    await page.getByRole('checkbox', { name: 'running' }).uncheck();
    await expect(page.getByText(AGENT_NAME).first()).toBeVisible();
    await expect(page.getByText(RIG_NAME).first()).toBeVisible();
  });

  test('beads renders the seeded work bead', async ({ page }) => {
    await gotoCityRoute(page, CITY_BASE, '/beads');
    await expect(page.getByRole('heading', { name: 'Beads', level: 1 })).toBeVisible();
    // Exact id match so 'work-1' cannot be satisfied by a longer id substring.
    await expect(page.getByText(WORK_BEAD_ID, { exact: true }).first()).toBeVisible();
    // The seeded work bead's title renders on its card — proof the row, not just
    // its id chip, projected.
    await expect(page.getByText(WORK_BEAD_TITLE, { exact: false }).first()).toBeVisible();
  });

  test('mail renders the seeded message', async ({ page }) => {
    await gotoCityRoute(page, CITY_BASE, '/mail');
    await expect(page.getByRole('heading', { name: 'Mail', level: 1 })).toBeVisible();
    // The seeded message is addressed builder→reviewer, so the default Inbox
    // (scoped to the operator alias) hides it. Switch to the "All" box, which
    // lists every message, then assert the seeded subject row renders.
    await page.getByRole('button', { name: 'All', exact: true }).click();
    await expect(page.getByText(MAIL_SUBJECT).first()).toBeVisible();
  });

  test('activity renders the seeded event stream', async ({ page }) => {
    await gotoCityRoute(page, CITY_BASE, '/activity');
    await expect(page.getByRole('heading', { name: 'Activity', level: 1 })).toBeVisible();
    // Scope to the named events table (three tables render: Supervisor events,
    // Deploy history, Git commits). Assert a seeded row: the anchor run's exact
    // subject id and the bead.created event type, both straight from the seeded
    // event log — this fails if the events feed / projection stops rendering.
    const eventsTable = page.getByRole('table', { name: 'Supervisor events' });
    await expect(eventsTable.getByText(ANCHOR_RUN_ID, { exact: true }).first()).toBeVisible();
    await expect(eventsTable.getByText('bead.created', { exact: true }).first()).toBeVisible();
  });

  test('health renders the system/local-tools widgets', async ({ page }) => {
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
  });

  // Close-side scenario (the completed run "run-done"): the corpus seeds a
  // SECOND run whose root and both steps are all closed, capped by a
  // molecule.resolved event. These four specs assert the close-side data renders
  // populated on every surface it reaches — the historical runs list, the
  // terminal run detail, the closed beads view, and the close-edge activity feed
  // — the render-truth half of Layer A's TestCompletedRunProjection.

  test('runs list history reveals the completed run as terminal', async ({ page }) => {
    // history=1 reveals the historical section directly; completed runs are
    // hidden from the default active view by design (routes/Runs.tsx).
    await gotoCityRoute(page, CITY_BASE, '/runs?history=1');
    await expect(page.getByRole('heading', { name: 'Runs', level: 1 })).toBeVisible();
    // The completed run lives ONLY in the Historical region — scope every
    // assertion to it so a leak into the active lanes cannot satisfy the spec.
    const history = page.getByRole('region', { name: 'Historical runs' });
    // The lane renders the run root id, its formula title, and a terminal phase
    // label ("complete"); the active anchor run carries none of these here.
    await expect(history.getByText(COMPLETED_RUN_ID, { exact: true }).first()).toBeVisible();
    await expect(history.getByText(COMPLETED_FORMULA, { exact: true }).first()).toBeVisible();
    await expect(history.getByText(COMPLETED_PHASE_LABEL, { exact: true }).first()).toBeVisible();
  });

  test('completed run detail renders terminal lanes/nodes', async ({ page }) => {
    await gotoCityRoute(page, CITY_BASE, `/runs/${COMPLETED_RUN_ID}`);
    // The detail h1 is the completed run's formula name — distinct from the
    // active run's, so this addresses the completed run unambiguously.
    await expect(page.getByRole('heading', { name: COMPLETED_FORMULA, level: 1 })).toBeVisible();
    // Terminal proof via the synopsis: "3 nodes. 3 done." only renders when ALL
    // three nodes read terminal (a projection that leaves a node in-progress
    // shows "N done." with N<3). A bare getByText('done') is VACUOUS here — it
    // substring-matches the "run-done" Root metadata cell (rendered
    // unconditionally) and this synopsis's own "3 done." even if no node is
    // terminal.
    await expect(page.getByText('3 nodes. 3 done.', { exact: false })).toBeVisible();
    // And a real graph node: the "approve" step button, with its terminal status
    // scoped to that node so a stray "done" elsewhere in the DOM cannot satisfy
    // it. The node's status text is "✓ done".
    const graph = page.getByRole('region', { name: 'Formula run graph' });
    const approveNode = graph.getByRole('button', { name: /approve step/ });
    await expect(approveNode).toBeVisible();
    await expect(approveNode.getByText('done')).toBeVisible();
  });

  test('beads reveals the completed run closed step', async ({ page }) => {
    await gotoCityRoute(page, CITY_BASE, '/beads');
    await expect(page.getByRole('heading', { name: 'Beads', level: 1 })).toBeVisible();
    // Closed beads are hidden by default; the "closed" status chip widens the
    // fetch (all=true) and narrows the board to closed rows. The completed run
    // surfaces via its closed TASK step — its molecule root is filtered out of
    // the engineering-types board (routes/Beads.tsx, supervisor/beadReads.ts).
    // Exact id match so a longer id substring cannot satisfy it.
    await page.getByRole('button', { name: 'closed' }).click();
    await expect(page.getByText(COMPLETED_STEP_APPROVE, { exact: true }).first()).toBeVisible();
  });

  test('activity renders the completed run close edges', async ({ page }) => {
    await gotoCityRoute(page, CITY_BASE, '/activity');
    await expect(page.getByRole('heading', { name: 'Activity', level: 1 })).toBeVisible();
    // The completed run's close-side events project as raw rows in the named
    // events table (routes/Activity.tsx renders event.type verbatim): a
    // bead.closed close edge and the molecule.resolved resolution, both keyed to
    // the exact run-done subject (not run-done.analyze/approve step subjects).
    const eventsTable = page.getByRole('table', { name: 'Supervisor events' });
    await expect(eventsTable.getByText('bead.closed', { exact: true }).first()).toBeVisible();
    await expect(eventsTable.getByText('molecule.resolved', { exact: true }).first()).toBeVisible();
    await expect(eventsTable.getByText(COMPLETED_RUN_ID, { exact: true }).first()).toBeVisible();
  });
});

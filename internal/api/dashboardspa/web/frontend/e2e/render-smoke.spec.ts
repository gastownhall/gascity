import {
  AGENT_ASSIGNED_BEAD_ID,
  AGENT_ASSIGNED_BEAD_TITLE,
  AGENT_NAME,
  AGENT_REPLY_BODY,
  AGENT_SESSION_SLUG,
  AGENT_SESSION_STATE,
  AGENT_SESSION_TEMPLATE,
  ANCHOR_FORMULA,
  ANCHOR_RUN_ID,
  CITY_BASE,
  CITY_NAME,
  COMPLETED_FORMULA,
  COMPLETED_PHASE_LABEL,
  COMPLETED_RUN_ID,
  COMPLETED_STEP_APPROVE,
  MAIL_SUBJECT,
  OPERATOR_MAIL_BODY,
  OPERATOR_MAIL_SUBJECT,
  REVIEW_BEAD_ID,
  REVIEW_BEAD_TITLE,
  REVIEW_DEP_TARGET_ID,
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
  test('cockpit home renders populated instruments and canonical-state sections', async ({
    page,
  }) => {
    await gotoCityRoute(page, CITY_BASE, '');
    await expect(page.getByRole('heading', { name: 'Home', level: 1 })).toBeVisible();
    // The h1 "Home" renders identically in the loading, error, and
    // runs-source-error branches, so assert seeded synopsis content that appears
    // ONLY once the home data loaded: the city name + the status-derived active
    // session count ("1 active sessions" = the one seeded session bead) and the
    // census-derived running-run count ("1 running" = the in-progress anchor run).
    await expect(
      page.getByText('dashport-city · 1 active sessions · 1 running', { exact: false }),
    ).toBeVisible();
    // Dial grid: the "active sessions" instrument carries the SAME status-derived
    // count (1). The dial-grid renders identically empty when the home data fails,
    // so a populated instrument value proves the census/status reads wired through.
    const dials = page.getByTestId('dial-grid');
    await expect(dials).toBeVisible();
    await expect(dials.getByRole('link', { name: 'active sessions: 1' })).toBeVisible();
    // runs-in-flight canonical-state section carries the same run count.
    await expect(
      page
        .getByRole('region', { name: 'runs in flight · canonical state' })
        .getByRole('link', { name: 'running: 1' }),
    ).toBeVisible();
    // formula-run-progress section names the seeded run's formula — a run-summary
    // derived instrument, empty unless the run projected.
    await expect(
      page
        .getByRole('region', { name: 'formula run progress' })
        .getByRole('link', { name: new RegExp(ANCHOR_FORMULA) }),
    ).toBeVisible();
    // systems section's mail lamp carries the seeded unread count (3 seeded
    // messages), proving the status read reached the lamps.
    await expect(
      page.getByRole('region', { name: 'systems' }).getByRole('link', { name: /3 unread/ }),
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

  test('health renders all six tile sources per their empirical branch', async ({ page }) => {
    await gotoCityRoute(page, CITY_BASE, '/health');
    await expect(page.getByRole('heading', { name: 'Health', level: 1 })).toBeVisible();

    // Source 1 — Supervisor tile (typed /v0/city/{c}/health). POPULATED: the
    // synopsis carries the seeded city name, proving the supervisor health read
    // wired through (a static header would not carry it).
    await expect(
      page.getByText(`Supervisor healthy on ${CITY_NAME}`, { exact: false }),
    ).toBeVisible();

    // Source 2 — Host + Admin tiles (/api/health/system). These read the SERVING
    // HOST (/proc + the Go process), so exact values are host-dependent and NOT
    // asserted. A bare CI runner still exposes every /proc metric and NumCPU, so
    // the tile always renders its rows — assert STRUCTURE (heading + row labels),
    // which holds on both a dev box and a CI runner.
    await expect(page.getByRole('heading', { name: 'Host', level: 2 })).toBeVisible();
    await expect(page.getByText('CPUs', { exact: true })).toBeVisible();
    await expect(page.getByText('Load (1m, 5m, 15m)')).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Admin process', level: 2 })).toBeVisible();
    await expect(page.getByText('Node', { exact: true })).toBeVisible();

    // Source 3 — Tool versions (/api/health/local-tools). Probes the host PATH for
    // gc/bd/dolt; a CI runner may report every tool "unavailable", but the table
    // ALWAYS renders one row per tool, so assert the row STRUCTURE (data-tool-version-row),
    // never a version value. Holds on both a dev box (versions) and CI (unavailable).
    await expect(page.getByRole('heading', { name: 'Tool versions' })).toBeVisible();
    await expect(page.locator('[data-tool-version-row="gc"]')).toBeVisible();
    await expect(page.locator('[data-tool-version-row="bd"]')).toBeVisible();
    await expect(page.locator('[data-tool-version-row="dolt"]')).toBeVisible();

    // Source 4 — Diagnostics / Beads usage (/api/city/{c}/supervisor-status). The
    // sampler needs the loopback base URL AND a completed background refresh; the
    // fake supervisor wires the base URL and pre-warms the sampler at boot, so the
    // tile projects the seeded work counts rather than the cold "warming up" copy.
    // POPULATED: the Beads-usage rows render, and the warming-up copy is ABSENT —
    // the negative assertion is the proof the sampler read reached the tile.
    await expect(page.getByRole('heading', { name: 'Beads usage' })).toBeVisible();
    await expect(page.getByText('In progress', { exact: true })).toBeVisible();
    await expect(page.getByText(/sample is warming up/)).toHaveCount(0);

    // Source 5 — Bead stores · per rig (/api/city/{c}/rig-store-health). The seeded
    // rig ("demo") has no on-disk .beads store, so the sampler probes it and the
    // tile renders the rig row in its DESIGNED unreachable state — a populated row
    // (the seeded rig is present) carrying the designed degraded note.
    await expect(page.getByRole('heading', { name: 'Bead stores · per rig' })).toBeVisible();
    await expect(page.getByText('.beads store not found on disk')).toBeVisible();

    // Source 6 — Dolt-noms · 24 h (/api/city/{c}/dolt-noms/trend). The seeded
    // status reports a store_health.size_bytes, so the sampler appends a trend
    // sample and the tile renders its sparkline. POPULATED: the sparkline figure
    // is present (its aria-label is the stable handle).
    await expect(page.getByRole('heading', { name: 'Dolt-noms · 24 h' })).toBeVisible();
    await expect(page.locator('[aria-label="24 hour dolt-noms size trend"]')).toBeVisible();
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

  // Remaining surfaces (ga-r375m2): every dashboard view either renders POPULATED
  // seeded content or its DESIGNED degraded/empty state — never a blank pane, dead
  // spinner, or error boundary. Each spec below probes one surface the earlier
  // rounds left presence-only or unseeded.

  test('mail thread detail renders both message bodies of the seeded thread', async ({ page }) => {
    await gotoCityRoute(page, CITY_BASE, '/mail');
    await expect(page.getByRole('heading', { name: 'Mail', level: 1 })).toBeVisible();
    // The seeded operator↔agent thread carries two messages (a handoff and the
    // agent's reply). Open it from the "All" box (the operator-scoped Inbox hides
    // the agent-addressed rows), then assert BOTH bodies render in the thread
    // modal — proof the /mail/thread/{id} read projected the whole thread, not a
    // single row. Both thread rows share the subject, so click the first.
    await page.getByRole('button', { name: 'All', exact: true }).click();
    await page
      .getByRole('row', { name: new RegExp(OPERATOR_MAIL_SUBJECT) })
      .first()
      .click();
    const thread = page.getByRole('dialog');
    await expect(thread.getByRole('heading', { name: OPERATOR_MAIL_SUBJECT })).toBeVisible();
    await expect(thread.getByText(OPERATOR_MAIL_BODY)).toBeVisible();
    await expect(thread.getByText(AGENT_REPLY_BODY)).toBeVisible();
  });

  test('agent detail renders metadata, the assigned bead, chat, and idle live-peek', async ({
    page,
  }) => {
    await gotoCityRoute(page, CITY_BASE, `/agents/${AGENT_SESSION_SLUG}`);
    // Header + StatusBadge: the seeded session resolves the slug, so the detail
    // page (not its not-found shell) renders — the h1 is the agent alias and the
    // badge carries the (runtime-overlaid) session state.
    await expect(page.getByRole('heading', { name: AGENT_SESSION_SLUG, level: 1 })).toBeVisible();
    await expect(page.getByText(AGENT_SESSION_STATE, { exact: false })).toBeVisible();
    // AgentMetadata: real values from the seeded session — the resolved provider
    // and the rig-encoding template. A not-found/loading shell carries neither.
    await expect(page.getByText('test-agent', { exact: false })).toBeVisible();
    await expect(page.getByText(AGENT_SESSION_TEMPLATE, { exact: false })).toBeVisible();
    // AgentBeadsAssigned: the in-progress bead assigned to this agent's alias. The
    // button's title anchors it to the exact bead so a stray "preflight" elsewhere
    // cannot satisfy it; its visible text is the bead title.
    const assigned = page.locator(`[title="Open ${AGENT_ASSIGNED_BEAD_ID}"]`);
    await expect(assigned).toBeVisible();
    await expect(assigned).toHaveText(AGENT_ASSIGNED_BEAD_TITLE);
    // Chat thread: the two operator↔agent messages render their bodies (the
    // builder→reviewer handoff is NOT between operator and agent, so it is
    // correctly absent from this pane).
    await expect(page.getByText(OPERATOR_MAIL_BODY)).toBeVisible();
    await expect(page.getByText(AGENT_REPLY_BODY)).toBeVisible();
    // Live-peek pane: the seeded stack backs no live runtime, so no transcript is
    // streamed. Assert the pane's DESIGNED idle/empty state (its explicit copy),
    // not a blank pane — the "renders a designed empty state" branch of the bar.
    await expect(page.getByText('No turns in this session yet.')).toBeVisible();
  });

  test('run detail diff tab renders its designed unavailable state', async ({ page }) => {
    await gotoCityRoute(page, CITY_BASE, `/runs/${ANCHOR_RUN_ID}`);
    // The Diff tab is the default-active run-evidence view. Exercise it (re-select)
    // and assert the panel. The seeded run records no work_dir, so a real seeded
    // city cannot produce a diff — assert the DESIGNED unavailable state, not a
    // blank panel. The tab is genuinely exercised: it is selected and its panel
    // renders its own copy.
    const diffTab = page.getByRole('tab', { name: 'Diff' });
    await expect(diffTab).toHaveAttribute('aria-selected', 'true');
    await diffTab.click();
    const panel = page.getByRole('tabpanel');
    await expect(panel.getByText('No diff available for this run.')).toBeVisible();
    await expect(panel.getByText(/did not record a work_dir/)).toBeVisible();
  });

  test('activity commits and deploys render their designed empty states', async ({ page }) => {
    await gotoCityRoute(page, CITY_BASE, '/activity');
    await expect(page.getByRole('heading', { name: 'Activity', level: 1 })).toBeVisible();
    // Commits and Deploys read HOST-scoped sources (the admin git repo and the
    // deploy log), which the seeded city does not provide — the fake supervisor
    // pins both to its empty scratch root, so each renders its DESIGNED empty
    // state. Exercise each mode via its nav link, then assert the empty row scoped
    // to the named table so a stray "No … in this window." cannot leak in.
    await page.getByRole('link', { name: 'Commits' }).click();
    const commits = page.getByRole('table', { name: 'Git commits' });
    await expect(commits).toBeVisible();
    await expect(commits.getByText('No commits in this window.')).toBeVisible();

    await page.getByRole('link', { name: 'Deploys' }).click();
    const deploys = page.getByRole('table', { name: 'Deploy history' });
    await expect(deploys).toBeVisible();
    await expect(deploys.getByText('No deploy records in this window.')).toBeVisible();
  });

  test('bead detail modal renders dependencies and closes cleanly', async ({ page }) => {
    await gotoCityRoute(page, CITY_BASE, '/beads');
    await expect(page.getByRole('heading', { name: 'Beads', level: 1 })).toBeVisible();
    // Open the seeded review step, which "needs" the preflight step. Its row button
    // is anchored by title so the click is unambiguous.
    await page.locator(`[title="Select ${REVIEW_BEAD_ID}"]`).click();
    const modal = page.getByRole('dialog');
    await expect(modal.getByRole('heading', { name: REVIEW_BEAD_TITLE })).toBeVisible();
    // POPULATED BeadDependencies: the modal renders the single upstream dependency
    // built client-side from the bead's needs edge. Assert the section heading and
    // the resolved dependency target (id + title) — proof the edge projected, not
    // the "No dependencies." empty branch.
    await expect(modal.getByRole('heading', { name: 'Dependencies' })).toBeVisible();
    await expect(modal.getByText(REVIEW_DEP_TARGET_ID, { exact: false })).toBeVisible();
    // Closes cleanly — the modal leaves the DOM, no leaked error boundary. Both
    // the header "×" (aria-label Close) and the footer "Close" action dismiss it;
    // the header handle is first in the DOM.
    await modal.getByRole('button', { name: 'Close' }).first().click();
    await expect(page.getByRole('dialog')).toHaveCount(0);
  });

  test('runs view SSE indicator reaches its live/connected state', async ({ page }) => {
    await gotoCityRoute(page, CITY_BASE, '/runs');
    // The Runs view opens an EventSource over /v0/city/{c}/events/stream; the
    // SseIndicator flips to its connected badge once the stream is live. Proving it
    // reaches "live" exercises the browser EventSource path end-to-end (not just a
    // one-shot fetch). The badge's title is the stable handle; its label reads
    // "live" in the open state.
    const live = page.getByTitle('SSE stream: open');
    await expect(live).toBeVisible({ timeout: 15_000 });
    await expect(live).toHaveText(/live/);
  });
});

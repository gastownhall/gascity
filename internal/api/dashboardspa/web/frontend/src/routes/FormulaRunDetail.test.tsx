import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter, Route, Routes, useNavigate } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FormulaRunDetailPage, runDetailNudgeRefresh } from './FormulaRunDetail';
import { invalidate, setCached } from '../api/cache';
import { setActiveCity } from '../api/cityBase';
import { NowProvider } from '../contexts/NowContext';
import { resetSupervisorApiForTests } from '../supervisor/client';
import {
  GC_EVENT_PREFIX,
  type TranscriptResult,
  type TranscriptTurn,
  type RunDiffResponse,
  type FormulaRunDetail,
  type RunScopeKind,
  type RunLane,
  type RunSummary,
  type SourceState,
} from 'gas-city-dashboard-shared';
import { ApiClientError } from '../api/client';
import rawFormulaRunDetailFixture from '../test/fixtures/formula-run-detail.json';

const loadSupervisorFormulaRunDetail = vi.hoisted(() => vi.fn());

vi.mock('../supervisor/runDetail', () => ({
  loadSupervisorFormulaRunDetail,
}));

vi.mock('../hooks/useEntityLinks', () => ({
  useEntityLinks: () => ({ view: null, loading: false, error: null }),
}));

const eventSources: FakeEventSource[] = [];

interface FormulaRunDetailFixture {
  detail: FormulaRunDetail;
  diff: RunDiffResponse;
  transcripts: Record<string, TranscriptResult>;
  streamTurns: Record<string, TranscriptTurn[]>;
}

const formulaRunDetailFixture = parseFormulaRunDetailFixture(rawFormulaRunDetailFixture);
const detail = formulaRunDetailFixture.detail;
const diff = formulaRunDetailFixture.diff;
const transcripts = formulaRunDetailFixture.transcripts;
const reviewPipelineName = /multi-model review pipeline/i;
const applyFixesName = /apply review fixes/i;
const fetchUrls: string[] = [];
let currentDetail: FormulaRunDetail = detail;
let currentDiff: RunDiffResponse = diff;

beforeEach(() => {
  setActiveCity('test-city');
  resetSupervisorApiForTests();
  eventSources.length = 0;
  fetchUrls.length = 0;
  invalidate('formula-run');
  invalidate('runs:summary:test-city');
  loadSupervisorFormulaRunDetail.mockReset();
  loadSupervisorFormulaRunDetail.mockImplementation(async () => currentDetail);
  currentDetail = detail;
  currentDiff = diff;
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      fetchUrls.push(url);
      if (url.startsWith('/api/city/test-city/runs/gc-adopt-pr-active/diff')) {
        return jsonResponse(currentDiff);
      }
      if (url.startsWith('/api/city/test-city/runs/gc-adopt-pr-active')) {
        throw new Error(`old dashboard formula-run mirror should not be called: ${url}`);
      }
      const transcriptPrefix = '/v0/city/test-city/session/';
      if (url.startsWith(transcriptPrefix) && url.endsWith('/transcript?format=conversation')) {
        expect(init?.method ?? (input instanceof Request ? input.method : 'GET')).toBe('GET');
        const id = decodeURIComponent(
          url.slice(transcriptPrefix.length, -'/transcript?format=conversation'.length),
        );
        const transcript = transcripts[id] ?? transcripts[sessionTranscriptFixtureId(id)];
        if (transcript !== undefined) {
          return jsonResponse(toSupervisorTranscript(transcript));
        }
      }
      if (url.includes('/sessions/') && url.endsWith('/peek')) {
        throw new Error('old dashboard session peek route should not be called');
      }
      throw new Error(`unexpected fetch: ${url}`);
    }),
  );
});

afterEach(() => {
  cleanup();
  resetSupervisorApiForTests();
  vi.unstubAllGlobals();
});

describe('FormulaRunDetailPage', () => {
  it('shows a single initial-loading message without calling it a refresh', () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => new Promise<Response>(() => {})),
    );

    renderPage();

    expect(screen.getAllByText(/^Loading formula run\.$/i)).toHaveLength(1);
    const refreshButton = screen.getByRole('button', { name: /^refresh$/i }) as HTMLButtonElement;
    expect(refreshButton.disabled).toBe(true);
    expect(screen.queryByRole('button', { name: /^refreshing$/i })).toBeNull();
  });

  it('renders an optimistic stage-ladder skeleton from the snapshot lane while detail loads', async () => {
    // The first load of a run is bounded by the supervisor's all-store scan
    // (gascity-dashboard-wqsk). When the operator arrives from /runs the
    // run-summary cache already holds this run's lane, so the page shows its
    // title + phase stages instantly instead of a blank spinner. Here the
    // detail (and diff) fetch hangs while the warm summary supplies the lane.
    setCached('runs:summary:test-city', runSummarySourceWithActiveLane());
    vi.stubGlobal(
      'fetch',
      vi.fn(() => new Promise<Response>(() => {})),
    );
    loadSupervisorFormulaRunDetail.mockImplementationOnce(
      () => new Promise<FormulaRunDetail>(() => {}),
    );

    renderPage();

    const ladder = await screen.findByRole('list', { name: /pending adoption run stages/i });
    const stageRows = within(ladder)
      .getAllByRole('listitem')
      .map((item) => item.textContent?.replace(/\s+/g, ' ').trim());
    expect(stageRows).toEqual(['◆ Intake', '⬣ Implementation', '· Review']);
    expect(screen.getByText(/^Loading run detail\.$/i)).toBeTruthy();
    // The plain spinner is replaced by the skeleton, and the heavy detail
    // diagram has not rendered yet.
    expect(screen.queryByText(/^Loading formula run\.$/i)).toBeNull();
    expect(screen.queryByRole('heading', { name: /local changes/i })).toBeNull();
  });

  it('renders the optimistic skeleton for a BLOCKED run lane (gascity-dashboard-4xcv)', async () => {
    // Blocked lanes moved out of summary.lanes into blockedLanes; a blocked
    // run is the most likely one the operator clicks into, so the skeleton
    // lookup must search both buckets.
    const source = runSummarySourceWithActiveLane();
    if (source.status === 'error') throw new Error(source.error);
    source.data.blockedLanes = source.data.lanes;
    source.data.lanes = [];
    source.data.totalActive = 0;
    setCached('runs:summary:test-city', source);
    vi.stubGlobal(
      'fetch',
      vi.fn(() => new Promise<Response>(() => {})),
    );
    loadSupervisorFormulaRunDetail.mockImplementationOnce(
      () => new Promise<FormulaRunDetail>(() => {}),
    );

    renderPage();

    expect(await screen.findByRole('list', { name: /pending adoption run stages/i })).toBeTruthy();
    expect(screen.queryByText(/^Loading formula run\.$/i)).toBeNull();
  });

  it('fires NO run-summary mount read on a cold cache, consuming a warm hit only (gascity-dashboard-i60u)', async () => {
    // i60u cold-load stopgap: a direct/refresh load arrives with the run-summary
    // cache cold. The detail page must NOT fire its own heavy molecule(all=true)
    // + city-feed scan just to paint an optimistic skeleton — that read saturates
    // the browser's ~6-conn/host pool and queues the detail's own fast reads
    // behind it. With the cache cold (beforeEach invalidated it) the skeleton is
    // absent and the lightweight loading state renders; no run-summary fetch is
    // issued. The always-mounted RunSummaryProvider stays the sole owner of that
    // key's fetch — exercised separately in runSummarySubscription.test.tsx.
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        fetchUrls.push(requestUrl(input));
        return new Promise<Response>(() => {});
      }),
    );
    loadSupervisorFormulaRunDetail.mockImplementationOnce(
      () => new Promise<FormulaRunDetail>(() => {}),
    );

    renderPage();

    // Lightweight loading state, NOT the optimistic skeleton ladder.
    expect(await screen.findByText(/^Loading formula run\.$/i)).toBeTruthy();
    expect(screen.queryByRole('list', { name: /pending adoption run stages/i })).toBeNull();
    // No heavy city-wide mount read (molecule history scan / city formula feed).
    const heavyReads = fetchUrls.filter(
      (url) => /[?&]type=molecule(&|$)/.test(url) || url.includes('/formulas/feed'),
    );
    expect(heavyReads).toEqual([]);
  });

  it('does not repeat missing formula metadata as formula detail and a partial banner', async () => {
    currentDetail = {
      ...detail,
      formula: { kind: 'unavailable', reason: 'missing_formula_metadata' },
      formulaDetail: { kind: 'unavailable', reason: 'missing_formula_metadata' },
      completeness: {
        kind: 'partial',
        reasons: ['formula_detail_missing_formula_metadata'],
      },
    };

    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });

    expect(screen.getByText(/^metadata missing$/i)).toBeTruthy();
    expect(screen.queryByText(/^Formula Detail$/i)).toBeNull();
    expect(screen.queryByText(/partial run data/i)).toBeNull();
  });

  it('renders the dashboard-derived phase ladder from the run detail stages', async () => {
    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });

    // gascity-dashboard-ud6j: the run-detail view surfaces the same phase
    // ladder the RunMap lane shows, computed by the backend from this run's
    // own beads. The ladder is a labelled list whose stage words read in
    // greyscale (DESIGN.md "States have words").
    const ladder = screen.getByRole('list', { name: /adopt pr #42 stages/i });
    expect(ladder).toBeTruthy();
    const stageRows = within(ladder)
      .getAllByRole('listitem')
      .map((item) => item.textContent?.replace(/\s+/g, ' ').trim());
    expect(stageRows).toEqual([
      '◆ Intake',
      '◆ Implementation',
      '⬣ Review round 2',
      '· Approval',
      '· Finalization',
    ]);
  });

  it('starts with no selected node and toggles exactly one selected node', async () => {
    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });
    expect(screen.getByText(/3 running, 1 done, 1 ready, 1 skipped/i)).toBeTruthy();
    expect(screen.getByText(/v11 · seq 91/i)).toBeTruthy();
    expect(screen.getByRole('tab', { name: /diff/i }).getAttribute('aria-controls')).toBe(
      'run-evidence-panel',
    );
    expect(screen.getByRole('tabpanel').getAttribute('aria-labelledby')).toBe(
      'run-evidence-tab-diff',
    );
    expect(nodePressed(reviewPipelineName)).toBe('false');
    expect(nodePressed(applyFixesName)).toBe('false');

    fireEvent.click(screen.getByRole('button', { name: reviewPipelineName }));
    expect(nodePressed(reviewPipelineName)).toBe('true');
    openSessionTab();
    await screen.findByText(/checking graph\.v2 node grouping/i);
    expect(screen.getByRole('tabpanel').getAttribute('aria-labelledby')).toBe(
      'run-evidence-tab-session',
    );

    fireEvent.click(screen.getByRole('button', { name: applyFixesName }));
    expect(nodePressed(reviewPipelineName)).toBe('false');
    expect(nodePressed(applyFixesName)).toBe('true');
    // Server-picked visible instance is the current iteration 2 (not-started),
    // so the default panel shows the not-started copy, not the historical
    // iteration 1 transcript.
    await screen.findByText('This node has not started a session yet.');

    fireEvent.click(screen.getByRole('button', { name: applyFixesName }));
    expect(nodePressed(applyFixesName)).toBe('false');
    expect(screen.getByText(/select a node/i)).toBeTruthy();
  });

  it('renders the backend running-formula progress summary instead of deriving it in React', async () => {
    currentDetail = {
      ...detail,
      progress: {
        ...detail.progress,
        visibleNodeCount: 99,
        edgeCount: 88,
        statusCounts: { pending: 99 },
      },
    };

    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });

    expect(screen.getByText(/99 nodes\. 99 pending/i)).toBeTruthy();
  });

  it('clears query-driven selection when the node query is removed', async () => {
    renderPage('/runs/gc-adopt-pr-active?node=review-pipeline', true);
    await screen.findByRole('heading', { name: /adopt pr #42/i });
    await waitFor(() => expect(nodePressed(reviewPipelineName)).toBe('true'));
    openSessionTab();

    fireEvent.click(screen.getByRole('button', { name: /clear node query/i }));

    await waitFor(() => expect(nodePressed(reviewPipelineName)).toBe('false'));
    expect(screen.getByText(/select a node/i)).toBeTruthy();
  });

  it('applies query-driven selection when refresh materializes the requested node', async () => {
    currentDetail = withoutNode(detail, 'review-pipeline');
    renderPage('/runs/gc-adopt-pr-active?node=review-pipeline');
    await screen.findByRole('heading', { name: /adopt pr #42/i });
    expect(screen.queryByRole('button', { name: reviewPipelineName })).toBeNull();
    fireEvent.click(screen.getByRole('tab', { name: /session/i }));
    expect(screen.getByText(/select a node/i)).toBeTruthy();

    currentDetail = detail;
    fireEvent.click(screen.getByRole('button', { name: /refresh/i }));

    await waitFor(() => expect(nodePressed(reviewPipelineName)).toBe('true'));
    await screen.findByText(/checking graph\.v2 node grouping/i);
  });

  it('renders a pushed detail frame from the per-run stream with no re-GET (P4)', async () => {
    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });
    const detailStream = requireRunDetailStream();
    // First paint spent exactly one detail GET; the stream must not add another.
    expect(loadSupervisorFormulaRunDetail).toHaveBeenCalledTimes(1);

    detailStream.open();
    detailStream.dispatch('detail', {
      ...detail,
      title: 'Adopt PR #42 refreshed',
      snapshotVersion: 12,
      snapshotEventSeq: { kind: 'known', seq: 92 },
    });

    await screen.findByRole('heading', { name: /adopt pr #42 refreshed/i });
    expect(screen.getByText(/v12 · seq 92/i)).toBeTruthy();
    // The pushed frame rendered with ZERO additional detail GET.
    expect(loadSupervisorFormulaRunDetail).toHaveBeenCalledTimes(1);
  });

  it('does not refresh terminal runs from ambient city events without run identity', async () => {
    currentDetail = terminalDetail();
    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });
    const cityStream = requireCityEventSource();
    await waitFor(() => expect(diffUrls()).toHaveLength(1));

    cityStream.dispatch('event', { type: `${GC_EVENT_PREFIX.session}updated` });
    await Promise.resolve();

    expect(loadSupervisorFormulaRunDetail).toHaveBeenCalledTimes(1);
    expect(diffUrls()).toHaveLength(1);
  });

  it('drives ambient suppression from the server progress.terminal flag, not a client taxonomy', async () => {
    // An all-`done` census that the OLD client fold would classify terminal, but
    // the server reports progress.terminal=false. The retired isTerminalProgress
    // derivation would suppress here; the server flag must win and the ambient
    // event must still refresh — proving the flag, not a re-derived taxonomy,
    // gates suppression. P4 moved detail to the stream, so this ambient nudge now
    // refreshes the DIFF; the terminal flag still gates whether it fires. The
    // Diff tab is the default-active tab, so the P5 tab gate is open here.
    currentDetail = {
      ...terminalDetail(),
      progress: { ...terminalDetail().progress, terminal: false },
    };
    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });
    const cityStream = requireCityEventSource();
    await waitFor(() => expect(diffUrls()).toHaveLength(1));

    cityStream.dispatch('event', { type: `${GC_EVENT_PREFIX.session}updated` });
    await waitFor(() => expect(diffUrls()).toHaveLength(2));
  });

  it('fires NO diff POST from a nudge while the Diff tab is hidden (P5 tab gate)', async () => {
    // The Diff tab is the default-active view, so switch to Session first to hide
    // it. A nudge must then issue ZERO /diff POSTs — the git-exec chain no longer
    // runs for a tab the operator can't see.
    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });
    const cityStream = requireCityEventSource();
    await waitFor(() => expect(diffUrls()).toHaveLength(1));

    openSessionTab();
    const hiddenTabDiffCount = diffUrls().length;

    cityStream.dispatch('event', { type: `${GC_EVENT_PREFIX.session}updated` });
    await Promise.resolve();
    await Promise.resolve();

    expect(diffUrls()).toHaveLength(hiddenTabDiffCount);
  });

  it('refreshes the diff once when the operator switches to the Diff tab (P5, not stale)', async () => {
    // Switch away from the default Diff tab, let a nudge fire (no diff POST while
    // hidden), then switch back — the Diff tab must refresh once on activation so
    // it does not show stale changes the hidden-tab nudges skipped.
    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });
    const cityStream = requireCityEventSource();
    await waitFor(() => expect(diffUrls()).toHaveLength(1));

    openSessionTab();
    cityStream.dispatch('event', { type: `${GC_EVENT_PREFIX.session}updated` });
    await Promise.resolve();
    expect(diffUrls()).toHaveLength(1);

    currentDiff = {
      ...diff,
      changedFiles: [{ path: 'src/switched.ts', status: 'M', kind: 'code' }],
      status: [' M src/switched.ts'],
      patch: [
        'diff --git a/src/switched.ts b/src/switched.ts',
        'index 3a4e79a..b6c9d02 100644',
        '--- a/src/switched.ts',
        '+++ b/src/switched.ts',
        '@@ -1 +1 @@',
        '-stale',
        '+fresh on tab activation',
      ].join('\n'),
    };
    fireEvent.click(screen.getByRole('tab', { name: /diff/i }));

    await screen.findByText('fresh on tab activation');
    // Exactly one activation refresh (the hidden-tab nudge added none).
    await waitFor(() => expect(diffUrls()).toHaveLength(2));
  });

  it('coalesces a burst of nudges into at most one diff POST per window (P5)', async () => {
    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });
    const cityStream = requireCityEventSource();
    // Default Diff tab is visible, so the diff refresh gate is open.
    await waitFor(() => expect(diffUrls()).toHaveLength(1));

    // A burst inside one coalesce window yields a single leading fire.
    cityStream.dispatch('event', { type: `${GC_EVENT_PREFIX.session}updated` });
    cityStream.dispatch('event', { type: `${GC_EVENT_PREFIX.session}updated` });
    cityStream.dispatch('event', { type: `${GC_EVENT_PREFIX.session}updated` });
    await waitFor(() => expect(diffUrls()).toHaveLength(2));

    await Promise.resolve();
    await Promise.resolve();
    // No per-event storm: the burst added exactly one diff POST.
    expect(diffUrls()).toHaveLength(2);
  });

  it('does not refresh from city events before the initial run detail identifies the run', async () => {
    const initialLoad = deferred<FormulaRunDetail>();
    loadSupervisorFormulaRunDetail.mockReturnValue(initialLoad.promise);

    renderPage();
    const cityStream = requireCityEventSource();
    expect(loadSupervisorFormulaRunDetail).toHaveBeenCalledTimes(1);

    cityStream.dispatch('event', { type: `${GC_EVENT_PREFIX.bead}updated` });
    await Promise.resolve();

    expect(loadSupervisorFormulaRunDetail).toHaveBeenCalledTimes(1);
    initialLoad.resolve(detail);
    await screen.findByRole('heading', { name: /adopt pr #42/i });
  });

  it('does not load the execution-folder diff before the initial run detail is ready', async () => {
    const initialLoad = deferred<FormulaRunDetail>();
    loadSupervisorFormulaRunDetail.mockReturnValue(initialLoad.promise);

    renderPage();
    await Promise.resolve();

    expect(diffUrls()).toHaveLength(0);

    initialLoad.resolve(detail);
    await screen.findByRole('heading', { name: /adopt pr #42/i });
    await waitFor(() => expect(diffUrls()).toHaveLength(1));
  });

  it('refreshes the execution-folder diff during a run without leaving an explicit Diff tab choice', async () => {
    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });
    const cityStream = requireCityEventSource();

    fireEvent.click(screen.getByRole('button', { name: reviewPipelineName }));
    expect(nodePressed(reviewPipelineName)).toBe('true');
    fireEvent.click(screen.getByRole('tab', { name: /diff/i }));
    await screen.findByRole('heading', { name: /local changes/i });

    currentDiff = {
      ...diff,
      changedFiles: [{ path: 'src/live-run.ts', status: 'M', kind: 'code' }],
      status: [' M src/live-run.ts'],
      patch: [
        'diff --git a/src/live-run.ts b/src/live-run.ts',
        'index 3a4e79a..b6c9d02 100644',
        '--- a/src/live-run.ts',
        '+++ b/src/live-run.ts',
        '@@ -1 +1 @@',
        '-stale diff',
        '+live run diff update',
      ].join('\n'),
    };
    currentDetail = {
      ...detail,
      snapshotVersion: 12,
      snapshotEventSeq: { kind: 'known', seq: 92 },
    };
    cityStream.dispatch('event', { type: `${GC_EVENT_PREFIX.session}updated` });

    await screen.findByText('live run diff update');
    expect(screen.getByRole('tabpanel').getAttribute('aria-labelledby')).toBe(
      'run-evidence-tab-diff',
    );
    expect(screen.queryByText(/checking graph\.v2 node grouping/i)).toBeNull();
  });

  it('ignores city events whose gc metadata identifies another formula run', async () => {
    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });
    const cityStream = requireCityEventSource();
    const detailStream = requireRunDetailStream();
    detailStream.open();
    // The diff nudge must not fire for an unrelated run; capture the current
    // count so a stray refresh is detectable. (Detail no longer re-GETs on the
    // nudge — it streams — so this asserts the diff-lane match filter.)
    await waitFor(() => expect(diffUrls()).toHaveLength(1));
    const diffCount = diffUrls().length;

    cityStream.dispatch('event', {
      type: `${GC_EVENT_PREFIX.bead}updated`,
      payload: {
        bead: {
          metadata: {
            'gc.run_id': 'other-run',
            'gc.root_bead_id': 'other-root',
          },
        },
      },
    });

    await Promise.resolve();
    expect(diffUrls()).toHaveLength(diffCount);
    expect(screen.getByRole('heading', { name: /adopt pr #42/i })).toBeTruthy();

    // A pushed frame for THIS run (the stream is per-run, so a frame is always
    // this run's) updates the rendered detail — with zero re-GET.
    detailStream.dispatch('detail', {
      ...detail,
      title: 'Adopt PR #42 current formula run refresh',
      snapshotVersion: 12,
      snapshotEventSeq: { kind: 'known', seq: 92 },
    });

    await screen.findByRole('heading', { name: /adopt pr #42 current formula run refresh/i });
    expect(screen.getByText(/v12 · seq 92/i)).toBeTruthy();
    expect(loadSupervisorFormulaRunDetail).toHaveBeenCalledTimes(1);
  });

  it('rejects a half-specified scope query without loading the formula run', async () => {
    // Only scope_kind, no scope_ref. The backend rejects this as a 400, so the
    // frontend must fail closed too — silently dropping the scope would load the
    // WRONG (default city) run for a truncated deep link.
    renderPage('/runs/gc-adopt-pr-active?scope_kind=city');

    await screen.findByRole('alert');
    expect(screen.getByText(/invalid run scope query/i)).toBeTruthy();
    expect(fetchUrls.some((url) => url.startsWith('/api/city/test-city/runs/'))).toBe(false);
  });

  it('passes complete scope query params when loading detail and diff', async () => {
    renderPage('/runs/gc-adopt-pr-active?scope_kind=city&scope_ref=racoon-city');
    await screen.findByRole('heading', { name: /adopt pr #42/i });

    const runUrls = fetchUrls.filter((url) => url.startsWith('/api/city/test-city/runs/'));
    // The detail loader is scope-independent now (the BFF projection derives the
    // run's scope from its own root bead); the route's scope still drives the
    // separate run-diff fetch below.
    expect(loadSupervisorFormulaRunDetail).toHaveBeenCalledWith('gc-adopt-pr-active');
    expect(runUrls).toContain(
      '/api/city/test-city/runs/gc-adopt-pr-active/diff?scope_kind=city&scope_ref=racoon-city',
    );
  });

  it('surfaces malformed complete scope query params without loading the formula run', async () => {
    renderPage('/runs/gc-adopt-pr-active?scope_kind=workspace&scope_ref=racoon-city');

    await screen.findByRole('alert');
    expect(screen.getByText(/invalid run scope query/i)).toBeTruthy();
    expect(fetchUrls.some((url) => url.startsWith('/api/city/test-city/runs/'))).toBe(false);
  });

  it('rejects duplicated scope query params without loading the formula run', async () => {
    renderPage('/runs/gc-adopt-pr-active?scope_kind=city&scope_kind=rig&scope_ref=racoon-city');

    await screen.findByRole('alert');
    expect(screen.getByText(/invalid run scope query/i)).toBeTruthy();
    expect(fetchUrls.some((url) => url.startsWith('/api/city/test-city/runs/'))).toBe(false);
  });

  it('shows loop iteration history in the session panel for a selected semantic node', async () => {
    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });
    fireEvent.click(screen.getByRole('button', { name: reviewPipelineName }));
    openSessionTab();
    await screen.findByText(/checking graph\.v2 node grouping/i);

    fireEvent.click(screen.getByRole('radio', { name: /iteration 1/i }));
    await screen.findByText(/found two issues/i);
    expect(screen.getByText(/historical/i)).toBeTruthy();
  });

  it('keeps historical-only loop transcripts available without adding left-graph nodes', async () => {
    renderPage('/runs/gc-adopt-pr-active?node=old-only-review');
    await screen.findByRole('heading', { name: /adopt pr #42/i });

    expect(screen.queryByRole('button', { name: /old-only review/i })).toBeNull();
    openSessionTab();
    await screen.findByText(/historical-only/i);
    await screen.findByText(/found two issues/i);
    expect(requireCityEventSource()).toBeTruthy();
    expect(sessionEventSources()).toHaveLength(0);
  });

  it('streams named turn events for an active selected node', async () => {
    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });
    fireEvent.click(screen.getByRole('button', { name: reviewPipelineName }));
    openSessionTab();
    await screen.findByText(/checking graph\.v2 node grouping/i);
    await screen.findByText(/command: node --import tsx --test/i);
    await screen.findByText(/stdout: 5 graph\.v2 enrichment tests passed/i);
    expect(screen.getByText(/^tool use$/i)).toBeTruthy();
    expect(screen.getByText(/^tool result$/i)).toBeTruthy();
    expect(screen.getByText(/^final$/i)).toBeTruthy();

    await waitFor(() => expect(sessionEventSources()).toHaveLength(1));
    const stream = sessionEventSources()[0];
    stream?.open();
    await screen.findByText(/^live$/i);

    stream?.dispatch('turn', {
      role: 'assistant',
      text: 'streaming progress on iteration 2',
    });

    await screen.findByText(/streaming progress on iteration 2/i);

    stream?.fail(FakeEventSource.CONNECTING);
    stream?.dispatch('turn', {
      role: 'assistant',
      text: 'stream kept its listener after a transient error',
    });

    await screen.findByText(/stream kept its listener after a transient error/i);
  });

  it('streams supervisor transcript snapshot events for an active selected node', async () => {
    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });
    fireEvent.click(screen.getByRole('button', { name: reviewPipelineName }));
    openSessionTab();
    await screen.findByText(/checking graph\.v2 node grouping/i);
    await waitFor(() => expect(sessionEventSources()).toHaveLength(1));

    const stream = sessionEventSources()[0];
    stream?.open();
    await screen.findByText(/^live$/i);
    stream?.dispatch('turn', {
      session_id: 'gc-session-review-i2',
      template: 'runs.codex',
      provider: 'codex',
      format: 'conversation',
      turns: [
        {
          role: 'assistant',
          text: 'supervisor snapshot event replaced the active transcript',
        },
      ],
      total_chars: 55,
      captured_at: '2026-01-01T00:00:00.000Z',
      truncated: false,
    });

    await screen.findByText(/supervisor snapshot event replaced the active transcript/i);
  });

  it('closes the active session stream when selection changes or the Session tab is hidden', async () => {
    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });
    fireEvent.click(screen.getByRole('button', { name: reviewPipelineName }));
    openSessionTab();
    await screen.findByText(/checking graph\.v2 node grouping/i);
    await waitFor(() => expect(sessionEventSources()).toHaveLength(1));
    const firstStream = sessionEventSources()[0];
    expect(firstStream?.closed).toBe(false);

    fireEvent.click(screen.getByRole('button', { name: applyFixesName }));
    openSessionTab();
    // apply-fixes defaults to the server-picked current iteration 2 (not-started),
    // so switching nodes shows the not-started copy — and must close the prior
    // review-pipeline stream.
    await screen.findByText('This node has not started a session yet.');
    await waitFor(() => expect(firstStream?.closed).toBe(true));

    fireEvent.click(screen.getByRole('button', { name: reviewPipelineName }));
    await screen.findByText(/checking graph\.v2 node grouping/i);
    await waitFor(() => expect(sessionEventSources()).toHaveLength(2));
    const secondStream = sessionEventSources()[1];
    expect(secondStream?.closed).toBe(false);

    fireEvent.click(screen.getByRole('tab', { name: /diff/i }));
    await screen.findByRole('heading', { name: /local changes/i });
    await waitFor(() => expect(secondStream?.closed).toBe(true));
  });

  it('surfaces current not-started instances beside historical attached evidence', async () => {
    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });

    fireEvent.click(screen.getByRole('button', { name: applyFixesName }));
    openSessionTab();
    // The server's visibleExecutionInstanceId points at the current iteration 2
    // instance (not the historical attached iteration 1 the old heuristic
    // surfaced), so the not-started current instance shows by default.
    await screen.findByText('This node has not started a session yet.');

    fireEvent.click(screen.getByRole('radio', { name: /iteration 1/i }));

    await screen.findByText(/apply the iteration 1 review fixes/i);
  });

  it('keeps the Session tab available so a selected node can explain unresolved sessions', async () => {
    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });
    expect((screen.getByRole('tab', { name: /session/i }) as HTMLButtonElement).disabled).toBe(
      false,
    );

    fireEvent.click(screen.getByRole('button', { name: /pre-approval ci repair loop/i }));

    openSessionTab();
    await screen.findByText(/session unresolved for this node/i);
    const sessionTab = screen.getByRole('tab', { name: /session/i }) as HTMLButtonElement;
    expect(sessionTab.disabled).toBe(false);
    expect(sessionTab.getAttribute('aria-disabled')).toBeNull();
  });

  it('renders the current execution-folder diff as grouped files', async () => {
    const { container } = renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });
    expect(screen.getByRole('heading', { name: /local changes/i })).toBeTruthy();
    expect(screen.getByText('shared/src/runs/enrich.ts')).toBeTruthy();
    expect(screen.getByText('docs/plan.md')).toBeTruthy();
    await screen.findByText('preserve failed attempt transcript links');
    expect(container.querySelector('.diff-code-insert')?.textContent).toContain('preserve failed');
    expect(container.querySelector('.diff-code-delete')?.textContent).toContain('old session');
  });

  it('renders an informative list-only message for a v1 / wisp (unsupported) run, not the generic failure (gascity-dashboard-9w3k)', async () => {
    // A v1 / wisp run is clickable in the run list but has no graph.v2 detail
    // view: the BFF rejects it with 422 + reason 'not_run_view'. The page must
    // explain that clearly rather than show the opaque generic fallback.
    loadSupervisorFormulaRunDetail.mockImplementation(async () => {
      throw new ApiClientError(422, 'run is not a graph.v2 run', undefined, 'not_run_view');
    });

    renderPage();

    await screen.findByText(
      /detailed step view isn’t available for this run \(v1\/wisp runs are list-only\)/i,
    );
    expect(screen.getByText(/appears in the run list only/i)).toBeTruthy();
    // Not the generic dead-end, and not an error alert.
    expect(screen.queryByText(/^formula run unavailable\.$/i)).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('renders an honest not-found message for a raw 404 (ambiguous), not the v1 over-claim or the generic failure (Major 2)', async () => {
    // gascity-dashboard (Major 2): a 404 (no run root in the projection) is
    // ambiguous — it can be a v1/wisp id, a completed run whose events rotated
    // out, a pruned run, or a stale/wrong derived scope. The page must NOT assert
    // it is definitively v1 (the 'unsupported' copy) and must NOT fall to the
    // generic "Formula run unavailable." dead-end either.
    loadSupervisorFormulaRunDetail.mockImplementation(async () => {
      throw new ApiClientError(404, 'unknown run');
    });

    renderPage();

    await screen.findByText(/this run’s detail snapshot was not found/i);
    // Does not over-claim v1.
    expect(screen.queryByText(/v1\/wisp runs are list-only/i)).toBeNull();
    // Not the generic dead-end, and not an error alert.
    expect(screen.queryByText(/^formula run unavailable\.$/i)).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('still shows the generic failure for a malformed graph.v2 snapshot (invalid_snapshot)', async () => {
    // A genuine load failure (malformed graph.v2 snapshot: 422 +
    // 'invalid_snapshot') must NOT be mistaken for a v1 list-only run.
    loadSupervisorFormulaRunDetail.mockImplementation(async () => {
      throw new ApiClientError(422, 'run snapshot is invalid', undefined, 'invalid_snapshot');
    });

    renderPage();

    await screen.findByRole('alert');
    expect(screen.queryByText(/v1 \(wisp\) runs yet/i)).toBeNull();
  });

  it('surfaces a partial formula run snapshot on the detail page', async () => {
    currentDetail = {
      ...detail,
      completeness: {
        kind: 'partial',
        reasons: ['supervisor_snapshot_partial'],
      },
    };
    renderPage();

    await screen.findByRole('heading', { name: /adopt pr #42/i });
    expect(screen.getByText(/partial run data/i)).toBeTruthy();
  });

  it('shows no-graph and selected-node-without-session empty states', async () => {
    currentDetail = {
      ...detail,
      nodes: detail.nodes
        .filter((node) => node.id === 'old-only-review')
        .map((node) => ({
          ...node,
          visibleInGraph: false,
          historicalOnly: true,
        })),
      lanes: [],
      edges: [],
    };
    renderPage();

    await screen.findByRole('heading', { name: /adopt pr #42/i });
    expect(screen.getByText(/no graph nodes have materialized/i)).toBeTruthy();

    currentDetail = detail;
    cleanup();
    invalidate('formula-run');
    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });
    fireEvent.click(screen.getByRole('button', { name: /pre-approval ci repair loop/i }));
    openSessionTab();
    expect(screen.getByText(/session unresolved for this node/i)).toBeTruthy();
  });

  it('shows retry attempt tabs for multiple attempts in the selected execution context', async () => {
    currentDetail = detailWithRebaseAttempts();
    renderPage();
    await screen.findByRole('heading', { name: /adopt pr #42/i });

    fireEvent.click(screen.getByRole('button', { name: /rebase and local validation/i }));
    openSessionTab();

    expect(screen.getByRole('radio', { name: /attempt 1/i })).toBeTruthy();
    expect(screen.getByRole('radio', { name: /attempt 2/i })).toBeTruthy();
    await screen.findByText(/rebased cleanly/i);
  });
});

describe('runDetailNudgeRefresh (P4 stream-vs-nudge division, P5 diff tab gate)', () => {
  it('refreshes only the diff when the detail stream is live and the Diff tab is visible', async () => {
    const refreshDetail = vi.fn(() => Promise.resolve());
    const refreshDiff = vi.fn(() => Promise.resolve());
    await runDetailNudgeRefresh(true, true, refreshDetail, refreshDiff);
    // The stream carries detail, so a nudge must NOT re-GET it (no double refetch).
    expect(refreshDetail).not.toHaveBeenCalled();
    expect(refreshDiff).toHaveBeenCalledTimes(1);
  });

  it('refreshes BOTH detail and diff when the stream is unavailable and the Diff tab is visible (F2)', async () => {
    const refreshDetail = vi.fn(() => Promise.resolve());
    const refreshDiff = vi.fn(() => Promise.resolve());
    // No EventSource → the stream can't carry detail, so the nudge must keep the
    // detail auto-refresh alive (otherwise detail freezes after first paint).
    await runDetailNudgeRefresh(false, true, refreshDetail, refreshDiff);
    expect(refreshDetail).toHaveBeenCalledTimes(1);
    expect(refreshDiff).toHaveBeenCalledTimes(1);
  });

  it('fires NO diff refresh when the Diff tab is hidden but the stream is live (P5)', async () => {
    const refreshDetail = vi.fn(() => Promise.resolve());
    const refreshDiff = vi.fn(() => Promise.resolve());
    await runDetailNudgeRefresh(true, false, refreshDetail, refreshDiff);
    // Stream carries detail; Diff tab hidden → zero git-exec diff read.
    expect(refreshDetail).not.toHaveBeenCalled();
    expect(refreshDiff).not.toHaveBeenCalled();
  });

  it('still refreshes detail (not the diff) when the Diff tab is hidden and the stream is unavailable (P5)', async () => {
    const refreshDetail = vi.fn(() => Promise.resolve());
    const refreshDiff = vi.fn(() => Promise.resolve());
    await runDetailNudgeRefresh(false, false, refreshDetail, refreshDiff);
    // Detail still auto-refreshes without a stream; the hidden Diff tab skips its
    // git-exec read.
    expect(refreshDetail).toHaveBeenCalledTimes(1);
    expect(refreshDiff).not.toHaveBeenCalled();
  });
});

function renderPage(initialEntry = '/runs/gc-adopt-pr-active', includeRouteControls = false) {
  return render(
    <MemoryRouter
      initialEntries={[initialEntry]}
      future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
    >
      <Routes>
        <Route
          path="/runs/:runId"
          element={
            <NowProvider intervalMs={1_000_000}>
              <FormulaRunDetailPage />
              {includeRouteControls && <RouteControls />}
            </NowProvider>
          }
        />
      </Routes>
    </MemoryRouter>,
  );
}

function RouteControls() {
  const navigate = useNavigate();
  return (
    <button type="button" onClick={() => navigate('/runs/gc-adopt-pr-active')}>
      Clear node query
    </button>
  );
}

function runSummarySource(lanes: RunLane[] = []): SourceState<RunSummary> {
  return {
    source: 'runs',
    status: 'fresh',
    fetchedAt: '2026-05-25T00:00:00.000Z',
    staleAt: '2026-05-25T00:01:00.000Z',
    error: { kind: 'none' },
    data: {
      totalActive: lanes.length,
      totalHistorical: 0,
      historicalLanes: [],
      blockedLanes: [],
      runCounts: {
        total: lanes.length,
        visible: lanes.length,
        prReview: 0,
        designReview: 0,
        bugfix: 0,
        blocked: 0,
        other: lanes.length,
      },
      lanes,
      recentChanges: [],
      census: { status: 'unavailable', error: 'run health has not been derived' },
    },
  };
}

// A run summary whose runs source carries one lane matching the route's runId,
// so the detail page can render an optimistic skeleton while the full detail
// loads.
function runSummarySourceWithActiveLane(): SourceState<RunSummary> {
  const lane: RunLane = {
    id: 'gc-adopt-pr-active',
    title: 'Pending adoption run',
    formula: { status: 'known', name: 'mol-adopt-pr-v2' },
    scope: {
      status: 'available',
      kind: 'city',
      ref: 'racoon-city',
      rootStoreRef: 'city:racoon-city',
    },
    external: { status: 'unavailable', error: 'external unavailable in test' },
    phase: 'implementation',
    phaseLabel: 'Implementation',
    statusCounts: { in_progress: 1 },
    activeAssignees: [],
    updatedAt: { status: 'available', at: '2026-05-27T22:01:00Z' },
    stages: [
      { key: 'intake', label: 'Intake', status: 'complete' },
      { key: 'implementation', label: 'Implementation', status: 'active' },
      { key: 'review', label: 'Review', status: 'pending' },
    ],
    progress: { status: 'unavailable', error: 'active run step unavailable' },
    formulaStageResolved: false,
    health: {
      status: 'available',
      data: {
        phaseConfidence: 'known',
        needsOperator: false,
        stuckNode: { status: 'unavailable', error: 'run stuck node unavailable' },
        thrashingDetected: false,
        session: { status: 'unresolved', error: 'run session unresolved' },
      },
    },
  };
  return runSummarySource([lane]);
}

function jsonResponse(payload: unknown): Response {
  return new Response(JSON.stringify(payload), {
    status: 200,
    headers: { 'content-type': 'application/json' },
  });
}

function deferred<T>(): {
  promise: Promise<T>;
  resolve: (value: T) => void;
} {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((innerResolve) => {
    resolve = innerResolve;
  });
  return { promise, resolve };
}

function requestUrl(input: RequestInfo | URL): string {
  const url =
    input instanceof Request ? input.url : input instanceof URL ? input.toString() : String(input);
  return stripSameOrigin(url);
}

function stripSameOrigin(url: string): string {
  const origin = window.location.origin;
  return url.startsWith(origin) ? url.slice(origin.length) : url;
}

function sessionTranscriptFixtureId(id: string): string {
  return id === 'gc-session-rebase-a1' || id === 'gc-session-rebase-a2' ? 'gc-session-rebase' : id;
}

function toSupervisorTranscript(transcript: TranscriptResult) {
  return {
    id: transcript.session_id,
    template: transcript.template ?? '',
    provider: transcript.provider ?? '',
    format: transcript.format ?? 'conversation',
    turns: transcript.turns,
  };
}

function nodePressed(name: RegExp): string | null {
  return screen.getByRole('button', { name }).getAttribute('aria-pressed');
}

function openSessionTab(): void {
  fireEvent.click(screen.getByRole('tab', { name: /session/i }));
}

function requireCityEventSource(): FakeEventSource {
  const source = eventSources.find((eventSource) => eventSource.url.endsWith('/events/stream'));
  if (source === undefined) throw new Error('expected city event source');
  return source;
}

// P4: the per-run detail stream is a distinct BFF EventSource
// (/api/city/.../runs/{id}/detail/stream) that pushes the whole FormulaRunDetail
// as a `detail` frame. Detail refresh now arrives here instead of via a nudge
// re-GET.
function requireRunDetailStream(): FakeEventSource {
  const source = eventSources.find((eventSource) => eventSource.url.endsWith('/detail/stream'));
  if (source === undefined) throw new Error('expected run-detail stream source');
  return source;
}

function sessionEventSources(): FakeEventSource[] {
  return eventSources.filter((eventSource) => eventSource.url.includes('/session/'));
}

function diffUrls(): string[] {
  return fetchUrls.filter(
    (url) => url.startsWith('/api/city/test-city/runs/') && url.includes('/diff'),
  );
}

function terminalDetail(): FormulaRunDetail {
  return {
    ...detail,
    progress: {
      ...detail.progress,
      visibleNodeCount: 8,
      statusCounts: { done: 8 },
      allStatusCounts: { done: 8 },
      terminal: true,
    },
    nodes: detail.nodes.map((node) => ({
      ...node,
      status: 'done',
      executionInstances: node.executionInstances.map((instance) => ({
        ...instance,
        status: 'done',
      })),
    })),
  };
}

function withoutNode(detailValue: FormulaRunDetail, nodeId: string): FormulaRunDetail {
  return {
    ...detailValue,
    nodes: detailValue.nodes.filter((node) => node.id !== nodeId),
    lanes: detailValue.lanes.map((lane) => ({
      ...lane,
      nodeIds: lane.nodeIds.filter((id) => id !== nodeId),
    })),
  };
}

function detailWithRebaseAttempts(): FormulaRunDetail {
  return {
    ...detail,
    nodes: detail.nodes.map((node) => {
      if (node.id !== 'rebase-check') return node;
      return {
        ...node,
        attemptSummary: {
          kind: 'tracked',
          count: 2,
          badge: { kind: 'bounded', label: '2/3' },
          active: { kind: 'idle' },
        },
        visibleExecutionInstanceId: 'gc-rebase-check-a2',
        executionInstances: [
          {
            id: 'gc-rebase-check-a1',
            semanticNodeId: 'rebase-check',
            beadId: 'gc-rebase-check-a1',
            iteration: { kind: 'base' },
            attempt: { kind: 'attempt', value: 1 },
            label: 'attempt 1',
            status: 'failed',
            session: {
              kind: 'attached',
              streamable: false,
              link: {
                sessionId: 'gc-session-rebase-a1',
                sessionName: 'rebase-attempt-1',
                assignee: 'codex',
              },
            },
            currentIteration: true,
            historical: false,
          },
          {
            id: 'gc-rebase-check-a2',
            semanticNodeId: 'rebase-check',
            beadId: 'gc-rebase-check-a2',
            iteration: { kind: 'base' },
            attempt: { kind: 'attempt', value: 2 },
            label: 'attempt 2',
            status: 'completed',
            session: {
              kind: 'attached',
              streamable: false,
              link: {
                sessionId: 'gc-session-rebase-a2',
                sessionName: 'rebase-attempt-2',
                assignee: 'codex',
              },
            },
            currentIteration: true,
            historical: false,
          },
        ],
      };
    }),
  };
}

function parseFormulaRunDetailFixture(raw: unknown): FormulaRunDetailFixture {
  if (!isRecord(raw)) throw new Error('run detail fixture must be an object');
  if (!isRecord(raw.detail)) throw new Error('run detail fixture missing detail');
  if (!isRunScopeKind(raw.detail.scopeKind)) {
    throw new Error('run detail fixture has invalid scopeKind');
  }
  if (!Array.isArray(raw.detail.nodes)) {
    throw new Error('run detail fixture missing detail.nodes');
  }
  if (!Array.isArray(raw.detail.edges)) {
    throw new Error('run detail fixture missing detail.edges');
  }
  if (!Array.isArray(raw.detail.stages)) {
    throw new Error('run detail fixture missing detail.stages');
  }
  if (typeof raw.detail.phase !== 'string') {
    throw new Error('run detail fixture missing detail.phase');
  }
  if (!isRecord(raw.diff)) throw new Error('run detail fixture missing diff');
  if (!isRecord(raw.transcripts)) {
    throw new Error('run detail fixture missing transcripts');
  }
  if (!isRecord(raw.streamTurns)) {
    throw new Error('run detail fixture missing streamTurns');
  }
  return raw as unknown as FormulaRunDetailFixture;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isRunScopeKind(value: unknown): value is RunScopeKind {
  return value === 'city' || value === 'rig';
}

class FakeEventSource {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;

  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  readonly url: string;
  readonly withCredentials: boolean;
  readyState = FakeEventSource.CONNECTING;
  closed = false;
  readonly listeners = new Map<string, Array<(event: MessageEvent<string>) => void>>();

  constructor(url: string | URL, init?: EventSourceInit) {
    this.url = String(url);
    this.withCredentials = init?.withCredentials ?? false;
    eventSources.push(this);
  }

  open(): void {
    this.readyState = FakeEventSource.OPEN;
    this.onopen?.(new Event('open'));
  }

  fail(readyState = FakeEventSource.CLOSED): void {
    this.readyState = readyState;
    this.onerror?.(new Event('error'));
  }

  addEventListener(type: string, listener: ((event: MessageEvent<string>) => void) | null): void {
    if (!listener) return;
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
  }

  removeEventListener(
    type: string,
    listener: ((event: MessageEvent<string>) => void) | null,
  ): void {
    if (!listener) return;
    this.listeners.set(
      type,
      (this.listeners.get(type) ?? []).filter((candidate) => candidate !== listener),
    );
  }

  dispatch(type: string, payload: unknown): void {
    const event = new MessageEvent(type, { data: JSON.stringify(payload) });
    for (const listener of this.listeners.get(type) ?? []) {
      listener(event);
    }
    if (type === 'message') {
      this.onmessage?.(event);
    }
  }

  close(): void {
    this.readyState = FakeEventSource.CLOSED;
    this.closed = true;
    this.listeners.clear();
  }
}

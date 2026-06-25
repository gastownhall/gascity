import { type RunLane, type RunSummary, type SourceState } from 'gas-city-dashboard-shared';
import { useCallback } from 'react';
import { useSearchParams } from 'react-router-dom';
import { useAttentionModel } from '../attention/context';
import { resourceAttentionSeverity } from '../attention/routeHighlight';
import { Button } from '../components/Button';
import { PageHeader } from '../components/PageHeader';
import { PartialDataNotice } from '../components/PartialDataNotice';
import { SseIndicator } from '../components/SseIndicator';
import { RunMap, RUNS_HISTORICAL_SECTION_ID } from '../components/run/RunMap';
import { useNow } from '../contexts/NowContext';
import { formatRelative } from '../hooks/time';
import { useRunSummary } from '../runs/runSummarySubscription';

// /runs route (gascity-dashboard-0t6, made live in gascity-dashboard-bqn). The
// fetch, SSE refresh, and degraded-load retry now live in the shared
// run-summary subscription (gascity-dashboard-2j8e.7), which the nav badge reads
// too — so the page and the badge render the same source by construction. This
// component is the source's renderer: it owns only the ?history=1 toggle,
// freshness label, and lane layout.
//
// The app-level NowProvider refreshes relative-time labels in lane cards; the
// shared subscription's SSE path drives actual data updates.

const RUN_PHASE_GRAMMAR = 'Phase grammar: intake, implementation, review, approval, finalization.';
const HISTORY_QUERY_PARAM = 'history';
const HISTORY_QUERY_VALUE = '1';

export function RunsPage() {
  const attention = useAttentionModel();
  const { source: data, loading, error, refresh, sseState } = useRunSummary();
  const [searchParams, setSearchParams] = useSearchParams();
  // gascity-dashboard-yh5i: ?history=1 toggles the historical lane
  // section. Pure render-time state — the summary already carries both
  // active + historical arrays, so the toggle does not trigger a fetch
  // and the shared run-summary subscription stays stable across modes.
  const showHistory = searchParams.get(HISTORY_QUERY_PARAM) === HISTORY_QUERY_VALUE;
  const now = useNow();
  const runs = data ?? null;
  const runsData =
    runs?.status === 'fresh' || runs?.status === 'fixture' || runs?.status === 'stale'
      ? runs.data
      : null;
  const totalHistorical = runsData?.totalHistorical ?? 0;
  // gascity-dashboard-n6f1: the backend now degrades (not collapses) when a
  // single rig's recent-run query fails, flagging lanesPartial. Surface it
  // so the operator reads a short lane set as "some rigs unavailable" rather
  // than "everything's done." Mirrors the roster-partial signal in Agents.tsx.
  const lanesPartial = runsData?.lanesPartial === true;

  const toggleHistory = useCallback(() => {
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (showHistory) {
          next.delete(HISTORY_QUERY_PARAM);
        } else {
          next.set(HISTORY_QUERY_PARAM, HISTORY_QUERY_VALUE);
        }
        return next;
      },
      { replace: false },
    );
  }, [showHistory, setSearchParams]);

  const runAttentionSeverity = useCallback(
    (lane: RunLane) => resourceAttentionSeverity(attention, 'runs', lane.id),
    [attention],
  );

  const synopsis = runSynopsis(data);

  const freshnessLabel = runs
    ? runs.status === 'fresh'
      ? null
      : runs.status === 'fixture'
        ? 'fixture data'
        : runs.status === 'error'
          ? 'live data unavailable'
          : runs.fetchedAt
            ? `stale ${formatRelative(runs.fetchedAt, now)} ago`
            : 'stale'
    : null;

  return (
    <section>
      <PageHeader
        title="Formula Runs"
        synopsis={synopsis}
        className="md:items-start"
        meta={
          <>
            {error && (
              <span className="normal-case text-body text-accent" role="alert">
                {error}
              </span>
            )}
            {freshnessLabel !== null && (
              <span
                className={`text-label uppercase tracking-wider tnum ${
                  runs?.status === 'error' ? 'text-accent' : 'text-fg-faint'
                }`}
              >
                {freshnessLabel}
              </span>
            )}
            <div className="grid w-full min-w-[18rem] grid-cols-[7rem_minmax(6.5rem,1fr)] items-center gap-x-4 gap-y-3 sm:w-[34rem] sm:grid-cols-[7rem_6.5rem_10rem_7rem]">
              <SseIndicator state={sseState} />
              <span>
                {lanesPartial ? (
                  <PartialDataNotice
                    glyph="◐"
                    label="runs partial"
                    title="one or more rigs' recent runs were unavailable; the lane set may be incomplete"
                  />
                ) : (
                  <span aria-hidden="true" className="invisible normal-case text-body text-warn">
                    runs partial
                  </span>
                )}
              </span>
              <Button
                size="sm"
                className="w-full justify-center"
                onClick={toggleHistory}
                // yh5i: disable only when the toggle is off AND there's
                // nothing to show. If the user already opened history
                // (showHistory=true) we must let them close it, even if
                // the last historical lane has since dropped out — otherwise
                // a back-button + SSE refresh sequence locks the toggle open.
                disabled={!showHistory && totalHistorical === 0}
                aria-expanded={showHistory}
                // aria-controls only references the historical section's id
                // when that element is actually in the DOM; the WAI-ARIA
                // spec requires referenced ids to exist.
                {...(showHistory ? { 'aria-controls': RUNS_HISTORICAL_SECTION_ID } : {})}
                aria-label={
                  showHistory
                    ? 'Hide historical formula runs.'
                    : totalHistorical === 0
                      ? 'No completed formula runs in the current window.'
                      : `Show ${totalHistorical} completed formula runs.`
                }
              >
                {showHistory
                  ? 'Hide history'
                  : totalHistorical > 0
                    ? `Show history (${totalHistorical})`
                    : 'Show history'}
              </Button>
              <Button
                size="sm"
                className="w-full justify-center"
                onClick={() => void refresh()}
                disabled={loading}
              >
                {loading ? 'Refreshing' : 'Refresh'}
              </Button>
            </div>
          </>
        }
      />

      {data === undefined || runs === null ? (
        <p className="text-body text-fg-muted italic">Loading formula runs.</p>
      ) : (
        <RunMap
          source={runs}
          now={now}
          showHistory={showHistory}
          attentionSeverity={runAttentionSeverity}
        />
      )}
    </section>
  );
}

function runSynopsis(data: SourceState<RunSummary> | undefined): string {
  if (data === undefined) return 'Loading formula run lanes.';

  if (data.status !== 'error') {
    return `${data.data.totalActive} active runs across the supervisor's bead store. ${RUN_PHASE_GRAMMAR}`;
  }

  return `Run counts unavailable: ${data.error}. ${RUN_PHASE_GRAMMAR}`;
}

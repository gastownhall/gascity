import { useEffect, useMemo, useRef, useState } from 'react';
import type { StatusBody, UsageBody } from 'gas-city-dashboard-shared/gc-supervisor';
import { activeCityOrThrow, getActiveCity } from '../api/cityBase';
import { useAttentionModel } from '../attention/context';
import {
  ColumnHeading,
  DialCell,
  Gauge,
  LiveChip,
  Microcopy,
  NeedsYouStrip,
  Odometer,
  Oscilloscope,
  PipelineBar,
  RunRings,
  StatusLamps,
  VUBank,
  buildVuMeters,
  computeUsageMetrics,
  fmtCompact,
  fmtCost,
  fmtCount,
  fmtRate,
  laneToRing,
  pipelineSegments,
  statusLamps,
  useCockpitTelemetry,
} from '../components/cockpit';
import { PageHeader } from '../components/PageHeader';
import { useCachedData } from '../hooks/useCachedData';
import { useFaviconSignal } from '../hooks/useFaviconSignal';
import { useRunSummary } from '../runs/runSummarySubscription';
import { supervisorApi } from '../supervisor/client';

// CockpitHome — the live-activity dashboard home (specs/plans/0002). The page's
// job is "show everything happening in the city right now": a scrolling event
// oscilloscope, an odometer of beads closed today, dial-row gauges (events,
// burn, throughput), a bead-status pipeline, and a bottom grid of per-session VU
// meters, run-progress rings, and system lamps. Attention/needs-you is a single
// maroon strip below the header (the sole `text-accent` on the page, DESIGN.md
// One Mark Rule); everything else carries status by shape/position/tone
// (ok-sage, warn-ochre), never accent.
//
// The route composes hooks + JSX only: the live-telemetry model lives in
// useCockpitTelemetry, and the pure source→instrument derivations live in
// components/cockpit/{derive,roster}. Every instrument stays mounted at all
// times: a degraded source rests at its zero/empty state with a small
// per-instrument microcopy line, never a collapsed sentence in place of the
// dial (the anti-Norse rule). Live rates ride `worker.operation` feed events
// when they carry cost/tokens, falling back to the short-poll usage read
// otherwise — no fabricated zeros.

const USAGE_POLL_MS = 20_000; // usage endpoint is time-bucket cached server-side
const STATUS_POLL_MS = 15_000;
const SESSIONS_POLL_MS = 15_000;

const EVENTS_MAX = 150;
const EVENTS_WARN = 120;
const BURN_MAX = 600;
const BURN_WARN = 480;
const TOKENS_MAX = 40_000;
const TOKENS_WARN = 32_000;

// VU full-scale from the cockpit mockup: 620k tokens per minute pins a meter.
const VU_MAX = 620_000;

const MAX_RINGS = 8; // active lanes are wrapped; cap the ring row so it stays legible

export function CockpitHomePage() {
  const city = getActiveCity();
  const cityKey = city ?? 'no-city';

  const { source: runsSource } = useRunSummary();
  const attention = useAttentionModel();

  const [paused, setPaused] = useState(false);
  // Ref mirror so the poll intervals read the live pause state without
  // re-subscribing. Written in render (idempotent) so it is current before the
  // effects below run.
  const pausedRef = useRef(paused);
  pausedRef.current = paused;

  // While paused, hold the last live projection of the SSE-driven sources so
  // nothing on the wall moves. The polls freeze on application (below).
  const runsView = useFrozenWhilePaused(runsSource, paused);
  const attentionView = useFrozenWhilePaused(attention, paused);

  const usageState = useCachedData<UsageBody>(`home:usage:${cityKey}`, () =>
    supervisorApi().cityUsage(activeCityOrThrow('cockpit usage read')),
  );
  const statusState = useCachedData<StatusBody>(`home:status:${cityKey}`, () =>
    supervisorApi().cityStatus(activeCityOrThrow('cockpit status read')),
  );
  const sessionsState = useCachedData(`home:sessions:${cityKey}`, () =>
    supervisorApi().listSessions(activeCityOrThrow('cockpit sessions read')),
  );
  const usageRefresh = usageState.refresh;
  const statusRefresh = statusState.refresh;
  const sessionsRefresh = sessionsState.refresh;

  // Short-poll the reads. Their server responses are time-bucket cached, so a
  // routine poll rides the cache; the interval keeps the dials live between the
  // event stream's coarser cues. Skipped while paused (poll results are not
  // applied), resuming on unpause.
  useEffect(() => {
    const id = setInterval(() => {
      if (!pausedRef.current) void usageRefresh();
    }, USAGE_POLL_MS);
    return () => clearInterval(id);
  }, [usageRefresh]);
  useEffect(() => {
    const id = setInterval(() => {
      if (!pausedRef.current) void statusRefresh();
    }, STATUS_POLL_MS);
    return () => clearInterval(id);
  }, [statusRefresh]);
  useEffect(() => {
    const id = setInterval(() => {
      if (!pausedRef.current) void sessionsRefresh();
    }, SESSIONS_POLL_MS);
    return () => clearInterval(id);
  }, [sessionsRefresh]);

  // The live-telemetry model: one gc event subscription fanned out to the
  // oscilloscope (via `subscribe`) and the feed-derived readings (`feedStats`),
  // frozen on the wall while paused.
  const { feedStats, subscribe, connState } = useCockpitTelemetry(city, paused);

  // ── source projections ────────────────────────────────────────────────
  const runsLoading = runsView === undefined;
  const runsErrored = runsView?.status === 'error';
  const runs = runsView !== undefined && runsView.status !== 'error' ? runsView.data : null;
  // Freeze the polled reads while paused so a poll that was already in flight
  // when pause began cannot move the wall when it resolves (application gate).
  const usage = useFrozenWhilePaused(usageState.data ?? null, paused);
  const status = useFrozenWhilePaused(statusState.data ?? null, paused);
  const sessions = useFrozenWhilePaused(sessionsState.data?.items ?? null, paused);
  const usageMetrics = usage !== null ? computeUsageMetrics(usage) : null;

  // Live-or-fallback rates: the feed window wins when it carries token-bearing
  // ops, otherwise the short-poll usage read stands in. No fabrication — a
  // window of zero-value ops simply leaves the fallback in effect.
  const hasLiveRates = feedStats.windowCostPerHr !== null;
  const usageUnavailable = usage === null && !hasLiveRates;
  const burnPerHr = feedStats.windowCostPerHr ?? usageMetrics?.burnPerHr ?? 0;
  const tokensPerSec = feedStats.windowTokensPerSec ?? usageMetrics?.tokensPerSec ?? 0;

  const rings = useMemo(
    () => (runs !== null ? runs.lanes.slice(0, MAX_RINGS).map(laneToRing) : []),
    [runs],
  );
  const segments = useMemo(() => pipelineSegments(status?.work ?? null), [status]);
  const vuMeters = useMemo(
    () => buildVuMeters(sessions, usage, feedStats.sessionTokPerMin),
    [sessions, usage, feedStats.sessionTokPerMin],
  );
  const lamps = useMemo(
    () => statusLamps(status, feedStats, attentionView.items),
    [status, feedStats, attentionView.items],
  );

  // ── favicon alarm (R8 hysteresis) ─────────────────────────────────────
  // The favicon rides the LIVE attention model (not the pause-frozen view) so an
  // alarm is never silenced by a UI pause. `failing` is the count of actionable
  // ('attention'-severity) items; cycleKey advances per run snapshot.
  const failingCount = attention.items.filter((item) => item.severity === 'attention').length;
  const cycleKey =
    runsSource !== undefined && runsSource.status !== 'error' ? runsSource.fetchedAt : 'pre-snapshot';
  useFaviconSignal({ failing: failingCount, cycleKey });

  // ── header pieces ─────────────────────────────────────────────────────
  const burnKnown = usageMetrics !== null || hasLiveRates;
  const synopsis = (
    <span className="tnum" data-testid="cockpit-synopsis">
      {city ?? 'city'} · {fmtCount(runs !== null ? runs.totalActive : null)} runs ·{' '}
      {fmtCount(status !== null ? (status.session_counts_detail?.active ?? status.running) : null)}{' '}
      sessions · {usageMetrics !== null ? fmtCompact(usageMetrics.tokensToday) : '—'} tokens ·{' '}
      {burnKnown ? `${fmtCost(burnPerHr)}/hr` : '—'}
    </span>
  );

  const liveChip = (
    <LiveChip
      connState={connState}
      paused={paused}
      lastAgeMs={feedStats.lastAgeMs}
      onToggle={() => setPaused((prev) => !prev)}
    />
  );

  const odometerSub =
    usageMetrics !== null ? `${fmtCost(usageMetrics.costToday)} est today` : '— est today';
  const gaugeNote = usageUnavailable ? 'usage unavailable' : undefined;
  const vuNote =
    sessions === null && usage === null
      ? 'usage unavailable'
      : vuMeters.length === 0
        ? 'no live sessions'
        : undefined;
  const systemsNote = status === null ? 'city status unavailable' : undefined;
  const ringsEmptyText = runsErrored
    ? 'run data unavailable'
    : runsLoading
      ? 'loading runs…'
      : 'no runs in flight';

  return (
    <section>
      <PageHeader title="Home" synopsis={synopsis} meta={liveChip} />

      {/* The needs-you strip: the sole maroon mark on the page (One Mark Rule). */}
      <NeedsYouStrip items={attentionView.items} />

      {/* Band 1 — event oscilloscope. */}
      <div className="mb-[1.6rem]">
        <Oscilloscope subscribe={subscribe} paused={paused} />
      </div>

      {/* Band 2 — odometer + dial row. Every instrument stays mounted; a
          degraded source rests at zero with a small microcopy line. */}
      <div className="mb-[1.8rem] flex flex-wrap items-start justify-between gap-x-10 gap-y-8" data-testid="dial-row">
        <Odometer
          value={feedStats.beadsClosedToday}
          digits={4}
          label="beads closed today"
          sub={odometerSub}
        />
        <DialCell>
          <Gauge
            label="events / min"
            value={feedStats.perMin}
            max={EVENTS_MAX}
            warnFrom={EVENTS_WARN}
            format={(value) => String(Math.round(value))}
            tickFormat={(value) => String(Math.round(value))}
            href="/activity?mode=events"
          />
        </DialCell>
        <DialCell note={gaugeNote} noteTestId="burn-note">
          <Gauge
            label="burn · $ / hr"
            value={burnPerHr}
            max={BURN_MAX}
            warnFrom={BURN_WARN}
            format={(value) => fmtCost(value)}
            tickFormat={(value) => String(Math.round(value))}
            href="/activity?mode=events&type=worker.operation"
          />
        </DialCell>
        <DialCell note={gaugeNote} noteTestId="tokens-note">
          <Gauge
            label="tokens / s"
            value={tokensPerSec}
            max={TOKENS_MAX}
            warnFrom={TOKENS_WARN}
            format={(value) => fmtRate(value)}
            tickFormat={(value) => `${Math.round(value / 1000)}k`}
            href="/activity?mode=events&type=worker.operation"
          />
        </DialCell>
      </div>

      {/* Band 3 — bead-status pipeline (always present; zero total = even split). */}
      <div className="mb-[1.8rem]">
        <PipelineBar segments={segments} />
      </div>

      {/* Band 4 — session VU bank / run rings / system lamps. */}
      <div className="grid grid-cols-1 gap-10 lg:[grid-template-columns:5fr_4fr_3fr]">
        <section aria-label="Session token throughput">
          <ColumnHeading>session throughput · tok/min</ColumnHeading>
          <VUBank meters={vuMeters} max={VU_MAX} />
          {vuNote !== undefined && <Microcopy testId="vu-note">{vuNote}</Microcopy>}
        </section>

        <section aria-label="Active run progress">
          <ColumnHeading>formula runs · stage progress</ColumnHeading>
          {rings.length > 0 ? (
            <RunRings runs={rings} />
          ) : (
            <p data-testid="rings-empty" className="text-body italic text-fg-muted">
              {ringsEmptyText}
            </p>
          )}
        </section>

        <section aria-label="System status">
          <ColumnHeading>systems</ColumnHeading>
          <StatusLamps lamps={lamps} />
          {systemsNote !== undefined && <Microcopy testId="systems-note">{systemsNote}</Microcopy>}
        </section>
      </div>
    </section>
  );
}

// ── freeze-while-paused ───────────────────────────────────────────────────

/**
 * Holds the last live value while `paused` is true so an SSE-driven source stops
 * moving on the wall; returns the live value again the moment pause lifts. The
 * ref write during render is idempotent (same input → same output).
 */
function useFrozenWhilePaused<T>(live: T, paused: boolean): T {
  const ref = useRef(live);
  if (!paused) ref.current = live;
  return ref.current;
}

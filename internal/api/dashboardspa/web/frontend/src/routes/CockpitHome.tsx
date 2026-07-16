import { useEffect, useMemo, useRef, useState, type MutableRefObject } from 'react';
import { Link } from 'react-router-dom';
import type {
  ListBodySessionResponse,
  RunsCensusOutputBody,
  StatusBody,
  UsageBody,
} from 'gas-city-dashboard-shared/gc-supervisor';
import { activeCityOrThrow, getActiveCity } from '../api/cityBase';
import { useAttentionModel } from '../attention/context';
import {
  ActivityTrace,
  ContextMeters,
  Gauge,
  InstrumentNote,
  Odometer,
  PipelineBar,
  RunRings,
  StatusLamps,
  type LampState,
} from '../components/cockpit/Instruments';
import {
  burnPerHour,
  laneToRing,
  pipelineSegments,
  tokensPerMinute,
} from '../components/cockpit/model';
import { PageHeader } from '../components/PageHeader';
import { useCachedData } from '../hooks/useCachedData';
import { useRunSummary } from '../runs/runSummarySubscription';
import { SUPERVISOR_REQUEST_TIMEOUT_MS, supervisorApi } from '../supervisor/client';

const POLL_MS = 15_000;
const MAX_TRACE_SAMPLES = 48;
const MAX_RINGS = 8;

export function CockpitHomePage() {
  const city = getActiveCity();
  const cityKey = city ?? 'no-city';
  const [paused, setPaused] = useState(false);
  const pausedRef = useRef(paused);
  pausedRef.current = paused;

  const usageState = useCachedData<UsageBody>(`cockpit:usage:${cityKey}`, () =>
    supervisorApi().cityUsage(activeCityOrThrow('cockpit usage read')),
  );
  const statusState = useCachedData<StatusBody>(`cockpit:status:${cityKey}`, () =>
    supervisorApi().cityStatus(activeCityOrThrow('cockpit status read')),
  );
  const runsState = useCachedData<RunsCensusOutputBody>(`cockpit:runs:${cityKey}`, () =>
    supervisorApi().runCensus(activeCityOrThrow('cockpit run census read')),
  );
  const sessionsState = useCachedData<ListBodySessionResponse>(`cockpit:sessions:${cityKey}`, () =>
    supervisorApi().listSessions(activeCityOrThrow('cockpit sessions read')),
  );
  const runSummary = useRunSummary();
  const attention = useAttentionModel();

  usePoll(usageState.refresh, usageState.loading, pausedRef);
  usePoll(statusState.refresh, statusState.loading, pausedRef);
  usePoll(runsState.refresh, runsState.loading, pausedRef);
  usePoll(sessionsState.refresh, sessionsState.loading, pausedRef);

  const usageReading = useFrozenWhilePaused(useReadingSnapshot(usageState, cityKey), paused);
  const statusReading = useFrozenWhilePaused(useReadingSnapshot(statusState, cityKey), paused);
  const runsReading = useFrozenWhilePaused(useReadingSnapshot(runsState, cityKey), paused);
  const sessionsReading = useFrozenWhilePaused(useReadingSnapshot(sessionsState, cityKey), paused);
  const runProjection = useFrozenWhilePaused(
    {
      source: runSummary.source,
      loading: runSummary.loading,
      sseState: runSummary.sseState,
    },
    paused,
  );

  const usage = usageReading.data;
  const status = statusReading.data;
  const canonicalRuns = runsReading.data;
  const sessions = sessionsReading.data;
  const richRuns = runProjection.source;

  const [activitySamples, setActivitySamples] = useState<number[]>([]);
  const lastUsageSampleRef = useRef<string | null>(null);
  useEffect(() => {
    if (paused || usage === undefined || !usage.available) return;
    if (lastUsageSampleRef.current === usage.updated_at) return;
    lastUsageSampleRef.current = usage.updated_at;
    const next = Math.max(0, usage.recent.invocations);
    setActivitySamples((previous) => [...previous, next].slice(-MAX_TRACE_SAMPLES));
  }, [paused, usage]);

  const usageAvailable = usage?.available === true;
  const usageDomainNote =
    usage === undefined
      ? undefined
      : [
          !usage.available ? 'usage recording is not local' : undefined,
          usage.available && !usage.recording ? 'usage recording is off' : undefined,
          usage.partial
            ? usage.partial_reasons?.join(' · ') || 'usage estimate is partial'
            : undefined,
          usage.today.unpriced > 0 || usage.recent.unpriced > 0
            ? 'cost excludes unpriced model calls'
            : undefined,
        ]
          .filter((note): note is string => note !== undefined)
          .join(' · ') || undefined;
  const recentTokens = usageAvailable
    ? tokensPerMinute(usage.recent, usage.recent_window_secs)
    : null;
  const recentBurn = usageAvailable ? burnPerHour(usage.recent, usage.recent_window_secs) : null;
  const activeSessionsFromStatus = status?.session_counts_detail?.active;
  const activeSessions =
    activeSessionsFromStatus ??
    (sessions === undefined
      ? null
      : (sessions.items ?? []).filter((session) => session.running).length);
  const segments = useMemo(
    () => pipelineSegments(canonicalRuns?.status_counts ?? null),
    [canonicalRuns?.status_counts],
  );
  const contextMeters = useMemo(
    () =>
      (sessions?.items ?? [])
        .filter(
          (session) =>
            session.running &&
            typeof session.context_pct === 'number' &&
            Number.isFinite(session.context_pct),
        )
        .sort((a, b) => (b.context_pct ?? 0) - (a.context_pct ?? 0))
        .slice(0, 8)
        .map((session) => ({
          id: session.id,
          label: session.title || session.session_name || session.template,
          value: session.context_pct ?? 0,
          href: '/agents',
        })),
    [sessions?.items],
  );
  const rings = useMemo(() => {
    if (richRuns === undefined || richRuns.status === 'error') return [];
    return [...richRuns.data.lanes, ...richRuns.data.blockedLanes]
      .slice(0, MAX_RINGS)
      .map(laneToRing);
  }, [richRuns]);

  const feedState: LampState = runProjection.sseState === 'open' ? 'healthy' : 'unknown';
  const statusStale = status !== undefined && statusReading.stale;
  const statusPartial = status?.partial === true;
  const statusProvenance = statusStale ? 'stale' : statusPartial ? 'partial' : null;
  const lamps = [
    {
      key: 'feed',
      label: 'live feed',
      value:
        runProjection.sseState === 'open' ? 'connected' : connectionLabel(runProjection.sseState),
      state: feedState,
      href: '/activity',
    },
    status === undefined
      ? {
          key: 'store',
          label: 'dolt store',
          value: 'unavailable',
          state: 'unknown' as const,
          href: '/health',
        }
      : status.store_health === undefined
        ? {
            key: 'store',
            label: 'dolt store',
            value: 'not reported',
            state: 'unknown' as const,
            href: '/health',
          }
        : {
            key: 'store',
            label: 'dolt store',
            value:
              statusProvenance === null
                ? storeHealthLabel(status.store_health)
                : `${statusProvenance} · last reported ${storeHealthLabel(status.store_health)}`,
            state:
              statusProvenance !== null
                ? ('unknown' as const)
                : storeHealthLabel(status.store_health) !== 'healthy'
                  ? ('warning' as const)
                  : ('healthy' as const),
            href: '/health',
          },
    status === undefined
      ? {
          key: 'mail',
          label: 'mail',
          value: 'unavailable',
          state: 'unknown' as const,
          href: '/mail',
        }
      : {
          key: 'mail',
          label: 'mail',
          value:
            statusProvenance === null
              ? `${status.mail.unread} unread`
              : `${statusProvenance} · last reported ${status.mail.unread} unread`,
          state:
            statusProvenance !== null
              ? ('unknown' as const)
              : status.mail.unread > 0
                ? ('warning' as const)
                : ('healthy' as const),
          href: '/mail',
        },
    status === undefined
      ? {
          key: 'agents',
          label: 'agents',
          value: 'unavailable',
          state: 'unknown' as const,
          href: '/agents',
        }
      : {
          key: 'agents',
          label: 'agents',
          value: `${statusProvenance === null ? '' : `${statusProvenance} · last reported `}${
            status.agents.quarantined > 0
              ? `${status.agents.quarantined} quarantined`
              : `${status.agents.running}/${status.agents.total} running`
          }`,
          state:
            statusProvenance !== null
              ? ('unknown' as const)
              : status.agents.quarantined > 0 || status.agents.suspended > 0
                ? ('warning' as const)
                : ('healthy' as const),
          href: '/agents',
        },
  ];

  const usageNote = readingNote(usageReading, 'usage', usageDomainNote);
  const statusNote = readingNote(
    statusReading,
    'city status',
    status?.partial ? 'city status is partial' : undefined,
  );
  const runsNote = readingNote(
    runsReading,
    'run states',
    canonicalRuns?.partial ? 'run projection is partial' : undefined,
  );
  const sessionsNote = readingNote(
    sessionsReading,
    'sessions',
    sessions?.partial ? 'session list is partial' : undefined,
  );
  const activeSessionsNote = activeSessionsFromStatus === undefined ? sessionsNote : statusNote;
  const ringsNote =
    richRuns === undefined
      ? runProjection.loading
        ? 'loading run progress…'
        : 'run progress unavailable'
      : richRuns.status === 'error'
        ? 'run progress unavailable'
        : richRuns.status === 'stale'
          ? 'run progress is stale'
          : rings.length === 0
            ? 'no runs in flight'
            : undefined;

  const synopsis = `${city ?? 'city'} · ${formatCount(activeSessions)} active sessions · ${formatCount(
    canonicalRuns?.status_counts.active,
  )} running · ${usageAvailable ? formatCompact(usage.today.input_tokens + usage.today.output_tokens + usage.today.cache_read_tokens + usage.today.cache_creation_tokens) : '—'} tokens today`;

  return (
    <section>
      <PageHeader
        title="Home"
        synopsis={synopsis}
        meta={
          <button
            type="button"
            aria-pressed={paused}
            onClick={() => setPaused((current) => !current)}
            className="focus-mark min-h-6 border-b border-rule text-fg-muted hover:text-fg"
          >
            {paused ? 'resume' : 'pause'} instruments
          </button>
        }
      />

      <NeedsYouStrip items={attention.topItems} />

      <div className="mb-8">
        <ActivityTrace
          samples={activitySamples}
          available={usageAvailable}
          note={
            usageNote ??
            (activitySamples.length === 0 ? 'waiting for the first usage sample' : undefined)
          }
        />
      </div>

      <div
        className="mb-8 grid items-start justify-items-center gap-x-4 gap-y-8 [grid-template-columns:repeat(auto-fit,minmax(150px,1fr))]"
        data-testid="dial-grid"
      >
        <Odometer
          label="model calls today"
          value={usageAvailable ? usage.today.invocations : null}
          note={
            usageAvailable
              ? [`${formatUsd(usage.today.cost_usd_estimate)} estimated today`, usageNote]
                  .filter((note): note is string => note !== undefined)
                  .join(' · ')
              : usageNote
          }
        />
        <Gauge
          label="active sessions"
          value={activeSessions}
          max={Math.max(10, (activeSessions ?? 0) * 1.25)}
          formatted={formatCount(activeSessions)}
          href="/agents"
          note={activeSessionsNote}
        />
        <Gauge
          label="tokens / min"
          value={recentTokens}
          max={Math.max(1_000, (recentTokens ?? 0) * 1.25)}
          formatted={recentTokens === null ? '—' : formatCompact(recentTokens)}
          href="/activity"
          note={usageNote}
        />
        <Gauge
          label="burn · $ / hr"
          value={recentBurn}
          max={Math.max(10, (recentBurn ?? 0) * 1.25)}
          formatted={recentBurn === null ? '—' : formatUsd(recentBurn)}
          href="/activity"
          note={usageNote}
        />
      </div>

      <section className="mb-8" aria-labelledby="run-state-title">
        <h2 id="run-state-title" className="mb-2 text-label uppercase tracking-wider text-fg-faint">
          runs in flight · canonical state
        </h2>
        <PipelineBar segments={segments} available={canonicalRuns !== undefined} />
        {runsNote && <InstrumentNote>{runsNote}</InstrumentNote>}
      </section>

      <div className="grid grid-cols-1 gap-10 lg:[grid-template-columns:5fr_4fr_3fr]">
        <section aria-labelledby="context-title">
          <h2 id="context-title" className="mb-2 text-label uppercase tracking-wider text-fg-faint">
            live session context
          </h2>
          <ContextMeters meters={contextMeters} />
          {(sessionsNote || contextMeters.length === 0) && (
            <InstrumentNote>{sessionsNote ?? 'no live session context reported'}</InstrumentNote>
          )}
        </section>

        <section aria-labelledby="progress-title">
          <h2
            id="progress-title"
            className="mb-2 text-label uppercase tracking-wider text-fg-faint"
          >
            formula run progress
          </h2>
          <RunRings runs={rings} />
          {ringsNote && <InstrumentNote>{ringsNote}</InstrumentNote>}
        </section>

        <section aria-labelledby="systems-title">
          <h2 id="systems-title" className="mb-2 text-label uppercase tracking-wider text-fg-faint">
            systems
          </h2>
          <StatusLamps lamps={lamps} />
        </section>
      </div>
    </section>
  );
}

function usePoll(
  refresh: () => Promise<void>,
  loading: boolean,
  pausedRef: MutableRefObject<boolean>,
) {
  useEffect(() => {
    // A fixed interval can supersede every response that takes longer than the
    // cadence because useCachedData publishes only its latest run. Keep one
    // timer instead: poll after settlement, or elect a recovery attempt after
    // the supervisor request budget if a broken fetch never settles.
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;

    function schedule(delay: number) {
      if (stopped) return;
      if (timer !== undefined) clearTimeout(timer);
      timer = setTimeout(run, delay);
    }

    function run() {
      timer = undefined;
      if (pausedRef.current) {
        schedule(POLL_MS);
        return;
      }

      const pending = refresh();
      schedule(SUPERVISOR_REQUEST_TIMEOUT_MS);
      void pending.then(
        () => schedule(POLL_MS),
        () => schedule(POLL_MS),
      );
    }

    schedule(loading ? SUPERVISOR_REQUEST_TIMEOUT_MS : POLL_MS);
    return () => {
      stopped = true;
      if (timer !== undefined) clearTimeout(timer);
    };
  }, [loading, pausedRef, refresh]);
}

function useFrozenWhilePaused<T>(live: T, paused: boolean): T {
  const valueRef = useRef(live);
  if (!paused) valueRef.current = live;
  return valueRef.current;
}

function useReadingSnapshot<T>(
  state: {
    data: T | undefined;
    loading: boolean;
    error: string | null;
    fetchedAt: string | undefined;
  },
  key: string,
) {
  const failedRef = useRef<{ key: string; data: T; fetchedAt: string | undefined } | null>(null);
  if (failedRef.current?.key !== key) failedRef.current = null;

  if (state.error !== null && state.data !== undefined) {
    failedRef.current = { key, data: state.data, fetchedAt: state.fetchedAt };
  } else if (failedRef.current !== null && !state.loading) {
    failedRef.current = null;
  }

  const failed = failedRef.current;
  return {
    data: failed?.data ?? state.data,
    loading: state.loading,
    fetchedAt: failed?.fetchedAt ?? state.fetchedAt,
    stale: failed !== null,
  };
}

function readingNote<T>(
  state: {
    data: T | undefined;
    loading: boolean;
    fetchedAt: string | undefined;
    stale: boolean;
  },
  label: string,
  domainNote?: string,
): string | undefined {
  if (state.data === undefined) {
    if (state.loading) return `loading ${label}…`;
    return `${label} unavailable`;
  }
  if (state.stale) {
    return `${label} is stale · refresh failed`;
  }
  if (domainNote) return domainNote;
  return undefined;
}

function storeHealthLabel(storeHealth: NonNullable<StatusBody['store_health']>): string {
  const lastMaintenanceStatus = storeHealth.last_gc_status?.trim();
  if (lastMaintenanceStatus && lastMaintenanceStatus !== 'success') return 'maintenance failed';
  return storeHealth.warning ? 'maintenance overdue' : 'healthy';
}

function NeedsYouStrip({
  items,
}: {
  items: readonly { id: string; title: string; href?: string; severity: string }[];
}) {
  const item = items.find((candidate) => candidate.severity === 'attention');
  if (!item) return null;
  const body = (
    <>
      <span className="mr-2 uppercase tracking-wider">needs you</span>
      <span className="text-fg">{item.title}</span>
    </>
  );
  return (
    <div className="mb-8 border-y border-accent/30 py-2 text-label text-accent">
      {item.href ? (
        <Link to={item.href} className="focus-mark inline-block min-h-6 no-underline">
          {body}
        </Link>
      ) : (
        body
      )}
    </div>
  );
}

function connectionLabel(state: string): string {
  switch (state) {
    case 'connecting':
      return 'connecting';
    case 'degraded':
      return 'degraded';
    default:
      return 'disconnected';
  }
}

function formatCount(value: number | null | undefined): string {
  return typeof value === 'number' && Number.isFinite(value)
    ? String(Math.max(0, Math.round(value)))
    : '—';
}

function formatCompact(value: number): string {
  return new Intl.NumberFormat('en', { notation: 'compact', maximumFractionDigits: 1 }).format(
    Math.max(0, value),
  );
}

function formatUsd(value: number): string {
  return new Intl.NumberFormat('en-US', {
    style: 'currency',
    currency: 'USD',
    maximumFractionDigits: 2,
  }).format(Math.max(0, value));
}

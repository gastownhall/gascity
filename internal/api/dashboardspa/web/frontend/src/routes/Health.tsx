import { useCallback, type ReactNode } from 'react';
import type {
  DoltNomsTrend,
  LocalToolVersion,
  LocalToolVersions,
  RigStoreHealth,
  RigStoreHealthReport,
  RigStoreHealthUnavailableReason,
  RigStoreRollup,
  SupervisorStatusReport,
  SystemHealth,
} from 'gas-city-dashboard-shared';
import { api, formatApiError } from '../api/client';
import { getActiveCity } from '../api/cityBase';
import { useAttentionModel } from '../attention/context';
import { attentionSectionProps, prefixedAttentionSeverity } from '../attention/routeHighlight';
import { Button } from '../components/Button';
import { PageHeader } from '../components/PageHeader';
import { StatusBadge, type StatusTone } from '../components/StatusBadge';
import type {
  HealthOutputBody,
  StatusBody,
  StatusStoreHealth,
  StatusWorkCounts,
} from 'gas-city-dashboard-shared/gc-supervisor';
import { useCachedData } from '../hooks/useCachedData';
import { useVisibleRefresh } from '../hooks/useVisibleRefresh';
import { formatHumanSize } from '../lib/format';
import { formatShortDate } from '../hooks/time';
import { supervisorApiForRequestBudget } from '../supervisor/client';

const HEALTH_SUPERVISOR_REQUEST_TIMEOUT_MS = 2_500;

export function HealthPage() {
  const attention = useAttentionModel();
  const cityName = getActiveCity();
  const systemHealth = useCachedData('health:system', fetchSystemHealth);
  const supervisorHealth = useCachedData(
    `health:supervisor:${cityName ?? 'no-city'}`,
    fetchSupervisorHealth,
  );
  const supervisorStatusCache = useCachedData(
    `health:status:${cityName ?? 'no-city'}`,
    fetchSupervisorStatus,
  );
  const localToolVersions = useCachedData('health:local-tools', fetchLocalToolVersions);
  const doltNomsTrend = useCachedData(
    `health:dolt-noms-trend:${cityName ?? 'no-city'}`,
    fetchDoltNomsTrend,
  );
  const rigStoreHealth = useCachedData(
    `health:rig-store:${cityName ?? 'no-city'}`,
    fetchRigStoreHealth,
  );
  const refreshSystemHealth = systemHealth.refresh;
  const refreshSupervisorHealth = supervisorHealth.refresh;
  const refreshSupervisorStatus = supervisorStatusCache.refresh;
  const refreshLocalToolVersions = localToolVersions.refresh;
  const refreshDoltNomsTrend = doltNomsTrend.refresh;
  const refreshRigStoreHealth = rigStoreHealth.refresh;
  const sourceLoading =
    systemHealth.loading ||
    supervisorHealth.loading ||
    supervisorStatusCache.loading ||
    localToolVersions.loading ||
    doltNomsTrend.loading ||
    rigStoreHealth.loading;
  const error =
    [
      systemHealth.error,
      supervisorHealth.error,
      supervisorStatusCache.error,
      localToolVersions.error,
      doltNomsTrend.error,
      rigStoreHealth.error,
    ]
      .filter((value): value is string => value !== null)
      .join('; ') || null;
  const refresh = useCallback(async () => {
    await Promise.all([
      refreshSystemHealth(),
      refreshSupervisorHealth(),
      refreshSupervisorStatus(),
      refreshLocalToolVersions(),
      refreshDoltNomsTrend(),
      refreshRigStoreHealth(),
    ]);
  }, [
    refreshDoltNomsTrend,
    refreshLocalToolVersions,
    refreshRigStoreHealth,
    refreshSupervisorHealth,
    refreshSupervisorStatus,
    refreshSystemHealth,
  ]);
  const healthState = systemHealth.data ?? null;
  const health = healthState?.status === 'available' ? healthState.data : null;
  const healthError = healthState?.status === 'unavailable' ? healthState.error : null;
  const supervisor = supervisorHealth.data ?? null;
  const status = supervisorStatusCache.data ?? null;
  const localTools = localToolVersions.data ?? null;
  const trend = doltNomsTrend.data ?? null;
  const rigStores = rigStoreHealth.data ?? null;
  const rigStoreSummary = rigStores ? rigStoreSummaryStatus(rigStores) : undefined;
  const hasAnyData =
    healthState !== null ||
    supervisor !== null ||
    status !== null ||
    localTools !== null ||
    trend !== null ||
    rigStores !== null;
  const hostHealthStatus = health ? hostStatus(health) : undefined;
  const supervisorAttention = prefixedAttentionSeverity(attention, 'health', [
    'health:supervisor-',
  ]);
  const hostAttention = prefixedAttentionSeverity(attention, 'health', [
    'health:load-',
    'health:memory-',
  ]);
  const adminAttention = prefixedAttentionSeverity(attention, 'health', ['health:dashboard-']);
  const doltNomsAttention = prefixedAttentionSeverity(attention, 'health', ['health:dolt-noms-']);

  useVisibleRefresh(refresh, 30_000);

  return (
    <section>
      <PageHeader
        title="Health"
        synopsis={
          hasAnyData ? buildSynopsis(health, supervisor) : 'Reading state from the supervisor.'
        }
        meta={
          <>
            {error && (
              <span className="normal-case text-body text-accent" role="alert">
                {error}
              </span>
            )}
            <Button size="sm" onClick={() => void refresh()}>
              {sourceLoading && !hasAnyData ? 'Loading' : 'Refresh'}
            </Button>
          </>
        }
      />

      {hasAnyData ? (
        <div className="space-y-12">
          <Section
            title="Supervisor"
            attention={supervisorAttention}
            {...(supervisor ? { status: supervisorStatus(supervisor) } : {})}
          >
            {supervisor === null ? (
              <p className="text-body text-fg-muted italic">Loading supervisor state.</p>
            ) : supervisor.status === 'available' ? (
              <KvList>
                {/* izgc F7/F8: city + version are optional per supervisor's
                    OpenAPI. Absence is itself a wire-drift signal — render
                    in 'warn' tone rather than coalescing to a glanceable
                    em-dash, so the operator notices the regression. */}
                {supervisor.data.city !== undefined ? (
                  <Kv label="City" value={supervisor.data.city} />
                ) : (
                  <Kv label="City" value="not reported by supervisor" tone="warn" />
                )}
                {supervisor.data.version !== undefined ? (
                  <Kv label="Version" value={supervisor.data.version} />
                ) : (
                  <Kv label="Version" value="not reported by supervisor" tone="warn" />
                )}
                <Kv label="Uptime" value={formatDuration(supervisor.data.uptime_sec)} />
                <Kv label="Status" value={supervisor.data.status} />
              </KvList>
            ) : (
              <p className="text-body text-accent">
                Supervisor not reachable. The dashboard shell stays up; live data is stale.
              </p>
            )}
          </Section>

          <Section
            title="Host"
            attention={hostAttention}
            {...(hostHealthStatus ? { status: hostHealthStatus } : {})}
          >
            {healthState === null ? (
              <p className="text-body text-fg-muted italic">Loading dashboard host health.</p>
            ) : health === null ? (
              <p className="text-body text-accent">
                Dashboard host health unavailable{healthError ? `: ${healthError}` : ''}.
              </p>
            ) : (
              <KvList>
                <Kv label="CPUs" value={health.host.cpu_count.toString()} />
                <Kv
                  label="Load (1m, 5m, 15m)"
                  value={`${health.host.load_avg_1.toFixed(2)}, ${health.host.load_avg_5.toFixed(2)}, ${health.host.load_avg_15.toFixed(2)}`}
                  {...(health.host.load_avg_1 > health.host.cpu_count
                    ? { tone: 'warn' as const }
                    : {})}
                />
                <Kv
                  label="Memory free"
                  value={`${formatHumanSize(health.host.free_mem_bytes)} of ${formatHumanSize(health.host.total_mem_bytes)}`}
                  {...(health.host.free_mem_bytes / health.host.total_mem_bytes < 0.1
                    ? { tone: 'warn' as const }
                    : {})}
                />
                <Kv label="Host uptime" value={formatDuration(health.host.uptime_sec)} />
              </KvList>
            )}
          </Section>

          <Section title="Admin process" attention={adminAttention}>
            {healthState === null ? (
              <p className="text-body text-fg-muted italic">Loading dashboard process health.</p>
            ) : health === null ? (
              <p className="text-body text-accent">
                Dashboard process health unavailable{healthError ? `: ${healthError}` : ''}.
              </p>
            ) : (
              <KvList>
                <Kv label="PID" value={health.admin.pid.toString()} />
                <Kv label="Uptime" value={formatDuration(health.admin.uptime_sec)} />
                <Kv label="RSS" value={formatHumanSize(health.admin.rss_bytes)} />
                <Kv label="Heap used" value={formatHumanSize(health.admin.heap_used_bytes)} />
                <Kv label="Node" value={health.admin.node_version} />
              </KvList>
            )}
          </Section>

          <Section title="Tool versions">
            <ToolVersionsTable state={localTools} />
          </Section>

          <Section title="Diagnostics">
            <div className="space-y-8">
              <DoltUsageBlock usage={doltUsageOf(status)} />
              <BeadsUsageBlock usage={beadsUsageOf(status)} />
            </div>
          </Section>

          <Section
            title="Bead stores · per rig"
            meta={rigStoreMeta(rigStores)}
            {...(rigStoreSummary ? { status: rigStoreSummary } : {})}
          >
            <RigStoreHealthBlock report={rigStores} />
          </Section>

          <Section title="Store thresholds">
            <ConfigComparison comparison={configComparisonOf(status)} />
          </Section>

          <Section
            title="Dolt-noms · 24 h"
            attention={doltNomsAttention}
            meta={trend && trend.samples.length > 0 ? `${trend.samples.length} samples` : undefined}
          >
            {trend === null ? (
              <p className="text-body text-fg-muted italic">Loading.</p>
            ) : !trend.available ? (
              <p className="text-body text-fg-muted italic">
                Dolt-noms metric unavailable: {doltUnavailableCopy(trend.reason)}.
              </p>
            ) : trend.samples.length === 0 ? (
              <p className="text-body text-fg-muted italic">
                No samples yet. Backend just started; next sample in ten minutes or less.
              </p>
            ) : (
              <Sparkline samples={trend.samples} />
            )}
          </Section>
        </div>
      ) : (
        <p className="text-body text-fg-muted italic">Loading.</p>
      )}
    </section>
  );
}

function Section({
  title,
  status,
  meta,
  attention,
  children,
}: {
  title: string;
  status?: { tone: StatusTone; label: string };
  meta?: ReactNode;
  attention?: ReturnType<typeof prefixedAttentionSeverity>;
  children: ReactNode;
}) {
  return (
    <section {...attentionSectionProps(attention ?? null)}>
      <header className="flex items-baseline justify-between gap-4 mb-4 pb-2 border-b border-rule">
        <h2 className="text-headline font-semibold text-fg">{title}</h2>
        <div className="flex items-baseline gap-4">
          {meta && (
            <span className="text-label uppercase tracking-wider text-fg-muted">{meta}</span>
          )}
          {status && <StatusBadge tone={status.tone} label={status.label} />}
        </div>
      </header>
      {children}
    </section>
  );
}

function KvList({ children }: { children: ReactNode }) {
  // Two-column typeset list. Label left, value right, hairlines
  // between rows. Tabular numerals for value alignment.
  return (
    <dl className="grid grid-cols-[max-content_1fr] gap-x-8 gap-y-3 max-w-prose">{children}</dl>
  );
}

function Kv({ label, value, tone }: { label: string; value: string; tone?: 'warn' | 'stuck' }) {
  const valueColor = tone === 'warn' ? 'text-warn' : tone === 'stuck' ? 'text-accent' : 'text-fg';
  return (
    <>
      <dt className="text-body text-fg-muted">{label}</dt>
      <dd className={`text-body tnum font-medium ${valueColor}`}>{value}</dd>
    </>
  );
}

type DiagnosticDatum<T> =
  | { status: 'available'; value: T; source: string; stale?: string }
  | { status: 'unavailable'; reason: string };

interface ConfigComparisonRow {
  label: string;
  recommended: string;
  loaded: string;
  withinRecommendation: boolean;
}

function ToolVersionsTable({ state }: { state: LocalToolVersionsState | null }) {
  if (state === null) {
    return <p className="text-body text-fg-muted italic">Loading tool versions.</p>;
  }
  if (state.status === 'unavailable') {
    return (
      <p className="text-body text-fg-muted italic">Tool versions unavailable: {state.error}.</p>
    );
  }
  // gc first (it frames the section), then the tools the operator most often
  // checks. A plain passthrough of each installed version — gc owns version-floor
  // policy, so the panel reports what is installed without judging it.
  const rows: { label: string; tool: LocalToolVersion }[] = [
    { label: 'gc', tool: state.data.gc },
    { label: 'bd', tool: state.data.beads },
    { label: 'dolt', tool: state.data.dolt },
  ];
  return (
    <div className="grid grid-cols-[1fr_max-content] gap-x-8 gap-y-3 max-w-prose">
      <div className="text-label uppercase tracking-wider text-fg-muted">Tool</div>
      <div className="text-label uppercase tracking-wider text-fg-muted text-right">Installed</div>
      {rows.map((row) => (
        <ToolVersionRow key={row.label} label={row.label} tool={row.tool} />
      ))}
    </div>
  );
}

function ToolVersionRow({ label, tool }: { label: string; tool: LocalToolVersion }) {
  // `contents` makes the row's cells participate directly in the parent grid.
  return (
    <div className="contents" data-tool-version-row={label}>
      <div className="text-body text-fg">{label}</div>
      <div className="text-right">
        {tool.status === 'available' ? (
          <span className="text-body tnum font-medium text-fg">{tool.version}</span>
        ) : (
          <div className="space-y-1">
            <div className="text-body tnum font-medium text-warn">unavailable</div>
            <div className="text-label text-fg-muted normal-case">{tool.reason}</div>
          </div>
        )}
      </div>
    </div>
  );
}

function DoltUsageBlock({ usage }: { usage: DiagnosticDatum<StatusStoreHealth> }) {
  if (usage.status === 'unavailable') {
    return <UnavailableNote heading="Dolt usage" reason={usage.reason} />;
  }
  const u = usage.value;
  return (
    <div className="space-y-2">
      <h3 className="text-label uppercase tracking-wider text-fg-muted">Dolt usage</h3>
      {usage.stale !== undefined && <StaleNote message={usage.stale} />}
      <KvList>
        <Kv label="On-disk size" value={formatHumanSize(statusNumber(u.size_bytes))} />
        <Kv label="Live rows" value={u.live_rows.toLocaleString()} />
        <Kv label="MB per row" value={u.ratio_mb_per_row.toString()} />
        <Kv
          label="Last maintenance"
          value={u.last_gc_status ?? 'not reported'}
          {...(u.last_gc_status !== undefined && u.last_gc_status !== 'success'
            ? { tone: 'warn' as const }
            : {})}
        />
        {u.last_gc_at !== undefined && (
          <Kv label="Last maintenance at" value={formatShortDate(u.last_gc_at)} />
        )}
        <Kv label="Store path" value={u.path} />
      </KvList>
    </div>
  );
}

function BeadsUsageBlock({ usage }: { usage: DiagnosticDatum<StatusWorkCounts> }) {
  if (usage.status === 'unavailable') {
    return <UnavailableNote heading="Beads usage" reason={usage.reason} />;
  }
  const u = usage.value;
  return (
    <div className="space-y-2">
      <h3 className="text-label uppercase tracking-wider text-fg-muted">Beads usage</h3>
      {usage.stale !== undefined && <StaleNote message={usage.stale} />}
      <KvList>
        <Kv label="Open" value={u.open.toString()} />
        <Kv label="Ready" value={u.ready.toString()} />
        <Kv label="In progress" value={u.in_progress.toString()} />
      </KvList>
    </div>
  );
}

function RigStoreHealthBlock({ report }: { report: RigStoreHealthReport | null }) {
  if (report === null) {
    return <p className="text-body text-fg-muted italic">Loading per-rig store health.</p>;
  }
  if (!report.available && report.rigs.length === 0) {
    return (
      <p className="text-body text-fg-muted italic">
        Per-rig store health unavailable: {rigStoreUnavailableCopy(report.reason)}.
      </p>
    );
  }
  // Sort worst-first so a degraded store is never buried below healthy ones.
  const rigs = [...report.rigs].sort((a, b) => rollupRank(b.rollup) - rollupRank(a.rollup));
  return (
    <div className="space-y-6 max-w-prose">
      {!report.available && (
        <p className="text-body text-warn italic">
          Showing the last sample; refresh failed: {rigStoreUnavailableCopy(report.reason)}.
        </p>
      )}
      {rigs.map((rig) => (
        <RigStoreRow key={rig.rig} rig={rig} />
      ))}
    </div>
  );
}

function RigStoreRow({ rig }: { rig: RigStoreHealth }) {
  const status = rigRollupStatus(rig);
  return (
    <div className="space-y-2 border-b border-rule pb-4 last:border-b-0">
      <div className="flex items-baseline justify-between gap-4">
        <span className="text-body font-medium text-fg">{rig.rig}</span>
        <StatusBadge tone={status.tone} label={status.label} />
      </div>
      <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1">
        <Kv
          label="Dolt server"
          value={rigDoltServerValue(rig)}
          {...(rig.doltConnected === false ? { tone: 'stuck' as const } : {})}
        />
        {rig.issueCount !== null && (
          <Kv label="Live issues" value={rig.issueCount.toLocaleString()} />
        )}
      </dl>
      {rig.problems.length > 0 && (
        <ul className="space-y-1">
          {rig.problems.map((p) => (
            <li
              key={`${p.category}/${p.name}`}
              className={`text-label ${p.status === 'error' ? 'text-accent' : 'text-warn'}`}
            >
              {p.name}: {p.message}
            </li>
          ))}
        </ul>
      )}
      {rig.note !== undefined && <p className="text-label text-fg-muted italic">{rig.note}</p>}
    </div>
  );
}

function rigDoltServerValue(rig: RigStoreHealth): string {
  const endpoint = rig.doltEndpoint ?? 'no endpoint reported';
  if (rig.doltConnected === true) return `up · ${endpoint}`;
  if (rig.doltConnected === false) return `DOWN · ${endpoint}`;
  return `unknown · ${endpoint}`;
}

function rigRollupStatus(rig: RigStoreHealth): { tone: StatusTone; label: string } {
  switch (rig.rollup) {
    case 'ok':
      return { tone: 'ok', label: 'healthy' };
    case 'warn':
      return { tone: 'warn', label: 'warnings' };
    case 'down':
      if (!rig.reachable) return { tone: 'stuck', label: 'unreachable' };
      if (rig.doltConnected === false) return { tone: 'stuck', label: 'dolt down' };
      return { tone: 'stuck', label: 'errors' };
  }
}

function rollupRank(rollup: RigStoreRollup): number {
  return rollup === 'down' ? 2 : rollup === 'warn' ? 1 : 0;
}

function rigStoreMeta(report: RigStoreHealthReport | null): string | undefined {
  if (report === null || report.rigs.length === 0) return undefined;
  const counts = { ok: 0, warn: 0, down: 0 };
  for (const rig of report.rigs) counts[rig.rollup] += 1;
  return `${counts.ok} ok · ${counts.warn} warn · ${counts.down} down`;
}

function rigStoreSummaryStatus(
  report: RigStoreHealthReport,
): { tone: StatusTone; label: string } | undefined {
  if (report.rigs.some((r) => r.rollup === 'down')) return { tone: 'stuck', label: 'attention' };
  if (report.rigs.some((r) => r.rollup === 'warn')) return { tone: 'warn', label: 'warnings' };
  if (report.rigs.length > 0) return { tone: 'ok', label: 'healthy' };
  return undefined;
}

function rigStoreUnavailableCopy(reason: RigStoreHealthUnavailableReason): string {
  switch (reason) {
    case 'not_sampled_yet':
      return 'backend just started; first sample is in flight';
    case 'rig_list_failed':
      return 'the supervisor rig list could not be read';
    case 'fetch_failed':
      return 'the dashboard backend could not be reached';
  }
}

function ConfigComparison({ comparison }: { comparison: DiagnosticDatum<ConfigComparisonRow[]> }) {
  if (comparison.status === 'unavailable') {
    return (
      <p className="text-body text-fg-muted italic">Comparison unavailable: {comparison.reason}.</p>
    );
  }
  return (
    <div className="space-y-2">
      {comparison.stale !== undefined && <StaleNote message={comparison.stale} />}
      <div className="grid grid-cols-[1fr_max-content_max-content] gap-x-8 gap-y-3 max-w-prose">
        <div className="text-label uppercase tracking-wider text-fg-muted">Setting</div>
        <div className="text-label uppercase tracking-wider text-fg-muted text-right">
          Recommended
        </div>
        <div className="text-label uppercase tracking-wider text-fg-muted text-right">Loaded</div>
        {comparison.value.map((row) => (
          <ComparisonRow key={row.label} row={row} />
        ))}
      </div>
    </div>
  );
}

function ComparisonRow({ row }: { row: ConfigComparisonRow }) {
  const tone = row.withinRecommendation ? 'text-fg' : 'text-warn';
  return (
    <div className={`contents ${tone}`} data-comparison-row={row.label}>
      <div className={`text-body ${tone}`}>
        {row.label}
        {!row.withinRecommendation && (
          <span className="text-label uppercase tracking-wider text-warn"> · over</span>
        )}
      </div>
      <div className="text-body tnum text-fg-muted text-right">{row.recommended}</div>
      <div className={`text-body tnum font-medium text-right ${tone}`}>{row.loaded}</div>
    </div>
  );
}

function UnavailableNote({ heading, reason }: { heading: string; reason: string }) {
  return (
    <div className="space-y-2">
      <h3 className="text-label uppercase tracking-wider text-fg-muted">{heading}</h3>
      <p className="text-body text-fg-muted italic">Unavailable: {reason}.</p>
    </div>
  );
}

// Warn-toned "showing last sample" line for cached diagnostics whose latest read
// failed, matching the rig-store-health stale affordance.
function StaleNote({ message }: { message: string }) {
  return <p className="text-body text-warn italic">{message}</p>;
}

function Sparkline({ samples }: { samples: { ts: string; bytes: number }[] }) {
  if (samples.length === 0) return null;
  const max = Math.max(...samples.map((s) => s.bytes));
  const min = Math.min(...samples.map((s) => s.bytes));
  const range = max - min || 1;
  const width = 600;
  const height = 60;
  const stepX = samples.length > 1 ? width / (samples.length - 1) : width;
  const points = samples
    .map((s, i) => {
      const x = i * stepX;
      const y = height - ((s.bytes - min) / range) * height;
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    })
    .join(' ');
  return (
    <div className="space-y-3 max-w-prose">
      <svg
        viewBox={`0 0 ${width} ${height}`}
        preserveAspectRatio="none"
        className="w-full h-16"
        aria-label="24 hour dolt-noms size trend"
      >
        <polyline
          fill="none"
          stroke="currentColor"
          strokeWidth="1"
          className="text-accent"
          points={points}
        />
      </svg>
      <div className="flex items-baseline justify-between text-label uppercase tracking-wider text-fg-muted tnum">
        <span>min {formatHumanSize(min)}</span>
        <span>max {formatHumanSize(max)}</span>
      </div>
    </div>
  );
}

function doltUnavailableCopy(
  reason: Extract<DoltNomsTrend, { available: false }>['reason'],
): string {
  switch (reason) {
    case 'store_health_absent':
      return 'supervisor is not reporting store_health; samples resume when it recovers';
    case 'sample_failed':
      return 'latest supervisor status read failed; check the backend log';
  }
}

type SupervisorHealthState =
  | { status: 'available'; data: HealthOutputBody }
  | { status: 'unavailable'; error: string };

type SystemHealthState =
  | { status: 'available'; data: SystemHealth }
  | { status: 'unavailable'; error: string };

type SupervisorStatusState =
  // `staleReason` is set when the data is the last good snapshot served after a
  // later sample failed (degraded, not blank) — drives the "showing last sample"
  // marker on the diagnostics widgets. null means the data is fresh.
  | {
      status: 'available';
      data: StatusBody;
      staleReason: Extract<SupervisorStatusReport, { available: false }>['reason'] | null;
    }
  | { status: 'unavailable'; error: string };

type LocalToolVersionsState =
  | { status: 'available'; data: LocalToolVersions }
  | { status: 'unavailable'; error: string };

async function fetchSystemHealth(): Promise<SystemHealthState> {
  try {
    return {
      status: 'available',
      data: await api.systemHealth(),
    };
  } catch (err) {
    return {
      status: 'unavailable',
      error: formatApiError(err, 'dashboard host health unavailable'),
    };
  }
}

async function fetchSupervisorHealth(): Promise<SupervisorHealthState> {
  const cityName = getActiveCity();
  if (cityName === null) {
    throw new Error('Health page loaded before an active city was resolved');
  }
  try {
    return {
      status: 'available',
      data: await supervisorApiForRequestBudget(HEALTH_SUPERVISOR_REQUEST_TIMEOUT_MS).cityHealth(
        cityName,
      ),
    };
  } catch {
    return {
      status: 'unavailable',
      error: 'supervisor health unavailable',
    };
  }
}

function supervisorStatusUnavailableCopy(
  reason: Extract<SupervisorStatusReport, { available: false }>['reason'],
): string {
  switch (reason) {
    case 'not_sampled_yet':
      return 'supervisor status sample is warming up; data appears after the next backend sample';
    case 'status_read_failed':
      return 'latest supervisor status read failed; check the backend log';
  }
}

// Stale marker shown above cached diagnostics, mirroring the rig-store-health
// "Showing the last sample" affordance so a degraded snapshot is never mistaken
// for a fresh read.
function supervisorStatusStaleCopy(
  reason: Extract<SupervisorStatusReport, { available: false }>['reason'],
): string {
  return `Showing the last sample; refresh failed: ${supervisorStatusUnavailableCopy(reason)}.`;
}

// gascity-dashboard-4bol: read the dashboard backend's cached /status snapshot
// (sampled on the background ceiling) instead of racing the slow supervisor on a
// short interactive budget — live /status runs 10–38s (gastownhall/gascity-dashboard#88).
// A degraded report still carries the last good status, so the store-thresholds /
// dolt-usage / beads-usage widgets show real (cached) data with a "stale" marker
// rather than "supervisor status unavailable".
async function fetchSupervisorStatus(): Promise<SupervisorStatusState> {
  try {
    const report = await api.supervisorStatus();
    if (report.available) {
      return { status: 'available', data: report.status, staleReason: null };
    }
    if (report.status !== null) {
      return { status: 'available', data: report.status, staleReason: report.reason };
    }
    return {
      status: 'unavailable',
      error: supervisorStatusUnavailableCopy(report.reason),
    };
  } catch (err) {
    return {
      status: 'unavailable',
      error: formatApiError(err, 'supervisor status unavailable'),
    };
  }
}

async function fetchLocalToolVersions(): Promise<LocalToolVersionsState> {
  try {
    return {
      status: 'available',
      data: await api.localToolVersions(),
    };
  } catch {
    return {
      status: 'unavailable',
      error: 'local tool versions unavailable',
    };
  }
}

async function fetchDoltNomsTrend(): Promise<DoltNomsTrend> {
  try {
    return await api.doltTrend();
  } catch {
    return {
      available: false,
      reason: 'sample_failed',
      samples: [],
    };
  }
}

async function fetchRigStoreHealth(): Promise<RigStoreHealthReport> {
  try {
    return await api.rigStoreHealth();
  } catch {
    // Transport-level failure (backend unreachable / 5xx / decode) — a
    // distinct root cause from the backend's own 'rig_list_failed', so it must
    // not borrow that reason and point the operator at the supervisor.
    return { available: false, reason: 'fetch_failed', rigs: [] };
  }
}

function buildSynopsis(
  h: SystemHealth | null,
  supervisorState: SupervisorHealthState | null,
): string {
  const parts: string[] = [];
  if (supervisorState === null) {
    parts.push('Supervisor state still loading.');
  } else if (supervisorState.status === 'available') {
    const supervisor = supervisorState.data;
    const verb = supervisor.status === 'ok' ? 'healthy' : supervisor.status;
    // izgc F7/F8: city is optional per OpenAPI. Skip the locator clause if
    // absent rather than rendering "Supervisor healthy on undefined" — the
    // adjacent Kv block surfaces the absence with a warn tone.
    if (supervisor.city !== undefined) {
      parts.push(
        `Supervisor ${verb} on ${supervisor.city}, uptime ${formatDuration(supervisor.uptime_sec)}.`,
      );
    } else {
      parts.push(`Supervisor ${verb}, uptime ${formatDuration(supervisor.uptime_sec)}.`);
    }
  } else {
    parts.push('Supervisor unreachable.');
  }
  if (h === null) {
    parts.push('Host health unavailable.');
    return parts.join(' ');
  }
  const usedPct = Math.round(100 * (1 - h.host.free_mem_bytes / h.host.total_mem_bytes));
  parts.push(
    `Memory at ${usedPct}%; ${h.host.cpu_count} CPUs averaging ${h.host.load_avg_1.toFixed(2)} load.`,
  );
  return parts.join(' ');
}

function supervisorStatus(supervisorState: SupervisorHealthState): {
  tone: StatusTone;
  label: string;
} {
  if (supervisorState.status === 'unavailable') return { tone: 'stuck', label: 'offline' };
  if (supervisorState.data.status === 'ok') return { tone: 'ok', label: 'healthy' };
  return { tone: 'warn', label: supervisorState.data.status };
}

function hostStatus(h: SystemHealth): { tone: StatusTone; label: string } | undefined {
  const memPct = h.host.free_mem_bytes / h.host.total_mem_bytes;
  if (memPct < 0.05) return { tone: 'stuck', label: 'memory critical' };
  if (memPct < 0.1) return { tone: 'warn', label: 'memory low' };
  if (h.host.load_avg_1 > h.host.cpu_count * 1.5) return { tone: 'warn', label: 'load high' };
  return undefined;
}

function doltUsageOf(
  statusState: SupervisorStatusState | null,
): DiagnosticDatum<StatusStoreHealth> {
  if (statusState === null) {
    return { status: 'unavailable', reason: 'supervisor status still loading' };
  }
  if (statusState.status === 'unavailable') {
    return { status: 'unavailable', reason: statusState.error };
  }
  const storeHealth = statusState.data.store_health;
  if (storeHealth === undefined) {
    return {
      status: 'unavailable',
      reason: 'supervisor did not report store_health',
    };
  }
  return {
    status: 'available',
    value: storeHealth,
    source: 'supervisor status.store_health',
    ...(statusState.staleReason !== null
      ? { stale: supervisorStatusStaleCopy(statusState.staleReason) }
      : {}),
  };
}

function beadsUsageOf(
  statusState: SupervisorStatusState | null,
): DiagnosticDatum<StatusWorkCounts> {
  if (statusState === null) {
    return { status: 'unavailable', reason: 'supervisor status still loading' };
  }
  if (statusState.status === 'unavailable') {
    return { status: 'unavailable', reason: statusState.error };
  }
  return {
    status: 'available',
    value: statusState.data.work,
    source: 'supervisor status.work',
    ...(statusState.staleReason !== null
      ? { stale: supervisorStatusStaleCopy(statusState.staleReason) }
      : {}),
  };
}

function configComparisonOf(
  statusState: SupervisorStatusState | null,
): DiagnosticDatum<ConfigComparisonRow[]> {
  const usage = doltUsageOf(statusState);
  if (usage.status === 'unavailable') {
    return { status: 'unavailable', reason: usage.reason };
  }
  const storeHealth = usage.value;
  return {
    status: 'available',
    source: 'supervisor status.store_health (threshold vs actual)',
    ...(usage.stale !== undefined ? { stale: usage.stale } : {}),
    value: [
      {
        label: 'Dolt MB-per-row ratio',
        recommended: `<= ${storeHealth.threshold_mb_per_row}`,
        loaded: String(storeHealth.ratio_mb_per_row),
        withinRecommendation: !storeHealth.warning,
      },
    ],
  };
}

function statusNumber(value: number | bigint): number {
  return typeof value === 'bigint' ? Number(value) : value;
}

function formatDuration(sec: number): string {
  if (sec < 60) return `${sec}s`;
  if (sec < 3600) return `${Math.round(sec / 60)}m`;
  if (sec < 86_400) return `${Math.round(sec / 3600)}h`;
  const days = Math.floor(sec / 86_400);
  const hours = Math.round((sec % 86_400) / 3600);
  return hours > 0 ? `${days}d ${hours}h` : `${days}d`;
}

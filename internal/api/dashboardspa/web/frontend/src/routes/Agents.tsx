import { useCallback, useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import {
  GC_EVENT_PREFIX,
  effectiveContextPct,
  selectAgentsNeedingYou,
  type AgentNeedsYou,
} from 'gas-city-dashboard-shared';
import { Button } from '../components/Button';
import { useAttentionModel } from '../attention/context';
import {
  agentNeedsYouActionLabel,
  agentNeedsYouReasonLabel,
  agentNeedsYouReasonTone,
} from '../attention/agentNeedsYou';
import { attentionDataProps, resourceAttentionSeverity } from '../attention/routeHighlight';
import { ListSearchBar } from '../components/ListSearchBar';
import { Modal } from '../components/Modal';
import { PageHeader } from '../components/PageHeader';
import { PartialDataNotice } from '../components/PartialDataNotice';
import { WorkInFlight } from '../components/WorkInFlight';
import { LiveSessionPeek, isAgentStreamable } from '../components/LiveSessionPeek';
import { SseIndicator } from '../components/SseIndicator';
import { StatusBadge, stateTone } from '../components/StatusBadge';
import { Table, type TableColumn } from '../components/Table';
import { useNow } from '../contexts/NowContext';
import { READ_ONLY_CONTROL_TITLE, ReadOnlyBadge, useReadOnly } from '../contexts/ReadOnlyContext';
import { useCachedData } from '../hooks/useCachedData';
import { useGcEventRefresh } from '../hooks/useGcEvents';
import { formatRelative } from '../hooks/time';
import {
  attachCommand,
  listAgentPendingInteractions,
  respondToAgentPendingInteraction,
  type AgentPendingInteraction,
} from '../supervisor/agentPending';
import { listSupervisorSessions } from '../supervisor/sessionReads';
import { listSupervisorBeads } from '../supervisor/beadReads';
import { listSupervisorAgents, type SupervisorAgent } from '../supervisor/agentReads';
import {
  agentProject,
  cleanWorkerName,
  isAgentOutsideRig,
  isPerRigDispatcherAgent,
} from '../hooks/projectOf';
import { agentSlug } from '../hooks/sessionSlug';

// gascity-dashboard-ay6: the Agents view consumes the supervisor's
// first-class /v0/city/{name}/agents roster. The previous implementation
// derived agents from the dashboard sessions mirror, which undercounted
// any agent that wasn't currently running a session
// (orphan / configured-but-asleep agents simply didn't appear).
//
// gascity-dashboard-fgzf: reverted from the flat sortable/filterable
// table (chips + sort + rig dropdown + useListFilters) to the older,
// simpler view — a single 'running' toggle (default on) over a plain
// list, with the 'rig · agent' label restored. No sort control, no
// persisted chip state.

// An agent is "actively running" when it is not suspended and the
// supervisor reports it as alive (state active/running, or running flag
// set on a detached-but-live process). The default view shows only these.
export function isRunningAgent(a: SupervisorAgent): boolean {
  return !a.suspended && (a.state === 'active' || a.state === 'running' || a.running === true);
}

// Whether an agent is visible while the 'running' toggle is on. Running agents
// always show; a non-running agent shows ONLY if it has an urgent ('attention')
// item (e.g. blocked on a pending interaction) so a genuinely stuck agent is
// never hidden. A passive 'watch' item (suspended / asleep / idle) does NOT
// keep a non-running agent visible — keying on any-non-null severity is what
// let a suspended=true agent leak through the 'running' filter.
export function isVisibleUnderRunning(
  a: SupervisorAgent,
  severity: 'attention' | 'watch' | null,
): boolean {
  return isRunningAgent(a) || severity === 'attention';
}

// Display label for the row: 'rig · agent' (e.g. 'gascity-packs · polecat-1').
// Agents outside a rig (cross-rig orchestration or the residual no-rig
// bucket) carry no rig prefix — just the (cleaned) alias. The alias is run
// through cleanWorkerName so a path-leaking or session-suffixed name
// (e.g. `/home/ds/gas-city/city-infra-polecat`, `polecat-gc-335825`) renders
// as a clean role, never the raw path or `-gc-XXXXX` suffix.
export function agentRowLabel(a: SupervisorAgent): string {
  const name = cleanWorkerName(a.name);
  if (isAgentOutsideRig(a)) return name;
  return `${agentProject(a).label} · ${name}`;
}

const AGENT_SEARCH_FIELDS = (a: SupervisorAgent): ReadonlyArray<string> =>
  [a.name, a.display_name, a.pool, a.rig, a.provider, a.model].filter(
    (field): field is string => typeof field === 'string' && field.length > 0,
  );

export function AgentsPage() {
  const attention = useAttentionModel();
  const { data, loading, error, refresh } = useCachedData('agents', listSupervisorAgents);
  // The supervisor's AgentResponse.session (SessionInfo) carries only
  // `name`/`attached`/`last_activity` — NOT the session id. Peek needs
  // the session id (gc-XXX format) per SESSION_ID_RE on the backend.
  // Fetch the sessions list in parallel so we can map agent.session.name
  // -> session.id at peek time.
  const sessionsCache = useCachedData('sessions', listSupervisorSessions);
  // The "Workers active" section is SESSION-driven: it counts the live worker
  // sessions (stable across the bead churn), grouped by rig. Beads are fetched
  // as best-effort secondary context only — when an in-progress bead's assignee
  // embeds a worker session's id, the section appends it to that worker's row.
  // The beads churn to zero within seconds and aren't reliably aggregated, so a
  // worker with no captured bead is the common (and still-valid) case.
  const beadsCache = useCachedData('beads:in-flight', () => listSupervisorBeads());
  const rows = useMemo<SupervisorAgent[]>(() => data?.items ?? [], [data]);
  const sessionIds = useMemo(
    () => (sessionsCache.data?.items ?? []).map((session) => session.id).sort(),
    [sessionsCache.data],
  );
  const agentNames = useMemo(() => rows.map((agent) => agent.name).sort(), [rows]);
  const pendingCache = useCachedData(
    `agent-pending:${agentNames.join(',')}:${sessionIds.join(',')}`,
    () => listAgentPendingInteractions(rows, sessionsCache.data?.items ?? []),
  );
  const sessionsById = useMemo(() => {
    const map = new Map<string, string>();
    for (const s of sessionsCache.data?.items ?? []) {
      if (s.session_name) map.set(s.session_name, s.id);
    }
    return map;
  }, [sessionsCache.data]);
  const pendingByAgent = useMemo(() => {
    const map = new Map<string, AgentPendingInteraction>();
    for (const pending of pendingCache.data ?? []) {
      map.set(pending.agentName, pending);
    }
    return map;
  }, [pendingCache.data]);
  // gascity-dashboard-2j8e.4: the same selectAgentsNeedingYou the nav badge
  // counts, so the "Needs you" section header and the badge number are one
  // number (count parity). The detail rows pair each agent with the cleaned
  // 'rig · agent' label and a drilldown link (the path to address it).
  const needsYouRows = useMemo(() => {
    const pending = (pendingCache.data ?? []).map((p) => ({
      agentName: p.agentName,
      ...(p.pending.prompt === undefined ? {} : { prompt: p.pending.prompt }),
    }));
    const byName = new Map(rows.map((a) => [a.name, a]));
    return selectAgentsNeedingYou(rows, pending).flatMap((need) => {
      const agent = byName.get(need.name);
      // need.name came from `rows`, so the lookup always resolves; flatMap with
      // an empty fallback narrows the type without inventing a label.
      if (agent === undefined) return [];
      return [{ need, label: agentRowLabel(agent), slug: agentSlug(agent) }];
    });
  }, [rows, pendingCache.data]);
  const now = useNow();

  // Default to the actively-running view (restores the older simple view).
  // Ephemeral: not persisted, resets between visits.
  const [runningOnly, setRunningOnly] = useState(true);
  const [search, setSearch] = useState('');
  const [rigFilter, setRigFilter] = useState('');

  // Peek key is the agent alias (`name`); modal resolves the live session
  // by mapping agent.session.name -> session.id via the sessions cache.
  const [peekAlias, setPeekAlias] = useState<string | null>(null);
  const [responseMessage, setResponseMessage] = useState<string | null>(null);
  const [responseError, setResponseError] = useState<string | null>(null);
  const [responding, setResponding] = useState<{
    sessionId: string;
    action: string;
  } | null>(null);
  const peekAgent = useMemo(
    () => (peekAlias === null ? null : (rows.find((a) => a.name === peekAlias) ?? null)),
    [rows, peekAlias],
  );
  const peekSessionId = useMemo(() => {
    const sessionName = peekAgent?.session?.name;
    if (!sessionName) return null;
    return sessionsById.get(sessionName) ?? null;
  }, [peekAgent, sessionsById]);

  const sseState = useGcEventRefresh(
    [GC_EVENT_PREFIX.session, GC_EVENT_PREFIX.bead, 'agent.'],
    () => {
      void refresh();
      // The Work-in-flight section reads beads + sessions; keep both fresh on a
      // bead/session transition so the in-flight rows track the live work.
      void beadsCache.refresh();
      void sessionsCache.refresh();
    },
  );

  const synopsis = useMemo(() => buildAgentSynopsis(rows), [rows]);
  const readOnly = useReadOnly();
  const handlePendingResponse = useCallback(
    async (pending: AgentPendingInteraction, action: 'approve' | 'deny') => {
      // Defense-in-depth: the disabled buttons already block this, but a
      // keyboard/programmatic path must never reach a write the server 405s.
      if (readOnly) return;
      setResponding({ sessionId: pending.sessionId, action });
      setResponseMessage(null);
      setResponseError(null);
      try {
        await respondToAgentPendingInteraction(pending.sessionId, {
          action,
          request_id: pending.pending.request_id,
        });
        setResponseMessage(`responded to ${pending.agentName}`);
        await pendingCache.refresh();
      } catch (err) {
        setResponseError(err instanceof Error ? err.message : 'response failed');
      } finally {
        setResponding(null);
      }
    },
    [pendingCache, readOnly],
  );

  // Rig dropdown options: the normalized rig labels (basenames, not raw
  // paths) present in the current roster, sorted. Empty value = all rigs.
  // Outside-rig agents (cross-rig Orchestration, builtin Maintenance pools like
  // `dog`, and the residual no-rig bucket) are excluded — their bucket labels
  // are not rigs and must not appear as rig filter options (gascity-dashboard-jj2z).
  const rigOptions = useMemo(
    () =>
      Array.from(
        new Set(rows.filter((a) => !isAgentOutsideRig(a)).map((a) => agentProject(a).label)),
      ).sort((x, y) => x.localeCompare(y)),
    [rows],
  );
  // If the selected rig leaves the roster, fall back to all rigs.
  useEffect(() => {
    if (rigFilter !== '' && !rigOptions.includes(rigFilter)) setRigFilter('');
  }, [rigOptions, rigFilter]);

  // Simple client-side filtering: rig dropdown, the 'running' toggle, then a
  // free-text search. No sort mode, no chip group, no persisted state.
  const visibleRows = useMemo(() => {
    const q = search.trim().toLowerCase();
    return rows.filter((a) => {
      if (rigFilter !== '' && agentProject(a).label !== rigFilter) return false;
      // Only 'attention' severity (e.g. blocked on a pending interaction)
      // bypasses the 'running' filter, never a passive 'watch' (suspended /
      // asleep / idle). Keying on any-non-null severity is what let a
      // suspended=true agent leak through the toggle. See isVisibleUnderRunning.
      const severity = resourceAttentionSeverity(attention, 'agents', a.name);
      if (runningOnly && !isVisibleUnderRunning(a, severity)) return false;
      if (q.length === 0) return true;
      return AGENT_SEARCH_FIELDS(a).some((field) => field.toLowerCase().includes(q));
    });
  }, [rows, rigFilter, runningOnly, search, attention]);

  const rowProps = useMemo(
    () => (agent: SupervisorAgent) =>
      attentionDataProps(resourceAttentionSeverity(attention, 'agents', agent.name)),
    [attention],
  );
  const rosterUnavailable = error !== null && rows.length === 0;
  const emptyMessage = rosterUnavailable
    ? 'Agent roster unavailable.'
    : rows.length === 0
      ? 'No agents configured.'
      : 'No agents match the current search or filter.';

  const columns = useMemo<ReadonlyArray<TableColumn<SupervisorAgent>>>(
    () => [
      {
        key: 'name',
        label: 'Agent',
        sortable: true,
        // Sort by the visible 'rig · agent' label so the column order matches.
        sortValue: (r) => agentRowLabel(r),
        render: (r) => {
          // Per-rig dispatchers (alias '<rig>/control-dispatcher') perform an
          // orchestration role; italicize the label so the operator can spot
          // them at a glance.
          const dispatcher = isPerRigDispatcherAgent(r);
          // Primary label is 'rig · agent' (e.g. 'gascity-packs · polecat-1').
          // display_name is the provider's human-readable label
          // (e.g. "Claude (Account 5)") and reads as muted secondary context.
          const secondary =
            r.display_name && r.display_name !== r.name
              ? r.display_name
              : (r.provider ?? r.model ?? '');
          // ay6.2: orphan agents (no bound session) still render a link, but
          // AgentDetail resolves nothing — a distinct title tooltip and muted
          // color pre-empt the dead-end without disabling the link.
          const orphan = !r.session;
          const linkTitle = orphan
            ? `${r.name} — configured but not running; detail will show no live session`
            : `Open drilldown for ${r.name}`;
          const linkColor = orphan ? 'text-fg-muted' : 'text-fg';
          return (
            <div className="min-w-0">
              <Link
                to={`/agents/${encodeURIComponent(agentSlug(r))}`}
                className={`block ${linkColor} truncate hover:text-accent focus-mark ${
                  dispatcher ? 'font-normal italic' : 'font-medium'
                }`}
                title={linkTitle}
              >
                {agentRowLabel(r)}
              </Link>
              {secondary && (
                <div className="text-label uppercase tracking-wider text-fg-faint mt-1 truncate">
                  {secondary}
                </div>
              )}
            </div>
          );
        },
      },
      {
        key: 'state',
        label: 'State',
        sortable: true,
        sortValue: (r) => r.state,
        render: (r) => (
          <StatusBadge
            tone={stateTone(r.state)}
            label={r.state}
            {...(r.session?.attached ? { trailing: 'att' } : {})}
            {...(r.unavailable_reason ? { title: `unavailable: ${r.unavailable_reason}` } : {})}
          />
        ),
        className: 'w-32',
      },
      {
        key: 'activity',
        label: 'Activity',
        sortable: true,
        sortValue: (r) => r.activity ?? '',
        render: (r) => {
          const pending = pendingByAgent.get(r.name);
          if (pending !== undefined) {
            return (
              <div className="min-w-0">
                <StatusBadge tone="stuck" label="needs you" />
                <p className="mt-1 truncate text-fg-muted" title={pending.pending.prompt}>
                  {pending.pending.prompt ?? pending.pending.kind}
                </p>
              </div>
            );
          }
          return (
            <span className="text-fg-muted">{r.activity ?? (r.running ? 'running' : '·')}</span>
          );
        },
        className: 'w-28',
      },
      {
        key: 'context',
        label: 'Context',
        sortable: true,
        // Sort by the value we DISPLAY, not the raw gc value — otherwise
        // mayor (raw 89%, effective 18%) would sort above a true-90%
        // agent that looks calmer.
        sortValue: (r) => effectiveContextPct(r) ?? -1,
        align: 'right',
        render: (r) => {
          const pct = effectiveContextPct(r);
          if (typeof pct !== 'number') {
            return <span className="text-fg-faint">·</span>;
          }
          // Tooltip exposes the raw gc value when scaling kicked in so
          // the operator can audit the dashboard against gc directly.
          const title =
            typeof r.context_pct === 'number' && r.context_pct !== pct
              ? `gc reports ${r.context_pct}% against ${r.context_window ?? '?'}-token window; scaled to model's true window`
              : undefined;
          return (
            <span
              title={title}
              className={`tnum ${
                pct >= 95
                  ? 'text-accent font-medium'
                  : pct >= 80
                    ? 'text-warn font-medium'
                    : 'text-fg-muted'
              }`}
            >
              {pct}%
            </span>
          );
        },
        className: 'w-24',
      },
      {
        key: 'last_active',
        label: 'Last active',
        sortable: true,
        sortValue: (r) => r.session?.last_activity ?? '',
        render: (r) => {
          const ts = r.session?.last_activity;
          if (!ts) return <span className="text-fg-faint tnum">·</span>;
          return <span className="tnum text-fg-muted">{formatRelative(ts, now)}</span>;
        },
        className: 'w-32',
      },
      {
        key: 'actions',
        label: '',
        render: (r) => {
          // Orphan agents (no bound session) have nothing to peek into.
          // Render an empty cell rather than collapsing the column width.
          if (!r.session) return null;
          const pending = pendingByAgent.get(r.name);
          return (
            <div className="flex justify-end gap-2">
              {pending !== undefined && (
                <>
                  {readOnly && <ReadOnlyBadge />}
                  <Button
                    size="sm"
                    tone="quiet"
                    title={readOnly ? READ_ONLY_CONTROL_TITLE : undefined}
                    disabled={readOnly || responding?.sessionId === pending.sessionId}
                    onClick={() => void handlePendingResponse(pending, 'approve')}
                  >
                    {responding?.sessionId === pending.sessionId && responding.action === 'approve'
                      ? 'Approving'
                      : 'Approve'}
                  </Button>
                  <Button
                    size="sm"
                    tone="quiet"
                    title={readOnly ? READ_ONLY_CONTROL_TITLE : undefined}
                    disabled={readOnly || responding?.sessionId === pending.sessionId}
                    onClick={() => void handlePendingResponse(pending, 'deny')}
                  >
                    {responding?.sessionId === pending.sessionId && responding.action === 'deny'
                      ? 'Denying'
                      : 'Deny'}
                  </Button>
                  <CopyAttachButton command={attachCommand(r.name)} />
                </>
              )}
              <Button size="sm" tone="quiet" onClick={() => setPeekAlias(r.name)}>
                Peek
              </Button>
            </div>
          );
        },
        align: 'right',
        className: 'w-80',
      },
    ],
    [handlePendingResponse, now, pendingByAgent, readOnly, responding],
  );

  return (
    <section>
      <PageHeader
        title="Agents"
        synopsis={rosterUnavailable ? 'Agent roster unavailable.' : synopsis}
        meta={
          <>
            <SseIndicator state={sseState} />
            {error && (
              <span className="normal-case text-body text-accent" role="alert">
                {error}
              </span>
            )}
            <PartialDataNotice
              show={data?.partial === true}
              label="roster partial"
              title={data?.partial_errors?.join('\n') ?? 'one or more agent backends unavailable'}
            />
            <Button size="sm" onClick={() => void refresh()} disabled={loading}>
              {loading ? 'Refreshing' : 'Refresh'}
            </Button>
          </>
        }
      />

      <NeedsYouSection rows={needsYouRows} />

      <WorkInFlight
        beads={beadsCache.data?.items ?? []}
        sessions={sessionsCache.data?.items ?? []}
        sessionsLoading={sessionsCache.loading}
        sessionsError={sessionsCache.error}
      />

      {/* The bottom roster is the "available agents" view — the mayor, PLs, and
          dispatchers that are running-but-idle, distinct from the working
          sessions above. A calm section header (matching the "Workers active"
          style) keeps the two groups from bleeding into one. */}
      <header className="flex items-baseline justify-between border-b border-rule pb-2 mb-4">
        <h2 className="text-headline text-fg">Available agents</h2>
        <span className="text-label tnum text-fg-muted">{rows.length}</span>
      </header>

      <div className="mb-6 space-y-3">
        <ListSearchBar
          value={search}
          onChange={setSearch}
          placeholder="Search agents by alias, rig, pool, provider"
          matchCount={visibleRows.length}
          totalCount={rows.length}
          ariaLabel="Search agents"
        />
        <div className="flex items-baseline gap-6">
          <label className="inline-flex items-baseline gap-2 text-label uppercase tracking-wider text-fg-muted cursor-pointer hover:text-fg transition-colors duration-150 ease-out-quart">
            <input
              type="checkbox"
              checked={runningOnly}
              onChange={(e) => setRunningOnly(e.target.checked)}
              // Neutral accent, not maroon: the adjacent "running" word already
              // names the state (greyscale-safe), and a maroon check would be a
              // second mark per DESIGN.md One Mark Rule.
              style={{ accentColor: 'oklch(var(--fg-muted))' }}
              className="translate-y-[2px]"
            />
            <span>running</span>
          </label>
          {rigOptions.length > 1 && (
            <label className="inline-flex items-baseline gap-2 text-label uppercase tracking-wider text-fg-muted">
              <span>rig</span>
              <select
                value={rigFilter}
                onChange={(e) => setRigFilter(e.target.value)}
                aria-label="Rig filter"
                className="text-label uppercase tracking-wider text-fg-muted bg-transparent border-0 focus-mark cursor-pointer hover:text-fg transition-colors duration-150 ease-out-quart"
              >
                <option value="">all rigs</option>
                {rigOptions.map((rig) => (
                  <option key={rig} value={rig}>
                    {rig}
                  </option>
                ))}
              </select>
            </label>
          )}
        </div>
      </div>
      {responseMessage && (
        <div className="mb-4 text-body text-fg-muted" role="status">
          {responseMessage}
        </div>
      )}
      {responseError && (
        <div className="mb-4 text-body text-accent" role="alert">
          {responseError}
        </div>
      )}

      <Table
        rows={visibleRows}
        columns={columns}
        rowKey={(r) => r.name}
        rowProps={rowProps}
        empty={emptyMessage}
        initialSort={{ key: 'last_active', dir: 'desc' }}
      />

      <Modal
        open={peekAlias !== null}
        onClose={() => setPeekAlias(null)}
        title={peekAgent?.name ?? peekAlias ?? 'Transcript'}
        caption={
          // SessionInfo on the supervisor side carries only name/attached/
          // last_activity — no session id — so we resolve agent.session.name
          // -> session.id through the sessions cache. If sessions hasn't
          // loaded yet (or the agent's session is missing from it), surface
          // that explicitly instead of letting peek hit the route with an
          // invalid id and degrade to "invalid session id".
          peekAgent && peekAgent.session && !peekSessionId
            ? sessionsCache.loading
              ? 'Resolving session…'
              : `No live session matches "${peekAgent.session.name}".`
            : isAgentStreamable(peekAgent)
              ? "Live transcript from the supervisor's session stream."
              : "Snapshot from the supervisor's transcript API."
        }
        widthClass="max-w-5xl"
      >
        <LiveSessionPeek
          sessionId={peekSessionId}
          stream={isAgentStreamable(peekAgent)}
          showBadge
          showCaption
        />
      </Modal>
    </section>
  );
}

interface NeedsYouRow {
  need: AgentNeedsYou;
  label: string;
  slug: string;
}

// gascity-dashboard-2j8e.4: the agents that need the operator, each with the
// reason (glyph + word) and the next step. Mirrors the Runs Blocked section:
// hairline-divided rows, no card chrome, hidden entirely when nothing needs
// you (the calm room does not announce the absence of trouble).
function NeedsYouSection({ rows }: { rows: readonly NeedsYouRow[] }) {
  if (rows.length === 0) return null;
  return (
    <section aria-label="Agents needing you" className="mb-10">
      <h2 className="text-label uppercase tracking-wider text-fg-faint tnum">
        Needs you ({rows.length})
      </h2>
      <ol className="mt-3 divide-y divide-rule">
        {rows.map(({ need, label, slug }) => (
          <li key={need.name} className="py-3">
            <div className="flex items-baseline justify-between gap-4">
              <Link
                to={`/agents/${encodeURIComponent(slug)}`}
                className="focus-mark block min-w-0 truncate text-title text-fg hover:text-accent"
              >
                {label}
              </Link>
              <StatusBadge
                tone={agentNeedsYouReasonTone(need.reason)}
                label={agentNeedsYouReasonLabel(need.reason)}
              />
            </div>
            <p className="mt-1 text-body text-fg leading-snug">{need.detail}</p>
            <p className="mt-0.5 text-body text-fg-muted leading-snug">
              {agentNeedsYouActionLabel(need.action)}
            </p>
          </li>
        ))}
      </ol>
    </section>
  );
}

function CopyAttachButton({ command }: { command: string }) {
  const [state, setState] = useState<'idle' | 'copied' | 'failed'>('idle');
  const label = state === 'copied' ? 'Copied' : state === 'failed' ? 'Copy failed' : 'Copy attach';

  return (
    <Button
      size="sm"
      tone="quiet"
      title={command}
      onClick={() => {
        void copyAttachCommand(command, setState);
      }}
    >
      {label}
    </Button>
  );
}

async function copyAttachCommand(
  command: string,
  setState: (state: 'idle' | 'copied' | 'failed') => void,
): Promise<void> {
  try {
    await navigator.clipboard.writeText(command);
    setState('copied');
  } catch {
    setState('failed');
  }
}

// stateTone moved to components/StatusBadge.tsx (alongside StatusTone /
// beadStatusTone) to break the Agents → WorkInFlight → Agents import cycle.
// Re-exported here so existing importers of `./Agents` keep working.
export { stateTone } from '../components/StatusBadge';

// Buckets a raw state into the synopsis category. Distinct from
// stateTone because 'detached' and 'idle' share a tone (neutral) but the
// header text breaks them out.
export type SynopsisBucket =
  | 'active'
  | 'idle'
  | 'detached'
  | 'rate-limited'
  | 'stuck'
  | 'suspended';

function stateBucket(agent: SupervisorAgent): SynopsisBucket {
  if (agent.suspended) return 'suspended';
  switch (agent.state) {
    case 'active':
    case 'running':
      return 'active';
    case 'detached':
      return 'detached';
    case 'rate-limited':
    case 'rate_limited':
    case 'waiting':
      return 'rate-limited';
    case 'failed':
    case 'closed':
    case 'errored':
    case 'stuck':
      return 'stuck';
    default:
      return 'idle';
  }
}

export function buildAgentSynopsis(rows: ReadonlyArray<SupervisorAgent>): string {
  if (rows.length === 0) return 'No agents configured.';
  const counts = new Map<SynopsisBucket, number>();
  for (const r of rows) {
    const b = stateBucket(r);
    counts.set(b, (counts.get(b) ?? 0) + 1);
  }
  const parts: string[] = [];
  const active = counts.get('active') ?? 0;
  const idle = counts.get('idle') ?? 0;
  const detached = counts.get('detached') ?? 0;
  const rateLimited = counts.get('rate-limited') ?? 0;
  const stuck = counts.get('stuck') ?? 0;
  const suspended = counts.get('suspended') ?? 0;
  if (active > 0) parts.push(`${active} active`);
  if (idle > 0) parts.push(`${idle} idle`);
  if (detached > 0) parts.push(`${detached} detached`);
  if (rateLimited > 0) parts.push(`${rateLimited} rate-limited`);
  if (stuck > 0) parts.push(`${stuck} stuck`);
  if (suspended > 0) parts.push(`${suspended} suspended`);
  return parts.join(', ') + '.';
}

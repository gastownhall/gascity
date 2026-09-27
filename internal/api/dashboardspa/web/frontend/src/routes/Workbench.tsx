import { useCallback, useEffect, useMemo, useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import { GC_EVENT_PREFIX } from 'gas-city-dashboard-shared';
import { getActiveCity } from '../api/cityBase';
import { BeadBody } from '../components/BeadBody';
import { LiveSessionPeek } from '../components/LiveSessionPeek';
import { Button } from '../components/Button';
import { PageHeader } from '../components/PageHeader';
import { StatusBadge, beadStatusTone } from '../components/StatusBadge';
import { useCachedData } from '../hooks/useCachedData';
import { useGcEventRefresh } from '../hooks/useGcEvents';
import { listSupervisorBeads } from '../supervisor/beadReads';
import { startSupervisorAttempt, updateSupervisorBead } from '../supervisor/beadWrites';
import { listSupervisorSessions } from '../supervisor/sessionReads';
import { fetchAttemptDiff } from '../supervisor/attemptReads';
import { sendSupervisorMail } from '../supervisor/mailWrites';
import { useOperatorConfig } from '../contexts/OperatorConfigContext';
import { resolveAttempts, type ExecutionAttempt } from '../lib/workbenchAttempts';
import { resolvePreview } from '../lib/workbenchPreview';

// Gas City Workbench: Canvas/Kanban/Priority/Work Queue as views over the same
// Beads data.
//
// Every view resolves the same Bead identity from one shared read
// (`listSupervisorBeads`) and converges after a successful mutation: lane
// movement patches the Bead through the typed supervisor API and the SSE bead
// event path refreshes the shared read. Kanban lanes move status, Priority
// lanes move priority, and closing requires confirmation. Moving or editing a
// card NEVER starts an agent: no Execution Attempt or Session is created here —
// assignment, Sessions, worktrees and agent lifecycle stay Gas City's job.

const QUEUE_REFRESH_COALESCE_MS = 10_000;

export type WorkbenchView = 'queue' | 'kanban' | 'priority' | 'rigs' | 'attention';

const KANBAN_STATUSES = ['open', 'in_progress', 'blocked', 'hooked', 'closed'] as const;
const PRIORITY_LANES = [0, 1, 2, 3, 4] as const;

type Row = Awaited<ReturnType<typeof listSupervisorBeads>>['items'][number];
type Patch = { status?: string; priority?: number };

export function WorkbenchPage() {
  const cityName = getActiveCity();
  const cityCacheKey = cityName ?? 'no-city';
  const [searchParams, setSearchParams] = useSearchParams();
  const selectedBeadParam = normalizeSelectedBeadParam(searchParams.get('bead'));
  const [selectedId, setSelectedId] = useState<string | null>(selectedBeadParam);
  const [view, setView] = useState<WorkbenchView>('queue');
  const [query, setQuery] = useState('');
  const [statusFilter, setStatusFilter] = useState('all');
  const [order, setOrder] = useState<'priority' | 'title' | 'updated'>('priority');
  // Optimistic overlays keyed by bead id; cleared once the shared read catches up.
  const [overrides, setOverrides] = useState<Record<string, Patch>>({});
  const [mutationError, setMutationError] = useState<string | null>(null);
  const [confirmCloseId, setConfirmCloseId] = useState<string | null>(null);

  const { data, loading, error, refresh } = useCachedData(`workbench:queue:${cityCacheKey}`, () =>
    listSupervisorBeads({ includeClosed: true }),
  );
  const {
    data: sessionsData,
    loading: sessionsLoading,
    error: sessionsError,
  } = useCachedData(`workbench:sessions:${cityCacheKey}`, () => listSupervisorSessions());
  const sessions = useMemo(() => sessionsData?.items ?? [], [sessionsData]);
  const rows = useMemo(() => applyOverrides(data?.items ?? [], overrides), [data, overrides]);
  const rigByBead = useMemo(() => {
    const linked = new Map<string, { rig: string; createdAt: string }>();
    for (const session of sessions) {
      if (session.active_bead && session.rig) {
        const previous = linked.get(session.active_bead);
        if (!previous || session.created_at > previous.createdAt) {
          linked.set(session.active_bead, { rig: session.rig, createdAt: session.created_at });
        }
      }
    }
    return new Map([...linked].map(([id, session]) => [id, session.rig]));
  }, [sessions]);
  const queueRows = useMemo(
    () =>
      rows
        .filter((row) => {
          const matchesQuery = `${row.id} ${row.title}`
            .toLowerCase()
            .includes(query.trim().toLowerCase());
          return matchesQuery && (statusFilter === 'all' || row.status === statusFilter);
        })
        .sort((a, b) => {
          if (order === 'title') return a.title.localeCompare(b.title) || a.id.localeCompare(b.id);
          if (order === 'updated')
            return (
              String(b.updated_at ?? '').localeCompare(String(a.updated_at ?? '')) ||
              a.id.localeCompare(b.id)
            );
          return (a.priority ?? 99) - (b.priority ?? 99) || a.id.localeCompare(b.id);
        }),
    [rows, query, statusFilter, order],
  );
  const attentionRows = useMemo(
    () =>
      queueRows.filter(
        (row) =>
          row.status === 'blocked' ||
          row.is_blocked ||
          (sessionsData !== undefined && resolveAttempts(row, sessions).staleReference),
      ),
    [queueRows, sessions, sessionsData],
  );
  const rigGroups = useMemo(() => {
    const groups = new Map<string, Row[]>();
    for (const row of queueRows) {
      const rig = rigByBead.get(row.id) ?? 'No rig-linked session';
      groups.set(rig, [...(groups.get(rig) ?? []), row]);
    }
    return [...groups.entries()].sort(([left], [right]) =>
      left === 'No rig-linked session'
        ? 1
        : right === 'No rig-linked session'
          ? -1
          : left.localeCompare(right),
    );
  }, [queueRows, rigByBead]);
  const hasLoadedQueue = data !== undefined;

  useGcEventRefresh([GC_EVENT_PREFIX.bead], () => void refresh(), {
    coalesceMs: QUEUE_REFRESH_COALESCE_MS,
  });

  useEffect(() => {
    if (selectedBeadParam !== null) setSelectedId(selectedBeadParam);
  }, [selectedBeadParam]);

  const openBead = (beadId: string) => {
    setSelectedId(beadId);
    setSearchParams(beadId.length > 0 ? { bead: beadId } : {}, { replace: true });
  };
  const closeBead = () => {
    setSelectedId(null);
    setSearchParams({}, { replace: true });
  };

  const mutate = useCallback(
    async (id: string, patch: Patch) => {
      setMutationError(null);
      setOverrides((current) => ({ ...current, [id]: { ...current[id], ...patch } }));
      try {
        await updateSupervisorBead(id, patch);
        await refresh();
        setOverrides((current) => {
          const next = { ...current };
          delete next[id];
          return next;
        });
      } catch (cause) {
        // Server rejection / reconnect: drop the optimistic overlay so the view
        // reconverges on the authoritative read, and surface the failure.
        setOverrides((current) => {
          const next = { ...current };
          delete next[id];
          return next;
        });
        setMutationError(cause instanceof Error ? cause.message : 'Update rejected');
      }
    },
    [refresh],
  );

  const confirmClose = useCallback(async () => {
    const id = confirmCloseId;
    setConfirmCloseId(null);
    if (id === null) return;
    await mutate(id, { status: 'closed' });
  }, [confirmCloseId, mutate]);

  const selectedBead = useMemo(
    () => rows.find((bead) => bead.id === selectedId) ?? null,
    [rows, selectedId],
  );

  const synopsis = hasLoadedQueue
    ? rows.length === 0
      ? 'Nothing on the queue right now.'
      : `${rows.length} ${rows.length === 1 ? 'bead' : 'beads'} across ${viewLabel(view)}.`
    : 'Loading work queue.';

  return (
    <section>
      <PageHeader
        title="Workbench"
        synopsis={synopsis}
        meta={
          <>
            {error && hasLoadedQueue && (
              <span className="normal-case text-body text-accent" role="alert">
                {error}
              </span>
            )}
            {mutationError && (
              <span className="normal-case text-body text-accent" role="alert">
                {mutationError}
              </span>
            )}
            <span className="text-label uppercase tracking-wider text-fg-faint">Beads-backed</span>
            <Button size="sm" onClick={() => void refresh()} disabled={loading}>
              {loading && !hasLoadedQueue ? 'Loading' : loading ? 'Refreshing' : 'Refresh'}
            </Button>
          </>
        }
      />

      <nav aria-label="Workbench views" className="mb-4 flex gap-2">
        {(['queue', 'kanban', 'priority', 'rigs', 'attention'] as const).map((candidate) => (
          <Button
            key={candidate}
            size="sm"
            onClick={() => setView(candidate)}
            aria-pressed={view === candidate}
            tone={view === candidate ? 'accent' : 'quiet'}
          >
            {viewLabel(candidate)}
            {candidate === 'attention' && attentionRows.length > 0
              ? ` · ${attentionRows.length}`
              : ''}
          </Button>
        ))}
      </nav>

      <div className="grid gap-5 xl:grid-cols-[minmax(17rem,0.8fr)_minmax(0,1.2fr)]">
        <div className="min-w-0">
          <div aria-label="Workbench filters" className="mb-3 flex flex-wrap gap-2">
            <input
              type="search"
              aria-label="Search workbench"
              placeholder="Search beads"
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              className="min-w-36 flex-1 rounded-sm border border-rule bg-surface px-2 py-1 text-body text-fg"
            />
            <select
              aria-label="Filter status"
              value={statusFilter}
              onChange={(event) => setStatusFilter(event.target.value)}
              className="rounded-sm border border-rule bg-surface px-2 py-1 text-body text-fg"
            >
              <option value="all">All statuses</option>
              {KANBAN_STATUSES.map((status) => (
                <option key={status} value={status}>
                  {status}
                </option>
              ))}
            </select>
            <select
              aria-label="Order work queue"
              value={order}
              onChange={(event) => setOrder(event.target.value as typeof order)}
              className="rounded-sm border border-rule bg-surface px-2 py-1 text-body text-fg"
            >
              <option value="priority">Priority</option>
              <option value="updated">Recently updated</option>
              <option value="title">Title</option>
            </select>
          </div>
          {!hasLoadedQueue && loading ? (
            <p className="text-body text-fg-muted italic">Loading work queue.</p>
          ) : error && !hasLoadedQueue ? (
            <div className="space-y-3">
              <p className="text-body text-accent" role="alert">
                Work queue unavailable: {error}
              </p>
              <Button size="sm" onClick={() => void refresh()} disabled={loading}>
                Retry
              </Button>
            </div>
          ) : rows.length === 0 ? (
            <p className="text-body text-fg-muted italic">Nothing on the queue right now.</p>
          ) : queueRows.length === 0 ? (
            <p className="text-body text-fg-muted">No beads match these filters.</p>
          ) : view === 'queue' ? (
            <BeadList
              label="Work queue"
              rows={queueRows}
              selectedId={selectedId}
              onOpen={openBead}
            />
          ) : view === 'attention' ? (
            attentionRows.length === 0 ? (
              <p className="text-body text-fg-muted">
                No blocked beads or stale session references match these filters.
              </p>
            ) : (
              <BeadList
                label="Needs attention"
                rows={attentionRows}
                selectedId={selectedId}
                onOpen={openBead}
              />
            )
          ) : view === 'rigs' ? (
            <div className="space-y-5">
              {sessionsError && (
                <p role="alert" className="text-body text-accent">
                  Session ownership unavailable: {sessionsError}
                </p>
              )}
              {sessionsLoading && sessionsData === undefined && (
                <p className="text-body text-fg-muted">Loading rig links…</p>
              )}
              {sessionsData !== undefined &&
                rigGroups.map(([rig, rigRows]) => (
                  <section
                    key={rig}
                    aria-label={rig === 'No rig-linked session' ? rig : `Rig ${rig}`}
                    className="border-t border-rule pt-2"
                  >
                    <div className="mb-2 flex items-baseline justify-between gap-2">
                      <h2 className="text-label font-semibold uppercase tracking-wider text-fg-muted">
                        {rig}
                      </h2>
                      <span className="text-label text-fg-faint">{rigRows.length}</span>
                    </div>
                    <BeadList
                      label={`${rig} beads`}
                      rows={rigRows}
                      selectedId={selectedId}
                      onOpen={openBead}
                    />
                  </section>
                ))}
            </div>
          ) : (
            <LaneBoard
              rows={queueRows}
              view={view}
              selectedId={selectedId}
              onOpen={openBead}
              onMove={mutate}
              onRequestClose={(id) => setConfirmCloseId(id)}
            />
          )}
        </div>
        {selectedId !== null && (
          <section
            aria-label="Selected bead"
            className="min-w-0 rounded-sm border border-rule bg-surface p-4 sm:p-5"
          >
            <div className="mb-4 flex items-center justify-between gap-3">
              <h2 className="text-body font-semibold text-fg">
                <code>{selectedId}</code>
              </h2>
              <Button size="sm" tone="quiet" onClick={closeBead}>
                Clear selection
              </Button>
            </div>
            {selectedBead ? (
              <>
                <BeadBody bead={selectedBead} />
                <AttemptPanel bead={selectedBead} sessions={sessions} />
              </>
            ) : hasLoadedQueue ? (
              <p className="text-body text-fg-muted">This bead was resolved or removed.</p>
            ) : (
              <p className="text-body text-fg-muted">Loading bead.</p>
            )}
          </section>
        )}
      </div>

      {confirmCloseId !== null && (
        <div
          role="alertdialog"
          aria-label="Confirm close"
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/40"
        >
          <div className="rounded-sm border border-rule bg-surface p-5 max-w-sm space-y-3">
            <p className="text-body text-fg">
              Close <code>{confirmCloseId}</code>? Closing changes the Bead status and cannot be
              undone from here.
            </p>
            <div className="flex justify-end gap-2">
              <Button size="sm" tone="quiet" onClick={() => setConfirmCloseId(null)}>
                Cancel
              </Button>
              <Button size="sm" onClick={() => void confirmClose()}>
                Close bead
              </Button>
            </div>
          </div>
        </div>
      )}
    </section>
  );
}

function viewLabel(view: WorkbenchView): string {
  switch (view) {
    case 'queue':
      return 'Work Queue';
    case 'kanban':
      return 'Kanban';
    case 'priority':
      return 'Priority';
    case 'rigs':
      return 'Rigs';
    case 'attention':
      return 'Needs attention';
  }
}

function BeadList({
  label,
  rows,
  selectedId,
  onOpen,
}: {
  label: string;
  rows: Row[];
  selectedId: string | null;
  onOpen: (id: string) => void;
}) {
  return (
    <ul aria-label={label} className="space-y-2">
      {rows.map((bead) => (
        <li key={bead.id}>
          <button
            type="button"
            onClick={() => onOpen(bead.id)}
            aria-pressed={selectedId === bead.id}
            title={`Open ${bead.id}`}
            className="w-full text-left rounded-sm border border-rule px-4 py-3 hover:border-fg-faint focus-mark transition-colors duration-150 ease-out-quart"
          >
            <span className="flex items-baseline justify-between gap-3">
              <span className="text-body text-fg font-medium truncate">{bead.title}</span>
              <StatusBadge tone={beadStatusTone(bead.status)} label={bead.status} />
            </span>
            <span className="text-label uppercase tracking-wider text-fg-faint">
              <code>{bead.id}</code>
              {' · '}
              {bead.issue_type}
            </span>
          </button>
        </li>
      ))}
    </ul>
  );
}

function applyOverrides(rows: Row[], overrides: Record<string, Patch>): Row[] {
  if (Object.keys(overrides).length === 0) return rows;
  return rows.map((row) => {
    const patch = overrides[row.id];
    return patch ? ({ ...row, ...patch } as Row) : row;
  });
}

interface LaneBoardProps {
  rows: Row[];
  view: 'kanban' | 'priority';
  selectedId: string | null;
  onOpen: (id: string) => void;
  onMove: (id: string, patch: Patch) => void | Promise<void>;
  onRequestClose: (id: string) => void;
}

// LaneBoard renders Kanban (lanes = status) or Priority (lanes = priority) over
// the SAME rows the Work Queue shows. Dropping a card on a lane patches exactly
// the one field that view owns: Kanban -> status, Priority -> priority.
function LaneBoard({ rows, view, selectedId, onOpen, onMove, onRequestClose }: LaneBoardProps) {
  const lanes =
    view === 'kanban'
      ? KANBAN_STATUSES.map((status) => ({
          key: `status:${status}`,
          label: status,
          patch: { status } as Patch,
        }))
      : PRIORITY_LANES.map((priority) => ({
          key: `priority:${priority}`,
          label: `P${priority}`,
          patch: { priority } as Patch,
        }));

  const laneKeyFor = (row: Row): string =>
    view === 'kanban' ? `status:${row.status}` : `priority:${row.priority}`;

  return (
    <div aria-label={`${viewLabel(view)} board`} className="flex gap-3 overflow-x-auto">
      {lanes.map((lane) => {
        const laneRows = rows.filter((row) => laneKeyFor(row) === lane.key);
        return (
          <div
            key={lane.key}
            data-testid={`lane-${lane.key}`}
            onDragOver={(event) => event.preventDefault()}
            onDrop={(event) => {
              event.preventDefault();
              const id = event.dataTransfer.getData('text/bead-id');
              if (id) void onMove(id, lane.patch);
            }}
            className="min-w-[14rem] flex-1 rounded-sm border border-rule p-2"
          >
            <div className="mb-2 flex items-center justify-between">
              <span className="text-label uppercase tracking-wider text-fg-faint">
                {lane.label}
              </span>
              <span className="text-label text-fg-faint">{laneRows.length}</span>
            </div>
            <ul className="space-y-2">
              {laneRows.map((row) => (
                <li
                  key={row.id}
                  draggable
                  onDragStart={(event) => event.dataTransfer.setData('text/bead-id', row.id)}
                  className="rounded-sm border border-rule px-3 py-2"
                >
                  <button
                    type="button"
                    onClick={() => onOpen(row.id)}
                    aria-pressed={selectedId === row.id}
                    className="w-full text-left"
                  >
                    <span className="text-body text-fg font-medium truncate">{row.title}</span>
                    <span className="block text-label text-fg-faint">
                      <code>{row.id}</code>
                    </span>
                  </button>
                  <Button size="sm" tone="quiet" onClick={() => onRequestClose(row.id)}>
                    Close
                  </Button>
                  <select
                    aria-label={`Move ${row.id} to ${view === 'kanban' ? 'status' : 'priority'}`}
                    value={view === 'kanban' ? row.status : row.priority}
                    onChange={(event) =>
                      void onMove(
                        row.id,
                        view === 'kanban'
                          ? { status: event.target.value }
                          : { priority: Number(event.target.value) },
                      )
                    }
                    className="mt-2 w-full rounded-sm border border-rule bg-surface px-2 py-1 text-label text-fg"
                  >
                    <option value={view === 'kanban' ? row.status : row.priority}>Move to…</option>
                    {lanes
                      .filter((target) => target.key !== lane.key)
                      .map((target) => (
                        <option
                          key={target.key}
                          value={view === 'kanban' ? target.patch.status : target.patch.priority}
                        >
                          {target.label}
                        </option>
                      ))}
                  </select>
                </li>
              ))}
            </ul>
          </div>
        );
      })}
    </div>
  );
}

// AttemptPanel projects the selected Bead's newest ACTIVE Execution Attempt and
// its prior attempts from the typed session read. Gas City owns the Session and
// worktree; this only resolves and displays them (no new record).
//
// The terminal is the Session's own live stream (LiveSessionPeek), keyed by the
// attempt's session id: selecting a different attempt changes the key, so React
// detaches the previous stream before attaching the next. It never opens a
// parallel terminal process — it follows Gas City's session identity and
// streaming boundaries, and grants no access to unrelated terminals.
function AttemptPanel({
  bead,
  sessions,
}: {
  bead: Row;
  sessions: NonNullable<Awaited<ReturnType<typeof listSupervisorSessions>>['items']>;
}) {
  const attempts = resolveAttempts(bead, sessions);
  const [inspectedId, setInspectedId] = useState<string | null>(null);
  const inspected =
    attempts.history.find((attempt) => attempt.sessionId === inspectedId) ?? attempts.current;
  const inspectingHistory =
    inspected !== null && inspected.sessionId !== attempts.current?.sessionId;
  return (
    <section aria-label="Execution attempt" className="mt-4 max-w-prose space-y-1">
      {inspected ? (
        <>
          <p className="text-body text-fg">
            {inspectingHistory ? 'Prior' : 'Current'} attempt: <code>{inspected.sessionName}</code>{' '}
            ({inspected.state})
            {inspected.workDir.length > 0 ? (
              <>
                {' · '}
                <code>{inspected.workDir}</code>
              </>
            ) : null}
          </p>
          <LiveSessionPeek
            key={inspected.sessionId}
            sessionId={inspected.sessionId}
            stream={!inspectingHistory}
            showBadge
            showCaption
          />
          {inspectingHistory ? (
            <p className="text-label text-fg-muted">
              Historical worktree diff and PR actions are unavailable; showing this session’s output
              only.
            </p>
          ) : (
            <>
              <AttemptDiffPanel key={`diff:${inspected.sessionId}`} beadId={bead.id} />
              <AttemptChatPanel key={`chat:${inspected.sessionId}`} attempt={inspected} />
              <AttemptPreviewPanel bead={bead} />
              <AttemptPullRequestPanel bead={bead} attempt={inspected} />
            </>
          )}
        </>
      ) : attempts.staleReference ? (
        <p className="text-body text-accent" role="alert">
          Referenced session <code>{attempts.referencedSessionId}</code> is no longer present (stale
          reference).
        </p>
      ) : (
        <AttemptStartActions bead={bead} hasHistory={attempts.history.length > 0} />
      )}
      {attempts.history.length > 0 && (
        <ul aria-label="Attempt history" className="space-y-1">
          {attempts.history.map((attempt) => (
            <li key={attempt.sessionId} className="text-label text-fg-faint">
              <button
                type="button"
                aria-label={`Inspect ${attempt.sessionName}`}
                aria-pressed={inspected?.sessionId === attempt.sessionId}
                onClick={() => setInspectedId(attempt.sessionId)}
                className="text-left underline decoration-rule hover:text-fg focus-mark"
              >
                <code>{attempt.sessionName}</code> — {attempt.state}
              </button>
            </li>
          ))}
        </ul>
      )}
      {inspectingHistory && attempts.current && (
        <Button size="sm" tone="quiet" onClick={() => setInspectedId(null)}>
          Return to current attempt
        </Button>
      )}
    </section>
  );
}

// AttemptDiffPanel shows the read-only worktree diff for the attempt's Bead,
// bounded and with explicit states. Viewing it cannot mutate the worktree.
function AttemptDiffPanel({ beadId }: { beadId: string }) {
  const { data, loading, error } = useCachedData(`workbench:diff:${beadId}`, () =>
    fetchAttemptDiff(beadId),
  );
  if (loading && data === undefined) {
    return <p className="text-body text-fg-muted italic">Loading diff…</p>;
  }
  if (error && data === undefined) {
    return (
      <p className="text-body text-accent" role="alert">
        Diff unavailable: {error}
      </p>
    );
  }
  if (!data) return null;
  if (data.state === 'empty') {
    return <p className="text-body text-fg-muted italic">No changes in this attempt's worktree.</p>;
  }
  if (data.state === 'missing_worktree') {
    return <p className="text-body text-fg-muted italic">Worktree missing for this attempt.</p>;
  }
  if (data.state === 'unavailable') {
    return (
      <p className="text-body text-accent" role="alert">
        Diff unavailable for this attempt.
      </p>
    );
  }
  return (
    <div aria-label="Attempt diff" className="space-y-1">
      {data.binary && <p className="text-label text-fg-faint">Binary diff</p>}
      <pre className="max-h-80 overflow-auto text-label">{data.text}</pre>
      {data.truncated && (
        <p className="text-label text-fg-faint">Diff truncated ({data.bytes} bytes).</p>
      )}
    </div>
  );
}

// AttemptPullRequestPanel exposes policy-bound PR actions for the attempt. It
// has no merge authority: prepare/queue delegate to Gas City (here, a request to
// the merge queue role), and the action + result are auditable against the Bead.
function AttemptPullRequestPanel({ bead, attempt }: { bead: Row; attempt: ExecutionAttempt }) {
  return (
    <section aria-label="Pull request actions" className="mt-3">
      <p role="status" className="text-body text-fg-muted">
        PR actions unavailable: Gas City has not supplied queue, policy, and conflict verdicts for{' '}
        <code>{bead.id}</code> / <code>{attempt.sessionId}</code>.
      </p>
    </section>
  );
}

// AttemptPreviewPanel shows the attempt's application preview (a projection of
// Gas City lifecycle state, resolved from the Bead's gc.preview_url). The frame
// is sandboxed and only ever points at an allowlisted host; missing/blocked/
// unavailable states are explicit, and it starts no preview process.
function AttemptPreviewPanel({ bead }: { bead: Row }) {
  const preview = resolvePreview(bead);
  if (preview.state === 'missing') {
    return <p className="text-body text-fg-muted italic">No preview for this attempt.</p>;
  }
  if (preview.state === 'blocked') {
    return (
      <p className="text-body text-accent" role="alert">
        Preview host {preview.host} is not allowlisted; not framed.
      </p>
    );
  }
  if (preview.state === 'unavailable' || preview.url === null) {
    return (
      <p className="text-body text-accent" role="alert">
        Preview unavailable for this attempt.
      </p>
    );
  }
  return (
    <div aria-label="Attempt preview" className="space-y-1">
      <iframe
        title="Execution Attempt preview"
        src={preview.url}
        sandbox="allow-scripts allow-same-origin"
        className="h-80 w-full rounded-sm border border-rule"
      />
      <p className="text-label text-fg-faint">
        <code>{preview.host}</code>
      </p>
    </div>
  );
}

// AttemptStartActions offers an EXPLICIT start/resume when a Bead has no active
// attempt. Both delegate lifecycle creation to Gas City (sling -> new Session +
// worktree); neither revives a completed/failed Session in place. "Resume in new
// attempt" is offered only when prior attempts exist, and links the new attempt
// to that history by construction (Gas City mints a distinct Session). Repeated
// submission is idempotent: the buttons disable while a start is in flight.
function AttemptStartActions({ bead, hasHistory }: { bead: Row; hasHistory: boolean }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const metadata = (bead as { metadata?: Record<string, string> }).metadata ?? {};
  const target = (metadata['gc.routed_to'] ?? '').trim();

  const start = async () => {
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      await startSupervisorAttempt(bead.id, target);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'start failed');
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-2">
      <p className="text-body text-fg-muted italic">No active execution attempt.</p>
      {target.length > 0 ? (
        <div className="flex gap-2">
          <Button size="sm" onClick={() => void start()} disabled={busy}>
            {hasHistory ? 'Resume in new attempt' : 'Start new attempt'}
          </Button>
        </div>
      ) : (
        <p className="text-body text-fg-faint">
          No sling target on this Bead; start it from its rig.
        </p>
      )}
      {error && (
        <p role="alert" className="text-body text-accent">
          {error}
        </p>
      )}
    </div>
  );
}

type ChatState = 'queued' | 'mail accepted; awaiting session acknowledgement' | 'rejected';
interface ChatMessage {
  id: string;
  text: string;
  state: ChatState;
}

// AttemptChatPanel adds per-attempt chat: a submitted message targets the newest
// active Session/worktree with follow_up semantics. Messages are queued in order
// and only leave the queue once Gas City accepts them; a rejection stays visible
// without re-sending. Sending to a completed/failed attempt is not offered here
// (the panel only renders for a live attempt), so chat cannot restart one.
function AttemptChatPanel({ attempt }: { attempt: ExecutionAttempt }) {
  const { operatorWireAlias } = useOperatorConfig();
  const [draft, setDraft] = useState('');
  const [messages, setMessages] = useState<ChatMessage[]>([]);

  const submit = async () => {
    const text = draft.trim();
    if (text.length === 0) return;
    const id = `${attempt.sessionId}:${Date.now()}:${messages.length}`;
    setMessages((current) => [...current, { id, text, state: 'queued' }]);
    setDraft('');
    try {
      await sendSupervisorMail(
        { to: attempt.sessionName, subject: 'workbench follow-up', body: text },
        operatorWireAlias,
      );
      setMessages((current) =>
        current.map((message) =>
          message.id === id
            ? { ...message, state: 'mail accepted; awaiting session acknowledgement' }
            : message,
        ),
      );
    } catch {
      setMessages((current) =>
        current.map((message) => (message.id === id ? { ...message, state: 'rejected' } : message)),
      );
    }
  };

  return (
    <section aria-label="Attempt chat" className="mt-3 space-y-2">
      <ul aria-label="Queued messages" className="space-y-1">
        {messages.map((message) => (
          <li key={message.id} className="text-label text-fg-faint">
            <span className="uppercase tracking-wider">{message.state}</span> · {message.text}
          </li>
        ))}
      </ul>
      <div className="flex gap-2">
        <input
          aria-label="Message"
          value={draft}
          onChange={(event) => setDraft(event.target.value)}
          className="flex-1 rounded-sm border border-rule px-2 py-1 text-body"
        />
        <Button size="sm" onClick={() => void submit()} disabled={draft.trim().length === 0}>
          Send
        </Button>
      </div>
    </section>
  );
}

function normalizeSelectedBeadParam(value: string | null): string | null {
  const clean = value?.trim();
  return clean && clean.length > 0 ? clean : null;
}

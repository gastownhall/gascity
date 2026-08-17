import { useCallback, useEffect, useMemo, useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import { GC_EVENT_PREFIX } from 'gas-city-dashboard-shared';
import { formatApiError } from '../api/client';
import { getActiveCity } from '../api/cityBase';
import { useAttentionModel } from '../attention/context';
import { resourceAttentionSeverity } from '../attention/routeHighlight';
import { BeadAttentionPanel } from '../components/beads/BeadAttentionPanel';
import { BeadBoardSection } from '../components/beads/BeadBoardSection';
import { BeadDetailModal } from '../components/BeadDetailModal';
import { Button } from '../components/Button';
import { FilterChips } from '../components/FilterChips';
import { ListSearchBar } from '../components/ListSearchBar';
import { Modal } from '../components/Modal';
import { PageHeader } from '../components/PageHeader';
import { StatusBadge } from '../components/StatusBadge';
import { READ_ONLY_CONTROL_TITLE, ReadOnlyBadge, useReadOnly } from '../contexts/ReadOnlyContext';
import { buildBeadGraph } from '../lib/beadGraph';
import { resolveRigName, rigNameOptions } from '../lib/rigNames';
import { useCachedData } from '../hooks/useCachedData';
import { useGcEventRefresh } from '../hooks/useGcEvents';
import { useListFilters, type FilterChip } from '../hooks/useListFilters';
import { beadProject } from '../hooks/projectOf';
import { listSupervisorAgents, type SupervisorAgent } from '../supervisor/agentReads';
import { listSupervisorBeads, type SupervisorBead } from '../supervisor/beadReads';
import { listSupervisorRigs } from '../supervisor/rigReads';
import { closeSupervisorBead, createAndSlingSupervisorBead } from '../supervisor/beadWrites';
import { listSupervisorSessions } from '../supervisor/sessionReads';

const EMPTY_IDS: ReadonlySet<string> = new Set();
const RIG_FILTER_ALL = '';
const CLOSED_CHIP_ID = 'closed';
// The board's refresh is a full list refetch (~1.3MB on a busy city). A busy
// city emits bead.* events roughly every few seconds, so the default ~2.5s
// coalesce window still drives a near-continuous full refetch — wasteful under
// normal churn and, when the supervisor's task query spikes to 14-21s, enough
// to keep superseding each in-flight slow fetch before it lands. Coalesce the
// board's SSE-driven refresh into a much wider trailing window so churn yields
// at most one refetch per window and a latency spike can no longer empty it.
const BOARD_REFRESH_COALESCE_MS = 10_000;

interface ActionMessage {
  tone: 'ok' | 'error';
  text: string;
}

const BEAD_CHIPS: ReadonlyArray<FilterChip<SupervisorBead>> = [
  { id: 'open', label: 'open', match: (b) => b.status === 'open' },
  { id: 'in_progress', label: 'in progress', match: (b) => b.status === 'in_progress' },
  { id: 'blocked', label: 'blocked', match: (b) => b.status === 'blocked' },
  { id: CLOSED_CHIP_ID, label: 'closed', match: (b) => b.status === 'closed' },
];

const BEAD_SEARCH_FIELDS = (b: SupervisorBead): ReadonlyArray<string | undefined> => [
  b.id,
  b.title,
  b.assignee,
  ...(b.labels ?? []),
];

export function BeadsPage() {
  const attention = useAttentionModel();
  const readOnly = useReadOnly();
  const cityName = getActiveCity();
  const cityCacheKey = cityName ?? 'no-city';
  const [searchParams] = useSearchParams();
  const selectedBeadParam = normalizeSelectedBeadParam(searchParams.get('bead'));
  const [rigFilter, setRigFilter] = useState<string>(RIG_FILTER_ALL);
  // Closed beads dwarf the open queue (~199.7K closed vs ~1K open on this
  // city), so scanning them on every load makes the `task` query spike and
  // trips the fetch-window truncation warning. Default the board to the
  // non-closed set (open/in_progress/blocked) and fetch closed beads lazily,
  // only once the operator activates the `closed` status control. showClosed
  // drives BOTH the fetch (includeClosed) and the cache key so flipping it
  // forces exactly one fresh fan-out. It lives here — outside useListFilters,
  // which is declared below and owns the client-side chip filters — because
  // the fetch must be parameterized before the filters hook exists (the
  // hook needs `rows` from this fetch). The `closed` chip's toggle is wired
  // to flip showClosed in addition to the normal client filter, keeping the
  // four status controls reading as one group while `closed` alone widens
  // the data scope.
  const [showClosed, setShowClosed] = useState(false);
  const [selectedId, setSelectedId] = useState<string | null>(selectedBeadParam);
  const [closing, setClosing] = useState<SupervisorBead | null>(null);
  const [actionInFlight, setActionInFlight] = useState<string | null>(null);
  const [actionMessage, setActionMessage] = useState<ActionMessage | null>(null);
  const [creating, setCreating] = useState(false);
  const [createInFlight, setCreateInFlight] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);
  const [newTitle, setNewTitle] = useState('');
  const [newBody, setNewBody] = useState('');
  const [newRig, setNewRig] = useState('');
  const [newAgent, setNewAgent] = useState('');

  const { data, loading, error, refresh } = useCachedData(
    `beads:board:${cityCacheKey}:${rigFilter}:${showClosed ? 'all' : 'open'}`,
    () =>
      listSupervisorBeads({
        includeClosed: showClosed,
        ...(rigFilter === RIG_FILTER_ALL ? {} : { rigFilter }),
      }),
  );
  const rows = useMemo(() => data?.items ?? [], [data]);
  const totalShown = data?.total ?? 0;
  const upstreamTotal = data?.upstream_total;
  const upstreamFetched = data?.upstream_fetched;
  const fetchLimit = data?.fetch_limit;
  const hasLoadedBoard = data !== undefined;

  const sessions = useCachedData(`sessions:${cityCacheKey}`, listSupervisorSessions);
  const sessionItems = useMemo(() => sessions.data?.items ?? [], [sessions.data]);
  const agents = useCachedData(`agents:${cityCacheKey}`, listSupervisorAgents);
  const agentItems = useMemo(() => agents.data?.items ?? [], [agents.data]);
  const rigs = useCachedData(`rigs:${cityCacheKey}`, listSupervisorRigs);
  const rigItems = useMemo(() => rigs.data?.items ?? [], [rigs.data]);

  // The dropdown lists the supervisor's authoritative rig names, never the raw
  // `agent.rig` values — those are sometimes filesystem paths and sometimes
  // directories that belong to no registered rig at all. Agents are matched to
  // a rig by canonicalizing their reported `rig` against the same list, so an
  // agent reporting a path still groups under its real rig name.
  const dispatchRigOptions = useMemo(() => rigNameOptions(rigItems), [rigItems]);
  const agentRigName = useCallback(
    (agent: SupervisorAgent) => resolveRigName(agent.rig, rigItems),
    [rigItems],
  );
  const filteredDispatchAgents = useMemo(
    () =>
      newRig.length === 0
        ? agentItems
        : agentItems.filter((agent) => agentRigName(agent) === newRig),
    [agentItems, agentRigName, newRig],
  );

  useEffect(() => {
    if (!creating) return;
    if (filteredDispatchAgents.length === 0) {
      if (newAgent.length > 0) setNewAgent('');
      return;
    }
    if (!filteredDispatchAgents.some((agent) => agent.name === newAgent)) {
      setNewAgent(filteredDispatchAgents[0]?.name ?? '');
    }
  }, [creating, filteredDispatchAgents, newAgent]);

  useEffect(() => {
    if (rigFilter !== RIG_FILTER_ALL && !dispatchRigOptions.includes(rigFilter)) {
      setRigFilter(RIG_FILTER_ALL);
    }
  }, [dispatchRigOptions, rigFilter]);

  const filteredRows = rows;

  const filters = useListFilters<SupervisorBead>({
    viewKey: 'beads',
    rows: filteredRows,
    projectOf: beadProject,
    searchOf: BEAD_SEARCH_FIELDS,
    chips: BEAD_CHIPS,
  });

  // The `closed` chip is special: besides being a client-side filter (like
  // open/in_progress/blocked), toggling it flips showClosed, which widens the
  // fetch to include closed beads. Keeping it inside BEAD_CHIPS means all four
  // controls render and read as one "Status" group; only this wrapper gives
  // `closed` its extra data-scope effect.
  // Destructure the stable toggleChip so the wrapper can depend on it directly
  // (the `filters` object is a fresh ref each render); keeps toggleStatusChip
  // stable without depending on the whole object.
  const { toggleChip } = filters;
  const toggleStatusChip = useCallback(
    (id: string) => {
      if (id === CLOSED_CHIP_ID) setShowClosed((prev) => !prev);
      toggleChip(id);
    },
    [toggleChip],
  );

  useGcEventRefresh([GC_EVENT_PREFIX.bead], () => void refresh(), {
    coalesceMs: BOARD_REFRESH_COALESCE_MS,
  });

  useEffect(() => {
    if (selectedBeadParam !== null) setSelectedId(selectedBeadParam);
  }, [selectedBeadParam]);

  const runClose = useCallback(
    async (bead: SupervisorBead) => {
      // Defense-in-depth: the disabled buttons already block this, but a
      // keyboard/programmatic path must never reach a write the server 405s.
      if (readOnly) return;
      setActionInFlight(bead.id);
      setActionMessage(null);
      try {
        await closeSupervisorBead(bead.id);
        setClosing(null);
        setActionMessage({ tone: 'ok', text: `Closed ${bead.id}.` });
        await refresh();
      } catch (err) {
        setActionMessage({ tone: 'error', text: formatApiError(err, 'close failed') });
      } finally {
        setActionInFlight(null);
      }
    },
    [readOnly, refresh],
  );

  const openCreateBead = useCallback(() => {
    const defaultRig = dispatchRigOptions[0] ?? '';
    const defaultAgent = agentItems.find(
      (agent) => defaultRig.length === 0 || agentRigName(agent) === defaultRig,
    );
    setNewTitle('');
    setNewBody('');
    setNewRig(defaultRig);
    setNewAgent(defaultAgent?.name ?? '');
    setCreateError(null);
    setActionMessage(null);
    setCreating(true);
  }, [agentItems, agentRigName, dispatchRigOptions]);

  const handleDispatchRigChange = useCallback(
    (rig: string) => {
      setNewRig(rig);
      const currentAgentStillVisible = agentItems.some(
        (agent) => agent.name === newAgent && (rig.length === 0 || agentRigName(agent) === rig),
      );
      if (!currentAgentStillVisible) {
        const defaultAgent = agentItems.find(
          (agent) => rig.length === 0 || agentRigName(agent) === rig,
        );
        setNewAgent(defaultAgent?.name ?? '');
      }
    },
    [agentItems, agentRigName, newAgent],
  );

  const createAndSling = useCallback(async () => {
    // Defense-in-depth: a form Enter-submit can fire even with the submit
    // button disabled, so guard the write itself in read-only mode.
    if (readOnly) return;
    setCreateInFlight(true);
    setCreateError(null);
    try {
      const result = await createAndSlingSupervisorBead({
        title: newTitle,
        description: newBody,
        rig: newRig,
        target: newAgent,
      });
      setActionMessage({
        tone: 'ok',
        text: `Created ${result.bead.id} and slung to ${newAgent}.`,
      });
      setCreating(false);
      await refresh();
    } catch (err) {
      setCreateError(formatApiError(err, 'create and sling failed'));
    } finally {
      setCreateInFlight(false);
    }
  }, [newAgent, newBody, newRig, newTitle, readOnly, refresh]);

  const matched = useMemo(() => filters.groups.flatMap((group) => group.rows), [filters.groups]);
  const graph = useMemo(() => buildBeadGraph(matched), [matched]);
  const groupIds = useMemo(() => {
    const map = new Map<string, Set<string>>();
    for (const group of filters.groups) {
      map.set(group.projectKey, new Set(group.rows.map((row) => row.id)));
    }
    return map;
  }, [filters.groups]);
  const selectedBead = useMemo(
    () => matched.find((bead) => bead.id === selectedId) ?? null,
    [matched, selectedId],
  );
  const selectedNode = useMemo(
    () => (selectedId === null ? null : (graph.nodes.get(selectedId) ?? null)),
    [graph, selectedId],
  );
  const beadAttentionSeverity = useMemo(
    () => (beadId: string) => resourceAttentionSeverity(attention, 'beads', beadId),
    [attention],
  );

  // The "Needs you" section (gascity-dashboard-2j8e.3) renders the same
  // beads-domain attention items the nav badge counts, so the page count and the
  // badge agree. Each item opens the bead to act on it — the operator does not
  // claim beads (a bead assignee must be a concrete session, never the human
  // operator), so there is no inline Claim affordance.
  const renderBeadActions = useCallback(
    (bead: SupervisorBead) => {
      const busy = actionInFlight !== null;
      const actionLabel = actionInFlight === bead.id ? 'closing' : null;

      const roTitle = readOnly ? READ_ONLY_CONTROL_TITLE : undefined;

      return (
        <div className="flex flex-wrap items-center justify-end gap-2">
          {readOnly && <ReadOnlyBadge />}
          {actionLabel && (
            <span className="text-label uppercase tracking-wider text-fg-faint">{actionLabel}</span>
          )}
          <Button
            type="button"
            size="sm"
            tone="quiet"
            title={roTitle}
            disabled={readOnly || busy || bead.status === 'closed'}
            onClick={() => {
              setActionMessage(null);
              setClosing(bead);
            }}
          >
            Close
          </Button>
        </div>
      );
    },
    [actionInFlight, readOnly],
  );

  const synopsis = useMemo(
    () => (hasLoadedBoard ? buildSynopsis(filteredRows, totalShown, rigFilter) : 'Loading beads.'),
    [filteredRows, hasLoadedBoard, totalShown, rigFilter],
  );

  const isTruncated =
    typeof upstreamTotal === 'number' &&
    typeof upstreamFetched === 'number' &&
    upstreamFetched < upstreamTotal;

  return (
    <section>
      <PageHeader
        title="Beads"
        synopsis={synopsis}
        meta={
          <>
            {error && (
              <span className="normal-case text-body text-accent" role="alert">
                {error}
              </span>
            )}
            {sessions.error && (
              <span className="normal-case text-body text-accent" role="alert">
                {sessions.error}
              </span>
            )}
            {agents.error && (
              <span className="normal-case text-body text-accent" role="alert">
                {agents.error}
              </span>
            )}
            {rigs.error && (
              <span className="normal-case text-body text-accent" role="alert">
                {rigs.error}
              </span>
            )}
            <span className="text-label uppercase tracking-wider text-fg-faint">
              {showClosed ? 'All statuses' : 'Open work'}
            </span>
            {readOnly && <ReadOnlyBadge />}
            <Button
              type="button"
              size="sm"
              title={readOnly ? READ_ONLY_CONTROL_TITLE : undefined}
              onClick={openCreateBead}
              disabled={readOnly || agents.loading || agentItems.length === 0}
            >
              New bead
            </Button>
            <Button size="sm" onClick={() => void refresh()} disabled={loading}>
              {loading && !hasLoadedBoard ? 'Loading' : loading ? 'Refreshing' : 'Refresh'}
            </Button>
          </>
        }
      />

      <div className="space-y-2 mb-6 text-body text-fg-muted max-w-prose">
        {isTruncated && (
          <p className="text-warn">
            <StatusBadge
              tone="warn"
              label={`Fetch window covered ${upstreamFetched} of ${upstreamTotal} store beads. Raise the fetch limit (currently ${fetchLimit ?? '?'}) if engineering work sits past the window.`}
            />
          </p>
        )}
        {rigFilter !== RIG_FILTER_ALL && (
          <p>
            Filtering by rig <span className="text-accent">{rigFilter}</span>.{' '}
            <button
              type="button"
              onClick={() => setRigFilter(RIG_FILTER_ALL)}
              className="text-fg-muted hover:text-fg focus-mark underline decoration-dotted underline-offset-2 rounded-sm"
            >
              Clear
            </button>
          </p>
        )}
        {actionMessage && (
          <p
            className={actionMessage.tone === 'error' ? 'text-accent' : 'text-fg-muted'}
            role={actionMessage.tone === 'error' ? 'alert' : 'status'}
          >
            {actionMessage.text}
          </p>
        )}
      </div>

      <BeadAttentionPanel items={attention.byDomain.beads.items} onOpen={setSelectedId} />

      <div className="mb-6 space-y-3">
        <ListSearchBar
          value={filters.search}
          onChange={filters.setSearch}
          placeholder="Search beads by id, title, label, assignee"
          matchCount={filters.totalMatches}
          totalCount={filteredRows.length}
          ariaLabel="Search beads"
        />
        <div className="flex flex-wrap items-baseline gap-x-8 gap-y-3">
          <FilterChips
            chips={BEAD_CHIPS}
            activeIds={filters.activeChipIds}
            onToggle={toggleStatusChip}
            legend="Status"
          />
          {dispatchRigOptions.length > 1 && (
            <label className="flex items-baseline gap-2 text-label">
              <span className="uppercase tracking-wider text-fg-muted">Rig</span>
              <select
                value={rigFilter}
                onChange={(event) => setRigFilter(event.target.value)}
                aria-label="Rig filter"
                className="text-label uppercase tracking-wider text-fg-muted bg-transparent border-0 focus-mark cursor-pointer hover:text-fg transition-colors duration-150 ease-out-quart"
              >
                <option value={RIG_FILTER_ALL}>all rigs</option>
                {dispatchRigOptions.map((rig) => (
                  <option key={rig} value={rig}>
                    {rig}
                  </option>
                ))}
              </select>
            </label>
          )}
        </div>
      </div>

      {!hasLoadedBoard && loading ? (
        <p className="text-body text-fg-muted italic">Loading beads.</p>
      ) : matched.length === 0 ? (
        <p className="text-body text-fg-muted italic">
          {filters.search.length > 0 || filters.activeChipIds.size > 0
            ? 'No beads match the current search or filter.'
            : 'Nothing on the queue right now.'}
        </p>
      ) : (
        <div className="space-y-12">
          {filters.groups.map((group) => (
            <BeadBoardSection
              key={group.projectKey}
              label={group.project}
              count={group.totalInProject}
              graph={graph}
              ids={groupIds.get(group.projectKey) ?? EMPTY_IDS}
              selectedId={selectedId}
              attentionSeverity={beadAttentionSeverity}
              onSelect={setSelectedId}
            />
          ))}
        </div>
      )}

      <BeadDetailModal
        open={selectedId !== null}
        onClose={() => setSelectedId(null)}
        beadId={selectedId}
        initialBead={selectedBead}
        depNode={selectedNode}
        sessions={sessionItems}
        onOpenBead={setSelectedId}
        renderActions={renderBeadActions}
      />

      <Modal
        open={closing !== null}
        onClose={() => {
          if (actionInFlight === null) setClosing(null);
        }}
        title={closing ? `Close ${closing.id}` : 'Close bead'}
        caption={closing?.title}
        widthClass="max-w-xl"
        footer={
          <>
            <Button
              type="button"
              size="sm"
              tone="quiet"
              disabled={actionInFlight !== null}
              onClick={() => setClosing(null)}
            >
              Cancel
            </Button>
            <Button
              type="button"
              size="sm"
              tone="accent"
              title={readOnly ? READ_ONLY_CONTROL_TITLE : undefined}
              disabled={readOnly || closing === null || actionInFlight !== null}
              onClick={() => {
                if (closing) void runClose(closing);
              }}
            >
              Close bead
            </Button>
          </>
        }
      >
        <p className="text-body text-fg-muted">
          Close this bead? It will be marked closed and drop out of the open queue.
        </p>
      </Modal>

      <Modal
        open={creating}
        onClose={() => {
          if (!createInFlight) setCreating(false);
        }}
        title="New bead"
        caption="Create and sling"
        widthClass="max-w-2xl"
        footer={
          <>
            <Button
              type="button"
              size="sm"
              tone="quiet"
              disabled={createInFlight}
              onClick={() => setCreating(false)}
            >
              Cancel
            </Button>
            <Button
              type="submit"
              form="new-bead-form"
              size="sm"
              title={readOnly ? READ_ONLY_CONTROL_TITLE : undefined}
              disabled={
                readOnly ||
                createInFlight ||
                newTitle.trim().length === 0 ||
                newAgent.trim().length === 0
              }
            >
              {createInFlight ? 'Creating' : 'Create and sling'}
            </Button>
          </>
        }
      >
        <form
          id="new-bead-form"
          className="space-y-5"
          onSubmit={(event) => {
            event.preventDefault();
            void createAndSling();
          }}
        >
          {createError && (
            <p className="text-accent" role="alert">
              {createError}
            </p>
          )}
          <label className="block space-y-2 text-body">
            <span className="text-label uppercase tracking-wider text-fg-muted">Title</span>
            <input
              value={newTitle}
              onChange={(event) => setNewTitle(event.target.value)}
              required
              className="w-full rounded-sm border border-rule bg-transparent px-3 py-2 text-body text-fg focus-mark"
            />
          </label>
          <label className="block space-y-2 text-body">
            <span className="text-label uppercase tracking-wider text-fg-muted">Body</span>
            <textarea
              value={newBody}
              onChange={(event) => setNewBody(event.target.value)}
              rows={5}
              className="w-full rounded-sm border border-rule bg-transparent px-3 py-2 text-body text-fg focus-mark"
            />
          </label>
          <div className="grid gap-4 sm:grid-cols-2">
            <label className="block space-y-2 text-body">
              <span className="text-label uppercase tracking-wider text-fg-muted">Rig</span>
              <select
                value={newRig}
                onChange={(event) => handleDispatchRigChange(event.target.value)}
                className="w-full rounded-sm border border-rule bg-transparent px-3 py-2 text-body text-fg focus-mark"
              >
                {dispatchRigOptions.length === 0 && <option value="">all rigs</option>}
                {dispatchRigOptions.map((rig) => (
                  <option key={rig} value={rig}>
                    {rig}
                  </option>
                ))}
              </select>
            </label>
            <label className="block space-y-2 text-body">
              <span className="text-label uppercase tracking-wider text-fg-muted">Agent</span>
              <select
                value={newAgent}
                onChange={(event) => setNewAgent(event.target.value)}
                required
                className="w-full rounded-sm border border-rule bg-transparent px-3 py-2 text-body text-fg focus-mark"
              >
                {filteredDispatchAgents.map((agent) => (
                  <option key={agent.name} value={agent.name}>
                    {agent.display_name ?? agent.name}
                  </option>
                ))}
              </select>
            </label>
          </div>
        </form>
      </Modal>
    </section>
  );
}

function normalizeSelectedBeadParam(value: string | null): string | null {
  const clean = value?.trim();
  return clean && clean.length > 0 ? clean : null;
}

function buildSynopsis(
  filtered: ReadonlyArray<SupervisorBead>,
  totalShown: number,
  rigFilter: string,
): string {
  if (rigFilter !== RIG_FILTER_ALL && filtered.length === 0) return `No beads on ${rigFilter}.`;
  const open = filtered.filter((bead) => bead.status === 'open').length;
  const inProgress = filtered.filter((bead) => bead.status === 'in_progress').length;
  const blocked = filtered.filter((bead) => bead.status === 'blocked').length;
  const parts: string[] = [];
  if (open > 0) parts.push(`${open} open`);
  if (inProgress > 0) parts.push(`${inProgress} in progress`);
  if (blocked > 0) parts.push(`${blocked} blocked`);
  if (parts.length === 0) return 'Nothing on the queue.';
  let summary = `${parts.join(', ')}.`;
  if (rigFilter !== RIG_FILTER_ALL) summary = `${rigFilter}: ${summary}`;
  if (totalShown > filtered.length) summary += ` Showing ${filtered.length} of ${totalShown}.`;
  return summary;
}

import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type PointerEvent as ReactPointerEvent,
  type WheelEvent as ReactWheelEvent,
} from 'react';
import { GC_EVENT_PREFIX } from 'gas-city-dashboard-shared';
import { useLocation } from 'react-router-dom';
import { getActiveCity } from '../api/cityBase';
import { BeadDetailModal } from '../components/BeadDetailModal';
import { PageHeader } from '../components/PageHeader';
import { useCachedData } from '../hooks/useCachedData';
import { useGcEventRefresh } from '../hooks/useGcEvents';
import {
  buildCanvasEdges,
  computeCanvasLayout,
  fitCanvas,
  normalizeCanvasPriority,
  type CanvasBounds,
  type CanvasEdge,
  type CanvasLayoutMode,
  type CanvasPoint,
} from '../lib/beadsCanvasLayout';
import { buildBeadGraph } from '../lib/beadGraph';
import { listSupervisorBeads, type SupervisorBead } from '../supervisor/beadReads';
import { listSupervisorRigs } from '../supervisor/rigReads';

const NODE_BASE_WIDTH = 220;
const NODE_BASE_HEIGHT = 112;
const CANVAS_HEIGHT = 680;
const REFRESH_COALESCE_MS = 10_000;
const STATUS_ORDER = ['open', 'in_progress', 'blocked', 'closed'] as const;
const TYPE_ORDER = ['bug', 'feature', 'task', 'epic', 'chore', 'decision'] as const;
const TYPE_COLORS: Readonly<Record<string, string>> = {
  bug: '#fb7185',
  feature: '#2dd4bf',
  task: '#60a5fa',
  epic: '#fbbf24',
  chore: '#94a3b8',
  decision: '#e879f9',
};
const STATUS_COLORS: Readonly<Record<string, string>> = {
  open: '#60a5fa',
  in_progress: '#34d399',
  blocked: '#fb7185',
  closed: '#94a3b8',
};
const EDGE_STYLES: Readonly<Record<string, { color: string; dash?: string }>> = {
  blocks: { color: '#fb7185' },
  contains: { color: '#94a3b8', dash: '7 7' },
  references: { color: '#60a5fa', dash: '3 6' },
  'derived-from': { color: '#34d399', dash: '9 6' },
  supersedes: { color: '#fbbf24', dash: '12 6' },
};

type PositionOverrides = Record<string, CanvasPoint>;

interface PanDrag {
  pointerId: number;
  clientX: number;
  clientY: number;
  pan: CanvasPoint;
}

interface NodeDrag {
  pointerId: number;
  id: string;
  clientX: number;
  clientY: number;
  position: CanvasPoint;
  moved: boolean;
}

export function BeadsCanvasPage() {
  const cityName = getActiveCity();
  const location = useLocation();
  const cityKey = cityName ?? 'no-city';
  const viewportRef = useRef<HTMLDivElement>(null);
  const panDragRef = useRef<PanDrag | null>(null);
  const nodeDragRef = useRef<NodeDrag | null>(null);
  const suppressNodeClickRef = useRef(false);
  const [rigFilter, setRigFilter] = useState('');
  const [showClosed, setShowClosed] = useState(false);
  const [layoutMode, setLayoutMode] = useState<CanvasLayoutMode>('clusters');
  const [query, setQuery] = useState('');
  const [hiddenStatuses, setHiddenStatuses] = useState<ReadonlySet<string>>(
    () => new Set(['closed']),
  );
  const [hiddenTypes, setHiddenTypes] = useState<ReadonlySet<string>>(() => new Set());
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [viewport, setViewport] = useState({ width: 1200, height: CANVAS_HEIGHT });
  const [pan, setPan] = useState<CanvasPoint>({ x: 40, y: 40 });
  const [zoom, setZoom] = useState(1);
  const storageKey = `gascity:beads-canvas:positions:v1:${cityKey}`;
  const [overrides, setOverrides] = useState<PositionOverrides>(() => readOverrides(storageKey));
  const controlTarget = useMemo(
    () => resolveControlTarget(location.search, cityName),
    [cityName, location.search],
  );

  useEffect(() => {
    setOverrides(readOverrides(storageKey));
  }, [storageKey]);

  const { data, loading, error, refresh } = useCachedData(
    `beads:canvas:${cityKey}:${rigFilter}:${showClosed ? 'all' : 'open'}`,
    (signal) =>
      listSupervisorBeads({
        includeClosed: showClosed,
        ...(rigFilter ? { rigFilter } : {}),
        signal,
      }),
  );
  const rigs = useCachedData(`rigs:canvas:${cityKey}`, () => listSupervisorRigs());

  useGcEventRefresh([GC_EVENT_PREFIX.bead], () => void refresh(), {
    coalesceMs: REFRESH_COALESCE_MS,
  });

  const rows = useMemo(() => data?.items ?? [], [data]);
  const issueTypes = useMemo(
    () =>
      [...new Set(rows.map((bead) => bead.issue_type))].sort(
        (a, b) => typeRank(a) - typeRank(b) || a.localeCompare(b),
      ),
    [rows],
  );
  const normalizedQuery = query.trim().toLowerCase();
  const visibleBeads = useMemo(
    () =>
      rows.filter((bead) => {
        if (hiddenStatuses.has(bead.status) || hiddenTypes.has(bead.issue_type)) return false;
        if (!normalizedQuery) return true;
        return [bead.id, bead.title, bead.assignee ?? '', ...(bead.labels ?? [])]
          .join(' ')
          .toLowerCase()
          .includes(normalizedQuery);
      }),
    [hiddenStatuses, hiddenTypes, normalizedQuery, rows],
  );
  const layoutItems = useMemo(
    () =>
      visibleBeads.map((bead) => {
        const scale = bead.priority === 0 ? 1.16 : bead.priority === 1 ? 1.08 : 1;
        return {
          id: bead.id,
          issueType: bead.issue_type,
          priority: bead.priority,
          width: NODE_BASE_WIDTH * scale,
          height: NODE_BASE_HEIGHT * scale,
        };
      }),
    [visibleBeads],
  );
  const sizeById = useMemo(
    () => new Map(layoutItems.map((item) => [item.id, { width: item.width, height: item.height }])),
    [layoutItems],
  );
  const layout = useMemo(
    () => computeCanvasLayout(layoutItems, layoutMode, viewport.width / viewport.height),
    [layoutItems, layoutMode, viewport.height, viewport.width],
  );
  const positions = useMemo(() => {
    const combined = new Map(layout.positions);
    for (const bead of visibleBeads) {
      const overridden = overrides[bead.id];
      if (overridden) combined.set(bead.id, overridden);
    }
    return combined;
  }, [layout.positions, overrides, visibleBeads]);
  const edges = useMemo(() => buildCanvasEdges(visibleBeads), [visibleBeads]);
  const contentBounds = useMemo(
    () => boundsForPositions(layout.bounds, positions, sizeById),
    [layout.bounds, positions, sizeById],
  );
  const graph = useMemo(() => buildBeadGraph(visibleBeads), [visibleBeads]);
  const selectedBead = useMemo(
    () => visibleBeads.find((bead) => bead.id === selectedId) ?? null,
    [selectedId, visibleBeads],
  );

  useEffect(() => {
    const element = viewportRef.current;
    if (!element) return;
    const update = () => {
      const rect = element.getBoundingClientRect();
      setViewport({
        width: Math.max(1, rect.width || 1200),
        height: Math.max(1, rect.height || CANVAS_HEIGHT),
      });
    };
    update();
    if (typeof ResizeObserver !== 'function') return;
    const observer = new ResizeObserver(update);
    observer.observe(element);
    return () => observer.disconnect();
  }, []);

  const fit = useCallback(() => {
    const fitted = fitCanvas(contentBounds, viewport);
    setZoom(fitted.zoom);
    setPan(fitted.pan);
  }, [contentBounds, viewport]);

  const toggleStatus = (status: string) => {
    if (status === 'closed') setShowClosed((current) => !current);
    setHiddenStatuses((current) => toggledSet(current, status));
  };

  const toggleType = (issueType: string) => {
    setHiddenTypes((current) => toggledSet(current, issueType));
  };

  const resetLayout = () => {
    setOverrides({});
    localStorage.removeItem(storageKey);
    setZoom(1);
    setPan({ x: 40, y: 40 });
  };

  const handleViewportPointerDown = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (event.button !== 0) return;
    const target = event.target;
    if (target instanceof Element && target.closest('[data-canvas-node]')) return;
    event.currentTarget.setPointerCapture(event.pointerId);
    panDragRef.current = {
      pointerId: event.pointerId,
      clientX: event.clientX,
      clientY: event.clientY,
      pan,
    };
  };

  const handleViewportPointerMove = (event: ReactPointerEvent<HTMLDivElement>) => {
    const drag = panDragRef.current;
    if (!drag || drag.pointerId !== event.pointerId) return;
    setPan({
      x: drag.pan.x + event.clientX - drag.clientX,
      y: drag.pan.y + event.clientY - drag.clientY,
    });
  };

  const endViewportDrag = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (panDragRef.current?.pointerId !== event.pointerId) return;
    panDragRef.current = null;
    if (event.currentTarget.hasPointerCapture(event.pointerId)) {
      event.currentTarget.releasePointerCapture(event.pointerId);
    }
  };

  const handleWheel = (event: ReactWheelEvent<HTMLDivElement>) => {
    event.preventDefault();
    const rect = event.currentTarget.getBoundingClientRect();
    const pointer = { x: event.clientX - rect.left, y: event.clientY - rect.top };
    const nextZoom = clamp(zoom * (event.deltaY < 0 ? 1.12 : 0.89), 0.08, 2.5);
    const ratio = nextZoom / zoom;
    setPan({
      x: pointer.x - (pointer.x - pan.x) * ratio,
      y: pointer.y - (pointer.y - pan.y) * ratio,
    });
    setZoom(nextZoom);
  };

  const beginNodeDrag = (
    event: ReactPointerEvent<HTMLButtonElement>,
    id: string,
    position: CanvasPoint,
  ) => {
    if (event.button !== 0) return;
    event.stopPropagation();
    event.currentTarget.setPointerCapture(event.pointerId);
    nodeDragRef.current = {
      pointerId: event.pointerId,
      id,
      clientX: event.clientX,
      clientY: event.clientY,
      position,
      moved: false,
    };
  };

  const moveNode = (event: ReactPointerEvent<HTMLButtonElement>) => {
    const drag = nodeDragRef.current;
    if (!drag || drag.pointerId !== event.pointerId) return;
    const dx = (event.clientX - drag.clientX) / zoom;
    const dy = (event.clientY - drag.clientY) / zoom;
    if (Math.abs(dx) + Math.abs(dy) > 3) drag.moved = true;
    setOverrides((current) => ({
      ...current,
      [drag.id]: { x: drag.position.x + dx, y: drag.position.y + dy },
    }));
  };

  const endNodeDrag = (event: ReactPointerEvent<HTMLButtonElement>) => {
    const drag = nodeDragRef.current;
    if (!drag || drag.pointerId !== event.pointerId) return;
    suppressNodeClickRef.current = drag.moved;
    nodeDragRef.current = null;
    if (event.currentTarget.hasPointerCapture(event.pointerId)) {
      event.currentTarget.releasePointerCapture(event.pointerId);
    }
    setOverrides((current) => {
      writeOverrides(storageKey, current);
      return current;
    });
  };

  const openNode = (id: string) => {
    if (suppressNodeClickRef.current) {
      suppressNodeClickRef.current = false;
      return;
    }
    setSelectedId(id);
  };

  return (
    <section className="space-y-5" aria-labelledby="beads-canvas-title">
      <PageHeader
        title="Beads Canvas"
        synopsis="The Inktree Bubbly Canvas model, projected over this city's live Beads store."
      />

      <div className="beads-canvas-controls" aria-label="Canvas controls">
        <div className="flex flex-wrap items-center gap-2">
          <button
            type="button"
            className={modeButtonClass(layoutMode === 'clusters')}
            aria-pressed={layoutMode === 'clusters'}
            onClick={() => setLayoutMode('clusters')}
          >
            Clusters
          </button>
          <button
            type="button"
            className={modeButtonClass(layoutMode === 'lanes')}
            aria-pressed={layoutMode === 'lanes'}
            onClick={() => setLayoutMode('lanes')}
          >
            Priority lanes
          </button>
          <span className="mx-1 h-5 border-l border-rule" aria-hidden="true" />
          {STATUS_ORDER.map((status) => (
            <button
              key={status}
              type="button"
              className={filterButtonClass(!hiddenStatuses.has(status))}
              aria-pressed={!hiddenStatuses.has(status)}
              onClick={() => toggleStatus(status)}
            >
              <span
                className="inline-block h-2 w-2 rounded-full"
                style={{ backgroundColor: STATUS_COLORS[status] }}
                aria-hidden="true"
              />
              {status.replace('_', ' ')}
            </button>
          ))}
        </div>

        <div className="grid min-w-0 gap-2 md:grid-cols-[minmax(12rem,1fr)_12rem_auto]">
          <label className="sr-only" htmlFor="canvas-search">
            Search beads
          </label>
          <input
            id="canvas-search"
            type="search"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="Search id, title, assignee, label"
            className="min-w-0 rounded-sm border border-rule bg-surface px-3 py-2 text-sm text-fg outline-none focus:border-accent"
          />
          <label className="sr-only" htmlFor="canvas-rig-filter">
            Rig filter
          </label>
          <select
            id="canvas-rig-filter"
            value={rigFilter}
            onChange={(event) => setRigFilter(event.target.value)}
            className="rounded-sm border border-rule bg-surface px-3 py-2 text-sm text-fg outline-none focus:border-accent"
          >
            <option value="">all rigs</option>
            {(rigs.data?.items ?? []).map((rig) => (
              <option key={rig.name} value={rig.name}>
                {rig.name}
              </option>
            ))}
          </select>
          <div className="flex items-center gap-1">
            <button
              type="button"
              className="beads-canvas-tool"
              onClick={() => setZoom((v) => clamp(v / 1.2, 0.08, 2.5))}
              aria-label="Zoom out"
            >
              −
            </button>
            <button type="button" className="beads-canvas-tool min-w-14" onClick={fit}>
              fit
            </button>
            <button
              type="button"
              className="beads-canvas-tool"
              onClick={() => setZoom((v) => clamp(v * 1.2, 0.08, 2.5))}
              aria-label="Zoom in"
            >
              +
            </button>
            <button type="button" className="beads-canvas-tool" onClick={resetLayout}>
              reset
            </button>
          </div>
        </div>

        {issueTypes.length > 0 && (
          <div className="flex flex-wrap gap-2" aria-label="Issue type filters">
            {issueTypes.map((issueType) => (
              <button
                key={issueType}
                type="button"
                className={filterButtonClass(!hiddenTypes.has(issueType))}
                aria-pressed={!hiddenTypes.has(issueType)}
                onClick={() => toggleType(issueType)}
              >
                <span
                  className="inline-block h-2 w-2 rounded-full"
                  style={{ backgroundColor: typeColor(issueType) }}
                  aria-hidden="true"
                />
                {issueType}
              </button>
            ))}
          </div>
        )}

        <div className="flex flex-wrap items-center justify-between gap-3 text-xs text-fg-faint">
          <span aria-live="polite">
            {loading && !data
              ? 'Loading Beads…'
              : `${visibleBeads.length} visible · ${rows.length} loaded · ${Math.round(zoom * 100)}%`}
          </span>
          <span>drag bubbles · drag space to pan · wheel to zoom · click to inspect</span>
        </div>
      </div>

      {error && (
        <p role="alert" className="border-l-2 border-warn pl-3 text-sm text-warn">
          Could not refresh the live canvas: {error}
        </p>
      )}

      <div
        ref={viewportRef}
        className="beads-canvas-viewport"
        style={{ height: CANVAS_HEIGHT }}
        onPointerDown={handleViewportPointerDown}
        onPointerMove={handleViewportPointerMove}
        onPointerUp={endViewportDrag}
        onPointerCancel={endViewportDrag}
        onWheel={handleWheel}
        role="application"
        aria-label="Beads dependency canvas"
      >
        {visibleBeads.length === 0 && !loading ? (
          <div className="absolute inset-0 grid place-items-center text-sm text-fg-muted">
            No beads match these filters.
          </div>
        ) : (
          <div
            className="absolute left-0 top-0 origin-top-left"
            style={{
              width: contentBounds.x + contentBounds.width + 80,
              height: contentBounds.y + contentBounds.height + 80,
              transform: `translate(${pan.x}px, ${pan.y}px) scale(${zoom})`,
            }}
          >
            {layout.territories.map((territory) => (
              <div
                key={territory.key}
                className="beads-canvas-territory"
                style={{
                  left: territory.x,
                  top: territory.y,
                  width: territory.width,
                  height: territory.height,
                }}
              >
                <span className="beads-canvas-territory-label">
                  {territory.label} · {territory.members.length}
                </span>
              </div>
            ))}

            <svg
              className="pointer-events-none absolute left-0 top-0 overflow-visible"
              width={contentBounds.x + contentBounds.width + 80}
              height={contentBounds.y + contentBounds.height + 80}
              aria-label={`${edges.length} dependency edges`}
            >
              <defs>
                <marker
                  id="beads-canvas-arrow"
                  viewBox="0 0 10 10"
                  refX="9"
                  refY="5"
                  markerWidth="6"
                  markerHeight="6"
                  orient="auto-start-reverse"
                >
                  <path d="M 0 0 L 10 5 L 0 10 z" fill="context-stroke" />
                </marker>
              </defs>
              {edges.map((edge) => (
                <CanvasEdgeLine
                  key={`${edge.from}:${edge.to}:${edge.relation}`}
                  edge={edge}
                  positions={positions}
                  sizeById={sizeById}
                />
              ))}
            </svg>

            {visibleBeads.map((bead) => {
              const position = positions.get(bead.id);
              const size = sizeById.get(bead.id);
              if (!position || !size) return null;
              return (
                <button
                  key={bead.id}
                  type="button"
                  data-canvas-node
                  className="beads-canvas-bubble focus-mark"
                  style={bubbleStyle(bead, position, size)}
                  title={`Inspect ${bead.id}`}
                  aria-label={`${bead.id}: ${bead.title}`}
                  onPointerDown={(event) => beginNodeDrag(event, bead.id, position)}
                  onPointerMove={moveNode}
                  onPointerUp={endNodeDrag}
                  onPointerCancel={endNodeDrag}
                  onClick={() => openNode(bead.id)}
                >
                  <span className="flex items-center justify-between gap-2 text-[0.65rem] uppercase tracking-wider text-fg-faint">
                    <code>{bead.id}</code>
                    <span>P{normalizeCanvasPriority(bead.priority)}</span>
                  </span>
                  <strong className="line-clamp-2 text-left text-sm font-semibold leading-snug text-fg">
                    {bead.title}
                  </strong>
                  <span className="flex min-w-0 items-center justify-between gap-2 text-[0.65rem] text-fg-muted">
                    <span className="truncate">{bead.issue_type}</span>
                    <span className="truncate">{bead.status.replace('_', ' ')}</span>
                  </span>
                </button>
              );
            })}
          </div>
        )}

        {visibleBeads.length > 0 && (
          <CanvasMinimap
            bounds={contentBounds}
            positions={positions}
            sizeById={sizeById}
            beads={visibleBeads}
            viewport={viewport}
            pan={pan}
            zoom={zoom}
            onCenter={(world) =>
              setPan({
                x: viewport.width / 2 - world.x * zoom,
                y: viewport.height / 2 - world.y * zoom,
              })
            }
          />
        )}
      </div>

      <nav className="beads-canvas-control-dock" aria-label="Orchestration control">
        <span className="beads-canvas-control-status">
          <span aria-hidden="true" />
          live Beads
        </span>
        <span className="beads-canvas-control-divider" aria-hidden="true" />
        <a
          href={controlTarget.href}
          className="beads-canvas-control-link focus-mark"
          target={controlTarget.external ? '_blank' : undefined}
          rel={controlTarget.external ? 'noreferrer' : undefined}
        >
          {controlTarget.label}
        </a>
      </nav>

      <BeadDetailModal
        open={selectedId !== null}
        beadId={selectedId}
        initialBead={selectedBead}
        depNode={selectedId ? (graph.nodes.get(selectedId) ?? null) : null}
        onOpenBead={setSelectedId}
        onClose={() => setSelectedId(null)}
      />
    </section>
  );
}

function resolveControlTarget(
  search: string,
  cityName: string | null,
): { label: string; href: string; external: boolean } {
  const params = new URLSearchParams(search);
  const configuredUrl = params.get('controlUrl');
  if (params.get('control') === 'gastown' && configuredUrl) {
    try {
      const parsed = new URL(configuredUrl);
      if (parsed.protocol === 'http:' || parsed.protocol === 'https:') {
        return { label: 'Gas Town control', href: parsed.href, external: true };
      }
    } catch {
      // An invalid or unsafe configured target falls through to local Gas City.
    }
  }

  return {
    label: 'Gas City control',
    href: cityName ? `/city/${encodeURIComponent(cityName)}` : '/',
    external: false,
  };
}

function CanvasEdgeLine({
  edge,
  positions,
  sizeById,
}: {
  edge: CanvasEdge;
  positions: ReadonlyMap<string, CanvasPoint>;
  sizeById: ReadonlyMap<string, { width: number; height: number }>;
}) {
  const from = positions.get(edge.from);
  const to = positions.get(edge.to);
  const fromSize = sizeById.get(edge.from);
  const toSize = sizeById.get(edge.to);
  if (!from || !to || !fromSize || !toSize) return null;
  const style = EDGE_STYLES[edge.relation] ?? { color: '#a78bfa', dash: '4 6' };
  return (
    <line
      x1={from.x + fromSize.width / 2}
      y1={from.y + fromSize.height / 2}
      x2={to.x + toSize.width / 2}
      y2={to.y + toSize.height / 2}
      stroke={style.color}
      strokeWidth="2"
      strokeDasharray={style.dash}
      strokeOpacity="0.64"
      markerEnd="url(#beads-canvas-arrow)"
    >
      <title>{`${edge.from} ${edge.relation} ${edge.to}`}</title>
    </line>
  );
}

function CanvasMinimap({
  bounds,
  positions,
  sizeById,
  beads,
  viewport,
  pan,
  zoom,
  onCenter,
}: {
  bounds: CanvasBounds;
  positions: ReadonlyMap<string, CanvasPoint>;
  sizeById: ReadonlyMap<string, { width: number; height: number }>;
  beads: readonly SupervisorBead[];
  viewport: { width: number; height: number };
  pan: CanvasPoint;
  zoom: number;
  onCenter: (point: CanvasPoint) => void;
}) {
  const width = 190;
  const height = 118;
  const scale = Math.min(width / Math.max(bounds.width, 1), height / Math.max(bounds.height, 1));
  const toMini = (point: CanvasPoint) => ({
    x: (point.x - bounds.x) * scale,
    y: (point.y - bounds.y) * scale,
  });
  const visibleTopLeft = { x: -pan.x / zoom, y: -pan.y / zoom };
  const miniViewport = toMini(visibleTopLeft);

  return (
    <div className="beads-canvas-minimap" aria-label="Minimap navigation">
      <div className="mb-1 flex items-center justify-between text-[0.65rem] uppercase tracking-wider text-fg-faint">
        <span>minimap</span>
        <span>{Math.round(zoom * 100)}%</span>
      </div>
      <svg
        width={width}
        height={height}
        viewBox={`0 0 ${width} ${height}`}
        role="application"
        aria-label="Canvas minimap; click to navigate"
        onClick={(event) => {
          const rect = event.currentTarget.getBoundingClientRect();
          onCenter({
            x: bounds.x + ((event.clientX - rect.left) / rect.width) * bounds.width,
            y: bounds.y + ((event.clientY - rect.top) / rect.height) * bounds.height,
          });
        }}
      >
        {beads.map((bead) => {
          const position = positions.get(bead.id);
          const size = sizeById.get(bead.id);
          if (!position || !size) return null;
          const mini = toMini(position);
          return (
            <rect
              key={bead.id}
              x={mini.x}
              y={mini.y}
              width={Math.max(2, size.width * scale)}
              height={Math.max(2, size.height * scale)}
              rx="1"
              fill={typeColor(bead.issue_type)}
              opacity="0.72"
            />
          );
        })}
        <rect
          x={miniViewport.x}
          y={miniViewport.y}
          width={(viewport.width / zoom) * scale}
          height={(viewport.height / zoom) * scale}
          fill="none"
          stroke="currentColor"
          strokeWidth="1.5"
        />
      </svg>
    </div>
  );
}

function boundsForPositions(
  fallback: CanvasBounds,
  positions: ReadonlyMap<string, CanvasPoint>,
  sizeById: ReadonlyMap<string, { width: number; height: number }>,
): CanvasBounds {
  if (positions.size === 0) return fallback;
  let left = fallback.x;
  let top = fallback.y;
  let right = fallback.x + fallback.width;
  let bottom = fallback.y + fallback.height;
  for (const [id, point] of positions) {
    const size = sizeById.get(id);
    if (!size) continue;
    left = Math.min(left, point.x);
    top = Math.min(top, point.y);
    right = Math.max(right, point.x + size.width);
    bottom = Math.max(bottom, point.y + size.height);
  }
  return { x: left, y: top, width: right - left, height: bottom - top };
}

function bubbleStyle(
  bead: SupervisorBead,
  position: CanvasPoint,
  size: { width: number; height: number },
): CSSProperties {
  const type = typeColor(bead.issue_type);
  const status = STATUS_COLORS[bead.status] ?? '#a78bfa';
  return {
    left: position.x,
    top: position.y,
    width: size.width,
    height: size.height,
    borderColor: type,
    boxShadow: `0 18px 45px -28px ${type}, inset 0 1px 0 rgba(255,255,255,.18)`,
    background: `linear-gradient(145deg, color-mix(in oklch, ${type} 12%, transparent), oklch(var(--surface) / .9) 54%), radial-gradient(circle at 82% 14%, ${status}22, transparent 42%)`,
  };
}

function typeColor(issueType: string): string {
  return TYPE_COLORS[issueType] ?? '#a78bfa';
}

function typeRank(issueType: string): number {
  const rank = TYPE_ORDER.indexOf(issueType as (typeof TYPE_ORDER)[number]);
  return rank === -1 ? TYPE_ORDER.length : rank;
}

function modeButtonClass(active: boolean): string {
  return `rounded-sm border px-3 py-1.5 text-xs font-medium transition-colors ${
    active
      ? 'border-accent bg-accent/10 text-accent'
      : 'border-rule bg-surface text-fg-muted hover:text-fg'
  }`;
}

function filterButtonClass(active: boolean): string {
  return `inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs transition-colors ${
    active
      ? 'border-rule bg-surface text-fg'
      : 'border-transparent bg-surface-tint text-fg-faint opacity-50'
  }`;
}

function toggledSet(current: ReadonlySet<string>, value: string): ReadonlySet<string> {
  const next = new Set(current);
  if (next.has(value)) next.delete(value);
  else next.add(value);
  return next;
}

function readOverrides(key: string): PositionOverrides {
  try {
    const parsed: unknown = JSON.parse(localStorage.getItem(key) ?? '{}');
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return {};
    const result: PositionOverrides = {};
    for (const [id, value] of Object.entries(parsed)) {
      if (
        value &&
        typeof value === 'object' &&
        'x' in value &&
        'y' in value &&
        typeof value.x === 'number' &&
        typeof value.y === 'number' &&
        Number.isFinite(value.x) &&
        Number.isFinite(value.y)
      ) {
        result[id] = { x: value.x, y: value.y };
      }
    }
    return result;
  } catch {
    return {};
  }
}

function writeOverrides(key: string, overrides: PositionOverrides): void {
  try {
    localStorage.setItem(key, JSON.stringify(overrides));
  } catch {
    // Layout persistence is a convenience; private browsing can reject writes.
  }
}

function clamp(value: number, minimum: number, maximum: number): number {
  return Math.min(maximum, Math.max(minimum, value));
}

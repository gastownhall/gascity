import type { SupervisorBead } from '../supervisor/beadReads';

// Pure dependency-graph + kanban-column builder for the Beads board
// (gascity-dashboard-6frc). The board is "part kanban, part dependency
// graph" in the DESIGN.md editorial register: status columns, with each
// bead's upstream (`deps`) and downstream (`blocks`) relations surfaced as
// typeset rows rather than a node-edge canvas.
//
// Honesty discipline (inherited from prd_bead-linked-view.md): the fetched
// bead list is a window, not the whole store. A dependency that points
// outside the window resolves to `bead: null` and is rendered `unresolved`,
// never fabricated. "Ready" is only claimed when every blocking need
// resolves to a closed bead inside the window — an unresolved need means we
// cannot prove readiness, so we do not assert it.
//
// ZFC: pure structural inversion of supervisor-provided fields (`needs`,
// `dependencies`, `status`). No scoring, no semantic classification.

export type BoardColumnId = 'ready' | 'open' | 'in_progress' | 'blocked' | 'done';

export interface BoardColumn {
  id: BoardColumnId;
  /** Display label in the established label register (caller upper-cases). */
  label: string;
}

/** Column order, left to right. The single source of truth for the board. */
export const BOARD_COLUMNS: readonly BoardColumn[] = [
  { id: 'ready', label: 'ready' },
  { id: 'open', label: 'open' },
  { id: 'in_progress', label: 'in progress' },
  { id: 'blocked', label: 'blocked' },
  { id: 'done', label: 'done' },
];

/** A single upstream dependency edge, resolved against the fetched window. */
export interface BeadDepEdge {
  /** The dependency target bead id (always present). */
  id: string;
  /** Resolved bead in the fetched window, or null when it points outside. */
  bead: SupervisorBead | null;
  /** Edge provenance: `needs` (the supervisor's blocking set) or a
   *  `dependencies[].type` string. */
  kind: string;
}

export interface BeadNode {
  bead: SupervisorBead;
  /** Upstream beads this one depends on, each resolved-or-unresolved. */
  deps: BeadDepEdge[];
  /** Downstream beads that depend on this one (in-window only). */
  blocks: SupervisorBead[];
  /** Derived board column. */
  column: BoardColumnId;
  /** Open + every blocking need resolves to a closed in-window bead. */
  ready: boolean;
  /** Any upstream edge points outside the fetched window. */
  hasUnresolvedDeps: boolean;
}

export interface BeadGraph {
  /** Every bead keyed by id. */
  nodes: Map<string, BeadNode>;
  /** Beads grouped by column, each sorted by priority then id. */
  columns: Record<BoardColumnId, BeadNode[]>;
}

/** Upstream dependency ids for a bead, paired with their edge kind, deduped
 *  (first occurrence wins; `needs` is read before structured dependencies). */
function depEntries(bead: SupervisorBead): ReadonlyArray<{ id: string; kind: string }> {
  const seen = new Set<string>();
  const entries: { id: string; kind: string }[] = [];
  for (const id of bead.needs ?? []) {
    if (id.length === 0 || seen.has(id)) continue;
    seen.add(id);
    entries.push({ id, kind: 'needs' });
  }
  for (const dep of bead.dependencies ?? []) {
    const id = dep.depends_on_id;
    if (id.length === 0 || seen.has(id)) continue;
    seen.add(id);
    entries.push({ id, kind: dep.type });
  }
  return entries;
}

/** Blocking-need ids only (the supervisor's explicit `needs` set), used for
 *  readiness. Structured `dependencies` of arbitrary type are shown in the
 *  graph but do not gate readiness — only `needs` is documented as
 *  "needs before it can run". */
function blockingNeedIds(bead: SupervisorBead): readonly string[] {
  return (bead.needs ?? []).filter((id) => id.length > 0);
}

function columnFor(node: { bead: SupervisorBead; ready: boolean }): BoardColumnId {
  switch (node.bead.status) {
    case 'in_progress':
      return 'in_progress';
    case 'blocked':
      return 'blocked';
    case 'closed':
      return 'done';
    case 'open':
    default:
      return node.ready ? 'ready' : 'open';
  }
}

function byPriorityThenId(a: BeadNode, b: BeadNode): number {
  const pa = a.bead.priority ?? Number.POSITIVE_INFINITY;
  const pb = b.bead.priority ?? Number.POSITIVE_INFINITY;
  if (pa !== pb) return pa - pb;
  return a.bead.id < b.bead.id ? -1 : a.bead.id > b.bead.id ? 1 : 0;
}

export function buildBeadGraph(beads: readonly SupervisorBead[]): BeadGraph {
  const byId = new Map<string, SupervisorBead>();
  for (const b of beads) byId.set(b.id, b);

  const blocksOf = new Map<string, SupervisorBead[]>();
  const nodes = new Map<string, BeadNode>();

  // First pass: resolve upstream edges and readiness; accumulate inverse edges.
  for (const b of beads) {
    const deps: BeadDepEdge[] = depEntries(b).map(({ id, kind }) => ({
      id,
      kind,
      bead: byId.get(id) ?? null,
    }));
    const hasUnresolvedDeps = deps.some((d) => d.bead === null);

    const needs = blockingNeedIds(b);
    const ready = b.status === 'open' && needs.every((id) => byId.get(id)?.status === 'closed');

    const node: BeadNode = {
      bead: b,
      deps,
      blocks: [],
      ready,
      hasUnresolvedDeps,
      column: 'open',
    };
    node.column = columnFor(node);
    nodes.set(b.id, node);

    for (const dep of deps) {
      if (dep.bead === null) continue;
      const list = blocksOf.get(dep.id);
      if (list) list.push(b);
      else blocksOf.set(dep.id, [b]);
    }
  }

  // Second pass: attach inverse edges (sorted for deterministic render).
  for (const [id, blockers] of blocksOf) {
    const node = nodes.get(id);
    if (node) {
      node.blocks = [...blockers].sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
    }
  }

  const columns = emptyColumns();
  for (const node of nodes.values()) columns[node.column].push(node);
  for (const col of BOARD_COLUMNS) columns[col.id].sort(byPriorityThenId);

  return { nodes, columns };
}

function emptyColumns(): Record<BoardColumnId, BeadNode[]> {
  return {
    ready: [],
    open: [],
    in_progress: [],
    blocked: [],
    done: [],
  };
}

/**
 * Project the graph's columns down to just the beads in `ids`, preserving
 * each column's existing order. Lets the page render one board per rig from
 * a single shared graph — so cross-rig dependency edges still resolve
 * (the graph is built over all beads), while display is grouped by rig.
 */
export function selectColumns(
  graph: BeadGraph,
  ids: ReadonlySet<string>,
): Record<BoardColumnId, BeadNode[]> {
  const out = emptyColumns();
  for (const col of BOARD_COLUMNS) {
    out[col.id] = graph.columns[col.id].filter((n) => ids.has(n.bead.id));
  }
  return out;
}

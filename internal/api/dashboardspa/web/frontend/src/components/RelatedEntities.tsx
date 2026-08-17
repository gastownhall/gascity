import { useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import type { EntityLinkView, LinkNode, LinkNodeType } from 'gas-city-dashboard-shared';
import { formatRelative } from '../hooks/time';

// Reusable "Related" section (R5). One component woven into the foot of
// AgentDetail, FormulaRunDetail, and BeadDetailModal.
//
// Pure typography in the established register (DESIGN.md): a tracked
// RELATED label + a one-line summary, rows grouped by entity kind, each
// row a link/button opening the adjacent surface. No card, chip, or
// left-stripe.
//
// Density discipline (RK3): the header is a single summary line
// (`12 resolved, 3 unresolved, 2 candidates`); detail is behind an
// expand; rows are capped per group with a typeset `+ N more`. At most
// ONE aggregate section-level maroon — never per-row — sanctioned by
// DESIGN.md §2 only when the unresolved count crosses a threshold.
//
// Staleness (R7 / RK2): a node older than its source band renders dimmed
// with its own inline age, so a 23h-old PR looks different from a
// 60s-fresh bead at the row level.

const MAX_ROWS_PER_GROUP = 6;
// A node older than this (ms) is rendered as visibly stale. GitHub-sourced
// nodes can be up to 24h old; supervisor nodes refresh every ~60s. 1h is a
// calm middle ground that flags a genuinely old contributing source.
const STALE_AFTER_MS = 60 * 60 * 1000;
// Aggregate maroon fires when unresolved links cross this count — the
// operator's eye should land on "this view has a lot of dangling links".
const UNRESOLVED_MARK_THRESHOLD = 3;

const GROUP_ORDER: LinkNodeType[] = [
  'bead',
  'formula_run',
  'session',
  'github_pr',
  'github_issue',
  'order_run',
];

const GROUP_LABEL: Record<LinkNodeType, string> = {
  bead: 'Beads',
  session: 'Sessions',
  github_pr: 'Pull requests',
  github_issue: 'Issues',
  formula_run: 'Formula runs',
  order_run: 'Order runs',
};

export interface RelatedEntitiesProps {
  view: EntityLinkView | null;
  loading: boolean;
  error: string | null;
  now: number;
  /** Open the bead modal for a related bead. */
  onOpenBead?: (beadId: string) => void;
}

interface GroupRow {
  node: LinkNode;
  relation: string;
}

export function RelatedEntities({ view, loading, error, now, onOpenBead }: RelatedEntitiesProps) {
  const [expanded, setExpanded] = useState(false);

  const groups = useMemo(() => buildGroups(view), [view]);
  const counts = useMemo(() => summarize(view), [view]);

  const showMark = counts.unresolved >= UNRESOLVED_MARK_THRESHOLD;

  return (
    <section className="mt-12">
      <header className="flex items-baseline justify-between mb-4 gap-3">
        <h2 className="text-label uppercase tracking-wider text-fg-faint">Related</h2>
        <div className="flex items-baseline gap-3 min-w-0">
          {view && view.asOf && (
            <span className="text-label uppercase tracking-wider text-fg-faint tnum shrink-0">
              as of {formatRelative(view.asOf, now)}
            </span>
          )}
          <SummaryLine loading={loading} counts={counts} showMark={showMark} />
        </div>
      </header>

      {error !== null ? (
        <p className="text-body text-accent" role="alert">
          {error}
        </p>
      ) : loading && view === null ? (
        <p className="text-body text-fg-muted italic">Loading related entities.</p>
      ) : view === null || groups.length === 0 ? (
        <p className="text-body text-fg-muted italic">No related entities.</p>
      ) : (
        <>
          {view.partial && (
            <p className="text-label uppercase tracking-wider text-warn mb-4" role="status">
              Partial: some sources did not load. Links may be incomplete.
            </p>
          )}
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="text-label uppercase tracking-wider text-fg-faint hover:text-fg focus-mark mb-4"
            aria-expanded={expanded}
          >
            {expanded ? 'Hide detail' : 'Show detail'}
          </button>
          {expanded && (
            <div className="space-y-8">
              {groups.map((group) => (
                <RelatedGroup
                  key={group.type}
                  type={group.type}
                  rows={group.rows}
                  now={now}
                  {...(onOpenBead !== undefined ? { onOpenBead } : {})}
                />
              ))}
            </div>
          )}
        </>
      )}
    </section>
  );
}

function SummaryLine({
  loading,
  counts,
  showMark,
}: {
  loading: boolean;
  counts: Summary;
  showMark: boolean;
}) {
  if (loading) {
    return <span className="text-label uppercase tracking-wider text-fg-faint">·</span>;
  }
  const parts: string[] = [];
  if (counts.resolved > 0) parts.push(`${counts.resolved} resolved`);
  if (counts.unresolved > 0) parts.push(`${counts.unresolved} unresolved`);
  if (counts.candidates > 0) parts.push(`${counts.candidates} candidates`);
  const text = parts.length > 0 ? parts.join(', ') : 'none';
  // The single aggregate maroon mark (One Mark Rule): the unresolved count
  // crossing the threshold is the one loud moment in this section.
  return (
    <span
      className={`text-label uppercase tracking-wider tnum truncate ${
        showMark ? 'text-accent' : 'text-fg-faint'
      }`}
    >
      {showMark && <span aria-hidden>■ </span>}
      {text}
    </span>
  );
}

function RelatedGroup({
  type,
  rows,
  now,
  onOpenBead,
}: {
  type: LinkNodeType;
  rows: GroupRow[];
  now: number;
  onOpenBead?: (beadId: string) => void;
}) {
  const visible = rows.slice(0, MAX_ROWS_PER_GROUP);
  const overflow = rows.length - visible.length;
  return (
    <div>
      <header className="flex items-baseline justify-between mb-2">
        <h3 className="text-label uppercase tracking-wider text-fg-faint">{GROUP_LABEL[type]}</h3>
        <span className="text-label uppercase tracking-wider text-fg-faint tnum">
          {rows.length}
        </span>
      </header>
      <ul className="space-y-2">
        {visible.map((row) => (
          <RelatedRow
            key={`${row.relation}\u0000${row.node.key}`}
            row={row}
            now={now}
            {...(onOpenBead !== undefined ? { onOpenBead } : {})}
          />
        ))}
      </ul>
      {overflow > 0 && (
        <p className="text-label uppercase tracking-wider text-fg-faint mt-2">+ {overflow} more</p>
      )}
    </div>
  );
}

function RelatedRow({
  row,
  now,
  onOpenBead,
}: {
  row: GroupRow;
  now: number;
  onOpenBead?: (beadId: string) => void;
}) {
  const { node, relation } = row;
  const stale = isStale(node.fetchedAt, now);
  const label = node.title ?? node.ref;
  const dimmed = node.unresolved || stale;

  return (
    <li className="flex items-baseline gap-3 min-w-0">
      <span className="text-label uppercase tracking-wider text-fg-faint shrink-0 w-20 truncate">
        {relation}
      </span>
      <span className="min-w-0 flex-1 truncate">
        <NodeLink
          node={node}
          label={label}
          dimmed={dimmed}
          {...(onOpenBead !== undefined ? { onOpenBead } : {})}
        />
      </span>
      <span className="text-label uppercase tracking-wider text-fg-faint tnum shrink-0">
        {node.unresolved
          ? unresolvedLabel(node)
          : node.fetchedAt
            ? formatRelative(node.fetchedAt, now)
            : (node.status ?? '·')}
      </span>
    </li>
  );
}

function NodeLink({
  node,
  label,
  dimmed,
  onOpenBead,
}: {
  node: LinkNode;
  label: string;
  dimmed: boolean;
  onOpenBead?: (beadId: string) => void;
}) {
  const className = `text-body text-left truncate min-w-0 focus-mark ${
    dimmed ? 'text-fg-muted' : 'text-fg hover:text-accent'
  }`;

  // Beads open the modal in place; sessions/runs route; unresolved
  // GitHub entities surface an outbound ↗ to the sanitised url when present.
  if (node.type === 'bead' && !node.unresolved && onOpenBead) {
    return (
      <button
        type="button"
        onClick={() => onOpenBead(node.ref)}
        className={className}
        title={`Open ${node.ref}`}
      >
        {label}
      </button>
    );
  }
  if (node.type === 'session' && !node.unresolved) {
    return (
      <Link to={`/agents/${encodeURIComponent(node.ref)}`} className={className}>
        {label}
      </Link>
    );
  }
  if (node.url) {
    return (
      <a
        href={node.url}
        target="_blank"
        rel="noreferrer noopener"
        className={className}
        title={node.url}
      >
        {label} <span aria-hidden>↗</span>
      </a>
    );
  }
  return <span className={className}>{label}</span>;
}

function unresolvedLabel(node: LinkNode): string {
  if (node.candidateCount !== undefined && node.candidateCount > 1) {
    return `${node.candidateCount} candidates`;
  }
  return 'unresolved';
}

interface Summary {
  resolved: number;
  unresolved: number;
  candidates: number;
}

function summarize(view: EntityLinkView | null): Summary {
  const out: Summary = { resolved: 0, unresolved: 0, candidates: 0 };
  if (view === null) return out;
  for (const node of view.nodes) {
    if (node.key === view.focus.key) continue;
    if (node.candidateCount !== undefined && node.candidateCount > 1) {
      out.candidates += 1;
    } else if (node.unresolved) {
      out.unresolved += 1;
    } else {
      out.resolved += 1;
    }
  }
  return out;
}

interface RenderGroup {
  type: LinkNodeType;
  rows: GroupRow[];
}

function buildGroups(view: EntityLinkView | null): RenderGroup[] {
  if (view === null) return [];
  const nodeByKey = new Map<string, LinkNode>();
  for (const node of view.nodes) nodeByKey.set(node.key, node);

  const rowsByType = new Map<LinkNodeType, GroupRow[]>();
  for (const edge of view.edges) {
    if (edge.from !== view.focus.key) continue;
    const node = nodeByKey.get(edge.to);
    if (node === undefined) continue;
    const list = rowsByType.get(node.type) ?? [];
    list.push({ node, relation: edge.relation });
    rowsByType.set(node.type, list);
  }

  const groups: RenderGroup[] = [];
  for (const type of GROUP_ORDER) {
    const rows = rowsByType.get(type);
    if (rows && rows.length > 0) {
      // Resolved rows first within a group; unresolved drop to the bottom.
      rows.sort((a, b) => Number(a.node.unresolved) - Number(b.node.unresolved));
      groups.push({ type, rows });
    }
  }
  return groups;
}

// Exported for the row-level staleness test (RK2).
export function isStale(fetchedAt: string | null, now: number): boolean {
  if (fetchedAt === null) return false;
  const ms = Date.parse(fetchedAt);
  if (!Number.isFinite(ms)) return false;
  return now - ms > STALE_AFTER_MS;
}

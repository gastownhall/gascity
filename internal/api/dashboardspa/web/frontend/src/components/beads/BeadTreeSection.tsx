import { useEffect, useMemo, useRef } from 'react';
import type { BadgeSeverity } from '../../attention/compose';
import { attentionListItemProps } from '../../attention/routeHighlight';
import type { SupervisorBead } from '../../supervisor/beadReads';

export interface BeadTreeNode {
  bead: SupervisorBead;
  children: BeadTreeNode[];
  orphaned: boolean;
  contextOnly: boolean;
}

interface MutableBeadTreeNode {
  bead: SupervisorBead;
  children: MutableBeadTreeNode[];
  orphaned: boolean;
}

const TYPE_ORDER = new Map<string, number>([
  ['epic', 0],
  ['feature', 1],
  ['task', 2],
  ['bug', 3],
  ['chore', 4],
  ['decision', 5],
]);

function byTypePriorityThenId(a: { bead: SupervisorBead }, b: { bead: SupervisorBead }): number {
  const typeA = TYPE_ORDER.get(a.bead.issue_type) ?? Number.POSITIVE_INFINITY;
  const typeB = TYPE_ORDER.get(b.bead.issue_type) ?? Number.POSITIVE_INFINITY;
  if (typeA !== typeB) return typeA - typeB;
  const priorityA = a.bead.priority ?? Number.POSITIVE_INFINITY;
  const priorityB = b.bead.priority ?? Number.POSITIVE_INFINITY;
  if (priorityA !== priorityB) return priorityA - priorityB;
  return a.bead.id.localeCompare(b.bead.id);
}

function createsParentCycle(
  beadId: string,
  parentId: string,
  byId: ReadonlyMap<string, MutableBeadTreeNode>,
): boolean {
  const visited = new Set<string>([beadId]);
  let current: string | undefined = parentId;
  while (current !== undefined) {
    if (visited.has(current)) return true;
    visited.add(current);
    const parent = byId.get(current);
    const next = parent?.bead.parent?.trim();
    current = next && byId.has(next) ? next : undefined;
  }
  return false;
}

export function buildVisibleBeadTree(
  beads: readonly SupervisorBead[],
  visibleIds?: ReadonlySet<string>,
): BeadTreeNode[] {
  const byId = new Map<string, MutableBeadTreeNode>();
  for (const bead of beads) {
    if (!byId.has(bead.id)) byId.set(bead.id, { bead, children: [], orphaned: false });
  }

  const roots: MutableBeadTreeNode[] = [];
  for (const node of byId.values()) {
    const parentId = node.bead.parent?.trim();
    if (!parentId) {
      roots.push(node);
      continue;
    }
    const parent = byId.get(parentId);
    if (!parent || parent === node || createsParentCycle(node.bead.id, parentId, byId)) {
      node.orphaned = true;
      roots.push(node);
      continue;
    }
    parent.children.push(node);
  }

  const sortRecursively = (nodes: MutableBeadTreeNode[]) => {
    nodes.sort(byTypePriorityThenId);
    for (const node of nodes) sortRecursively(node.children);
  };
  sortRecursively(roots);

  const prune = (node: MutableBeadTreeNode): BeadTreeNode | null => {
    const children = node.children
      .map(prune)
      .filter((child): child is BeadTreeNode => child !== null);
    const directlyVisible = visibleIds === undefined || visibleIds.has(node.bead.id);
    if (!directlyVisible && children.length === 0) return null;
    return {
      bead: node.bead,
      children,
      orphaned: node.orphaned,
      contextOnly: !directlyVisible,
    };
  };

  return roots.map(prune).filter((node): node is BeadTreeNode => node !== null);
}

export function expandableBeadIds(nodes: readonly BeadTreeNode[]): string[] {
  const ids: string[] = [];
  const visit = (node: BeadTreeNode) => {
    if (node.children.length > 0) ids.push(node.bead.id);
    for (const child of node.children) visit(child);
  };
  for (const node of nodes) visit(node);
  return ids;
}

interface BeadTreeSectionProps {
  label: string;
  count: number;
  rows: readonly SupervisorBead[];
  visibleIds: ReadonlySet<string>;
  expandedIds: ReadonlySet<string>;
  collapsed: boolean;
  forceExpanded?: boolean;
  onToggleProject: () => void;
  onToggleExpanded: (beadId: string) => void;
  onExpandAll: (beadIds: readonly string[]) => void;
  onCollapseAll: (beadIds: readonly string[]) => void;
  selectedId: string | null;
  attentionSeverity?: ((beadId: string) => BadgeSeverity | null) | undefined;
  onSelect: (beadId: string) => void;
}

export function BeadTreeSection({
  label,
  count,
  rows,
  visibleIds,
  expandedIds,
  collapsed,
  forceExpanded = false,
  onToggleProject,
  onToggleExpanded,
  onExpandAll,
  onCollapseAll,
  selectedId,
  attentionSeverity,
  onSelect,
}: BeadTreeSectionProps) {
  const roots = useMemo(() => buildVisibleBeadTree(rows, visibleIds), [rows, visibleIds]);
  const expandableIds = useMemo(() => expandableBeadIds(roots), [roots]);
  const hierarchyRoots = roots.filter(
    (node) =>
      node.children.length > 0 ||
      node.bead.issue_type === 'epic' ||
      node.bead.issue_type === 'feature',
  );
  const looseRoots = roots.filter((node) => !hierarchyRoots.includes(node));
  const allExpanded =
    expandableIds.length > 0 && expandableIds.every((beadId) => expandedIds.has(beadId));

  return (
    <section aria-label={label}>
      <header className="flex flex-wrap items-baseline justify-between gap-3 border-b border-rule pb-2 mb-3">
        <button
          type="button"
          aria-expanded={!collapsed}
          onClick={onToggleProject}
          className="flex items-baseline gap-2 text-left focus-mark rounded-sm"
        >
          <span className="text-fg-faint" aria-hidden="true">
            {collapsed ? '›' : '⌄'}
          </span>
          <h2 className="text-headline text-fg">{label}</h2>
          <span className="text-label tnum text-fg-muted">{count}</span>
        </button>
        {!collapsed && expandableIds.length > 0 && (
          <button
            type="button"
            onClick={() =>
              allExpanded ? onCollapseAll(expandableIds) : onExpandAll(expandableIds)
            }
            className="text-label uppercase tracking-wider text-fg-faint hover:text-fg focus-mark rounded-sm"
          >
            {allExpanded ? 'Collapse all' : 'Expand all'}
          </button>
        )}
      </header>

      {!collapsed && (
        <div className="space-y-5">
          {hierarchyRoots.length > 0 && (
            <ul role="tree" aria-label={`${label} bead hierarchy`} className="space-y-0.5">
              {hierarchyRoots.map((node) => (
                <BeadTreeRow
                  key={node.bead.id}
                  node={node}
                  depth={0}
                  parentType={null}
                  expandedIds={expandedIds}
                  forceExpanded={forceExpanded}
                  selectedId={selectedId}
                  attentionSeverity={attentionSeverity}
                  onToggleExpanded={onToggleExpanded}
                  onSelect={onSelect}
                />
              ))}
            </ul>
          )}

          {looseRoots.length > 0 && (
            <div>
              <h3 className="mb-2 text-label uppercase tracking-wider text-fg-muted">
                Loose beads <span className="tnum text-fg-faint">({looseRoots.length})</span>
              </h3>
              <ul role="tree" aria-label={`${label} loose beads`} className="space-y-0.5">
                {looseRoots.map((node) => (
                  <BeadTreeRow
                    key={node.bead.id}
                    node={node}
                    depth={0}
                    parentType={null}
                    expandedIds={expandedIds}
                    forceExpanded={forceExpanded}
                    selectedId={selectedId}
                    attentionSeverity={attentionSeverity}
                    onToggleExpanded={onToggleExpanded}
                    onSelect={onSelect}
                  />
                ))}
              </ul>
            </div>
          )}
        </div>
      )}
    </section>
  );
}

interface BeadTreeRowProps {
  node: BeadTreeNode;
  depth: number;
  parentType: string | null;
  expandedIds: ReadonlySet<string>;
  forceExpanded: boolean;
  selectedId: string | null;
  attentionSeverity?: ((beadId: string) => BadgeSeverity | null) | undefined;
  onToggleExpanded: (beadId: string) => void;
  onSelect: (beadId: string) => void;
}

function BeadTreeRow({
  node,
  depth,
  parentType,
  expandedIds,
  forceExpanded,
  selectedId,
  attentionSeverity,
  onToggleExpanded,
  onSelect,
}: BeadTreeRowProps) {
  const selected = selectedId === node.bead.id;
  const hasChildren = node.children.length > 0;
  const expanded = hasChildren && (forceExpanded || expandedIds.has(node.bead.id));
  const rowRef = useRef<HTMLLIElement | null>(null);
  const { className: attentionClassName = '', ...attentionProps } = attentionListItemProps(
    attentionSeverity?.(node.bead.id) ?? null,
  );

  useEffect(() => {
    if (!selected) return;
    rowRef.current?.scrollIntoView?.({ block: 'center', inline: 'nearest' });
  }, [selected]);

  const typeLabel =
    node.bead.issue_type === 'task' && parentType === 'task' ? 'subtask' : node.bead.issue_type;
  const indent = Math.min(depth, 5) * 20;

  return (
    <li
      ref={rowRef}
      role="treeitem"
      aria-level={depth + 1}
      aria-expanded={hasChildren ? expanded : undefined}
      {...attentionProps}
    >
      <div
        className={`group flex min-w-0 items-start gap-2 rounded-sm py-2 pr-2 transition-colors duration-150 ease-out-quart ${
          selected ? 'bg-surface-tint' : 'hover:bg-surface-tint/60'
        } ${node.contextOnly ? 'text-fg-faint' : ''} ${attentionClassName}`}
        style={{ paddingLeft: `${indent}px` }}
      >
        <span className="flex h-6 w-5 shrink-0 items-center justify-center">
          {hasChildren ? (
            <button
              type="button"
              title={`${expanded ? 'Collapse' : 'Expand'} ${node.bead.id}`}
              aria-label={`${expanded ? 'Collapse' : 'Expand'} ${node.bead.id}`}
              onClick={() => onToggleExpanded(node.bead.id)}
              className="h-6 w-5 text-fg-faint hover:text-fg focus-mark rounded-sm"
            >
              {expanded ? '⌄' : '›'}
            </button>
          ) : (
            <span aria-hidden="true">·</span>
          )}
        </span>

        <button
          type="button"
          title={`Select ${node.bead.id}`}
          aria-pressed={selected}
          onClick={() => onSelect(node.bead.id)}
          className="grid min-w-0 flex-1 grid-cols-[minmax(7rem,auto)_minmax(5rem,auto)_minmax(0,1fr)] items-baseline gap-x-3 gap-y-0.5 text-left focus-mark rounded-sm md:grid-cols-[minmax(7rem,auto)_minmax(5rem,auto)_minmax(0,1fr)_auto_auto_auto]"
        >
          <span className="tnum text-label text-fg-muted">{node.bead.id}</span>
          <span className="text-label uppercase tracking-wider text-fg-faint">{typeLabel}</span>
          <span className={`min-w-0 text-body text-fg ${selected ? 'font-medium' : ''}`}>
            {node.bead.title}
          </span>
          <span className="text-label uppercase tracking-wider text-fg-muted">
            {node.bead.status.replaceAll('_', ' ')}
          </span>
          <span className="text-label tnum text-fg-faint">
            {node.bead.priority == null ? '' : `P${node.bead.priority}`}
          </span>
          <span className="truncate text-label text-fg-faint">{node.bead.assignee ?? ''}</span>
          {(node.orphaned || node.contextOnly) && (
            <span className="col-span-full text-label normal-case tracking-normal text-warn">
              {node.orphaned ? 'parent unavailable' : 'context'}
            </span>
          )}
        </button>
      </div>

      {expanded && (
        <ul role="group">
          {node.children.map((child) => (
            <BeadTreeRow
              key={child.bead.id}
              node={child}
              depth={depth + 1}
              parentType={node.bead.issue_type}
              expandedIds={expandedIds}
              forceExpanded={forceExpanded}
              selectedId={selectedId}
              attentionSeverity={attentionSeverity}
              onToggleExpanded={onToggleExpanded}
              onSelect={onSelect}
            />
          ))}
        </ul>
      )}
    </li>
  );
}

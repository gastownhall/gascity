import type { HTMLAttributes, ReactNode } from 'react';
import { ProjectGroupHeader } from './ProjectGroupHeader';
import { Table, type SortState, type TableColumn } from './Table';
import type { ProjectGroup } from '../hooks/useListFilters';

// GroupedTable renders one Table per project group with a header
// disclosure, or a single italic empty-state line. Keeps the three
// list routes (Beads / Agents / Mail) DRY around the same pattern.

interface GroupedTableProps<T> {
  groups: ReadonlyArray<ProjectGroup<T>>;
  columns: ReadonlyArray<TableColumn<T>>;
  rowKey: (row: T) => string;
  onToggleProject: (project: string) => void;
  /** Optional per-row click handler (used by Mail to open thread). */
  onRowClick?: (row: T) => void;
  rowProps?: (row: T) => HTMLAttributes<HTMLTableRowElement>;
  /** Italic line shown when there are no groups at all. */
  emptyMessage: ReactNode;
  /** Per-project empty (rare: filters return zero rows for a project). */
  perProjectEmpty?: ReactNode;
  initialSort?: SortState | null;
}

export function GroupedTable<T>({
  groups,
  columns,
  rowKey,
  onToggleProject,
  onRowClick,
  rowProps,
  emptyMessage,
  perProjectEmpty,
  initialSort,
}: GroupedTableProps<T>) {
  if (groups.length === 0) {
    return <p className="py-10 text-center text-fg-muted italic">{emptyMessage}</p>;
  }

  return (
    <div className="space-y-8">
      {groups.map((group) => (
        <section key={group.projectKey}>
          <ProjectGroupHeader
            project={group.project}
            count={group.totalInProject}
            collapsed={group.collapsed}
            onToggle={() => onToggleProject(group.projectKey)}
            collapsible={group.collapsible}
          />
          {!group.collapsed && (
            <Table
              columns={columns}
              rows={group.rows}
              rowKey={rowKey}
              empty={perProjectEmpty ?? 'No items.'}
              {...(onRowClick !== undefined ? { onRowClick } : {})}
              {...(rowProps !== undefined ? { rowProps } : {})}
              {...(initialSort !== undefined ? { initialSort } : {})}
            />
          )}
        </section>
      ))}
    </div>
  );
}

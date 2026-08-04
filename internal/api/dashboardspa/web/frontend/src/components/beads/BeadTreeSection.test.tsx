import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { SupervisorBead } from '../../supervisor/beadReads';
import { BeadTreeSection, buildVisibleBeadTree } from './BeadTreeSection';

afterEach(() => cleanup());

function bead(id: string, extra: Partial<SupervisorBead> = {}): SupervisorBead {
  return {
    id,
    title: id,
    status: 'open',
    issue_type: 'task',
    created_at: '2026-08-04T00:00:00Z',
    ...extra,
  };
}

describe('buildVisibleBeadTree', () => {
  it('builds project → epic → task → subtask nesting from parent ids', () => {
    const roots = buildVisibleBeadTree([
      bead('P-1', { issue_type: 'epic' }),
      bead('P-1.1', { parent: 'P-1' }),
      bead('P-1.1.1', { parent: 'P-1.1' }),
      bead('P-1.1.1.1', { parent: 'P-1.1.1' }),
    ]);

    expect(roots).toHaveLength(1);
    expect(roots[0]?.bead.id).toBe('P-1');
    expect(roots[0]?.children[0]?.bead.id).toBe('P-1.1');
    expect(roots[0]?.children[0]?.children[0]?.bead.id).toBe('P-1.1.1');
    expect(roots[0]?.children[0]?.children[0]?.children[0]?.bead.id).toBe('P-1.1.1.1');
  });

  it('keeps ancestors as context when only a nested subtask matches', () => {
    const rows = [
      bead('P-1', { issue_type: 'epic' }),
      bead('P-1.1', { parent: 'P-1' }),
      bead('P-1.1.1', { parent: 'P-1.1' }),
      bead('P-2', { issue_type: 'epic' }),
    ];

    const roots = buildVisibleBeadTree(rows, new Set(['P-1.1.1']));

    expect(roots.map((node) => node.bead.id)).toEqual(['P-1']);
    expect(roots[0]?.contextOnly).toBe(true);
    expect(roots[0]?.children[0]?.contextOnly).toBe(true);
    expect(roots[0]?.children[0]?.children[0]?.contextOnly).toBe(false);
  });

  it('keeps missing-parent and cyclic beads visible as orphaned roots', () => {
    const roots = buildVisibleBeadTree([
      bead('ORPHAN', { parent: 'MISSING' }),
      bead('A', { parent: 'B' }),
      bead('B', { parent: 'A' }),
    ]);

    expect(roots.map((node) => node.bead.id).sort()).toEqual(['A', 'B', 'ORPHAN']);
    expect(roots.every((node) => node.orphaned)).toBe(true);
  });
});

describe('BeadTreeSection', () => {
  it('expands epics, tasks, subtasks, and nested subtasks independently', () => {
    const onToggleExpanded = vi.fn();
    render(
      <BeadTreeSection
        label="project"
        count={4}
        rows={[
          bead('P-1', { title: 'Epic', issue_type: 'epic' }),
          bead('P-1.1', { title: 'Task', parent: 'P-1' }),
          bead('P-1.1.1', { title: 'Subtask', parent: 'P-1.1' }),
          bead('P-1.1.1.1', { title: 'Nested subtask', parent: 'P-1.1.1' }),
        ]}
        visibleIds={new Set(['P-1', 'P-1.1', 'P-1.1.1', 'P-1.1.1.1'])}
        expandedIds={new Set(['P-1', 'P-1.1'])}
        collapsed={false}
        onToggleProject={vi.fn()}
        onToggleExpanded={onToggleExpanded}
        onExpandAll={vi.fn()}
        onCollapseAll={vi.fn()}
        selectedId={null}
        onSelect={vi.fn()}
      />,
    );

    const tree = screen.getByRole('tree', { name: /project bead hierarchy/i });
    expect(within(tree).getByText('Epic')).toBeTruthy();
    expect(within(tree).getByText('Task')).toBeTruthy();
    expect(within(tree).getByText('Subtask')).toBeTruthy();
    expect(within(tree).queryByText('Nested subtask')).toBeNull();
    expect(within(tree).getByText('subtask')).toBeTruthy();

    const epicTreeItem = screen.getByTitle('Collapse P-1').closest('[role="treeitem"]');
    const taskTreeItem = screen.getByTitle('Collapse P-1.1').closest('[role="treeitem"]');
    const subtaskTreeItem = screen.getByTitle('Expand P-1.1.1').closest('[role="treeitem"]');
    expect(epicTreeItem?.contains(taskTreeItem)).toBe(true);
    expect(taskTreeItem?.contains(subtaskTreeItem)).toBe(true);

    fireEvent.click(screen.getByTitle('Expand P-1.1.1'));
    expect(onToggleExpanded).toHaveBeenCalledWith('P-1.1.1');
  });

  it('renders leaf roots under Loose beads and marks unavailable parents', () => {
    render(
      <BeadTreeSection
        label="project"
        count={2}
        rows={[bead('LOOSE'), bead('ORPHAN', { parent: 'MISSING' })]}
        visibleIds={new Set(['LOOSE', 'ORPHAN'])}
        expandedIds={new Set()}
        collapsed={false}
        onToggleProject={vi.fn()}
        onToggleExpanded={vi.fn()}
        onExpandAll={vi.fn()}
        onCollapseAll={vi.fn()}
        selectedId={null}
        onSelect={vi.fn()}
      />,
    );

    expect(screen.getByText(/loose beads/i)).toBeTruthy();
    expect(screen.getByText('parent unavailable')).toBeTruthy();
  });
});

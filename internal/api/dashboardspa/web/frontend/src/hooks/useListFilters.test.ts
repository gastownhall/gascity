import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { useListFilters, type FilterChip } from './useListFilters';

interface Row {
  id: string;
  title: string;
  status: 'open' | 'closed';
  project: string;
}

const rows: Row[] = [
  { id: 'gc-1', title: 'fix peek modal', status: 'open', project: 'gc' },
  { id: 'gc-2', title: 'rollup release', status: 'closed', project: 'gc' },
  { id: 'codeprobe-a', title: 'parse pdb', status: 'open', project: 'codeprobe' },
  { id: 'codeprobe-b', title: 'fix peek', status: 'closed', project: 'codeprobe' },
  { id: 'agent-diag-1', title: 'classifier polish', status: 'open', project: 'agent-diagnostics' },
];

const chips: FilterChip<Row>[] = [
  { id: 'open', label: 'open', match: (r) => r.status === 'open' },
  { id: 'closed', label: 'closed', match: (r) => r.status === 'closed' },
];

const projectOf = (r: Row) => r.project;
const searchOf = (r: Row) => [r.id, r.title];

beforeEach(() => {
  window.localStorage.clear();
});

afterEach(() => {
  window.localStorage.clear();
});

describe('useListFilters', () => {
  it('returns all rows grouped by project when no search or chip is active', () => {
    const { result } = renderHook(() =>
      useListFilters({ viewKey: 'beads', rows, projectOf, searchOf, chips }),
    );

    expect(result.current.totalMatches).toBe(5);
    expect(result.current.groups.map((g) => g.project)).toEqual([
      'agent-diagnostics',
      'codeprobe',
      'gc',
    ]);
    const gcGroup = result.current.groups.find((g) => g.project === 'gc');
    expect(gcGroup?.rows).toHaveLength(2);
    expect(gcGroup?.totalInProject).toBe(2);
  });

  it('filters by case-insensitive substring across all searchable fields', () => {
    const { result } = renderHook(() =>
      useListFilters({ viewKey: 'beads', rows, projectOf, searchOf, chips }),
    );

    act(() => result.current.setSearch('PEEK'));
    expect(result.current.totalMatches).toBe(2);
    const ids = result.current.groups.flatMap((g) => g.rows.map((r) => r.id));
    expect(ids.sort()).toEqual(['codeprobe-b', 'gc-1']);
  });

  it('matches across id and title independently', () => {
    const { result } = renderHook(() =>
      useListFilters({ viewKey: 'beads', rows, projectOf, searchOf, chips }),
    );

    act(() => result.current.setSearch('gc-'));
    expect(result.current.totalMatches).toBe(2);

    act(() => result.current.setSearch('classifier'));
    expect(result.current.totalMatches).toBe(1);
    expect(result.current.groups[0]?.rows[0]?.id).toBe('agent-diag-1');
  });

  it('filters by a single active chip', () => {
    const { result } = renderHook(() =>
      useListFilters({ viewKey: 'beads', rows, projectOf, searchOf, chips }),
    );

    act(() => result.current.toggleChip('open'));
    expect(result.current.totalMatches).toBe(3);
    const statuses = result.current.groups.flatMap((g) => g.rows.map((r) => r.status));
    expect(statuses.every((s) => s === 'open')).toBe(true);
  });

  it('treats multiple active chips as a union (OR) so the operator can broaden, not narrow', () => {
    const { result } = renderHook(() =>
      useListFilters({ viewKey: 'beads', rows, projectOf, searchOf, chips }),
    );

    act(() => {
      result.current.toggleChip('open');
      result.current.toggleChip('closed');
    });
    expect(result.current.totalMatches).toBe(5);
  });

  it('combines search and chips as AND', () => {
    const { result } = renderHook(() =>
      useListFilters({ viewKey: 'beads', rows, projectOf, searchOf, chips }),
    );

    act(() => {
      result.current.setSearch('peek');
      result.current.toggleChip('open');
    });
    expect(result.current.totalMatches).toBe(1);
    expect(result.current.groups[0]?.rows[0]?.id).toBe('gc-1');
  });

  it('persists collapsed project ids in localStorage under the view key', () => {
    const { result, rerender } = renderHook(() =>
      useListFilters({ viewKey: 'beads', rows, projectOf, searchOf, chips }),
    );

    act(() => result.current.toggleProject('gc'));
    expect(result.current.isCollapsed('gc')).toBe(true);

    // Collapsed groups still report rows count in the header, but the
    // consumer is expected to skip rendering the rows themselves.
    const gcGroup = result.current.groups.find((g) => g.project === 'gc');
    expect(gcGroup?.collapsed).toBe(true);

    rerender();
    // Re-mount with same key should restore.
    const { result: fresh } = renderHook(() =>
      useListFilters({ viewKey: 'beads', rows, projectOf, searchOf, chips }),
    );
    expect(fresh.current.isCollapsed('gc')).toBe(true);
    expect(fresh.current.isCollapsed('codeprobe')).toBe(false);
  });

  it('keeps collapse state per view key (beads collapse does not affect mail)', () => {
    const { result: beads } = renderHook(() =>
      useListFilters({ viewKey: 'beads', rows, projectOf, searchOf, chips }),
    );
    act(() => beads.current.toggleProject('gc'));

    const { result: mail } = renderHook(() =>
      useListFilters({ viewKey: 'mail', rows, projectOf, searchOf, chips }),
    );
    expect(mail.current.isCollapsed('gc')).toBe(false);
  });

  it('toggles a chip off when called twice', () => {
    const { result } = renderHook(() =>
      useListFilters({ viewKey: 'beads', rows, projectOf, searchOf, chips }),
    );

    act(() => result.current.toggleChip('open'));
    expect(result.current.activeChipIds.has('open')).toBe(true);

    act(() => result.current.toggleChip('open'));
    expect(result.current.activeChipIds.has('open')).toBe(false);
    expect(result.current.totalMatches).toBe(5);
  });

  it('returns empty groups (preserving project headers if collapsed state was persisted) when search has no matches', () => {
    const { result } = renderHook(() =>
      useListFilters({ viewKey: 'beads', rows, projectOf, searchOf, chips }),
    );

    act(() => result.current.setSearch('xxxxnomatch'));
    expect(result.current.totalMatches).toBe(0);
    expect(result.current.groups).toEqual([]);
  });

  it('preserves input row order within each project group', () => {
    const { result } = renderHook(() =>
      useListFilters({ viewKey: 'beads', rows, projectOf, searchOf, chips }),
    );

    const gcGroup = result.current.groups.find((g) => g.project === 'gc');
    expect(gcGroup?.rows.map((r) => r.id)).toEqual(['gc-1', 'gc-2']);
  });

  it('seeds activeChipIds from initialActiveChipIds on mount', () => {
    // Chip state is ephemeral, so initialActiveChipIds is the only way to
    // give a view a default filter (Agents booting into "open" only).
    const { result } = renderHook(() =>
      useListFilters({
        viewKey: 'beads',
        rows,
        projectOf,
        searchOf,
        chips,
        initialActiveChipIds: ['open'],
      }),
    );

    expect(result.current.activeChipIds.has('open')).toBe(true);
    // 3 open rows across gc + codeprobe + agent-diagnostics.
    expect(result.current.totalMatches).toBe(3);
  });

  it('lets the operator toggle a seeded default chip back off', () => {
    const { result } = renderHook(() =>
      useListFilters({
        viewKey: 'beads',
        rows,
        projectOf,
        searchOf,
        chips,
        initialActiveChipIds: ['open'],
      }),
    );

    act(() => result.current.toggleChip('open'));
    expect(result.current.activeChipIds.has('open')).toBe(false);
    expect(result.current.totalMatches).toBe(5);
  });

  it('re-seeds the default chip set when viewKey changes', () => {
    let viewKey = 'agents:a';
    const { result, rerender } = renderHook(() =>
      useListFilters({
        viewKey,
        rows,
        projectOf,
        searchOf,
        chips,
        initialActiveChipIds: ['open'],
      }),
    );

    act(() => result.current.toggleChip('open'));
    expect(result.current.activeChipIds.has('open')).toBe(false);

    viewKey = 'agents:b';
    rerender();

    // viewKey reset restores the seed, not an empty set.
    expect(result.current.activeChipIds.has('open')).toBe(true);
  });

  it('resets ephemeral search and chip state when viewKey changes (Mail inbox <-> sent)', () => {
    let viewKey = 'mail:inbox';
    const { result, rerender } = renderHook(() =>
      useListFilters({ viewKey, rows, projectOf, searchOf, chips }),
    );

    act(() => {
      result.current.setSearch('peek');
      result.current.toggleChip('open');
    });
    expect(result.current.search).toBe('peek');
    expect(result.current.activeChipIds.has('open')).toBe(true);

    viewKey = 'mail:sent';
    rerender();

    expect(result.current.search).toBe('');
    expect(result.current.activeChipIds.size).toBe(0);
    expect(result.current.totalMatches).toBe(5);
  });
});

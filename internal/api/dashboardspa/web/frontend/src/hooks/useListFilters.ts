import { useCallback, useEffect, useMemo, useState } from 'react';
import { errorMessage } from 'gas-city-dashboard-shared';
import { readBrowserStorage, writeBrowserStorage } from '../lib/browserStorage';
import { reportClientError } from '../lib/clientErrorReporting';

// useListFilters: per-view search + filter chips + project grouping
// with collapse state persisted per view in localStorage.
//
// Search and chip state are NOT persisted — they reset between
// sessions because the operator is filtering for "right now". Only
// collapsed-group ids and the sort mode persist, because those map
// onto a stable mental model of which projects she's ignoring and
// how she likes to scan them.
//
// Persistence semantics depend on `defaultCollapsed`:
//   - false (default): the persisted Set holds projects the operator
//     has *explicitly collapsed*. Projects she hasn't touched are
//     expanded.
//   - true: the persisted Set holds projects the operator has
//     *explicitly expanded*. Projects she hasn't touched are
//     collapsed. Used by views where the rig/project list is long
//     enough that everything-expanded is noise.
// Both modes use distinct storage keys so flipping the default for
// a view never silently inverts pre-existing operator state.

export interface FilterChip<T> {
  /** Stable id used for active-set membership and persistence. */
  id: string;
  /** Human-readable label rendered in the chip. */
  label: string;
  /** Predicate: row passes this chip when match returns true. */
  match: (row: T) => boolean;
}

export interface ProjectGroup<T> {
  /** Display label for the group header. */
  project: string;
  /** Stable identity used for collapse state and pinning. */
  projectKey: string;
  rows: ReadonlyArray<T>;
  /** Count of rows in the project AFTER filters but BEFORE collapse. */
  totalInProject: number;
  collapsed: boolean;
  /** When false, the group renders without a chevron and never collapses. */
  collapsible: boolean;
}

export type SortMode = 'alpha' | 'activity';

/** projectOf return shape. A bare string keeps the simple case where
 *  display label and bucket key are identical (Beads, Mail). The
 *  richer object lets callers normalize for grouping while keeping a
 *  human-readable display label (Agents needs this to fold
 *  case/separator drift in rig paths without losing GEO/etc.). */
export type ProjectBucketResult = string | { key: string; label: string };

interface UseListFiltersOptions<T> {
  /** Stable per-view key, e.g. 'beads' | 'agents' | 'mail'. */
  viewKey: string;
  rows: ReadonlyArray<T>;
  /** Derive the project bucket for a row. */
  projectOf: (row: T) => ProjectBucketResult;
  /** Strings to consider for substring search. */
  searchOf: (row: T) => ReadonlyArray<string | undefined | null>;
  chips: ReadonlyArray<FilterChip<T>>;
  /**
   * Chip ids active on first mount and after every viewKey change. Chip
   * state is ephemeral (never persisted), so this is the only way to give
   * a view a default filter — e.g. Agents booting into "running" only.
   * The operator can still toggle the seeded chips off.
   */
  initialActiveChipIds?: ReadonlyArray<string>;
  /** When true, groups start collapsed and the persisted Set is "user-expanded". */
  defaultCollapsed?: boolean;
  /** Activity timestamp (epoch ms) for a row. Required for sortMode='activity'. */
  activityOf?: (row: T) => number | undefined;
  /** Sort mode the view boots into when nothing is persisted. */
  defaultSortMode?: SortMode;
  /** Project keys that always sort first (in the given order), regardless of sortMode. */
  pinnedProjects?: ReadonlyArray<string>;
  /** Project keys that cannot be collapsed (header has no chevron, rows always shown). */
  nonCollapsibleProjects?: ReadonlySet<string>;
}

interface UseListFiltersResult<T> {
  search: string;
  setSearch: (v: string) => void;
  activeChipIds: ReadonlySet<string>;
  toggleChip: (id: string) => void;
  isCollapsed: (project: string) => boolean;
  toggleProject: (project: string) => void;
  sortMode: SortMode;
  setSortMode: (m: SortMode) => void;
  /** Visible projects after filters, ordered per sortMode. */
  groups: ReadonlyArray<ProjectGroup<T>>;
  /** Total matching rows across all projects (BEFORE collapse). */
  totalMatches: number;
}

const COLLAPSED_KEY_PREFIX = 'gcd:listFilters:collapsed:';
const EXPANDED_KEY_PREFIX = 'gcd:listFilters:expanded:';
const SORT_KEY_PREFIX = 'gcd:listFilters:sortMode:';
const COMPONENT = 'useListFilters';

function storageKey(viewKey: string, defaultCollapsed: boolean): string {
  return (defaultCollapsed ? EXPANDED_KEY_PREFIX : COLLAPSED_KEY_PREFIX) + viewKey;
}

function loadOverrides(viewKey: string, defaultCollapsed: boolean): Set<string> {
  const key = storageKey(viewKey, defaultCollapsed);
  const stored = readBrowserStorage('localStorage', key, COMPONENT);
  if (stored.status !== 'found') return new Set();
  try {
    const parsed = JSON.parse(stored.value);
    if (Array.isArray(parsed)) {
      return new Set(parsed.filter((v): v is string => typeof v === 'string'));
    }
  } catch (err) {
    reportStorageParseFailure(key, err);
  }
  return new Set();
}

function saveOverrides(viewKey: string, defaultCollapsed: boolean, ids: ReadonlySet<string>): void {
  writeBrowserStorage(
    'localStorage',
    storageKey(viewKey, defaultCollapsed),
    JSON.stringify(Array.from(ids)),
    COMPONENT,
  );
}

function loadSortMode(viewKey: string, fallback: SortMode): SortMode {
  const stored = readBrowserStorage('localStorage', SORT_KEY_PREFIX + viewKey, COMPONENT);
  if (stored.status === 'found' && (stored.value === 'alpha' || stored.value === 'activity')) {
    return stored.value;
  }
  return fallback;
}

function saveSortMode(viewKey: string, mode: SortMode): void {
  writeBrowserStorage('localStorage', SORT_KEY_PREFIX + viewKey, mode, COMPONENT);
}

function reportStorageParseFailure(key: string, err: unknown): void {
  void reportClientError({
    component: COMPONENT,
    operation: 'localStorage.parse',
    message: `${key}: ${errorMessage(err)}`,
  });
}

const EMPTY_PINNED: ReadonlyArray<string> = [];
const EMPTY_NONCOLLAPSIBLE: ReadonlySet<string> = new Set();
const EMPTY_INITIAL_CHIPS: ReadonlyArray<string> = [];

export function useListFilters<T>({
  viewKey,
  rows,
  projectOf,
  searchOf,
  chips,
  initialActiveChipIds = EMPTY_INITIAL_CHIPS,
  defaultCollapsed = false,
  activityOf,
  defaultSortMode = 'alpha',
  pinnedProjects = EMPTY_PINNED,
  nonCollapsibleProjects = EMPTY_NONCOLLAPSIBLE,
}: UseListFiltersOptions<T>): UseListFiltersResult<T> {
  // Stable key so the viewKey-reset effect re-runs only when the seed set
  // actually changes, not on every render (the array prop is unstable).
  const initialChipKey = initialActiveChipIds.join(',');
  const [search, setSearch] = useState('');
  const [activeChipIds, setActiveChipIds] = useState<ReadonlySet<string>>(
    () => new Set(initialActiveChipIds),
  );
  // The Set is "user overrides": projects whose current collapse state
  // differs from defaultCollapsed. isCollapsed(p) = defaultCollapsed XOR overrides.has(p).
  const [overrides, setOverrides] = useState<ReadonlySet<string>>(() =>
    loadOverrides(viewKey, defaultCollapsed),
  );
  const [sortMode, setSortModeState] = useState<SortMode>(() =>
    loadSortMode(viewKey, defaultSortMode),
  );

  // When the view key changes (e.g. Mail switching inbox <-> sent),
  // reload persisted state AND reset ephemeral search/chip state.
  // Without the reset, an "unread" chip activated in inbox would
  // silently filter Sent (all read), making the box appear empty.
  useEffect(() => {
    setOverrides(loadOverrides(viewKey, defaultCollapsed));
    setSortModeState(loadSortMode(viewKey, defaultSortMode));
    setSearch('');
    setActiveChipIds(new Set(initialActiveChipIds));
    // initialActiveChipIds is keyed by initialChipKey for dep stability.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [viewKey, defaultCollapsed, defaultSortMode, initialChipKey]);

  const toggleChip = useCallback((id: string) => {
    setActiveChipIds((cur) => {
      const next = new Set(cur);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const toggleProject = useCallback(
    (project: string) => {
      setOverrides((cur) => {
        const next = new Set(cur);
        if (next.has(project)) next.delete(project);
        else next.add(project);
        saveOverrides(viewKey, defaultCollapsed, next);
        return next;
      });
    },
    [viewKey, defaultCollapsed],
  );

  const isCollapsed = useCallback(
    (project: string) => (overrides.has(project) ? !defaultCollapsed : defaultCollapsed),
    [overrides, defaultCollapsed],
  );

  const setSortMode = useCallback(
    (m: SortMode) => {
      setSortModeState(m);
      saveSortMode(viewKey, m);
    },
    [viewKey],
  );

  const groups = useMemo<ReadonlyArray<ProjectGroup<T>>>(() => {
    const needle = search.trim().toLowerCase();
    const activeChips = chips.filter((c) => activeChipIds.has(c.id));

    const matchesSearch = (row: T): boolean => {
      if (needle.length === 0) return true;
      for (const field of searchOf(row)) {
        if (field && field.toLowerCase().includes(needle)) return true;
      }
      return false;
    };

    const matchesChips = (row: T): boolean => {
      if (activeChips.length === 0) return true;
      // Multi-chip = OR (union). The operator broadens by adding chips,
      // not narrows: selecting "open + in_progress" should show both.
      for (const chip of activeChips) {
        if (chip.match(row)) return true;
      }
      return false;
    };

    interface BucketState {
      rows: T[];
      labelCounts: Map<string, number>;
    }
    const buckets = new Map<string, BucketState>();
    for (const row of rows) {
      if (!matchesSearch(row)) continue;
      if (!matchesChips(row)) continue;
      const result = projectOf(row);
      const key = typeof result === 'string' ? result : result.key;
      const label = typeof result === 'string' ? result : result.label;
      const bucket = buckets.get(key);
      if (bucket) {
        bucket.rows.push(row);
        bucket.labelCounts.set(label, (bucket.labelCounts.get(label) ?? 0) + 1);
      } else {
        buckets.set(key, {
          rows: [row],
          labelCounts: new Map([[label, 1]]),
        });
      }
    }

    // Pick a display label per bucket: highest-frequency wins; ties
    // prefer a label that contains uppercase letters so acronyms like
    // GEO survive against the lowercased basename `geo`.
    const pickLabel = (counts: Map<string, number>): string => {
      let best = '';
      let bestCount = -1;
      let bestHasUpper = false;
      for (const [label, count] of counts) {
        const hasUpper = /[A-Z]/.test(label);
        const wins = count > bestCount || (count === bestCount && hasUpper && !bestHasUpper);
        if (wins) {
          best = label;
          bestCount = count;
          bestHasUpper = hasUpper;
        }
      }
      return best;
    };

    const allKeys = Array.from(buckets.keys());
    // Pinned keys come first (in the order they were declared), even
    // if they're not in the result set. Pinned keys with no rows are
    // dropped — we don't render empty groups.
    const pinnedPresent = pinnedProjects.filter((k) => buckets.has(k));
    const pinnedSet = new Set(pinnedPresent);
    const unpinnedKeys = allKeys.filter((k) => !pinnedSet.has(k));

    if (sortMode === 'activity' && activityOf) {
      // Score each unpinned project by its most recent row activity.
      // Projects with no scoreable rows sink to the bottom in
      // alphabetical order so the result is deterministic.
      const score = new Map<string, number>();
      for (const key of unpinnedKeys) {
        const b = buckets.get(key);
        let best = -Infinity;
        if (b) {
          for (const row of b.rows) {
            const t = activityOf(row);
            if (typeof t === 'number' && Number.isFinite(t) && t > best) best = t;
          }
        }
        score.set(key, best);
      }
      unpinnedKeys.sort((a, b) => {
        const sa = score.get(a) ?? -Infinity;
        const sb = score.get(b) ?? -Infinity;
        if (sa !== sb) return sb - sa;
        return a.localeCompare(b);
      });
    } else {
      unpinnedKeys.sort();
    }

    const orderedKeys = [...pinnedPresent, ...unpinnedKeys];
    const collapsedFor = (key: string) => {
      if (nonCollapsibleProjects.has(key)) return false;
      return overrides.has(key) ? !defaultCollapsed : defaultCollapsed;
    };

    return orderedKeys.map((key) => {
      const b = buckets.get(key);
      const projRows = b?.rows ?? [];
      return {
        project: b ? pickLabel(b.labelCounts) : key,
        projectKey: key,
        rows: projRows,
        totalInProject: projRows.length,
        collapsed: collapsedFor(key),
        collapsible: !nonCollapsibleProjects.has(key),
      };
    });
  }, [
    rows,
    search,
    activeChipIds,
    chips,
    projectOf,
    searchOf,
    overrides,
    defaultCollapsed,
    sortMode,
    activityOf,
    pinnedProjects,
    nonCollapsibleProjects,
  ]);

  const totalMatches = useMemo(
    () => groups.reduce((sum, g) => sum + g.totalInProject, 0),
    [groups],
  );

  return {
    search,
    setSearch,
    activeChipIds,
    toggleChip,
    isCollapsed,
    toggleProject,
    sortMode,
    setSortMode,
    groups,
    totalMatches,
  };
}

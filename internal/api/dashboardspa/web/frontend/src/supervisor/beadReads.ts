import type {
  Bead,
  GetV0CityByCityNameBeadsData,
  ListBodyBead,
} from 'gas-city-dashboard-shared/gc-supervisor';
import { activeCityOrThrow } from '../api/cityBase';
import { SupervisorApiError, supervisorApi } from './client';

export type SupervisorBead = Bead;

export interface SupervisorBeadList extends Omit<ListBodyBead, 'items' | 'total'> {
  items: SupervisorBead[];
  total: number;
  upstream_total?: number;
  upstream_fetched: number;
  fetch_limit: number;
}

export interface ListSupervisorBeadsOptions {
  includeClosed?: boolean;
  includeBookkeeping?: boolean;
  rigFilter?: string;
  limit?: number;
}

// Pre-exposure load bounds (gascity-dashboard-q89b): board and detail-fallback
// list fetches run per client, so keep them modest. Truncation degrades
// visibly: upstream_total vs fetch_limit feeds the board's truncation notice.
const BEADS_FETCH_LIMIT = 1000;
const ASSIGNED_BEADS_FETCH_LIMIT = 200;
const DETAIL_FALLBACK_FETCH_LIMIT = 1000;
// The "real work" bead types the board fans out (one typed query each),
// then keeps via defaultBeadFilter — the bookkeeping types
// (message/session/molecule/…) are dropped. Every entry MUST be a type the
// live gc/bd backend accepts as a `type=` filter: a rig-scoped include-closed
// (`all=true`) query for a type the backend rejects fails closed (HTTP 503
// "invalid issue type") and that one rejected leg blanks the whole board.
const ENGINEERING_BEAD_TYPES: ReadonlySet<string> = new Set([
  'feature',
  'bug',
  'task',
  'epic',
  'chore',
  'decision',
]);

export async function listSupervisorBeads(
  options: ListSupervisorBeadsOptions = {},
): Promise<SupervisorBeadList> {
  const cityName = activeCityOrThrow('list supervisor beads');
  const limit = options.limit ?? BEADS_FETCH_LIMIT;
  const rigFilter = options.rigFilter?.trim() ?? '';
  const includeClosed = options.includeClosed ?? false;
  const includeBookkeeping = options.includeBookkeeping ?? false;
  const baseQuery: NonNullable<GetV0CityByCityNameBeadsData['query']> = {
    limit,
    ...(includeClosed ? { all: true } : {}),
    ...(rigFilter.length === 0 ? {} : { rig: rigFilter }),
  };
  const list = await supervisorApi().listBeads(cityName, baseQuery);
  const items = uniqueById(list.items ?? []);
  const statusFiltered = includeClosed ? items : items.filter((bead) => bead.status !== 'closed');
  const filtered = includeBookkeeping ? statusFiltered : statusFiltered.filter(defaultBeadFilter);
  const upstreamTotal = countAsNumber(list.total);
  return {
    items: filtered,
    total: filtered.length,
    ...(upstreamTotal === undefined ? {} : { upstream_total: upstreamTotal }),
    upstream_fetched: items.length,
    fetch_limit: limit,
  };
}

export async function listSupervisorBeadsAssignedTo(
  assignees: readonly string[],
  options: Pick<ListSupervisorBeadsOptions, 'includeClosed' | 'limit'> = {},
): Promise<SupervisorBeadList> {
  const cityName = activeCityOrThrow('list supervisor assigned beads');
  const uniqueAssignees = uniqueNonEmpty(assignees);
  const limit = options.limit ?? ASSIGNED_BEADS_FETCH_LIMIT;
  const includeClosed = options.includeClosed ?? false;
  if (uniqueAssignees.length === 0) {
    return {
      items: [],
      total: 0,
      upstream_fetched: 0,
      fetch_limit: limit,
    };
  }
  const lists = await Promise.all(
    uniqueAssignees.map((assignee) =>
      supervisorApi().listBeads(cityName, {
        assignee,
        limit,
        ...(includeClosed ? { all: true } : {}),
      }),
    ),
  );
  const items = uniqueById(lists.flatMap((list) => list.items ?? []));
  const upstreamTotal = sumTotals(lists);
  return {
    items,
    total: items.length,
    ...(upstreamTotal === undefined ? {} : { upstream_total: upstreamTotal }),
    upstream_fetched: items.length,
    fetch_limit: limit,
  };
}

export async function fetchSupervisorBead(id: string): Promise<SupervisorBead> {
  const cityName = activeCityOrThrow('fetch supervisor bead');
  try {
    return await supervisorApi().getBead(cityName, id);
  } catch (err) {
    if (!(err instanceof SupervisorApiError) || err.status !== 404) throw err;

    const list = await supervisorApi().listBeads(cityName, {
      limit: DETAIL_FALLBACK_FETCH_LIMIT,
    });
    const hit = (list.items ?? []).find((bead) => bead.id === id);
    if (hit !== undefined) return hit;
    throw err;
  }
}

function defaultBeadFilter(bead: SupervisorBead): boolean {
  if (!ENGINEERING_BEAD_TYPES.has(bead.issue_type)) return false;
  if (Array.isArray(bead.labels) && bead.labels.some((label) => label.startsWith('gc:'))) {
    return false;
  }
  return true;
}
function countAsNumber(value: ListBodyBead['total']): number | undefined {
  if (typeof value === 'number') return value;
  if (typeof value === 'bigint') return Number(value);
  return undefined;
}

function sumTotals(lists: readonly ListBodyBead[]): number | undefined {
  let total = 0;
  for (const list of lists) {
    const value = countAsNumber(list.total);
    if (value === undefined) return undefined;
    total += value;
  }
  return total;
}

function uniqueById(items: readonly SupervisorBead[]): SupervisorBead[] {
  const seen = new Set<string>();
  const unique: SupervisorBead[] = [];
  for (const item of items) {
    if (seen.has(item.id)) continue;
    seen.add(item.id);
    unique.push(item);
  }
  return unique;
}

function uniqueNonEmpty(values: readonly string[]): string[] {
  const seen = new Set<string>();
  const unique: string[] = [];
  for (const value of values) {
    const trimmed = value.trim();
    if (trimmed.length === 0 || seen.has(trimmed)) continue;
    seen.add(trimmed);
    unique.push(trimmed);
  }
  return unique;
}

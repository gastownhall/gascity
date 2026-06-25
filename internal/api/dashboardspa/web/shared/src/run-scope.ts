import { SCOPE_REF_RE, type RunScopeKind } from './run-detail.js';

export interface RunScope {
  scopeKind: RunScopeKind;
  scopeRef: string;
}

export interface RunScopeWithStoreRef extends RunScope {
  rootStoreRef: string;
}

export type RequestScopeResult = { ok: true; scope?: RunScope } | { ok: false; error: string };

export interface RequestScopeFields {
  scope_kind?: unknown;
  scope_ref?: unknown;
}

export interface SnapshotScopeFields {
  scope_kind?: unknown;
  scope_ref?: unknown;
}

export interface FeedScopeFields extends SnapshotScopeFields {
  root_store_ref?: unknown;
}

export function parseRunScopeKind(value: unknown): RunScopeKind | null {
  return value === 'city' || value === 'rig' ? value : null;
}

export function fromRequestScope(query: RequestScopeFields): RequestScopeResult {
  if (query.scope_kind !== undefined && typeof query.scope_kind !== 'string') {
    return { ok: false, error: 'invalid scope kind' };
  }
  if (query.scope_ref !== undefined && typeof query.scope_ref !== 'string') {
    return { ok: false, error: 'invalid scope ref' };
  }
  const rawScopeKind = query.scope_kind;
  const rawScopeRef = query.scope_ref;
  const scopeKind = parseRunScopeKind(rawScopeKind);
  if (rawScopeKind !== undefined && scopeKind === null) {
    return { ok: false, error: 'invalid scope kind' };
  }
  if ((rawScopeKind === undefined) !== (rawScopeRef === undefined)) {
    return { ok: false, error: 'scope kind and scope ref are required together' };
  }
  if (rawScopeRef !== undefined && !SCOPE_REF_RE.test(rawScopeRef)) {
    return { ok: false, error: 'invalid scope ref' };
  }
  if (scopeKind !== null && rawScopeRef !== undefined) {
    return { ok: true, scope: { scopeKind, scopeRef: rawScopeRef } };
  }
  return { ok: true };
}

export function fromSnapshotScope(snapshot: SnapshotScopeFields): RunScope | null {
  const scopeKind = parseRunScopeKind(snapshot.scope_kind);
  const scopeRef = stringValue(snapshot.scope_ref);
  return scopeKind !== null && scopeRef !== null ? { scopeKind, scopeRef } : null;
}

export function fromFeedScope(feed: FeedScopeFields): RunScopeWithStoreRef | null {
  const scope = fromSnapshotScope(feed);
  if (scope === null || !SCOPE_REF_RE.test(scope.scopeRef)) return null;
  return {
    ...scope,
    rootStoreRef: stringValue(feed.root_store_ref) ?? `${scope.scopeKind}:${scope.scopeRef}`,
  };
}

export function fromRootMetadataScope(
  metadata: Record<string, string> | undefined,
): RunScopeWithStoreRef | null {
  const rootStoreRef = stringValue(metadata?.['gc.root_store_ref']);

  // Primary source: the explicit gc.scope_kind / gc.scope_ref pair. When
  // present and valid it wins, and the existing behaviour is unchanged.
  const scopeKind = parseRunScopeKind(metadata?.['gc.scope_kind']);
  const scopeRef = stringValue(metadata?.['gc.scope_ref']);
  if (scopeKind !== null && scopeRef !== null && SCOPE_REF_RE.test(scopeRef)) {
    return {
      scopeKind,
      scopeRef,
      rootStoreRef: rootStoreRef ?? `${scopeKind}:${scopeRef}`,
    };
  }

  // Fallback (gascity-dashboard-km0w): live run-root beads carry only
  // gc.root_store_ref ("<kind>:<ref>") — never the gc.scope_ref pair. Recover
  // the lane scope from it. fromStoreRef rejects any prefix that is not exactly
  // 'rig'/'city' and any colon-less value (no guessing); the parsed ref is then
  // validated against the same SCOPE_REF_RE the explicit path uses.
  if (rootStoreRef === null) return null;
  const parsed = fromStoreRef(rootStoreRef);
  if (parsed === null || !SCOPE_REF_RE.test(parsed.scopeRef)) {
    return null;
  }
  return { ...parsed, rootStoreRef };
}

export function fromStoreRef(rootStoreRef: unknown): RunScope | null {
  const value = stringValue(rootStoreRef);
  if (value === null) return null;
  const colon = value.indexOf(':');
  if (colon <= 0 || colon >= value.length - 1) return null;
  const scopeKind = parseRunScopeKind(value.slice(0, colon));
  const scopeRef = stringValue(value.slice(colon + 1));
  return scopeKind !== null && scopeRef !== null ? { scopeKind, scopeRef } : null;
}

function stringValue(value: unknown): string | null {
  return typeof value === 'string' && value.trim().length > 0 ? value.trim() : null;
}

// Bead-ID cross-entity linked view (gascity-dashboard-j4x).
//
// Wire shapes for the relation index that joins the dashboard's six
// entity types (beads, formula runs, sessions, GitHub PRs/issues,
// formula/order runs) into a single bidirectional, provenance-tagged
// adjacency view rendered as a typeset "Related" section.
//
// The join is computed over supervisor-provided bead metadata (ZFC-clean:
// structural inversion of already-extracted fields, no read-time heuristics).
// Browser code can build it directly from supervisor API reads; backend tests
// still exercise the same shared builder through compatibility re-exports.

import type { IsoTimestamp } from './dashboard-sessions.js';

/**
 * Entity kinds a relation node can point at. Bead-native kinds resolve
 * authoritatively from supervisor metadata; the GitHub kinds may be an
 * honest unresolved reference (the bead records a PR/issue number but the
 * PR/issue is not in the fetched open-only set).
 */
export type LinkNodeType =
  | 'bead'
  | 'session'
  | 'github_pr'
  | 'github_issue'
  | 'formula_run'
  | 'order_run';

/**
 * Provenance tier of an edge, mirroring GitHub's own structured-vs-prose
 * distinction:
 *   - 'supervisor': authoritative bead metadata (parent, molecule, etc.).
 *   - 'external':   structured third-party field.
 *   - 'derived':    a value parsed from prose (the `Fixes #N` regex),
 *                   quarantined from authoritative fields.
 */
export type LinkProvenance = 'supervisor' | 'external' | 'derived';

/**
 * A reference to one entity in the relation view. The `key` is the
 * namespaced, globally-unique identity used for de-duplication and
 * adjacency (see makeNodeKey); `ref` is the human/route-facing handle.
 */
export interface LinkNodeRef {
  /** Namespaced identity (`<type>:<scope>:<ref>`). Stable for adjacency. */
  key: string;
  type: LinkNodeType;
  /** Display/route handle (bead id, `pr/<n>`, session id, run id). */
  ref: string;
}

/**
 * A summary node carried in the view. Payloads are display-only — never
 * full bodies. `url` (when present) has passed the `^https?://` allow-list
 * (R4); a value that failed the check is omitted, never rendered as href.
 */
export interface LinkNode extends LinkNodeRef {
  title: string | null;
  status: string | null;
  /** Sanitised outbound URL (http/https only) or null. */
  url: string | null;
  /**
   * Oldest contributing-source fetch time for this node (R7). Bead nodes
   * are supervisor-fresh; GitHub nodes can be up to 24h stale. The UI
   * renders a node whose age exceeds its band as visibly stale (RK2).
   */
  fetchedAt: IsoTimestamp | null;
  /**
   * True when the reference resolved to zero present entities (R6). The UI
   * renders an explicit `unresolved` row with an outbound `↗` rather than
   * hiding the link.
   */
  unresolved: boolean;
  /**
   * When a reference resolved to more than one candidate (e.g. retry
   * duplicate beads, a session name matching multiple sessions), the
   * count is recorded here and the UI renders `unresolved (N candidates)`
   * rather than guessing (R6).
   */
  candidateCount?: number;
}

/**
 * One directed adjacency edge. `from`/`to` are node keys (LinkNodeRef.key).
 */
export interface LinkEdge {
  from: string;
  to: string;
  /** Relation label, e.g. 'parent', 'child', 'molecule', 'pr', 'session'. */
  relation: string;
  provenance: LinkProvenance;
  /** True when both endpoints resolved to a present node. */
  resolved: boolean;
}

/**
 * The per-edge-type resolution outcome rollup (R11). Has a named consumer:
 * the Health register surfaces these rates so candidate link
 * directions are evaluated on measured hit-rate, not speculation (RK4).
 * Arithmetic aggregation only — no semantic judgement.
 */
export interface LinkResolutionStat {
  relation: string;
  resolved: number;
  unresolved: number;
  nCandidates: number;
}

/**
 * The full relation view for one focus entity. One hop only — adjacency,
 * not transitive closure.
 */
export interface EntityLinkView {
  /** The entity the view is centred on. */
  focus: LinkNodeRef;
  nodes: LinkNode[];
  edges: LinkEdge[];
  /** Per-edge-type resolution outcomes (R11). */
  stats: LinkResolutionStat[];
  /**
   * True when any contributing fetch failed or the focus ref did not
   * resolve to a known bead — mirrors routes/runs.ts partial flag.
   */
  partial: boolean;
  /** When the view was assembled (server clock). */
  generatedAt: IsoTimestamp;
  /**
   * Oldest contributing-source fetch time across all nodes (R7). Surfaced
   * as the section "as of" line; row-level staleness still wins per RK2.
   */
  asOf: IsoTimestamp | null;
}

/**
 * Build the namespaced, globally-unique node key (RK1 / OQ#1). Bead IDs
 * are unique within a single city today, but keying on
 * `<type>:<scope>:<ref>` prevents rig-scoped collisions where the same
 * bare ID can recur across scopes.
 *
 * The PRD specifies `scope_kind:scope_ref:id` to avoid cross-scope
 * collisions. `scope` MUST therefore already encode the bead's scope KIND
 * as well as its ref (the backend passes `<scope_kind>:<scope_ref>`, e.g.
 * `rig:rig-a` or `city:my-city`). A bare `scope_ref` is insufficient:
 * a city-scoped and a rig-scoped bead can share a `scope_ref` value and
 * would otherwise collide.
 */
export function makeNodeKey(type: LinkNodeType, ref: string, scope: string): string {
  return `${type}:${scope}:${ref}`;
}

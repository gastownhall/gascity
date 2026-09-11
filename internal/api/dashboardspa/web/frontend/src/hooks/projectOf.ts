import type { DashboardSession } from 'gas-city-dashboard-shared';
import { getActiveCity } from '../api/cityBase';
import type { AgentResponse } from 'gas-city-dashboard-shared/gc-supervisor';
import type { SupervisorBead } from '../supervisor/beadReads';
import type { SupervisorMailItem } from '../supervisor/mailReads';

// Per-source project derivation. There is no explicit project field
// on any of the three wire shapes, so we derive from observable
// conventions in the data:
//
// - Beads: ID is `<project>-<suffix>` where suffix is alnum, optionally
//   followed by `.N` (e.g. `gc-1920`, `codeprobe-4cl6.2`,
//   `code-intel-digest-mp5`). Strip the suffix to get the project.
//
// - Sessions: `rig` is a filesystem root path; basename = project.
//   We also fold case + underscores so /home/ds/projects/GEO and
//   bare `geo` group together, and scix_experiments meets
//   scix-experiments. The display label keeps the most-frequent
//   original form (the bucketer in useListFilters picks the winner).
//   Cross-rig orchestration sessions (mayor, the global control
//   dispatcher, chief-of-staff) have an empty rig and a known
//   template; they get lifted into a pinned 'Orchestration' bucket
//   so they don't fall through to (no rig).
//
// - Mail: `rig` is already a project name (e.g. "my-city"); use
//   directly. When absent, fall back to "(no rig)".

const BEAD_ID_RX = /^(.+?)-[a-z0-9]+(?:\.\d+)?$/i;

export function beadProject(bead: SupervisorBead): string {
  const m = BEAD_ID_RX.exec(bead.id);
  return m?.[1] ?? bead.id;
}

// Stable grouping KEY for cross-rig orchestration agents/sessions. This is an
// internal identity used for bucketing + collapse state and never shown raw to
// the operator: the displayed LABEL is the active city name (see
// `orchestrationLabel`). The key stays a constant so grouping is independent of
// whichever city is currently mounted.
export const ORCHESTRATION_PROJECT = 'Orchestration';

/**
 * Display label for the cross-rig orchestration bucket. The operator thinks of
 * mayor / dispatchers / PLs as "the city", not as an abstract "Orchestration"
 * group, so we render the active city name (e.g. `my-city`). Falls back to
 * the constant before the router has resolved a city — a degraded label is
 * better than an empty one.
 */
export function orchestrationLabel(): string {
  return getActiveCity() ?? ORCHESTRATION_PROJECT;
}

// Residual bucket for a row with no rig association and no orchestration role.
export const NO_RIG_PROJECT = '(no rig)';

// Pinned bucket for gascity builtin MAINTENANCE pools (e.g. `dog`). These pools
// run cross-rig housekeeping agents whose `rig` is empty, so without this
// carve-out the pool name falls through to pool-as-rig and is mislabelled as a
// rig in the Agents Rig column/filter. Distinct from Orchestration (mayor /
// dispatchers, which direct work) and from (no rig) (agents with no rig AND no
// pool).
export const MAINTENANCE_PROJECT = 'Maintenance';

// Templates whose sessions are cross-rig orchestration (no specific
// rig). Per-rig dispatchers (alias '<rig>/control-dispatcher') are
// NOT in this set — they belong to their rig and are styled inline.
const ORCHESTRATION_TEMPLATES: ReadonlySet<string> = new Set([
  'mayor',
  'control-dispatcher',
  'oversight-rig.chief-of-staff',
]);

export function isOrchestrationSession(s: DashboardSession): boolean {
  if (s.rig && s.rig.length > 0) return false;
  return !!s.template && ORCHESTRATION_TEMPLATES.has(s.template);
}

// A session is a per-rig dispatcher when it's scoped to a rig but
// performs the dispatcher role. Used to italicize the alias cell so
// the operator can spot orchestration even inside rig groups.
const PER_RIG_DISPATCHER_RX = /\/control-dispatcher$/;

export function isPerRigDispatcher(s: DashboardSession): boolean {
  if (!s.rig || s.rig.length === 0) return false;
  return PER_RIG_DISPATCHER_RX.test(s.alias ?? '');
}

// ── Worker-session classification (Work-in-flight signal) ─────────────────
//
// The Work-in-flight section counts the live WORKER sessions, not the
// in-progress beads: the work-beads churn to zero within seconds (focus-reviews
// finish fast) and live in rig stores the dashboard's bead fetch doesn't
// reliably aggregate, while the worker SESSIONS stay active across that churn.
// A worker session is the stable "what is working" signal.
//
// Orchestration sessions (mayor, the global + per-rig control dispatchers, the
// per-rig project leads, the oversight chief-of-staff) are excluded: they
// direct work, they don't perform it. The exclusion reuses the orchestration
// predicates above plus the role markers below.

// A worker session's template/name ends in a worker/pool role — `polecat`,
// `scix-worker`, `worker-1` — optionally with a numeric slot suffix, and may
// carry a trailing `-gc-XXXXX` spawned-session handle. Anchored to the END so a
// role like `oversight-worker-review` (not a worker) doesn't slip through on a
// mid-string match.
const WORKER_ROLE_RX = /(?:worker|polecat)(?:-\d+)?$/;

// Per-rig orchestration markers that DON'T live in ORCHESTRATION_TEMPLATES
// (which only covers the rig-less cross-rig set). A rig-scoped project-lead or
// chief-of-staff directs that rig's work; it is not a worker.
const RIG_ORCHESTRATION_RX = /(?:\.project-lead|chief-of-staff)$/;

/**
 * True when a session is a live worker actively performing work — the signal
 * the Work-in-flight section counts. Requires a live state (`active` or
 * `running` — the same running signal isRunningAgent / isSessionStreamable /
 * stateTone honour, so a worker reported as `running` is not silently dropped),
 * a worker/pool role in the template or runtime name, and that the session is
 * NOT any flavour of orchestration (cross-rig, per-rig dispatcher,
 * project-lead, chief-of-staff).
 */
export function isWorkerSession(s: DashboardSession): boolean {
  if (s.state !== 'active' && s.state !== 'running') return false;
  if (isOrchestrationSession(s) || isPerRigDispatcher(s)) return false;
  const template = s.template ?? '';
  const alias = s.alias ?? '';
  if (RIG_ORCHESTRATION_RX.test(template) || RIG_ORCHESTRATION_RX.test(alias)) {
    return false;
  }
  // The worker role can surface in the template (`polecat`, `scix-worker`) or,
  // for a dynamically-spawned slot, in the runtime session_name
  // (`polecat-gc-335825`). Strip any `-gc-XXXXX` handle first so the role
  // anchor matches the cleaned name.
  const sessionName = s.session_name;
  const candidates = [template, alias, sessionName]
    .filter((c) => c.length > 0)
    .map((c) => cleanWorkerName(c));
  return candidates.some((c) => WORKER_ROLE_RX.test(c));
}

function normalizeRigKey(name: string): string {
  return name.toLowerCase().replace(/_/g, '-');
}

export interface ProjectBucket {
  /** Stable identity used for grouping + collapse state. */
  key: string;
  /** Display label rendered in the group header. */
  label: string;
}

export function sessionProject(session: DashboardSession): ProjectBucket {
  if (isOrchestrationSession(session)) {
    return { key: ORCHESTRATION_PROJECT, label: orchestrationLabel() };
  }
  const candidate = session.rig ?? session.pool ?? session.template;
  if (!candidate) {
    return { key: NO_RIG_PROJECT, label: NO_RIG_PROJECT };
  }
  // basename — handle both '/' and '\' for cross-platform safety.
  const parts = candidate.split(/[\\/]/).filter(Boolean);
  const basename = parts[parts.length - 1] ?? candidate;
  return { key: normalizeRigKey(basename), label: basename };
}

export function mailProject(mail: SupervisorMailItem): string {
  if (mail.rig && mail.rig.length > 0) return mail.rig;
  return NO_RIG_PROJECT;
}

// ── Agent grouping (gascity-dashboard-ay6) ───────────────────────────────
//
// Parallel to the session-derived helpers above. AgentResponse has no `template`
// field (which sessionProject uses to detect cross-rig orchestration), so
// the agent-side analog keys on `name` (the alias) instead. Cross-rig
// agents — mayor, the global control dispatcher, oversight-rig.chief-of-staff
// — surface with `rig === ''` (or absent) and a well-known alias.
//
// Kept separate from sessionProject rather than folded together because the
// agent and session wires carry different identifying fields; coercing one
// into the other would either drop information or fabricate it.

const ORCHESTRATION_AGENT_NAMES: ReadonlySet<string> = new Set([
  'mayor',
  'control-dispatcher',
  'oversight-rig.chief-of-staff',
]);

// gascity builtin maintenance pool templates (the `dog` housekeeping pool;
// gascity internal/config defines these as cross-rig builtins). Their agents
// carry an empty `rig`, so they must be lifted into the Maintenance bucket
// rather than fall through to pool-as-rig. A small named constant set — not a
// regex or substring match (ZFC / anti-slop): the maintenance pools are a
// closed, enumerable set, and a loose match could wrongly capture a legitimate
// pool-as-rig (e.g. `research`) whose name merely contains a maintenance token.
const MAINTENANCE_POOLS: ReadonlySet<string> = new Set(['dog']);

export function isOrchestrationAgent(a: AgentResponse): boolean {
  if (a.rig && a.rig.length > 0) return false;
  return ORCHESTRATION_AGENT_NAMES.has(a.name);
}

/**
 * An agent is a per-rig dispatcher when it's scoped to a rig but performs
 * the dispatcher role. Mirrors `isPerRigDispatcher` for sessions, but keys
 * on the agent's `name` (the alias) rather than `session.alias`.
 */
export function isPerRigDispatcherAgent(a: AgentResponse): boolean {
  if (!a.rig || a.rig.length === 0) return false;
  return PER_RIG_DISPATCHER_RX.test(a.name);
}

export function agentProject(agent: AgentResponse): ProjectBucket {
  if (isOrchestrationAgent(agent)) {
    return { key: ORCHESTRATION_PROJECT, label: orchestrationLabel() };
  }
  // Agents — unlike sessions — never carry a `template` field; rig/pool are
  // the only grouping candidates. Empty-string `rig` is treated as absent
  // (the supervisor uses '' for cross-rig agents that aren't in the
  // orchestration set).
  const rig = agent.rig && agent.rig.length > 0 ? agent.rig : undefined;
  // A maintenance-builtin pool (e.g. `dog`) is cross-rig and is NOT a rig. Lift
  // it into the pinned Maintenance bucket before the pool-as-rig fallback, so
  // the pool name is never mislabelled as a rig. A real rig association still
  // wins (the rig-guard semantics), so this only fires for rig-less agents.
  if (!rig && agent.pool && MAINTENANCE_POOLS.has(agent.pool)) {
    return { key: MAINTENANCE_PROJECT, label: MAINTENANCE_PROJECT };
  }
  const candidate = rig ?? agent.pool;
  if (!candidate) {
    return { key: NO_RIG_PROJECT, label: NO_RIG_PROJECT };
  }
  const parts = candidate.split(/[\\/]/).filter(Boolean);
  const basename = canonicalRigLabel(parts[parts.length - 1] ?? candidate);
  return { key: normalizeRigKey(basename), label: basename };
}

/**
 * A pool/worker template path often points at a "-main" build tree or worktree
 * (e.g. `gascity-main` is the gc build tree, `gascity-packs-main` a packs
 * worktree) rather than the registered rig. The agent's real rig is the base
 * name — strip the suffix so the UI shows `gascity`, not `gascity-main`.
 */
export function canonicalRigLabel(name: string): string {
  return name.endsWith('-main') ? name.slice(0, -'-main'.length) : name;
}

// Trailing `-gc-XXXXX` (or other 2/4-letter-prefixed) live-session handle that
// leaks into a worker name when the supervisor labels a dynamically-spawned
// slot by its session (e.g. `polecat-gc-335825`). Mirrors the session-id
// alphabet; anchored to the END so only a real suffix is cut. The id body is
// hyphen-free (`gc-335825`, not `gc-33-5825`) so the match binds to the minimal
// trailing handle. The body MUST contain a digit (mirroring BARE_SESSION_ID_RX
// in shared/work-in-flight.ts): live session ids always carry a numeric handle,
// so requiring a digit stops the `[a-z]{4}` prefix branch from false-stripping a
// hyphenated role whose penultimate segment is four letters (e.g. the `-scix-`
// in `*-scix-worker`, which has no digit in `worker`).
const WORKER_SESSION_SUFFIX_RX = /-(?:gc|td|th|[a-z]{4})-[a-z0-9]*[0-9][a-z0-9]*$/;

/**
 * Clean a worker/agent/assignee name for display: strip any leading filesystem
 * path (keep the basename) and any trailing `-gc-XXXXX` live-session suffix, so
 * `/home/ds/gas-city/city-infra-polecat` shows as `city-infra-polecat` and
 * `polecat-gc-335825` shows as `polecat`. Returns the trimmed input unchanged
 * when neither a path nor a session suffix is present.
 */
export function cleanWorkerName(name: string): string {
  const trimmed = name.trim();
  // basename — handle both '/' and '\' for cross-platform safety.
  const parts = trimmed.split(/[\\/]/).filter(Boolean);
  const basename = parts[parts.length - 1] ?? trimmed;
  const stripped = basename.replace(WORKER_SESSION_SUFFIX_RX, '');
  return stripped.length > 0 ? stripped : basename;
}

/**
 * True when an agent has no rig association — the pinned cross-rig
 * Orchestration bucket (mayor, control-dispatcher, oversight chief-of-staff),
 * the pinned Maintenance bucket (builtin housekeeping pools like `dog`), or the
 * residual (no rig) bucket. The Agents Rig column shows the bare alias for
 * these rather than a pseudo-rig label, since none is a real rig.
 */
export function isAgentOutsideRig(agent: AgentResponse): boolean {
  const { key } = agentProject(agent);
  return key === ORCHESTRATION_PROJECT || key === MAINTENANCE_PROJECT || key === NO_RIG_PROJECT;
}

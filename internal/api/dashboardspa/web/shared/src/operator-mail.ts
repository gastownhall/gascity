// Sender-role classification for operator-facing mail — the single owner of the
// "which agent kind is this, and which mail actually needs the human" logic.
//
// The dashboard's operator-facing mail derivation needs one sender-role filter.
// Keep it in `shared` so browser projections and dashboard-local helpers cannot
// drift into different classifications.
//
// Pure over dashboard-owned projections (DashboardSession + a minimal
// OperatorMailItem) — no IO, no React.

import type { DashboardSession } from './dashboard-sessions.js';

/**
 * The mail fields the operator-mail sender-role filter reads — read state,
 * sender, and timestamp. Kept as a minimal structural shape rather than a
 * specific wire DTO so the filter stays portable across generated supervisor
 * mail reads. The direct-supervisor migration removed the shared mail DTO
 * DTO; this is the only mail surface this module needs.
 */
export interface OperatorMailItem {
  readonly from: string;
  readonly read: boolean;
  readonly created_at: string;
}

/**
 * Agent kind, derived from the wire data (the supervisor does not label kinds
 * directly — see classification rules in {@link agentKind}):
 * - `pool`: a multi-session worker (a "polecat"); the `pool` field is set, or
 *   the template names the polecat pool.
 * - `role`: a named, single-purpose agent (project-lead, reviewer, deacon, …).
 * - `orch`: city/orchestration layer (mayor, control-dispatcher) that directs
 *   the rest; rig-less, or a control-dispatcher.
 */
export type AgentKind = 'pool' | 'role' | 'orch';

export const AGENT_KINDS: readonly AgentKind[] = ['pool', 'role', 'orch'];

/** Label for the city-level (rig-less) agents: mayor, city control-dispatcher. */
export const ORCHESTRATION = 'orchestration';

const CONTROL_DISPATCHER = 'control-dispatcher';

/** The operator's canonical mail triager. By Gas City convention the mayor
 *  digests the worker firehose and forwards only what needs the human. */
const MAYOR = 'mayor';

export function basename(path: string | null | undefined): string {
  if (!path) return '';
  let end = path.length;
  while (end > 0 && path.charCodeAt(end - 1) === 47) end--;
  const trimmed = path.slice(0, end);
  const seg = trimmed.slice(trimmed.lastIndexOf('/') + 1);
  return seg || trimmed;
}

/** The pool-worker template token. A pool worker is a "polecat" by Gas City
 *  convention (see {@link agentKind}); the dashboard has no other pool kind. */
const POLECAT = 'polecat';

/**
 * A mail sender that is a pool worker (a "polecat") — the worker firehose the
 * operator's nav badge folds away (gascity-dashboard-2j8e.5). Classified from
 * the `from` STRING, not a live session: pool workers are ephemeral, so the
 * session that sent a message is often already gone and cannot be looked up.
 * The supervisor reports the sender inconsistently (a bare name, a session id,
 * or a filesystem path), so the polecat token is matched anywhere in the
 * basename — mirroring the template check {@link agentKind} runs on a session.
 */
export function isPoolWorkerSender(from: string): boolean {
  return basename(from).toLowerCase().includes(POLECAT);
}

/**
 * The operator's needs-you mail (gascity-dashboard-2j8e.5): unread messages that
 * are NOT pool-worker firehose. The single selector the Mail nav badge and the
 * Mail page both read, so the badge count and the page count cannot disagree
 * (mirrors {@link selectBlockedRuns} for the Runs badge). Recipient scoping is
 * the caller's job — both feed it the operator inbox — so this stays a pure
 * sender/read filter. Input order is preserved; the caller owns sort.
 */
export function selectOperatorActionableUnread<T extends OperatorMailItem>(
  mail: readonly T[],
): readonly T[] {
  return mail.filter((m) => !m.read && !isPoolWorkerSender(m.from));
}

/**
 * Canonical rig label. The supervisor reports a rig inconsistently — sometimes
 * a filesystem path (`/home/ds/projects/scix_experiments`), sometimes a name
 * (`scix-experiments`), with varying case — which split one project across two
 * groups. Normalise (basename · `_`→`-` · lowercase) so a project is one group.
 */
export function rigLabel(rig: string | null | undefined): string {
  const base = basename(rig);
  if (!base) return ORCHESTRATION;
  return base.split('_').join('-').toLowerCase();
}

/**
 * Classifies an agent's kind from the wire fields. The supervisor does not
 * carry a pool/role label (its `agent_kind` is the unrelated agent/provider
 * axis), so we derive it: orchestration first (a control-dispatcher or a
 * rig-less agent like the mayor directs the rest, regardless of any pool
 * bucket), then a pool worker (a multi-session "polecat", identified by the
 * `pool` field or the polecat template), else a named role agent.
 */
export function agentKind(s: DashboardSession): AgentKind {
  const template = s.template ?? '';
  const isDispatcher =
    template === CONTROL_DISPATCHER ||
    template.endsWith(`/${CONTROL_DISPATCHER}`) ||
    template.endsWith(`.${CONTROL_DISPATCHER}`);
  if (isDispatcher || rigLabel(s.rig) === ORCHESTRATION) return 'orch';
  if ((s.pool && s.pool.length > 0) || template === 'polecat' || template.endsWith('/polecat')) {
    return 'pool';
  }
  return 'role';
}

/**
 * Mail the operator should actually see: escalations from the orchestration
 * layer (the mayor), not the worker firehose. The wire's `read`/`priority`
 * flags are unusable as a "needs you" signal here — the supervisor never sets
 * priority and the operator never marks mail read (the mayor handles it) — so
 * we filter by SENDER ROLE instead: keep mail from an orchestration-kind agent
 * (resolved against the live session list) or the mayor, fold away pool-worker
 * chatter. Newest first.
 */
export function operatorMail<T extends OperatorMailItem>(
  mail: readonly T[],
  sessions: readonly DashboardSession[],
): readonly T[] {
  const orchSenders = new Set<string>([MAYOR]);
  for (const s of sessions) {
    if (agentKind(s) === 'orch') orchSenders.add(basename(s.title ?? s.alias ?? s.id) || s.id);
  }
  return mail
    .filter((m) => !m.read && orchSenders.has(basename(m.from)))
    .slice()
    .sort((a, b) => b.created_at.localeCompare(a.created_at));
}

/** Count of unread mail folded away by {@link operatorMail} — the worker
 *  reports the mayor handles. Surfaced so the filter is never silent. */
export function foldedMailCount(
  mail: readonly OperatorMailItem[],
  shown: readonly OperatorMailItem[],
): number {
  const unread = mail.filter((m) => !m.read).length;
  return Math.max(0, unread - shown.length);
}

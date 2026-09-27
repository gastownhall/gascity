import type { SupervisorBead } from '../supervisor/beadReads';
import type { ExecutionAttempt } from '../lib/workbenchAttempts';

// Policy-bound pull-request actions for a Workbench Execution Attempt.
//
// Workbench has NO direct merge authority: this module decides which
// prepare/queue actions are OFFERED, and the actions delegate to Gas City
// policy (repository rules, approvals, merge-queue). It never models merge,
// approval bypass, or force-push — those are not actions here at all.

export type PullRequestAction = 'prepare' | 'queue';

export type PullRequestBlockedReason =
  | 'no_active_attempt'
  | 'stale_revision'
  | 'policy_rejected'
  | 'conflict'
  | 'queue_unavailable';

export interface PullRequestPolicy {
  /** Actions the Workbench may offer (empty when blocked). */
  allowed: PullRequestAction[];
  /** Why no action is offered, when blocked. */
  blocked: PullRequestBlockedReason | null;
}

export interface PullRequestContext {
  bead: SupervisorBead;
  attempt: ExecutionAttempt | null;
  /** The revision the attempt currently points at (branch/commit). */
  attemptRevision: string | null;
  /** The revision Gas City policy would evaluate for the queued PR. */
  policyRevision: string | null;
  /** Whether the merge queue is reachable. */
  queueAvailable: boolean;
  /** Policy verdict from Gas City, when known. */
  policyRejected: boolean;
  /** Whether the worktree has an unresolved conflict. */
  hasConflict: boolean;
}

function revisionOf(bead: SupervisorBead): string {
  const metadata = (bead as { metadata?: Record<string, string> }).metadata ?? {};
  return (metadata['gc.work_branch'] ?? metadata['gc.work_commit'] ?? '').trim();
}

/**
 * Decide the offered PR actions for the selected attempt. Explicit and
 * fail-closed: any uncertainty yields no action plus a reason, and no path ever
 * grants merge/force-push authority.
 */
export function pullRequestPolicy(ctx: PullRequestContext): PullRequestPolicy {
  if (ctx.attempt === null) return { allowed: [], blocked: 'no_active_attempt' };
  if (ctx.policyRejected) return { allowed: [], blocked: 'policy_rejected' };
  if (ctx.hasConflict) return { allowed: [], blocked: 'conflict' };
  if (!ctx.queueAvailable) return { allowed: [], blocked: 'queue_unavailable' };

  const attemptRevision = (ctx.attemptRevision ?? revisionOf(ctx.bead)).trim();
  const policyRevision = (ctx.policyRevision ?? '').trim();
  // The queued PR must target the exact revision the attempt produced.
  if (policyRevision.length > 0 && attemptRevision.length > 0 && policyRevision !== attemptRevision) {
    return { allowed: [], blocked: 'stale_revision' };
  }

  return { allowed: ['prepare', 'queue'], blocked: null };
}

/** Human-readable reason for a blocked state. */
export function blockedReasonLabel(reason: PullRequestBlockedReason): string {
  switch (reason) {
    case 'no_active_attempt':
      return 'No active attempt to prepare a pull request from.';
    case 'stale_revision':
      return 'The attempt revision has moved; refresh before queueing.';
    case 'policy_rejected':
      return 'Gas City policy rejected this pull request.';
    case 'conflict':
      return 'The worktree has an unresolved conflict.';
    case 'queue_unavailable':
      return 'The merge queue is unavailable.';
  }
}

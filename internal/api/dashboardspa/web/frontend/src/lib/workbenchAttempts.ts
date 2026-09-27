import type { SupervisorBead } from '../supervisor/beadReads';
import type { SupervisorSession } from '../supervisor/sessionReads';

// Execution Attempt projection for the Workbench.
//
// A Gas City "Execution Attempt" is not a second durable record: it is the
// existing Session (+ its worktree) that Gas City assigned to a Bead. This
// module resolves the Bead's newest ACTIVE attempt and its prior completed /
// failed attempts from the typed supervisor reads only — the Bead's own
// gc.* session metadata for the current attempt, and the session list for
// live state and history. Assignment, Sessions, worktrees, retries, pools and
// agent lifecycle stay Gas City's responsibility; nothing here duplicates them.

export interface ExecutionAttempt {
  sessionId: string;
  sessionName: string;
  workDir: string;
  state: string;
  active: boolean;
  startedAt: string;
  lastActive: string;
}

export interface ResolvedAttempts {
  /** Newest active attempt, or null when none is live. */
  current: ExecutionAttempt | null;
  /** Prior attempts, newest first. */
  history: ExecutionAttempt[];
  /** True when the Bead names a Session that the read cannot resolve. */
  staleReference: boolean;
  /** The Session id the Bead names, if any (for the stale case). */
  referencedSessionId: string | null;
}

function beadSessionId(bead: SupervisorBead): string | null {
  const metadata = (bead as { metadata?: Record<string, string> }).metadata ?? {};
  const id = (metadata['gc.session_id'] ?? metadata['isolation.session'] ?? '').trim();
  return id.length > 0 ? id : null;
}

function beadSessionName(bead: SupervisorBead): string {
  const metadata = (bead as { metadata?: Record<string, string> }).metadata ?? {};
  return (metadata['gc.session_name'] ?? '').trim();
}

function beadWorkDir(bead: SupervisorBead): string {
  const metadata = (bead as { metadata?: Record<string, string> }).metadata ?? {};
  return (metadata['gc.work_dir'] ?? metadata['work_dir'] ?? '').trim();
}

function isActive(session: SupervisorSession): boolean {
  return session.running === true || session.state === 'active' || session.state === 'working';
}

function toAttempt(session: SupervisorSession): ExecutionAttempt {
  return {
    sessionId: session.id,
    sessionName: session.session_name ?? session.id,
    workDir: session.work_dir ?? '',
    state: session.state,
    active: isActive(session),
    startedAt: session.created_at ?? '',
    lastActive: session.last_active ?? '',
  };
}

/**
 * Resolve a Bead's attempts from the typed session read.
 *
 * - The newest active Session the Bead names (or that lists the Bead as its
 *   `active_bead`) is the current attempt.
 * - Every other Session that names the Bead is history, newest first.
 * - A Bead that names a Session absent from the read is `staleReference`, with
 *   an explicit (empty) state rather than a fabricated attempt.
 */
export function resolveAttempts(
  bead: SupervisorBead,
  sessions: readonly SupervisorSession[],
): ResolvedAttempts {
  const referencedSessionId = beadSessionId(bead);
  const referencedName = beadSessionName(bead);

  const related = sessions.filter((session) => {
    if (session.id === referencedSessionId) return true;
    if (referencedName.length > 0 && session.session_name === referencedName) return true;
    return session.active_bead === bead.id;
  });

  const attempts = related.map(toAttempt).sort((a, b) => b.startedAt.localeCompare(a.startedAt));
  const current = attempts.find((attempt) => attempt.active) ?? null;
  const history = attempts.filter((attempt) => attempt !== current);

  // The Bead names a session the read cannot resolve: surface it explicitly.
  const staleReference =
    referencedSessionId !== null && !sessions.some((session) => session.id === referencedSessionId);

  return { current, history, staleReference, referencedSessionId };
}

/** The worktree a Bead's attempt owns, without duplicating it. */
export function attemptWorktree(bead: SupervisorBead, resolved: ResolvedAttempts): string {
  if (resolved.current?.workDir) return resolved.current.workDir;
  return beadWorkDir(bead);
}

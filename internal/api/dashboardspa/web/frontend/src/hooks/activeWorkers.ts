import { parseAssignee, IN_PROGRESS_STATUS } from 'gas-city-dashboard-shared';
import type { SupervisorBead } from '../supervisor/beadReads';
import type { SupervisorSession } from '../supervisor/sessionReads';
import { canonicalRigLabel, cleanWorkerName, isWorkerSession, sessionProject } from './projectOf';

// Session-driven "Workers active" derivation.
//
// The primary signal is the live WORKER SESSIONS, not the in-progress beads:
// the work-beads churn to zero within seconds (focus-reviews finish fast) and
// live in rig stores the dashboard's bead fetch doesn't reliably aggregate,
// while the worker sessions stay active across that churn. So we count the
// active worker sessions and group them by rig for the calm summary line.
//
// A bead, when one is captured for a worker, is attached as secondary context
// (`→ <bead-id>: <title>`). The common case is no bead — the worker being
// active IS the signal, so we never surface "unassigned" rows here.

/** A live worker session, plus the in-progress bead it is running when one is
 *  captured (best effort — usually absent, since the beads churn). */
export interface ActiveWorker {
  session: SupervisorSession;
  /** Clean rig label (e.g. `gascity`), `-main` suffix stripped. */
  rig: string;
  /** Clean worker role (e.g. `polecat`), no path / no `-gc-XXXXX` suffix. */
  worker: string;
  /** The in-progress bead assigned to this session, when one was captured. */
  bead?: SupervisorBead;
}

/** A rig and its active-worker count, for the summary line. */
export interface WorkerRigCount {
  rig: string;
  count: number;
}

export interface ActiveWorkers {
  /** Per-worker rows, most-recently-active first. */
  workers: ActiveWorker[];
  /** Rig counts, most workers first (ties broken alphabetically). */
  byRig: WorkerRigCount[];
  /** Total active worker sessions. */
  total: number;
}

/** The clean rig label for a worker session (session rig basename, `-main`
 *  suffix stripped). */
function rigOf(session: SupervisorSession): string {
  return canonicalRigLabel(sessionProject(session).label);
}

/** The clean worker role for a worker session. Prefer the template (the stable
 *  role name, e.g. `polecat`, `scix-worker`); fall back to the runtime
 *  session_name for a dynamically-spawned slot. Run through cleanWorkerName so
 *  no path / no `-gc-XXXXX` handle leaks into the display. */
function workerOf(session: SupervisorSession): string {
  const fromTemplate = cleanWorkerName(session.template ?? '');
  if (fromTemplate.length > 0) return fromTemplate;
  return cleanWorkerName(session.session_name ?? session.id);
}

function activityKey(w: ActiveWorker): number {
  const ms = w.session.last_active ? Date.parse(w.session.last_active) : NaN;
  return Number.isFinite(ms) ? ms : 0;
}

/**
 * Derive the active-worker rows + the per-rig summary from the live sessions,
 * best-effort joining each worker to its in-progress bead.
 *
 * Steps (mechanical, no semantic judgment):
 *  1. Keep only active worker sessions (isWorkerSession excludes orchestration).
 *  2. Index in-progress beads by the session id embedded in their assignee.
 *  3. For each worker session, attach its bead when one is indexed.
 *  4. Sort rows by most-recent activity; build rig counts (most workers first).
 */
export function deriveActiveWorkers(
  sessions: readonly SupervisorSession[],
  beads: readonly SupervisorBead[],
): ActiveWorkers {
  // Map a live session id -> the in-progress bead assigned to it (if any).
  const beadBySession = new Map<string, SupervisorBead>();
  for (const bead of beads) {
    if (bead.status !== IN_PROGRESS_STATUS) continue;
    const assignee = bead.assignee?.trim();
    if (!assignee) continue;
    const { sessionId } = parseAssignee(assignee);
    // First bead wins for a given session — deterministic, and a session runs
    // one unit of work at a time.
    if (sessionId && !beadBySession.has(sessionId)) {
      beadBySession.set(sessionId, bead);
    }
  }

  const workers: ActiveWorker[] = [];
  for (const session of sessions) {
    if (!isWorkerSession(session)) continue;
    const bead = beadBySession.get(session.id);
    workers.push({
      session,
      rig: rigOf(session),
      worker: workerOf(session),
      ...(bead ? { bead } : {}),
    });
  }

  workers.sort((a, b) => activityKey(b) - activityKey(a));

  const counts = new Map<string, number>();
  for (const w of workers) counts.set(w.rig, (counts.get(w.rig) ?? 0) + 1);
  const byRig: WorkerRigCount[] = Array.from(counts, ([rig, count]) => ({
    rig,
    count,
  })).sort((a, b) => b.count - a.count || a.rig.localeCompare(b.rig));

  return { workers, byRig, total: workers.length };
}

/**
 * The calm one-line summary, e.g.
 *   "7 workers active across gascity (3), scix-experiments (3), gascity-packs (2)".
 * Returns the empty-state sentence when no workers are active.
 */
export function summarizeActiveWorkers(workers: ActiveWorkers): string {
  if (workers.total === 0) return 'No workers active right now.';
  const noun = workers.total === 1 ? 'worker' : 'workers';
  const groups = workers.byRig.map((g) => `${g.rig} (${g.count})`).join(', ');
  return `${workers.total} ${noun} active across ${groups}.`;
}

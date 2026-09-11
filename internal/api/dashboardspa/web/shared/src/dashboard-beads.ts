import type { IsoTimestamp } from './dashboard-sessions.js';
import type { CountedList } from './lists.js';

export type BeadId = string;

export type BeadStatus = 'open' | 'in_progress' | 'blocked' | 'closed' | 'deferred' | string;

export type BeadIssueType =
  | 'feature'
  | 'bug'
  | 'task'
  | 'epic'
  | 'chore'
  | 'decision'
  | 'session'
  | 'message'
  | 'convoy'
  | string;

export interface DashboardBeadDependency {
  depends_on_id: string;
  issue_id: string;
  type: string;
}

/**
 * Dashboard-owned bead projection used by pure run and relationship selectors.
 * Supervisor `Bead` values are narrowed into this shape at frontend edges;
 * shared selectors do not import or own the generated supervisor type.
 */
export interface DashboardBead {
  id: BeadId;
  title: string;
  status: BeadStatus;
  issue_type: BeadIssueType;
  /** Normalized priority. Non-priority rows carry null. */
  priority: number | null;
  description?: string;
  assignee?: string;
  created_at: IsoTimestamp;
  labels?: string[];
  /** Selector metadata values are strings; callers parse typed values explicitly. */
  metadata?: Record<string, string>;
  /** Reference handle. On formula templates this is the formula name. */
  ref?: string;
  /** Parent bead id. */
  parent?: string;
  /** Originating actor / source id. */
  from?: string;
  /** True when the bead is ephemeral. */
  ephemeral?: boolean;
  /** Bead ids this bead needs before it can run. */
  needs?: string[] | null;
  /** Structured dependency rows. */
  dependencies?: DashboardBeadDependency[] | null;
  /** Last update time when exposed. Older fixtures only carry created_at. */
  updated_at?: IsoTimestamp;
}

export type DashboardBeadList = CountedList<DashboardBead>;

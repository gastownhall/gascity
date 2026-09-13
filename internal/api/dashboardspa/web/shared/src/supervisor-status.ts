import type { IsoTimestamp } from './dashboard-sessions.js';
import type { StatusBody } from './generated/gc-supervisor-client/index.js';

// gascity-dashboard-4bol: the supervisor /status read turns slow on a bloated
// city store (~247K beads / 21 rigs) and trips the interactive 2.5s budget the
// Health page used for its store-thresholds / dolt-usage / beads-usage widgets,
// surfacing "supervisor status unavailable" even while the 30s background
// samplers succeed. The dashboard backend now samples /status on the same
// higher ceiling and serves the cached snapshot, so the browser reads a fast
// local route instead of racing the slow supervisor. The envelope is
// dashboard-owned (availability + freshness metadata); `status` passes the raw
// supervisor wire shape through unchanged rather than mirroring it.

export type SupervisorStatusUnavailableReason =
  /** The backend sampler has not completed a first read yet. */
  | 'not_sampled_yet'
  /** Backend sampled, but the supervisor /status read failed. */
  | 'status_read_failed';

export type SupervisorStatusReport =
  | {
      available: true;
      sampledAt: IsoTimestamp;
      status: StatusBody;
    }
  | {
      available: false;
      reason: SupervisorStatusUnavailableReason;
      /** The last successfully sampled status, if any (degraded, not blank). */
      status: StatusBody | null;
    };

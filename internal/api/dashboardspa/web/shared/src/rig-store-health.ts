import type { IsoTimestamp } from './dashboard-sessions.js';

// Per-rig bead-store + dolt health, surfaced on the Health tab so the class of
// incident the city hit (dolt outage, store bloat, schema drift) is visible
// per rig before it bites. Unlike the supervisor's single city-level
// store_health, each rig carries its own embedded-dolt `.beads` store, so this
// is a dashboard-local probe (host-FS reachability + dolt endpoint liveness +
// `bd ping` connectivity check), not supervisor wire data.

/** Per-rig roll-up tone for the status dot. */
export type RigStoreRollup = 'ok' | 'warn' | 'down';

/** Status of a single `bd ping` check, mirroring its own vocabulary. */
export type RigStoreCheckStatus = 'ok' | 'warning' | 'error';

/** One non-ok store/dolt health check, surfaced for the operator. */
export interface RigStoreCheck {
  /** Check category, e.g. "Beads". */
  category: string;
  /** Check name, e.g. "Database Connectivity". */
  name: string;
  status: RigStoreCheckStatus;
  message: string;
}

export interface RigStoreHealth {
  /** Rig name as reported by the supervisor. */
  rig: string;
  /** Absolute host path of the rig's `.beads` store. */
  beadsPath: string;
  /** Roll-up tone driving the per-rig status dot. */
  rollup: RigStoreRollup;
  /** `.beads` store directory present on disk. */
  reachable: boolean;
  /** Configured dolt sql-server endpoint (host:port), or null when the store
   *  declares no server endpoint (e.g. embedded / jsonl-only mode). */
  doltEndpoint: string | null;
  /** Dolt sql-server reachable at its endpoint. `null` when there is no
   *  endpoint to probe (no outage to report for an embedded store). */
  doltConnected: boolean | null;
  /** Live bead row count when the probe reported it. Always `null` under
   *  `bd ping`, which reports connectivity only and emits no count. */
  issueCount: number | null;
  /** Store/dolt checks that are not `ok`. Benign hygiene categories
   *  (git hooks, editor integrations) are excluded — they are not store
   *  health and the dashboard operator cannot act on them here. */
  problems: RigStoreCheck[];
  /** Set when the probe could not fully assess the store (e.g. `bd ping`
   *  returned no parseable JSON while still exiting 0). */
  note?: string;
}

export type RigStoreHealthUnavailableReason =
  | 'not_sampled_yet'
  /** Backend sampled, but the supervisor rig list read failed. */
  | 'rig_list_failed'
  /** The browser could not reach the dashboard backend for the snapshot. */
  | 'fetch_failed';

export type RigStoreHealthReport =
  | {
      available: true;
      sampledAt: IsoTimestamp;
      rigs: RigStoreHealth[];
    }
  | {
      available: false;
      reason: RigStoreHealthUnavailableReason;
      /** Rigs from the prior successful sample, if any. */
      rigs: RigStoreHealth[];
    };

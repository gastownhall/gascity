import type { IsoTimestamp } from './dashboard-sessions.js';

export type HealthMetricUnavailableReason = 'sample_failed' | 'invalid_sample' | 'value_overflow';

export type HealthMetric<T> =
  | { status: 'available'; value: T }
  | { status: 'unavailable'; reason: HealthMetricUnavailableReason };

export interface HostLoadAverages {
  load_avg_1: number;
  load_avg_5: number;
  load_avg_15: number;
}

export interface HostMemory {
  total_mem_bytes: number;
  free_mem_bytes: number;
}

export interface SystemHealth {
  /** Backend process state — totally local to the admin dashboard's node process. */
  admin: {
    pid: number;
    uptime_sec: number;
    rss: HealthMetric<number>;
    heap_used_bytes: number;
    node_version: string;
  };
  /** Machine-level state from Node's os module. */
  host: {
    load: HealthMetric<HostLoadAverages>;
    memory: HealthMetric<HostMemory>;
    /** Number of logical CPUs. */
    cpu_count: number;
    uptime: HealthMetric<number>;
  };
}

export type LocalToolVersion =
  | { status: 'available'; version: string; source: string }
  | { status: 'unavailable'; reason: string };

/** Installed versions of the host tools the dashboard probes locally — the same
 *  version data `gc doctor` surfaces, reported as a plain passthrough. The
 *  dashboard does not maintain a recommended-version floor or compute drift
 *  verdicts; gc owns that policy. */
export interface LocalToolVersions {
  dolt: LocalToolVersion;
  beads: LocalToolVersion;
  gc: LocalToolVersion;
}

export interface DoltNomsSample {
  ts: IsoTimestamp;
  bytes: number;
}

export type DoltNomsUnavailableReason = 'store_health_absent' | 'sample_failed';

export type DoltNomsTrend =
  | {
      available: true;
      /** Up to 144 samples (24 h at 10-min cadence). */
      samples: DoltNomsSample[];
      source: string;
    }
  | {
      available: false;
      /** Historical samples, if the source became unavailable after sampling. */
      samples: DoltNomsSample[];
      reason: DoltNomsUnavailableReason;
    };

import type {
  SessionResponse,
  UsageBody,
  UsageSessionRecent,
} from 'gas-city-dashboard-shared/gc-supervisor';
import { cleanWorkerName } from '../../hooks/projectOf';
import type { VUMeter } from './VUBank';
import { sumTokens } from './derive';

// The session VU roster: one meter per live session, its value the session's
// live tok/min. Sessions and usage rows live in different id spaces (session ids
// vs usage session names), so each roster row carries a set of candidate keys
// and meterValue joins across them, live-window first.

const MAX_VU = 8; // active sessions are capped so the bank stays legible

interface RosterRow {
  id: string;
  name: string;
  rig: string;
  /** Candidate keys used to match live feed samples and usage rows. */
  keys: string[];
}

/**
 * buildVuMeters produces one meter per live session, sorted by rig then name and
 * capped at MAX_VU. Each value is the session's live tok/min from the
 * `worker.operation` window when present, else its usage-window tokens
 * normalized to per-minute, else zero. Meters deep-link to the agents roster
 * (usage/session names are not clean AgentDetail slugs).
 */
export function buildVuMeters(
  sessions: SessionResponse[] | null,
  usage: UsageBody | null,
  liveByKey: Record<string, number>,
): VUMeter[] {
  const roster = sessionRoster(sessions, usage);
  return roster
    .map((row) => ({ row, value: meterValue(row, usage, liveByKey) }))
    .sort((a, b) => a.row.rig.localeCompare(b.row.rig) || a.row.name.localeCompare(b.row.name))
    .slice(0, MAX_VU)
    .map(({ row, value }) => ({ id: row.id, name: row.name, value, href: '/agents' }));
}

/**
 * sessionRoster builds the candidate roster from the running sessions, falling
 * back to the usage-recent roster when the sessions read is unavailable so the
 * bank still populates from whatever live signal exists.
 */
export function sessionRoster(
  sessions: SessionResponse[] | null,
  usage: UsageBody | null,
): RosterRow[] {
  if (sessions !== null) {
    return sessions.filter((s) => s.running).map(sessionToRoster);
  }
  return (usage?.recent_by_session ?? []).map(usageToRoster);
}

/** sessionToRoster splits a session's `rig/name` identity into a roster row. */
export function sessionToRoster(session: SessionResponse): RosterRow {
  const raw = session.session_name;
  const slash = raw.indexOf('/');
  const rig =
    session.rig !== undefined && session.rig.length > 0
      ? session.rig
      : slash > 0
        ? raw.slice(0, slash)
        : '';
  const name = cleanWorkerName(slash > 0 ? raw.slice(slash + 1) : raw);
  const keys = uniqueStrings([session.id, cleanWorkerName(raw), name, session.alias]);
  return { id: session.id, name, rig, keys };
}

/** usageToRoster builds a roster row from a usage-recent session record. */
export function usageToRoster(session: UsageSessionRecent): RosterRow {
  const name = cleanWorkerName(session.session);
  return {
    id: session.session_id ?? session.session,
    name,
    rig: '',
    keys: uniqueStrings([session.session_id, session.session, name]),
  };
}

/**
 * meterValue resolves a roster row's tok/min: the live `worker.operation` window
 * value keyed by any of the row's candidate keys wins; otherwise the usage
 * window's tokens for a matching session, normalized to per-minute; else zero.
 */
export function meterValue(
  row: RosterRow,
  usage: UsageBody | null,
  liveByKey: Record<string, number>,
): number {
  for (const key of row.keys) {
    const live = liveByKey[key];
    if (live !== undefined) return live;
  }
  const windowSecs = Math.max(usage?.recent_window_secs ?? 0, 1);
  for (const entry of usage?.recent_by_session ?? []) {
    const entryName = cleanWorkerName(entry.session);
    if (
      (entry.session_id !== undefined && row.keys.includes(entry.session_id)) ||
      row.keys.includes(entryName)
    ) {
      return (sumTokens(entry) * 60) / windowSecs;
    }
  }
  return 0;
}

/** uniqueStrings compacts a mixed list into distinct non-empty strings, in order. */
export function uniqueStrings(values: Array<string | undefined>): string[] {
  const out: string[] = [];
  for (const value of values) {
    if (value !== undefined && value.length > 0 && !out.includes(value)) out.push(value);
  }
  return out;
}

export type { RosterRow };

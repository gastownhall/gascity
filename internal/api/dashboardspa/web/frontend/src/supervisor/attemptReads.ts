import { activeCityOrThrow } from '../api/cityBase';
import { resolveSupervisorBaseUrl, supervisorUrl } from './url';

// Read-only Execution-Attempt worktree diff, served by the typed Gas City API
// (GET /v0/city/{cityName}/bead/{id}/attempts/diff). The endpoint runs
// `git diff` in the attempt's worktree; this module only reads it and never
// stages, discards, commits, or otherwise mutates the worktree.

export type AttemptDiffState = 'ok' | 'empty' | 'missing_worktree' | 'unavailable';

export interface AttemptDiff {
  worktree: string;
  state: AttemptDiffState;
  text?: string;
  truncated: boolean;
  binary: boolean;
  bytes: number;
}

export async function fetchAttemptDiff(beadId: string): Promise<AttemptDiff> {
  const city = activeCityOrThrow('read attempt diff');
  const url = supervisorUrl(
    resolveSupervisorBaseUrl(),
    `/v0/city/${encodeURIComponent(city)}/bead/${encodeURIComponent(beadId)}/attempts/diff`,
  );
  const response = await fetch(url, { headers: { Accept: 'application/json' } });
  if (!response.ok) {
    throw new Error(`diff unavailable (${response.status})`);
  }
  const payload = (await response.json()) as { body?: AttemptDiff } & Partial<AttemptDiff>;
  return (payload.body ?? (payload as AttemptDiff)) as AttemptDiff;
}

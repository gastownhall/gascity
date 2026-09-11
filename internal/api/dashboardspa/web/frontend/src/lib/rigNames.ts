import type { RigResponse } from 'gas-city-dashboard-shared/gc-supervisor';

/**
 * The fields of a supervisor rig needed to canonicalize a rig identity: the
 * authoritative `name` and the filesystem `path` it lives at.
 */
export type RigNameSource = Pick<RigResponse, 'name' | 'path'>;

/**
 * Resolve a raw rig identity to its canonical rig name.
 *
 * The supervisor reports an agent's `rig` inconsistently — sometimes the rig
 * name, sometimes the filesystem path it runs in, and sometimes a path that
 * belongs to no registered rig at all (e.g. /home/ds/gascity-main). Matching
 * against the authoritative rig list collapses all of these to the one real
 * name, and returns undefined for anything the list does not recognize so
 * non-rig paths are dropped rather than surfaced.
 */
export function resolveRigName(
  raw: string | undefined | null,
  rigs: ReadonlyArray<RigNameSource>,
): string | undefined {
  const value = raw?.trim();
  if (!value) return undefined;
  const byName = rigs.find((rig) => rig.name === value);
  if (byName) return byName.name;
  const byPath = rigs.find((rig) => rig.path === value);
  return byPath?.name;
}

/**
 * The sorted, de-duplicated list of real rig names to offer as dropdown
 * options. Sourced from the supervisor's authoritative rig list so filesystem
 * paths and unregistered directories never appear as choices.
 */
export function rigNameOptions(rigs: ReadonlyArray<RigNameSource>): string[] {
  return Array.from(
    new Set(rigs.map((rig) => rig.name.trim()).filter((name) => name.length > 0)),
  ).sort((a, b) => a.localeCompare(b));
}

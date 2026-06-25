import type { RunSnapshotBead } from '../run-snapshot.js';
import type { RunExecutionPath } from '../run-detail.js';
import { meta, nonEmpty } from './bead-fields.js';

export function resolveRunExecutionPath(
  root: RunSnapshotBead | undefined,
  beads: RunSnapshotBead[],
  rigRoot?: string,
): RunExecutionPath {
  const candidates = [
    ...executionWorkDirs(root),
    ...beads.flatMap((bead) => executionWorkDirs(bead)),
    ...rigRoots(root),
    ...beads.flatMap((bead) => rigRoots(bead)),
    nonEmpty(rigRoot),
  ];
  const path = candidates.find((candidate) => candidate !== undefined);
  return path === undefined
    ? { kind: 'unavailable', reason: 'missing_cwd_and_rig_root' }
    : { kind: 'known', path };
}

function executionWorkDirs(bead: RunSnapshotBead | undefined): Array<string | undefined> {
  return [
    meta(bead, 'gc.cwd'),
    meta(bead, 'cwd'),
    meta(bead, 'gc.work_dir'),
    meta(bead, 'work_dir'),
  ];
}

function rigRoots(bead: RunSnapshotBead | undefined): Array<string | undefined> {
  return [meta(bead, 'gc.rig_root'), meta(bead, 'rig_root')];
}

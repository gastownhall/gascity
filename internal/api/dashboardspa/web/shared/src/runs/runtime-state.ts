import type { DashboardBead } from '../dashboard-beads.js';
import type { RunSnapshotBead, RunSnapshot } from '../run-snapshot.js';
import { nonEmpty } from './bead-fields.js';

const PRESENTATION_METADATA_KEYS = [
  'gc.outcome',
  'gc.cwd',
  'cwd',
  'gc.work_dir',
  'work_dir',
  'gc.rig_root',
  'rig_root',
  'session_id',
  'session_name',
  'gc.session_id',
  'gc.session_name',
  'gc.sessionId',
  'gc.sessionName',
  'rig_id',
] as const;

/**
 * The supervisor run snapshot carries compiled graph shape, but today its
 * embedded bead rows can lag the canonical /beads runtime state. Merge only
 * fields that affect presentation state so graph topology still comes from
 * /run while run status comes from exact live supervisor bead reads.
 */
export function mergeRunRuntimeState(
  raw: RunSnapshot,
  runtimeBeads: readonly DashboardBead[],
): RunSnapshot {
  if (!Array.isArray(raw.beads) || runtimeBeads.length === 0) return raw;
  const runtimeById = new Map(
    runtimeBeads
      .map((bead) => [nonEmpty(bead.id), bead] as const)
      .filter((entry): entry is readonly [string, DashboardBead] => entry[0] !== undefined),
  );

  return {
    ...raw,
    beads: raw.beads.map((bead) => mergeRunBead(bead, runtimeById.get(bead.id))),
  };
}

function mergeRunBead(bead: RunSnapshotBead, runtime: DashboardBead | undefined): RunSnapshotBead {
  if (!runtime) return bead;
  const status = nonEmpty(runtime.status) ?? bead.status;
  const assignee = nonEmpty(runtime.assignee) ?? bead.assignee;
  const merged: RunSnapshotBead = {
    ...bead,
    status,
    metadata: {
      ...bead.metadata,
      ...presentationMetadata(runtime.metadata),
    },
  };
  if (assignee !== undefined) merged.assignee = assignee;
  return merged;
}

function presentationMetadata(metadata: DashboardBead['metadata']): Record<string, string> {
  if (!metadata) return {};
  const out: Record<string, string> = {};
  for (const key of PRESENTATION_METADATA_KEYS) {
    const value = metadata[key];
    const text = nonEmpty(value);
    if (text) out[key] = text;
  }
  return out;
}

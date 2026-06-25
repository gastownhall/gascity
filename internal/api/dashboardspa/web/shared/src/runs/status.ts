import type { RunSnapshotBead } from '../run-snapshot.js';
import type { RunExecutionInstance, RunNodeStatus } from '../run-detail.js';
import { meta, nonEmpty } from './bead-fields.js';

export function presentationStatus(bead: RunSnapshotBead): RunNodeStatus {
  const raw = (nonEmpty(bead.status) ?? '').toLowerCase();
  const outcome = meta(bead, 'gc.outcome')?.toLowerCase();
  if (raw === 'closed' || raw === 'completed' || raw === 'done') {
    if (outcome === 'fail' || outcome === 'failed') return 'failed';
    if (outcome === 'skipped') return 'skipped';
    return 'completed';
  }
  if (raw === 'in_progress' || raw === 'active' || raw === 'running') {
    return 'active';
  }
  if (raw === 'blocked') return 'blocked';
  if (raw === 'ready') return 'ready';
  if (raw === 'failed') return 'failed';
  if (raw === 'skipped') return 'skipped';
  return 'pending';
}

export function aggregateStatus(
  instances: RunExecutionInstance[],
  visibleInstance: RunExecutionInstance | undefined,
): RunNodeStatus {
  if (instances.some((instance) => isRunningStatus(instance.status))) {
    return 'active';
  }
  if (visibleInstance?.status) return visibleInstance.status;
  return 'pending';
}

export function isRunningStatus(status: RunNodeStatus | undefined): boolean {
  return status === 'active' || status === 'running';
}

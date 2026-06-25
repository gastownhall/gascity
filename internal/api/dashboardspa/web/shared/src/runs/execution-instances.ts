import type { RunSnapshotBead } from '../run-snapshot.js';
import type {
  RunAttempt,
  RunAttemptSummary,
  RunConstructKind,
  RunControlBadge,
  RunDisplayNode,
  RunExecutionInstance,
  RunIteration,
  RunIterationSummary,
  RunNodeScope,
  RunSessionAttachment,
} from '../run-detail.js';
import { attemptFor, iterationFor, nonEmpty, positiveIntegerMeta } from './bead-fields.js';
import type { RunSessionLinkContext } from './session-link.js';
import { runSessionLinkFor } from './session-link.js';
import { aggregateStatus, isRunningStatus, presentationStatus } from './status.js';

export interface RunNodeGroup {
  semanticNodeId: string;
  title: string;
  kind: string;
  constructKind: RunConstructKind;
  scopeRef?: string;
  loopControlNodeId?: string;
  beads: RunSnapshotBead[];
}

export function buildRunDisplayNode(
  group: RunNodeGroup,
  controlBadges: RunControlBadge[],
  latestLoopIteration: number | undefined,
  sessionContext: RunSessionLinkContext = {},
): RunDisplayNode {
  const instances = group.beads
    .map((bead, index) => buildExecutionInstance(group.semanticNodeId, bead, index, sessionContext))
    .sort(compareExecutionInstances);
  const visibleInstance = preferredExecutionInstance(instances);
  const iterations = new Set(
    instances.map((instance) => iterationValue(instance.iteration)).filter(isNumber),
  );
  const visibleIteration =
    (visibleInstance ? iterationValue(visibleInstance.iteration) : undefined) ??
    (iterations.size > 0 ? Math.max(...iterations) : undefined);
  const historicalOnly =
    group.loopControlNodeId !== undefined &&
    visibleIteration !== undefined &&
    latestLoopIteration !== undefined &&
    visibleIteration < latestLoopIteration;

  for (const instance of instances) {
    const currentIteration =
      !historicalOnly &&
      (visibleIteration === undefined || iterationValue(instance.iteration) === visibleIteration);
    instance.currentIteration = currentIteration;
    instance.historical = !currentIteration;
    instance.session =
      instance.session.kind === 'attached'
        ? {
            ...instance.session,
            streamable: currentIteration && isRunningStatus(instance.status),
          }
        : instance.session;
  }

  if (visibleInstance === undefined) {
    throw new Error(`run node ${group.semanticNodeId} has no execution instances`);
  }

  const node: RunDisplayNode = {
    id: group.semanticNodeId,
    semanticNodeId: group.semanticNodeId,
    title: group.title,
    kind: group.kind,
    constructKind: group.constructKind,
    status: aggregateStatus(instances, visibleInstance),
    currentBeadId: visibleInstance.beadId,
    scope: runNodeScope(group.scopeRef),
    visibleInGraph: !historicalOnly,
    historicalOnly,
    iterationSummary: iterationSummaryFor(
      visibleIteration,
      iterations.size,
      group.loopControlNodeId,
    ),
    attemptSummary: attemptSummaryFor(instances, group.beads),
    visibleExecutionInstanceId: visibleInstance.id,
    executionInstances: instances,
    controlBadges,
  };
  return node;
}

export function latestIterationsByLoop(groups: RunNodeGroup[]): Map<string, number> {
  const latest = new Map<string, number>();
  for (const group of groups) {
    if (!group.loopControlNodeId) continue;
    for (const bead of group.beads) {
      const iteration = iterationFor(bead);
      if (!isNumber(iteration)) continue;
      const current = latest.get(group.loopControlNodeId);
      if (current === undefined || iteration > current) {
        latest.set(group.loopControlNodeId, iteration);
      }
    }
  }
  return latest;
}

function buildExecutionInstance(
  semanticNodeId: string,
  bead: RunSnapshotBead,
  index: number,
  sessionContext: RunSessionLinkContext,
): RunExecutionInstance {
  const beadId = nonEmpty(bead.id);
  if (beadId === undefined) {
    throw new Error(`run node ${semanticNodeId} has a bead with an empty id`);
  }
  const iteration = iterationFor(bead);
  const attempt = attemptFor(bead);
  const status = presentationStatus(bead);
  const sessionLink = runSessionLinkFor(bead, status, sessionContext);
  const instance: RunExecutionInstance = {
    id: beadId || `${semanticNodeId}:iteration-${iteration ?? 0}:attempt-${attempt ?? index}`,
    semanticNodeId,
    beadId,
    iteration: iterationState(iteration),
    attempt: attemptState(attempt),
    label: instanceLabel(iteration, attempt),
    status,
    session: sessionState(status, sessionLink),
    currentIteration: true,
    historical: false,
  };
  return instance;
}

function preferredExecutionInstance(
  instances: RunExecutionInstance[],
): RunExecutionInstance | undefined {
  return [...instances].sort(compareExecutionInstances).at(-1);
}

function compareExecutionInstances(
  left: RunExecutionInstance,
  right: RunExecutionInstance,
): number {
  return (
    iterationOrder(left.iteration) - iterationOrder(right.iteration) ||
    attemptOrder(left.attempt) - attemptOrder(right.attempt) ||
    left.beadId.localeCompare(right.beadId)
  );
}

function attemptSummaryFor(
  instances: RunExecutionInstance[],
  beads: RunSnapshotBead[],
): RunAttemptSummary {
  const attemptCount = attemptCountFor(instances);
  const activeAttempt = activeAttemptFor(instances);
  const badgeLabel = attemptBadgeFor(beads);
  if (attemptCount === 0 && badgeLabel === undefined) return { kind: 'none' };
  return {
    kind: 'tracked',
    count: Math.max(attemptCount, 1),
    badge:
      badgeLabel === undefined ? { kind: 'count-only' } : { kind: 'bounded', label: badgeLabel },
    active:
      activeAttempt === undefined ? { kind: 'idle' } : { kind: 'running', value: activeAttempt },
  };
}

function attemptBadgeFor(beads: RunSnapshotBead[]): string | undefined {
  const max = beads
    .map((bead) => positiveIntegerMeta(bead, 'gc.max_attempts'))
    .find((value) => value !== undefined);
  if (max === undefined) return undefined;
  const attempts = new Set(beads.map(attemptFor).filter(isNumber));
  return `${Math.max(attempts.size, 1)}/${max}`;
}

function attemptCountFor(instances: RunExecutionInstance[]): number {
  const attempts = new Set(
    instances.map((instance) => attemptValue(instance.attempt)).filter(isNumber),
  );
  return attempts.size;
}

function activeAttemptFor(instances: RunExecutionInstance[]): number | undefined {
  const active = instances.find((instance) => isRunningStatus(instance.status));
  return active ? attemptValue(active.attempt) : undefined;
}

function instanceLabel(iteration: number | undefined, attempt: number | undefined): string {
  if (iteration !== undefined && attempt !== undefined) {
    return `iteration ${iteration}, attempt ${attempt}`;
  }
  if (iteration !== undefined) return `iteration ${iteration}`;
  if (attempt !== undefined) return `attempt ${attempt}`;
  return 'base';
}

function runNodeScope(scopeRef: string | undefined): RunNodeScope {
  return scopeRef === undefined ? { kind: 'run' } : { kind: 'scoped', ref: scopeRef };
}

function iterationSummaryFor(
  visibleIteration: number | undefined,
  iterationCount: number,
  loopControlNodeId: string | undefined,
): RunIterationSummary {
  if (visibleIteration === undefined || iterationCount === 0) return { kind: 'single' };
  return {
    kind: 'stacked',
    visibleIteration,
    iterationCount,
    control:
      loopControlNodeId === undefined
        ? { kind: 'unknown' }
        : { kind: 'known', id: loopControlNodeId },
  };
}

function iterationState(value: number | undefined): RunIteration {
  return value === undefined ? { kind: 'base' } : { kind: 'loop', value };
}

function attemptState(value: number | undefined): RunAttempt {
  return value === undefined ? { kind: 'untracked' } : { kind: 'attempt', value };
}

function sessionState(
  status: RunExecutionInstance['status'],
  link: ReturnType<typeof runSessionLinkFor>,
): RunSessionAttachment {
  if (link !== undefined) {
    return { kind: 'attached', link, streamable: false };
  }
  return {
    kind: 'none',
    reason: status === 'pending' || status === 'ready' ? 'not_started' : 'session_unresolved',
  };
}

function iterationValue(iteration: RunIteration): number | undefined {
  return iteration.kind === 'loop' ? iteration.value : undefined;
}

function attemptValue(attempt: RunAttempt): number | undefined {
  return attempt.kind === 'attempt' ? attempt.value : undefined;
}

function iterationOrder(iteration: RunIteration): number {
  return iterationValue(iteration) ?? 0;
}

function attemptOrder(attempt: RunAttempt): number {
  return attemptValue(attempt) ?? 0;
}

function isNumber(value: number | undefined): value is number {
  return typeof value === 'number' && Number.isFinite(value);
}

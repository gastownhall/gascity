import type { RunSnapshotBead } from '../run-snapshot.js';
import type { RunControlBadge } from '../run-detail.js';
import { externalizeId, meta, nonEmpty, normalizedStepRef } from './bead-fields.js';
import type { RunNodeGroup } from './execution-instances.js';
import {
  badgeLabelFor,
  constructKindFor,
  displayTitleFor,
  externalKindFor,
  hiddenBadgeTargetFor,
  isHiddenConstruct,
  loopControlNodeIdFor,
  semanticNodeIdFor,
} from './node-shape.js';
import { presentationStatus } from './status.js';

export interface RunBeadGroups {
  groups: RunNodeGroup[];
  physicalToSemantic: Map<string, string>;
  badgesByTarget: Map<string, RunControlBadge[]>;
}

interface BeadIdentity {
  base: string;
  disambiguator: string | undefined;
  semanticNodeId: string;
}

export function groupRunBeads(beads: RunSnapshotBead[], rootBeadId: string): RunBeadGroups {
  const groupedBeads = new Map<string, RunSnapshotBead[]>();
  const physicalToSemantic = new Map<string, string>();
  const badgesByTarget = new Map<string, RunControlBadge[]>();
  const physicalLogicalTargets = referencedPhysicalLogicalTargets(beads);
  const identities = resolveBeadIdentities(beads, rootBeadId, physicalLogicalTargets);
  const badgeTargetAliases = buildBadgeTargetAliases(
    beads,
    rootBeadId,
    identities,
    physicalLogicalTargets,
  );

  for (const bead of beads) {
    const beadId = nonEmpty(bead.id) ?? '';
    const constructKind = constructKindFor(bead, rootBeadId);
    const semanticNodeId =
      identities.get(bead)?.semanticNodeId ?? semanticNodeIdFor(bead, rootBeadId);
    physicalToSemantic.set(beadId, semanticNodeId);

    if (isHiddenConstruct(constructKind)) {
      const target = hiddenBadgeTargetFor(bead, rootBeadId);
      const resolvedTarget = resolveBadgeTarget(bead, rootBeadId, badgeTargetAliases, target);
      if (resolvedTarget) {
        const badges = badgesByTarget.get(resolvedTarget) ?? [];
        badges.push({
          id: beadId || `${resolvedTarget}-${constructKind}`,
          label: badgeLabelFor(constructKind),
          status: presentationStatus(bead),
        });
        badgesByTarget.set(resolvedTarget, badges);
      }
      continue;
    }

    const group = groupedBeads.get(semanticNodeId) ?? [];
    group.push(bead);
    groupedBeads.set(semanticNodeId, group);
  }

  return {
    groups: [...groupedBeads].map(([semanticNodeId, groupBeads]) =>
      buildRunNodeGroup(semanticNodeId, groupBeads, rootBeadId),
    ),
    physicalToSemantic,
    badgesByTarget,
  };
}

function buildRunNodeGroup(
  semanticNodeId: string,
  beads: RunSnapshotBead[],
  rootBeadId: string,
): RunNodeGroup {
  const shapeBead = preferredShapeBead(beads, rootBeadId);
  const constructKind = constructKindFor(shapeBead, rootBeadId);
  const scopeRef = groupOptional(
    beads,
    shapeBead,
    (bead) => meta(bead, 'gc.scope_ref') ?? nonEmpty(bead.scope_ref),
  );
  const loopControlNodeId = groupOptional(beads, shapeBead, loopControlNodeIdFor);

  return {
    semanticNodeId,
    title: displayTitleFor(shapeBead, semanticNodeId),
    kind: externalKindFor(shapeBead, constructKind),
    constructKind,
    beads,
    ...(scopeRef !== undefined ? { scopeRef } : {}),
    ...(loopControlNodeId !== undefined ? { loopControlNodeId } : {}),
  };
}

function preferredShapeBead(
  beads: readonly RunSnapshotBead[],
  rootBeadId: string,
): RunSnapshotBead {
  const [first] = [...beads].sort((left, right) => {
    const priorityDiff =
      constructPriority(constructKindFor(right, rootBeadId)) -
      constructPriority(constructKindFor(left, rootBeadId));
    if (priorityDiff !== 0) return priorityDiff;
    return beadSortKey(left).localeCompare(beadSortKey(right));
  });
  if (!first) throw new Error('cannot build run node group from zero beads');
  return first;
}

function groupOptional(
  beads: readonly RunSnapshotBead[],
  shapeBead: RunSnapshotBead,
  resolve: (bead: RunSnapshotBead) => string | undefined,
): string | undefined {
  return resolve(shapeBead) ?? sortedBeads(beads).map(resolve).find(isDefined);
}

function constructPriority(kind: RunNodeGroup['constructKind']): number {
  switch (kind) {
    case 'run-root':
      return 100;
    case 'check-loop':
      return 90;
    case 'retry':
      return 80;
    case 'condition':
    case 'fanout':
    case 'scope':
    case 'expansion':
      return 70;
    case 'step':
      return 10;
    case 'control':
    case 'run-finalize':
    case 'scope-check':
    case 'spec':
    case 'unknown':
      return 0;
  }
}

function sortedBeads(beads: readonly RunSnapshotBead[]): RunSnapshotBead[] {
  return [...beads].sort((left, right) => beadSortKey(left).localeCompare(beadSortKey(right)));
}

function beadSortKey(bead: RunSnapshotBead): string {
  return [nonEmpty(bead.id), normalizedStepRef(bead), nonEmpty(bead.title)]
    .filter(isDefined)
    .join('\u0000');
}

function resolveBeadIdentities(
  beads: readonly RunSnapshotBead[],
  rootBeadId: string,
  physicalLogicalTargets: ReadonlySet<string>,
): Map<RunSnapshotBead, BeadIdentity> {
  const partialIdentities = new Map<RunSnapshotBead, Omit<BeadIdentity, 'semanticNodeId'>>();
  const identitiesByBase = new Map<string, Set<string>>();

  for (const bead of beads) {
    const constructKind = constructKindFor(bead, rootBeadId);
    if (isHiddenConstruct(constructKind)) continue;
    const base = groupingBaseSemanticId(bead, rootBeadId, physicalLogicalTargets);
    const disambiguator = duplicateResolutionIdentity(
      bead,
      rootBeadId,
      base,
      physicalLogicalTargets,
    );
    partialIdentities.set(bead, { base, disambiguator });
    const identity = disambiguator ?? base;
    const identities = identitiesByBase.get(base) ?? new Set<string>();
    identities.add(identity);
    identitiesByBase.set(base, identities);
  }

  const resolved = new Map<RunSnapshotBead, BeadIdentity>();
  for (const bead of beads) {
    const partial = partialIdentities.get(bead) ?? {
      base: semanticNodeIdFor(bead, rootBeadId),
      disambiguator: undefined,
    };
    const identities = identitiesByBase.get(partial.base);
    const semanticNodeId =
      identities && identities.size > 1 && partial.disambiguator
        ? partial.disambiguator
        : partial.base;
    resolved.set(bead, { ...partial, semanticNodeId });
  }
  return resolved;
}

function groupingBaseSemanticId(
  bead: RunSnapshotBead,
  rootBeadId: string,
  physicalLogicalTargets: ReadonlySet<string>,
): string {
  const beadId = nonEmpty(bead.id);
  if (beadId && beadId === rootBeadId) return rootBeadId;
  const explicit = meta(bead, 'gc.logical_bead_id') ?? nonEmpty(bead.logical_bead_id);
  if (explicit) return externalizeId(explicit);
  const constructKind = constructKindFor(bead, rootBeadId);
  if (
    (constructKind === 'check-loop' || constructKind === 'retry') &&
    beadId &&
    physicalLogicalTargets.has(beadId)
  ) {
    return externalizeId(beadId);
  }
  return semanticNodeIdFor(bead, rootBeadId);
}

function duplicateResolutionIdentity(
  bead: RunSnapshotBead,
  rootBeadId: string,
  base: string,
  physicalLogicalTargets: ReadonlySet<string>,
): string | undefined {
  const beadId = nonEmpty(bead.id);
  if (beadId && physicalLogicalTargets.has(beadId) && externalizeId(beadId) === base) {
    return base;
  }
  return stableSemanticIdentity(bead, rootBeadId);
}

function buildBadgeTargetAliases(
  beads: readonly RunSnapshotBead[],
  rootBeadId: string,
  identities: Map<RunSnapshotBead, BeadIdentity>,
  physicalLogicalTargets: ReadonlySet<string>,
): Map<string, string> {
  const candidates = new Map<string, Set<string>>();

  for (const bead of beads) {
    const constructKind = constructKindFor(bead, rootBeadId);
    if (isHiddenConstruct(constructKind)) continue;
    const identity = identities.get(bead);
    const resolved = identity?.semanticNodeId ?? semanticNodeIdFor(bead, rootBeadId);
    for (const alias of visibleNodeAliases(
      bead,
      rootBeadId,
      resolved,
      identity,
      physicalLogicalTargets,
    )) {
      const existing = candidates.get(alias) ?? new Set<string>();
      existing.add(resolved);
      candidates.set(alias, existing);
    }
  }

  const aliases = new Map<string, string>();
  for (const [alias, targets] of candidates) {
    if (targets.size === 1) {
      const [target] = [...targets];
      if (target) aliases.set(alias, target);
    }
  }
  return aliases;
}

function visibleNodeAliases(
  bead: RunSnapshotBead,
  rootBeadId: string,
  resolved: string,
  identity: BeadIdentity | undefined,
  physicalLogicalTargets: ReadonlySet<string>,
): string[] {
  return [
    resolved,
    semanticNodeIdFor(bead, rootBeadId),
    identity?.base ?? groupingBaseSemanticId(bead, rootBeadId, physicalLogicalTargets),
    identity?.disambiguator,
    stableSemanticIdentity(bead, rootBeadId),
    meta(bead, 'gc.step_id'),
    fullStepRefIdentity(normalizedStepRef(bead)),
    nonEmpty(bead.id),
  ]
    .filter((value): value is string => value !== undefined)
    .map((value) => externalizeId(value));
}

function isDefined<T>(value: T | undefined): value is T {
  return value !== undefined;
}

function referencedPhysicalLogicalTargets(beads: readonly RunSnapshotBead[]): Set<string> {
  const beadIds = new Set(
    beads.map((bead) => nonEmpty(bead.id)).filter((value): value is string => value !== undefined),
  );
  const targets = new Set<string>();
  for (const bead of beads) {
    const logical = meta(bead, 'gc.logical_bead_id') ?? nonEmpty(bead.logical_bead_id);
    if (logical && beadIds.has(logical)) targets.add(logical);
  }
  return targets;
}

function resolveBadgeTarget(
  bead: RunSnapshotBead,
  rootBeadId: string,
  aliases: Map<string, string>,
  fallback: string | null,
): string | null {
  if (constructKindFor(bead, rootBeadId) === 'run-finalize') return rootBeadId;
  for (const candidate of hiddenBadgeTargetCandidates(bead, fallback)) {
    const target = aliases.get(candidate);
    if (target) return target;
  }
  return fallback;
}

function hiddenBadgeTargetCandidates(bead: RunSnapshotBead, fallback: string | null): string[] {
  return [meta(bead, 'gc.control_for'), hiddenBadgeFullTargetFor(bead), fallback ?? undefined]
    .filter((value): value is string => value !== undefined)
    .flatMap((value) => {
      const stripped = stripControlSuffix(value);
      return [value, stripped, externalizeId(stripped)];
    })
    .map((value) => externalizeId(value));
}

function stableSemanticIdentity(bead: RunSnapshotBead, rootBeadId: string): string | undefined {
  const beadId = nonEmpty(bead.id);
  if (beadId && beadId === rootBeadId) return rootBeadId;
  const explicit = meta(bead, 'gc.logical_bead_id') ?? nonEmpty(bead.logical_bead_id);
  if (explicit) return externalizeId(explicit);
  const stepId = meta(bead, 'gc.step_id');
  if (stepId) return externalizeId(stepId);
  return fullStepRefIdentity(normalizedStepRef(bead));
}

function hiddenBadgeFullTargetFor(bead: RunSnapshotBead): string | undefined {
  const controlFor = meta(bead, 'gc.control_for');
  if (controlFor) return externalizeId(stripControlSuffix(controlFor));
  return fullStepRefIdentity(stripControlSuffix(normalizedStepRef(bead) ?? ''));
}

function fullStepRefIdentity(ref: string | null | undefined): string | undefined {
  const clean = nonEmpty(ref);
  if (!clean) return undefined;
  const stripped = stripControlSuffix(clean);
  const parts = stripped.split('.').filter(Boolean);
  if (parts.length === 0) return undefined;
  if (parts.length === 1) return externalizeId(parts[0] ?? stripped);
  return externalizeId(parts.slice(1).join('.'));
}

function stripControlSuffix(ref: string): string {
  return ref.replace(/-scope-check$/, '').replace(/\.scope-check$/, '');
}
